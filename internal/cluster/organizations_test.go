package cluster

import (
	"errors"
	"strings"
	"testing"
	"time"

	homeerrors "github.com/router-for-me/CLIProxyAPIHome/internal/errors"
)

func createOrganizationTestOrg(t *testing.T, repo *Repository, name string, credits float64) *UserRecord {
	t.Helper()
	org, errCreate := repo.CreateOrganization(t.Context(), UserUpdate{Username: &name, Credits: &credits})
	if errCreate != nil {
		t.Fatalf("CreateOrganization() error = %v", errCreate)
	}
	return org
}

func addOrganizationTestMember(t *testing.T, repo *Repository, orgID, userID uint, role string) {
	t.Helper()
	if _, errAdd := repo.AddOrganizationMember(t.Context(), orgID, userID, OrganizationMemberUpdate{Role: &role}); errAdd != nil {
		t.Fatalf("AddOrganizationMember() error = %v", errAdd)
	}
}

func createOrganizationTestKey(t *testing.T, repo *Repository, orgID, memberID uint, value string) *APIKeyRecord {
	t.Helper()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatalf("database() error = %v", errDB)
	}
	key := &APIKeyRecord{APIKey: value, UserID: &orgID, CreatedByUserID: &memberID, Channels: emptyAPIKeyChannelsJSON(), ModelGroups: emptyAPIKeyModelGroupsJSON()}
	if errCreate := db.Create(key).Error; errCreate != nil {
		t.Fatalf("create key error = %v", errCreate)
	}
	return key
}

func TestOrganizationAccountsAreSeparatedFromUsers(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	createPeriodLimitUser(t, repo, ctx, "alice", 1)
	org := createOrganizationTestOrg(t, repo, "acme", 10)
	if !org.IsOrganization() || org.Password != "" || org.ApprovalPending {
		t.Fatalf("organization = %+v, want passwordless approved organization", org)
	}

	users, errUsers := repo.ListUsers(ctx)
	if errUsers != nil {
		t.Fatalf("ListUsers() error = %v", errUsers)
	}
	if len(users) != 1 || users[0].Username != "alice" {
		t.Fatalf("ListUsers() = %+v, want only alice", users)
	}
	orgs, errOrgs := repo.ListOrganizations(ctx)
	if errOrgs != nil || len(orgs) != 1 || orgs[0].ID != org.ID {
		t.Fatalf("ListOrganizations() = %+v, %v", orgs, errOrgs)
	}
}

func TestOrganizationMembershipRules(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	owner := createPeriodLimitUser(t, repo, ctx, "owner", 0)
	member := createPeriodLimitUser(t, repo, ctx, "member", 0)
	org := createOrganizationTestOrg(t, repo, "acme", 10)
	other := createOrganizationTestOrg(t, repo, "other", 10)

	addOrganizationTestMember(t, repo, org.ID, owner.ID, OrganizationRoleOwner)
	addOrganizationTestMember(t, repo, org.ID, member.ID, OrganizationRoleMember)
	// One user may belong to several organizations.
	addOrganizationTestMember(t, repo, other.ID, member.ID, OrganizationRoleOwner)

	if _, errDup := repo.AddOrganizationMember(ctx, org.ID, member.ID, OrganizationMemberUpdate{}); !errors.Is(errDup, ErrOrganizationMemberExists) {
		t.Fatalf("duplicate add error = %v, want ErrOrganizationMemberExists", errDup)
	}
	if _, errNested := repo.AddOrganizationMember(ctx, org.ID, other.ID, OrganizationMemberUpdate{}); !errors.Is(errNested, ErrOrganizationInvalidMember) {
		t.Fatalf("nested org add error = %v, want ErrOrganizationInvalidMember", errNested)
	}
	bad := "admin"
	if _, errRole := repo.AddOrganizationMember(ctx, org.ID, member.ID, OrganizationMemberUpdate{Role: &bad}); !errors.Is(errRole, ErrOrganizationInvalidRole) {
		t.Fatalf("invalid role error = %v, want ErrOrganizationInvalidRole", errRole)
	}

	memberships, errMemberships := repo.ListUserOrganizations(ctx, member.ID)
	if errMemberships != nil || len(memberships) != 2 || memberships[0].OrganizationName != "acme" {
		t.Fatalf("ListUserOrganizations() = %+v, %v", memberships, errMemberships)
	}

	demote := OrganizationRoleMember
	if _, errDemote := repo.UpdateOrganizationMember(ctx, org.ID, owner.ID, OrganizationMemberUpdate{Role: &demote}); !errors.Is(errDemote, ErrOrganizationLastOwner) {
		t.Fatalf("demote last owner error = %v, want ErrOrganizationLastOwner", errDemote)
	}
	if errRemove := repo.RemoveOrganizationMember(ctx, org.ID, owner.ID); !errors.Is(errRemove, ErrOrganizationLastOwner) {
		t.Fatalf("remove last owner error = %v, want ErrOrganizationLastOwner", errRemove)
	}

	promote := OrganizationRoleOwner
	if _, errPromote := repo.UpdateOrganizationMember(ctx, org.ID, member.ID, OrganizationMemberUpdate{Role: &promote}); errPromote != nil {
		t.Fatalf("promote error = %v", errPromote)
	}
	if _, errDemote := repo.UpdateOrganizationMember(ctx, org.ID, owner.ID, OrganizationMemberUpdate{Role: &demote}); errDemote != nil {
		t.Fatalf("demote with another owner error = %v", errDemote)
	}
}

func TestRemovingOrganizationMemberDeletesTheirKeys(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	owner := createPeriodLimitUser(t, repo, ctx, "owner", 0)
	member := createPeriodLimitUser(t, repo, ctx, "member", 0)
	org := createOrganizationTestOrg(t, repo, "acme", 10)
	addOrganizationTestMember(t, repo, org.ID, owner.ID, OrganizationRoleOwner)
	addOrganizationTestMember(t, repo, org.ID, member.ID, OrganizationRoleMember)

	memberKey := createOrganizationTestKey(t, repo, org.ID, member.ID, "sk-org-member")
	ownerKey := createOrganizationTestKey(t, repo, org.ID, owner.ID, "sk-org-owner")
	personalKey, errPersonal := repo.CreateAPIKeyForUser(ctx, member.ID, APIKeyUserUpdate{APIKey: stringPtr("sk-personal")})
	if errPersonal != nil {
		t.Fatalf("CreateAPIKeyForUser() error = %v", errPersonal)
	}

	if errRemove := repo.RemoveOrganizationMember(ctx, org.ID, member.ID); errRemove != nil {
		t.Fatalf("RemoveOrganizationMember() error = %v", errRemove)
	}
	assertOrganizationKeyExists(t, repo, memberKey.APIKey, false)
	assertOrganizationKeyExists(t, repo, ownerKey.APIKey, true)
	assertOrganizationKeyExists(t, repo, personalKey.APIKey, true)
}

func TestDeletingUserRemovesOrganizationMembershipAndKeys(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	owner := createPeriodLimitUser(t, repo, ctx, "owner", 0)
	member := createPeriodLimitUser(t, repo, ctx, "member", 0)
	org := createOrganizationTestOrg(t, repo, "acme", 10)
	addOrganizationTestMember(t, repo, org.ID, owner.ID, OrganizationRoleOwner)
	addOrganizationTestMember(t, repo, org.ID, member.ID, OrganizationRoleMember)
	memberKey := createOrganizationTestKey(t, repo, org.ID, member.ID, "sk-org-member")

	if errDelete := repo.DeleteUser(ctx, member.ID); errDelete != nil {
		t.Fatalf("DeleteUser() error = %v", errDelete)
	}
	assertOrganizationKeyExists(t, repo, memberKey.APIKey, false)
	if _, errMember := repo.GetOrganizationMember(ctx, org.ID, member.ID); !errors.Is(errMember, ErrOrganizationMemberNotFound) {
		t.Fatalf("GetOrganizationMember() error = %v, want ErrOrganizationMemberNotFound", errMember)
	}

	if errDeleteOrg := repo.DeleteOrganization(ctx, org.ID); errDeleteOrg != nil {
		t.Fatalf("DeleteOrganization() error = %v", errDeleteOrg)
	}
	members, errMembers := repo.ListUserOrganizations(ctx, owner.ID)
	if errMembers != nil || len(members) != 0 {
		t.Fatalf("ListUserOrganizations() after org delete = %+v, %v", members, errMembers)
	}
}

func TestOrganizationMemberSpendLimitBlocksOnlyThatMember(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	owner := createPeriodLimitUser(t, repo, ctx, "owner", 0)
	member := createPeriodLimitUser(t, repo, ctx, "member", 0)
	org := createOrganizationTestOrg(t, repo, "acme", 100)
	addOrganizationTestMember(t, repo, org.ID, owner.ID, OrganizationRoleOwner)
	addOrganizationTestMember(t, repo, org.ID, member.ID, OrganizationRoleMember)
	if _, errLimit := repo.UpdateOrganizationMember(ctx, org.ID, member.ID, OrganizationMemberUpdate{
		Limit1dCredits: OptionalFloatUpdate{Set: true, Value: 5},
	}); errLimit != nil {
		t.Fatalf("UpdateOrganizationMember() error = %v", errLimit)
	}
	memberKey := createOrganizationTestKey(t, repo, org.ID, member.ID, "sk-org-member")
	ownerKey := createOrganizationTestKey(t, repo, org.ID, owner.ID, "sk-org-owner")

	db, _ := repo.database()
	if errAllowed := ensureAPIKeyUserBillingAllowed(ctx, db, memberKey); errAllowed != nil {
		t.Fatalf("member admission before spend error = %v", errAllowed)
	}

	createOrganizationTestCharge(t, repo, org.ID, member.ID, 5, time.Now().Add(-time.Hour))
	// Charges outside the rolling day count toward 7d/30d only.
	createOrganizationTestCharge(t, repo, org.ID, member.ID, 50, time.Now().Add(-48*time.Hour))

	errBlocked := ensureAPIKeyUserBillingAllowed(ctx, db, memberKey)
	if errBlocked == nil || !strings.HasPrefix(errBlocked.Error(), homeerrors.TypeUserPeriodLimitExceeded+":") {
		t.Fatalf("member admission after spend error = %v, want %s", errBlocked, homeerrors.TypeUserPeriodLimitExceeded)
	}
	if errOwner := ensureAPIKeyUserBillingAllowed(ctx, db, ownerKey); errOwner != nil {
		t.Fatalf("owner admission error = %v, want allowed", errOwner)
	}

	windows, errSpend := repo.OrganizationMemberSpend(ctx, org.ID, member.ID, time.Now())
	if errSpend != nil {
		t.Fatalf("OrganizationMemberSpend() error = %v", errSpend)
	}
	if windows[0].Used != 5 || windows[0].Remaining == nil || *windows[0].Remaining != 0 || windows[2].Used != 55 {
		t.Fatalf("spend windows = %+v", windows)
	}
}

func TestOrganizationKeyOfRemovedMemberIsRejected(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	member := createPeriodLimitUser(t, repo, ctx, "member", 0)
	org := createOrganizationTestOrg(t, repo, "acme", 100)
	// Simulate a key that outlived its membership row.
	key := createOrganizationTestKey(t, repo, org.ID, member.ID, "sk-stale")
	db, _ := repo.database()
	if errAllowed := ensureAPIKeyUserBillingAllowed(ctx, db, key); errAllowed == nil || !strings.HasPrefix(errAllowed.Error(), "permission_denied:") {
		t.Fatalf("stale key admission error = %v, want permission_denied", errAllowed)
	}
}

func TestOrganizationChargeRecordsActor(t *testing.T) {
	member := uint(7)
	if got := billingAPIKeyActorUserID(&APIKeyRecord{CreatedByUserID: &member}); got == nil || *got != member {
		t.Fatalf("billingAPIKeyActorUserID() = %v, want %d", got, member)
	}
	if got := billingAPIKeyActorUserID(&APIKeyRecord{}); got != nil {
		t.Fatalf("billingAPIKeyActorUserID(personal) = %v, want nil", got)
	}
}

func createOrganizationTestCharge(t *testing.T, repo *Repository, orgID, memberID uint, amount float64, at time.Time) {
	t.Helper()
	db, _ := repo.database()
	org, actor := orgID, memberID
	record := &BillingChargeRecord{
		ID:            billingID("charge"),
		UsageID:       uint(at.UnixNano() % 1_000_000_000),
		PayloadHash:   billingID("hash"),
		UserID:        &org,
		ActorUserID:   &actor,
		Provider:      "test",
		Model:         "test-model",
		Amount:        amount,
		PriceSnapshot: JSONB(`{}`),
		CreatedAt:     at.UTC(),
	}
	if errCreate := db.Create(record).Error; errCreate != nil {
		t.Fatalf("create charge error = %v", errCreate)
	}
}

func assertOrganizationKeyExists(t *testing.T, repo *Repository, value string, want bool) {
	t.Helper()
	db, _ := repo.database()
	var count int64
	if errCount := db.Model(&APIKeyRecord{}).Where("api_key = ?", value).Count(&count).Error; errCount != nil {
		t.Fatalf("count key error = %v", errCount)
	}
	if (count == 1) != want {
		t.Fatalf("key %s exists = %v, want %v", value, count == 1, want)
	}
}
