package userapi

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

// SetMyOrganizationMemberResetAllowance allows owners to replace a member's policy.
func (h *Handler) SetMyOrganizationMemberResetAllowance(c *gin.Context) {
	oc, ok := h.organizationContext(c)
	if !ok || !requireOrganizationOwner(c, oc) {
		return
	}
	memberID, ok := organizationMemberIDParam(c)
	if !ok {
		return
	}
	var body struct {
		Mode      string     `json:"mode"`
		Amount    *int64     `json:"amount"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if !decodeJSONBody(c, &body) {
		return
	}
	if body.Amount == nil {
		respondError(c, http.StatusBadRequest, "invalid body", errors.New("amount is required and must be a non-negative integer"))
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	member, errSet := h.repo.SetOrganizationMemberResetAllowance(ctx, oc.org.ID, memberID, body.Mode, *body.Amount, body.ExpiresAt, time.Now().UTC())
	if errSet != nil {
		var fieldErr cluster.PeriodLimitConfigError
		if errors.As(errSet, &fieldErr) {
			respondError(c, http.StatusBadRequest, fieldErr.Code, errSet)
		} else {
			respondOrganizationUserError(c, "organization_member_reset_allowance_set_failed", errSet)
		}
		return
	}
	windows, errWindows := h.repo.OrganizationMemberSpend(ctx, oc.org.ID, memberID, time.Now().UTC())
	if errWindows != nil {
		respondOrganizationUserError(c, "organization_member_spend_failed", errWindows)
		return
	}
	item := organizationMemberUserResponse(member)
	item["spend_windows"] = windows
	c.JSON(http.StatusOK, gin.H{"member": item})
}

// ResetMyOrganizationMemberPeriodLimits allows owners or the member themself to consume.
func (h *Handler) ResetMyOrganizationMemberPeriodLimits(c *gin.Context) {
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
	ctx, cancel := requestContext(c)
	defer cancel()
	member, windows, errReset := h.repo.ConsumeOrganizationMemberResetAllowance(ctx, oc.org.ID, memberID, time.Now().UTC())
	if errReset != nil {
		switch {
		case errors.Is(errReset, cluster.ErrResetAllowanceNoWindows):
			respondError(c, http.StatusConflict, "reset_allowance_no_enabled_windows", errReset)
		case errors.Is(errReset, cluster.ErrResetAllowanceUnavailable):
			respondError(c, http.StatusConflict, "reset_allowance_unavailable", errReset)
		default:
			respondOrganizationUserError(c, "organization_member_period_limits_reset_failed", errReset)
		}
		return
	}
	item := organizationMemberUserResponse(member)
	item["spend_windows"] = windows
	c.JSON(http.StatusOK, gin.H{"member": item})
}
