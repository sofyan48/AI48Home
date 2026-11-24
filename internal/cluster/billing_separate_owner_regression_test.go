package cluster

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestBillingSeparateOrganizationOwner(t *testing.T) {
	for _, unlimited := range []bool{false, true} {
		t.Run(fmt.Sprintf("unlimited=%v", unlimited), func(t *testing.T) {
			ctx := context.Background()
			repo, closeRepo := newBillingTestRepository(t, ctx)
			defer closeRepo()
			testBillingSeparateOrganizationOwner(t, ctx, repo, unlimited)
		})
	}
}

func TestBillingSeparateOrganizationOwnerPostgres(t *testing.T) {
	testBillingSeparateOrganizationOwner(t, context.Background(), newPostgresQuiescenceRepository(t), false)
}

// Insert the payer directly so this regression does not depend on organization
// creation, authentication, API-key validation, or identity migration workflows.
func testBillingSeparateOrganizationOwner(t *testing.T, ctx context.Context, repo *Repository, unlimited bool) {
	t.Helper()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	if errMigrate := db.AutoMigrate(&OrganizationRecord{}); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	member := &UserRecord{ID: 700001, Username: "separate-member", Credits: 50, BillingMultiplier: float64Ptr(3)}
	org := &OrganizationRecord{
		ID: 700002, Username: "separate-org", Credits: 100, CreditsUnlimited: unlimited,
		BillingMultiplier: float64Ptr(0.5), Timezone: "UTC",
		Limit5hCredits: float64Ptr(10), WindowMode5h: PeriodWindowModeFirstUse,
	}
	if errCreate := db.Create(member).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if errCreate := db.Create(org).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	key := &APIKeyRecord{APIKey: "separate-org-key", UserID: &org.ID, CreatedByUserID: &member.ID}
	if errCreate := db.Create(key).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if _, errPrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider: "openai", Model: "separate-model", RequestPrice: 2, Enabled: true,
	}); errPrice != nil {
		t.Fatal(errPrice)
	}
	payload := `{"timestamp":"2026-11-22T01:00:00Z","execution_id":"separate-org-execution","provider":"openai","model":"separate-model","api_key":"separate-org-key","request_id":"separate-org-request","tokens":{"input_tokens":10,"total_tokens":10}}`
	first, inserted, errAppend := repo.AppendUsageWithRuntimeOutcome(ctx, payload, UsageRuntimeMetadata{HomeIP: "192.0.2.10"})
	if errAppend != nil || !inserted {
		t.Fatalf("initial usage: inserted=%v error=%v", inserted, errAppend)
	}
	replay, replayInserted, errReplay := repo.AppendUsageWithRuntimeOutcome(ctx, payload, UsageRuntimeMetadata{HomeIP: "192.0.2.11"})
	if errReplay != nil || replayInserted || replay.ID != first.ID {
		t.Fatalf("replay: inserted=%v error=%v", replayInserted, errReplay)
	}
	charges, errCharges := repo.ListBillingCharges(ctx, BillingChargeQuery{UserText: org.Username})
	if errCharges != nil || len(charges.Records) != 1 {
		t.Fatalf("charges=%+v error=%v", charges, errCharges)
	}
	charge := charges.Records[0]
	wantBalance := 99.0
	if unlimited {
		wantBalance = 100
	}
	if charge.UserID == nil || *charge.UserID != org.ID || charge.ActorUserID == nil || *charge.ActorUserID != member.ID ||
		charge.Amount != 1 || charge.BaseAmount != 2 || charge.BillingMultiplier != 0.5 || charge.BalanceBefore != 100 || charge.BalanceAfter != wantBalance {
		t.Fatalf("charge=%+v", charge)
	}
	if errReload := db.First(org, "id = ?", org.ID).Error; errReload != nil {
		t.Fatal(errReload)
	}
	if org.Credits != wantBalance || org.PeriodWindowStart5h == nil || !org.PeriodWindowStart5h.Equal(first.Timestamp) {
		t.Fatalf("organization balance/window=%+v", org)
	}
	if errReload := db.First(member, "id = ?", member.ID).Error; errReload != nil || member.Credits != 50 {
		t.Fatalf("member=%+v error=%v", member, errReload)
	}
	var orgUserRows int64
	if errCount := db.Model(&UserRecord{}).Where("id = ?", org.ID).Count(&orgUserRows).Error; errCount != nil || orgUserRows != 0 {
		t.Fatalf("organization auth rows=%d error=%v", orgUserRows, errCount)
	}
	overview, errOverview := repo.BillingOverview(ctx, BillingOverviewQuery{UserID: &org.ID, UserText: org.Username})
	if errOverview != nil || overview.TotalBalance != wantBalance || overview.RequestCount != 1 ||
		len(overview.TopUsers) != 1 || overview.TopUsers[0].Label != org.Username ||
		len(overview.TopMembers) != 1 || overview.TopMembers[0].Label != member.Username {
		t.Fatalf("overview=%+v error=%v", overview, errOverview)
	}
	records, errRecords := repo.ListUsageObservabilityRecords(ctx, UsageObservabilityRecordQuery{User: org.Username})
	if errRecords != nil || len(records.Records) != 1 || records.Records[0].Client.Username != org.Username {
		t.Fatalf("usage records=%+v error=%v", records, errRecords)
	}
	from := first.Timestamp.Add(-time.Minute)
	to := first.Timestamp.Add(time.Minute)
	usageOverview, errUsageOverview := repo.UsageObservabilityOverview(ctx, UsageObservabilityOverviewQuery{
		From: &from, To: &to, UserID: &org.ID, Timezone: "UTC", Interval: "hour",
	})
	if errUsageOverview != nil || usageOverview.Totals.RequestCount != 1 || len(usageOverview.Top.Users) != 1 || usageOverview.Top.Users[0].Label != org.Username {
		t.Fatalf("usage overview=%+v error=%v", usageOverview, errUsageOverview)
	}
	request, balance, errRequest := repo.CreateBillingRechargeRequest(ctx, org.ID, 5, "organization topup")
	if errRequest != nil || balance != wantBalance {
		t.Fatalf("request=%+v balance=%v error=%v", request, balance, errRequest)
	}
	approved, ledger, errApprove := repo.ApproveBillingRechargeRequest(ctx, org.ID, request.ID)
	if !unlimited {
		wantBalance += 5
	}
	if errApprove != nil || approved.Credits != wantBalance || ledger.BalanceAfter != wantBalance {
		t.Fatalf("approved=%+v ledger=%+v error=%v", approved, ledger, errApprove)
	}
	_, replayLedger, errApproveReplay := repo.ApproveBillingRechargeRequest(ctx, org.ID, request.ID)
	if errApproveReplay != nil || replayLedger.ID != ledger.ID {
		t.Fatalf("approval replay=%+v error=%v", replayLedger, errApproveReplay)
	}
	ledgers, errLedgers := repo.ListBillingBalanceRecords(ctx, BillingBalanceQuery{UserText: org.Username})
	if errLedgers != nil || ledgers.Total != 1 {
		t.Fatalf("ledgers=%+v error=%v", ledgers, errLedgers)
	}
	if _, errDeduct := repo.ApplyBillingBalanceRecord(ctx, BillingBalanceUpdate{
		UserID: org.ID, Type: BillingBalanceTypeDeduct, Amount: 2, Note: "organization adjustment",
	}); errDeduct != nil {
		t.Fatal(errDeduct)
	}
	if !unlimited {
		wantBalance -= 2
	}
	if errReload := db.First(org, "id = ?", org.ID).Error; errReload != nil || org.Credits != wantBalance {
		t.Fatalf("final organization=%+v error=%v", org, errReload)
	}
	if errReload := db.First(member, "id = ?", member.ID).Error; errReload != nil || member.Credits != 50 {
		t.Fatalf("final member=%+v error=%v", member, errReload)
	}
}
