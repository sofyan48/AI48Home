package cluster

import (
	"context"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
)

func TestQuotaGuardCodexNamedHeaderMatchesProbeIdentity(t *testing.T) {
	repo, closeRepo := newBillingTestRepository(t, context.Background())
	defer closeRepo()
	seedQuotaSnapshotAuth(t, repo, "named-codex", "codex", "Named bucket", map[string]any{"type": "codex"})
	payload := `{"provider":"codex","auth_index":"named-codex","auth_type":"oauth","request_id":"named-header","timestamp":"2026-07-17T00:00:00Z","response_headers":{"X-Codex-Active-Limit":["code_review"],"X-Codex-Primary-Used-Percent":["100"],"X-Codex-Primary-Window-Minutes":["300"],"X-Codex-Primary-Reset-After-Seconds":["60"]}}`
	if _, errAppend := repo.AppendUsage(t.Context(), payload, ""); errAppend != nil {
		t.Fatal(errAppend)
	}
	assertQuotaGuard(t, repo, "named-codex", "alias", "quota_exhausted")
	at := time.Date(2026, 7, 17, 0, 1, 0, 0, time.UTC)
	// Exact identity emitted by internal/quota/codex.go for code-review.
	scopeID := "codex_code_review"
	window := QuotaWindow{ID: "codex-code-review-5-hour", Scope: "model", ScopeID: &scopeID, Status: "healthy", ObservedAt: at}
	wrongScope := window
	wrongScope.ScopeID = nil
	writeGuardWindow(t, repo, "named-codex", "active_probe", "success", wrongScope)
	assertQuotaGuard(t, repo, "named-codex", "alias", "quota_exhausted")
	writeGuardWindow(t, repo, "named-codex", "active_probe", "success", window)
	assertQuotaGuard(t, repo, "named-codex", "alias", "")
}

func TestQuotaGuardIdentityLifecycle(t *testing.T) {
	for _, operation := range []string{"upsert", "mutate", "patch", "delete"} {
		t.Run(operation, func(t *testing.T) {
			repo, closeRepo := newBillingTestRepository(t, context.Background())
			defer closeRepo()
			id := "guard-identity"
			seedQuotaSnapshotAuth(t, repo, id, "codex", "Identity", map[string]any{"type": "codex"})
			writeGuardWindow(t, repo, id, "active_probe", "success", QuotaWindow{ID: "codex-5-hour", Scope: "account", Status: "exhausted", ObservedAt: time.Now().UTC()})
			// Ordinary mutations, including refresh state, must preserve the guard.
			if _, _, _, errMutate := repo.MutateAuth(t.Context(), id, "test", func(auth *coreauth.Auth) bool {
				auth.Label = "Refreshed"
				return true
			}); errMutate != nil {
				t.Fatal(errMutate)
			}
			assertQuotaGuard(t, repo, id, "", "quota_exhausted")
			switch operation {
			case "upsert":
				seedQuotaSnapshotAuth(t, repo, id, "claude", "Replacement", map[string]any{"type": "claude"})
			case "mutate":
				if _, _, _, errMutate := repo.MutateAuth(t.Context(), id, "test", func(auth *coreauth.Auth) bool {
					auth.Provider = "claude"
					auth.Metadata["type"] = "claude"
					return true
				}); errMutate != nil {
					t.Fatal(errMutate)
				}
			case "patch":
				if errPatch := repo.db.Transaction(func(tx *gorm.DB) error {
					return repo.patchAuthTx(t.Context(), tx, id, &coreauth.Auth{ID: id, Index: id, Provider: "claude", Metadata: map[string]any{"type": "claude"}})
				}); errPatch != nil {
					t.Fatal(errPatch)
				}
			case "delete":
				if errDelete := repo.SoftDeleteAuth(t.Context(), id); errDelete != nil {
					t.Fatal(errDelete)
				}
			}
			assertQuotaGuard(t, repo, id, "", "")
		})
	}
}

func TestQuotaGuardBlockedCredentialKeepsProbing(t *testing.T) {
	repo, closeRepo := newBillingTestRepository(t, context.Background())
	defer closeRepo()
	id := "guard-probe"
	seedQuotaSnapshotAuth(t, repo, id, "codex", "Recovery", map[string]any{"type": "codex"})
	at := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	window := QuotaWindow{ID: "codex-5-hour", Scope: "account", Status: "exhausted", ObservedAt: at}
	writeGuardWindow(t, repo, id, "active_probe", "success", window)
	if errVersion := repo.db.Model(&QuotaSnapshotRecord{}).Where("credential_id = ?", id).Update("collector_version", QuotaSnapshotVersion("codex")).Error; errVersion != nil {
		t.Fatal(errVersion)
	}
	if claimed, errClaim := repo.ClaimEligibleQuotaProbe(t.Context(), id, "home", at.Add(time.Minute), time.Minute); errClaim != nil || claimed {
		t.Fatalf("fresh blocked snapshot claim = %v, %v", claimed, errClaim)
	}
	later := at.Add(2 * time.Hour)
	if claimed, errClaim := repo.ClaimEligibleQuotaProbe(t.Context(), id, "home", later, time.Minute); errClaim != nil || !claimed {
		t.Fatalf("blocked credential without usage claim = %v, %v", claimed, errClaim)
	}
	window.Status, window.ObservedAt = "healthy", later
	writeGuardWindow(t, repo, id, "active_probe", "success", window)
	assertQuotaGuard(t, repo, id, "", "")
	if claimed, errClaim := repo.ClaimEligibleQuotaProbe(t.Context(), id, "home", later.Add(2*time.Hour), time.Minute); errClaim != nil || claimed {
		t.Fatalf("recovered idle credential claim = %v, %v", claimed, errClaim)
	}
}

func TestQuotaGuardAggregateRecoveryRequiresCompleteWindows(t *testing.T) {
	repo, closeRepo := newBillingTestRepository(t, context.Background())
	defer closeRepo()
	at := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	if _, errUpsert := repo.UpsertQuotaSnapshot(t.Context(), QuotaSnapshotWrite{CredentialID: "aggregate", QuotaStatus: "exhausted", Source: "active_probe", CollectionStatus: "success", ObservedAt: &at}); errUpsert != nil {
		t.Fatal(errUpsert)
	}
	window := QuotaWindow{ID: "daily", Scope: "account", Status: "healthy", ObservedAt: at.Add(time.Minute)}
	writeGuardWindow(t, repo, "aggregate", "active_probe", "partial", window)
	assertQuotaGuard(t, repo, "aggregate", "", "quota_exhausted")
	writeGuardWindow(t, repo, "aggregate", "response_header", "success", window)
	assertQuotaGuard(t, repo, "aggregate", "", "quota_exhausted")
	writeGuardWindow(t, repo, "aggregate", "active_probe", "success", window)
	assertQuotaGuard(t, repo, "aggregate", "", "")
}
