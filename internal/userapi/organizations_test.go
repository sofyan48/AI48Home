package userapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

type orgTestEnv struct {
	handler *Handler
	router  *gin.Engine
	org     *cluster.UserRecord
	owner   *cluster.UserRecord
	member  *cluster.UserRecord
	solo    *cluster.UserRecord
	tokens  map[uint]string
}

func newOrgTestEnv(t *testing.T) (*orgTestEnv, func()) {
	t.Helper()
	handler, closeRepo := newUserBillingTestHandler(t)
	ctx := context.Background()
	create := func(name string) *cluster.UserRecord {
		credits := 5.0
		user, errCreate := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &name, Credits: &credits})
		if errCreate != nil {
			t.Fatalf("CreateUser(%s) error = %v", name, errCreate)
		}
		return user
	}
	env := &orgTestEnv{handler: handler, owner: create("owner"), member: create("member"), solo: create("solo"), tokens: map[uint]string{}}
	name, credits := "acme", 100.0
	org, errOrg := handler.repo.CreateOrganization(ctx, cluster.UserUpdate{Username: &name, Credits: &credits})
	if errOrg != nil {
		t.Fatalf("CreateOrganization() error = %v", errOrg)
	}
	env.org = org
	ownerRole := cluster.OrganizationRoleOwner
	if _, errAdd := handler.repo.AddOrganizationMember(ctx, org.ID, env.owner.ID, cluster.OrganizationMemberUpdate{Role: &ownerRole}); errAdd != nil {
		t.Fatalf("AddOrganizationMember(owner) error = %v", errAdd)
	}
	for _, user := range []*cluster.UserRecord{env.owner, env.member, env.solo} {
		env.tokens[user.ID] = createUserBillingBearerToken(t, handler, user.ID)
	}
	env.router = gin.New()
	Register(env.router.Group("/user"), handler)
	return env, closeRepo
}

func (e *orgTestEnv) do(t *testing.T, user *cluster.UserRecord, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.tokens[user.ID])
	req.Header.Set("Content-Type", "application/json")
	e.router.ServeHTTP(resp, req)
	return resp
}

func expectStatus(t *testing.T, resp *httptest.ResponseRecorder, want int) {
	t.Helper()
	if resp.Code != want {
		t.Fatalf("status=%d want=%d body=%s", resp.Code, want, resp.Body.String())
	}
}

func TestOrganizationAccountCannotLogIn(t *testing.T) {
	env, closeRepo := newOrgTestEnv(t)
	defer closeRepo()
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/user/login", strings.NewReader(`{"username":"acme","password":""}`))
	req.Header.Set("Content-Type", "application/json")
	env.router.ServeHTTP(resp, req)
	expectStatus(t, resp, http.StatusUnauthorized)

	// A bearer token minted for the organization ID must not authenticate either.
	token := createUserBillingBearerToken(t, env.handler, env.org.ID)
	me := httptest.NewRecorder()
	meReq := httptest.NewRequest(http.MethodGet, "/user/me", nil)
	meReq.Header.Set("Authorization", "Bearer "+token)
	env.router.ServeHTTP(me, meReq)
	expectStatus(t, me, http.StatusUnauthorized)
}

func TestPersonalUserWithoutOrganizationIsUnaffected(t *testing.T) {
	env, closeRepo := newOrgTestEnv(t)
	defer closeRepo()
	orgBase := fmt.Sprintf("/user/organizations/%d", env.org.ID)

	list := env.do(t, env.solo, http.MethodGet, "/user/organizations", "")
	expectStatus(t, list, http.StatusOK)
	if !strings.Contains(list.Body.String(), `"organizations":[]`) {
		t.Fatalf("solo organizations = %s, want empty list", list.Body.String())
	}
	// Non-members cannot discover or touch the organization.
	for _, path := range []string{orgBase, orgBase + "/api-keys", orgBase + "/billing/charges", orgBase + "/members"} {
		expectStatus(t, env.do(t, env.solo, http.MethodGet, path, ""), http.StatusNotFound)
	}
	expectStatus(t, env.do(t, env.solo, http.MethodPost, orgBase+"/api-keys", `{}`), http.StatusNotFound)

	// Personal routes keep working exactly as before.
	expectStatus(t, env.do(t, env.solo, http.MethodGet, "/user/me", ""), http.StatusOK)
	created := env.do(t, env.solo, http.MethodPost, "/user/api-keys", `{"display_name":"mine"}`)
	expectStatus(t, created, http.StatusOK)
	keys := env.do(t, env.solo, http.MethodGet, "/user/api-keys", "")
	expectStatus(t, keys, http.StatusOK)
	if strings.Count(keys.Body.String(), `"display_name":"mine"`) != 2 {
		t.Fatalf("solo keys = %s", keys.Body.String())
	}
	expectStatus(t, env.do(t, env.solo, http.MethodGet, "/user/billing/charges", ""), http.StatusOK)
	expectStatus(t, env.do(t, env.solo, http.MethodGet, "/user/period-limits", ""), http.StatusOK)
	expectStatus(t, env.do(t, env.solo, http.MethodPost, "/user/billing/recharge", `{"amount":3}`), http.StatusAccepted)

	// Admission for a personal key ignores organizations entirely.
	ctx := context.Background()
	records, _ := env.handler.repo.ListAPIKeyRecordsForUser(ctx, env.solo.ID)
	if len(records) != 1 || records[0].CreatedByUserID != nil {
		t.Fatalf("personal key = %+v, want no organization attribution", records)
	}
	if _, _, errAllowed := env.handler.repo.AllowedDispatchIDsForAPIKey(ctx, records[0].APIKey); errAllowed != nil {
		t.Fatalf("personal key admission error = %v", errAllowed)
	}
	// Personal users never appear as organizations and vice versa.
	users, _ := env.handler.repo.ListUsers(ctx)
	for _, user := range users {
		if user.IsOrganization() {
			t.Fatalf("ListUsers() returned organization %q", user.Username)
		}
	}
}

func TestOrganizationOwnerAndMemberPermissions(t *testing.T) {
	env, closeRepo := newOrgTestEnv(t)
	defer closeRepo()
	base := fmt.Sprintf("/user/organizations/%d", env.org.ID)

	// Owner adds the member by username with a spend limit.
	add := env.do(t, env.owner, http.MethodPost, base+"/members", `{"username":"member","limit_1d_credits":2}`)
	expectStatus(t, add, http.StatusOK)
	expectStatus(t, env.do(t, env.owner, http.MethodPost, base+"/members", `{"username":"acme"}`), http.StatusNotFound)

	// Member sees own spend limits but not the organization balance.
	summary := env.do(t, env.member, http.MethodGet, base, "")
	expectStatus(t, summary, http.StatusOK)
	if strings.Contains(summary.Body.String(), `"credits"`) || !strings.Contains(summary.Body.String(), `"spend_limits"`) {
		t.Fatalf("member summary leaked balance or missed limits: %s", summary.Body.String())
	}
	ownerSummary := env.do(t, env.owner, http.MethodGet, base, "")
	if !strings.Contains(ownerSummary.Body.String(), `"credits":100`) {
		t.Fatalf("owner summary = %s", ownerSummary.Body.String())
	}

	// Member-only restrictions.
	for _, call := range []struct{ method, path, body string }{
		{http.MethodGet, base + "/members", ""},
		{http.MethodPost, base + "/members", `{"username":"solo"}`},
		{http.MethodPatch, fmt.Sprintf("%s/members/%d", base, env.owner.ID), `{"role":"member"}`},
		{http.MethodGet, base + "/billing/balance-records", ""},
		{http.MethodPost, base + "/billing/recharge", `{"amount":1}`},
		{http.MethodPost, base + "/api-keys", fmt.Sprintf(`{"member_user_id":%d}`, env.owner.ID)},
	} {
		expectStatus(t, env.do(t, env.member, call.method, call.path, call.body), http.StatusForbidden)
	}

	// Each creates their own key; member only sees their own.
	memberKey := env.do(t, env.member, http.MethodPost, base+"/api-keys", `{"display_name":"member-key"}`)
	expectStatus(t, memberKey, http.StatusOK)
	ownerKey := env.do(t, env.owner, http.MethodPost, base+"/api-keys", `{"display_name":"owner-key"}`)
	expectStatus(t, ownerKey, http.StatusOK)
	var ownerKeyBody struct {
		APIKey struct {
			ID uint `json:"id"`
		} `json:"api_key"`
	}
	if errDecode := json.Unmarshal(ownerKey.Body.Bytes(), &ownerKeyBody); errDecode != nil {
		t.Fatal(errDecode)
	}
	memberList := env.do(t, env.member, http.MethodGet, base+"/api-keys", "")
	if strings.Contains(memberList.Body.String(), "owner-key") || !strings.Contains(memberList.Body.String(), "member-key") {
		t.Fatalf("member key list = %s", memberList.Body.String())
	}
	ownerList := env.do(t, env.owner, http.MethodGet, base+"/api-keys", "")
	if !strings.Contains(ownerList.Body.String(), "owner-key") || !strings.Contains(ownerList.Body.String(), "member-key") {
		t.Fatalf("owner key list = %s", ownerList.Body.String())
	}
	// Member cannot delete or rename the owner's key.
	ownerKeyPath := fmt.Sprintf("%s/api-keys/%d", base, ownerKeyBody.APIKey.ID)
	expectStatus(t, env.do(t, env.member, http.MethodDelete, ownerKeyPath, "{}"), http.StatusNotFound)
	expectStatus(t, env.do(t, env.member, http.MethodPatch, ownerKeyPath, `{"display_name":"x"}`), http.StatusNotFound)

	// Organization keys do not leak into personal key lists.
	personal := env.do(t, env.member, http.MethodGet, "/user/api-keys", "")
	if strings.Contains(personal.Body.String(), "member-key") {
		t.Fatalf("organization key leaked into personal list: %s", personal.Body.String())
	}

	// Last owner cannot leave; member can leave and their key disappears.
	expectStatus(t, env.do(t, env.owner, http.MethodDelete, fmt.Sprintf("%s/members/%d", base, env.owner.ID), "{}"), http.StatusConflict)
	expectStatus(t, env.do(t, env.member, http.MethodDelete, fmt.Sprintf("%s/members/%d", base, env.member.ID), "{}"), http.StatusOK)
	afterLeave := env.do(t, env.owner, http.MethodGet, base+"/api-keys", "")
	if strings.Contains(afterLeave.Body.String(), "member-key") {
		t.Fatalf("member key survived leaving: %s", afterLeave.Body.String())
	}
	expectStatus(t, env.do(t, env.member, http.MethodGet, base, ""), http.StatusNotFound)
}
