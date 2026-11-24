package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestOrganizationManagementChannelScope(t *testing.T) {
	handler, engine, closeRepo := newUserManagementHTTPTestServer(t)
	defer closeRepo()
	engine.GET("/organizations", handler.ListOrganizations)
	engine.POST("/organizations", handler.CreateOrganization)
	engine.GET("/organizations/:id", handler.GetOrganization)
	engine.PATCH("/organizations/:id", handler.UpdateOrganization)
	ctx := t.Context()
	created := performUserManagementRequest(t, engine, http.MethodPost, "/organizations", `{"name":"scoped","channel_groups":[8,7,8]}`)
	if created.Code != http.StatusOK || !strings.Contains(created.Body.String(), `"channel_groups":[7,8]`) {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	var body struct {
		Organization struct {
			ID uint `json:"id"`
		} `json:"organization"`
	}
	if errDecode := json.Unmarshal(created.Body.Bytes(), &body); errDecode != nil {
		t.Fatal(errDecode)
	}
	id := body.Organization.ID
	key, errKey := handler.repo.CreateAPIKey(ctx, cluster.APIKeyEntryUpdate{APIKey: "org-key", UserID: &id})
	if errKey != nil {
		t.Fatal(errKey)
	}
	path := fmt.Sprintf("/organizations/%d", id)
	for _, url := range []string{"/organizations", path} {
		response := performUserManagementRequest(t, engine, http.MethodGet, url, "")
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"channel_groups":[7,8]`) {
			t.Fatalf("get=%d %s", response.Code, response.Body.String())
		}
	}
	for _, patch := range []struct {
		body string
		want []uint
	}{
		{`{"name":"renamed"}`, []uint{7, 8}},
		{`{"channel_groups":[9]}`, []uint{9}},
		{`{"model_groups":[]}`, []uint{9}},
		{`{"channel_groups":[]}`, []uint{}},
	} {
		response := performUserManagementRequest(t, engine, http.MethodPatch, path, patch.body)
		if response.Code != http.StatusOK {
			t.Fatalf("patch=%d %s", response.Code, response.Body.String())
		}
		var result struct {
			Organization struct {
				Channels []uint `json:"channel_groups"`
			} `json:"organization"`
		}
		if errDecode := json.Unmarshal(response.Body.Bytes(), &result); errDecode != nil || !reflect.DeepEqual(result.Organization.Channels, patch.want) {
			t.Fatalf("patch scope=%v error=%v want=%v", result.Organization.Channels, errDecode, patch.want)
		}
		entries, errEntries := handler.repo.ListAPIKeyEntries(ctx)
		if errEntries != nil || len(entries) != 1 || entries[0].ID != key.ID || !slices.Equal(entries[0].Channels, patch.want) {
			t.Fatalf("key scope=%+v error=%v", entries, errEntries)
		}
	}
	invalid := performUserManagementRequest(t, engine, http.MethodPatch, path, `{"channel_groups":"bad"}`)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid=%d %s", invalid.Code, invalid.Body.String())
	}
}
