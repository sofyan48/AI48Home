package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func quotaFeedbackPayload(t *testing.T, provider, id, requestID, body string, at time.Time) string {
	t.Helper()
	payload, errMarshal := json.Marshal(map[string]any{"provider": provider, "auth_index": id, "auth_type": "oauth", "request_id": requestID,
		"timestamp": at.Format(time.RFC3339Nano), "failed": true, "fail": map[string]any{"status_code": 429, "body": body}})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return string(payload)
}

func TestQuotaFeedbackExplicitProviderSignals(t *testing.T) {
	for _, test := range []struct{ provider, body, want string }{
		{"codex", `{"error":{"type":"usage_limit_reached"}}`, "codex"},
		{"codex", `{"error":{"type":"rate_limit_error"}}`, ""},
		{"codex", `{"error":{"message":"usage_limit_reached"}}`, ""},
		{"openai", `{"error":{"type":"usage_limit_reached"}}`, ""},
		{"antigravity", `{"error":{"status":"RESOURCE_EXHAUSTED"}}`, ""},
		{"antigravity", `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","reason":"QUOTA_EXHAUSTED"}]}}`, ""},
		{"antigravity", `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED"}]}}`, "antigravity"},
		{"codex", `{"error":`, ""},
	} {
		payload := quotaFeedbackPayload(t, test.provider, "auth", "req", test.body, time.Now())
		if got := explicitQuotaFailureProvider(payload); got != test.want {
			t.Fatalf("provider=%s body=%s: got %q, want %q", test.provider, test.body, got, test.want)
		}
	}
	for _, payload := range []string{
		`{"provider":"codex","failed":false,"fail":{"status_code":429,"body":"{\"error\":{\"type\":\"usage_limit_reached\"}}"}}`,
		`{"provider":"codex","failed":true,"fail":{"status_code":503,"body":"{\"error\":{\"type\":\"usage_limit_reached\"}}"}}`,
	} {
		if got := explicitQuotaFailureProvider(payload); got != "" {
			t.Fatalf("non-quota failure latched: %s", payload)
		}
	}
}

func TestQuotaFeedbackDurabilityAndAuthoritativeRecovery(t *testing.T) {
	for _, provider := range []string{"codex", "antigravity"} {
		t.Run(provider, func(t *testing.T) {
			repo, closeRepo := newBillingTestRepository(t, context.Background())
			defer closeRepo()
			id := provider + "-feedback"
			seedQuotaSnapshotAuth(t, repo, id, provider, "Feedback", map[string]any{"type": provider})
			start := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
			body := `{"error":{"type":"usage_limit_reached"}}`
			windowID := "codex-5-hour"
			if provider == "antigravity" {
				body = `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED"}]}}`
				windowID = "antigravity-gemini-5h"
			}
			payload := quotaFeedbackPayload(t, provider, id, "feedback", body, start)
			if _, errAppend := repo.AppendUsage(t.Context(), payload, ""); errAppend != nil {
				t.Fatal(errAppend)
			}
			assertQuotaGuard(t, NewRepository(repo.db), id, "different-alias", "quota_exhausted")
			window := QuotaWindow{ID: windowID, Scope: "account", Status: "healthy", ObservedAt: start.Add(time.Minute)}
			writeGuardWindow(t, repo, id, "response_header", "success", window)
			assertQuotaGuard(t, repo, id, "", "quota_exhausted")
			otherWindow := window
			otherWindow.ID = "codex-code-review-5-hour"
			writeGuardWindow(t, repo, id, "active_probe", "success", otherWindow)
			assertQuotaGuard(t, repo, id, "", "quota_exhausted")
			writeGuardWindow(t, repo, id, "active_probe", "partial", window)
			assertQuotaGuard(t, repo, id, "", "quota_exhausted")
			writeGuardWindow(t, repo, id, "active_probe", "success", window)
			assertQuotaGuard(t, repo, id, "", "")
			// Duplicate failed requests must not bypass the recovery watermark.
			if _, errAppend := repo.AppendUsage(t.Context(), payload, ""); errAppend != nil {
				t.Fatal(errAppend)
			}
			assertQuotaGuard(t, repo, id, "", "")
		})
	}
}

func TestQuotaFeedbackWriteFailureAndIdentityFence(t *testing.T) {
	repo, closeRepo := newBillingTestRepository(t, context.Background())
	defer closeRepo()
	seedQuotaSnapshotAuth(t, repo, "feedback-fence", "codex", "Fence", map[string]any{"type": "codex"})
	start := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	payload := quotaFeedbackPayload(t, "codex", "feedback-fence", "feedback-fenced", `{"error":{"type":"usage_limit_reached"}}`, start)
	if errDrop := repo.db.Migrator().DropTable(&QuotaGuardRecord{}); errDrop != nil {
		t.Fatal(errDrop)
	}
	if _, errAppend := repo.AppendUsage(t.Context(), payload, ""); errAppend == nil {
		t.Fatal("explicit exhaustion acknowledged despite guard failure")
	}
	var count int64
	if errCount := repo.db.Model(&UsageRecord{}).Count(&count).Error; errCount != nil || count != 0 {
		t.Fatalf("usage committed after explicit guard failure: %d %v", count, errCount)
	}
	if errMigrate := repo.db.AutoMigrate(&QuotaGuardRecord{}); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	usage, errAppend := repo.AppendUsage(t.Context(), payload, "")
	if errAppend != nil {
		t.Fatal(errAppend)
	}
	assertQuotaGuard(t, repo, "feedback-fence", "", "quota_exhausted")
	if errUpdate := repo.db.Model(&AuthRecord{}).Where("uuid = ?", "feedback-fence").Update("quota_identity_version", usage.QuotaIdentityVersion+1).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	window := QuotaWindow{ID: "codex-5-hour", Scope: "account", Status: "healthy", ObservedAt: start.Add(time.Minute)}
	writeGuardWindow(t, repo, "feedback-fence", "active_probe", "success", window)
	// A stale feedback record newer than recovery must still fail the generation fence.
	usage.PayloadJSON = JSONB(quotaFeedbackPayload(t, "codex", "feedback-fence", "late", `{"error":{"type":"usage_limit_reached"}}`, start.Add(2*time.Minute)))
	if errFeedback := repo.RecordUsageQuotaFeedback(t.Context(), usage); errFeedback != nil {
		t.Fatal(errFeedback)
	}
	assertQuotaGuard(t, repo, "feedback-fence", "", "")
	// Unrelated auth writes must not suppress newly arriving exhausted feedback.
	if errUpdate := repo.db.Model(&AuthRecord{}).Where("uuid = ?", "feedback-fence").Update("updated_at", start.Add(time.Hour)).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	payload = quotaFeedbackPayload(t, "codex", "feedback-fence", "late-first-arrival", `{"error":{"type":"usage_limit_reached"}}`, start.Add(3*time.Minute))
	if _, errLate := repo.AppendUsage(t.Context(), payload, ""); errLate != nil {
		t.Fatal(errLate)
	}
	assertQuotaGuard(t, repo, "feedback-fence", "", "quota_exhausted")
}

func TestQuotaFeedbackWholeCredentialAndLegacyRecovery(t *testing.T) {
	repo, closeRepo := newBillingTestRepository(t, context.Background())
	defer closeRepo()
	seedQuotaSnapshotAuth(t, repo, "scoped-feedback", "antigravity", "Scoped", map[string]any{"type": "antigravity"})
	start := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	body := `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED"}]}}`
	payload := quotaFeedbackPayload(t, "antigravity", "scoped-feedback", "scoped-failure", body, start)
	// Neither the canonical model nor the alias narrows an explicit failure.
	var fields map[string]any
	if errDecode := json.Unmarshal([]byte(payload), &fields); errDecode != nil {
		t.Fatal(errDecode)
	}
	fields["model"], fields["alias"] = "gemini-3-pro-high", "claude-sonnet-4-5"
	encoded, errEncode := json.Marshal(fields)
	if errEncode != nil {
		t.Fatal(errEncode)
	}
	usage, errAppend := repo.AppendUsage(t.Context(), string(encoded), "")
	if errAppend != nil {
		t.Fatal(errAppend)
	}
	assertQuotaGuard(t, repo, "scoped-feedback", "gemini-3-flash", "quota_exhausted")
	assertQuotaGuard(t, repo, "scoped-feedback", "claude-sonnet-4-5", "quota_exhausted")
	var feedback QuotaGuardRecord
	if errFind := repo.db.Where("credential_id = ? AND window_id = ?", "scoped-feedback", quotaFeedbackWindowID).Take(&feedback).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if feedback.Scope != "account" || feedback.ScopeID != "antigravity" {
		t.Fatalf("explicit feedback is not account-wide: %+v", feedback)
	}
	// Simulate rows persisted by the earlier scoped implementation.
	for _, bucket := range []string{"gemini", "third-party"} {
		if errSeed := upsertQuotaGuardRecord(repo.db, QuotaGuardRecord{CredentialID: "scoped-feedback",
			WindowID: quotaFeedbackWindowID, Scope: "model", ScopeID: bucket, Blocked: true, ObservedAt: start}); errSeed != nil {
			t.Fatal(errSeed)
		}
	}
	assertQuotaGuard(t, repo, "healthy-other-credential", "gemini-3-flash", "")
	gemini, thirdParty := "gemini", "third-party"
	windows := []QuotaWindow{
		{ID: "antigravity-gemini-5h", Scope: "model", ScopeID: &gemini, Status: "healthy", Source: "active_probe", ObservedAt: start.Add(time.Minute)},
		{ID: "antigravity-gemini-weekly", Scope: "model", ScopeID: &gemini, Status: "exhausted", Source: "active_probe", ObservedAt: start.Add(time.Minute)},
		{ID: "antigravity-3p-weekly", Scope: "model", ScopeID: &thirdParty, Status: "healthy", Source: "active_probe", ObservedAt: start.Add(time.Minute)},
	}
	writeProbe := func(source, status string) {
		t.Helper()
		for index := range windows {
			windows[index].Source = source
		}
		if _, errUpsert := repo.UpsertQuotaSnapshot(t.Context(), QuotaSnapshotWrite{CredentialID: "scoped-feedback",
			Source: source, QuotaStatus: "exhausted", CollectionStatus: status, ReplaceWindows: true, Windows: windows}); errUpsert != nil {
			t.Fatal(errUpsert)
		}
	}
	writeProbe("active_probe", "success")
	assertQuotaGuard(t, repo, "scoped-feedback", "gemini-3-flash", "quota_exhausted")
	windows[1].Status = "healthy"
	windows[2].Status = "exhausted"
	for index := range windows {
		windows[index].ObservedAt = start.Add(2 * time.Minute)
	}
	writeProbe("response_header", "success")
	assertQuotaGuard(t, repo, "scoped-feedback", "gemini-3-flash", "quota_exhausted")
	writeProbe("active_probe", "partial")
	// Per-window guards recover, but the explicit failure still needs completeness.
	assertQuotaGuard(t, repo, "scoped-feedback", "gemini-3-flash", "quota_exhausted")
	writeProbe("active_probe", "success")
	assertQuotaGuard(t, repo, "scoped-feedback", "gemini-3-flash", "quota_exhausted")
	assertQuotaGuard(t, repo, "scoped-feedback", "claude-sonnet-4-5", "quota_exhausted")
	if errFeedback := repo.RecordUsageQuotaFeedback(t.Context(), usage); errFeedback != nil {
		t.Fatal(errFeedback)
	}
	assertQuotaGuard(t, repo, "scoped-feedback", "gemini-3-flash", "quota_exhausted")
	// An original unknown failure also remains blocked by this mixed probe.
	unknown := quotaFeedbackPayload(t, "antigravity", "scoped-feedback", "unknown-domain", body, start.Add(3*time.Minute))
	if _, errUnknown := repo.AppendUsage(t.Context(), unknown, ""); errUnknown != nil {
		t.Fatal(errUnknown)
	}
	for index := range windows {
		windows[index].ObservedAt = start.Add(4 * time.Minute)
	}
	writeProbe("active_probe", "success")
	assertQuotaGuard(t, repo, "scoped-feedback", "gemini-3-flash", "quota_exhausted")
	// Mixed probes must not clear legacy scoped request latches either.
	var legacy []QuotaGuardRecord
	if errFind := repo.db.Where("credential_id = ? AND window_id = ? AND scope = ?", "scoped-feedback", quotaFeedbackWindowID, "model").Find(&legacy).Error; errFind != nil {
		t.Fatal(errFind)
	}
	if len(legacy) != 2 || !legacy[0].Blocked || !legacy[1].Blocked {
		t.Fatalf("mixed probe cleared legacy feedback: %+v", legacy)
	}
	for index := range windows {
		windows[index].Status = "healthy"
		windows[index].ObservedAt = start.Add(5 * time.Minute)
	}
	writeProbe("active_probe", "success")
	for _, model := range []string{"gemini-3-flash", "claude-sonnet-4-5", "unknown-model", ""} {
		assertQuotaGuard(t, NewRepository(repo.db), "scoped-feedback", model, "")
	}
	// Legacy keys retain recovery watermarks rather than being orphaned or deleted.
	for _, bucket := range []string{"gemini", "third-party"} {
		if errReplay := upsertQuotaGuardRecord(repo.db, QuotaGuardRecord{CredentialID: "scoped-feedback",
			WindowID: quotaFeedbackWindowID, Scope: "model", ScopeID: bucket, Blocked: true, ObservedAt: start}); errReplay != nil {
			t.Fatal(errReplay)
		}
	}
	if errReplay := repo.RecordUsageQuotaFeedback(t.Context(), usage); errReplay != nil {
		t.Fatal(errReplay)
	}
	assertQuotaGuard(t, repo, "scoped-feedback", "gemini-3-flash", "")
}

func TestRuntimeAdapterQuotaGuard(t *testing.T) {
	var disabled *RuntimeAdapter
	for _, adapter := range []*RuntimeAdapter{disabled, NewRuntimeAdapter(nil, "")} {
		if errCheck := adapter.CheckDispatchQuota(t.Context(), "auth", "alias"); errCheck == nil {
			t.Fatal("disabled adapter did not fail closed")
		} else if typed, ok := errCheck.(*coreauth.Error); !ok || typed.Code != "quota_guard_unavailable" {
			t.Fatal(fmt.Sprintf("unexpected error: %v", errCheck))
		}
	}
	repo, closeRepo := newBillingTestRepository(t, context.Background())
	defer closeRepo()
	adapter := NewRuntimeAdapter(repo, "")
	if errCheck := adapter.CheckDispatchQuota(t.Context(), "auth", "alias"); errCheck != nil {
		t.Fatal(errCheck)
	}
	writeGuardWindow(t, repo, "auth", "active_probe", "success", QuotaWindow{ID: "daily", Scope: "account", Status: "exhausted", ObservedAt: time.Now()})
	if typed, ok := adapter.CheckDispatchQuota(t.Context(), "auth", "alias").(*coreauth.Error); !ok || typed.Code != "quota_exhausted" {
		t.Fatal("adapter did not forward exhaustion")
	}
}
