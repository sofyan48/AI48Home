package cluster

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	homeerrors "github.com/router-for-me/CLIProxyAPIHome/internal/errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	// UserKindPersonal is a login-capable user account.
	UserKindPersonal = "personal"
	// UserKindOrganization is a shared billing account that cannot log in.
	UserKindOrganization = "organization"

	// OrganizationRoleOwner manages members, keys, limits and billing.
	OrganizationRoleOwner = "owner"
	// OrganizationRoleMember uses organization credits through their own keys.
	OrganizationRoleMember = "member"
)

var (
	// ErrOrganizationNotFound indicates that the organization account does not exist.
	ErrOrganizationNotFound = errors.New("organization not found")
	// ErrOrganizationMemberNotFound indicates that the user is not a member.
	ErrOrganizationMemberNotFound = errors.New("organization member not found")
	// ErrOrganizationMemberExists indicates that the user is already a member.
	ErrOrganizationMemberExists = errors.New("user is already an organization member")
	// ErrOrganizationLastOwner protects the last owner from removal or demotion.
	ErrOrganizationLastOwner = errors.New("organization must keep at least one owner")
	// ErrOrganizationInvalidMember rejects organization accounts as members.
	ErrOrganizationInvalidMember = errors.New("member must be a personal user")
	// ErrOrganizationInvalidRole rejects unknown roles.
	ErrOrganizationInvalidRole = errors.New("invalid organization role")
)

// OrganizationMemberRecord links a personal user to an organization account.
type OrganizationMemberRecord struct {
	ID           uint   `gorm:"column:id;primaryKey;autoIncrement"`
	OrgUserID    uint   `gorm:"column:org_user_id;not null;uniqueIndex:idx_organization_member_pair,priority:1"`
	MemberUserID uint   `gorm:"column:member_user_id;not null;uniqueIndex:idx_organization_member_pair,priority:2;index"`
	Role         string `gorm:"column:role;type:varchar(16);not null"`
	// Spend limits are rolling credit budgets for this member; NULL disables a window.
	Limit1dCredits  *float64  `gorm:"column:limit_1d_credits"`
	Limit7dCredits  *float64  `gorm:"column:limit_7d_credits"`
	Limit30dCredits *float64  `gorm:"column:limit_30d_credits"`
	CreatedAt       time.Time `gorm:"column:created_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
}

// TableName returns the database table name.
func (OrganizationMemberRecord) TableName() string { return "organization_member" }

// OrganizationMemberUpdate is a partial member patch.
type OrganizationMemberUpdate struct {
	Role            *string
	Limit1dCredits  OptionalFloatUpdate
	Limit7dCredits  OptionalFloatUpdate
	Limit30dCredits OptionalFloatUpdate
}

// OrganizationMember is a member row joined with the member's username.
type OrganizationMember struct {
	OrganizationMemberRecord
	Username string `gorm:"column:username"`
}

// OrganizationMembership is one organization that a user belongs to.
type OrganizationMembership struct {
	OrganizationMemberRecord
	OrganizationName string `gorm:"column:organization_name"`
}

// OrganizationMemberSpendWindow reports one member spend window.
type OrganizationMemberSpendWindow struct {
	ID        string   `json:"id"`
	Limit     *float64 `json:"limit"`
	Used      float64  `json:"used"`
	Remaining *float64 `json:"remaining"`
}

// IsOrganization reports whether the user record is an organization account.
func (u *UserRecord) IsOrganization() bool {
	return u != nil && u.Kind == UserKindOrganization
}

// NormalizeOrganizationRole validates a role name.
func NormalizeOrganizationRole(role string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case OrganizationRoleOwner:
		return OrganizationRoleOwner, nil
	case OrganizationRoleMember, "":
		return OrganizationRoleMember, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrOrganizationInvalidRole, role)
	}
}

// CreateOrganization creates an organization billing account.
func (r *Repository) CreateOrganization(ctx context.Context, update UserUpdate) (*UserRecord, error) {
	// Organizations never authenticate; strip all credential material.
	update.Password = nil
	update.Email = nil
	update.MFA = nil
	update.Passkey = nil
	pending := false
	update.ApprovalPending = &pending
	return r.createUserWithKind(ctx, update, UserKindOrganization)
}

// ListOrganizations returns all organization accounts.
func (r *Repository) ListOrganizations(ctx context.Context) ([]UserRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	var records []UserRecord
	if errFind := db.WithContext(contextOrBackground(ctx)).Where("kind = ?", UserKindOrganization).Order("id").Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	return records, nil
}

// GetOrganization returns an organization account by ID.
func (r *Repository) GetOrganization(ctx context.Context, id uint) (*UserRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	return getOrganizationTx(ctx, db, id)
}

func getOrganizationTx(ctx context.Context, tx *gorm.DB, id uint) (*UserRecord, error) {
	if id == 0 {
		return nil, ErrOrganizationNotFound
	}
	record := &UserRecord{}
	errFirst := tx.WithContext(contextOrBackground(ctx)).Where("id = ? AND kind = ?", id, UserKindOrganization).First(record).Error
	if errors.Is(errFirst, gorm.ErrRecordNotFound) {
		return nil, ErrOrganizationNotFound
	}
	if errFirst != nil {
		return nil, errFirst
	}
	return record, nil
}

// ListOrganizationMembers returns members of one organization ordered by ID.
func (r *Repository) ListOrganizationMembers(ctx context.Context, orgID uint) ([]OrganizationMember, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if _, errOrg := getOrganizationTx(ctx, db, orgID); errOrg != nil {
		return nil, errOrg
	}
	var members []OrganizationMember
	errFind := db.WithContext(contextOrBackground(ctx)).
		Table("organization_member").
		Select(`"organization_member".*, "user"."username" AS username`).
		Joins(`JOIN "user" ON "user"."id" = "organization_member"."member_user_id" AND "user"."deleted_at" IS NULL`).
		Where(`"organization_member"."org_user_id" = ?`, orgID).
		Order(`"organization_member"."id"`).
		Scan(&members).Error
	if errFind != nil {
		return nil, errFind
	}
	return members, nil
}

// ListUserOrganizations returns every organization the user belongs to.
func (r *Repository) ListUserOrganizations(ctx context.Context, userID uint) ([]OrganizationMembership, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	var memberships []OrganizationMembership
	errFind := db.WithContext(contextOrBackground(ctx)).
		Table("organization_member").
		Select(`"organization_member".*, "user"."username" AS organization_name`).
		Joins(`JOIN "user" ON "user"."id" = "organization_member"."org_user_id" AND "user"."deleted_at" IS NULL`).
		Where(`"organization_member"."member_user_id" = ?`, userID).
		Order(`"organization_member"."org_user_id"`).
		Scan(&memberships).Error
	if errFind != nil {
		return nil, errFind
	}
	return memberships, nil
}

// GetOrganizationMember returns a single membership row.
func (r *Repository) GetOrganizationMember(ctx context.Context, orgID, memberID uint) (*OrganizationMemberRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	return getOrganizationMemberTx(ctx, db, orgID, memberID, false)
}

func getOrganizationMemberTx(ctx context.Context, tx *gorm.DB, orgID, memberID uint, lock bool) (*OrganizationMemberRecord, error) {
	query := tx.WithContext(contextOrBackground(ctx))
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	record := &OrganizationMemberRecord{}
	errFirst := query.Where("org_user_id = ? AND member_user_id = ?", orgID, memberID).First(record).Error
	if errors.Is(errFirst, gorm.ErrRecordNotFound) {
		return nil, ErrOrganizationMemberNotFound
	}
	if errFirst != nil {
		return nil, errFirst
	}
	return record, nil
}

// AddOrganizationMember adds a personal user to an organization.
func (r *Repository) AddOrganizationMember(ctx context.Context, orgID, memberID uint, update OrganizationMemberUpdate) (*OrganizationMemberRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	role := OrganizationRoleMember
	if update.Role != nil {
		normalized, errRole := NormalizeOrganizationRole(*update.Role)
		if errRole != nil {
			return nil, errRole
		}
		role = normalized
	}
	ctx = contextOrBackground(ctx)
	record := &OrganizationMemberRecord{OrgUserID: orgID, MemberUserID: memberID, Role: role}
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, errOrg := getOrganizationTx(ctx, tx, orgID); errOrg != nil {
			return errOrg
		}
		member := &UserRecord{}
		errMember := tx.Where("id = ?", memberID).First(member).Error
		if errors.Is(errMember, gorm.ErrRecordNotFound) {
			return fmt.Errorf("%w: %d", ErrUserNotFound, memberID)
		}
		if errMember != nil {
			return errMember
		}
		if member.IsOrganization() {
			return ErrOrganizationInvalidMember
		}
		if _, errExisting := getOrganizationMemberTx(ctx, tx, orgID, memberID, false); errExisting == nil {
			return ErrOrganizationMemberExists
		} else if !errors.Is(errExisting, ErrOrganizationMemberNotFound) {
			return errExisting
		}
		if errLimits := applyOrganizationMemberLimits(record, update); errLimits != nil {
			return errLimits
		}
		return tx.Create(record).Error
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return record, nil
}

// UpdateOrganizationMember changes a member's role or spend limits.
func (r *Repository) UpdateOrganizationMember(ctx context.Context, orgID, memberID uint, update OrganizationMemberUpdate) (*OrganizationMemberRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	ctx = contextOrBackground(ctx)
	var record *OrganizationMemberRecord
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, errOrg := getOrganizationTx(ctx, tx, orgID); errOrg != nil {
			return errOrg
		}
		if errLock := lockOrganizationOwnersTx(ctx, tx, orgID); errLock != nil {
			return errLock
		}
		current, errCurrent := getOrganizationMemberTx(ctx, tx, orgID, memberID, true)
		if errCurrent != nil {
			return errCurrent
		}
		record = current
		if update.Role != nil {
			role, errRole := NormalizeOrganizationRole(*update.Role)
			if errRole != nil {
				return errRole
			}
			if record.Role == OrganizationRoleOwner && role != OrganizationRoleOwner {
				if errLast := ensureAnotherOrganizationOwnerTx(ctx, tx, orgID, memberID); errLast != nil {
					return errLast
				}
			}
			record.Role = role
		}
		if errLimits := applyOrganizationMemberLimits(record, update); errLimits != nil {
			return errLimits
		}
		return tx.Save(record).Error
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return record, nil
}

// RemoveOrganizationMember removes a member and deletes every organization key they created.
func (r *Repository) RemoveOrganizationMember(ctx context.Context, orgID, memberID uint) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	ctx = contextOrBackground(ctx)
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Match key mutation lock ordering before taking membership row locks.
		if errLock := lockAPIKeyMutationTransaction(tx); errLock != nil {
			return errLock
		}
		if _, errOrg := getOrganizationTx(ctx, tx, orgID); errOrg != nil {
			return errOrg
		}
		if errLock := lockOrganizationOwnersTx(ctx, tx, orgID); errLock != nil {
			return errLock
		}
		current, errCurrent := getOrganizationMemberTx(ctx, tx, orgID, memberID, true)
		if errCurrent != nil {
			return errCurrent
		}
		if current.Role == OrganizationRoleOwner {
			if errLast := ensureAnotherOrganizationOwnerTx(ctx, tx, orgID, memberID); errLast != nil {
				return errLast
			}
		}
		if errDelete := tx.Delete(current).Error; errDelete != nil {
			return errDelete
		}
		return deleteOrganizationMemberKeysTx(tx, &orgID, memberID)
	})
}

// DeleteOrganization deletes an organization account, its memberships and its keys.
func (r *Repository) DeleteOrganization(ctx context.Context, orgID uint) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	ctx = contextOrBackground(ctx)
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errLock := lockAPIKeyMutationTransaction(tx); errLock != nil {
			return errLock
		}
		org, errOrg := getOrganizationTx(ctx, tx, orgID)
		if errOrg != nil {
			return errOrg
		}
		if errMembers := tx.Where("org_user_id = ?", orgID).Delete(&OrganizationMemberRecord{}).Error; errMembers != nil {
			return errMembers
		}
		result := tx.Where("user_id = ?", orgID).Delete(&APIKeyRecord{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected > 0 {
			if errEvent := appendEvent(tx, "config", "upsert", configAPIKeysRootKey, time.Now().UTC().UnixNano()); errEvent != nil {
				return errEvent
			}
		}
		return tx.Delete(org).Error
	})
}

// removeUserOrganizationLinksTx runs when any user account is deleted: personal
// users lose memberships and the organization keys they created; organization
// accounts lose their member rows (their own keys are removed by the caller).
func removeUserOrganizationLinksTx(tx *gorm.DB, userID uint) error {
	if errMembers := tx.Where("member_user_id = ? OR org_user_id = ?", userID, userID).Delete(&OrganizationMemberRecord{}).Error; errMembers != nil {
		return errMembers
	}
	return deleteOrganizationMemberKeysTx(tx, nil, userID)
}

// deleteOrganizationMemberKeysTx deletes organization keys created by a member.
// A nil orgID deletes the member's keys across every organization.
func deleteOrganizationMemberKeysTx(tx *gorm.DB, orgID *uint, memberID uint) error {
	// Personal keys keep created_by_user_id NULL, so this only matches organization keys.
	query := tx.Where("created_by_user_id = ?", memberID)
	if orgID != nil {
		query = query.Where("user_id = ?", *orgID)
	}
	result := query.Delete(&APIKeyRecord{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return nil
	}
	return appendEvent(tx, "config", "upsert", configAPIKeysRootKey, time.Now().UTC().UnixNano())
}

func lockOrganizationOwnersTx(ctx context.Context, tx *gorm.DB, orgID uint) error {
	// Serialize owner changes per organization through the organization row lock.
	record := &UserRecord{}
	return tx.WithContext(contextOrBackground(ctx)).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id").
		Where("id = ?", orgID).
		First(record).Error
}

func ensureAnotherOrganizationOwnerTx(ctx context.Context, tx *gorm.DB, orgID, memberID uint) error {
	var owners int64
	errCount := tx.WithContext(contextOrBackground(ctx)).
		Model(&OrganizationMemberRecord{}).
		Where("org_user_id = ? AND role = ? AND member_user_id <> ?", orgID, OrganizationRoleOwner, memberID).
		Count(&owners).Error
	if errCount != nil {
		return errCount
	}
	if owners == 0 {
		return ErrOrganizationLastOwner
	}
	return nil
}

func applyOrganizationMemberLimits(record *OrganizationMemberRecord, update OrganizationMemberUpdate) error {
	if errLimit := applyOptionalFloat(update.Limit1dCredits, &record.Limit1dCredits); errLimit != nil {
		return periodLimitFieldError("limit_1d_credits", PeriodLimitErrInvalidLimit, fmt.Sprintf("limit_1d_credits: %s", errLimit.Error()))
	}
	if errLimit := applyOptionalFloat(update.Limit7dCredits, &record.Limit7dCredits); errLimit != nil {
		return periodLimitFieldError("limit_7d_credits", PeriodLimitErrInvalidLimit, fmt.Sprintf("limit_7d_credits: %s", errLimit.Error()))
	}
	if errLimit := applyOptionalFloat(update.Limit30dCredits, &record.Limit30dCredits); errLimit != nil {
		return periodLimitFieldError("limit_30d_credits", PeriodLimitErrInvalidLimit, fmt.Sprintf("limit_30d_credits: %s", errLimit.Error()))
	}
	return nil
}

func organizationMemberLimitsEnabled(member *OrganizationMemberRecord) bool {
	return member != nil && (member.Limit1dCredits != nil || member.Limit7dCredits != nil || member.Limit30dCredits != nil)
}

// OrganizationMemberSpend returns rolling spend windows for one member.
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
	now = now.UTC()
	defs := []struct {
		id       string
		limit    *float64
		duration time.Duration
	}{
		{PeriodWindow1d, member.Limit1dCredits, periodLimit1dDuration},
		{PeriodWindow7d, member.Limit7dCredits, periodLimit7dDuration},
		{PeriodWindow30d, member.Limit30dCredits, periodLimit30dDuration},
	}
	since1d := now.Add(-periodLimit1dDuration)
	since7d := now.Add(-periodLimit7dDuration)
	since30d := now.Add(-periodLimit30dDuration)
	var row struct {
		Sum1d  float64 `gorm:"column:sum_1d"`
		Sum7d  float64 `gorm:"column:sum_7d"`
		Sum30d float64 `gorm:"column:sum_30d"`
	}
	errScan := db.WithContext(contextOrBackground(ctx)).Raw(`
SELECT
	COALESCE(SUM(CASE WHEN "billing_charge"."created_at" >= ? THEN "billing_charge"."amount" ELSE 0 END), 0) AS sum_1d,
	COALESCE(SUM(CASE WHEN "billing_charge"."created_at" >= ? THEN "billing_charge"."amount" ELSE 0 END), 0) AS sum_7d,
	COALESCE(SUM("billing_charge"."amount"), 0) AS sum_30d
FROM "billing_charge"
WHERE "billing_charge"."actor_user_id" = ?
	AND "billing_charge"."user_id" = ?
	AND "billing_charge"."created_at" >= ?
`, since1d, since7d, member.MemberUserID, member.OrgUserID, since30d).Scan(&row).Error
	if errScan != nil {
		return nil, errScan
	}
	used := map[string]float64{PeriodWindow1d: row.Sum1d, PeriodWindow7d: row.Sum7d, PeriodWindow30d: row.Sum30d}
	out := make([]OrganizationMemberSpendWindow, 0, len(defs))
	for _, def := range defs {
		out = append(out, OrganizationMemberSpendWindow{
			ID:        def.id,
			Limit:     def.limit,
			Used:      used[def.id],
			Remaining: remainingCredits(def.limit, used[def.id]),
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
	windows, errWindows := organizationMemberSpendWindowsDB(ctx, db, member, time.Now())
	if errWindows != nil {
		return errWindows
	}
	for _, window := range windows {
		if window.Limit == nil {
			continue
		}
		if *window.Limit == 0 || window.Used >= *window.Limit || math.IsNaN(window.Used) {
			return fmt.Errorf("%s: %s (window=%s mode=sliding used=%g limit=%g)",
				homeerrors.TypeUserPeriodLimitExceeded, homeerrors.MessageOrganizationMemberLimitExceeded,
				window.ID, window.Used, *window.Limit)
		}
	}
	return nil
}

// OrganizationAPIKeyScope restricts organization key mutations to one member's
// keys. A nil MemberUserID grants owner/administrator scope over every key.
type OrganizationAPIKeyScope struct {
	OrgID        uint
	MemberUserID *uint
}

func organizationAPIKeyQuery(tx *gorm.DB, scope OrganizationAPIKeyScope) *gorm.DB {
	query := tx.Where("user_id = ?", scope.OrgID)
	if memberID := normalizeOptionalUserID(scope.MemberUserID); memberID != nil {
		query = query.Where("created_by_user_id = ?", *memberID)
	}
	return query
}

// ListOrganizationAPIKeys returns organization keys visible to the scope.
func (r *Repository) ListOrganizationAPIKeys(ctx context.Context, scope OrganizationAPIKeyScope) ([]APIKeyRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	var records []APIKeyRecord
	if errFind := organizationAPIKeyQuery(db.WithContext(contextOrBackground(ctx)), scope).Order("id").Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	return records, nil
}

// CreateOrganizationAPIKey creates a key billed to the organization and owned by a member.
// Model groups default to the organization's configured groups, never to the member's.
func (r *Repository) CreateOrganizationAPIKey(ctx context.Context, orgID, memberID uint, update APIKeyUserUpdate) (*APIKeyRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if _, errMember := getOrganizationMemberTx(ctx, db, orgID, memberID, false); errMember != nil {
		return nil, errMember
	}
	if update.ModelGroups == nil {
		groups, errGroups := r.OrganizationModelGroups(ctx, orgID)
		if errGroups != nil {
			return nil, errGroups
		}
		update.ModelGroups = &groups
	}
	update.Channels = nil
	update.CreatedByUserID = &memberID
	return r.CreateAPIKeyForUser(ctx, orgID, update)
}

// UpdateOrganizationAPIKey renames or rotates an organization key inside the scope.
// Routing bindings stay administrator-managed and are not changed here.
func (r *Repository) UpdateOrganizationAPIKey(ctx context.Context, scope OrganizationAPIKeyScope, id uint, update APIKeyUserUpdate) (*APIKeyRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if errVisible := ensureOrganizationAPIKeyVisible(ctx, db, scope, id); errVisible != nil {
		return nil, errVisible
	}
	update.Channels = nil
	update.ModelGroups = nil
	return r.UpdateAPIKeyForUser(ctx, scope.OrgID, id, "", update)
}

// DeleteOrganizationAPIKey deletes an organization key inside the scope.
func (r *Repository) DeleteOrganizationAPIKey(ctx context.Context, scope OrganizationAPIKeyScope, id uint) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	if errVisible := ensureOrganizationAPIKeyVisible(ctx, db, scope, id); errVisible != nil {
		return errVisible
	}
	return r.DeleteAPIKeyForUser(ctx, scope.OrgID, id, "")
}

func ensureOrganizationAPIKeyVisible(ctx context.Context, db *gorm.DB, scope OrganizationAPIKeyScope, id uint) error {
	if id == 0 {
		return gorm.ErrRecordNotFound
	}
	record := &APIKeyRecord{}
	return organizationAPIKeyQuery(db.WithContext(contextOrBackground(ctx)), scope).Where("id = ?", id).First(record).Error
}

// OrganizationModelGroups returns the model groups configured for organization keys.
// Administrators set them through the organization's key bindings; new member keys inherit them.
func (r *Repository) OrganizationModelGroups(ctx context.Context, orgID uint) ([]uint, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	org, errOrg := getOrganizationTx(ctx, db, orgID)
	if errOrg != nil {
		return nil, errOrg
	}
	return organizationModelGroupsFromRecord(org)
}

// SetOrganizationModelGroups stores the organization's model groups and applies
// them to every existing organization key.
func (r *Repository) SetOrganizationModelGroups(ctx context.Context, orgID uint, groups []uint) ([]uint, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	groups = normalizeModelGroupIDs(groups)
	groupsJSON, errJSON := apiKeyModelGroupsJSON(groups)
	if errJSON != nil {
		return nil, errJSON
	}
	ctx = contextOrBackground(ctx)
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errLock := lockAPIKeyMutationTransaction(tx); errLock != nil {
			return errLock
		}
		if _, errOrg := getOrganizationTx(ctx, tx, orgID); errOrg != nil {
			return errOrg
		}
		if errGroups := ensureAPIKeyGroupsNotDeleted(tx, nil, &groups); errGroups != nil {
			return errGroups
		}
		if errUpdate := tx.Model(&UserRecord{}).Where("id = ?", orgID).Update("organization_model_groups", groupsJSON).Error; errUpdate != nil {
			return errUpdate
		}
		result := tx.Model(&APIKeyRecord{}).Where("user_id = ?", orgID).Update("model_groups", groupsJSON)
		if result.Error != nil {
			return result.Error
		}
		return appendEvent(tx, "config", "upsert", configAPIKeysRootKey, time.Now().UTC().UnixNano())
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return groups, nil
}

func organizationModelGroupsFromRecord(org *UserRecord) ([]uint, error) {
	if org == nil || len(org.OrganizationModelGroups) == 0 {
		return []uint{}, nil
	}
	groups, errGroups := apiKeyModelGroupsFromJSON(org.OrganizationModelGroups)
	if errGroups != nil {
		return nil, errGroups
	}
	return normalizeModelGroupIDs(groups), nil
}

// OrganizationMemberSpendSummary is one member's spend in a time range.
type OrganizationMemberSpendSummary struct {
	MemberUserID uint    `gorm:"column:member_user_id" json:"member_user_id"`
	Amount       float64 `gorm:"column:amount" json:"amount"`
	RequestCount int64   `gorm:"column:request_count" json:"request_count"`
}

// OrganizationSpendByMember aggregates organization charges per member since a time.
// Charges from administrator-created keys report member_user_id 0.
func (r *Repository) OrganizationSpendByMember(ctx context.Context, orgID uint, since time.Time) ([]OrganizationMemberSpendSummary, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	var rows []OrganizationMemberSpendSummary
	errScan := db.WithContext(contextOrBackground(ctx)).Raw(`
SELECT COALESCE("billing_charge"."actor_user_id", 0) AS member_user_id,
	COALESCE(SUM("billing_charge"."amount"), 0) AS amount,
	COUNT(*) AS request_count
FROM "billing_charge"
WHERE "billing_charge"."user_id" = ? AND "billing_charge"."created_at" >= ?
GROUP BY COALESCE("billing_charge"."actor_user_id", 0)
ORDER BY amount DESC
`, orgID, since.UTC()).Scan(&rows).Error
	if errScan != nil {
		return nil, errScan
	}
	return rows, nil
}

// OrganizationModelGroupsOf returns the model groups stored on an organization record.
func OrganizationModelGroupsOf(org *UserRecord) ([]uint, error) {
	return organizationModelGroupsFromRecord(org)
}

// GenerateAPIKeyValue returns a random key using the user API key format.
func GenerateAPIKeyValue() (string, error) {
	var raw [32]byte
	if _, errRandom := rand.Read(raw[:]); errRandom != nil {
		return "", errRandom
	}
	return "cpah_" + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
