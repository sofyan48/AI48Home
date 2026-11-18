package alerting

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// Service periodically evaluates alert rules and notifies channels.
type Service struct {
	db    *gorm.DB
	store *Store
	owner string
	mu    sync.Mutex
}

// NewService creates an alerting service on the Home database.
func NewService(db *gorm.DB) *Service {
	return &Service{db: db, store: NewStore(db), owner: uuid.NewString()}
}

// Store exposes the persistence layer for the management handlers.
func (s *Service) Store() *Store {
	if s == nil {
		return nil
	}
	return s.store
}

// Start migrates the alerting tables and runs the evaluator until ctx ends.
func (s *Service) Start(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("alerting service database is nil")
	}
	if errMigrate := s.store.Migrate(ctx); errMigrate != nil {
		return fmt.Errorf("alerting migrate: %w", errMigrate)
	}

	go s.loop(ctx)
	return nil
}

func (s *Service) loop(ctx context.Context) {
	wait := 15 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		settings, errSettings := s.store.GetSettings(ctx)
		if errSettings != nil {
			log.WithError(errSettings).Warn("alerting: load settings failed")
			wait = time.Minute
			continue
		}
		wait = settings.interval()
		if !settings.Enabled {
			continue
		}
		acquired, errLease := s.store.acquireLease(ctx, s.owner, 3*settings.interval())
		if errLease != nil {
			log.WithError(errLease).Warn("alerting: acquire lease failed")
			continue
		}
		if !acquired {
			continue
		}
		if errEval := s.Evaluate(ctx, settings); errEval != nil && ctx.Err() == nil {
			log.WithError(errEval).Warn("alerting: evaluation failed")
		}
	}
}

// Evaluate runs every enabled rule once and sends notifications for transitions.
func (s *Service) Evaluate(ctx context.Context, settings Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now, errNow := cluster.DatabaseNow(ctx, s.db)
	if errNow != nil {
		return errNow
	}
	candidates, evaluated, errCollect := s.collect(ctx, settings, now)
	if errCollect != nil {
		return errCollect
	}
	states, errStates := s.store.loadStates(ctx)
	if errStates != nil {
		return errStates
	}
	channels, errChannels := s.enabledChannels(ctx)
	if errChannels != nil {
		return errChannels
	}

	active := make(map[string]struct{}, len(candidates))
	for _, item := range candidates {
		active[item.Key] = struct{}{}
		s.handleActive(ctx, settings, channels, states[item.Key], item, now)
	}
	for key, state := range states {
		if _, ok := active[key]; ok {
			continue
		}
		// Only resolve rules that were actually evaluated this round; a disabled
		// rule or a failed check must not produce false "resolved" messages.
		if _, ok := evaluated[state.Rule]; !ok {
			continue
		}
		s.handleCleared(ctx, settings, channels, state, now)
	}
	return nil
}

// Resolve resets one firing alert without suppressing future evaluations or
// removing node tracking. Notification failures are logged, as in Evaluate.
func (s *Service) Resolve(ctx context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, errState := s.store.firingState(ctx, key)
	if errState != nil {
		return errState
	}
	settings, errSettings := s.store.GetSettings(ctx)
	if errSettings != nil {
		return errSettings
	}
	var channels []Channel
	if settings.SendResolved {
		loaded, errChannels := s.enabledChannels(ctx)
		if errChannels != nil {
			return errChannels
		}
		channels = loaded
	}
	if errDelete := s.store.deleteFiringState(ctx, key); errDelete != nil {
		return errDelete
	}
	if settings.SendResolved {
		s.broadcast(ctx, channels, Message{
			Rule: state.Rule, Title: state.Title, Detail: "Manually resolved by an operator.",
			Resolved: true, Time: time.Now().UTC(),
		})
	}
	return nil
}

func (s *Service) collect(ctx context.Context, settings Settings, now time.Time) ([]candidate, map[string]struct{}, error) {
	evaluated := make(map[string]struct{})
	out := make([]candidate, 0)

	needCredentials := settings.CredentialDeadEnabled || settings.CredentialRefreshEnabled ||
		settings.CredentialCooldownEnabled || settings.QuotaLowEnabled
	var credentials map[string]credentialInfo
	if needCredentials {
		loaded, errLoad := loadCredentials(ctx, s.db)
		if errLoad != nil {
			return nil, nil, errLoad
		}
		credentials = loaded
		out = append(out, credentialCandidates(settings, credentials, now)...)
		for _, rule := range []string{RuleCredentialDead, RuleCredentialRefresh, RuleCredentialCooldown} {
			if settings.ruleEnabled(rule) {
				evaluated[rule] = struct{}{}
			}
		}
	}
	if settings.QuotaLowEnabled {
		items, errQuota := quotaCandidates(ctx, s.db, settings, credentials, now)
		if errQuota != nil {
			log.WithError(errQuota).Warn("alerting: quota check failed")
		} else {
			out = append(out, items...)
			evaluated[RuleQuotaLow] = struct{}{}
		}
	}
	if settings.NodeOfflineEnabled {
		items, errNodes := nodeCandidates(ctx, s.db, settings, now)
		if errNodes != nil {
			log.WithError(errNodes).Warn("alerting: node check failed")
		} else {
			out = append(out, items...)
			evaluated[RuleNodeOffline] = struct{}{}
		}
	}
	return out, evaluated, nil
}

func (s *Service) handleActive(ctx context.Context, settings Settings, channels []Channel, state State, item candidate, now time.Time) {
	if state.Key == "" {
		state = State{Key: item.Key, Rule: item.Rule, Subject: item.Subject, FirstSeenAt: now}
	}
	state.Title = item.Title
	state.Detail = item.Detail
	state.UpdatedAt = now

	held := item.Immediate || now.Sub(state.FirstSeenAt) >= item.HoldFor
	shouldNotify := false
	if held && !state.Firing {
		state.Firing = true
		shouldNotify = true
	} else if state.Firing && settings.RepeatMinutes > 0 && state.LastNotifiedAt != nil &&
		now.Sub(*state.LastNotifiedAt) >= time.Duration(settings.RepeatMinutes)*time.Minute {
		shouldNotify = true
	}
	if shouldNotify {
		s.broadcast(ctx, channels, Message{Rule: item.Rule, Title: item.Title, Detail: item.Detail, Time: now})
		notifiedAt := now
		state.LastNotifiedAt = &notifiedAt
	}
	if errSave := s.store.saveState(ctx, state); errSave != nil {
		log.WithError(errSave).WithField("rule", item.Rule).Warn("alerting: save state failed")
	}
}

func (s *Service) handleCleared(ctx context.Context, settings Settings, channels []Channel, state State, now time.Time) {
	if state.Firing && settings.SendResolved {
		s.broadcast(ctx, channels, Message{Rule: state.Rule, Title: state.Title, Detail: "Condition cleared.", Resolved: true, Time: now})
	}
	if errDelete := s.store.deleteState(ctx, state.Key); errDelete != nil {
		log.WithError(errDelete).WithField("rule", state.Rule).Warn("alerting: delete state failed")
	}
}

func (s *Service) enabledChannels(ctx context.Context) ([]Channel, error) {
	all, errList := s.store.ListChannels(ctx)
	if errList != nil {
		return nil, errList
	}
	out := make([]Channel, 0, len(all))
	for _, channel := range all {
		if channel.Enabled {
			out = append(out, channel)
		}
	}
	return out, nil
}

func (s *Service) broadcast(ctx context.Context, channels []Channel, message Message) {
	for _, channel := range channels {
		if errSend := Send(ctx, channel, message); errSend != nil {
			log.WithFields(log.Fields{"channel_id": channel.ID, "channel_type": channel.Type, "rule": message.Rule}).
				WithError(errSend).Warn("alerting: send notification failed")
		}
	}
}
