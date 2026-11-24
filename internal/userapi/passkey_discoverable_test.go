package userapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestParseWebAuthnUserID(t *testing.T) {
	id, errParse := parseWebAuthnUserID(webAuthnUserID(42))
	if errParse != nil || id != 42 {
		t.Fatalf("round trip = %d, %v", id, errParse)
	}
	for _, handle := range []string{"", "cpah-user-", "cpah-user-0", "cpah-user-abc", "other-42"} {
		if _, errInvalid := parseWebAuthnUserID([]byte(handle)); errInvalid == nil {
			t.Fatalf("handle %q should be rejected", handle)
		}
	}
}

func TestDiscoverablePasskeyLogin(t *testing.T) {
	_, router, _ := newUserEmailTestHandler(t, nil)
	begin := performUserJSONRequest(t, router, http.MethodPost, "/user/login/passkey/begin", map[string]any{}, "")
	if begin.Code != http.StatusOK {
		t.Fatalf("begin = %d %s", begin.Code, begin.Body.String())
	}
	var body struct {
		ChallengeID string         `json:"challenge_id"`
		PublicKey   map[string]any `json:"publicKey"`
	}
	if errDecode := json.Unmarshal(begin.Body.Bytes(), &body); errDecode != nil {
		t.Fatal(errDecode)
	}
	if body.ChallengeID == "" || body.PublicKey["challenge"] == nil {
		t.Fatalf("begin body = %s", begin.Body.String())
	}
	if allow, ok := body.PublicKey["allowCredentials"].([]any); ok && len(allow) > 0 {
		t.Fatalf("discoverable login must not expose allowCredentials: %v", allow)
	}

	invalid := performUserJSONRequest(t, router, http.MethodPost, "/user/login/passkey", map[string]any{"challenge_id": body.ChallengeID, "credential": map[string]any{"id": "credential"}}, "")
	if invalid.Code != http.StatusUnauthorized {
		t.Fatalf("invalid assertion = %d %s", invalid.Code, invalid.Body.String())
	}
	missing := performUserJSONRequest(t, router, http.MethodPost, "/user/login/passkey", map[string]any{"credential": map[string]any{"id": "credential"}}, "")
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("missing challenge = %d %s", missing.Code, missing.Body.String())
	}
}
