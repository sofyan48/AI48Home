package alerting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"gorm.io/gorm"
)

func newTestService(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	db, errOpen := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
	if errOpen != nil {
		t.Fatalf("open sqlite: %v", errOpen)
	}
	sqlDB, errSQL := db.DB()
	if errSQL != nil {
		t.Fatalf("sql db: %v", errSQL)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if errClose := sqlDB.Close(); errClose != nil {
			t.Logf("close sqlite: %v", errClose)
		}
	})
	if errMigrate := db.AutoMigrate(&cluster.AuthRecord{}, &cluster.QuotaWindowRecord{}, &cluster.CPANodeRecord{}, &cluster.CPANodeMetadataRecord{}); errMigrate != nil {
		t.Fatalf("migrate upstream tables: %v", errMigrate)
	}
	service := NewService(db)
	if errMigrate := service.Store().Migrate(context.Background()); errMigrate != nil {
		t.Fatalf("migrate alerting: %v", errMigrate)
	}
	return service, db
}

func dbNow(t *testing.T, db *gorm.DB) time.Time {
	t.Helper()
	now, errNow := cluster.DatabaseNow(context.Background(), db)
	if errNow != nil {
		t.Fatalf("database now: %v", errNow)
	}
	return now
}

func firingKeys(t *testing.T, service *Service) map[string]State {
	t.Helper()
	states, errList := service.Store().ListStates(context.Background(), true)
	if errList != nil {
		t.Fatalf("list states: %v", errList)
	}
	out := make(map[string]State, len(states))
	for _, state := range states {
		out[state.Key] = state
	}
	return out
}

type teamsRecorder struct {
	mu       sync.Mutex
	messages []string
}

func (r *teamsRecorder) handler(w http.ResponseWriter, req *http.Request) {
	var payload struct {
		Attachments []struct {
			Content struct {
				Body []struct {
					Text string `json:"text"`
				} `json:"body"`
			} `json:"content"`
		} `json:"attachments"`
	}
	if errDecode := json.NewDecoder(req.Body).Decode(&payload); errDecode == nil &&
		len(payload.Attachments) > 0 && len(payload.Attachments[0].Content.Body) > 0 {
		r.mu.Lock()
		r.messages = append(r.messages, payload.Attachments[0].Content.Body[0].Text)
		r.mu.Unlock()
	}
	w.WriteHeader(http.StatusOK)
}

func (r *teamsRecorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.messages...)
}

func TestEvaluateCredentialQuotaAndNodeAlerts(t *testing.T) {
	ctx := context.Background()
	service, db := newTestService(t)
	recorder := &teamsRecorder{}
	server := httptest.NewTLSServer(http.HandlerFunc(recorder.handler))
	defer server.Close()
	originalClient := httpClient
	httpClient = server.Client()
	defer func() { httpClient = originalClient }()

	if _, errSave := service.Store().SaveChannel(ctx, Channel{Name: "ops", Type: ChannelTeams, Enabled: true, WebhookURL: server.URL}); errSave != nil {
		t.Fatalf("save channel: %v", errSave)
	}

	now := dbNow(t, db)
	longRetry := now.Add(2 * time.Hour)
	shortRetry := now.Add(time.Minute)
	ratioLow := 0.05
	ratioOK := 0.5
	records := []any{
		&cluster.AuthRecord{UUID: "dead", ID: "dead", Index: "dead", Label: "dead-acc", Provider: "codex", Status: "disabled", Disabled: true,
			AuthJSON: cluster.JSONB(`{"status_message":"unauthorized","last_error":{"message":"invalid token","http_status":401}}`)},
		&cluster.AuthRecord{UUID: "cool", ID: "cool", Index: "cool", Provider: "claude", Status: "error", NextRetryAfter: &longRetry,
			AuthJSON: cluster.JSONB(`{"quota":{"reason":"rate limited"}}`)},
		&cluster.AuthRecord{UUID: "brief", ID: "brief", Index: "brief", Provider: "claude", Status: "error", NextRetryAfter: &shortRetry,
			AuthJSON: cluster.JSONB(`{}`)},
		&cluster.AuthRecord{UUID: "refresh", ID: "refresh", Index: "refresh", Provider: "gemini", Status: "active",
			AuthJSON: cluster.JSONB(`{"last_refresh_error":{"code":"refresh_temporarily_unavailable","message":"timeout"}}`)},
		&cluster.QuotaWindowRecord{CredentialID: "refresh", WindowID: "weekly", Label: "Weekly", Scope: "account", Mode: "m", Status: "ok", Unit: "percent",
			PeriodUnit: "week", Source: "probe", RemainingRatio: &ratioLow, ObservedAt: now},
		&cluster.QuotaWindowRecord{CredentialID: "refresh", WindowID: "daily", Scope: "account", Mode: "m", Status: "ok", Unit: "percent",
			PeriodUnit: "day", Source: "probe", RemainingRatio: &ratioOK, ObservedAt: now},
		&cluster.CPANodeRecord{HomeIP: "10.0.0.1", HomePort: 8327, HomeStartedAt: now, NodeKey: "a", NodeID: "node-a", ClientIP: "10.0.0.9", LastSeenAt: now.Add(-10 * time.Minute)},
		&cluster.CPANodeRecord{HomeIP: "10.0.0.1", HomePort: 8327, HomeStartedAt: now, NodeKey: "b", NodeID: "node-b", ClientIP: "10.0.0.8", LastSeenAt: now},
	}
	for _, record := range records {
		if errCreate := db.Create(record).Error; errCreate != nil {
			t.Fatalf("seed %T: %v", record, errCreate)
		}
	}

	settings := DefaultSettings()
	settings.RefreshFailureMinutes = 0
	if errEval := service.Evaluate(ctx, settings); errEval != nil {
		t.Fatalf("evaluate: %v", errEval)
	}

	firing := firingKeys(t, service)
	for _, key := range []string{"credential_dead:dead", "credential_cooldown:cool", "credential_refresh:refresh", "quota_low:refresh:weekly", "node_offline:node-a"} {
		if _, ok := firing[key]; !ok {
			t.Errorf("expected firing alert %q, got %v", key, firing)
		}
	}
	for _, key := range []string{"credential_cooldown:brief", "quota_low:refresh:daily", "node_offline:node-b", "credential_refresh:dead"} {
		if _, ok := firing[key]; ok {
			t.Errorf("unexpected firing alert %q", key)
		}
	}
	if got := len(recorder.list()); got != 5 {
		t.Fatalf("notifications after first evaluation = %d, want 5: %v", got, recorder.list())
	}

	// A second evaluation without changes must not notify again.
	if errEval := service.Evaluate(ctx, settings); errEval != nil {
		t.Fatalf("second evaluate: %v", errEval)
	}
	if got := len(recorder.list()); got != 5 {
		t.Fatalf("notifications after unchanged evaluation = %d, want 5", got)
	}

	// Node A reconnects: one resolved notification.
	if errUpdate := db.Model(&cluster.CPANodeRecord{}).Where("node_key = ?", "a").Update("last_seen_at", dbNow(t, db)).Error; errUpdate != nil {
		t.Fatalf("update node: %v", errUpdate)
	}
	if errEval := service.Evaluate(ctx, settings); errEval != nil {
		t.Fatalf("third evaluate: %v", errEval)
	}
	messages := recorder.list()
	if len(messages) != 6 || !strings.HasPrefix(messages[5], "✅ RESOLVED: CPA node offline") {
		t.Fatalf("expected node resolved notification, got %v", messages)
	}
	if _, ok := firingKeys(t, service)["node_offline:node-a"]; ok {
		t.Fatal("node-a alert should be cleared")
	}
}

func TestEvaluateCooldownHoldAndDisabledRuleKeepsState(t *testing.T) {
	ctx := context.Background()
	service, db := newTestService(t)
	now := dbNow(t, db)
	retry := now.Add(10 * time.Minute)
	if errCreate := db.Create(&cluster.AuthRecord{UUID: "c", ID: "c", Index: "c", Status: "error", NextRetryAfter: &retry, AuthJSON: cluster.JSONB(`{}`)}).Error; errCreate != nil {
		t.Fatalf("seed: %v", errCreate)
	}
	settings := DefaultSettings()
	if errEval := service.Evaluate(ctx, settings); errEval != nil {
		t.Fatalf("evaluate: %v", errEval)
	}
	if _, ok := firingKeys(t, service)["credential_cooldown:c"]; ok {
		t.Fatal("short cooldown must be held, not fired")
	}
	// Simulate the cooldown having been observed for longer than the threshold.
	if errUpdate := db.Model(&State{}).Where("alert_key = ?", "credential_cooldown:c").
		Update("first_seen_at", now.Add(-time.Hour)).Error; errUpdate != nil {
		t.Fatalf("age state: %v", errUpdate)
	}
	if errEval := service.Evaluate(ctx, settings); errEval != nil {
		t.Fatalf("evaluate: %v", errEval)
	}
	if _, ok := firingKeys(t, service)["credential_cooldown:c"]; !ok {
		t.Fatal("persistent cooldown should fire after the hold period")
	}

	// Disabling the rule must not resolve the existing alert.
	settings.CredentialCooldownEnabled = false
	if errEval := service.Evaluate(ctx, settings); errEval != nil {
		t.Fatalf("evaluate: %v", errEval)
	}
	if _, ok := firingKeys(t, service)["credential_cooldown:c"]; !ok {
		t.Fatal("disabled rule must keep its existing state")
	}
}

func TestAcquireLeaseSingleOwner(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestService(t)
	store := service.Store()
	first, errFirst := store.acquireLease(ctx, "home-a", time.Minute)
	if errFirst != nil || !first {
		t.Fatalf("home-a acquire = %v, %v", first, errFirst)
	}
	second, errSecond := store.acquireLease(ctx, "home-b", time.Minute)
	if errSecond != nil || second {
		t.Fatalf("home-b must not acquire an active lease: %v, %v", second, errSecond)
	}
	renew, errRenew := store.acquireLease(ctx, "home-a", time.Minute)
	if errRenew != nil || !renew {
		t.Fatalf("home-a renew = %v, %v", renew, errRenew)
	}
}

func TestChannelValidationAndMasking(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestService(t)
	if _, errSave := service.Store().SaveChannel(ctx, Channel{Name: "x", Type: ChannelTeams, WebhookURL: "http://insecure"}); errSave == nil {
		t.Fatal("teams webhook must require https")
	}
	if _, errSave := service.Store().SaveChannel(ctx, Channel{Name: "x", Type: ChannelTelegram, BotToken: "123"}); errSave == nil {
		t.Fatal("telegram must require chat_id")
	}
	response := channelResponse(Channel{Type: ChannelTelegram, BotToken: "123456:SECRETTOKEN", WebhookURL: "https://x.webhook.office.com/webhookb2/abc/IncomingWebhook/sig"})
	if hint := response["bot_token_hint"].(string); strings.Contains(hint, "SECRET") {
		t.Fatalf("bot token leaked: %s", hint)
	}
	if hint := response["webhook_url_hint"].(string); strings.Contains(hint, "sig") || strings.Contains(hint, "abc") {
		t.Fatalf("webhook url leaked: %s", hint)
	}
}
