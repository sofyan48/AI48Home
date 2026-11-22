package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	homeerrors "github.com/router-for-me/CLIProxyAPIHome/internal/errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// defaultOrganizationMemberWindowMode keeps member limits rolling unless the
// owner picks another mode; it was the only behavior before window modes.
const defaultOrganizationMemberWindowMode = PeriodWindowModeSliding

// PeriodLimitErrExceedsOrganization rejects a member limit above the organization limit.
const PeriodLimitErrExceedsOrganization = "exceeds_organization_limit"

type organizationMemberWindowField struct {
	id        string
	limit     **float64
	mode      *string
	start     **time.Time
	update    OptionalFloatUpdate
	newMode   *string
	orgLimit  *float64
	limitName string
	modeName  string
}

func organizationMemberWindowFields(record *OrganizationMemberRecord, org *UserRecord, update OrganizationMemberUpdate) []organizationMemberWindowField {
	fields := []organizationMemberWindowField{
		{id: PeriodWindow5h, limit: &record.Limit5hCredits, mode: &record.WindowMode5h, start: &record.PeriodWindowStart5h, update: update.Limit5hCredits, newMode: update.WindowMode5h, limitName: "limit_5h_credits", modeName: "window_mode_5h"},
		{id: PeriodWindow1d, limit: &record.Limit1dCredits, mode: &record.WindowMode1d, start: &record.PeriodWindowStart1d, update: update.Limit1dCredits, newMode: update.WindowMode1d, limitName: "limit_1d_credits", modeName: "window_mode_1d"},
		{id: PeriodWindow7d, limit: &record.Limit7dCredits, mode: &record.WindowMode7d, start: &record.PeriodWindowStart7d, update: update.Limit7dCredits, newMode: update.WindowMode7d, limitName: "limit_7d_credits", modeName: "window_mode_7d"},
		{id: PeriodWindow30d, limit: &record.Limit30dCredits, mode: &record.WindowMode30d, start: &record.PeriodWindowStart30d, update: update.Limit30dCredits, newMode: update.WindowMode30d, limitName: "limit_30d_credits", modeName: "window_mode_30d"},
	}
	if org != nil {
		fields[0].orgLimit = org.Limit5hCredits
		fields[1].orgLimit = org.Limit1dCredits
		fields[2].orgLimit = org.Limit7dCredits
		fields[3].orgLimit = org.Limit30dCredits
	}
	return fields
}

// normalizeOrganizationMemberWindowMode maps empty legacy values to sliding.
func normalizeOrganizationMemberWindowMode(mode string, allowCalendar bool) (string, error) {
	if strings.TrimSpace(mode) == "" {
		return defaultOrganizationMemberWindowMode, nil
	}
	return normalizePeriodWindowMode(mode, allowCalendar)
}

// CanonicalOrganizationMemberWindowMode returns the stored member mode for API responses.
func CanonicalOrganizationMemberWindowMode(mode string, window string) string {
	normalized, errMode := normalizeOrganizationMemberWindowMode(mode, window != PeriodWindow5h)
	if errMode != nil {
		return defaultOrganizationMemberWindowMode
	}
	return normalized
}

// applyOrganizationMemberLimits applies a member limit patch. A newly set limit
// may not exceed the organization's own limit for the same window, because
// the organization limit would block the member first anyway.
func applyOrganizationMemberLimits(record *OrganizationMemberRecord, org *UserRecord, update OrganizationMemberUpdate) error {
	for _, field := range organizationMemberWindowFields(record, org, update) {
		if errLimit := applyOptionalFloat(field.update, field.limit); errLimit != nil {
			return periodLimitFieldError(field.limitName, PeriodLimitErrInvalidLimit, fmt.Sprintf("%s: %s", field.limitName, errLimit.Error()))
		}
		if field.update.Set && !field.update.Clear && field.orgLimit != nil && field.update.Value > *field.orgLimit {
			return periodLimitFieldError(field.limitName, PeriodLimitErrExceedsOrganization,
				fmt.Sprintf("%s: must not exceed the organization limit of %g", field.limitName, *field.orgLimit))
		}
		if field.newMode == nil {
			continue
		}
		mode, errMode := normalizeOrganizationMemberWindowMode(*field.newMode, field.id != PeriodWindow5h)
		if errMode != nil {
			return periodLimitFieldError(field.modeName, PeriodLimitErrInvalidWindowMode, fmt.Sprintf("%s: %s", field.modeName, errMode.Error()))
		}
		if mode != *field.mode {
			// A mode change starts a fresh first_use session instead of reusing stale state.
			*field.start = nil
		}
		*field.mode = mode
	}
	return nil
}

// OrganizationMemberLimitFields returns the member limit fields shared by the
// Management and User API responses.
func OrganizationMemberLimitFields(member *OrganizationMemberRecord) map[string]any {
	return map[string]any{
		"limit_5h_credits":  member.Limit5hCredits,
		"window_mode_5h":    CanonicalOrganizationMemberWindowMode(member.WindowMode5h, PeriodWindow5h),
		"limit_1d_credits":  member.Limit1dCredits,
		"window_mode_1d":    CanonicalOrganizationMemberWindowMode(member.WindowMode1d, PeriodWindow1d),
		"limit_7d_credits":  member.Limit7dCredits,
		"window_mode_7d":    CanonicalOrganizationMemberWindowMode(member.WindowMode7d, PeriodWindow7d),
		"limit_30d_credits": member.Limit30dCredits,
		"window_mode_30d":   CanonicalOrganizationMemberWindowMode(member.WindowMode30d, PeriodWindow30d),
		"model_groups":      OrganizationMemberModelGroupsOf(member),
		"max_api_keys":      member.MaxAPIKeys,
	}
}

func organizationMemberLimitsEnabled(member *OrganizationMemberRecord) bool {
	return member != nil && (member.Limit5hCredits != nil || member.Limit1dCredits != nil ||
		member.Limit7dCredits != nil || member.Limit30dCredits != nil)
}

func organizationMemberWindowSubject(member *OrganizationMemberRecord, org *UserRecord) periodWindowSubject {
	subject := periodWindowSubject{
		userID:      member.OrgUserID,
		actorUserID: &member.MemberUserID,
		windows: map[string]periodWindowConfig{
			PeriodWindow5h:  {limit: member.Limit5hCredits, mode: CanonicalOrganizationMemberWindowMode(member.WindowMode5h, PeriodWindow5h), start: member.PeriodWindowStart5h},
			PeriodWindow1d:  {limit: member.Limit1dCredits, mode: CanonicalOrganizationMemberWindowMode(member.WindowMode1d, PeriodWindow1d), start: member.PeriodWindowStart1d},
			PeriodWindow7d:  {limit: member.Limit7dCredits, mode: CanonicalOrganizationMemberWindowMode(member.WindowMode7d, PeriodWindow7d), start: member.PeriodWindowStart7d},
			PeriodWindow30d: {limit: member.Limit30dCredits, mode: CanonicalOrganizationMemberWindowMode(member.WindowMode30d, PeriodWindow30d), start: member.PeriodWindowStart30d},
		},
		weekResetDay:  DefaultWeekResetDay,
		weekResetHour: DefaultWeekResetHour,
	}
	if org != nil {
		// Calendar windows follow the organization's timezone and week reset.
		defaultUserPeriodLimitFields(org)
		subject.timezone = org.Timezone
		subject.weekResetDay = org.WeekResetDay
		subject.weekResetHour = org.WeekResetHour
	}
	return subject
}

func evaluateOrganizationMemberWindows(ctx context.Context, db *gorm.DB, member *OrganizationMemberRecord, now time.Time) ([]periodWindowEval, error) {
	org, errOrg := getOrganizationTx(ctx, db, member.OrgUserID)
	if errOrg != nil {
		return nil, errOrg
	}
	return evaluatePeriodWindows(ctx, db, organizationMemberWindowSubject(member, org), now)
}

// OrganizationMemberSpend returns the spend windows for one member.
func (r *Repository) OrganizationMemberSpend(ctx context.Context, orgID, memberID uint, now time.Time) ([]OrganizationMemberSpendWindow, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	member, errMember := getOrganizationMemberTx(ctx, db, orgID, memberID, false)
	if errMember != nil {
		return nil, errMember
	}
	return organizationMemberSpendWindowsDB(ctx, db, member, now)
}

func organizationMemberSpendWindowsDB(ctx context.Context, db *gorm.DB, member *OrganizationMemberRecord, now time.Time) ([]OrganizationMemberSpendWindow, error) {
	evals, errEval := evaluateOrganizationMemberWindows(ctx, db, member, now)
	if errEval != nil {
		return nil, errEval
	}
	out := make([]OrganizationMemberSpendWindow, 0, len(evals))
	for _, eval := range evals {
		out = append(out, OrganizationMemberSpendWindow{
			ID:        eval.ID,
			Limit:     eval.Limit,
			Used:      eval.Used,
			Remaining: remainingCredits(eval.Limit, eval.Used),
			Mode:      eval.Mode,
			Active:    eval.Active,
			ResetAt:   cloneTimePtr(eval.ResetAt),
		})
	}
	return out, nil
}

// ensureOrganizationMemberKeyAllowed admits an organization key only while its
// creator is still a member and within their spend limits.
func ensureOrganizationMemberKeyAllowed(ctx context.Context, db *gorm.DB, record *APIKeyRecord, orgID uint) error {
	memberID := normalizeOptionalUserID(record.CreatedByUserID)
	if memberID == nil {
		// Administrator-created organization keys only use organization limits.
		return nil
	}
	member, errMember := getOrganizationMemberTx(ctx, db, orgID, *memberID, false)
	if errors.Is(errMember, ErrOrganizationMemberNotFound) {
		// Removal deletes keys; a stale key racing that delete must not bill the organization.
		return fmt.Errorf("permission_denied: %s", homeerrors.MessageOrganizationMembershipRevoked)
	}
	if errMember != nil {
		return errMember
	}
	if !organizationMemberLimitsEnabled(member) {
		return nil
	}
	evals, errEval := evaluateOrganizationMemberWindows(ctx, db, member, time.Now())
	if errEval != nil {
		return errEval
	}
	for _, eval := range evals {
		if eval.Exceeded {
			return periodLimitExceededErrorWithMessage(eval, homeerrors.MessageOrganizationMemberLimitExceeded)
		}
	}
	return nil
}

// openOrganizationMemberFirstUseWindowsOnChargeTx opens the member's inactive
// first_use windows when one of their organization charges lands.
func openOrganizationMemberFirstUseWindowsOnChargeTx(ctx context.Context, tx *gorm.DB, orgID, memberID uint, chargeTime time.Time) error {
	member := &OrganizationMemberRecord{}
	errFirst := tx.WithContext(contextOrBackground(ctx)).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("org_user_id = ? AND member_user_id = ?", orgID, memberID).
		First(member).Error
	if errors.Is(errFirst, gorm.ErrRecordNotFound) {
		// The member left after the request was admitted; the charge still bills the organization.
		return nil
	}
	if errFirst != nil {
		return errFirst
	}
	if !organizationMemberLimitsEnabled(member) {
		return nil
	}
	evals, errEval := evaluateOrganizationMemberWindows(ctx, tx, member, chargeTime)
	if errEval != nil {
		return errEval
	}
	updates := map[string]any{}
	for _, eval := range evals {
		if eval.Mode == PeriodWindowModeFirstUse && eval.NeedOpen {
			updates[windowStartColumn(eval.ID)] = chargeTime.UTC()
		}
	}
	if len(updates) == 0 {
		return nil
	}
	return tx.WithContext(contextOrBackground(ctx)).
		Model(&OrganizationMemberRecord{}).
		Where("id = ?", member.ID).
		Updates(updates).Error
}
