package cluster

import (
	"context"
	"time"

	"gorm.io/gorm"
)

// OrganizationMemberResetAllowanceStatusAt uses the organization's timezone, not the personal user's.
func OrganizationMemberResetAllowanceStatusAt(member *OrganizationMemberRecord, now time.Time) UserResetAllowanceStatus {
	return UserResetAllowanceStatusAt(&UserRecord{
		Timezone:                member.ResetAllowanceTimezone,
		ResetAllowanceMode:      member.ResetAllowanceMode,
		ResetAllowanceAmount:    member.ResetAllowanceAmount,
		ResetAllowanceAvailable: member.ResetAllowanceAvailable,
		ResetAllowanceExpiresAt: member.ResetAllowanceExpiresAt,
		ResetAllowanceRenewsAt:  member.ResetAllowanceRenewsAt,
	}, now)
}

// SetOrganizationMemberResetAllowance replaces only this membership's policy.
func (r *Repository) SetOrganizationMemberResetAllowance(ctx context.Context, orgID, memberID uint, mode string, amount int64, expiresAt *time.Time, now time.Time) (*OrganizationMemberRecord, error) {
	if mode != ResetAllowanceOneTime && mode != ResetAllowanceMonthly {
		return nil, periodLimitFieldError("mode", "invalid_reset_allowance_mode", "mode must be one_time or monthly")
	}
	if amount < 0 {
		return nil, periodLimitFieldError("amount", "invalid_reset_allowance_amount", "amount must be a non-negative integer")
	}
	if mode == ResetAllowanceMonthly && expiresAt != nil {
		return nil, periodLimitFieldError("expires_at", "invalid_reset_allowance_expiry", "monthly allowance cannot expire")
	}
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	var member *OrganizationMemberRecord
	errTx := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		var errMember error
		member, errMember = getOrganizationMemberTx(ctx, tx, orgID, memberID, true)
		if errMember != nil {
			return errMember
		}
		var renewsAt *time.Time
		if mode == ResetAllowanceMonthly {
			next := startOfMonthInLocation(now, loadUserLocation(member.ResetAllowanceTimezone)).AddDate(0, 1, 0).UTC()
			renewsAt = &next
		}
		if errUpdate := tx.Model(member).Updates(map[string]any{
			"reset_allowance_mode": mode, "reset_allowance_amount": amount, "reset_allowance_available": amount,
			"reset_allowance_expires_at": expiresAt, "reset_allowance_renews_at": renewsAt,
		}).Error; errUpdate != nil {
			return errUpdate
		}
		member, errMember = getOrganizationMemberTx(ctx, tx, orgID, memberID, false)
		return errMember
	})
	return member, errTx
}

// ConsumeOrganizationMemberResetAllowance atomically resets every enabled member window.
// Historical charges, the organization's aggregate windows and personal limits are unchanged.
func (r *Repository) ConsumeOrganizationMemberResetAllowance(ctx context.Context, orgID, memberID uint, now time.Time) (*OrganizationMemberRecord, []OrganizationMemberSpendWindow, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, nil, errDB
	}
	now = now.UTC()
	var member *OrganizationMemberRecord
	var windows []OrganizationMemberSpendWindow
	errTx := db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		var errMember error
		member, errMember = getOrganizationMemberTx(ctx, tx, orgID, memberID, true)
		if errMember != nil {
			return errMember
		}
		if !organizationMemberLimitsEnabled(member) {
			return ErrResetAllowanceNoWindows
		}
		allowance := OrganizationMemberResetAllowanceStatusAt(member, now)
		if allowance.Available <= 0 {
			return ErrResetAllowanceUnavailable
		}
		if allowance.Mode == ResetAllowanceMonthly && (member.ResetAllowanceRenewsAt == nil || !now.Before(*member.ResetAllowanceRenewsAt)) {
			if errRenew := tx.Model(member).Updates(map[string]any{"reset_allowance_available": allowance.Amount, "reset_allowance_renews_at": allowance.RenewsAt}).Error; errRenew != nil {
				return errRenew
			}
		}
		updates := map[string]any{"reset_allowance_available": gorm.Expr("reset_allowance_available - 1")}
		for _, field := range organizationMemberWindowFields(member, nil, OrganizationMemberUpdate{}) {
			if *field.limit != nil {
				updates[windowStartColumn(field.id)] = nil
				updates[usageEpochColumn(field.id)] = now
			}
		}
		result := tx.Model(&OrganizationMemberRecord{}).Where("id = ? AND reset_allowance_available > 0", member.ID).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrResetAllowanceUnavailable
		}
		member, errMember = getOrganizationMemberTx(ctx, tx, orgID, memberID, false)
		if errMember != nil {
			return errMember
		}
		var errWindows error
		windows, errWindows = organizationMemberSpendWindowsDB(ctx, tx, member, now)
		return errWindows
	})
	if errTx != nil {
		return nil, nil, errTx
	}
	return member, windows, nil
}
