package alerting

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func seedResolveState(t *testing.T, service *Service, key string, firing bool) {
	t.Helper()
	if errSave := service.Store().saveState(context.Background(), State{
		Key: key, Rule: RuleNodeOffline, Subject: "node/a", Title: "CPA node offline",
		Firing: firing, FirstSeenAt: time.Now().UTC(),
	}); errSave != nil {
		t.Fatalf("seed state: %v", errSave)
	}
}

func TestResolvePreservesTrackingAndOtherStatesAndCanRefire(t *testing.T) {
	ctx := context.Background()
	service, db := newTestService(t)
	key := RuleNodeOffline + ":node/a"
	node := NodeSeen{NodeID: "node/a", ClientIP: "10.0.0.1", LastSeenAt: dbNow(t, db).Add(-time.Hour)}
	if errCreate := db.Create(&node).Error; errCreate != nil {
		t.Fatalf("seed tracking: %v", errCreate)
	}
	seedResolveState(t, service, key, true)
	seedResolveState(t, service, "other", true)
	seedResolveState(t, service, "pending", false)

	if errResolve := service.Resolve(ctx, key); errResolve != nil {
		t.Fatalf("resolve: %v", errResolve)
	}
	states, errStates := service.Store().loadStates(ctx)
	if errStates != nil || len(states) != 2 || !states["other"].Firing || states["pending"].Key != "pending" {
		t.Fatalf("remaining states = %v, %v", states, errStates)
	}
	var tracked NodeSeen
	if errFind := db.Where("node_id = ?", node.NodeID).First(&tracked).Error; errFind != nil {
		t.Fatalf("tracking removed: %v", errFind)
	}
	if tracked.NodeID != node.NodeID || tracked.ClientIP != node.ClientIP || !tracked.LastSeenAt.Equal(node.LastSeenAt) {
		t.Fatalf("tracking changed: %+v", tracked)
	}
	if errResolve := service.Resolve(ctx, key); !errors.Is(errResolve, ErrAlertNotFound) {
		t.Fatalf("repeat resolve = %v, want ErrAlertNotFound", errResolve)
	}
	if errEval := service.Evaluate(ctx, DefaultSettings()); errEval != nil {
		t.Fatalf("evaluate: %v", errEval)
	}
	if !firingKeys(t, service)[key].Firing {
		t.Fatal("still-offline node must fire again on next evaluation")
	}
}

func TestResolveNotFound(t *testing.T) {
	service, _ := newTestService(t)
	seedResolveState(t, service, "pending", false)
	for _, key := range []string{"absent", "pending"} {
		if errResolve := service.Resolve(context.Background(), key); !errors.Is(errResolve, ErrAlertNotFound) {
			t.Errorf("resolve %q = %v, want ErrAlertNotFound", key, errResolve)
		}
	}
	states, errStates := service.Store().loadStates(context.Background())
	if errStates != nil || len(states) != 1 || states["pending"].Key != "pending" {
		t.Fatalf("nonfiring state changed: %v, %v", states, errStates)
	}
}

func TestResolveNotifications(t *testing.T) {
	for _, sendResolved := range []bool{true, false} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[sendResolved], func(t *testing.T) {
			ctx := context.Background()
			service, _ := newTestService(t)
			seedResolveState(t, service, "node_offline:node/a", true)
			settings := DefaultSettings()
			settings.SendResolved = sendResolved
			// Manual resolution does not depend on automatic evaluation being enabled.
			settings.Enabled = false
			if _, errSave := service.Store().SaveSettings(ctx, settings); errSave != nil {
				t.Fatalf("save settings: %v", errSave)
			}
			var mu sync.Mutex
			var messages []string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				body, errRead := io.ReadAll(req.Body)
				if errRead != nil {
					t.Errorf("read notification: %v", errRead)
				}
				states, errStates := service.Store().loadStates(ctx)
				if errStates != nil || len(states) != 0 {
					t.Errorf("notification sent before deletion: %v, %v", states, errStates)
				}
				mu.Lock()
				messages = append(messages, string(body))
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			originalClient := httpClient
			httpClient = server.Client()
			defer func() { httpClient = originalClient }()
			for _, enabled := range []bool{true, false} {
				if _, errSave := service.Store().SaveChannel(ctx, Channel{
					Name: "ops", Type: ChannelTeams, Enabled: enabled, WebhookURL: server.URL,
				}); errSave != nil {
					t.Fatalf("save channel: %v", errSave)
				}
			}
			if errResolve := service.Resolve(ctx, "node_offline:node/a"); errResolve != nil {
				t.Fatalf("resolve: %v", errResolve)
			}
			mu.Lock()
			defer mu.Unlock()
			if !sendResolved && len(messages) != 0 {
				t.Fatalf("unexpected notifications: %v", messages)
			}
			if sendResolved && (len(messages) != 1 || !strings.Contains(messages[0], "RESOLVED: CPA node offline") ||
				!strings.Contains(messages[0], "Manually resolved by an operator.")) {
				t.Fatalf("expected one manual resolved notification, got %v", messages)
			}
		})
	}
}

func TestResolveErrorsPreserveState(t *testing.T) {
	for _, failure := range []string{"settings", "channels", "delete"} {
		t.Run(failure, func(t *testing.T) {
			service, db := newTestService(t)
			seedResolveState(t, service, "target", true)
			var statement string
			switch failure {
			case "settings":
				statement = "DROP TABLE alert_settings"
			case "channels":
				statement = "DROP TABLE alert_channels"
			case "delete":
				statement = "CREATE TRIGGER fail_alert_delete BEFORE DELETE ON alert_state BEGIN SELECT RAISE(ABORT, 'delete failed'); END"
			}
			if errExec := db.Exec(statement).Error; errExec != nil {
				t.Fatalf("inject failure: %v", errExec)
			}
			if errResolve := service.Resolve(context.Background(), "target"); errResolve == nil || errors.Is(errResolve, ErrAlertNotFound) {
				t.Fatalf("resolve = %v, want storage error", errResolve)
			}
			if !firingKeys(t, service)["target"].Firing {
				t.Fatal("failed resolve removed firing state")
			}
		})
	}
}

func TestResolveAlertHandler(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"empty body", "", http.StatusBadRequest, "invalid_body"},
		{"malformed", "{", http.StatusBadRequest, "invalid_body"},
		{"wrong type", `{"key":42}`, http.StatusBadRequest, "invalid_body"},
		{"array", `[]`, http.StatusBadRequest, "invalid_body"},
		{"null", `null`, http.StatusBadRequest, "invalid_key"},
		{"missing key", `{}`, http.StatusBadRequest, "invalid_key"},
		{"empty key", `{"key":""}`, http.StatusBadRequest, "invalid_key"},
		{"blank key", `{"key":" \t "}`, http.StatusBadRequest, "invalid_key"},
		{"not found", `{"key":"missing"}`, http.StatusNotFound, "alert_not_found"},
		{"not firing", `{"key":"pending"}`, http.StatusNotFound, "alert_not_found"},
		{"slash key", `{"key":"node_offline:node/a"}`, http.StatusOK, "ok"},
		{"storage error", `{"key":"node_offline:node/a"}`, http.StatusInternalServerError, "alerting_alert_resolve_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, db := newTestService(t)
			seedResolveState(t, service, "node_offline:node/a", true)
			seedResolveState(t, service, "pending", false)
			if tc.status == http.StatusInternalServerError {
				if errExec := db.Exec("DROP TABLE alert_state").Error; errExec != nil {
					t.Fatalf("inject failure: %v", errExec)
				}
			}
			engine := gin.New()
			h := &handler{service: service}
			engine.POST("/v8/management/alerting/alerts/resolve", h.resolveAlert)
			response := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v8/management/alerting/alerts/resolve", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			engine.ServeHTTP(response, req)
			if response.Code != tc.status || !strings.Contains(response.Body.String(), `"`+tc.code+`"`) {
				t.Fatalf("response = %d %s, want %d %s", response.Code, response.Body.String(), tc.status, tc.code)
			}
			if tc.status == http.StatusOK {
				if _, ok := firingKeys(t, service)["node_offline:node/a"]; ok {
					t.Fatal("successful handler did not remove state")
				}
			}
		})
	}
}
