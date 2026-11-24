package management

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestOrganizationMemberResetAllowanceManagementHTTP(t *testing.T) {
	handler, engine, closeRepo := newUserManagementHTTPTestServer(t)
	defer closeRepo()
	engine.PUT("/organizations/:id/members/:user_id/reset-allowance", handler.SetOrganizationMemberResetAllowance)
	engine.POST("/organizations/:id/members/:user_id/period-limits/reset", handler.ResetOrganizationMemberPeriodLimits)
	engine.GET("/capabilities", handler.GetCapabilities)
	ctx := t.Context()
	name := "reset-admin-member"
	user, errUser := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &name})
	if errUser != nil {
		t.Fatal(errUser)
	}
	orgName := "reset-admin-org"
	org, errOrg := handler.repo.CreateOrganization(ctx, cluster.UserUpdate{Username: &orgName})
	if errOrg != nil {
		t.Fatal(errOrg)
	}
	if _, errAdd := handler.repo.AddOrganizationMember(ctx, org.ID, user.ID, cluster.OrganizationMemberUpdate{}); errAdd != nil {
		t.Fatal(errAdd)
	}
	base := fmt.Sprintf("/organizations/%d/members/%d", org.ID, user.ID)
	configure, reset := base+"/reset-allowance", base+"/period-limits/reset"
	for _, payload := range []string{`{}`, `{"mode":"one_time","amount":-1}`, `{"mode":"bad","amount":1}`, `{"mode":"monthly","amount":1,"expires_at":"2026-01-01T00:00:00Z"}`} {
		response := performUserManagementRequest(t, engine, http.MethodPut, configure, payload)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid policy: %d %s", response.Code, response.Body.String())
		}
	}
	response := performUserManagementRequest(t, engine, http.MethodPut, configure, `{"mode":"one_time","amount":1}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"reset_allowance"`) || !strings.Contains(response.Body.String(), `"spend_windows"`) {
		t.Fatalf("configure: %d %s", response.Code, response.Body.String())
	}
	noWindows := performUserManagementRequest(t, engine, http.MethodPost, reset, "")
	if noWindows.Code != http.StatusConflict || !strings.Contains(noWindows.Body.String(), "reset_allowance_no_enabled_windows") {
		t.Fatalf("no windows: %d %s", noWindows.Code, noWindows.Body.String())
	}
	if _, errUpdate := handler.repo.UpdateOrganizationMember(ctx, org.ID, user.ID, cluster.OrganizationMemberUpdate{Limit5hCredits: cluster.OptionalFloatUpdate{Set: true, Value: 0}}); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	for _, path := range []string{reset, reset + "?windows=5h"} {
		invalid := performUserManagementRequest(t, engine, http.MethodPost, path, `{}`)
		if invalid.Code != http.StatusBadRequest {
			t.Fatalf("selectors accepted: %d %s", invalid.Code, invalid.Body.String())
		}
	}
	consumed := performUserManagementRequest(t, engine, http.MethodPost, reset, "")
	if consumed.Code != http.StatusOK || !strings.Contains(consumed.Body.String(), `"available":0`) {
		t.Fatalf("consume: %d %s", consumed.Code, consumed.Body.String())
	}
	exhausted := performUserManagementRequest(t, engine, http.MethodPost, reset, "")
	if exhausted.Code != http.StatusConflict || !strings.Contains(exhausted.Body.String(), "reset_allowance_unavailable") {
		t.Fatalf("exhausted: %d %s", exhausted.Code, exhausted.Body.String())
	}
	missing := performUserManagementRequest(t, engine, http.MethodPost, fmt.Sprintf("/organizations/%d/members/%d/period-limits/reset", org.ID+999, user.ID), "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing: %d %s", missing.Code, missing.Body.String())
	}
	caps := performUserManagementRequest(t, engine, http.MethodGet, "/capabilities", "")
	if caps.Code != http.StatusOK || !strings.Contains(caps.Body.String(), `"organization_member_reset_allowance":true`) {
		t.Fatalf("capabilities: %d %s", caps.Code, caps.Body.String())
	}
}
