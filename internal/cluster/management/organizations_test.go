package management

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestOrganizationManagementCreateAtomicAndLastOwnerDelete(t *testing.T) {
	handler, engine, closeRepo := newUserManagementHTTPTestServer(t)
	defer closeRepo()
	engine.GET("/organizations", handler.ListOrganizations)
	engine.POST("/organizations", handler.CreateOrganization)
	engine.DELETE("/users/:id", handler.DeleteUser)
	ctx := t.Context()

	name := "owner"
	owner, errOwner := handler.repo.CreateUser(ctx, cluster.UserUpdate{Username: &name})
	if errOwner != nil {
		t.Fatal(errOwner)
	}

	// An unknown owner rolls back the whole create.
	missing := performUserManagementRequest(t, engine, http.MethodPost, "/organizations", `{"name":"acme","owner_user_id":999999}`)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("create with missing owner = %d %s", missing.Code, missing.Body.String())
	}
	list := performUserManagementRequest(t, engine, http.MethodGet, "/organizations", "")
	if !strings.Contains(list.Body.String(), `"organizations":[]`) {
		t.Fatalf("failed create left an organization: %s", list.Body.String())
	}

	created := performUserManagementRequest(t, engine, http.MethodPost, "/organizations", fmt.Sprintf(`{"name":"acme","owner_user_id":%d,"allow_member_keys":false}`, owner.ID))
	if created.Code != http.StatusOK || !strings.Contains(created.Body.String(), `"allow_member_keys":false`) {
		t.Fatalf("create organization = %d %s", created.Code, created.Body.String())
	}
	duplicate := performUserManagementRequest(t, engine, http.MethodPost, "/organizations", `{"name":"acme"}`)
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate organization = %d %s", duplicate.Code, duplicate.Body.String())
	}

	blocked := performUserManagementRequest(t, engine, http.MethodDelete, fmt.Sprintf("/users/%d", owner.ID), "")
	if blocked.Code != http.StatusConflict || !strings.Contains(blocked.Body.String(), "last_owner") {
		t.Fatalf("delete last owner = %d %s", blocked.Code, blocked.Body.String())
	}
}
