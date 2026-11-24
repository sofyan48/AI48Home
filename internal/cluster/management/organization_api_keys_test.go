package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestOrganizationManagementNamesAndRenamesAPIKeys(t *testing.T) {
	handler, engine, closeRepo := newUserManagementHTTPTestServer(t)
	defer closeRepo()
	engine.POST("/organizations", handler.CreateOrganization)
	engine.POST("/organizations/:id/api-keys", handler.CreateOrganizationAPIKey)
	engine.PATCH("/organizations/:id/api-keys/:key_id", handler.UpdateOrganizationAPIKey)
	ctx := t.Context()

	name := "owner"
	owner, errOwner := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &name})
	if errOwner != nil {
		t.Fatal(errOwner)
	}
	created := performUserManagementRequest(t, engine, http.MethodPost, "/organizations", fmt.Sprintf(`{"name":"acme","owner_user_id":%d}`, owner.ID))
	if created.Code != http.StatusOK {
		t.Fatalf("create organization = %d %s", created.Code, created.Body.String())
	}
	var org struct {
		Organization struct {
			ID uint `json:"id"`
		} `json:"organization"`
	}
	if errDecode := json.Unmarshal(created.Body.Bytes(), &org); errDecode != nil || org.Organization.ID == 0 {
		t.Fatalf("decode organization: %v %s", errDecode, created.Body.String())
	}
	base := fmt.Sprintf("/organizations/%d/api-keys", org.Organization.ID)

	key := performUserManagementRequest(t, engine, http.MethodPost, base, fmt.Sprintf(`{"member_user_id":%d,"display_name":"ci"}`, owner.ID))
	if key.Code != http.StatusOK || !strings.Contains(key.Body.String(), `"display_name":"ci"`) {
		t.Fatalf("create named key = %d %s", key.Code, key.Body.String())
	}
	var keyBody struct {
		APIKey struct {
			ID uint `json:"id"`
		} `json:"api_key"`
	}
	if errDecode := json.Unmarshal(key.Body.Bytes(), &keyBody); errDecode != nil || keyBody.APIKey.ID == 0 {
		t.Fatalf("decode key: %v %s", errDecode, key.Body.String())
	}
	keyPath := fmt.Sprintf("%s/%d", base, keyBody.APIKey.ID)

	renamed := performUserManagementRequest(t, engine, http.MethodPatch, keyPath, `{"display_name":"  deploy bot  "}`)
	if renamed.Code != http.StatusOK || !strings.Contains(renamed.Body.String(), `"display_name":"deploy bot"`) {
		t.Fatalf("rename key = %d %s", renamed.Code, renamed.Body.String())
	}
	cleared := performUserManagementRequest(t, engine, http.MethodPatch, keyPath, `{"display_name":null}`)
	if cleared.Code != http.StatusOK || !strings.Contains(cleared.Body.String(), `"display_name":null`) {
		t.Fatalf("clear key name = %d %s", cleared.Code, cleared.Body.String())
	}
	if missing := performUserManagementRequest(t, engine, http.MethodPatch, keyPath, `{}`); missing.Code != http.StatusBadRequest {
		t.Fatalf("rename without display_name = %d %s", missing.Code, missing.Body.String())
	}
	if invalid := performUserManagementRequest(t, engine, http.MethodPatch, keyPath, fmt.Sprintf(`{"display_name":%q}`, strings.Repeat("x", cluster.APIKeyDisplayNameMaxLength+1))); invalid.Code != http.StatusBadRequest {
		t.Fatalf("rename too long = %d %s", invalid.Code, invalid.Body.String())
	}
	other := performUserManagementRequest(t, engine, http.MethodPatch, fmt.Sprintf("/organizations/%d/api-keys/%d", org.Organization.ID+999, keyBody.APIKey.ID), `{"display_name":"x"}`)
	if other.Code != http.StatusNotFound {
		t.Fatalf("rename through another organization = %d %s", other.Code, other.Body.String())
	}
}
