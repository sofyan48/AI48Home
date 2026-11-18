// Package alerting watches Home tables (credentials, quota windows, CPA nodes)
// and notifies Telegram or Microsoft Teams channels. It only reads existing
// tables and owns its own alert_* tables so upstream merges stay conflict-free.
package alerting

import "time"

const (
	ChannelTelegram = "telegram"
	ChannelTeams    = "teams"
)

const (
	RuleCredentialDead     = "credential_dead"
	RuleCredentialRefresh  = "credential_refresh"
	RuleCredentialCooldown = "credential_cooldown"
	RuleQuotaLow           = "quota_low"
	RuleNodeOffline        = "node_offline"
)

// Settings is the single-row alerting configuration.
type Settings struct {
	ID                        int       `gorm:"column:id;primaryKey" json:"-"`
	Enabled                   bool      `gorm:"column:enabled;not null" json:"enabled"`
	IntervalSeconds           int       `gorm:"column:interval_seconds;not null" json:"interval_seconds"`
	RepeatMinutes             int       `gorm:"column:repeat_minutes;not null" json:"repeat_minutes"`
	SendResolved              bool      `gorm:"column:send_resolved;not null" json:"send_resolved"`
	CredentialDeadEnabled     bool      `gorm:"column:credential_dead_enabled;not null" json:"credential_dead_enabled"`
	CredentialRefreshEnabled  bool      `gorm:"column:credential_refresh_enabled;not null" json:"credential_refresh_enabled"`
	RefreshFailureMinutes     int       `gorm:"column:refresh_failure_minutes;not null" json:"refresh_failure_minutes"`
	CredentialCooldownEnabled bool      `gorm:"column:credential_cooldown_enabled;not null" json:"credential_cooldown_enabled"`
	CooldownMinutes           int       `gorm:"column:cooldown_minutes;not null" json:"cooldown_minutes"`
	QuotaLowEnabled           bool      `gorm:"column:quota_low_enabled;not null" json:"quota_low_enabled"`
	QuotaThresholdPercent     float64   `gorm:"column:quota_threshold_percent;not null" json:"quota_threshold_percent"`
	NodeOfflineEnabled        bool      `gorm:"column:node_offline_enabled;not null" json:"node_offline_enabled"`
	NodeOfflineSeconds        int       `gorm:"column:node_offline_seconds;not null" json:"node_offline_seconds"`
	UpdatedAt                 time.Time `gorm:"column:updated_at" json:"updated_at"`
}

func (Settings) TableName() string { return "alert_settings" }

// DefaultSettings returns the settings used when no row exists yet.
func DefaultSettings() Settings {
	return Settings{
		ID:                        1,
		Enabled:                   true,
		IntervalSeconds:           60,
		RepeatMinutes:             0,
		SendResolved:              true,
		CredentialDeadEnabled:     true,
		CredentialRefreshEnabled:  true,
		RefreshFailureMinutes:     5,
		CredentialCooldownEnabled: true,
		CooldownMinutes:           30,
		QuotaLowEnabled:           true,
		QuotaThresholdPercent:     10,
		NodeOfflineEnabled:        true,
		NodeOfflineSeconds:        120,
	}
}

func (s Settings) ruleEnabled(rule string) bool {
	switch rule {
	case RuleCredentialDead:
		return s.CredentialDeadEnabled
	case RuleCredentialRefresh:
		return s.CredentialRefreshEnabled
	case RuleCredentialCooldown:
		return s.CredentialCooldownEnabled
	case RuleQuotaLow:
		return s.QuotaLowEnabled
	case RuleNodeOffline:
		return s.NodeOfflineEnabled
	default:
		return false
	}
}

func (s Settings) interval() time.Duration {
	return time.Duration(s.IntervalSeconds) * time.Second
}

// Channel is a notification destination.
type Channel struct {
	ID         uint      `gorm:"column:id;primaryKey;autoIncrement"`
	Name       string    `gorm:"column:name;not null;size:128"`
	Type       string    `gorm:"column:type;not null;size:32"`
	Enabled    bool      `gorm:"column:enabled;not null"`
	BotToken   string    `gorm:"column:bot_token;size:256"`
	ChatID     string    `gorm:"column:chat_id;size:128"`
	ThreadID   string    `gorm:"column:thread_id;size:64"`
	WebhookURL string    `gorm:"column:webhook_url;size:2048"`
	CreatedAt  time.Time `gorm:"column:created_at"`
	UpdatedAt  time.Time `gorm:"column:updated_at"`
}

func (Channel) TableName() string { return "alert_channels" }

// State tracks one alert subject so notifications are sent on transitions only.
type State struct {
	Key            string     `gorm:"column:alert_key;primaryKey;size:512" json:"key"`
	Rule           string     `gorm:"column:rule;not null;size:64;index" json:"rule"`
	Subject        string     `gorm:"column:subject;not null;size:384" json:"subject"`
	Title          string     `gorm:"column:title;size:512" json:"title"`
	Detail         string     `gorm:"column:detail;size:1024" json:"detail"`
	Firing         bool       `gorm:"column:firing;not null" json:"firing"`
	FirstSeenAt    time.Time  `gorm:"column:first_seen_at;not null" json:"first_seen_at"`
	LastNotifiedAt *time.Time `gorm:"column:last_notified_at" json:"last_notified_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at" json:"updated_at"`
}

func (State) TableName() string { return "alert_state" }

// leaseRecord elects a single evaluating Home when several Homes share a database.
type leaseRecord struct {
	ID        int       `gorm:"column:id;primaryKey"`
	Owner     string    `gorm:"column:owner;size:64"`
	ExpiresAt time.Time `gorm:"column:expires_at;not null"`
}

func (leaseRecord) TableName() string { return "alert_lease" }
