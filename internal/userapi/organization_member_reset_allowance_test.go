package userapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestOrganizationMemberResetAllowanceUserHTTP(t *testing.T) {
	env, closeRepo := newOrgTestEnv(t)
	defer closeRepo()
	ctx := t.Context()
	limit := cluster.OptionalFloatUpdate{Set: true, Value: 10}
	if _, errAdd := env.handler.repo.AddOrganizationMember(ctx, env.org.ID, env.member.ID, cluster.OrganizationMemberUpdate{Limit5hCredits: limit}); errAdd != nil {
		t.Fatal(errAdd)
	}
	base := fmt.Sprintf("/user/organizations/%d/members/%d", env.org.ID, env.member.ID)
	configure := base + "/reset-allowance"
	reset := base + "/period-limits/reset"
	policy := `{"mode":"one_time","amount":2}`
	expectStatus(t, env.do(t, env.member, http.MethodPut, configure, policy), http.StatusForbidden)
	expectStatus(t, env.do(t, env.solo, http.MethodPut, configure, policy), http.StatusNotFound)
	expectStatus(t, env.do(t, env.solo, http.MethodPost, reset, ""), http.StatusNotFound)
	otherReset := fmt.Sprintf("/user/organizations/%d/members/%d/period-limits/reset", env.org.ID, env.owner.ID)
	expectStatus(t, env.do(t, env.member, http.MethodPost, otherReset, ""), http.StatusForbidden)
	for _, payload := range []string{`{}`, `{"mode":"monthly","amount":-1}`, `{"mode":"bad","amount":1}`, `{"mode":"one_time","amount":1.5}`, `{"mode":"monthly","amount":1,"expires_at":"2026-01-01T00:00:00Z"}`} {
		expectStatus(t, env.do(t, env.owner, http.MethodPut, configure, payload), http.StatusBadRequest)
	}
	configured := env.do(t, env.owner, http.MethodPut, configure, policy)
	expectStatus(t, configured, http.StatusOK)
	if !strings.Contains(configured.Body.String(), `"spend_windows"`) || !strings.Contains(configured.Body.String(), `"reset_allowance"`) {
		t.Fatal(configured.Body.String())
	}
	for _, body := range []string{`{}`, `{"windows":["5h"]}`} {
		expectStatus(t, env.do(t, env.member, http.MethodPost, reset, body), http.StatusBadRequest)
	}
	expectStatus(t, env.do(t, env.member, http.MethodPost, reset+"?mode=counter", ""), http.StatusBadRequest)
	for _, caller := range []*cluster.UserRecord{env.member, env.owner} {
		response := env.do(t, caller, http.MethodPost, reset, "")
		expectStatus(t, response, http.StatusOK)
		var result struct {
			Member struct {
				UserID         uint                                    `json:"user_id"`
				ResetAllowance cluster.UserResetAllowanceStatus        `json:"reset_allowance"`
				Windows        []cluster.OrganizationMemberSpendWindow `json:"spend_windows"`
			} `json:"member"`
		}
		if errDecode := json.Unmarshal(response.Body.Bytes(), &result); errDecode != nil {
			t.Fatal(errDecode)
		}
		if result.Member.UserID != env.member.ID || len(result.Member.Windows) != 4 {
			t.Fatalf("response: %s", response.Body.String())
		}
	}
	exhausted := env.do(t, env.member, http.MethodPost, reset, "")
	expectStatus(t, exhausted, http.StatusConflict)
	if !strings.Contains(exhausted.Body.String(), "reset_allowance_unavailable") {
		t.Fatal(exhausted.Body.String())
	}
	summary := env.do(t, env.member, http.MethodGet, fmt.Sprintf("/user/organizations/%d", env.org.ID), "")
	expectStatus(t, summary, http.StatusOK)
	if !strings.Contains(summary.Body.String(), `"reset_allowance"`) {
		t.Fatal(summary.Body.String())
	}
	caps := env.do(t, env.member, http.MethodGet, "/user/capabilities", "")
	expectStatus(t, caps, http.StatusOK)
	if !strings.Contains(caps.Body.String(), `"organization_member_reset_allowance":true`) {
		t.Fatal(caps.Body.String())
	}
	missing := fmt.Sprintf("/user/organizations/%d/members/999999/reset-allowance", env.org.ID)
	expectStatus(t, env.do(t, env.owner, http.MethodPut, missing, policy), http.StatusNotFound)
}
