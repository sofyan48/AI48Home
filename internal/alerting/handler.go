package alerting

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/managementhttp"
)

// RouteOption registers the alerting endpoints under /v8/management/alerting.
// Routes inherit the existing Management API authentication middleware.
func RouteOption(service *Service) managementhttp.RouteOption {
	return func(r *managementhttp.RouteRegistry) {
		if r == nil || service == nil {
			return
		}
		h := &handler{service: service}
		r.Set(http.MethodGet, "/alerting/settings", h.getSettings)
		r.Set(http.MethodPut, "/alerting/settings", h.putSettings)
		r.Set(http.MethodGet, "/alerting/channels", h.listChannels)
		r.Set(http.MethodPost, "/alerting/channels", h.createChannel)
		r.Set(http.MethodPatch, "/alerting/channels/:id", h.updateChannel)
		r.Set(http.MethodDelete, "/alerting/channels/:id", h.deleteChannel)
		r.Set(http.MethodPost, "/alerting/channels/:id/test", h.testChannel)
		r.Set(http.MethodGet, "/alerting/alerts", h.listAlerts)
		r.Set(http.MethodPost, "/alerting/evaluate", h.evaluate)
		r.Set(http.MethodDelete, "/alerting/nodes/:node_id", h.forgetNode)
	}
}

type handler struct {
	service *Service
}

func requestContext(c *gin.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request.Context(), 30*time.Second)
}

func respondError(c *gin.Context, status int, code string, err error) {
	message := code
	if err != nil {
		message = err.Error()
	}
	c.JSON(status, gin.H{"error": code, "message": message})
}

func (h *handler) getSettings(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	settings, errGet := h.service.Store().GetSettings(ctx)
	if errGet != nil {
		respondError(c, http.StatusInternalServerError, "alerting_settings_load_failed", errGet)
		return
	}
	c.JSON(http.StatusOK, gin.H{"settings": settings})
}

func (h *handler) putSettings(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	// Start from the stored settings so partial bodies keep omitted fields.
	settings, errGet := h.service.Store().GetSettings(ctx)
	if errGet != nil {
		respondError(c, http.StatusInternalServerError, "alerting_settings_load_failed", errGet)
		return
	}
	if errBind := c.ShouldBindJSON(&settings); errBind != nil {
		respondError(c, http.StatusBadRequest, "invalid_body", errBind)
		return
	}
	saved, errSave := h.service.Store().SaveSettings(ctx, settings)
	if errSave != nil {
		respondError(c, http.StatusBadRequest, "invalid_settings", errSave)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "settings": saved})
}

type channelRequest struct {
	Name       *string `json:"name"`
	Type       *string `json:"type"`
	Enabled    *bool   `json:"enabled"`
	BotToken   *string `json:"bot_token"`
	ChatID     *string `json:"chat_id"`
	ThreadID   *string `json:"thread_id"`
	WebhookURL *string `json:"webhook_url"`
}

func (req channelRequest) apply(channel *Channel) {
	if req.Name != nil {
		channel.Name = *req.Name
	}
	if req.Type != nil {
		channel.Type = *req.Type
	}
	if req.Enabled != nil {
		channel.Enabled = *req.Enabled
	}
	// Empty secret fields on update keep the stored value, because responses only
	// return masked secrets.
	if req.BotToken != nil && strings.TrimSpace(*req.BotToken) != "" {
		channel.BotToken = *req.BotToken
	}
	if req.ChatID != nil {
		channel.ChatID = *req.ChatID
	}
	if req.ThreadID != nil {
		channel.ThreadID = *req.ThreadID
	}
	if req.WebhookURL != nil && strings.TrimSpace(*req.WebhookURL) != "" {
		channel.WebhookURL = *req.WebhookURL
	}
}

func channelResponse(channel Channel) gin.H {
	return gin.H{
		"id":              channel.ID,
		"name":            channel.Name,
		"type":            channel.Type,
		"enabled":         channel.Enabled,
		"chat_id":         channel.ChatID,
		"thread_id":       channel.ThreadID,
		"bot_token_set":   channel.BotToken != "",
		"bot_token_hint":  maskSecret(channel.BotToken),
		"webhook_url_set": channel.WebhookURL != "",
		"webhook_url_hint": func() string {
			if channel.WebhookURL == "" {
				return ""
			}
			return maskURL(channel.WebhookURL)
		}(),
		"created_at": channel.CreatedAt,
		"updated_at": channel.UpdatedAt,
	}
}

func maskSecret(value string) string {
	if len(value) <= 4 {
		if value == "" {
			return ""
		}
		return "****"
	}
	return "****" + value[len(value)-4:]
}

// maskURL keeps only the host so the webhook signature is never returned.
func maskURL(raw string) string {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	host, _, _ := strings.Cut(trimmed, "/")
	return "https://" + host + "/****"
}

func (h *handler) listChannels(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	channels, errList := h.service.Store().ListChannels(ctx)
	if errList != nil {
		respondError(c, http.StatusInternalServerError, "alerting_channels_load_failed", errList)
		return
	}
	items := make([]gin.H, 0, len(channels))
	for _, channel := range channels {
		items = append(items, channelResponse(channel))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *handler) createChannel(c *gin.Context) {
	var req channelRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil {
		respondError(c, http.StatusBadRequest, "invalid_body", errBind)
		return
	}
	channel := Channel{Enabled: true}
	req.apply(&channel)
	ctx, cancel := requestContext(c)
	defer cancel()
	saved, errSave := h.service.Store().SaveChannel(ctx, channel)
	if errSave != nil {
		respondError(c, http.StatusBadRequest, "invalid_channel", errSave)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "channel": channelResponse(saved)})
}

func (h *handler) loadChannel(c *gin.Context, ctx context.Context) (Channel, bool) {
	id, errParse := strconv.ParseUint(c.Param("id"), 10, 64)
	if errParse != nil || id == 0 {
		respondError(c, http.StatusBadRequest, "invalid_id", nil)
		return Channel{}, false
	}
	channel, errGet := h.service.Store().GetChannel(ctx, uint(id))
	if errors.Is(errGet, ErrChannelNotFound) {
		respondError(c, http.StatusNotFound, "channel_not_found", errGet)
		return Channel{}, false
	}
	if errGet != nil {
		respondError(c, http.StatusInternalServerError, "alerting_channel_load_failed", errGet)
		return Channel{}, false
	}
	return channel, true
}

func (h *handler) updateChannel(c *gin.Context) {
	var req channelRequest
	if errBind := c.ShouldBindJSON(&req); errBind != nil {
		respondError(c, http.StatusBadRequest, "invalid_body", errBind)
		return
	}
	ctx, cancel := requestContext(c)
	defer cancel()
	channel, ok := h.loadChannel(c, ctx)
	if !ok {
		return
	}
	req.apply(&channel)
	saved, errSave := h.service.Store().SaveChannel(ctx, channel)
	if errSave != nil {
		respondError(c, http.StatusBadRequest, "invalid_channel", errSave)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "channel": channelResponse(saved)})
}

func (h *handler) deleteChannel(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	channel, ok := h.loadChannel(c, ctx)
	if !ok {
		return
	}
	if errDelete := h.service.Store().DeleteChannel(ctx, channel.ID); errDelete != nil {
		respondError(c, http.StatusInternalServerError, "alerting_channel_delete_failed", errDelete)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *handler) testChannel(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	channel, ok := h.loadChannel(c, ctx)
	if !ok {
		return
	}
	message := Message{
		Rule:   "test",
		Title:  "Test notification from AI48Home",
		Detail: "Alerting channel \"" + channel.Name + "\" is configured correctly.",
		Time:   time.Now().UTC(),
	}
	if errSend := Send(ctx, channel, message); errSend != nil {
		respondError(c, http.StatusBadGateway, "alerting_test_failed", errSend)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *handler) listAlerts(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	firingOnly := c.Query("firing") != "false"
	states, errList := h.service.Store().ListStates(ctx, firingOnly)
	if errList != nil {
		respondError(c, http.StatusInternalServerError, "alerting_alerts_load_failed", errList)
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": states})
}

func (h *handler) evaluate(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	settings, errGet := h.service.Store().GetSettings(ctx)
	if errGet != nil {
		respondError(c, http.StatusInternalServerError, "alerting_settings_load_failed", errGet)
		return
	}
	if errEval := h.service.Evaluate(ctx, settings); errEval != nil {
		respondError(c, http.StatusInternalServerError, "alerting_evaluate_failed", errEval)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// forgetNode stops tracking a decommissioned CPA node and clears its alert
// without sending a resolved notification.
func (h *handler) forgetNode(c *gin.Context) {
	ctx, cancel := requestContext(c)
	defer cancel()
	nodeID := strings.TrimSpace(c.Param("node_id"))
	if nodeID == "" {
		respondError(c, http.StatusBadRequest, "invalid_node_id", nil)
		return
	}
	if errForget := h.service.Store().ForgetNode(ctx, nodeID); errForget != nil {
		respondError(c, http.StatusInternalServerError, "alerting_node_forget_failed", errForget)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
