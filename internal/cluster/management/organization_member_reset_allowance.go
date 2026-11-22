package management

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

// SetOrganizationMemberResetAllowance configures a membership-specific reset policy.
func (h *Handler) SetOrganizationMemberResetAllowance(c *gin.Context) {
	orgID, ok := organizationIDFromParam(c)
	if !ok {
		return
	}
	memberID, ok := organizationMemberIDFromParam(c)
	if !ok {
		return
	}
	var body struct {
		Mode      string     `json:"mode"`
		Amount    *int64     `json:"amount"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errBind)
		return
	}
	if body.Amount == nil {
		respondError(c, http.StatusBadRequest, "invalid body", errors.New("amount is required and must be a non-negative integer"))
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	member, errSet := h.repo.SetOrganizationMemberResetAllowance(ctx, orgID, memberID, body.Mode, *body.Amount, body.ExpiresAt, time.Now().UTC())
	if errSet != nil {
		if !respondUserPeriodLimitValidationError(c, errSet) {
			respondOrganizationError(c, "organization_member_reset_allowance_set_failed", errSet)
		}
		return
	}
	windows, errWindows := h.repo.OrganizationMemberSpend(ctx, orgID, memberID, time.Now().UTC())
	if errWindows != nil {
		respondOrganizationError(c, "organization_member_spend_failed", errWindows)
		return
	}
	item := organizationMemberRecordResponse(member)
	item["spend_windows"] = windows
	c.JSON(http.StatusOK, gin.H{"member": item})
}

// ResetOrganizationMemberPeriodLimits consumes an allowance even for administrators.
func (h *Handler) ResetOrganizationMemberPeriodLimits(c *gin.Context) {
	orgID, ok := organizationIDFromParam(c)
	if !ok {
		return
	}
	memberID, ok := organizationMemberIDFromParam(c)
	if !ok {
		return
	}
	if len(c.Request.URL.Query()) != 0 {
		respondError(c, http.StatusBadRequest, "invalid body", errors.New("reset accepts no selectors or body"))
		return
	}
	if c.Request.Body != nil {
		body, errRead := io.ReadAll(io.LimitReader(c.Request.Body, 1))
		if errRead != nil || len(body) != 0 {
			respondError(c, http.StatusBadRequest, "invalid body", errors.New("reset accepts no selectors or body"))
			return
		}
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	member, windows, errReset := h.repo.ConsumeOrganizationMemberResetAllowance(ctx, orgID, memberID, time.Now().UTC())
	if errReset != nil {
		switch {
		case errors.Is(errReset, cluster.ErrResetAllowanceNoWindows):
			respondError(c, http.StatusConflict, "reset_allowance_no_enabled_windows", errReset)
		case errors.Is(errReset, cluster.ErrResetAllowanceUnavailable):
			respondError(c, http.StatusConflict, "reset_allowance_unavailable", errReset)
		default:
			respondOrganizationError(c, "organization_member_period_limits_reset_failed", errReset)
		}
		return
	}
	item := organizationMemberRecordResponse(member)
	item["spend_windows"] = windows
	c.JSON(http.StatusOK, gin.H{"member": item})
}
