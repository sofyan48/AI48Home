package managementhttp

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	cpasdkapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/api"
	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
)

// managementAuthMiddleware keeps the upstream ban policy unless explicitly disabled for development.
func managementAuthMiddleware(handler *cpasdkapi.Handler) gin.HandlerFunc {
	if os.Getenv("HOME_DEV_DISABLE_MANAGEMENT_IP_BAN") != "true" {
		return handler.Middleware()
	}
	log.Warn("development management IP ban is disabled; do not enable this option in production")
	return managementAuthWithoutBan(strings.TrimSpace(os.Getenv("MANAGEMENT_PASSWORD")))
}

func managementAuthWithoutBan(envSecret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		value, _ := c.Get("management.auth-config")
		cfg, _ := value.(*cpaconfig.Config)
		allowRemote := envSecret != ""
		secretHash := ""
		if cfg != nil {
			allowRemote = allowRemote || cfg.RemoteManagement.AllowRemote
			secretHash = cfg.RemoteManagement.SecretKey
		}
		clientIP := c.ClientIP()
		if clientIP != "127.0.0.1" && clientIP != "::1" && !allowRemote {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "remote management disabled"})
			return
		}
		if secretHash == "" && envSecret == "" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "remote management key not set"})
			return
		}
		provided := c.GetHeader("Authorization")
		if parts := strings.SplitN(provided, " ", 2); len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
			provided = parts[1]
		}
		if provided == "" {
			provided = c.GetHeader("X-Management-Key")
		}
		if provided == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing management key"})
			return
		}
		if envSecret != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(envSecret)) == 1 {
			c.Next()
			return
		}
		if secretHash == "" || bcrypt.CompareHashAndPassword([]byte(secretHash), []byte(provided)) != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid management key"})
			return
		}
		c.Next()
	}
}
