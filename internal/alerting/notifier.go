package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const notifyTimeout = 15 * time.Second

// Message is one notification sent to every enabled channel.
type Message struct {
	Rule     string
	Title    string
	Detail   string
	Resolved bool
	Time     time.Time
}

func (m Message) heading() string {
	if m.Resolved {
		return "✅ RESOLVED: " + m.Title
	}
	return "🚨 " + m.Title
}

var httpClient = &http.Client{Timeout: notifyTimeout}

// Send delivers a message through one channel.
func Send(ctx context.Context, channel Channel, message Message) error {
	switch channel.Type {
	case ChannelTelegram:
		return sendTelegram(ctx, channel, message)
	case ChannelTeams:
		return sendTeams(ctx, channel, message)
	default:
		return fmt.Errorf("unsupported channel type %q", channel.Type)
	}
}

func sendTelegram(ctx context.Context, channel Channel, message Message) error {
	text := fmt.Sprintf("<b>%s</b>\n%s\n<i>%s · %s</i>",
		htmlEscape(message.heading()), htmlEscape(message.Detail), htmlEscape(message.Rule), message.Time.UTC().Format(time.RFC3339))
	payload := map[string]any{
		"chat_id":                  channel.ChatID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	if channel.ThreadID != "" {
		payload["message_thread_id"] = channel.ThreadID
	}
	endpoint := "https://api.telegram.org/bot" + channel.BotToken + "/sendMessage"
	// The endpoint embeds the bot token, so errors must never include it.
	return postJSON(ctx, endpoint, payload, "telegram")
}

func sendTeams(ctx context.Context, channel Channel, message Message) error {
	color := "Attention"
	if message.Resolved {
		color = "Good"
	}
	card := map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": "1.4",
		"body": []any{
			map[string]any{"type": "TextBlock", "text": message.heading(), "weight": "Bolder", "size": "Medium", "color": color, "wrap": true},
			map[string]any{"type": "TextBlock", "text": message.Detail, "wrap": true},
			map[string]any{"type": "TextBlock", "text": message.Rule + " · " + message.Time.UTC().Format(time.RFC3339), "isSubtle": true, "size": "Small", "wrap": true},
		},
	}
	payload := map[string]any{
		"type": "message",
		"attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"content":     card,
		}},
	}
	return postJSON(ctx, channel.WebhookURL, payload, "teams")
}

func postJSON(ctx context.Context, endpoint string, payload any, kind string) error {
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return fmt.Errorf("%s: encode payload: %w", kind, errMarshal)
	}
	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errReq != nil {
		return fmt.Errorf("%s: build request failed", kind)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		// Do not wrap errDo: url.Error contains the full endpoint, including secrets.
		return fmt.Errorf("%s: request failed", kind)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithField("channel_type", kind).WithError(errClose).Debug("alerting: close response body failed")
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: unexpected status %d: %s", kind, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

func htmlEscape(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(value)
}
