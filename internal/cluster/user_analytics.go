package cluster

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

const UserAnalyticsMaxRange = 366 * 24 * time.Hour

var ErrUserAnalyticsKey = errors.New("analytics API key not found")

// UserAnalyticsQuery has no caller-controlled ownership override.
type UserAnalyticsQuery struct {
	From     time.Time
	To       time.Time
	Model    string
	APIKeyID *uint
}

type UserAnalyticsMetrics struct {
	Requests         int64   `json:"requests"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheTokens      int64   `json:"cache_tokens"`
	BilledAmount     float64 `json:"billed_amount"`
	BaseAmount       float64 `json:"base_amount"`
	UnpricedRequests int64   `json:"unpriced_requests"`
}

type UserAnalyticsDay struct {
	Date string `json:"date"`
	UserAnalyticsMetrics
}

type UserAnalyticsModel struct {
	Model string `json:"model"`
	UserAnalyticsMetrics
}

type UserAnalyticsKey struct {
	APIKeyID *uint `json:"api_key_id"`
	UserAnalyticsMetrics
}

type UserAnalyticsResult struct {
	Summary  UserAnalyticsMetrics `json:"summary"`
	Daily    []UserAnalyticsDay   `json:"daily"`
	PerModel []UserAnalyticsModel `json:"per_model"`
	PerKey   []UserAnalyticsKey   `json:"per_key"`
}

// UserAnalytics aggregates the immutable charge ledger, including zero-price and
// unpriced charges. Never infer historical ownership from a current key binding.
// One grouped statement supplies every view, avoiding inconsistent reads while
// ingestion continues and avoiding loading individual requests or secret fields.
func (r *Repository) UserAnalytics(ctx context.Context, userID uint, query UserAnalyticsQuery) (UserAnalyticsResult, error) {
	if userID == 0 || !query.From.Before(query.To) || query.To.Sub(query.From) > UserAnalyticsMaxRange {
		return UserAnalyticsResult{}, fmt.Errorf("user and a positive range of at most 366 days are required")
	}
	db, errDB := r.database()
	if errDB != nil {
		return UserAnalyticsResult{}, errDB
	}
	db = db.WithContext(contextOrBackground(ctx))
	if query.APIKeyID != nil {
		// Include soft-deleted keys for historical exports, but not reassigned keys.
		var count int64
		if errCount := db.Unscoped().Model(&APIKeyRecord{}).Where("id = ? AND user_id = ?", *query.APIKeyID, userID).Count(&count).Error; errCount != nil {
			return UserAnalyticsResult{}, errCount
		}
		if count == 0 {
			return UserAnalyticsResult{}, ErrUserAnalyticsKey
		}
	}
	dateExpr := `strftime('%Y-%m-%d', created_at)`
	if db.Dialector.Name() == "postgres" {
		dateExpr = `to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD')`
	}
	scope := db.Table("billing_charge").Where("user_id = ? AND created_at >= ? AND created_at < ?", userID, query.From.UTC(), query.To.UTC())
	if query.Model != "" {
		scope = scope.Where("model = ?", query.Model)
	}
	if query.APIKeyID != nil {
		scope = scope.Where("api_key_id = ?", *query.APIKeyID)
	}
	var rows []struct {
		Date                 string
		Model                string
		APIKeyID             *uint
		UserAnalyticsMetrics `gorm:"embedded"`
	}
	if errScan := scope.Select(dateExpr + ` AS date, model, api_key_id,
		COUNT(*) AS requests, COALESCE(SUM(input_tokens), 0) AS input_tokens,
		COALESCE(SUM(output_tokens), 0) AS output_tokens, COALESCE(SUM(cache_tokens), 0) AS cache_tokens,
		COALESCE(SUM(amount), 0) AS billed_amount, COALESCE(SUM(base_amount), 0) AS base_amount,
		SUM(CASE WHEN cost_diagnostic = 'unpriced_model' THEN 1 ELSE 0 END) AS unpriced_requests`).
		Group(dateExpr + ", model, api_key_id").Scan(&rows).Error; errScan != nil {
		return UserAnalyticsResult{}, errScan
	}
	result := UserAnalyticsResult{Daily: []UserAnalyticsDay{}, PerModel: []UserAnalyticsModel{}, PerKey: []UserAnalyticsKey{}}
	days := map[string]int{}
	from := query.From.UTC()
	for day := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC); day.Before(query.To); day = day.AddDate(0, 0, 1) {
		date := day.Format(time.DateOnly)
		days[date] = len(result.Daily)
		result.Daily = append(result.Daily, UserAnalyticsDay{Date: date})
	}
	models := map[string]UserAnalyticsMetrics{}
	keys := map[uint]UserAnalyticsMetrics{}
	for _, row := range rows {
		result.Summary.add(row.UserAnalyticsMetrics)
		if index, exists := days[row.Date]; exists {
			result.Daily[index].UserAnalyticsMetrics.add(row.UserAnalyticsMetrics)
		}
		model := models[row.Model]
		model.add(row.UserAnalyticsMetrics)
		models[row.Model] = model
		var id uint
		if row.APIKeyID != nil {
			id = *row.APIKeyID
		}
		key := keys[id]
		key.add(row.UserAnalyticsMetrics)
		keys[id] = key
	}
	for model, metrics := range models {
		result.PerModel = append(result.PerModel, UserAnalyticsModel{Model: model, UserAnalyticsMetrics: metrics})
	}
	for id, metrics := range keys {
		var keyID *uint
		if id != 0 {
			value := id
			keyID = &value
		}
		result.PerKey = append(result.PerKey, UserAnalyticsKey{APIKeyID: keyID, UserAnalyticsMetrics: metrics})
	}
	sort.Slice(result.PerModel, func(i, j int) bool { return result.PerModel[i].Model < result.PerModel[j].Model })
	sort.Slice(result.PerKey, func(i, j int) bool {
		if result.PerKey[i].APIKeyID == nil {
			return true
		}
		if result.PerKey[j].APIKeyID == nil {
			return false
		}
		return *result.PerKey[i].APIKeyID < *result.PerKey[j].APIKeyID
	})
	return result, nil
}

func (m *UserAnalyticsMetrics) add(other UserAnalyticsMetrics) {
	m.Requests += other.Requests
	m.InputTokens += other.InputTokens
	m.OutputTokens += other.OutputTokens
	m.CacheTokens += other.CacheTokens
	m.BilledAmount += other.BilledAmount
	m.BaseAmount += other.BaseAmount
	m.UnpricedRequests += other.UnpricedRequests
}
