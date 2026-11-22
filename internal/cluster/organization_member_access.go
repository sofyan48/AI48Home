package cluster

import (
	"context"
	"fmt"
	"math"
	"time"

	"gorm.io/gorm"
)

// OptionalIntFromFloat converts a JSON number patch into an integer patch.
// Fractional or out-of-range values become -1 so validation rejects them.
func OptionalIntFromFloat(update OptionalFloatUpdate) OptionalIntUpdate {
	if !update.Set {
		return OptionalIntUpdate{}
	}
	if update.Clear {
		return OptionalIntUpdate{Set: true, Clear: true}
	}
	value := update.Value
	if value != math.Trunc(value) || value < 0 || value > math.MaxInt32 {
		return OptionalIntUpdate{Set: true, Value: -1}
	}
	return OptionalIntUpdate{Set: true, Value: int(value)}
}

// organizationMemberModelGroupsFromRecord returns the member model-group subset.
func organizationMemberModelGroupsFromRecord(member *OrganizationMemberRecord) ([]uint, error) {
	if member == nil || len(member.ModelGroups) == 0 {
		return []uint{}, nil
	}
	groups, errGroups := apiKeyModelGroupsFromJSON(member.ModelGroups)
	if errGroups != nil {
		return nil, errGroups
	}
	if groups == nil {
		groups = []uint{}
	}
	return groups, nil
}

// OrganizationMemberModelGroupsOf returns the member model-group subset for API responses.
func OrganizationMemberModelGroupsOf(member *OrganizationMemberRecord) []uint {
	groups, errGroups := organizationMemberModelGroupsFromRecord(member)
	if errGroups != nil {
		return []uint{}
	}
	return groups
}

// effectiveOrganizationMemberModelGroups is the scope applied to a member's keys:
// the member subset when set, otherwise the organization scope.
func effectiveOrganizationMemberModelGroups(org *UserRecord, member *OrganizationMemberRecord) ([]uint, error) {
	subset, errSubset := organizationMemberModelGroupsFromRecord(member)
	if errSubset != nil {
		return nil, errSubset
	}
	if len(subset) > 0 {
		return subset, nil
	}
	return organizationModelGroupsFromRecord(org)
}

// validateOrganizationMemberModelGroups requires a member subset inside the
// organization scope. An unrestricted organization allows any active group.
func validateOrganizationMemberModelGroups(tx *gorm.DB, org *UserRecord, groups []uint) error {
	if len(groups) == 0 {
		return nil
	}
	if errDeleted := ensureAPIKeyGroupsNotDeleted(tx, nil, &groups); errDeleted != nil {
		return errDeleted
	}
	orgGroups, errOrg := organizationModelGroupsFromRecord(org)
	if errOrg != nil {
		return errOrg
	}
	if len(orgGroups) == 0 {
		var count int64
		if errCount := tx.Model(&ModelGroupRecord{}).Where("id IN ?", groups).Count(&count).Error; errCount != nil {
			return errCount
		}
		if int(count) != len(groups) {
			return fmt.Errorf("%w: unknown model group", ErrOrganizationMemberModelGroups)
		}
		return nil
	}
	allowed := make(map[uint]struct{}, len(orgGroups))
	for _, id := range orgGroups {
		allowed[id] = struct{}{}
	}
	for _, id := range groups {
		if _, ok := allowed[id]; !ok {
			return fmt.Errorf("%w: model group %d", ErrOrganizationMemberModelGroups, id)
		}
	}
	return nil
}

// applyOrganizationMemberAccess applies the model-group subset and key cap.
func applyOrganizationMemberAccess(tx *gorm.DB, record *OrganizationMemberRecord, org *UserRecord, update OrganizationMemberUpdate) error {
	if update.ModelGroups != nil {
		groups := normalizeModelGroupIDs(*update.ModelGroups)
		if errValidate := validateOrganizationMemberModelGroups(tx, org, groups); errValidate != nil {
			return errValidate
		}
		groupsJSON, errJSON := apiKeyModelGroupsJSON(groups)
		if errJSON != nil {
			return errJSON
		}
		record.ModelGroups = groupsJSON
	}
	if update.MaxAPIKeys.Set {
		if update.MaxAPIKeys.Clear {
			record.MaxAPIKeys = nil
		} else {
			if update.MaxAPIKeys.Value < 0 {
				return ErrOrganizationInvalidKeyLimit
			}
			value := update.MaxAPIKeys.Value
			record.MaxAPIKeys = &value
		}
	}
	return nil
}

// applyOrganizationMemberKeyScopeTx rewrites the model groups of a member's keys.
func applyOrganizationMemberKeyScopeTx(tx *gorm.DB, org *UserRecord, member *OrganizationMemberRecord) error {
	groups, errGroups := effectiveOrganizationMemberModelGroups(org, member)
	if errGroups != nil {
		return errGroups
	}
	groupsJSON, errJSON := apiKeyModelGroupsJSON(groups)
	if errJSON != nil {
		return errJSON
	}
	result := tx.Model(&APIKeyRecord{}).
		Where("user_id = ? AND created_by_user_id = ?", member.OrgUserID, member.MemberUserID).
		Update("model_groups", groupsJSON)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return nil
	}
	return appendEvent(tx, "config", "upsert", configAPIKeysRootKey, time.Now().UTC().UnixNano())
}

// reconcileOrganizationMemberScopesTx keeps member subsets inside a new
// organization scope. Groups removed from the organization are dropped from
// every subset; a member left with none would silently widen to the whole
// organization, so that change is rejected instead.
func reconcileOrganizationMemberScopesTx(tx *gorm.DB, orgID uint, orgGroups []uint) error {
	if len(orgGroups) == 0 {
		return nil
	}
	allowed := make(map[uint]struct{}, len(orgGroups))
	for _, id := range orgGroups {
		allowed[id] = struct{}{}
	}
	var members []OrganizationMemberRecord
	if errFind := tx.Where("org_user_id = ?", orgID).Find(&members).Error; errFind != nil {
		return errFind
	}
	for index := range members {
		subset, errSubset := organizationMemberModelGroupsFromRecord(&members[index])
		if errSubset != nil {
			return errSubset
		}
		if len(subset) == 0 {
			continue
		}
		kept := make([]uint, 0, len(subset))
		for _, id := range subset {
			if _, ok := allowed[id]; ok {
				kept = append(kept, id)
			}
		}
		if len(kept) == len(subset) {
			continue
		}
		if len(kept) == 0 {
			return fmt.Errorf("%w: member %d", ErrOrganizationModelGroupsConflict, members[index].MemberUserID)
		}
		keptJSON, errJSON := apiKeyModelGroupsJSON(kept)
		if errJSON != nil {
			return errJSON
		}
		if errUpdate := tx.Model(&OrganizationMemberRecord{}).Where("id = ?", members[index].ID).Update("model_groups", keptJSON).Error; errUpdate != nil {
			return errUpdate
		}
	}
	return nil
}

// applyOrganizationKeyScopesTx rewrites every organization key to its effective scope.
func applyOrganizationKeyScopesTx(ctx context.Context, tx *gorm.DB, orgID uint) error {
	org, errOrg := getOrganizationTx(ctx, tx, orgID)
	if errOrg != nil {
		return errOrg
	}
	orgGroupsJSON, errJSON := apiKeyModelGroupsJSON(mustOrganizationGroups(org))
	if errJSON != nil {
		return errJSON
	}
	// Administrator-created keys and members without a subset use the organization scope.
	if errUpdate := tx.Model(&APIKeyRecord{}).Where("user_id = ?", orgID).Update("model_groups", orgGroupsJSON).Error; errUpdate != nil {
		return errUpdate
	}
	var members []OrganizationMemberRecord
	if errFind := tx.Where("org_user_id = ?", orgID).Find(&members).Error; errFind != nil {
		return errFind
	}
	for index := range members {
		if len(OrganizationMemberModelGroupsOf(&members[index])) == 0 {
			continue
		}
		if errScope := applyOrganizationMemberKeyScopeTx(tx, org, &members[index]); errScope != nil {
			return errScope
		}
	}
	return nil
}

func mustOrganizationGroups(org *UserRecord) []uint {
	groups, errGroups := organizationModelGroupsFromRecord(org)
	if errGroups != nil {
		return []uint{}
	}
	return groups
}

// ensureOrganizationMemberKeyCapacityTx rejects a new key beyond max_api_keys.
func ensureOrganizationMemberKeyCapacityTx(ctx context.Context, tx *gorm.DB, member *OrganizationMemberRecord) error {
	if member.MaxAPIKeys == nil {
		return nil
	}
	var count int64
	errCount := tx.WithContext(contextOrBackground(ctx)).Model(&APIKeyRecord{}).
		Where("user_id = ? AND created_by_user_id = ?", member.OrgUserID, member.MemberUserID).
		Count(&count).Error
	if errCount != nil {
		return errCount
	}
	if count >= int64(*member.MaxAPIKeys) {
		return fmt.Errorf("%w (max %d)", ErrOrganizationMemberKeyLimit, *member.MaxAPIKeys)
	}
	return nil
}

// OrganizationModelGroupOption is a model group an owner may assign to members.
type OrganizationModelGroupOption struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
}

// OrganizationModelGroupOptions lists the groups owners may choose for member
// subsets: the organization scope, or every enabled group when unrestricted.
func (r *Repository) OrganizationModelGroupOptions(ctx context.Context, orgID uint) ([]OrganizationModelGroupOption, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	ctx = contextOrBackground(ctx)
	org, errOrg := getOrganizationTx(ctx, db, orgID)
	if errOrg != nil {
		return nil, errOrg
	}
	orgGroups, errGroups := organizationModelGroupsFromRecord(org)
	if errGroups != nil {
		return nil, errGroups
	}
	query := db.WithContext(ctx).Model(&ModelGroupRecord{}).Order("id")
	if len(orgGroups) > 0 {
		// Disabled groups stay listed so owners can still see existing assignments.
		query = query.Where("id IN ?", orgGroups)
	} else {
		query = query.Where("disabled = ?", false)
	}
	var records []ModelGroupRecord
	if errFind := query.Find(&records).Error; errFind != nil {
		return nil, errFind
	}
	out := make([]OrganizationModelGroupOption, 0, len(records))
	for _, record := range records {
		out = append(out, OrganizationModelGroupOption{ID: record.ID, Name: record.GroupName})
	}
	return out, nil
}

// OrganizationMemberKeyCounts returns the number of organization keys per member.
func (r *Repository) OrganizationMemberKeyCounts(ctx context.Context, orgID uint) (map[uint]int64, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	var rows []struct {
		MemberUserID uint  `gorm:"column:member_user_id"`
		Count        int64 `gorm:"column:key_count"`
	}
	errScan := db.WithContext(contextOrBackground(ctx)).Model(&APIKeyRecord{}).
		Select("created_by_user_id AS member_user_id, COUNT(*) AS key_count").
		Where("user_id = ? AND created_by_user_id IS NOT NULL", orgID).
		Group("created_by_user_id").
		Scan(&rows).Error
	if errScan != nil {
		return nil, errScan
	}
	out := make(map[uint]int64, len(rows))
	for _, row := range rows {
		out[row.MemberUserID] = row.Count
	}
	return out, nil
}
