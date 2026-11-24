package push

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/respserver/dispatch"
)

// The fixture is captured from AI48Proxy's real authenticated HTTP gateway and
// mTLS LPUSH delivery in gateway_authenticated_openai_nonstream_delivers_usage_and_release.
func TestProxyUsageOrganizationIngestion(t *testing.T) {
	ctx := context.Background()
	db, rt := newUsageIngestionTestRuntime(t, true)
	repo := cluster.NewRepository(db)
	username, credits := "proxy-member", 100.0
	member, errMember := repo.CreateUser(ctx, cluster.UserUpdate{Username: &username, Credits: &credits})
	if errMember != nil {
		t.Fatal(errMember)
	}
	orgName := "proxy-org"
	org, errOrg := repo.CreateOrganization(ctx, cluster.UserUpdate{Username: &orgName, Credits: &credits})
	if errOrg != nil {
		t.Fatal(errOrg)
	}
	if _, errAdd := repo.AddOrganizationMember(ctx, org.ID, member.ID, cluster.OrganizationMemberUpdate{}); errAdd != nil {
		t.Fatal(errAdd)
	}
	keyValue := "local-caller-key"
	key, errKey := repo.CreateOrganizationAPIKey(ctx, org.ID, member.ID, cluster.APIKeyUserUpdate{APIKey: &keyValue})
	if errKey != nil {
		t.Fatal(errKey)
	}
	if _, errPrice := repo.CreateBillingModelPrice(ctx, cluster.BillingModelPriceUpdate{Provider: "openai", Model: "resolved-model", RequestPrice: 2, Enabled: true}); errPrice != nil {
		t.Fatal(errPrice)
	}
	payload, errRead := os.ReadFile("testdata/ai48proxy_usage.json")
	if errRead != nil {
		t.Fatal(errRead)
	}
	env := dispatch.Env{Runtime: rt, NodeID: "proxy-node", ClientIP: "192.0.2.20"}
	for range 2 {
		if reply := handleUsage(ctx, env, []string{"LPUSH", "usage", string(payload)}); reply.Kind != dispatch.ReplyKindInteger || reply.Integer != 1 {
			t.Fatalf("ingestion reply=%+v", reply)
		}
	}
	charges, errCharges := repo.ListBillingCharges(ctx, cluster.BillingChargeQuery{UserID: &org.ID, ActorUserID: &member.ID, Limit: 10})
	if errCharges != nil || charges.Total != 1 || len(charges.Records) != 1 {
		t.Fatalf("organization member charges=%+v error=%v", charges, errCharges)
	}
	charge := charges.Records[0]
	if charge.APIKeyID == nil || *charge.APIKeyID != key.ID || charge.Amount != 2 || charge.BalanceAfter != 98 {
		t.Fatalf("charge=%+v", charge)
	}
	// A real failed execution can have no consumed tokens and therefore no charge.
	var failed map[string]any
	if errDecode := json.Unmarshal(payload, &failed); errDecode != nil {
		t.Fatal(errDecode)
	}
	failed["execution_id"] = "proxy-no-tokens"
	failed["failed"] = true
	failed["fail"] = map[string]any{"status_code": 429}
	delete(failed, "token_breakdown")
	delete(failed, "accounting_version")
	delete(failed, "tokens")
	failedPayload, errEncode := json.Marshal(failed)
	if errEncode != nil {
		t.Fatal(errEncode)
	}
	if reply := handleUsage(ctx, env, []string{"LPUSH", "usage", string(failedPayload)}); reply.Kind != dispatch.ReplyKindInteger {
		t.Fatalf("failed ingestion reply=%+v", reply)
	}
	for _, scope := range []struct {
		name  string
		owner *uint
		actor *uint
		want  int64
	}{
		{name: "all", want: 2},
		{name: "organization", owner: &org.ID, want: 2},
		{name: "member", owner: &org.ID, actor: &member.ID, want: 2},
		{name: "personal isolation", owner: &member.ID},
	} {
		records, errRecords := repo.ListUsageObservabilityRecords(ctx, cluster.UsageObservabilityRecordQuery{UserID: scope.owner, ActorUserID: scope.actor, Limit: 10})
		if errRecords != nil || records.Total != scope.want {
			t.Fatalf("%s records=%+v error=%v", scope.name, records, errRecords)
		}
		overview, errOverview := repo.UsageObservabilityOverview(ctx, cluster.UsageObservabilityOverviewQuery{UserID: scope.owner, ActorUserID: scope.actor, Timezone: "UTC", Interval: "hour"})
		if errOverview != nil || overview.Totals.RequestCount != scope.want {
			t.Fatalf("%s overview=%+v error=%v", scope.name, overview, errOverview)
		}
		aggregates, errAggregates := repo.ListUsageObservabilityAggregates(ctx, cluster.UsageObservabilityAggregateQuery{UserID: scope.owner, ActorUserID: scope.actor, GroupBy: "model", Metric: "request_count", Limit: 10})
		if errAggregates != nil {
			t.Fatal(errAggregates)
		}
		var count int64
		for _, item := range aggregates.Items {
			count += item.RequestCount
		}
		if count != scope.want {
			t.Fatalf("%s aggregate count=%d, want %d", scope.name, count, scope.want)
		}
	}
	// A later binding change must not overwrite the actor stored on a charge.
	if errUpdate := db.Model(&cluster.APIKeyRecord{}).Where("id = ?", key.ID).Update("created_by_user_id", nil).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	historical, errHistorical := repo.ListUsageObservabilityRecords(ctx, cluster.UsageObservabilityRecordQuery{UserID: &org.ID, ActorUserID: &member.ID, Limit: 10})
	if errHistorical != nil || historical.Total != 1 {
		t.Fatalf("historical actor total=%d error=%v", historical.Total, errHistorical)
	}
	personal, errPersonal := repo.GetUser(ctx, member.ID)
	if errPersonal != nil || personal.Credits != 100 {
		t.Fatalf("personal balance changed: user=%+v error=%v", personal, errPersonal)
	}
}
