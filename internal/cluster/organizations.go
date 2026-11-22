package cluster

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

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
	// ErrOrganizationMemberModelGroups rejects member model groups outside the organization scope.
	ErrOrganizationMemberModelGroups = errors.New("member model groups must be a subset of the organization model groups")
	// ErrOrganizationModelGroupsConflict rejects an organization scope change that would leave a restricted member with no models.
	ErrOrganizationModelGroupsConflict = errors.New("organization model groups would remove every model group of a restricted member")
	// ErrOrganizationMemberKeyLimit rejects a key beyond the member's max_api_keys.
	ErrOrganizationMemberKeyLimit = errors.New("organization member api key limit reached")
	// ErrOrganizationInvalidKeyLimit rejects a negative max_api_keys.
	ErrOrganizationInvalidKeyLimit = errors.New("max_api_keys must be a non-negative integer")
)

// OrganizationMemberRecord links a personal user to an organization account.
type OrganizationMemberRecord struct {
	ID           uint   `gorm:"column:id;primaryKey;autoIncrement"`
	OrgUserID    uint   `gorm:"column:org_user_id;not null;uniqueIndex:idx_organization_member_pair,priority:1"`
	MemberUserID uint   `gorm:"column:member_user_id;not null;uniqueIndex:idx_organization_member_pair,priority:2;index"`
	Role         string `gorm:"column:role;type:varchar(16);not null"`
	// Spend limits are credit budgets for this member inside the organization;
	// NULL disables a window. Window modes match user period limits; rows created
	// before v25 default to sliding, the only mode supported at the time.
	Limit5hCredits  *float64 `gorm:"column:limit_5h_credits"`
	WindowMode5h    string   `gorm:"column:window_mode_5h;type:varchar(16);not null;default:sliding"`
	Limit1dCredits  *float64 `gorm:"column:limit_1d_credits"`
	WindowMode1d    string   `gorm:"column:window_mode_1d;type:varchar(16);not null;default:sliding"`
	Limit7dCredits  *float64 `gorm:"column:limit_7d_credits"`
	WindowMode7d    string   `gorm:"column:window_mode_7d;type:varchar(16);not null;default:sliding"`
	Limit30dCredits *float64 `gorm:"column:limit_30d_credits"`
	WindowMode30d   string   `gorm:"column:window_mode_30d;type:varchar(16);not null;default:sliding"`
	// first_use window state, opened by the member's first billable charge.
	PeriodWindowStart5h     *time.Time `gorm:"column:period_window_start_5h"`
	PeriodWindowStart1d     *time.Time `gorm:"column:period_window_start_1d"`
	PeriodWindowStart7d     *time.Time `gorm:"column:period_window_start_7d"`
	PeriodWindowStart30d    *time.Time `gorm:"column:period_window_start_30d"`
	UsageEpoch5h            *time.Time `gorm:"column:usage_epoch_5h"`
	UsageEpoch1d            *time.Time `gorm:"column:usage_epoch_1d"`
	UsageEpoch7d            *time.Time `gorm:"column:usage_epoch_7d"`
	UsageEpoch30d           *time.Time `gorm:"column:usage_epoch_30d"`
	ResetAllowanceMode      string     `gorm:"column:reset_allowance_mode;not null;default:''"`
	ResetAllowanceAmount    int64      `gorm:"column:reset_allowance_amount;not null;default:0"`
	ResetAllowanceAvailable int64      `gorm:"column:reset_allowance_available;not null;default:0"`
	ResetAllowanceExpiresAt *time.Time `gorm:"column:reset_allowance_expires_at"`
	ResetAllowanceRenewsAt  *time.Time `gorm:"column:reset_allowance_renews_at"`
	ResetAllowanceTimezone  string     `gorm:"-"`
	// ModelGroups narrows this member's organization keys to a subset of the
	// organization model groups; empty inherits the organization scope.
	ModelGroups JSONB `gorm:"column:model_groups"`
	// MaxAPIKeys caps the organization keys owned by this member; NULL is unlimited.
	MaxAPIKeys *int      `gorm:"column:max_api_keys"`
	CreatedAt  time.Time `gorm:"column:created_at"`
	UpdatedAt  time.Time `gorm:"column:updated_at"`
}

// TableName returns the database table name.
func (OrganizationMemberRecord) TableName() string { return "organization_member" }

// OrganizationMemberUpdate is a partial member patch.
type OrganizationMemberUpdate struct {
	Role            *string
	Limit5hCredits  OptionalFloatUpdate
	WindowMode5h    *string
	Limit1dCredits  OptionalFloatUpdate
	WindowMode1d    *string
	Limit7dCredits  OptionalFloatUpdate
	WindowMode7d    *string
	Limit30dCredits OptionalFloatUpdate
	WindowMode30d   *string
	// ModelGroups replaces the member model-group subset; an empty list inherits the organization.
	ModelGroups *[]uint
	MaxAPIKeys  OptionalIntUpdate
}

// OptionalIntUpdate captures a three-state integer patch: absent / null / value.
type OptionalIntUpdate struct {
	Set   bool
	Clear bool
	Value int
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
	ID        string     `json:"id"`
	Limit     *float64   `json:"limit"`
	Used      float64    `json:"used"`
	Remaining *float64   `json:"remaining"`
	Mode      string     `json:"mode"`
	Active    bool       `json:"active"`
	ResetAt   *time.Time `json:"reset_at"`
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
	return r.CreateOrganizationWithSetup(ctx, update, OrganizationSetup{})
}

// OrganizationSetup is optional configuration applied when an organization is created.
type OrganizationSetup struct {
	OwnerUserID     *uint
	ModelGroups     *[]uint
	AllowMemberKeys *bool
}

// CreateOrganizationWithSetup creates an organization, its first owner, model
// groups and member-key setting in one transaction, so a failed step never
// leaves a half-configured or ownerless organization behind.
func (r *Repository) CreateOrganizationWithSetup(ctx context.Context, update UserUpdate, setup OrganizationSetup) (*UserRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	// Organizations never authenticate; strip all credential material.
	update.Password = nil
	update.Email = nil
	update.MFA = nil
	update.Passkey = nil
	pending := false
	update.ApprovalPending = &pending
	ctx = contextOrBackground(ctx)
	var org *UserRecord
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		created, errCreate := createUserWithKindTx(ctx, tx, update, UserKindOrganization)
		if errCreate != nil {
			return errCreate
		}
		updates := map[string]any{}
		if setup.ModelGroups != nil {
			groups := normalizeModelGroupIDs(*setup.ModelGroups)
			if errGroups := ensureAPIKeyGroupsNotDeleted(tx, nil, &groups); errGroups != nil {
				return errGroups
			}
			groupsJSON, errJSON := apiKeyModelGroupsJSON(groups)
			if errJSON != nil {
				return errJSON
			}
			updates["organization_model_groups"] = groupsJSON
		}
		if setup.AllowMemberKeys != nil {
			updates["organization_member_keys_disabled"] = !*setup.AllowMemberKeys
		}
		if len(updates) > 0 {
			if errUpdate := tx.Model(&UserRecord{}).Where("id = ?", created.ID).Updates(updates).Error; errUpdate != nil {
				return errUpdate
			}
		}
		if setup.OwnerUserID != nil {
			owner := OrganizationRoleOwner
			if _, errOwner := addOrganizationMemberTx(ctx, tx, created.ID, *setup.OwnerUserID, OrganizationMemberUpdate{Role: &owner}); errOwner != nil {
				return errOwner
			}
		}
		reloaded, errReload := getOrganizationTx(ctx, tx, created.ID)
		org = reloaded
		return errReload
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return org, nil
}

// ErrOrganizationLastOwnerAccount blocks deleting a personal account that is
// the only owner of an organization.
var ErrOrganizationLastOwnerAccount = errors.New("user is the last owner of an organization; transfer ownership or delete the organization first")

// ensureNotLastOrganizationOwnerTx rejects deleting a user who is the only
// owner of any organization.
func ensureNotLastOrganizationOwnerTx(ctx context.Context, tx *gorm.DB, userID uint) error {
	var owned []OrganizationMemberRecord
	if errFind := tx.WithContext(contextOrBackground(ctx)).
		Where("member_user_id = ? AND role = ?", userID, OrganizationRoleOwner).
		Find(&owned).Error; errFind != nil {
		return errFind
	}
	for _, membership := range owned {
		if errLock := lockOrganizationOwnersTx(ctx, tx, membership.OrgUserID); errLock != nil {
			return errLock
		}
		if errOther := ensureAnotherOrganizationOwnerTx(ctx, tx, membership.OrgUserID, userID); errors.Is(errOther, ErrOrganizationLastOwner) {
			name := ""
			if org, errOrg := getOrganizationTx(ctx, tx, membership.OrgUserID); errOrg == nil {
				name = org.Username
			}
			return fmt.Errorf("%w (organization %q)", ErrOrganizationLastOwnerAccount, name)
		} else if errOther != nil {
			return errOther
		}
	}
	return nil
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
	org, errOrg := getOrganizationTx(ctx, db, orgID)
	if errOrg != nil {
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
	for index := range members {
		members[index].ResetAllowanceTimezone = org.Timezone
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
	org, errOrg := getOrganizationTx(ctx, tx, orgID)
	if errOrg != nil {
		return nil, errOrg
	}
	record.ResetAllowanceTimezone = org.Timezone
	return record, nil
}

// AddOrganizationMember adds a personal user to an organization.
func (r *Repository) AddOrganizationMember(ctx context.Context, orgID, memberID uint, update OrganizationMemberUpdate) (*OrganizationMemberRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	ctx = contextOrBackground(ctx)
	var record *OrganizationMemberRecord
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		added, errAdd := addOrganizationMemberTx(ctx, tx, orgID, memberID, update)
		record = added
		return errAdd
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return record, nil
}

func addOrganizationMemberTx(ctx context.Context, tx *gorm.DB, orgID, memberID uint, update OrganizationMemberUpdate) (*OrganizationMemberRecord, error) {
	role := OrganizationRoleMember
	if update.Role != nil {
		normalized, errRole := NormalizeOrganizationRole(*update.Role)
		if errRole != nil {
			return nil, errRole
		}
		role = normalized
	}
	record := &OrganizationMemberRecord{
		OrgUserID: orgID, MemberUserID: memberID, Role: role,
		WindowMode5h: defaultOrganizationMemberWindowMode, WindowMode1d: defaultOrganizationMemberWindowMode,
		WindowMode7d: defaultOrganizationMemberWindowMode, WindowMode30d: defaultOrganizationMemberWindowMode,
	}
	errApply := func() error {
		org, errOrg := getOrganizationTx(ctx, tx, orgID)
		if errOrg != nil {
			return errOrg
		}
		record.ResetAllowanceTimezone = org.Timezone
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
		if errLimits := applyOrganizationMemberLimits(record, org, update); errLimits != nil {
			return errLimits
		}
		if errAccess := applyOrganizationMemberAccess(tx, record, org, update); errAccess != nil {
			return errAccess
		}
		return tx.Create(record).Error
	}()
	if errApply != nil {
		return nil, errApply
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
		org, errOrg := getOrganizationTx(ctx, tx, orgID)
		if errOrg != nil {
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
		if errLimits := applyOrganizationMemberLimits(record, org, update); errLimits != nil {
			return errLimits
		}
		if errAccess := applyOrganizationMemberAccess(tx, record, org, update); errAccess != nil {
			return errAccess
		}
		if errSave := tx.Save(record).Error; errSave != nil {
			return errSave
		}
		if update.ModelGroups == nil {
			return nil
		}
		return applyOrganizationMemberKeyScopeTx(tx, org, record)
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
// Model groups are the member subset when set, otherwise the organization's groups;
// they never come from the member's personal keys.
func (r *Repository) CreateOrganizationAPIKey(ctx context.Context, orgID, memberID uint, update APIKeyUserUpdate) (*APIKeyRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	org, errOrg := getOrganizationTx(ctx, db, orgID)
	if errOrg != nil {
		return nil, errOrg
	}
	member, errMember := getOrganizationMemberTx(ctx, db, orgID, memberID, false)
	if errMember != nil {
		return nil, errMember
	}
	groups, errGroups := effectiveOrganizationMemberModelGroups(org, member)
	if errGroups != nil {
		return nil, errGroups
	}
	update.ModelGroups = &groups
	update.Channels = nil
	update.CreatedByUserID = &memberID
	update.beforeCreate = func(ctx context.Context, tx *gorm.DB) error {
		// Re-read under the key mutation lock so concurrent creates respect max_api_keys.
		current, errCurrent := getOrganizationMemberTx(ctx, tx, orgID, memberID, false)
		if errCurrent != nil {
			return errCurrent
		}
		return ensureOrganizationMemberKeyCapacityTx(ctx, tx, current)
	}
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
		if errMembers := reconcileOrganizationMemberScopesTx(tx, orgID, groups); errMembers != nil {
			return errMembers
		}
		if errKeys := applyOrganizationKeyScopesTx(ctx, tx, orgID); errKeys != nil {
			return errKeys
		}
		return appendEvent(tx, "config", "upsert", configAPIKeysRootKey, time.Now().UTC().UnixNano())
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return groups, nil
}

// SetOrganizationMemberKeysAllowed controls whether regular members may create
// their own organization keys. Existing keys are kept.
func (r *Repository) SetOrganizationMemberKeysAllowed(ctx context.Context, orgID uint, allowed bool) (*UserRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	ctx = contextOrBackground(ctx)
	if _, errOrg := getOrganizationTx(ctx, db, orgID); errOrg != nil {
		return nil, errOrg
	}
	if errUpdate := db.WithContext(ctx).Model(&UserRecord{}).Where("id = ?", orgID).Update("organization_member_keys_disabled", !allowed).Error; errUpdate != nil {
		return nil, errUpdate
	}
	return getOrganizationTx(ctx, db, orgID)
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
