package cluster

import (
	"fmt"
	"testing"
)

func TestOrganizationUsageChargesBillOrganizationNotMember(t *testing.T) {
	for _, tc := range []struct {
		name      string
		unlimited bool
	}{
		{name: "limited credits"},
		{name: "unlimited credits", unlimited: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
			defer closeRepo()

			owner := createPeriodLimitUser(t, repo, ctx, "owner", 20)
			member := createPeriodLimitUser(t, repo, ctx, "member", 30)
			other := createPeriodLimitUser(t, repo, ctx, "other", 40)
			org := createOrganizationTestOrg(t, repo, "acme", 100)
			addOrganizationTestMember(t, repo, org.ID, owner.ID, OrganizationRoleOwner)
			addOrganizationTestMember(t, repo, org.ID, member.ID, OrganizationRoleMember)
			ownerKey := createOrganizationTestKey(t, repo, org.ID, owner.ID, "org-owner-key")
			memberKey := createOrganizationTestKey(t, repo, org.ID, member.ID, "org-member-key")
			personalKey, errKey := repo.CreateAPIKeyForUser(ctx, member.ID, APIKeyUserUpdate{APIKey: stringPtr("personal-key")})
			if errKey != nil {
				t.Fatal(errKey)
			}
			// The billed account's multiplier must win over the acting member's.
			db, errDB := repo.database()
			if errDB != nil {
				t.Fatal(errDB)
			}
			if errUpdate := db.Model(&OrganizationRecord{}).Where("id = ?", org.ID).Updates(map[string]any{
				"billing_multiplier": 0.5, "credits_unlimited": tc.unlimited,
			}).Error; errUpdate != nil {
				t.Fatal(errUpdate)
			}
			if errUpdate := db.Model(&UserRecord{}).Where("id = ?", member.ID).Update("billing_multiplier", 2).Error; errUpdate != nil {
				t.Fatal(errUpdate)
			}
			if _, errPrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
				Provider: "openai", Model: "org-model", InputPricePerMillion: 2, Enabled: true,
			}); errPrice != nil {
				t.Fatal(errPrice)
			}

			for index, key := range []*APIKeyRecord{ownerKey, memberKey, personalKey} {
				payload := fmt.Sprintf(`{"timestamp":"2026-11-22T01:00:00Z","execution_id":"org-execution-%d","provider":"openai","model":"org-model","api_key":%q,"request_id":"org-request-%d","tokens":{"input_tokens":1000000,"total_tokens":1000000}}`, index, key.APIKey, index)
				first, inserted, errAppend := repo.AppendUsageWithRuntimeOutcome(ctx, payload, UsageRuntimeMetadata{HomeIP: "192.0.2.10"})
				if errAppend != nil || !inserted {
					t.Fatalf("initial usage %d: inserted=%v error=%v", index, inserted, errAppend)
				}
				replay, replayInserted, errReplay := repo.AppendUsageWithRuntimeOutcome(ctx, payload, UsageRuntimeMetadata{HomeIP: "192.0.2.11"})
				if errReplay != nil || replayInserted || replay.ID != first.ID {
					t.Fatalf("replayed usage %d: inserted=%v error=%v", index, replayInserted, errReplay)
				}
			}

			charges, errCharges := repo.ListBillingCharges(ctx, BillingChargeQuery{UserID: &org.ID, Limit: 10})
			if errCharges != nil || charges.Total != 2 || len(charges.Records) != 2 {
				t.Fatalf("organization charges=%+v error=%v", charges, errCharges)
			}
			actors := map[uint]bool{}
			for _, charge := range charges.Records {
				if charge.UserID == nil || *charge.UserID != org.ID || charge.ActorUserID == nil || charge.APIKeyID == nil {
					t.Fatalf("charge owner/actor/key attribution=%+v", charge)
				}
				actors[*charge.ActorUserID] = true
				if charge.BaseAmount != 2 || charge.Amount != 1 || charge.BillingMultiplier != 0.5 {
					t.Fatalf("organization pricing=%+v", charge)
				}
				if tc.unlimited {
					if charge.BalanceBefore != 100 || charge.BalanceAfter != 100 {
						t.Fatalf("unlimited balance snapshot=%+v", charge)
					}
				} else if charge.BalanceBefore-charge.BalanceAfter != 1 {
					t.Fatalf("limited balance snapshot=%+v", charge)
				}
			}
			if !actors[owner.ID] || !actors[member.ID] {
				t.Fatalf("organization actors=%v", actors)
			}
			personalCharges, errPersonal := repo.ListBillingCharges(ctx, BillingChargeQuery{UserID: &member.ID, Limit: 10})
			if errPersonal != nil || len(personalCharges.Records) != 1 {
				t.Fatalf("personal charges=%+v error=%v", personalCharges, errPersonal)
			}
			personalCharge := personalCharges.Records[0]
			if personalCharge.ActorUserID != nil || personalCharge.Amount != 4 || personalCharge.BillingMultiplier != 2 {
				t.Fatalf("personal charge=%+v", personalCharge)
			}
			wantOrgCredits := 98.0
			if tc.unlimited {
				wantOrgCredits = 100
			}
			for userID, want := range map[uint]float64{org.ID: wantOrgCredits, owner.ID: 20, member.ID: 26, other.ID: 40} {
				user, errUser := getBillingOwnerTx(ctx, db, userID)
				if errUser != nil || user.Credits != want {
					t.Fatalf("account %d credits=%+v error=%v, want %v", userID, user, errUser, want)
				}
			}
		})
	}
}

func TestOrganizationUsageChargesAfterMemberLeaves(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	owner := createPeriodLimitUser(t, repo, ctx, "owner", 20)
	member := createPeriodLimitUser(t, repo, ctx, "member", 30)
	org := createOrganizationTestOrg(t, repo, "acme", 100)
	addOrganizationTestMember(t, repo, org.ID, owner.ID, OrganizationRoleOwner)
	addOrganizationTestMember(t, repo, org.ID, member.ID, OrganizationRoleMember)
	key := createOrganizationTestKey(t, repo, org.ID, member.ID, "departed-member-key")
	if _, errPrice := repo.CreateBillingModelPrice(ctx, BillingModelPriceUpdate{
		Provider: "openai", Model: "org-model", InputPricePerMillion: 2, Enabled: true,
	}); errPrice != nil {
		t.Fatal(errPrice)
	}
	if errRemove := repo.RemoveOrganizationMember(ctx, org.ID, member.ID); errRemove != nil {
		t.Fatal(errRemove)
	}
	// An admitted request can finish after membership removal soft-deletes its key.
	payload := fmt.Sprintf(`{"timestamp":"2026-11-22T01:00:00Z","execution_id":"departed-member-execution","provider":"openai","model":"org-model","api_key":%q,"failed":true,"fail":{"status_code":499},"tokens":{"input_tokens":1000000,"total_tokens":1000000}}`, key.APIKey)
	if _, errAppend := repo.AppendUsage(ctx, payload, "192.0.2.10"); errAppend != nil {
		t.Fatal(errAppend)
	}
	charges, errCharges := repo.ListBillingCharges(ctx, BillingChargeQuery{UserID: &org.ID, Limit: 10})
	if errCharges != nil || len(charges.Records) != 1 {
		t.Fatalf("charges=%+v error=%v", charges, errCharges)
	}
	charge := charges.Records[0]
	if charge.ActorUserID == nil || *charge.ActorUserID != member.ID || charge.APIKeyID == nil || *charge.APIKeyID != key.ID || charge.Amount != 2 || charge.BalanceBefore != 100 || charge.BalanceAfter != 98 {
		t.Fatalf("late failed request charge=%+v", charge)
	}
	// Failed requests without consumed tokens must not create charges.
	noTokens := fmt.Sprintf(`{"timestamp":"2026-11-22T01:00:01Z","execution_id":"departed-member-no-tokens","provider":"openai","model":"org-model","api_key":%q,"failed":true,"fail":{"status_code":500}}`, key.APIKey)
	if _, errAppend := repo.AppendUsage(ctx, noTokens, "192.0.2.10"); errAppend != nil {
		t.Fatal(errAppend)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	for _, check := range []struct {
		model any
		want  int64
	}{{&UsageRecord{}, 2}, {&BillingChargeRecord{}, 1}} {
		var count int64
		if errCount := db.Model(check.model).Count(&count).Error; errCount != nil || count != check.want {
			t.Fatalf("%T count=%d error=%v, want %d", check.model, count, errCount, check.want)
		}
	}
	for userID, want := range map[uint]float64{org.ID: 98, owner.ID: 20, member.ID: 30} {
		user, errUser := getBillingOwnerTx(ctx, db, userID)
		if errUser != nil || user.Credits != want {
			t.Fatalf("account %d credits=%+v error=%v, want %v", userID, user, errUser, want)
		}
	}
}
