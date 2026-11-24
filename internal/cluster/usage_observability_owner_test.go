package cluster

import (
	"fmt"
	"testing"
	"time"
)

func TestUsageObservabilityFiltersByOrganizationAndMember(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	alice := createPeriodLimitUser(t, repo, ctx, "alice", 100)
	bob := createPeriodLimitUser(t, repo, ctx, "bob", 100)
	org := createOrganizationTestOrg(t, repo, "acme", 100)
	addOrganizationTestMember(t, repo, org.ID, alice.ID, OrganizationRoleOwner)
	addOrganizationTestMember(t, repo, org.ID, bob.ID, OrganizationRoleMember)
	createOrganizationTestKey(t, repo, org.ID, alice.ID, "org-key-alice-0001")
	createOrganizationTestKey(t, repo, org.ID, bob.ID, "org-key-bob-00002")
	personalKey := "personal-key-alice-3"
	if _, errKey := repo.CreateAPIKeyForUser(ctx, alice.ID, APIKeyUserUpdate{APIKey: &personalKey}); errKey != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errKey)
	}
	if _, errPrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{Provider: "openai", Model: "gpt-4.1-mini", RequestPrice: 1, Enabled: true}); errPrice != nil {
		t.Fatalf("CreateBillingModelPrice() error = %v", errPrice)
	}
	for index, key := range []string{"org-key-alice-0001", "org-key-bob-00002", "org-key-bob-00002", personalKey} {
		payload := fmt.Sprintf(`{"timestamp":"2026-06-10T01:0%d:00Z","provider":"openai","model":"gpt-4.1-mini","api_key":%q,"request_id":"req-owner-%d","tokens":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`, index, key, index)
		if _, errUsage := repo.AppendUsage(ctx, payload, "192.0.2.10"); errUsage != nil {
			t.Fatalf("AppendUsage(%d) error = %v", index, errUsage)
		}
	}

	from := time.Date(2026, time.June, 10, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, time.June, 11, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		user  *uint
		actor *uint
		want  int64
	}{
		{name: "all", want: 4},
		{name: "organization", user: &org.ID, want: 3},
		{name: "organization member", user: &org.ID, actor: &bob.ID, want: 2},
		{name: "personal owner", user: &alice.ID, want: 1},
	}
	for _, tc := range cases {
		overview, errOverview := repo.UsageObservabilityOverview(ctx, UsageObservabilityOverviewQuery{From: &from, To: &to, UserID: tc.user, ActorUserID: tc.actor, Timezone: "UTC", Interval: "hour"})
		if errOverview != nil {
			t.Fatalf("%s: UsageObservabilityOverview() error = %v", tc.name, errOverview)
		}
		if overview.Totals.RequestCount != tc.want {
			t.Fatalf("%s: overview request count = %d, want %d", tc.name, overview.Totals.RequestCount, tc.want)
		}
		records, errRecords := repo.ListUsageObservabilityRecords(ctx, UsageObservabilityRecordQuery{From: &from, To: &to, UserID: tc.user, ActorUserID: tc.actor, Limit: 10})
		if errRecords != nil {
			t.Fatalf("%s: ListUsageObservabilityRecords() error = %v", tc.name, errRecords)
		}
		if records.Total != tc.want {
			t.Fatalf("%s: record total = %d, want %d", tc.name, records.Total, tc.want)
		}
		aggregates, errAggregates := repo.ListUsageObservabilityAggregates(ctx, UsageObservabilityAggregateQuery{From: &from, To: &to, UserID: tc.user, ActorUserID: tc.actor, GroupBy: "model", Metric: "request_count", Limit: 10})
		if errAggregates != nil {
			t.Fatalf("%s: ListUsageObservabilityAggregates() error = %v", tc.name, errAggregates)
		}
		if len(aggregates.Items) != 1 || aggregates.Items[0].RequestCount != tc.want {
			t.Fatalf("%s: aggregates = %+v, want one model with %d requests", tc.name, aggregates.Items, tc.want)
		}
	}

	charges, errCharges := repo.BillingOverview(ctx, BillingOverviewQuery{From: &from, ToExclusive: &to, UserID: &org.ID, ActorUserID: &bob.ID})
	if errCharges != nil {
		t.Fatalf("BillingOverview() error = %v", errCharges)
	}
	if charges.RequestCount != 2 {
		t.Fatalf("billing overview request count = %d, want 2", charges.RequestCount)
	}
}
