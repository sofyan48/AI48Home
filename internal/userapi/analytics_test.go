package userapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestUserAnalyticsAuthenticationAndFilters(t *testing.T) {
	t.Parallel()
	h, closeRepo := newUserBillingTestHandler(t)
	defer closeRepo()
	first, second := seedUserBillingCharges(t, h)
	token := createUserBillingBearerToken(t, h, first.ID)
	keys, errKeys := h.repo.ListAPIKeyRecordsForUser(t.Context(), second.ID)
	if errKeys != nil || len(keys) == 0 {
		t.Fatalf("keys=%v err=%v", keys, errKeys)
	}
	router := gin.New()
	Register(router.Group("/user"), h)
	base := "?from=2026-06-10&to=2026-06-11"
	for _, test := range []struct {
		token    string
		query    string
		status   int
		requests int64
	}{
		{"", "", http.StatusUnauthorized, 0},
		{token, base, http.StatusOK, 1},
		{token, base + fmt.Sprintf("&user_id=%d&api_key=second-client-key", second.ID), http.StatusOK, 1},
		{token, base + fmt.Sprintf("&api_key_id=%d", keys[0].ID), http.StatusNotFound, 0},
		{token, base + "&api_key_id=999999999", http.StatusNotFound, 0},
		{token, base + "&api_key_id=0", http.StatusBadRequest, 0},
		{token, base + "&api_key_id=-1", http.StatusBadRequest, 0},
		{token, base + "&model=missing", http.StatusOK, 0},
		{token, "?from=2025-01-01&to=2026-06-11", http.StatusBadRequest, 0},
	} {
		resp := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/user/analytics"+test.query, nil)
		if test.token != "" {
			req.Header.Set("Authorization", "Bearer "+test.token)
		}
		router.ServeHTTP(resp, req)
		if resp.Code != test.status {
			t.Fatalf("%s status=%d body=%s", test.query, resp.Code, resp.Body.String())
		}
		if test.status != http.StatusOK {
			continue
		}
		var payload userAnalyticsResponse
		decoder := json.NewDecoder(resp.Body)
		decoder.DisallowUnknownFields()
		if errDecode := decoder.Decode(&payload); errDecode != nil {
			t.Fatal(errDecode)
		}
		if payload.Summary.Requests != test.requests || payload.Source != "billing_charge" || len(payload.Daily) != 1 || payload.PerKey == nil || payload.PerModel == nil {
			t.Fatalf("payload=%+v", payload)
		}
		if test.requests > 0 && (payload.Summary.BilledAmount != 1 || payload.Summary.InputTokens == 0) {
			t.Fatalf("metrics=%+v", payload.Summary)
		}
	}
}

func TestUserAnalyticsDateRange(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 10, 15, 30, 0, 0, time.UTC)
	for _, test := range []struct {
		query string
		valid bool
	}{
		{"", true},
		{"?to=2026-06-11", true},
		{"?from=2026-06-10&to=2026-06-11", true},
		{"?from=2025-06-10&to=2026-06-11", true},
		{"?from=2025-06-09&to=2026-06-11", false},
		{"?from=2026-06-11&to=2026-06-11", false},
		{"?from=2026-06-12&to=2026-06-11", false},
		{"?from=", false}, {"?to=", false}, {"?model=", false},
		{"?from=2026-06-10&from=2026-06-11", false},
		{"?from=garbage", false}, {"?to=1781049600", false},
		{"?from=2026-06-10T08:00:00%2B08:00&to=2026-06-11T08:00:00%2B08:00", true},
		{"?model=" + strings.Repeat("x", 513), false},
	} {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodGet, "/user/analytics"+test.query, nil)
		query, errQuery := userAnalyticsQuery(ctx, now, "UTC")
		if (errQuery == nil) != test.valid {
			t.Fatalf("%s query=%+v err=%v", test.query, query, errQuery)
		}
		if !test.valid {
			continue
		}
		if query.From.Location() != time.UTC || query.To.Location() != time.UTC {
			t.Fatalf("not UTC: %+v", query)
		}
		if test.query == "" && (query.To.Format(time.RFC3339) != "2026-06-11T00:00:00Z" || query.To.Sub(query.From) != 30*24*time.Hour) {
			t.Fatalf("defaults=%+v", query)
		}
		if strings.Contains(test.query, "%2B08") && query.From.Format(time.RFC3339) != "2026-06-10T00:00:00Z" {
			t.Fatalf("offset normalization=%+v", query)
		}
	}
}
