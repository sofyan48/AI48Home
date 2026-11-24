package cluster

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func TestOrganizationChannelScopeInheritanceAndMutations(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()
	member := createPeriodLimitUser(t, repo, ctx, "member", 0)
	org := createOrganizationTestOrg(t, repo, "scoped", 100)
	addOrganizationTestMember(t, repo, org.ID, member.ID, OrganizationRoleOwner)
	existing := createOrganizationTestKey(t, repo, org.ID, member.ID, "existing")
	personalValue := "personal"
	personal, errPersonal := repo.CreateAPIKeyForUser(ctx, member.ID, APIKeyUserUpdate{APIKey: &personalValue})
	if errPersonal != nil {
		t.Fatal(errPersonal)
	}
	groups := []uint{71, 72}
	if _, errScope := repo.SetOrganizationChannelGroups(ctx, org.ID, []uint{72, 71, 71, 0}); errScope != nil {
		t.Fatal(errScope)
	}
	db, _ := repo.database()
	assertChannels := func(id uint, want []uint) {
		t.Helper()
		var key APIKeyRecord
		if errLoad := db.First(&key, id).Error; errLoad != nil {
			t.Fatal(errLoad)
		}
		got, errChannels := apiKeyChannelsFromJSON(key.Channels)
		if errChannels != nil || !slices.Equal(got, want) {
			t.Fatalf("key %d channels=%v error=%v want=%v", id, got, errChannels, want)
		}
	}
	assertChannels(existing.ID, groups)
	assertChannels(personal.ID, []uint{})
	bypass := []uint{}
	value := "member-key"
	key, errKey := repo.CreateOrganizationAPIKey(ctx, org.ID, member.ID, APIKeyUserUpdate{APIKey: &value, Channels: &bypass})
	if errKey != nil {
		t.Fatal(errKey)
	}
	assertChannels(key.ID, groups)
	admin, errAdmin := repo.CreateAPIKey(ctx, APIKeyEntryUpdate{APIKey: "admin-key", UserID: &org.ID, Channels: &bypass})
	if errAdmin != nil {
		t.Fatal(errAdmin)
	}
	assertChannels(admin.ID, groups)
	for name, mutate := range map[string]func() error{
		"admin-patch": func() error {
			_, errUpdate := repo.UpdateAPIKey(ctx, APIKeySelector{ID: key.ID}, APIKeyAdminUpdate{Channels: &bypass})
			return errUpdate
		},
		"bindings": func() error {
			_, errUpdate := repo.UpdateAPIKeyBindings(ctx, key.APIKey, nil, &bypass, nil)
			return errUpdate
		},
		"user-patch": func() error {
			_, errUpdate := repo.UpdateAPIKeyForUser(ctx, org.ID, key.ID, "", APIKeyUserUpdate{Channels: &bypass})
			return errUpdate
		},
		"duplicate-create": func() error {
			_, errCreate := repo.CreateAPIKeyForUser(ctx, org.ID, APIKeyUserUpdate{APIKey: &value, Channels: &bypass, CreatedByUserID: &member.ID})
			return errCreate
		},
	} {
		t.Run(name, func(t *testing.T) {
			if errMutation := mutate(); errMutation != nil {
				t.Fatal(errMutation)
			}
			assertChannels(key.ID, groups)
		})
	}
	model, errModel := repo.CreateModelGroup(ctx, "model", false)
	if errModel != nil {
		t.Fatal(errModel)
	}
	models := []uint{model.ID}
	if _, errScope := repo.SetOrganizationModelGroups(ctx, org.ID, models); errScope != nil {
		t.Fatal(errScope)
	}
	if _, errMember := repo.UpdateOrganizationMember(ctx, org.ID, member.ID, OrganizationMemberUpdate{ModelGroups: &models}); errMember != nil {
		t.Fatal(errMember)
	}
	assertChannels(key.ID, groups)
	// Reassignment and restoration use the organization policy, not caller channels.
	if _, errAssign := repo.UpdateAPIKey(ctx, APIKeySelector{ID: personal.ID}, APIKeyAdminUpdate{UserID: &org.ID, Channels: &bypass}); errAssign != nil {
		t.Fatal(errAssign)
	}
	assertChannels(personal.ID, groups)
	if errDelete := repo.DeleteAPIKey(ctx, APIKeySelector{ID: admin.ID}); errDelete != nil {
		t.Fatal(errDelete)
	}
	if _, errRestore := repo.CreateAPIKey(ctx, APIKeyEntryUpdate{APIKey: admin.APIKey, UserID: &org.ID, Channels: &bypass}); errRestore != nil {
		t.Fatal(errRestore)
	}
	assertChannels(admin.ID, groups)
	// Generic full replacement must not clear scope even with an explicit empty binding.
	if _, errReplace := repo.ReplaceAPIKeyEntries(ctx, []APIKeyEntryUpdate{{ID: key.ID, APIKey: key.APIKey, Channels: &bypass}}); errReplace != nil {
		t.Fatal(errReplace)
	}
	assertChannels(key.ID, groups)
	// Runtime resolution must also enforce policy over stale/imported bindings.
	if errStale := db.Model(&APIKeyRecord{}).Where("id = ?", key.ID).Update("channels", JSONB(`[]`)).Error; errStale != nil {
		t.Fatal(errStale)
	}
	effective, errEffective := effectiveAPIKeyChannels(ctx, db, key)
	if errEffective != nil || !reflect.DeepEqual(effective, groups) {
		t.Fatalf("effective=%v error=%v", effective, errEffective)
	}
	// Unknown scopes fail closed through both dispatch and catalog resolution.
	authIDs, _, errDispatch := allowedModelDispatchIDsForAPIKeyRecord(ctx, db, key, "model")
	if errDispatch != nil || authIDs == nil || len(authIDs) != 0 {
		t.Fatalf("dispatch auth IDs=%v error=%v", authIDs, errDispatch)
	}
	policy, errPolicy := repo.ModelAccessForAPIKey(ctx, key.APIKey)
	if errPolicy != nil || policy.Allows("outside", "model") {
		t.Fatalf("catalog did not enforce organization scope: %v", errPolicy)
	}
	if _, errClear := repo.SetOrganizationChannelGroups(ctx, org.ID, nil); errClear != nil {
		t.Fatal(errClear)
	}
	assertChannels(key.ID, []uint{})
}

func TestOrganizationChannelScopeMigrationAndSnapshot(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()
	org := createOrganizationTestOrg(t, repo, "legacy", 100)
	db, _ := repo.database()
	if errDrop := db.Migrator().DropColumn(&OrganizationRecord{}, "organization_channel_groups"); errDrop != nil {
		t.Fatal(errDrop)
	}
	if currentDatabaseVersion <= 28 {
		t.Fatal("migration gate must advance past v28")
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	restored, errOrg := repo.GetOrganization(ctx, org.ID)
	if errOrg != nil {
		t.Fatal(errOrg)
	}
	groups, errGroups := OrganizationChannelGroupsOf(restored)
	if errGroups != nil || len(groups) != 0 {
		t.Fatalf("legacy scope=%v error=%v", groups, errGroups)
	}
	if db.Migrator().HasColumn(&UserRecord{}, "organization_channel_groups") {
		t.Fatal("organization scope leaked into user storage")
	}
	for _, version := range []int{28, currentDatabaseVersion} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			if version == currentDatabaseVersion {
				if _, errScope := repo.SetOrganizationChannelGroups(ctx, org.ID, []uint{42}); errScope != nil {
					t.Fatal(errScope)
				}
			}
			path := filepath.Join(t.TempDir(), "snapshot.zip")
			if _, errExport := exportDatabaseSnapshotVersion(ctx, db, path, version); errExport != nil {
				t.Fatal(errExport)
			}
			snapshot, errOpen := OpenDatabaseSnapshot(ctx, path)
			if errOpen != nil {
				t.Fatal(errOpen)
			}
			defer func() {
				if errClose := snapshot.Close(); errClose != nil {
					t.Error(errClose)
				}
			}()
			target := openDatabaseSnapshotSQLiteRawTestDB(t, filepath.Join(t.TempDir(), "target.db"))
			if _, errImport := ImportDatabaseSnapshot(ctx, target, snapshot, nil); errImport != nil {
				t.Fatal(errImport)
			}
			var actual OrganizationRecord
			if errLoad := target.First(&actual, org.ID).Error; errLoad != nil {
				t.Fatal(errLoad)
			}
			want := []uint{}
			if version == currentDatabaseVersion {
				want = []uint{42}
			}
			got, errScope := OrganizationChannelGroupsOf(actual.userRecord())
			if errScope != nil || !slices.Equal(got, want) {
				t.Fatalf("snapshot scope=%v error=%v want=%v", got, errScope, want)
			}
		})
	}
}

func TestOrganizationChannelScopeDeletedGroup(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()
	group, errGroup := repo.CreateChannelGroup(ctx, "credentials", false)
	if errGroup != nil {
		t.Fatal(errGroup)
	}
	scope := []uint{group.ID}
	name := "scoped"
	org, errOrg := repo.CreateOrganizationWithSetup(ctx, UserUpdate{Username: &name}, OrganizationSetup{ChannelGroups: &scope})
	if errOrg != nil {
		t.Fatal(errOrg)
	}
	if errDelete := repo.DeleteChannelGroup(ctx, group.ID); errDelete != nil {
		t.Fatal(errDelete)
	}
	org, errOrg = repo.GetOrganization(ctx, org.ID)
	if errOrg != nil {
		t.Fatal(errOrg)
	}
	got, errScope := OrganizationChannelGroupsOf(org)
	if errScope != nil || len(got) != 0 {
		t.Fatalf("deleted group scope=%v error=%v", got, errScope)
	}
	if _, errSet := repo.SetOrganizationChannelGroups(ctx, org.ID, scope); !errors.Is(errSet, ErrDeletedAccessGroup) {
		t.Fatalf("deleted scope error=%v", errSet)
	}
	name = "must-rollback"
	if _, errCreate := repo.CreateOrganizationWithSetup(ctx, UserUpdate{Username: &name}, OrganizationSetup{ChannelGroups: &scope}); !errors.Is(errCreate, ErrDeletedAccessGroup) {
		t.Fatalf("deleted create scope error=%v", errCreate)
	}
	orgs, errList := repo.ListOrganizations(ctx)
	if errList != nil || len(orgs) != 1 {
		t.Fatalf("failed create left organization: %v %v", orgs, errList)
	}
}
