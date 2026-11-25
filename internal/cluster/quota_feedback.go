package cluster

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const quotaFeedbackWindowID = "request-quota-exhaustion"

// explicitQuotaFailureProvider deliberately follows the structured provider
// contracts used by core auth's retry-hint parser, not generic HTTP throttling.
func explicitQuotaFailureProvider(payload string) string {
	if !gjson.Get(payload, "failed").Bool() || gjson.Get(payload, "fail.status_code").Int() != http.StatusTooManyRequests {
		return ""
	}
	body := gjson.Get(payload, "fail.body").String()
	if !gjson.Valid(body) {
		return ""
	}
	provider := normalizeQuotaProviderID(gjson.Get(payload, "provider").String())
	switch provider {
	case "codex":
		if gjson.Get(body, "error.type").String() == "usage_limit_reached" {
			return provider
		}
	case "antigravity":
		for _, detail := range gjson.Get(body, "error.details").Array() {
			if detail.Get("@type").String() == "type.googleapis.com/google.rpc.ErrorInfo" && detail.Get("reason").String() == "QUOTA_EXHAUSTED" {
				return provider
			}
		}
	}
	return ""
}

// persistExplicitQuotaFeedback runs in the core usage transaction. Identity
// captured at usage resolution is checked under a credential lock, including on
// redelivery. It never trusts a caller-supplied fail.code or model alias.
func persistExplicitQuotaFeedback(tx *gorm.DB, usage *UsageRecord) error {
	provider := explicitQuotaFailureProvider(string(usage.PayloadJSON))
	if provider == "" || usage.QuotaCredentialID == "" || usage.QuotaIdentityVersion <= 0 {
		return nil
	}
	var authRecord AuthRecord
	query := tx.Where("uuid = ?", usage.QuotaCredentialID)
	if tx.Dialector.Name() == "postgres" {
		query = query.Clauses(clause.Locking{Strength: "SHARE"})
	}
	if errFind := query.First(&authRecord).Error; errFind != nil {
		if errors.Is(errFind, gorm.ErrRecordNotFound) {
			return nil
		}
		return errFind
	}
	if quotaCredentialIdentityVersion(authRecord) != usage.QuotaIdentityVersion || normalizeQuotaProviderID(authRecord.Provider) != provider {
		return nil
	}
	auth, errAuth := quotaAuthFromRecord(&authRecord)
	if errAuth != nil {
		return errAuth
	}
	if usage.QuotaIdentityKey != quotaUsageIdentityKey(provider, quotaCredentialType(auth)) {
		return nil
	}
	// Auth row timestamps also advance on refreshes and unrelated in-flight
	// results. Only the identity fences above may reject an exhausted signal;
	// its timestamp orders it against authoritative recovery, not auth writes.
	observedAt, errTime := time.Parse(time.RFC3339Nano, gjson.Get(string(usage.PayloadJSON), "timestamp").String())
	if errTime != nil {
		return nil
	}
	if observedAt.After(usage.CreatedAt) {
		observedAt = usage.CreatedAt
	}
	return upsertQuotaGuardRecord(tx, QuotaGuardRecord{CredentialID: authRecord.UUID,
		WindowID: quotaFeedbackWindowID, Scope: "account", ScopeID: provider, Blocked: true, ObservedAt: observedAt.UTC()})
}

// recoverQuotaFeedback requires a complete authoritative view of the provider's
// quota domain, since a request failure cannot identify which window failed.
// Legacy scoped request latches follow the same credential-wide recovery policy.
func recoverQuotaFeedback(tx *gorm.DB, input QuotaSnapshotWrite) error {
	if input.Source != "active_probe" || input.CollectionStatus != "success" || !input.ReplaceWindows || len(input.PreserveWindowPrefixes) != 0 || len(input.Windows) == 0 {
		return nil
	}

	for _, provider := range []string{"codex", "antigravity"} {
		observedAt := time.Time{}
		matches := true
		hasBaseDomain := false
		for _, window := range input.Windows {
			if !strings.HasPrefix(window.ID, provider+"-") || window.Source != "active_probe" || (window.Status != "healthy" && window.Status != "low") {
				matches = false
				break
			}
			if provider == "codex" && window.Scope == "account" && window.ScopeID == nil && (window.ID == "codex-5-hour" || window.ID == "codex-1-week") {
				hasBaseDomain = true
			}
			if provider == "antigravity" && (window.ID == "antigravity-gemini-5h" || window.ID == "antigravity-3p-5h" || window.ID == "antigravity-gemini-weekly" || window.ID == "antigravity-3p-weekly") {
				hasBaseDomain = true
			}
			if observedAt.IsZero() || window.ObservedAt.Before(observedAt) {
				observedAt = window.ObservedAt
			}
		}
		if matches && hasBaseDomain {
			if errUpsert := upsertQuotaGuardRecord(tx, QuotaGuardRecord{CredentialID: input.CredentialID,
				WindowID: quotaFeedbackWindowID, Scope: "account", ScopeID: provider, ObservedAt: observedAt}); errUpsert != nil {
				return errUpsert
			}
			if provider == "antigravity" {
				// Keep watermarks for both legacy keys so delayed feedback cannot relatch them.
				for _, bucket := range []string{"gemini", "third-party"} {
					if errUpsert := upsertQuotaGuardRecord(tx, QuotaGuardRecord{CredentialID: input.CredentialID,
						WindowID: quotaFeedbackWindowID, Scope: "model", ScopeID: bucket, ObservedAt: observedAt}); errUpsert != nil {
						return errUpsert
					}
				}
			}
		}
	}
	return nil
}

// RecordUsageQuotaFeedback retries explicit feedback from a trusted repository-
// returned usage record using its original identity fence, not a newly resolved
// identity. Callers must not construct this record from untrusted wire fields.
func (r *Repository) RecordUsageQuotaFeedback(ctx context.Context, usage *UsageRecord) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	if usage == nil {
		return nil
	}
	return db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		return persistExplicitQuotaFeedback(tx, usage)
	})
}
