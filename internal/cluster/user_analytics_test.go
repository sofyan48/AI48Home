package cluster

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestUserAnalyticsAggregatesAndHistoricalOwnership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, closeRepo := newBillingTestRepository(t, ctx)
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	owner, other := uint(11), uint(22)
	key := APIKeyRecord{APIKey: "secret-do-not-export", UserID: &owner}
	if errCreate := db.Create(&key).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	start := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	query := UserAnalyticsQuery{From: start, To: start.AddDate(0, 0, 3)}
	for index, fixture := range []struct {
		stamp      time.Time
		user       *uint
		model      string
		amount     float64
		diagnostic string
	}{
		{start.Add(-time.Nanosecond), &owner, "excluded-before", 99, ""},
		{start, &owner, "a", 1.25, ""},
		{start.Add(23 * time.Hour), &owner, "a", 0, BillingCostDiagnosticUnpricedModel},
		{start.Add(48 * time.Hour), &owner, "b", 2.5, ""},
		{query.To, &owner, "excluded-end", 99, ""},
		{start, &other, "foreign", 99, ""},
		{start, nil, "unattributed", 99, ""},
	} {
		charge := BillingChargeRecord{
			ID: fmt.Sprintf("analytics-%d", index), UsageID: uint(index + 1), PayloadHash: fmt.Sprintf("hash-%d", index),
			UserID: fixture.user, APIKeyID: &key.ID, Model: fixture.model, CreatedAt: fixture.stamp,
			InputTokens: 10, OutputTokens: 3, CacheTokens: 2,
			Amount: fixture.amount, BaseAmount: fixture.amount * 2, CostDiagnostic: fixture.diagnostic, PriceSnapshot: JSONB(`{}`),
		}
		if errCreate := db.Create(&charge).Error; errCreate != nil {
			t.Fatal(errCreate)
		}
	}
	result, errAnalytics := repo.UserAnalytics(ctx, owner, query)
	if errAnalytics != nil {
		t.Fatal(errAnalytics)
	}
	want := UserAnalyticsMetrics{Requests: 3, InputTokens: 30, OutputTokens: 9, CacheTokens: 6, BilledAmount: 3.75, BaseAmount: 7.5, UnpricedRequests: 1}
	if !reflect.DeepEqual(result.Summary, want) {
		t.Fatalf("summary=%+v want=%+v", result.Summary, want)
	}
	if len(result.Daily) != 3 || result.Daily[0].Requests != 2 || result.Daily[1].Requests != 0 || result.Daily[2].Requests != 1 {
		t.Fatalf("daily=%+v", result.Daily)
	}
	if len(result.PerModel) != 2 || result.PerModel[0].Model != "a" || result.PerModel[0].Requests != 2 || len(result.PerKey) != 1 || result.PerKey[0].UserAnalyticsMetrics != want {
		t.Fatalf("result=%+v", result)
	}
	query.Model = "a"
	query.APIKeyID = &key.ID
	filtered, errFiltered := repo.UserAnalytics(ctx, owner, query)
	if errFiltered != nil || filtered.Summary.Requests != 2 || filtered.Summary.BilledAmount != 1.25 {
		t.Fatalf("filtered=%+v err=%v", filtered, errFiltered)
	}
	if _, errForeign := repo.UserAnalytics(ctx, other, query); !errors.Is(errForeign, ErrUserAnalyticsKey) {
		t.Fatalf("foreign key error=%v", errForeign)
	}
	if errReassign := db.Model(&APIKeyRecord{}).Where("id = ?", key.ID).Update("user_id", other).Error; errReassign != nil {
		t.Fatal(errReassign)
	}
	if _, errReassigned := repo.UserAnalytics(ctx, owner, query); !errors.Is(errReassigned, ErrUserAnalyticsKey) {
		t.Fatalf("reassigned key error=%v", errReassigned)
	}
	query.Model = ""
	query.APIKeyID = nil
	after, errAfter := repo.UserAnalytics(ctx, owner, query)
	if errAfter != nil || !reflect.DeepEqual(after, result) {
		t.Fatalf("ownership changed: %+v err=%v", after, errAfter)
	}
	query.Model = "missing"
	empty, errEmpty := repo.UserAnalytics(ctx, owner, query)
	if errEmpty != nil || empty.Summary.Requests != 0 || len(empty.Daily) != 3 || empty.PerKey == nil || empty.PerModel == nil {
		t.Fatalf("empty=%+v err=%v", empty, errEmpty)
	}
	if _, errZero := repo.UserAnalytics(ctx, 0, query); errZero == nil {
		t.Fatal("zero user must fail closed")
	}
	query.To = query.From.Add(UserAnalyticsMaxRange + time.Nanosecond)
	if _, errRange := repo.UserAnalytics(ctx, owner, query); errRange == nil {
		t.Fatal("unbounded range accepted")
	}
}
