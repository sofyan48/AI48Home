# Alerting (Telegram / Microsoft Teams)

AI48Home-specific extension implemented in `internal/alerting/`. This document contains the detailed alerting contract linked from `api.md`.

Home evaluates alert rules every `interval_seconds` by reading existing tables (`auth`, `quota_window`, `cpa_node_membership`, `cpa_node`). Notifications are sent only on state transitions (firing, and resolved when `send_resolved` is true). When several Homes share a PostgreSQL database, a lease in `alert_lease` ensures only one Home evaluates and sends.

All routes are under `/v8/management` and use the normal Management API authentication.

## Rules

| Rule | Fires when | Setting |
|---|---|---|
| `credential_dead` | Credential was disabled by Home after an unauthorized upstream response (`status=disabled`, `status_message=unauthorized`). Operator-disabled credentials are ignored. | `credential_dead_enabled` |
| `credential_refresh` | Credential has `last_refresh_error` for at least `refresh_failure_minutes`. | `credential_refresh_enabled`, `refresh_failure_minutes` |
| `credential_cooldown` | `next_retry_after` is at least `cooldown_minutes` in the future, or the cooldown has been observed for `cooldown_minutes`. | `credential_cooldown_enabled`, `cooldown_minutes` |
| `quota_low` | A non-unlimited, unexpired quota window has `remaining_ratio <= quota_threshold_percent / 100`. One alert per credential window. | `quota_low_enabled`, `quota_threshold_percent` |
| `node_offline` | A Worker has not sent a subscription heartbeat for `node_offline_seconds`. Membership `last_seen_at` is authoritative when available; connection snapshot timestamps are a fallback for nodes without membership records. Active memberships are tracked even without a connection snapshot. Last-seen history in `alert_nodes` never moves backward and survives snapshot or membership removal. Closed memberships alone do not rediscover forgotten Workers. Alert titles use `Worker offline:`; rule keys and API fields remain unchanged. | `node_offline_enabled`, `node_offline_seconds` |

Disabling a rule keeps its current alerts without sending resolved messages.

## Tables

`alert_settings`, `alert_channels`, `alert_state`, `alert_nodes`, `alert_lease`. They are created on startup by the alerting service.

## Settings

### `GET /alerting/settings`

```json
{
  "settings": {
    "enabled": true,
    "interval_seconds": 60,
    "repeat_minutes": 0,
    "send_resolved": true,
    "credential_dead_enabled": true,
    "credential_refresh_enabled": true,
    "refresh_failure_minutes": 5,
    "credential_cooldown_enabled": true,
    "cooldown_minutes": 30,
    "quota_low_enabled": true,
    "quota_threshold_percent": 10,
    "node_offline_enabled": true,
    "node_offline_seconds": 120,
    "updated_at": "0001-01-01T00:00:00Z"
  }
}
```

### `PUT /alerting/settings`

Body: any subset of the settings fields; omitted fields keep their stored values. `repeat_minutes: 0` disables reminders for alerts that stay firing.

Validation: `interval_seconds` 15–3600, `cooldown_minutes >= 1`, `0 < quota_threshold_percent < 100`, `node_offline_seconds >= 10`, other durations `>= 0`. Errors return `400 {"error":"invalid_settings","message":"..."}`.

## Channels

### `GET /alerting/channels`

```json
{
  "items": [
    {
      "id": 1,
      "name": "ops-telegram",
      "type": "telegram",
      "enabled": true,
      "chat_id": "-1001234567890",
      "thread_id": "",
      "bot_token_set": true,
      "bot_token_hint": "****abcd",
      "webhook_url_set": false,
      "webhook_url_hint": "",
      "created_at": "2026-11-17T10:00:00Z",
      "updated_at": "2026-11-17T10:00:00Z"
    }
  ]
}
```

Secrets (`bot_token`, `webhook_url`) are never returned; only masked hints.

### `POST /alerting/channels`

```json
{ "name": "ops-telegram", "type": "telegram", "bot_token": "123456:ABC...", "chat_id": "-1001234567890", "thread_id": "" }
```

```json
{ "name": "ops-teams", "type": "teams", "webhook_url": "https://<tenant>.webhook.office.com/..." }
```

- `type`: `telegram` or `teams`.
- Telegram requires `bot_token` and `chat_id`; `thread_id` is optional (forum topics).
- Teams requires an `https` `webhook_url` (Workflows "Post to a channel when a webhook request is received", or a legacy Incoming Webhook). The payload is an Adaptive Card message.
- `enabled` defaults to `true`.

Response: `{"status":"ok","channel":{...}}`.

### `PATCH /alerting/channels/:id`

Same fields as create, all optional. Empty `bot_token` / `webhook_url` keep the stored secret.

### `DELETE /alerting/channels/:id`

Response: `{"status":"ok"}`; `404 channel_not_found` when missing.

### `POST /alerting/channels/:id/test`

Sends a test notification. Returns `502 alerting_test_failed` with the upstream status when delivery fails.

## Alerts

### `GET /alerting/alerts`

Query: `firing=false` also includes alerts still in their hold period. Default returns only firing alerts.

```json
{
  "items": [
    {
      "key": "quota_low:cred-1:weekly",
      "rule": "quota_low",
      "subject": "cred-1",
      "title": "Upstream quota low: my-account (codex)",
      "detail": "Window \"Weekly\" has 7.0% remaining. Resets at 2026-11-20T00:00:00Z.",
      "firing": true,
      "first_seen_at": "2026-11-17T10:00:00Z",
      "last_notified_at": "2026-11-17T10:00:00Z",
      "updated_at": "2026-11-17T10:05:00Z"
    }
  ]
}
```

### `POST /alerting/alerts/resolve`

Manually resolves one currently firing alert. Send the exact `key` returned by the alert list in a JSON body, not a URL path segment, so keys containing `/` are supported:

```json
{ "key": "node_offline:node/a" }
```

Response: `200 {"status":"ok"}`. Invalid JSON or a non-string key returns `400 invalid_body`; a missing, null, empty, or whitespace-only key returns `400 invalid_key`. Absent or nonfiring alerts return `404 alert_not_found`. Database/configuration errors return `500 alerting_alert_resolve_failed`.

This is a **one-shot reset**, not acknowledgment or persistent suppression: it deletes only the selected firing state. A still-active condition can fire again on the next evaluation (subject to the rule's normal hold period). Other alerts and Worker tracking are unchanged; use `DELETE /alerting/nodes/:node_id` separately to forget a decommissioned node.

When `send_resolved` is true, enabled channels receive a resolved notification with detail `Manually resolved by an operator.` only after successful deletion. When false, no notification is sent. Manual resolution works even when automatic evaluation or the rule is disabled. Notification delivery failures are logged, as for evaluation notifications; they do not restore the state or fail the successful reset.

Resolution and evaluation share this Home service's mutex. This does not introduce cross-Home serialization or change the existing evaluator lease; another Home can still evaluate concurrently.

### `POST /alerting/evaluate`

Runs one evaluation immediately on this Home (ignores the lease). Response: `{"status":"ok"}`.

### `DELETE /alerting/nodes/:node_id`

Stops tracking a decommissioned Worker and clears its `node_offline` alert without a resolved message.
