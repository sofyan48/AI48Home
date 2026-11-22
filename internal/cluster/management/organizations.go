package management

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"gorm.io/gorm"
)

type organizationWriteRequest struct {
	Name              *string           `json:"name"`
	Credits           *float64          `json:"credits"`
	CreditsUnlimited  *bool             `json:"credits_unlimited"`
	BillingMultiplier optionalJSONFloat `json:"billing_multiplier"`
	Timezone          *string           `json:"timezone"`
	Limit5h           optionalJSONFloat `json:"limit_5h_credits"`
	Limit1d           optionalJSONFloat `json:"limit_1d_credits"`
	Limit7d           optionalJSONFloat `json:"limit_7d_credits"`
	Limit30d          optionalJSONFloat `json:"limit_30d_credits"`
	ModelGroups       *[]uint           `json:"model_groups"`
	OwnerUserID       *uint             `json:"owner_user_id"`
	AllowMemberKeys   *bool             `json:"allow_member_keys"`
}

type organizationMemberWriteRequest struct {
	UserID        *uint             `json:"user_id"`
	Username      *string           `json:"username"`
	Role          *string           `json:"role"`
	Limit5h       optionalJSONFloat `json:"limit_5h_credits"`
	WindowMode5h  *string           `json:"window_mode_5h"`
	Limit1d       optionalJSONFloat `json:"limit_1d_credits"`
	WindowMode1d  *string           `json:"window_mode_1d"`
	Limit7d       optionalJSONFloat `json:"limit_7d_credits"`
	WindowMode7d  *string           `json:"window_mode_7d"`
	Limit30d      optionalJSONFloat `json:"limit_30d_credits"`
	WindowMode30d *string           `json:"window_mode_30d"`
	ModelGroups   *[]uint           `json:"model_groups"`
	MaxAPIKeys    optionalJSONFloat `json:"max_api_keys"`
}

type organizationAPIKeyWriteRequest struct {
	MemberUserID *uint   `json:"member_user_id"`
	DisplayName  *string `json:"display_name"`
	APIKey       *string `json:"api_key"`
}

// ListOrganizations returns organization accounts with member counts.
func (h *Handler) ListOrganizations(c *gin.Context) {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	records, errRecords := h.repo.ListOrganizations(ctx)
	if errRecords != nil {
		respondError(c, http.StatusInternalServerError, "organization_load_failed", errRecords)
		return
	}
	items := make([]gin.H, 0, len(records))
	for index := range records {
		members, errMembers := h.repo.ListOrganizationMembers(ctx, records[index].ID)
		if errMembers != nil {
			respondError(c, http.StatusInternalServerError, "organization_load_failed", errMembers)
			return
		}
		item := organizationResponse(&records[index])
		item["member_count"] = len(members)
		owners := make([]string, 0)
		for _, member := range members {
			if member.Role == cluster.OrganizationRoleOwner {
				owners = append(owners, member.Username)
			}
		}
		item["owners"] = owners
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{"organizations": items})
}

// GetOrganization returns one organization with members and keys.
func (h *Handler) GetOrganization(c *gin.Context) {
	id, ok := organizationIDFromParam(c)
	if !ok {
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	org, errOrg := h.repo.GetOrganization(ctx, id)
	if errOrg != nil {
		respondOrganizationError(c, "organization_load_failed", errOrg)
		return
	}
	members, errMembers := h.repo.ListOrganizationMembers(ctx, id)
	if errMembers != nil {
		respondOrganizationError(c, "organization_load_failed", errMembers)
		return
	}
	spend, errSpend := h.repo.OrganizationSpendByMember(ctx, id, time.Now().UTC().Add(-30*24*time.Hour))
	if errSpend != nil {
		respondOrganizationError(c, "organization_load_failed", errSpend)
		return
	}
	pending, errPending := h.repo.PendingBillingRechargeRequests(ctx, []uint{id})
	if errPending != nil {
		respondError(c, http.StatusInternalServerError, "billing_recharge_request_load_failed", errPending)
		return
	}
	keyCounts, errCounts := h.repo.OrganizationMemberKeyCounts(ctx, id)
	if errCounts != nil {
		respondOrganizationError(c, "organization_load_failed", errCounts)
		return
	}
	item := organizationResponse(org)
	item["members"] = organizationMembersResponse(members, spend, keyCounts)
	item["pending_topup"] = pendingTopupResponse(pending[id])
	c.JSON(http.StatusOK, gin.H{"organization": item})
}

// CreateOrganization creates an organization and optionally its first owner.
func (h *Handler) CreateOrganization(c *gin.Context) {
	var body organizationWriteRequest
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errBind)
		return
	}
	if body.Name == nil || strings.TrimSpace(*body.Name) == "" {
		respondErrorWithFieldErrors(c, http.StatusBadRequest, "invalid body", errRequired("name"), []fieldErrorItem{{Field: "name", Code: "required"}})
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	// One transaction: a failed owner or model-group step leaves no organization behind.
	org, errCreate := h.repo.CreateOrganizationWithSetup(ctx, organizationUserUpdate(body), cluster.OrganizationSetup{
		OwnerUserID: body.OwnerUserID, ModelGroups: body.ModelGroups, AllowMemberKeys: body.AllowMemberKeys,
	})
	if errCreate != nil {
		if respondUserPeriodLimitValidationError(c, errCreate) {
			return
		}
		respondOrganizationError(c, "organization_create_failed", errCreate)
		return
	}
	c.JSON(http.StatusOK, gin.H{"organization": organizationResponse(org)})
}

// UpdateOrganization updates organization billing settings and model groups.
func (h *Handler) UpdateOrganization(c *gin.Context) {
	id, ok := organizationIDFromParam(c)
	if !ok {
		return
	}
	var body organizationWriteRequest
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errBind)
		return
	}
	if body.Name != nil && strings.TrimSpace(*body.Name) == "" {
		respondErrorWithFieldErrors(c, http.StatusBadRequest, "invalid body", errRequired("name"), []fieldErrorItem{{Field: "name", Code: "required"}})
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	if _, errOrg := h.repo.GetOrganization(ctx, id); errOrg != nil {
		respondOrganizationError(c, "organization_update_failed", errOrg)
		return
	}
	org, errUpdate := h.repo.UpdateUser(ctx, id, organizationUserUpdate(body))
	if errUpdate != nil {
		if respondUserPeriodLimitValidationError(c, errUpdate) {
			return
		}
		respondOrganizationError(c, "organization_update_failed", errUpdate)
		return
	}
	if body.ModelGroups != nil {
		if _, errGroups := h.repo.SetOrganizationModelGroups(ctx, id, *body.ModelGroups); errGroups != nil {
			respondOrganizationError(c, "organization_update_failed", errGroups)
			return
		}
		if errRefresh := h.refreshConfig(ctx); errRefresh != nil {
			respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
			return
		}
		org, _ = h.repo.GetOrganization(ctx, id)
	}
	if body.AllowMemberKeys != nil {
		updated, errAllow := h.repo.SetOrganizationMemberKeysAllowed(ctx, id, *body.AllowMemberKeys)
		if errAllow != nil {
			respondOrganizationError(c, "organization_update_failed", errAllow)
			return
		}
		org = updated
	}
	c.JSON(http.StatusOK, gin.H{"organization": organizationResponse(org)})
}

// DeleteOrganization deletes an organization, its memberships and its keys.
func (h *Handler) DeleteOrganization(c *gin.Context) {
	id, ok := organizationIDFromParam(c)
	if !ok {
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	if errDelete := h.repo.DeleteOrganization(ctx, id); errDelete != nil {
		respondOrganizationError(c, "organization_delete_failed", errDelete)
		return
	}
	if errRefresh := h.refreshConfig(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return
	}
	respondOK(c)
}

// AddOrganizationMember adds a personal user by ID or username.
func (h *Handler) AddOrganizationMember(c *gin.Context) {
	id, ok := organizationIDFromParam(c)
	if !ok {
		return
	}
	var body organizationMemberWriteRequest
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errBind)
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	memberID := uint(0)
	switch {
	case body.UserID != nil && *body.UserID > 0:
		memberID = *body.UserID
	case body.Username != nil && strings.TrimSpace(*body.Username) != "":
		user, errUser := h.repo.GetUserByUsername(ctx, *body.Username)
		if errUser != nil {
			respondOrganizationError(c, "organization_member_add_failed", errUser)
			return
		}
		memberID = user.ID
	default:
		respondErrorWithFieldErrors(c, http.StatusBadRequest, "invalid body", errRequired("user_id"), []fieldErrorItem{{Field: "user_id", Code: "required"}})
		return
	}
	member, errAdd := h.repo.AddOrganizationMember(ctx, id, memberID, organizationMemberUpdate(body))
	if errAdd != nil {
		if respondUserPeriodLimitValidationError(c, errAdd) {
			return
		}
		respondOrganizationError(c, "organization_member_add_failed", errAdd)
		return
	}
	c.JSON(http.StatusOK, gin.H{"member": organizationMemberRecordResponse(member)})
}

// UpdateOrganizationMember changes a member role or spend limits.
func (h *Handler) UpdateOrganizationMember(c *gin.Context) {
	id, ok := organizationIDFromParam(c)
	if !ok {
		return
	}
	memberID, ok := organizationMemberIDFromParam(c)
	if !ok {
		return
	}
	var body organizationMemberWriteRequest
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errBind)
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	member, errUpdate := h.repo.UpdateOrganizationMember(ctx, id, memberID, organizationMemberUpdate(body))
	if errUpdate != nil {
		if respondUserPeriodLimitValidationError(c, errUpdate) {
			return
		}
		respondOrganizationError(c, "organization_member_update_failed", errUpdate)
		return
	}
	c.JSON(http.StatusOK, gin.H{"member": organizationMemberRecordResponse(member)})
}

// RemoveOrganizationMember removes a member and deletes their organization keys.
func (h *Handler) RemoveOrganizationMember(c *gin.Context) {
	id, ok := organizationIDFromParam(c)
	if !ok {
		return
	}
	memberID, ok := organizationMemberIDFromParam(c)
	if !ok {
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	if errRemove := h.repo.RemoveOrganizationMember(ctx, id, memberID); errRemove != nil {
		respondOrganizationError(c, "organization_member_remove_failed", errRemove)
		return
	}
	if errRefresh := h.refreshConfig(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return
	}
	respondOK(c)
}

// ListOrganizationAPIKeys lists every organization key.
func (h *Handler) ListOrganizationAPIKeys(c *gin.Context) {
	id, ok := organizationIDFromParam(c)
	if !ok {
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	if _, errOrg := h.repo.GetOrganization(ctx, id); errOrg != nil {
		respondOrganizationError(c, "organization_api_key_load_failed", errOrg)
		return
	}
	records, errRecords := h.repo.ListOrganizationAPIKeys(ctx, cluster.OrganizationAPIKeyScope{OrgID: id})
	if errRecords != nil {
		respondOrganizationError(c, "organization_api_key_load_failed", errRecords)
		return
	}
	items := make([]gin.H, 0, len(records))
	for index := range records {
		items = append(items, organizationAPIKeyResponse(&records[index]))
	}
	c.JSON(http.StatusOK, gin.H{"api_keys": items})
}

// CreateOrganizationAPIKey creates an organization key for a member.
func (h *Handler) CreateOrganizationAPIKey(c *gin.Context) {
	id, ok := organizationIDFromParam(c)
	if !ok {
		return
	}
	var body organizationAPIKeyWriteRequest
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errBind)
		return
	}
	if body.MemberUserID == nil || *body.MemberUserID == 0 {
		respondErrorWithFieldErrors(c, http.StatusBadRequest, "invalid body", errRequired("member_user_id"), []fieldErrorItem{{Field: "member_user_id", Code: "required"}})
		return
	}
	key := ""
	if body.APIKey != nil {
		key = strings.TrimSpace(*body.APIKey)
	}
	if key == "" {
		generated, errGenerate := cluster.GenerateAPIKeyValue()
		if errGenerate != nil {
			respondError(c, http.StatusInternalServerError, "api_key_generate_failed", errGenerate)
			return
		}
		key = generated
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	record, errCreate := h.repo.CreateOrganizationAPIKey(ctx, id, *body.MemberUserID, cluster.APIKeyUserUpdate{
		APIKey:         &key,
		DisplayName:    body.DisplayName,
		DisplayNameSet: body.DisplayName != nil,
	})
	if errCreate != nil {
		respondOrganizationError(c, "organization_api_key_create_failed", errCreate)
		return
	}
	if errRefresh := h.refreshConfig(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return
	}
	c.JSON(http.StatusOK, gin.H{"api_key": organizationAPIKeyResponse(record)})
}

// DeleteOrganizationAPIKey deletes one organization key.
func (h *Handler) DeleteOrganizationAPIKey(c *gin.Context) {
	id, ok := organizationIDFromParam(c)
	if !ok {
		return
	}
	keyID, errKeyID := strconv.ParseUint(strings.TrimSpace(c.Param("key_id")), 10, 64)
	if errKeyID != nil || keyID == 0 {
		respondError(c, http.StatusBadRequest, "invalid id", fmt.Errorf("invalid api key id"))
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	if errDelete := h.repo.DeleteOrganizationAPIKey(ctx, cluster.OrganizationAPIKeyScope{OrgID: id}, uint(keyID)); errDelete != nil {
		respondOrganizationError(c, "organization_api_key_delete_failed", errDelete)
		return
	}
	if errRefresh := h.refreshConfig(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return
	}
	respondOK(c)
}

// ListUserOrganizationsForAdmin lists organizations a personal user belongs to.
func (h *Handler) ListUserOrganizationsForAdmin(c *gin.Context) {
	id, ok := userIDFromParam(c)
	if !ok {
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	memberships, errMemberships := h.repo.ListUserOrganizations(ctx, id)
	if errMemberships != nil {
		respondOrganizationError(c, "organization_load_failed", errMemberships)
		return
	}
	items := make([]gin.H, 0, len(memberships))
	for _, membership := range memberships {
		items = append(items, gin.H{
			"organization_id":   membership.OrgUserID,
			"organization_name": membership.OrganizationName,
			"role":              membership.Role,
		})
	}
	c.JSON(http.StatusOK, gin.H{"organizations": items})
}

func organizationUserUpdate(body organizationWriteRequest) cluster.UserUpdate {
	update := cluster.UserUpdate{
		Credits:           body.Credits,
		CreditsUnlimited:  body.CreditsUnlimited,
		BillingMultiplier: optionalFloatToUpdate(body.BillingMultiplier),
		Timezone:          body.Timezone,
		Limit5hCredits:    optionalFloatToUpdate(body.Limit5h),
		Limit1dCredits:    optionalFloatToUpdate(body.Limit1d),
		Limit7dCredits:    optionalFloatToUpdate(body.Limit7d),
		Limit30dCredits:   optionalFloatToUpdate(body.Limit30d),
	}
	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		update.Username = &name
	}
	return update
}

func organizationMemberUpdate(body organizationMemberWriteRequest) cluster.OrganizationMemberUpdate {
	return cluster.OrganizationMemberUpdate{
		Role:            body.Role,
		Limit5hCredits:  optionalFloatToUpdate(body.Limit5h),
		WindowMode5h:    body.WindowMode5h,
		Limit1dCredits:  optionalFloatToUpdate(body.Limit1d),
		WindowMode1d:    body.WindowMode1d,
		Limit7dCredits:  optionalFloatToUpdate(body.Limit7d),
		WindowMode7d:    body.WindowMode7d,
		Limit30dCredits: optionalFloatToUpdate(body.Limit30d),
		WindowMode30d:   body.WindowMode30d,
		ModelGroups:     body.ModelGroups,
		MaxAPIKeys:      cluster.OptionalIntFromFloat(optionalFloatToUpdate(body.MaxAPIKeys)),
	}
}

func organizationResponse(org *cluster.UserRecord) gin.H {
	if org == nil {
		return gin.H{}
	}
	item := userRecordToMap(org)
	for _, key := range []string{"password_set", "approval_pending", "mfa", "passkey"} {
		delete(item, key)
	}
	item["name"] = org.Username
	groups, errGroups := cluster.OrganizationModelGroupsOf(org)
	if errGroups != nil {
		groups = []uint{}
	}
	item["model_groups"] = groups
	item["allow_member_keys"] = !org.OrganizationMemberKeysDisabled
	return item
}

func organizationMembersResponse(members []cluster.OrganizationMember, spend []cluster.OrganizationMemberSpendSummary, keyCounts map[uint]int64) []gin.H {
	spent := make(map[uint]cluster.OrganizationMemberSpendSummary, len(spend))
	for _, row := range spend {
		spent[row.MemberUserID] = row
	}
	items := make([]gin.H, 0, len(members))
	for index := range members {
		item := organizationMemberRecordResponse(&members[index].OrganizationMemberRecord)
		item["username"] = members[index].Username
		item["spend_30d"] = spent[members[index].MemberUserID].Amount
		item["requests_30d"] = spent[members[index].MemberUserID].RequestCount
		item["key_count"] = keyCounts[members[index].MemberUserID]
		items = append(items, item)
	}
	return items
}

func organizationMemberRecordResponse(member *cluster.OrganizationMemberRecord) gin.H {
	if member == nil {
		return gin.H{}
	}
	item := cluster.OrganizationMemberLimitFields(member)
	item["organization_id"] = member.OrgUserID
	item["user_id"] = member.MemberUserID
	item["role"] = member.Role
	item["created_at"] = member.CreatedAt
	item["updated_at"] = member.UpdatedAt
	return item
}

func organizationAPIKeyResponse(record *cluster.APIKeyRecord) gin.H {
	if record == nil {
		return gin.H{}
	}
	return gin.H{
		"id":                 record.ID,
		"api_key":            record.APIKey,
		"display_name":       record.DisplayName,
		"created_by_user_id": record.CreatedByUserID,
		"created_at":         record.CreatedAt,
		"updated_at":         record.UpdatedAt,
	}
}

func organizationIDFromParam(c *gin.Context) (uint, bool) {
	return userIDFromParam(c)
}

func organizationMemberIDFromParam(c *gin.Context) (uint, bool) {
	id, errID := cluster.ParseUserRecordID(c.Param("user_id"))
	if errID != nil {
		respondError(c, http.StatusBadRequest, "invalid id", errID)
		return 0, false
	}
	return id, true
}

func respondOrganizationError(c *gin.Context, code string, err error) {
	switch {
	case errors.Is(err, cluster.ErrOrganizationNotFound), errors.Is(err, cluster.ErrOrganizationMemberNotFound),
		errors.Is(err, cluster.ErrUserNotFound), errors.Is(err, gorm.ErrRecordNotFound):
		respondError(c, http.StatusNotFound, "not_found", err)
	case errors.Is(err, cluster.ErrOrganizationMemberExists), errors.Is(err, cluster.ErrAPIKeyExists):
		respondError(c, http.StatusConflict, "conflict", err)
	case errors.Is(err, cluster.ErrOrganizationLastOwner):
		respondError(c, http.StatusConflict, "last_owner", err)
	case errors.Is(err, cluster.ErrOrganizationInvalidMember), errors.Is(err, cluster.ErrOrganizationInvalidRole),
		errors.Is(err, cluster.ErrDeletedAccessGroup), errors.Is(err, cluster.ErrInvalidAPIKeyDisplayName),
		errors.Is(err, cluster.ErrOrganizationMemberModelGroups), errors.Is(err, cluster.ErrOrganizationInvalidKeyLimit):
		respondError(c, http.StatusBadRequest, "invalid body", err)
	case errors.Is(err, cluster.ErrOrganizationModelGroupsConflict):
		respondError(c, http.StatusConflict, "member_model_groups_conflict", err)
	case errors.Is(err, cluster.ErrOrganizationMemberKeyLimit):
		respondError(c, http.StatusConflict, "member_key_limit", err)
	case cluster.IsUserConflictError(err):
		respondError(c, http.StatusConflict, "organization_exists", err)
	default:
		respondError(c, http.StatusInternalServerError, code, err)
	}
}
