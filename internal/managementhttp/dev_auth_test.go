package managementhttp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	cpasdkapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/api"
	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"golang.org/x/crypto/bcrypt"
)

func TestManagementDevIPBan(t *testing.T) {
	hash, errHash := bcrypt.GenerateFromPassword([]byte("test-key"), bcrypt.MinCost)
	if errHash != nil {
		t.Fatal(errHash)
	}
	for _, disabled := range []bool{false, true} {
		name := "production"
		if disabled {
			name = "development"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("MANAGEMENT_PASSWORD", "")
			t.Setenv("HOME_DEV_DISABLE_MANAGEMENT_IP_BAN", "false")
			if disabled {
				t.Setenv("HOME_DEV_DISABLE_MANAGEMENT_IP_BAN", "true")
			}
			cfg := &cpaconfig.Config{}
			cfg.RemoteManagement.SecretKey = string(hash)
			cfg.RemoteManagement.AllowRemote = true
			handler := cpasdkapi.NewHandlerWithoutConfigFilePath(cfg, nil)
			engine := gin.New()
			engine.Use(func(c *gin.Context) { c.Set("management.auth-config", cfg) })
			engine.GET("/", managementAuthMiddleware(handler), func(c *gin.Context) { c.Status(http.StatusOK) })
			request := func(key string) int {
				t.Helper()
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req.RemoteAddr = "192.0.2.1:1234"
				req.Header.Set("X-Management-Key", key)
				response := httptest.NewRecorder()
				engine.ServeHTTP(response, req)
				return response.Code
			}
			for i := 0; i < 7; i++ {
				status := request("wrong-key")
				if disabled && status != http.StatusUnauthorized {
					t.Fatalf("invalid key attempt %d: status = %d", i, status)
				}
			}
			want := http.StatusForbidden
			if disabled {
				want = http.StatusOK
			}
			if status := request("test-key"); status != want {
				t.Fatalf("valid key after failures: status = %d, want %d", status, want)
			}
			if disabled {
				cfg.RemoteManagement.AllowRemote = false
				if status := request("test-key"); status != http.StatusForbidden {
					t.Fatalf("remote access disabled: status = %d", status)
				}
			}
		})
	}
}
