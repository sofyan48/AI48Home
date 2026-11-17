package alerting

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrChannelNotFound is returned when a channel id does not exist.
var ErrChannelNotFound = errors.New("alert channel not found")

// Store persists alerting data in alert_* tables.
type Store struct {
	db *gorm.DB
}

// NewStore creates a store bound to the Home database.
func NewStore(db *gorm.DB) *Store {
	return &Store{db: db}
}

// Migrate creates or updates the alerting tables.
func (s *Store) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("alerting store database is nil")
	}
	return s.db.WithContext(ctx).AutoMigrate(&Settings{}, &Channel{}, &State{}, &NodeSeen{}, &leaseRecord{})
}

// GetSettings returns persisted settings or defaults when none exist.
func (s *Store) GetSettings(ctx context.Context) (Settings, error) {
	var record Settings
	errFind := s.db.WithContext(ctx).Where("id = ?", 1).Limit(1).Find(&record).Error
	if errFind != nil {
		return Settings{}, errFind
	}
	if record.ID == 0 {
		return DefaultSettings(), nil
	}
	return record, nil
}

// SaveSettings validates and stores settings.
func (s *Store) SaveSettings(ctx context.Context, settings Settings) (Settings, error) {
	settings.ID = 1
	if errValidate := validateSettings(settings); errValidate != nil {
		return Settings{}, errValidate
	}
	settings.UpdatedAt = time.Now().UTC()
	if errSave := s.db.WithContext(ctx).Save(&settings).Error; errSave != nil {
		return Settings{}, errSave
	}
	return settings, nil
}

func validateSettings(settings Settings) error {
	switch {
	case settings.IntervalSeconds < 15 || settings.IntervalSeconds > 3600:
		return fmt.Errorf("interval_seconds must be between 15 and 3600")
	case settings.RepeatMinutes < 0:
		return fmt.Errorf("repeat_minutes must not be negative")
	case settings.RefreshFailureMinutes < 0:
		return fmt.Errorf("refresh_failure_minutes must not be negative")
	case settings.CooldownMinutes < 1:
		return fmt.Errorf("cooldown_minutes must be at least 1")
	case settings.QuotaThresholdPercent <= 0 || settings.QuotaThresholdPercent >= 100:
		return fmt.Errorf("quota_threshold_percent must be between 0 and 100")
	case settings.NodeOfflineSeconds < 10:
		return fmt.Errorf("node_offline_seconds must be at least 10")
	}
	return nil
}

// ListChannels returns every channel ordered by id.
func (s *Store) ListChannels(ctx context.Context) ([]Channel, error) {
	var records []Channel
	errFind := s.db.WithContext(ctx).Order("id ASC").Find(&records).Error
	return records, errFind
}

// GetChannel returns one channel.
func (s *Store) GetChannel(ctx context.Context, id uint) (Channel, error) {
	var record Channel
	errFind := s.db.WithContext(ctx).Where("id = ?", id).Limit(1).Find(&record).Error
	if errFind != nil {
		return Channel{}, errFind
	}
	if record.ID == 0 {
		return Channel{}, ErrChannelNotFound
	}
	return record, nil
}

// SaveChannel validates and creates or updates a channel.
func (s *Store) SaveChannel(ctx context.Context, channel Channel) (Channel, error) {
	channel.Name = strings.TrimSpace(channel.Name)
	channel.Type = strings.ToLower(strings.TrimSpace(channel.Type))
	channel.BotToken = strings.TrimSpace(channel.BotToken)
	channel.ChatID = strings.TrimSpace(channel.ChatID)
	channel.ThreadID = strings.TrimSpace(channel.ThreadID)
	channel.WebhookURL = strings.TrimSpace(channel.WebhookURL)
	if errValidate := validateChannel(channel); errValidate != nil {
		return Channel{}, errValidate
	}
	now := time.Now().UTC()
	channel.UpdatedAt = now
	if channel.ID == 0 {
		channel.CreatedAt = now
	}
	if errSave := s.db.WithContext(ctx).Save(&channel).Error; errSave != nil {
		return Channel{}, errSave
	}
	return channel, nil
}

func validateChannel(channel Channel) error {
	if channel.Name == "" {
		return fmt.Errorf("name is required")
	}
	switch channel.Type {
	case ChannelTelegram:
		if channel.BotToken == "" || channel.ChatID == "" {
			return fmt.Errorf("bot_token and chat_id are required for telegram")
		}
	case ChannelTeams:
		if !strings.HasPrefix(strings.ToLower(channel.WebhookURL), "https://") {
			return fmt.Errorf("webhook_url must be an https URL for teams")
		}
	default:
		return fmt.Errorf("type must be %q or %q", ChannelTelegram, ChannelTeams)
	}
	return nil
}

// DeleteChannel removes a channel.
func (s *Store) DeleteChannel(ctx context.Context, id uint) error {
	result := s.db.WithContext(ctx).Where("id = ?", id).Delete(&Channel{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrChannelNotFound
	}
	return nil
}

// ListStates returns tracked alerts; firingOnly limits the result to active alerts.
func (s *Store) ListStates(ctx context.Context, firingOnly bool) ([]State, error) {
	var records []State
	query := s.db.WithContext(ctx).Order("updated_at DESC").Limit(500)
	if firingOnly {
		query = query.Where("firing = ?", true)
	}
	errFind := query.Find(&records).Error
	return records, errFind
}

func (s *Store) loadStates(ctx context.Context) (map[string]State, error) {
	var records []State
	if errFind := s.db.WithContext(ctx).Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	out := make(map[string]State, len(records))
	for _, record := range records {
		out[record.Key] = record
	}
	return out, nil
}

func (s *Store) saveState(ctx context.Context, state State) error {
	return s.db.WithContext(ctx).Save(&state).Error
}

func (s *Store) deleteState(ctx context.Context, key string) error {
	return s.db.WithContext(ctx).Where("alert_key = ?", key).Delete(&State{}).Error
}

// ForgetNode removes a CPA node from offline tracking together with its alert.
func (s *Store) ForgetNode(ctx context.Context, nodeID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errNode := tx.Where("node_id = ?", nodeID).Delete(&NodeSeen{}).Error; errNode != nil {
			return errNode
		}
		return tx.Where("alert_key = ?", RuleNodeOffline+":"+nodeID).Delete(&State{}).Error
	})
}

// acquireLease makes owner the evaluator for ttl; it returns false when another
// Home holds an unexpired lease.
func (s *Store) acquireLease(ctx context.Context, owner string, ttl time.Duration) (bool, error) {
	acquired := false
	errTx := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now, errNow := cluster.DatabaseNow(ctx, tx)
		if errNow != nil {
			return errNow
		}
		seed := leaseRecord{ID: 1, Owner: "", ExpiresAt: now.Add(-time.Second)}
		if errSeed := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&seed).Error; errSeed != nil {
			return errSeed
		}
		result := tx.Model(&leaseRecord{}).
			Where("id = ? AND (owner = ? OR expires_at < ?)", 1, owner, now).
			Updates(map[string]any{"owner": owner, "expires_at": now.Add(ttl)})
		if result.Error != nil {
			return result.Error
		}
		acquired = result.RowsAffected == 1
		return nil
	})
	return acquired, errTx
}
