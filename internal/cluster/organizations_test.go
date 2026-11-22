package cluster

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	homeerrors "github.com/router-for-me/CLIProxyAPIHome/internal/errors"
	"gorm.io/gorm"
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
	// Windows are ordered 5h, 1d, 7d, 30d.
	if len(windows) != 4 || windows[1].Used != 5 || windows[1].Remaining == nil || *windows[1].Remaining != 0 || windows[3].Used != 55 {
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

func TestOrganizationModelGroupsMigrationRepairsMissingColumn(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	// Reproduce a database migrated by the initial organization release.
	if errDrop := db.Migrator().DropColumn(&UserRecord{}, "organization_model_groups"); errDrop != nil {
		t.Fatal(errDrop)
	}
	if currentDatabaseVersion <= 23 {
		t.Fatal("organization model-group migration must advance the PostgreSQL gate past v23")
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	if !db.Migrator().HasColumn(&UserRecord{}, "organization_model_groups") {
		t.Fatal("migration did not restore organization_model_groups")
	}
	name := "personal-after-migration"
	user, errCreate := repo.CreateUser(ctx, UserUpdate{Username: &name})
	if errCreate != nil {
		t.Fatalf("personal user creation after migration: %v", errCreate)
	}
	if user.IsOrganization() {
		t.Fatal("personal user became an organization")
	}
	if _, supported := databaseSnapshotModels(23); !supported {
		t.Fatal("v23 snapshots must remain importable")
	}
}

func TestOrganizationMemberWindowModes(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	member := createPeriodLimitUser(t, repo, ctx, "member", 0)
	org := createOrganizationTestOrg(t, repo, "acme", 100)
	firstUse := PeriodWindowModeFirstUse
	calendar := PeriodWindowModeCalendar
	record, errAdd := repo.AddOrganizationMember(ctx, org.ID, member.ID, OrganizationMemberUpdate{
		Limit5hCredits: OptionalFloatUpdate{Set: true, Value: 3}, WindowMode5h: &firstUse,
		Limit7dCredits: OptionalFloatUpdate{Set: true, Value: 10}, WindowMode7d: &calendar,
	})
	if errAdd != nil {
		t.Fatalf("AddOrganizationMember() error = %v", errAdd)
	}
	if record.WindowMode5h != PeriodWindowModeFirstUse || record.WindowMode7d != PeriodWindowModeCalendar || record.WindowMode1d != PeriodWindowModeSliding {
		t.Fatalf("window modes = %q/%q/%q", record.WindowMode5h, record.WindowMode1d, record.WindowMode7d)
	}
	if _, errCal := repo.UpdateOrganizationMember(ctx, org.ID, member.ID, OrganizationMemberUpdate{WindowMode5h: &calendar}); errCal == nil {
		t.Fatal("calendar mode accepted for 5h window")
	}

	key := createOrganizationTestKey(t, repo, org.ID, member.ID, "sk-org-windows")
	db, _ := repo.database()
	// A charge from before the first_use session opened does not count toward it.
	createOrganizationTestCharge(t, repo, org.ID, member.ID, 3, time.Now().Add(-time.Hour))
	if errAllowed := ensureAPIKeyUserBillingAllowed(ctx, db, key); errAllowed != nil {
		t.Fatalf("admission with closed first_use window error = %v", errAllowed)
	}
	if errOpen := db.Transaction(func(tx *gorm.DB) error {
		return openOrganizationMemberFirstUseWindowsOnChargeTx(ctx, tx, org.ID, member.ID, time.Now().Add(-30*time.Minute))
	}); errOpen != nil {
		t.Fatalf("open first_use window error = %v", errOpen)
	}
	createOrganizationTestCharge(t, repo, org.ID, member.ID, 3, time.Now().Add(-10*time.Minute))
	errBlocked := ensureAPIKeyUserBillingAllowed(ctx, db, key)
	if errBlocked == nil || !strings.Contains(errBlocked.Error(), "window=5h mode=first_use") {
		t.Fatalf("admission after first_use spend error = %v, want 5h first_use block", errBlocked)
	}
	windows, errSpend := repo.OrganizationMemberSpend(ctx, org.ID, member.ID, time.Now())
	if errSpend != nil || len(windows) != 4 || windows[0].ID != PeriodWindow5h || !windows[0].Active || windows[0].ResetAt == nil {
		t.Fatalf("spend windows = %+v, %v", windows, errSpend)
	}

	// Changing a mode resets first_use state.
	sliding := PeriodWindowModeSliding
	updated, errUpdate := repo.UpdateOrganizationMember(ctx, org.ID, member.ID, OrganizationMemberUpdate{WindowMode5h: &sliding})
	if errUpdate != nil || updated.PeriodWindowStart5h != nil {
		t.Fatalf("mode change = %+v, %v; want cleared window start", updated, errUpdate)
	}
}

func TestOrganizationMemberLimitCannotExceedOrganizationLimit(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	member := createPeriodLimitUser(t, repo, ctx, "member", 0)
	org := createOrganizationTestOrg(t, repo, "acme", 100)
	if _, errOrg := repo.UpdateUser(ctx, org.ID, UserUpdate{Limit1dCredits: OptionalFloatUpdate{Set: true, Value: 20}}); errOrg != nil {
		t.Fatalf("set organization limit error = %v", errOrg)
	}
	_, errAdd := repo.AddOrganizationMember(ctx, org.ID, member.ID, OrganizationMemberUpdate{Limit1dCredits: OptionalFloatUpdate{Set: true, Value: 25}})
	var configErr PeriodLimitConfigError
	if !errors.As(errAdd, &configErr) || configErr.Code != PeriodLimitErrExceedsOrganization || configErr.Field != "limit_1d_credits" {
		t.Fatalf("add above organization limit error = %v, want %s", errAdd, PeriodLimitErrExceedsOrganization)
	}
	if _, errEqual := repo.AddOrganizationMember(ctx, org.ID, member.ID, OrganizationMemberUpdate{Limit1dCredits: OptionalFloatUpdate{Set: true, Value: 20}}); errEqual != nil {
		t.Fatalf("add at organization limit error = %v", errEqual)
	}
}

func TestOrganizationMemberKeysToggle(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	org := createOrganizationTestOrg(t, repo, "acme", 100)
	if org.OrganizationMemberKeysDisabled {
		t.Fatal("new organizations must allow member keys by default")
	}
	updated, errSet := repo.SetOrganizationMemberKeysAllowed(ctx, org.ID, false)
	if errSet != nil || !updated.OrganizationMemberKeysDisabled {
		t.Fatalf("SetOrganizationMemberKeysAllowed(false) = %+v, %v", updated, errSet)
	}
	personal := createPeriodLimitUser(t, repo, ctx, "personal", 0)
	if _, errPersonal := repo.SetOrganizationMemberKeysAllowed(ctx, personal.ID, false); !errors.Is(errPersonal, ErrOrganizationNotFound) {
		t.Fatalf("toggle on personal user error = %v, want ErrOrganizationNotFound", errPersonal)
	}
}

func TestOrganizationMemberModelGroupSubset(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	cheap, _ := repo.CreateModelGroup(ctx, "cheap", false)
	premium, _ := repo.CreateModelGroup(ctx, "premium", false)
	outside, _ := repo.CreateModelGroup(ctx, "outside", false)
	member := createPeriodLimitUser(t, repo, ctx, "member", 0)
	other := createPeriodLimitUser(t, repo, ctx, "other", 0)
	org := createOrganizationTestOrg(t, repo, "acme", 100)
	if _, errScope := repo.SetOrganizationModelGroups(ctx, org.ID, []uint{cheap.ID, premium.ID}); errScope != nil {
		t.Fatalf("SetOrganizationModelGroups() error = %v", errScope)
	}

	bad := []uint{outside.ID}
	if _, errAdd := repo.AddOrganizationMember(ctx, org.ID, member.ID, OrganizationMemberUpdate{ModelGroups: &bad}); !errors.Is(errAdd, ErrOrganizationMemberModelGroups) {
		t.Fatalf("subset outside organization error = %v", errAdd)
	}
	subset := []uint{cheap.ID}
	if _, errAdd := repo.AddOrganizationMember(ctx, org.ID, member.ID, OrganizationMemberUpdate{ModelGroups: &subset}); errAdd != nil {
		t.Fatalf("AddOrganizationMember(subset) error = %v", errAdd)
	}
	addOrganizationTestMember(t, repo, org.ID, other.ID, OrganizationRoleMember)

	value := "sk-member-subset"
	memberKey, errKey := repo.CreateOrganizationAPIKey(ctx, org.ID, member.ID, APIKeyUserUpdate{APIKey: &value})
	if errKey != nil {
		t.Fatalf("CreateOrganizationAPIKey(member) error = %v", errKey)
	}
	assertKeyModelGroups(t, memberKey, []uint{cheap.ID})
	otherValue := "sk-other-org"
	otherKey, errOther := repo.CreateOrganizationAPIKey(ctx, org.ID, other.ID, APIKeyUserUpdate{APIKey: &otherValue})
	if errOther != nil {
		t.Fatalf("CreateOrganizationAPIKey(other) error = %v", errOther)
	}
	assertKeyModelGroups(t, otherKey, []uint{cheap.ID, premium.ID})

	// Widening the member subset rewrites their existing keys only.
	wider := []uint{cheap.ID, premium.ID}
	if _, errUpdate := repo.UpdateOrganizationMember(ctx, org.ID, member.ID, OrganizationMemberUpdate{ModelGroups: &wider}); errUpdate != nil {
		t.Fatalf("UpdateOrganizationMember(wider) error = %v", errUpdate)
	}
	assertStoredKeyModelGroups(t, repo, value, []uint{cheap.ID, premium.ID})

	// Shrinking the organization scope trims member subsets and their keys.
	if _, errScope := repo.SetOrganizationModelGroups(ctx, org.ID, []uint{premium.ID}); errScope != nil {
		t.Fatalf("shrink organization scope error = %v", errScope)
	}
	stored, _ := repo.GetOrganizationMember(ctx, org.ID, member.ID)
	if got := OrganizationMemberModelGroupsOf(stored); len(got) != 1 || got[0] != premium.ID {
		t.Fatalf("trimmed subset = %v, want [%d]", got, premium.ID)
	}
	assertStoredKeyModelGroups(t, repo, value, []uint{premium.ID})
	assertStoredKeyModelGroups(t, repo, otherValue, []uint{premium.ID})

	// Removing every group of a restricted member would widen them, so it is rejected.
	if _, errScope := repo.SetOrganizationModelGroups(ctx, org.ID, []uint{cheap.ID}); !errors.Is(errScope, ErrOrganizationModelGroupsConflict) {
		t.Fatalf("conflicting scope error = %v, want ErrOrganizationModelGroupsConflict", errScope)
	}
	assertStoredKeyModelGroups(t, repo, value, []uint{premium.ID})

	// Clearing the subset inherits the organization scope again.
	none := []uint{}
	if _, errClear := repo.UpdateOrganizationMember(ctx, org.ID, member.ID, OrganizationMemberUpdate{ModelGroups: &none}); errClear != nil {
		t.Fatalf("clear subset error = %v", errClear)
	}
	assertStoredKeyModelGroups(t, repo, value, []uint{premium.ID})
}

func TestOrganizationMemberMaxAPIKeys(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	member := createPeriodLimitUser(t, repo, ctx, "member", 0)
	org := createOrganizationTestOrg(t, repo, "acme", 100)
	if _, errAdd := repo.AddOrganizationMember(ctx, org.ID, member.ID, OrganizationMemberUpdate{MaxAPIKeys: OptionalIntUpdate{Set: true, Value: 1}}); errAdd != nil {
		t.Fatalf("AddOrganizationMember() error = %v", errAdd)
	}
	first, second := "sk-cap-1", "sk-cap-2"
	if _, errFirst := repo.CreateOrganizationAPIKey(ctx, org.ID, member.ID, APIKeyUserUpdate{APIKey: &first}); errFirst != nil {
		t.Fatalf("first key error = %v", errFirst)
	}
	if _, errSecond := repo.CreateOrganizationAPIKey(ctx, org.ID, member.ID, APIKeyUserUpdate{APIKey: &second}); !errors.Is(errSecond, ErrOrganizationMemberKeyLimit) {
		t.Fatalf("second key error = %v, want ErrOrganizationMemberKeyLimit", errSecond)
	}
	// Personal keys never count toward the organization cap.
	personal := "sk-personal-cap"
	if _, errPersonal := repo.CreateAPIKeyForUser(ctx, member.ID, APIKeyUserUpdate{APIKey: &personal}); errPersonal != nil {
		t.Fatalf("personal key error = %v", errPersonal)
	}
	counts, errCounts := repo.OrganizationMemberKeyCounts(ctx, org.ID)
	if errCounts != nil || counts[member.ID] != 1 {
		t.Fatalf("key counts = %v, %v", counts, errCounts)
	}
	if _, errNeg := repo.UpdateOrganizationMember(ctx, org.ID, member.ID, OrganizationMemberUpdate{MaxAPIKeys: OptionalIntUpdate{Set: true, Value: -1}}); !errors.Is(errNeg, ErrOrganizationInvalidKeyLimit) {
		t.Fatalf("negative cap error = %v", errNeg)
	}
	if _, errClear := repo.UpdateOrganizationMember(ctx, org.ID, member.ID, OrganizationMemberUpdate{MaxAPIKeys: OptionalIntUpdate{Set: true, Clear: true}}); errClear != nil {
		t.Fatalf("clear cap error = %v", errClear)
	}
	if _, errSecond := repo.CreateOrganizationAPIKey(ctx, org.ID, member.ID, APIKeyUserUpdate{APIKey: &second}); errSecond != nil {
		t.Fatalf("key after clearing cap error = %v", errSecond)
	}
}

func TestDeletingLastOwnerAccountIsBlocked(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	owner := createPeriodLimitUser(t, repo, ctx, "owner", 0)
	second := createPeriodLimitUser(t, repo, ctx, "second", 0)
	org := createOrganizationTestOrg(t, repo, "acme", 10)
	addOrganizationTestMember(t, repo, org.ID, owner.ID, OrganizationRoleOwner)

	errDelete := repo.DeleteUser(ctx, owner.ID)
	if !errors.Is(errDelete, ErrOrganizationLastOwnerAccount) || !strings.Contains(errDelete.Error(), `"acme"`) {
		t.Fatalf("delete last owner error = %v, want ErrOrganizationLastOwnerAccount naming acme", errDelete)
	}
	if _, errStill := repo.GetUser(ctx, owner.ID); errStill != nil {
		t.Fatalf("owner was deleted despite the guard: %v", errStill)
	}

	// With a second owner the account can be deleted.
	addOrganizationTestMember(t, repo, org.ID, second.ID, OrganizationRoleOwner)
	if errDelete := repo.DeleteUser(ctx, owner.ID); errDelete != nil {
		t.Fatalf("delete owner with co-owner error = %v", errDelete)
	}
	if _, errMember := repo.GetOrganizationMember(ctx, org.ID, second.ID); errMember != nil {
		t.Fatalf("co-owner membership lost: %v", errMember)
	}
}

func TestDeleteUserOnOrganizationDeletesItsKeys(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	owner := createPeriodLimitUser(t, repo, ctx, "owner", 0)
	org := createOrganizationTestOrg(t, repo, "acme", 10)
	addOrganizationTestMember(t, repo, org.ID, owner.ID, OrganizationRoleOwner)
	key := createOrganizationTestKey(t, repo, org.ID, owner.ID, "sk-org-cascade")
	if errDelete := repo.DeleteUser(ctx, org.ID); errDelete != nil {
		t.Fatalf("DeleteUser(organization) error = %v", errDelete)
	}
	assertOrganizationKeyExists(t, repo, key.APIKey, false)
	if memberships, _ := repo.ListUserOrganizations(ctx, owner.ID); len(memberships) != 0 {
		t.Fatalf("memberships after organization delete = %+v", memberships)
	}
}

func TestCreateOrganizationWithSetupIsAtomic(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()

	owner := createPeriodLimitUser(t, repo, ctx, "owner", 0)
	group, _ := repo.CreateModelGroup(ctx, "scope", false)
	name, missing := "broken", uint(999999)
	if _, errCreate := repo.CreateOrganizationWithSetup(ctx, UserUpdate{Username: &name}, OrganizationSetup{OwnerUserID: &missing}); errCreate == nil {
		t.Fatal("create with missing owner succeeded")
	}
	nested := createOrganizationTestOrg(t, repo, "nested", 0)
	if _, errCreate := repo.CreateOrganizationWithSetup(ctx, UserUpdate{Username: &name}, OrganizationSetup{OwnerUserID: &nested.ID}); !errors.Is(errCreate, ErrOrganizationInvalidMember) {
		t.Fatalf("create with organization owner error = %v", errCreate)
	}
	if orgs, _ := repo.ListOrganizations(ctx); len(orgs) != 1 {
		t.Fatalf("failed creates left organizations behind: %+v", orgs)
	}

	groups, allow := []uint{group.ID}, false
	org, errCreate := repo.CreateOrganizationWithSetup(ctx, UserUpdate{Username: &name}, OrganizationSetup{OwnerUserID: &owner.ID, ModelGroups: &groups, AllowMemberKeys: &allow})
	if errCreate != nil {
		t.Fatalf("CreateOrganizationWithSetup() error = %v", errCreate)
	}
	if got := OrganizationModelGroupsOfMust(t, org); len(got) != 1 || got[0] != group.ID || !org.OrganizationMemberKeysDisabled {
		t.Fatalf("organization setup = groups %v, member keys disabled %v", got, org.OrganizationMemberKeysDisabled)
	}
	if member, errMember := repo.GetOrganizationMember(ctx, org.ID, owner.ID); errMember != nil || member.Role != OrganizationRoleOwner {
		t.Fatalf("first owner = %+v, %v", member, errMember)
	}
}

func OrganizationModelGroupsOfMust(t *testing.T, org *UserRecord) []uint {
	t.Helper()
	groups, errGroups := OrganizationModelGroupsOf(org)
	if errGroups != nil {
		t.Fatal(errGroups)
	}
	return groups
}

func assertKeyModelGroups(t *testing.T, record *APIKeyRecord, want []uint) {
	t.Helper()
	got, errGroups := apiKeyModelGroupsFromJSON(record.ModelGroups)
	if errGroups != nil || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("key %s model groups = %v (%v), want %v", record.APIKey, got, errGroups, want)
	}
}

func assertStoredKeyModelGroups(t *testing.T, repo *Repository, value string, want []uint) {
	t.Helper()
	db, _ := repo.database()
	record := &APIKeyRecord{}
	if errFind := db.Where("api_key = ?", value).First(record).Error; errFind != nil {
		t.Fatalf("load key %s: %v", value, errFind)
	}
	assertKeyModelGroups(t, record, want)
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
