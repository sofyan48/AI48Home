package userapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"gorm.io/gorm"
)

// Organization routes live under /user/organizations and never change the
// behavior of personal-account routes such as /user/api-keys or /user/billing.

type orgOptionalFloat struct {
	set   bool
	clear bool
	value float64
}

func (o *orgOptionalFloat) UnmarshalJSON(data []byte) error {
	o.set = true
	if strings.TrimSpace(string(data)) == "null" {
		o.clear = true
		return nil
	}
	return json.Unmarshal(data, &o.value)
}

func (o orgOptionalFloat) update() cluster.OptionalFloatUpdate {
	if !o.set {
		return cluster.OptionalFloatUpdate{}
	}
	if o.clear {
		return cluster.OptionalFloatUpdate{Set: true, Clear: true}
	}
	return cluster.OptionalFloatUpdate{Set: true, Value: o.value}
}

type orgMemberRequest struct {
	Username      string           `json:"username"`
	Role          *string          `json:"role"`
	Limit5h       orgOptionalFloat `json:"limit_5h_credits"`
	WindowMode5h  *string          `json:"window_mode_5h"`
	Limit1d       orgOptionalFloat `json:"limit_1d_credits"`
	WindowMode1d  *string          `json:"window_mode_1d"`
	Limit7d       orgOptionalFloat `json:"limit_7d_credits"`
	WindowMode7d  *string          `json:"window_mode_7d"`
	Limit30d      orgOptionalFloat `json:"limit_30d_credits"`
	WindowMode30d *string          `json:"window_mode_30d"`
	ModelGroups   *[]uint          `json:"model_groups"`
	MaxAPIKeys    orgOptionalFloat `json:"max_api_keys"`
}

type orgSettingsRequest struct {
	AllowMemberKeys *bool `json:"allow_member_keys"`
}

type orgAPIKeyRequest struct {
	DisplayName  json.RawMessage `json:"display_name"`
	APIKey       *string         `json:"api_key"`
	MemberUserID *uint           `json:"member_user_id"`
}

// orgContext is the authenticated caller inside one organization.
type orgContext struct {
	user   *cluster.UserRecord
	org    *cluster.UserRecord
	member *cluster.OrganizationMemberRecord
}

func (o orgContext) isOwner() bool {
	return o.member != nil && o.member.Role == cluster.OrganizationRoleOwner
}

// actorFilter returns nil for owners (whole organization) and the caller's ID for members.
func (o orgContext) actorFilter() *uint {
	if o.isOwner() {
		return nil
	}
	id := o.user.ID
	return &id
}

func (h *Handler) organizationContext(c *gin.Context) (orgContext, bool) {
	ctx, cancel := requestContext(c)
	defer cancel()
	user, ok := h.authenticatedUser(c, ctx, authFields{})
	if !ok {
		return orgContext{}, false
	}
	orgID, errID := cluster.ParseUserRecordID(c.Param("org_id"))
	if errID != nil {
		respondError(c, http.StatusBadRequest, "invalid_id", fmt.Errorf("invalid organization id"))
		return orgContext{}, false
	}
	member, errMember := h.repo.GetOrganizationMember(ctx, orgID, user.ID)
	if errMember != nil {
		// Non-members get the same 404 as a missing organization.
		if errors.Is(errMember, cluster.ErrOrganizationMemberNotFound) {
			respondError(c, http.StatusNotFound, "not_found", fmt.Errorf("organization not found"))
			return orgContext{}, false
		}
		respondError(c, http.StatusInternalServerError, "organization_load_failed", errMember)
		return orgContext{}, false
	}
	org, errOrg := h.repo.GetOrganization(ctx, orgID)
	if errOrg != nil {
		respondOrganizationUserError(c, "organization_load_failed", errOrg)
		return orgContext{}, false
	}
	return orgContext{user: user, org: org, member: member}, true
}

// canCreateOwnKeys reports whether the caller may create keys for themselves.
func (o orgContext) canCreateOwnKeys() bool {
	return o.isOwner() || (o.org != nil && !o.org.OrganizationMemberKeysDisabled)
}

func requireOrganizationOwner(c *gin.Context, oc orgContext) bool {
	if oc.isOwner() {
		return true
	}
	respondError(c, http.StatusForbidden, "organization_owner_required", fmt.Errorf("organization owner role is required"))
	return false
}

// ListMyOrganizations lists organizations the caller belongs to.
func (h *Handler) ListMyOrganizations(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	user, ok := h.authenticatedUser(c, ctx, authFields{})
	if !ok {
		return
	}
	memberships, errMemberships := h.repo.ListUserOrganizations(ctx, user.ID)
	if errMemberships != nil {
		respondError(c, http.StatusInternalServerError, "organization_load_failed", errMemberships)
		return
	}
	items := make([]gin.H, 0, len(memberships))
	for _, membership := range memberships {
		items = append(items, gin.H{
			"id":   membership.OrgUserID,
			"name": membership.OrganizationName,
			"role": membership.Role,
		})
	}
	c.JSON(http.StatusOK, gin.H{"organizations": items})
}

// GetMyOrganization returns the organization summary visible to the caller.
// Owners see the shared balance; members only see their own spend limits.
func (h *Handler) GetMyOrganization(c *gin.Context) {
	oc, ok := h.organizationContext(c)
	if !ok {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	windows, errWindows := h.repo.OrganizationMemberSpend(ctx, oc.org.ID, oc.user.ID, time.Now().UTC())
	if errWindows != nil {
		respondOrganizationUserError(c, "organization_load_failed", errWindows)
		return
	}
	item := gin.H{
		"id":                oc.org.ID,
		"name":              oc.org.Username,
		"role":              oc.member.Role,
		"spend_limits":      windows,
		"allow_member_keys": !oc.org.OrganizationMemberKeysDisabled,
		"can_create_keys":   oc.canCreateOwnKeys(),
		"member":            organizationMemberUserResponse(oc.member),
	}
	if oc.isOwner() {
		item["credits"] = oc.org.Credits
		item["credits_unlimited"] = oc.org.CreditsUnlimited
		// Owners need the organization caps to keep member limits within them.
		item["organization_limits"] = gin.H{
			"limit_5h_credits":  oc.org.Limit5hCredits,
			"limit_1d_credits":  oc.org.Limit1dCredits,
			"limit_7d_credits":  oc.org.Limit7dCredits,
			"limit_30d_credits": oc.org.Limit30dCredits,
		}
		options, errOptions := h.repo.OrganizationModelGroupOptions(ctx, oc.org.ID)
		if errOptions != nil {
			respondOrganizationUserError(c, "organization_load_failed", errOptions)
			return
		}
		item["model_group_options"] = options
	}
	c.JSON(http.StatusOK, gin.H{"organization": item})
}

// UpdateMyOrganizationSettings changes owner-managed organization settings.
func (h *Handler) UpdateMyOrganizationSettings(c *gin.Context) {
	oc, ok := h.organizationContext(c)
	if !ok || !requireOrganizationOwner(c, oc) {
		return
	}
	var body orgSettingsRequest
	if !decodeJSONBody(c, &body) {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	org := oc.org
	if body.AllowMemberKeys != nil {
		updated, errUpdate := h.repo.SetOrganizationMemberKeysAllowed(ctx, oc.org.ID, *body.AllowMemberKeys)
		if errUpdate != nil {
			respondOrganizationUserError(c, "organization_update_failed", errUpdate)
			return
		}
		org = updated
	}
	c.JSON(http.StatusOK, gin.H{"organization": gin.H{
		"id": org.ID, "name": org.Username, "allow_member_keys": !org.OrganizationMemberKeysDisabled,
	}})
}

// ListMyOrganizationMembers lists members with 30-day spend (owner only).
func (h *Handler) ListMyOrganizationMembers(c *gin.Context) {
	oc, ok := h.organizationContext(c)
	if !ok || !requireOrganizationOwner(c, oc) {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	members, errMembers := h.repo.ListOrganizationMembers(ctx, oc.org.ID)
	if errMembers != nil {
		respondOrganizationUserError(c, "organization_member_load_failed", errMembers)
		return
	}
	spend, errSpend := h.repo.OrganizationSpendByMember(ctx, oc.org.ID, time.Now().UTC().Add(-30*24*time.Hour))
	if errSpend != nil {
		respondOrganizationUserError(c, "organization_member_load_failed", errSpend)
		return
	}
	keyCounts, errCounts := h.repo.OrganizationMemberKeyCounts(ctx, oc.org.ID)
	if errCounts != nil {
		respondOrganizationUserError(c, "organization_member_load_failed", errCounts)
		return
	}
	spent := make(map[uint]cluster.OrganizationMemberSpendSummary, len(spend))
	for _, row := range spend {
		spent[row.MemberUserID] = row
	}
	items := make([]gin.H, 0, len(members))
	for index := range members {
		item := organizationMemberUserResponse(&members[index].OrganizationMemberRecord)
		item["username"] = members[index].Username
		item["key_count"] = keyCounts[members[index].MemberUserID]
		item["spend_30d"] = spent[members[index].MemberUserID].Amount
		item["requests_30d"] = spent[members[index].MemberUserID].RequestCount
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{"members": items})
}

// AddMyOrganizationMember adds an existing personal user by username (owner only).
func (h *Handler) AddMyOrganizationMember(c *gin.Context) {
	oc, ok := h.organizationContext(c)
	if !ok || !requireOrganizationOwner(c, oc) {
		return
	}
	var body orgMemberRequest
	if !decodeJSONBody(c, &body) {
		return
	}
	username := strings.TrimSpace(body.Username)
	if username == "" {
		respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("username is required"))
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	target, errTarget := h.repo.GetUserByUsername(ctx, username)
	if errTarget != nil || target.IsOrganization() || target.ApprovalPending {
		respondError(c, http.StatusNotFound, "user_not_found", fmt.Errorf("user not found"))
		return
	}
	member, errAdd := h.repo.AddOrganizationMember(ctx, oc.org.ID, target.ID, organizationMemberUpdateFromRequest(body))
	if errAdd != nil {
		respondOrganizationUserError(c, "organization_member_add_failed", errAdd)
		return
	}
	item := organizationMemberUserResponse(member)
	item["username"] = target.Username
	c.JSON(http.StatusOK, gin.H{"member": item})
}

// UpdateMyOrganizationMember changes role or spend limits (owner only).
func (h *Handler) UpdateMyOrganizationMember(c *gin.Context) {
	oc, ok := h.organizationContext(c)
	if !ok || !requireOrganizationOwner(c, oc) {
		return
	}
	memberID, ok := organizationMemberIDParam(c)
	if !ok {
		return
	}
	var body orgMemberRequest
	if !decodeJSONBody(c, &body) {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	member, errUpdate := h.repo.UpdateOrganizationMember(ctx, oc.org.ID, memberID, organizationMemberUpdateFromRequest(body))
	if errUpdate != nil {
		respondOrganizationUserError(c, "organization_member_update_failed", errUpdate)
		return
	}
	c.JSON(http.StatusOK, gin.H{"member": organizationMemberUserResponse(member)})
}

// RemoveMyOrganizationMember removes a member (owner) or leaves the organization (self).
// The removed member's organization keys are deleted.
func (h *Handler) RemoveMyOrganizationMember(c *gin.Context) {
	oc, ok := h.organizationContext(c)
	if !ok {
		return
	}
	memberID, ok := organizationMemberIDParam(c)
	if !ok {
		return
	}
	if memberID != oc.user.ID && !requireOrganizationOwner(c, oc) {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	if errRemove := h.repo.RemoveOrganizationMember(ctx, oc.org.ID, memberID); errRemove != nil {
		respondOrganizationUserError(c, "organization_member_remove_failed", errRemove)
		return
	}
	if errRefresh := h.refreshConfig(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return
	}
	respondOK(c)
}

// ListMyOrganizationAPIKeys lists keys: owners see all, members only their own.
func (h *Handler) ListMyOrganizationAPIKeys(c *gin.Context) {
	oc, ok := h.organizationContext(c)
	if !ok {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	records, errRecords := h.repo.ListOrganizationAPIKeys(ctx, cluster.OrganizationAPIKeyScope{OrgID: oc.org.ID, MemberUserID: oc.actorFilter()})
	if errRecords != nil {
		respondOrganizationUserError(c, "api_key_load_failed", errRecords)
		return
	}
	items := make([]gin.H, 0, len(records))
	for index := range records {
		item, okItem := apiKeyRecordResponse(c, &records[index])
		if !okItem {
			return
		}
		item["created_by_user_id"] = records[index].CreatedByUserID
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{"api_keys": items, "items": items})
}

// CreateMyOrganizationAPIKey creates an organization key owned by the caller.
// Owners may create a key on behalf of another member with member_user_id.
func (h *Handler) CreateMyOrganizationAPIKey(c *gin.Context) {
	oc, ok := h.organizationContext(c)
	if !ok {
		return
	}
	var body orgAPIKeyRequest
	if !decodeJSONBody(c, &body) {
		return
	}
	ownerID := oc.user.ID
	if body.MemberUserID != nil && *body.MemberUserID != oc.user.ID {
		if !requireOrganizationOwner(c, oc) {
			return
		}
		ownerID = *body.MemberUserID
	} else if !oc.canCreateOwnKeys() {
		respondError(c, http.StatusForbidden, "organization_member_keys_disabled", fmt.Errorf("organization owners must create keys for members"))
		return
	}
	key := ""
	if body.APIKey != nil {
		key = strings.TrimSpace(*body.APIKey)
	}
	if key == "" {
		generated, errGenerate := generateAPIKey()
		if errGenerate != nil {
			respondError(c, http.StatusInternalServerError, "api_key_generate_failed", errGenerate)
			return
		}
		key = generated
	}
	displayName, displayNameSet, okName := organizationKeyDisplayName(c, body.DisplayName)
	if !okName {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	record, errCreate := h.repo.CreateOrganizationAPIKey(ctx, oc.org.ID, ownerID, cluster.APIKeyUserUpdate{
		APIKey: &key, DisplayName: displayName, DisplayNameSet: displayNameSet,
	})
	if errCreate != nil {
		respondOrganizationUserError(c, "api_key_create_failed", errCreate)
		return
	}
	if errRefresh := h.refreshConfig(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return
	}
	item, okItem := apiKeyRecordResponse(c, record)
	if !okItem {
		return
	}
	item["created_by_user_id"] = record.CreatedByUserID
	c.JSON(http.StatusOK, gin.H{"api_key": item})
}

// UpdateMyOrganizationAPIKey renames or rotates a visible organization key.
func (h *Handler) UpdateMyOrganizationAPIKey(c *gin.Context) {
	oc, ok := h.organizationContext(c)
	if !ok {
		return
	}
	keyID, ok := organizationKeyIDParam(c)
	if !ok {
		return
	}
	var body orgAPIKeyRequest
	if !decodeJSONBody(c, &body) {
		return
	}
	displayName, displayNameSet, okName := organizationKeyDisplayName(c, body.DisplayName)
	if !okName {
		return
	}
	update := cluster.APIKeyUserUpdate{DisplayName: displayName, DisplayNameSet: displayNameSet}
	if body.APIKey != nil {
		next := strings.TrimSpace(*body.APIKey)
		update.APIKey = &next
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	record, errUpdate := h.repo.UpdateOrganizationAPIKey(ctx, cluster.OrganizationAPIKeyScope{OrgID: oc.org.ID, MemberUserID: oc.actorFilter()}, keyID, update)
	if errUpdate != nil {
		respondOrganizationUserError(c, "api_key_update_failed", errUpdate)
		return
	}
	if errRefresh := h.refreshConfig(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return
	}
	item, okItem := apiKeyRecordResponse(c, record)
	if !okItem {
		return
	}
	item["created_by_user_id"] = record.CreatedByUserID
	c.JSON(http.StatusOK, gin.H{"api_key": item})
}

// DeleteMyOrganizationAPIKey deletes a visible organization key.
func (h *Handler) DeleteMyOrganizationAPIKey(c *gin.Context) {
	oc, ok := h.organizationContext(c)
	if !ok {
		return
	}
	keyID, ok := organizationKeyIDParam(c)
	if !ok {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	if errDelete := h.repo.DeleteOrganizationAPIKey(ctx, cluster.OrganizationAPIKeyScope{OrgID: oc.org.ID, MemberUserID: oc.actorFilter()}, keyID); errDelete != nil {
		respondOrganizationUserError(c, "api_key_delete_failed", errDelete)
		return
	}
	if errRefresh := h.refreshConfig(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return
	}
	respondOK(c)
}

// ListMyOrganizationCharges lists charges: owners see all, members only their own.
func (h *Handler) ListMyOrganizationCharges(c *gin.Context) {
	oc, ok := h.organizationContext(c)
	if !ok {
		return
	}
	from, to, ok := userBillingDateRangeFromRequest(c)
	if !ok {
		return
	}
	limit, offset, ok := userBillingPaginationFromRequest(c)
	if !ok {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	orgID := oc.org.ID
	result, errCharges := h.repo.ListBillingCharges(ctx, cluster.BillingChargeQuery{
		From: from, ToExclusive: to, UserID: &orgID, ActorUserID: oc.actorFilter(), Limit: limit, Offset: offset,
	})
	if errCharges != nil {
		respondError(c, http.StatusInternalServerError, "billing_charge_load_failed", errCharges)
		return
	}
	items := make([]gin.H, 0, len(result.Records))
	for index := range result.Records {
		item := currentUserBillingChargeResponse(&result.Records[index])
		item["actor_user_id"] = result.Records[index].ActorUserID
		item["api_key_label"] = result.Records[index].APIKeyLabel
		if !oc.isOwner() {
			// The running organization balance is owner-only information.
			delete(item, "balance_after")
		}
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": result.Total, "limit": limit, "offset": offset})
}

// ListMyOrganizationBalanceRecords lists the organization ledger (owner only).
func (h *Handler) ListMyOrganizationBalanceRecords(c *gin.Context) {
	oc, ok := h.organizationContext(c)
	if !ok || !requireOrganizationOwner(c, oc) {
		return
	}
	from, to, ok := userBillingDateRangeFromRequest(c)
	if !ok {
		return
	}
	limit, offset, ok := userBillingPaginationFromRequest(c)
	if !ok {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	orgID := oc.org.ID
	result, errRecords := h.repo.ListBillingBalanceRecords(ctx, cluster.BillingBalanceQuery{
		From: from, ToExclusive: to, UserID: &orgID, Limit: limit, Offset: offset,
	})
	if errRecords != nil {
		respondError(c, http.StatusInternalServerError, "billing_balance_record_load_failed", errRecords)
		return
	}
	items := make([]gin.H, 0, len(result.Records))
	for index := range result.Records {
		items = append(items, currentUserBillingBalanceRecordResponse(&result.Records[index]))
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": result.Total, "limit": limit, "offset": offset})
}

// RechargeMyOrganization submits a top-up request for the organization (owner only).
func (h *Handler) RechargeMyOrganization(c *gin.Context) {
	oc, ok := h.organizationContext(c)
	if !ok || !requireOrganizationOwner(c, oc) {
		return
	}
	var body struct {
		Amount *float64 `json:"amount"`
		Note   string   `json:"note"`
	}
	if !decodeJSONBody(c, &body) {
		return
	}
	if body.Amount == nil || *body.Amount <= 0 || math.IsNaN(*body.Amount) || math.IsInf(*body.Amount, 0) {
		respondError(c, http.StatusBadRequest, "invalid_amount", fmt.Errorf("amount must be a positive finite number"))
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	record, balance, errRecharge := h.repo.CreateBillingRechargeRequest(ctx, oc.org.ID, *body.Amount, body.Note)
	if errRecharge != nil {
		switch {
		case errors.Is(errRecharge, cluster.ErrTopupPending):
			respondError(c, http.StatusConflict, "topup_pending", errRecharge)
		case errors.Is(errRecharge, cluster.ErrInvalidTopupAmount):
			respondError(c, http.StatusBadRequest, "invalid_amount", errRecharge)
		default:
			respondError(c, http.StatusInternalServerError, "billing_recharge_failed", errRecharge)
		}
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"request": record, "current_balance": balance})
}

// GetMyOrganizationRechargeRequest returns the pending top-up request (owner only).
func (h *Handler) GetMyOrganizationRechargeRequest(c *gin.Context) {
	oc, ok := h.organizationContext(c)
	if !ok || !requireOrganizationOwner(c, oc) {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	requests, errRequests := h.repo.PendingBillingRechargeRequests(ctx, []uint{oc.org.ID})
	if errRequests != nil {
		respondError(c, http.StatusInternalServerError, "billing_recharge_request_load_failed", errRequests)
		return
	}
	c.JSON(http.StatusOK, gin.H{"request": requests[oc.org.ID]})
}

// MyOrganizationAnalytics aggregates charges: owners see all, members only their own.
func (h *Handler) MyOrganizationAnalytics(c *gin.Context) {
	oc, ok := h.organizationContext(c)
	if !ok {
		return
	}
	query, errQuery := userAnalyticsQuery(c, time.Now(), oc.user.Timezone)
	if errQuery != nil {
		respondError(c, http.StatusBadRequest, "invalid_analytics_query", errQuery)
		return
	}
	query.ActorUserID = oc.actorFilter()
	ctx, cancel := requestContext(c)
	defer cancel()
	result, errAnalytics := h.repo.UserAnalytics(ctx, oc.org.ID, query)
	if errors.Is(errAnalytics, cluster.ErrUserAnalyticsKey) {
		respondError(c, http.StatusNotFound, "analytics_key_not_found", fmt.Errorf("API key not found"))
		return
	}
	if errAnalytics != nil {
		respondError(c, http.StatusInternalServerError, "analytics_load_failed", errAnalytics)
		return
	}
	c.JSON(http.StatusOK, userAnalyticsResponse{
		From: query.From, To: query.To, Timezone: query.Timezone, Source: "billing_charge",
		Model: query.Model, APIKeyID: query.APIKeyID, UserAnalyticsResult: result,
	})
}

// ListMyOrganizationRequestLogs lists request logs: owners see all, members only their own.
func (h *Handler) ListMyOrganizationRequestLogs(c *gin.Context) {
	oc, ok := h.organizationContext(c)
	if !ok {
		return
	}
	from, ok := userRequestLogTimeQuery(c, "from")
	if !ok {
		return
	}
	to, ok := userRequestLogTimeQuery(c, "to")
	if !ok {
		return
	}
	if from != nil && to != nil && from.After(*to) {
		respondError(c, http.StatusBadRequest, "invalid_time_range", fmt.Errorf("from must not be after to"))
		return
	}
	limit, offset, ok := userBillingPaginationFromRequest(c)
	if !ok {
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	result, errLogs := h.repo.ListUserRequestLogs(ctx, oc.org.ID, cluster.UserRequestLogQuery{
		From: from, To: to, RequestID: c.Query("request_id"), ActorUserID: oc.actorFilter(), Limit: limit, Offset: offset,
	})
	if errLogs != nil {
		respondError(c, http.StatusInternalServerError, "request_logs_load_failed", errLogs)
		return
	}
	items := make([]userRequestLogItem, 0, len(result.Items))
	for _, record := range result.Items {
		items = append(items, userRequestLogItem{
			Timestamp: record.Timestamp.UTC().Format(time.RFC3339Nano),
			EventType: record.EventType, Status: record.Status, Model: record.Model,
			Tokens: record.Tokens, LatencyMS: record.LatencyMS, RequestID: record.RequestID,
		})
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": result.Total, "limit": result.Limit, "offset": result.Offset, "summary": result.Summary})
}

func organizationMemberUpdateFromRequest(body orgMemberRequest) cluster.OrganizationMemberUpdate {
	return cluster.OrganizationMemberUpdate{
		Role:            body.Role,
		Limit5hCredits:  body.Limit5h.update(),
		WindowMode5h:    body.WindowMode5h,
		Limit1dCredits:  body.Limit1d.update(),
		WindowMode1d:    body.WindowMode1d,
		Limit7dCredits:  body.Limit7d.update(),
		WindowMode7d:    body.WindowMode7d,
		Limit30dCredits: body.Limit30d.update(),
		WindowMode30d:   body.WindowMode30d,
		ModelGroups:     body.ModelGroups,
		MaxAPIKeys:      cluster.OptionalIntFromFloat(body.MaxAPIKeys.update()),
	}
}

func organizationMemberUserResponse(member *cluster.OrganizationMemberRecord) gin.H {
	if member == nil {
		return gin.H{}
	}
	item := gin.H(cluster.OrganizationMemberLimitFields(member))
	item["user_id"] = member.MemberUserID
	item["role"] = member.Role
	item["created_at"] = member.CreatedAt
	return item
}

func organizationKeyDisplayName(c *gin.Context, raw json.RawMessage) (*string, bool, bool) {
	if len(raw) == 0 {
		return nil, false, true
	}
	var name *string
	if errName := json.Unmarshal(raw, &name); errName != nil {
		respondError(c, http.StatusBadRequest, "invalid_body", errName)
		return nil, false, false
	}
	return name, true, true
}

func organizationMemberIDParam(c *gin.Context) (uint, bool) {
	id, errID := cluster.ParseUserRecordID(c.Param("user_id"))
	if errID != nil {
		respondError(c, http.StatusBadRequest, "invalid_id", fmt.Errorf("invalid member id"))
		return 0, false
	}
	return id, true
}

func organizationKeyIDParam(c *gin.Context) (uint, bool) {
	id, errID := strconv.ParseUint(strings.TrimSpace(c.Param("key_id")), 10, 64)
	if errID != nil || id == 0 {
		respondError(c, http.StatusBadRequest, "invalid_id", fmt.Errorf("invalid api key id"))
		return 0, false
	}
	return uint(id), true
}

func respondOrganizationUserError(c *gin.Context, code string, err error) {
	switch {
	case errors.Is(err, cluster.ErrOrganizationNotFound), errors.Is(err, cluster.ErrOrganizationMemberNotFound),
		errors.Is(err, cluster.ErrUserNotFound), errors.Is(err, gorm.ErrRecordNotFound):
		respondError(c, http.StatusNotFound, "not_found", err)
	case errors.Is(err, cluster.ErrOrganizationMemberExists):
		respondError(c, http.StatusConflict, "member_exists", err)
	case errors.Is(err, cluster.ErrAPIKeyExists):
		respondError(c, http.StatusConflict, "api_key_exists", err)
	case errors.Is(err, cluster.ErrOrganizationLastOwner):
		respondError(c, http.StatusConflict, "last_owner", err)
	case errors.Is(err, cluster.ErrOrganizationInvalidMember), errors.Is(err, cluster.ErrOrganizationInvalidRole),
		errors.Is(err, cluster.ErrInvalidAPIKeyDisplayName), errors.Is(err, cluster.ErrDeletedAccessGroup):
		respondError(c, http.StatusBadRequest, "invalid_body", err)
	case errors.Is(err, cluster.ErrOrganizationMemberModelGroups):
		respondError(c, http.StatusBadRequest, "invalid_model_groups", err)
	case errors.Is(err, cluster.ErrOrganizationInvalidKeyLimit):
		respondError(c, http.StatusBadRequest, "invalid_key_limit", err)
	case errors.Is(err, cluster.ErrOrganizationMemberKeyLimit):
		respondError(c, http.StatusConflict, "member_key_limit", err)
	default:
		var configErr cluster.PeriodLimitConfigError
		if errors.As(err, &configErr) {
			code := "invalid_limit"
			if configErr.Code == cluster.PeriodLimitErrExceedsOrganization {
				code = configErr.Code
			}
			respondError(c, http.StatusBadRequest, code, err)
			return
		}
		respondError(c, http.StatusInternalServerError, code, err)
	}
}
