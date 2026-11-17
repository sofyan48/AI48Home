package registry

import (
	"strings"
	"time"
)

// FilterAvailableModels excludes confirmed retirements, not temporary request failures.
// Unknown lifecycle metadata remains available for backward compatibility.
func FilterAvailableModels(provider string, models []*ModelInfo) []*ModelInfo {
	return filterAvailableModelsAt(provider, models, time.Now())
}

func filterAvailableModelsAt(provider string, models []*ModelInfo, now time.Time) []*ModelInfo {
	out := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(model.LifecycleStatus)) {
		case "retired", "removed", "unavailable":
			continue
		}
		retirement := model.RetirementDate
		if retirement == "" && strings.EqualFold(strings.TrimSpace(provider), "claude") {
			id := model.MetadataModelID
			if id == "" && model.UserDefined {
				id = model.Name
			}
			if id == "" {
				id = model.ID
			}
			retirement = claudeRetirementDate(strings.ToLower(strings.TrimSpace(id)))
		}
		if date, errParse := time.Parse(time.DateOnly, retirement); errParse == nil && !now.Before(date) {
			continue
		}
		out = append(out, model)
	}
	return out
}

// Dates apply only to Anthropic-operated Claude, not partner providers.
// Source: https://platform.claude.com/docs/en/about-claude/model-deprecations
func claudeRetirementDate(id string) string {
	switch id {
	case "claude-1.0", "claude-1.1", "claude-1.2", "claude-1.3", "claude-instant-1.0", "claude-instant-1.1", "claude-instant-1.2":
		return "2024-11-06"
	case "claude-2.0", "claude-2.1", "claude-3-sonnet-20240229":
		return "2025-07-21"
	case "claude-3-5-sonnet-20240620", "claude-3-5-sonnet-20241022", "claude-3-5-sonnet-latest":
		return "2025-10-28"
	case "claude-3-opus-20240229", "claude-3-opus-latest":
		return "2026-01-05"
	case "claude-3-5-haiku-20241022", "claude-3-5-haiku-latest", "claude-3-7-sonnet-20250219", "claude-3-7-sonnet-latest":
		return "2026-02-19"
	case "claude-3-haiku-20240307":
		return "2026-04-20"
	case "claude-opus-4-20250514", "claude-opus-4-0", "claude-sonnet-4-20250514", "claude-sonnet-4-0":
		return "2026-06-15"
	case "claude-opus-4-1-20250805", "claude-opus-4-1":
		return "2026-08-05"
	case "claude-sonnet-4-5-20250929", "claude-sonnet-4-5":
		return "2026-11-30"
	default:
		return ""
	}
}
