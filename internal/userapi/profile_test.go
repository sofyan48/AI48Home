package userapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func createProfileTestUser(t *testing.T, handler *Handler, name string) *cluster.UserRecord {
	t.Helper()
	password, email, credits := "unchanged-password-hash", name+"@example.com", 42.0
	mfa, passkey := cluster.JSONB(`{"secret":"unchanged"}`), cluster.JSONB(`[]`)
	user, errCreate := handler.repo.CreateUser(context.Background(), cluster.UserUpdate{
		Username: &name, Password: &password, Email: &email, Credits: &credits, MFA: &mfa, Passkey: &passkey,
		Limit1dCredits: cluster.OptionalFloatUpdate{Set: true, Value: 10},
	})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	return user
}

func profileTestSnapshot(t *testing.T, handler *Handler, id uint) *cluster.UserRecord {
	t.Helper()
	user, errUser := handler.repo.GetUser(context.Background(), id)
	if errUser != nil {
		t.Fatal(errUser)
	}
	return user
}

func TestUpdateCurrentUserAllowedEdits(t *testing.T) {
	handler, router, _ := newUserEmailTestHandler(t, nil)
	user := createProfileTestUser(t, handler, "alice")
	other := createProfileTestUser(t, handler, "other")
	beforeOther := profileTestSnapshot(t, handler, other.ID)
	before := profileTestSnapshot(t, handler, user.ID)
	token := createUserTestBearerToken(t, handler, user.ID, user.SessionVersion)
	for _, edit := range []struct {
		body, name, timezone string
	}{
		{`{"username":"  renamed  "}`, "renamed", user.Timezone},
		{`{"timezone":" UTC "}`, "renamed", "UTC"},
		{`{"username":"final","timezone":"America/New_York"}`, "final", "America/New_York"},
	} {
		response := performUserJSONRequest(t, router, http.MethodPatch, "/user/me", json.RawMessage(edit.body), token)
		if response.Code != http.StatusOK {
			t.Fatalf("PATCH = %d %s", response.Code, response.Body.String())
		}
		var body struct {
			Status string         `json:"status"`
			User   map[string]any `json:"user"`
		}
		if errDecode := json.Unmarshal(response.Body.Bytes(), &body); errDecode != nil {
			t.Fatal(errDecode)
		}
		if body.Status != "ok" || body.User["username"] != edit.name || body.User["timezone"] != edit.timezone || body.User["id"] != float64(user.ID) {
			t.Fatalf("unexpected profile: %s", response.Body.String())
		}
		if _, exists := body.User["password"]; exists || len(response.Result().Cookies()) != 0 {
			t.Fatal("profile edit exposed password or changed cookies")
		}
		after := profileTestSnapshot(t, handler, user.ID)
		if after.Username != edit.name || after.Timezone != edit.timezone {
			t.Fatalf("edits not persisted: %+v", after)
		}
		after.Username, after.Timezone, after.UpdatedAt = before.Username, before.Timezone, before.UpdatedAt
		if !reflect.DeepEqual(before, after) {
			t.Fatal("profile edit changed non-profile fields")
		}
		me := performUserJSONRequest(t, router, http.MethodGet, "/user/me", nil, token)
		if me.Code != http.StatusOK || me.Body.String() != response.Body.String() {
			t.Fatalf("existing session or GET profile changed: %d %s", me.Code, me.Body.String())
		}
	}
	if !reflect.DeepEqual(beforeOther, profileTestSnapshot(t, handler, other.ID)) {
		t.Fatal("another user's profile changed")
	}
}

func TestUpdateCurrentUserRejectsInvalidPayloadWithoutMutation(t *testing.T) {
	handler, router, _ := newUserEmailTestHandler(t, nil)
	user := createProfileTestUser(t, handler, "alice")
	other := createProfileTestUser(t, handler, "taken")
	token := createUserTestBearerToken(t, handler, user.ID, user.SessionVersion)
	before := profileTestSnapshot(t, handler, user.ID)
	beforeOther := profileTestSnapshot(t, handler, other.ID)
	payloads := []string{
		``, ` `, `{}`, `null`, `[]`, `true`, `"text"`, `{`, `{} {}`,
		`{"username":""}`, `{"username":" \t "}`, `{"username":null}`, `{"username":1}`, `{"username":{}}`,
		`{"timezone":""}`, `{"timezone":null}`, `{"timezone":false}`,
		`{"username":"changed","timezone":"Mars/Olympus"}`, `{"Username":"changed"}`,
		`{"username":"changed","credits":999}`, `{"username":"changed","password":"hacked"}`,
		`{"username":"changed","id":2}`, `{"username":"changed","approval_pending":false}`,
	}
	for _, field := range []string{
		"id", "user_id", "email", "email_verified_at", "email_version", "password", "new_password",
		"credits", "credits_unlimited", "billing_multiplier", "approval_pending", "session_version",
		"mfa", "passkey", "limit_1d_credits", "window_mode_1d", "week_reset_day", "reset_allowance", "unknown",
	} {
		payloads = append(payloads, `{"username":"changed","`+field+`":null}`)
	}
	for _, payload := range payloads {
		t.Run(payload, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPatch, "/user/me", strings.NewReader(payload))
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			wantCode := "invalid_body"
			if strings.Contains(payload, "Mars/Olympus") {
				wantCode = "invalid_timezone"
			}
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"error":"`+wantCode+`"`) {
				t.Fatalf("status = %d %s", response.Code, response.Body.String())
			}
			if !reflect.DeepEqual(before, profileTestSnapshot(t, handler, user.ID)) || !reflect.DeepEqual(beforeOther, profileTestSnapshot(t, handler, other.ID)) {
				t.Fatal("invalid request mutated a user")
			}
		})
	}
	conflict := performUserJSONRequest(t, router, http.MethodPatch, "/user/me", map[string]string{"username": other.Username, "timezone": "UTC"}, token)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), `"error":"user_exists"`) {
		t.Fatalf("conflict = %d %s", conflict.Code, conflict.Body.String())
	}
	if !reflect.DeepEqual(before, profileTestSnapshot(t, handler, user.ID)) {
		t.Fatal("conflicting username mutated profile")
	}
}

func TestUpdateCurrentUserAuthentication(t *testing.T) {
	handler, router, _ := newUserEmailTestHandler(t, nil)
	user := createProfileTestUser(t, handler, "alice")
	key, errKey := handler.userJWTSigningKey(context.Background())
	if errKey != nil {
		t.Fatal(errKey)
	}
	expired, errToken := signUserJWT(key, userJWTClaims{
		UserID: user.ID, SessionVersion: user.SessionVersion, TokenType: userJWTClaimType,
		IssuedAt: time.Now().Add(-2 * time.Hour).Unix(), ExpiresAt: time.Now().Add(-time.Hour).Unix(),
	})
	if errToken != nil {
		t.Fatal(errToken)
	}
	for _, token := range []string{"", "invalid", expired, createUserTestBearerToken(t, handler, user.ID, user.SessionVersion+1)} {
		before := profileTestSnapshot(t, handler, user.ID)
		response := performUserJSONRequest(t, router, http.MethodPatch, "/user/me", map[string]string{"username": "changed"}, token)
		if response.Code != http.StatusUnauthorized || !reflect.DeepEqual(before, profileTestSnapshot(t, handler, user.ID)) {
			t.Fatalf("authentication = %d %s", response.Code, response.Body.String())
		}
	}
	pending := true
	if _, errUpdate := handler.repo.UpdateUser(context.Background(), user.ID, cluster.UserUpdate{ApprovalPending: &pending}); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	before := profileTestSnapshot(t, handler, user.ID)
	token := createUserTestBearerToken(t, handler, user.ID, user.SessionVersion)
	response := performUserJSONRequest(t, router, http.MethodPatch, "/user/me", map[string]string{"username": "changed"}, token)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"error":"approval_pending"`) || !reflect.DeepEqual(before, profileTestSnapshot(t, handler, user.ID)) {
		t.Fatalf("pending authentication = %d %s", response.Code, response.Body.String())
	}
}
