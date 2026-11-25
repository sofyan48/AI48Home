package cluster

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
)

func assertQuotaGuard(t *testing.T, repo *Repository, credentialID, model, code string) {
	t.Helper()
	errCheck := repo.CheckDispatchQuota(t.Context(), credentialID, model)
	if code == "" {
		if errCheck != nil {
			t.Fatalf("CheckDispatchQuota(%q, %q) = %v", credentialID, model, errCheck)
		}
		return
	}
	var quotaError *coreauth.Error
	if !errors.As(errCheck, &quotaError) || quotaError.Code != code {
		t.Fatalf("CheckDispatchQuota(%q, %q) = %#v, want %s", credentialID, model, errCheck, code)
	}
}

func writeGuardWindow(t *testing.T, repo *Repository, id, source, collection string, window QuotaWindow) {
	t.Helper()
	window.Source = source
	observedAt := window.ObservedAt
	if _, errUpsert := repo.UpsertQuotaSnapshot(t.Context(), QuotaSnapshotWrite{
		CredentialID: id, QuotaStatus: window.Status, Source: source, CollectionStatus: collection,
		ObservedAt: &observedAt, ReplaceWindows: true, Windows: []QuotaWindow{window},
	}); errUpsert != nil {
		t.Fatal(errUpsert)
	}
}

func TestQuotaGuardDurableWindowRecovery(t *testing.T) {
	repo, closeRepo := newBillingTestRepository(t, context.Background())
	defer closeRepo()
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	scopeID := "upstream-model"
	window := QuotaWindow{ID: "daily", Scope: "model", ScopeID: &scopeID, Status: "exhausted", ObservedAt: start, ResetAt: &start, ExpiresAt: &start}
	assertQuotaGuard(t, repo, "credential", "alias", "")
	writeGuardWindow(t, repo, "credential", "active_probe", "success", window)
	for _, model := range []string{"", "upstream-model", "alias", "unrelated-model"} {
		assertQuotaGuard(t, repo, "credential", model, "quota_exhausted")
	}

	assertQuotaGuard(t, repo, "other-credential", "alias", "")

	for index, status := range []string{"unknown", "error", "unsupported"} {
		window.Status, window.ObservedAt = status, start.Add(time.Duration(index+1)*time.Minute)
		writeGuardWindow(t, repo, "credential", "active_probe", "partial", window)
		assertQuotaGuard(t, repo, "credential", "upstream-model", "quota_exhausted")
	}
	// Replacement omission removes display windows, not the latch.
	omittedAt := start.Add(4 * time.Minute)
	if _, errUpsert := repo.UpsertQuotaSnapshot(t.Context(), QuotaSnapshotWrite{CredentialID: "credential", QuotaStatus: "healthy",
		CollectionStatus: "success", Source: "active_probe", ObservedAt: &omittedAt, ReplaceWindows: true}); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	assertQuotaGuard(t, repo, "credential", "upstream-model", "quota_exhausted")

	window.Status, window.ObservedAt = "healthy", start.Add(5*time.Minute)
	writeGuardWindow(t, repo, "credential", "response_header", "success", window)
	assertQuotaGuard(t, repo, "credential", "upstream-model", "quota_exhausted")
	// Neither a failed probe nor a different scope identity is recovery.
	window.ObservedAt = start.Add(6 * time.Minute)
	writeGuardWindow(t, repo, "credential", "active_probe", "failed", window)
	assertQuotaGuard(t, repo, "credential", "upstream-model", "quota_exhausted")
	otherScopeID := "other-model"
	window.ScopeID, window.ObservedAt = &otherScopeID, start.Add(7*time.Minute)
	writeGuardWindow(t, repo, "credential", "active_probe", "success", window)
	assertQuotaGuard(t, repo, "credential", "upstream-model", "quota_exhausted")
	window.ScopeID, window.Status, window.ObservedAt = &scopeID, "low", start.Add(8*time.Minute)
	writeGuardWindow(t, repo, "credential", "active_probe", "partial", window)
	assertQuotaGuard(t, repo, "credential", "alias", "")

	// Guard watermarks survive repository reconstruction and delayed reports.
	reopened := NewRepository(repo.db)
	window.Status, window.ObservedAt = "exhausted", start.Add(time.Minute)
	writeGuardWindow(t, reopened, "credential", "active_probe", "success", window)
	assertQuotaGuard(t, reopened, "credential", "alias", "")
}

func TestQuotaGuardOrderingAndIndependentWindows(t *testing.T) {
	repo, closeRepo := newBillingTestRepository(t, context.Background())
	defer closeRepo()
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, firstStatus := range []string{"healthy", "exhausted"} {
		t.Run(firstStatus, func(t *testing.T) {
			window := QuotaWindow{ID: "daily", Scope: "account", Status: firstStatus, ObservedAt: start}
			writeGuardWindow(t, repo, firstStatus, "active_probe", "success", window)
			if firstStatus == "healthy" {
				window.Status = "exhausted"
			} else {
				window.Status = "healthy"
			}
			writeGuardWindow(t, repo, firstStatus, "active_probe", "success", window)
			assertQuotaGuard(t, repo, firstStatus, "", "quota_exhausted")
			// A newer healthy window must not mask a delayed different window.
			window.Status, window.ObservedAt = "healthy", start.Add(10*time.Minute)
			writeGuardWindow(t, repo, firstStatus, "active_probe", "success", window)
			window.ID, window.Status, window.ObservedAt = "weekly", "exhausted", start.Add(time.Minute)
			writeGuardWindow(t, repo, firstStatus, "active_probe", "success", window)
			assertQuotaGuard(t, repo, firstStatus, "", "quota_exhausted")
			window.Status, window.ObservedAt, window.IsUnlimited = "unknown", start.Add(11*time.Minute), true
			writeGuardWindow(t, repo, firstStatus, "active_probe", "success", window)
			assertQuotaGuard(t, repo, firstStatus, "", "")
		})
	}
}

func TestQuotaGuardUsageHeadersCannotRecover(t *testing.T) {
	repo, closeRepo := newBillingTestRepository(t, context.Background())
	defer closeRepo()
	seedQuotaSnapshotAuth(t, repo, "codex-guard", "codex", "Quota guard", map[string]any{"type": "codex"})
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for index, percent := range []int{100, 10} {
		payload := fmt.Sprintf(`{"timestamp":%q,"provider":"codex","auth_type":"oauth","auth_index":"codex-guard","request_id":%q,"response_headers":{"X-Codex-Active-Limit":["premium"],"X-Codex-Primary-Used-Percent":[%q],"X-Codex-Primary-Window-Minutes":["300"],"X-Codex-Primary-Reset-After-Seconds":["60"]}}`,
			start.Add(time.Duration(index)*time.Minute).Format(time.RFC3339Nano), fmt.Sprintf("guard-%d", index), fmt.Sprint(percent))
		if _, errAppend := repo.AppendUsageWithRuntime(t.Context(), payload, UsageRuntimeMetadata{}); errAppend != nil {
			t.Fatal(errAppend)
		}
		assertQuotaGuard(t, repo, "codex-guard", "public-alias", "quota_exhausted")
	}
	window := QuotaWindow{ID: "codex-5-hour", Scope: "account", Status: "healthy", ObservedAt: start.Add(2 * time.Minute)}
	writeGuardWindow(t, repo, "codex-guard", "active_probe", "success", window)
	assertQuotaGuard(t, repo, "codex-guard", "public-alias", "")
}

func TestQuotaGuardUsageFailurePreservesCoreIngestion(t *testing.T) {
	repo, closeRepo := newBillingTestRepository(t, context.Background())
	defer closeRepo()
	seedQuotaSnapshotAuth(t, repo, "codex-atomic", "codex", "Atomic quota", map[string]any{"type": "codex"})
	payload := `{"timestamp":"2026-07-16T01:00:00Z","provider":"codex","auth_type":"oauth","auth_index":"codex-atomic","request_id":"atomic-quota","response_headers":{"X-Codex-Active-Limit":["premium"],"X-Codex-Primary-Used-Percent":["100"],"X-Codex-Primary-Window-Minutes":["300"],"X-Codex-Primary-Reset-After-Seconds":["60"]}}`
	if errDrop := repo.db.Migrator().DropTable(&QuotaGuardRecord{}); errDrop != nil {
		t.Fatal(errDrop)
	}
	if _, errAppend := repo.AppendUsageWithRuntime(t.Context(), payload, UsageRuntimeMetadata{}); errAppend != nil {
		t.Fatal(errAppend)
	}
	var count int64
	if errCount := repo.db.Model(&UsageRecord{}).Where("request_id = ?", "atomic-quota").Count(&count).Error; errCount != nil || count != 1 {
		t.Fatalf("committed usage count = %d, error = %v", count, errCount)
	}
	assertQuotaGuard(t, repo, "codex-atomic", "", "quota_guard_unavailable")
}

func TestQuotaGuardUnavailable(t *testing.T) {
	assertQuotaGuard(t, nil, "credential", "", "quota_guard_unavailable")
	repo, closeRepo := newBillingTestRepository(t, context.Background())
	defer closeRepo()
	assertQuotaGuard(t, repo, "", "", "quota_guard_unavailable")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var quotaError *coreauth.Error
	if errCheck := repo.CheckDispatchQuota(ctx, "credential", ""); !errors.As(errCheck, &quotaError) || quotaError.Code != "quota_guard_unavailable" {
		t.Fatalf("canceled check = %v", errCheck)
	}
	if errDrop := repo.db.Migrator().DropTable(&QuotaGuardRecord{}); errDrop != nil {
		t.Fatal(errDrop)
	}
	assertQuotaGuard(t, repo, "credential", "", "quota_guard_unavailable")
}

func TestQuotaGuardMigrationAndSnapshot(t *testing.T) {
	repo, closeRepo := newBillingTestRepository(t, context.Background())
	defer closeRepo()
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	legacy := QuotaWindowRecord{CredentialID: "legacy", WindowID: "daily", Scope: "account", Status: "exhausted", ObservedAt: start, ExpiresAt: &start, ResetAt: &start}
	if errCreate := repo.db.Create(&legacy).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	for range 2 {
		if errMigrate := AutoMigrate(repo.db); errMigrate != nil {
			t.Fatal(errMigrate)
		}
	}
	assertQuotaGuard(t, repo, "legacy", "", "quota_exhausted")
	// Remove the display row: only the portable durable guard remains.
	if errDelete := repo.db.Where("credential_id = ?", "legacy").Delete(&QuotaWindowRecord{}).Error; errDelete != nil {
		t.Fatal(errDelete)
	}
	// A recovered watermark must survive migration and portable restoration too.
	if errRecover := upsertQuotaGuardRecord(repo.db, QuotaGuardRecord{CredentialID: "recovered", WindowID: "daily", Scope: "account", ObservedAt: start.Add(time.Minute)}); errRecover != nil {
		t.Fatal(errRecover)
	}
	legacy.CredentialID = "recovered"
	if errCreate := repo.db.Create(&legacy).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if errMigrate := AutoMigrate(repo.db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	assertQuotaGuard(t, repo, "recovered", "", "")
	if errDelete := repo.db.Where("credential_id = ?", "recovered").Delete(&QuotaWindowRecord{}).Error; errDelete != nil {
		t.Fatal(errDelete)
	}
	path := filepath.Join(t.TempDir(), "quota.zip")
	if _, errExport := ExportDatabaseSnapshot(t.Context(), repo.db, DatabaseSnapshotExportOptions{Path: path}); errExport != nil {
		t.Fatal(errExport)
	}
	snapshot, errOpen := OpenDatabaseSnapshot(t.Context(), path)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	defer func() {
		if errClose := snapshot.Close(); errClose != nil {
			t.Error(errClose)
		}
	}()
	restored, closeRestored := newBillingTestRepository(t, context.Background())
	defer closeRestored()
	if _, errImport := ImportDatabaseSnapshot(t.Context(), restored.db, snapshot, nil); errImport != nil {
		t.Fatal(errImport)
	}
	assertQuotaGuard(t, restored, "legacy", "alias", "quota_exhausted")
	if errDelayed := upsertQuotaGuardRecord(restored.db, QuotaGuardRecord{CredentialID: "recovered", WindowID: "daily", Scope: "account", Blocked: true, ObservedAt: start}); errDelayed != nil {
		t.Fatal(errDelayed)
	}
	assertQuotaGuard(t, restored, "recovered", "", "")
}
