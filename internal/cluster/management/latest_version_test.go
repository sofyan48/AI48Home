package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestGetLatestVersionResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name          string
		upstreamCode  int
		upstreamBody  string
		wantCode      int
		wantVersion   string
		wantPublished any
	}{
		{name: "published", upstreamCode: http.StatusOK, upstreamBody: `{"tag_name":"v0.1.2"}`, wantCode: http.StatusOK, wantVersion: "v0.1.2", wantPublished: true},
		{name: "no release yet", upstreamCode: http.StatusNotFound, upstreamBody: `{"message":"Not Found"}`, wantCode: http.StatusOK, wantVersion: "", wantPublished: false},
		{name: "upstream failure", upstreamCode: http.StatusInternalServerError, upstreamBody: `boom`, wantCode: http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.upstreamCode)
				_, _ = w.Write([]byte(tc.upstreamBody))
			}))
			defer upstream.Close()
			previousURL := latestReleaseURL
			latestReleaseURL = upstream.URL
			defer func() { latestReleaseURL = previousURL }()

			db, cleanup := openManagementLogTestDB(t)
			defer cleanup()
			handler := NewHandler(cluster.NewRepository(db), nil, "127.0.0.1", 0)
			engine := gin.New()
			engine.GET("/server/latest-version", handler.GetLatestVersion)

			resp := httptest.NewRecorder()
			engine.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/server/latest-version", nil))
			if resp.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d body=%s", resp.Code, tc.wantCode, resp.Body.String())
			}
			if tc.wantCode != http.StatusOK {
				return
			}
			var body map[string]any
			if errDecode := json.Unmarshal(resp.Body.Bytes(), &body); errDecode != nil {
				t.Fatalf("decode body: %v", errDecode)
			}
			if body["latest-version"] != tc.wantVersion || body["release-published"] != tc.wantPublished {
				t.Fatalf("body = %v, want latest-version=%q release-published=%v", body, tc.wantVersion, tc.wantPublished)
			}
		})
	}
}
