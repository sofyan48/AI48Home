package cluster

import (
	"context"
	"net/http"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// QuotaGuardRecord is independent of display snapshot freshness and replacement.
// Recovered rows retain an observation watermark to reject delayed exhaustion.
// ScopeID is non-null so the composite key behaves identically on both databases.
type QuotaGuardRecord struct {
	CredentialID string    `gorm:"column:credential_id;primaryKey;size:128"`
	WindowID     string    `gorm:"column:window_id;primaryKey;size:128"`
	Scope        string    `gorm:"column:scope;primaryKey;size:32"`
	ScopeID      string    `gorm:"column:scope_id;primaryKey;size:128;not null"`
	Blocked      bool      `gorm:"column:blocked;not null"`
	ObservedAt   time.Time `gorm:"column:observed_at;not null"`
}

func (QuotaGuardRecord) TableName() string { return "quota_guard" }

// CheckDispatchQuota returns nil only when no durable exhaustion is known.
// Any exhausted window blocks every model on the credential, regardless of scope.
// This is a check, not a reservation; it does not serialize dispatch with probes.
func (r *Repository) CheckDispatchQuota(ctx context.Context, credentialID, _ string) error {
	db, errDB := r.database()
	if errDB != nil || strings.TrimSpace(credentialID) == "" {
		return quotaGuardUnavailable()
	}
	var records []QuotaGuardRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).Where("credential_id = ? AND blocked = ?", strings.TrimSpace(credentialID), true).Find(&records).Error; errFind != nil {
		return quotaGuardUnavailable()
	}
	if len(records) > 0 {
		return &coreauth.Error{Code: "quota_exhausted", Message: "credential quota is exhausted; authoritative recovery is required", HTTPStatus: http.StatusTooManyRequests}
	}
	return nil
}

func quotaGuardUnavailable() *coreauth.Error {
	return &coreauth.Error{Code: "quota_guard_unavailable", Message: "durable quota guard is unavailable", Retryable: true, HTTPStatus: http.StatusServiceUnavailable}
}

func upsertQuotaGuardRecord(tx *gorm.DB, record QuotaGuardRecord) error {
	// At equal timestamps exhaustion wins, independently of arrival order.
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "credential_id"}, {Name: "window_id"}, {Name: "scope"}, {Name: "scope_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"blocked", "observed_at"}),
		Where: clause.Where{Exprs: []clause.Expression{clause.Expr{
			SQL:  `"quota_guard"."observed_at" < EXCLUDED."observed_at" OR ("quota_guard"."observed_at" = EXCLUDED."observed_at" AND EXCLUDED."blocked" = ?)`,
			Vars: []any{true},
		}}},
	}).Create(&record).Error
}

func updateQuotaGuards(tx *gorm.DB, input QuotaSnapshotWrite) error {
	windows := input.Windows
	if len(windows) == 0 && input.ObservedAt != nil && !input.ObservedAt.IsZero() {
		// Aggregate-only reports get their own key, never cleared by omission
		// from a later report containing windows.
		windows = []QuotaWindow{{ID: "", Scope: "unknown", Status: input.QuotaStatus, Source: input.Source, ObservedAt: *input.ObservedAt}}
	}
	for _, window := range windows {
		blocked := window.Status == "exhausted"
		recovery := window.Source == "active_probe" && input.Source == "active_probe" &&
			(input.CollectionStatus == "success" || input.CollectionStatus == "partial") &&
			(window.Status == "healthy" || window.Status == "low")
		if !blocked && !recovery {
			continue
		}
		scopeID := ""
		if window.ScopeID != nil {
			scopeID = *window.ScopeID
		}
		if errUpsert := upsertQuotaGuardRecord(tx, QuotaGuardRecord{
			CredentialID: input.CredentialID, WindowID: window.ID, Scope: window.Scope,
			ScopeID: scopeID, Blocked: blocked, ObservedAt: window.ObservedAt,
		}); errUpsert != nil {
			return errUpsert
		}
	}
	if errRecovery := recoverQuotaFeedback(tx, input); errRecovery != nil {
		return errRecovery
	}
	// Aggregate-only exhaustion can recover from a complete healthy active
	// replacement, but never from omission, a partial report, or headers.
	if input.Source == "active_probe" && input.CollectionStatus == "success" && input.ReplaceWindows && len(input.PreserveWindowPrefixes) == 0 && len(input.Windows) > 0 {
		observedAt := input.Windows[0].ObservedAt
		for _, window := range input.Windows {
			if window.Source != "active_probe" || (window.Status != "healthy" && window.Status != "low") {
				return nil
			}
			if window.ObservedAt.Before(observedAt) {
				observedAt = window.ObservedAt
			}
		}
		return upsertQuotaGuardRecord(tx, QuotaGuardRecord{CredentialID: input.CredentialID, Scope: "unknown", ObservedAt: observedAt})
	}
	return nil
}

// migrateQuotaGuards is idempotent and deliberately ignores expiry/reset times.
// Only exhausted legacy rows are seeded: healthy display headers are not proof
// of recovery, and must not clear guards imported from a portable snapshot.
func migrateQuotaGuards(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var windows []QuotaWindowRecord
		if errFind := tx.Where("status = ?", "exhausted").Find(&windows).Error; errFind != nil {
			return errFind
		}
		for _, window := range windows {
			scopeID := window.ScopeID
			if errUpsert := upsertQuotaGuardRecord(tx, QuotaGuardRecord{CredentialID: window.CredentialID, WindowID: window.WindowID,
				Scope: window.Scope, ScopeID: scopeID, Blocked: true, ObservedAt: window.ObservedAt}); errUpsert != nil {
				return errUpsert
			}
		}
		var snapshots []QuotaSnapshotRecord
		if errFind := tx.Where("quota_status = ? AND NOT EXISTS (SELECT 1 FROM quota_window WHERE quota_window.credential_id = quota_snapshot.credential_id)", "exhausted").Find(&snapshots).Error; errFind != nil {
			return errFind
		}
		for _, snapshot := range snapshots {
			observedAt := snapshot.UpdatedAt
			if snapshot.ObservedAt != nil {
				observedAt = *snapshot.ObservedAt
			}
			if errUpsert := upsertQuotaGuardRecord(tx, QuotaGuardRecord{CredentialID: snapshot.CredentialID, Scope: "unknown", Blocked: true, ObservedAt: observedAt}); errUpsert != nil {
				return errUpsert
			}
		}
		return nil
	})
}
