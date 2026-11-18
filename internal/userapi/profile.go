package userapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

// UpdateCurrentUser edits only the authenticated user's public profile fields.
func (h *Handler) UpdateCurrentUser(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	user, ok := h.authenticatedUser(c, ctx, authFields{})
	if !ok {
		return
	}

	var fields map[string]json.RawMessage
	if !decodeJSONBody(c, &fields) {
		return
	}
	if len(fields) == 0 {
		respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("username or timezone is required"))
		return
	}
	update := cluster.UserUpdate{}
	for field, raw := range fields {
		if field != "username" && field != "timezone" {
			respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("unknown profile field %q", field))
			return
		}
		var value string
		if errUnmarshal := json.Unmarshal(raw, &value); errUnmarshal != nil || strings.TrimSpace(value) == "" {
			respondError(c, http.StatusBadRequest, "invalid_body", fmt.Errorf("%s must be a non-empty string", field))
			return
		}
		value = strings.TrimSpace(value)
		if field == "username" {
			update.Username = &value
		} else {
			update.Timezone = &value
		}
	}

	updated, errUpdate := h.repo.UpdateUser(ctx, user.ID, update)
	if errUpdate != nil {
		var configError cluster.PeriodLimitConfigError
		switch {
		case cluster.IsUserConflictError(errUpdate):
			respondError(c, http.StatusConflict, "user_exists", fmt.Errorf("user already exists"))
		case errors.As(errUpdate, &configError):
			respondError(c, http.StatusBadRequest, configError.Code, configError)
		default:
			respondUserError(c, "profile_update_failed", errUpdate)
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": h.userResponseWithPasskeys(updated), "status": "ok"})
}
