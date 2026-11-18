package alerting

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const maxDetailLength = 300

// candidate is a condition currently observed for one subject.
type candidate struct {
	Key     string
	Rule    string
	Subject string
	Title   string
	Detail  string
	// HoldFor delays the first notification until the condition persists this long.
	HoldFor time.Duration
	// Immediate fires on the first observation regardless of HoldFor.
	Immediate bool
}

// credentialInfo is the subset of credential state the checks need.
type credentialInfo struct {
	ID             string
	Name           string
	Provider       string
	Status         string
	Disabled       bool
	NextRetryAfter *time.Time
	StatusMessage  string
	LastError      string
	RefreshError   string
	QuotaReason    string
}

func (c credentialInfo) displayName() string {
	name := strings.TrimSpace(c.Name)
	if name == "" {
		name = c.ID
	}
	if provider := strings.TrimSpace(c.Provider); provider != "" {
		return fmt.Sprintf("%s (%s)", name, provider)
	}
	return name
}

// loadCredentials reads active credentials without exposing secret fields.
func loadCredentials(ctx context.Context, db *gorm.DB) (map[string]credentialInfo, error) {
	var records []cluster.AuthRecord
	errFind := db.WithContext(ctx).
		Select("uuid", "label", "provider", "status", "disabled", "next_retry_after", "auth_json").
		Find(&records).Error
	if errFind != nil {
		return nil, fmt.Errorf("load credentials: %w", errFind)
	}
	out := make(map[string]credentialInfo, len(records))
	for _, record := range records {
		raw := string(record.AuthJSON)
		out[record.UUID] = credentialInfo{
			ID:             record.UUID,
			Name:           record.Label,
			Provider:       record.Provider,
			Status:         string(record.Status),
			Disabled:       record.Disabled,
			NextRetryAfter: record.NextRetryAfter,
			StatusMessage:  gjson.Get(raw, "status_message").String(),
			LastError:      errorText(gjson.Get(raw, "last_error")),
			RefreshError:   errorText(gjson.Get(raw, "last_refresh_error")),
			QuotaReason:    gjson.Get(raw, "quota.reason").String(),
		}
	}
	return out, nil
}

func errorText(value gjson.Result) string {
	if !value.Exists() || value.Type == gjson.Null {
		return ""
	}
	message := strings.TrimSpace(value.Get("message").String())
	if message == "" {
		message = strings.TrimSpace(value.Get("code").String())
	}
	if status := value.Get("http_status").Int(); status > 0 && message != "" {
		message = fmt.Sprintf("HTTP %d: %s", status, message)
	}
	return truncate(message, maxDetailLength)
}

// isDeadCredential matches credentials disabled by Home after an unauthorized
// upstream response, not credentials disabled by an operator.
func isDeadCredential(c credentialInfo) bool {
	return c.Status == string(coreauth.StatusDisabled) && strings.EqualFold(strings.TrimSpace(c.StatusMessage), "unauthorized")
}

func credentialCandidates(settings Settings, credentials map[string]credentialInfo, now time.Time) []candidate {
	out := make([]candidate, 0)
	for _, credential := range credentials {
		name := credential.displayName()
		if settings.CredentialDeadEnabled && isDeadCredential(credential) {
			detail := credential.LastError
			if detail == "" {
				detail = "Upstream rejected the credential (unauthorized)."
			}
			out = append(out, candidate{
				Key: RuleCredentialDead + ":" + credential.ID, Rule: RuleCredentialDead, Subject: credential.ID,
				Title: "Credential dead: " + name, Detail: detail, Immediate: true,
			})
		}
		if credential.Disabled {
			continue
		}
		if settings.CredentialRefreshEnabled && credential.RefreshError != "" {
			out = append(out, candidate{
				Key: RuleCredentialRefresh + ":" + credential.ID, Rule: RuleCredentialRefresh, Subject: credential.ID,
				Title: "Credential refresh failing: " + name, Detail: credential.RefreshError,
				HoldFor: time.Duration(settings.RefreshFailureMinutes) * time.Minute,
			})
		}
		if settings.CredentialCooldownEnabled && credential.NextRetryAfter != nil && credential.NextRetryAfter.After(now) {
			threshold := time.Duration(settings.CooldownMinutes) * time.Minute
			remaining := credential.NextRetryAfter.Sub(now)
			reason := credential.QuotaReason
			if reason == "" {
				reason = credential.LastError
			}
			detail := fmt.Sprintf("Cooling down until %s (%s left).", credential.NextRetryAfter.UTC().Format(time.RFC3339), roundDuration(remaining))
			if reason != "" {
				detail += " Reason: " + reason
			}
			out = append(out, candidate{
				Key: RuleCredentialCooldown + ":" + credential.ID, Rule: RuleCredentialCooldown, Subject: credential.ID,
				Title: "Credential in long cooldown: " + name, Detail: truncate(detail, maxDetailLength),
				HoldFor: threshold, Immediate: remaining >= threshold,
			})
		}
	}
	return out
}

func quotaCandidates(ctx context.Context, db *gorm.DB, settings Settings, credentials map[string]credentialInfo, now time.Time) ([]candidate, error) {
	var windows []cluster.QuotaWindowRecord
	errFind := db.WithContext(ctx).
		Where("is_unlimited = ? AND (expires_at IS NULL OR expires_at > ?)", false, now).
		Find(&windows).Error
	if errFind != nil {
		return nil, fmt.Errorf("load quota windows: %w", errFind)
	}
	out := make([]candidate, 0)
	for _, window := range windows {
		credential, ok := credentials[window.CredentialID]
		if !ok || credential.Disabled {
			continue
		}
		ratio, okRatio := remainingRatio(window)
		if !okRatio || ratio*100 > settings.QuotaThresholdPercent {
			continue
		}
		label := strings.TrimSpace(window.Label)
		if label == "" {
			label = window.WindowID
		}
		detail := fmt.Sprintf("Window %q has %.1f%% remaining.", label, math.Max(ratio, 0)*100)
		if window.ResetAt != nil {
			detail += " Resets at " + window.ResetAt.UTC().Format(time.RFC3339) + "."
		}
		out = append(out, candidate{
			Key:     RuleQuotaLow + ":" + window.CredentialID + ":" + window.WindowID,
			Rule:    RuleQuotaLow,
			Subject: window.CredentialID,
			Title:   "Upstream quota low: " + credential.displayName(),
			Detail:  detail, Immediate: true,
		})
	}
	return out, nil
}

func remainingRatio(window cluster.QuotaWindowRecord) (float64, bool) {
	if window.RemainingRatio != nil {
		return *window.RemainingRatio, true
	}
	if window.UsedRatio != nil {
		return 1 - *window.UsedRatio, true
	}
	if window.Remaining != nil && window.Limit != nil && *window.Limit > 0 {
		return *window.Remaining / *window.Limit, true
	}
	return 0, false
}

// NodeSeen remembers the last heartbeat of every CPA node. Home prunes stale
// cpa_node rows shortly after the heartbeat timeout, so offline detection
// needs its own memory of nodes that disappeared.
type NodeSeen struct {
	NodeID     string    `gorm:"column:node_id;primaryKey;size:256" json:"node_id"`
	ClientIP   string    `gorm:"column:client_ip;size:128" json:"client_ip"`
	LastSeenAt time.Time `gorm:"column:last_seen_at;not null" json:"last_seen_at"`
	UpdatedAt  time.Time `gorm:"column:updated_at" json:"updated_at"`
}

func (NodeSeen) TableName() string { return "alert_nodes" }

func nodeCandidates(ctx context.Context, db *gorm.DB, settings Settings, now time.Time) ([]candidate, error) {
	var records []cluster.CPANodeRecord
	errFind := db.WithContext(ctx).Select("node_id", "client_ip", "last_seen_at").Find(&records).Error
	if errFind != nil {
		return nil, fmt.Errorf("load cpa nodes: %w", errFind)
	}
	latest := make(map[string]NodeSeen)
	for _, record := range records {
		nodeID := strings.TrimSpace(record.NodeID)
		if nodeID == "" {
			nodeID = strings.TrimSpace(record.ClientIP)
		}
		if nodeID == "" {
			continue
		}
		if current, ok := latest[nodeID]; ok && !record.LastSeenAt.After(current.LastSeenAt) {
			continue
		}
		latest[nodeID] = NodeSeen{NodeID: nodeID, ClientIP: record.ClientIP, LastSeenAt: record.LastSeenAt.UTC(), UpdatedAt: now}
	}
	for _, seen := range latest {
		errUpsert := db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "node_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"client_ip", "last_seen_at", "updated_at"}),
		}).Create(&seen).Error
		if errUpsert != nil {
			return nil, fmt.Errorf("record cpa node heartbeat: %w", errUpsert)
		}
	}

	var known []NodeSeen
	if errKnown := db.WithContext(ctx).Find(&known).Error; errKnown != nil {
		return nil, fmt.Errorf("load known cpa nodes: %w", errKnown)
	}
	names := nodeNames(ctx, db)
	threshold := time.Duration(settings.NodeOfflineSeconds) * time.Second
	out := make([]candidate, 0)
	for _, node := range known {
		silent := now.Sub(node.LastSeenAt)
		if silent < threshold {
			continue
		}
		name := node.NodeID
		if custom := strings.TrimSpace(names[node.NodeID]); custom != "" {
			name = fmt.Sprintf("%s (%s)", custom, node.NodeID)
		}
		detail := fmt.Sprintf("No heartbeat for %s (last seen %s", roundDuration(silent), node.LastSeenAt.UTC().Format(time.RFC3339))
		if node.ClientIP != "" {
			detail += ", IP " + node.ClientIP
		}
		detail += ")."
		out = append(out, candidate{
			Key: RuleNodeOffline + ":" + node.NodeID, Rule: RuleNodeOffline, Subject: node.NodeID,
			Title: "CPA node offline: " + name, Detail: detail, Immediate: true,
		})
	}
	return out, nil
}

// nodeNames returns operator-assigned names; failures only lose the names.
func nodeNames(ctx context.Context, db *gorm.DB) map[string]string {
	var records []cluster.CPANodeMetadataRecord
	out := make(map[string]string)
	if errFind := db.WithContext(ctx).Find(&records).Error; errFind != nil {
		return out
	}
	for _, record := range records {
		out[record.NodeID] = record.NodeName
	}
	return out
}

func roundDuration(d time.Duration) string {
	if d >= time.Minute {
		return d.Round(time.Minute).String()
	}
	return d.Round(time.Second).String()
}

func truncate(value string, limit int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}
