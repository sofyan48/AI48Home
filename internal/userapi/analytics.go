package userapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

type userAnalyticsResponse struct {
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Timezone string    `json:"timezone"`
	Source   string    `json:"source"`
	Model    string    `json:"model"`
	APIKeyID *uint     `json:"api_key_id"`
	cluster.UserAnalyticsResult
}

func (h *Handler) CurrentUserAnalytics(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	user, ok := h.authenticatedUser(c, ctx, authFields{})
	if !ok {
		return
	}
	query, errQuery := userAnalyticsQuery(c, time.Now())
	if errQuery != nil {
		respondError(c, http.StatusBadRequest, "invalid_analytics_query", errQuery)
		return
	}
	result, errAnalytics := h.repo.UserAnalytics(ctx, user.ID, query)
	if errors.Is(errAnalytics, cluster.ErrUserAnalyticsKey) {
		respondError(c, http.StatusNotFound, "analytics_key_not_found", fmt.Errorf("API key not found"))
		return
	}
	if errAnalytics != nil {
		respondError(c, http.StatusInternalServerError, "analytics_load_failed", errAnalytics)
		return
	}
	c.JSON(http.StatusOK, userAnalyticsResponse{
		From: query.From, To: query.To, Timezone: "UTC", Source: "billing_charge",
		Model: query.Model, APIKeyID: query.APIKeyID, UserAnalyticsResult: result,
	})
}

func userAnalyticsQuery(c *gin.Context, now time.Time) (cluster.UserAnalyticsQuery, error) {
	now = now.UTC()
	to := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	query := cluster.UserAnalyticsQuery{To: to}
	values := c.Request.URL.Query()
	for _, key := range []string{"from", "to", "model", "api_key_id"} {
		if entries, exists := values[key]; exists && (len(entries) != 1 || strings.TrimSpace(entries[0]) == "") {
			return query, fmt.Errorf("%s must have exactly one non-empty value", key)
		}
	}
	for _, key := range []string{"to", "from"} {
		if raw, exists := values[key]; exists {
			parsed, errParse := time.Parse(time.DateOnly, raw[0])
			if errParse != nil {
				parsed, errParse = time.Parse(time.RFC3339Nano, raw[0])
			}
			if errParse != nil {
				return query, fmt.Errorf("%s must be YYYY-MM-DD or RFC3339", key)
			}
			if key == "to" {
				query.To = parsed.UTC()
			} else {
				query.From = parsed.UTC()
			}
		}
	}
	if _, exists := values["from"]; !exists {
		query.From = query.To.AddDate(0, 0, -30)
	}
	if !query.From.Before(query.To) || query.To.Sub(query.From) > cluster.UserAnalyticsMaxRange {
		return query, fmt.Errorf("range must be positive and at most 366 days; to is exclusive")
	}
	query.Model = strings.TrimSpace(c.Query("model"))
	if len(query.Model) > 512 {
		return query, fmt.Errorf("model must not exceed 512 bytes")
	}
	if raw, exists := values["api_key_id"]; exists {
		id, errParse := strconv.ParseUint(raw[0], 10, strconv.IntSize)
		if errParse != nil || id == 0 {
			return query, fmt.Errorf("api_key_id must be a positive integer")
		}
		value := uint(id)
		query.APIKeyID = &value
	}
	return query, nil
}
