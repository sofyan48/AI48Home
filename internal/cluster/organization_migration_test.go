package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func seedOrganizationMigrationUsers(t *testing.T, db *gorm.DB) []UserRecord {
	t.Helper()
	now := time.Date(2026, 5, 4, 3, 2, 1, 123456000, time.FixedZone("migration-test", 7*60*60))
	zero := 0.0
	multiplier := 1.75
	email := "must-not-copy@example.test"
	records := []UserRecord{
		{ID: 3, Username: "personal", Password: "personal-hash", CreatedAt: now, UpdatedAt: now},
		{ID: 91, Username: "deleted-personal", DeletedAt: gorm.DeletedAt{Time: now, Valid: true}},
		{
			ID: 20, Kind: UserKindOrganization, Username: "organization",
			Password: "must-not-copy", Email: &email,
			OrganizationModelGroups: JSONB(`[7,9]`), OrganizationMemberKeysDisabled: true,
			Credits: 123.25, CreditsUnlimited: true, BillingMultiplier: &multiplier,
			Timezone: "UTC", ResetAllowanceMode: "daily", ResetAllowanceAmount: 77,
			ResetAllowanceAvailable: 33, ResetAllowanceExpiresAt: &now, ResetAllowanceRenewsAt: &now,
			Limit5hCredits: &zero, WindowMode5h: "fixed", Limit1dCredits: &multiplier,
			WindowMode1d: "first_use", Limit7dCredits: &zero, WindowMode7d: "fixed",
			Limit30dCredits: &multiplier, WindowMode30d: "first_use",
			WeekResetDay: 0, WeekResetHour: 0, PeriodWindowStart5h: &now,
			PeriodWindowStart1d: &now, PeriodWindowStart7d: &now, PeriodWindowStart30d: &now,
			UsageEpoch5h: &now, UsageEpoch1d: &now, UsageEpoch7d: &now, UsageEpoch30d: &now,
			CreatedAt: now, UpdatedAt: now,
		},
		{ID: 65, Kind: UserKindOrganization, Username: "deleted-organization", Credits: -2.5, DeletedAt: gorm.DeletedAt{Time: now, Valid: true}},
	}
	for index := range records {
		if errCreate := db.Create(&records[index]).Error; errCreate != nil {
			t.Fatal(errCreate)
		}
	}
	// Store values that differ from GORM defaults to exercise lossless copying.
	if errUpdate := db.Model(&UserRecord{}).Where("id = ?", 20).Updates(map[string]any{
		"week_reset_day": 0, "window_mode_5h": "", "updated_at": now,
	}).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if errLoad := db.Unscoped().Order("id").Find(&records).Error; errLoad != nil {
		t.Fatal(errLoad)
	}
	return records
}

func assertOrganizationMigrationUsers(t *testing.T, db *gorm.DB, legacy []UserRecord, snapshot bool) {
	t.Helper()
	for _, record := range legacy {
		if snapshot {
			// The portable codec normalizes instants to UTC and encodes SQL NULL
			// JSONB as JSON null. Live migrations must preserve the exact values.
			encoded, errMarshal := json.Marshal(record)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			if errUnmarshal := json.Unmarshal(encoded, &record); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			normalizeSnapshotTimes(&record)
		}
		var identity BillingIdentityRecord
		if errLoad := db.First(&identity, record.ID).Error; errLoad != nil {
			t.Fatal(errLoad)
		}
		kind := UserKindPersonal
		if record.Kind == UserKindOrganization {
			kind = UserKindOrganization
			var actual OrganizationRecord
			if errLoad := db.Unscoped().First(&actual, record.ID).Error; errLoad != nil {
				t.Fatal(errLoad)
			}
			if expected := organizationFromUser(&record); !reflect.DeepEqual(&actual, expected) {
				t.Fatalf("organization %d changed: got %#v, want %#v", record.ID, actual, expected)
			}
			var count int64
			if errCount := db.Unscoped().Model(&UserRecord{}).Where("id = ?", record.ID).Count(&count).Error; errCount != nil || count != 0 {
				t.Fatalf("legacy organization still in user: count=%d error=%v", count, errCount)
			}
		} else {
			var actual UserRecord
			if errLoad := db.Unscoped().First(&actual, record.ID).Error; errLoad != nil {
				t.Fatal(errLoad)
			}
			if !reflect.DeepEqual(actual, record) {
				for index := 0; index < reflect.TypeOf(record).NumField(); index++ {
					if got, want := reflect.ValueOf(actual).Field(index).Interface(), reflect.ValueOf(record).Field(index).Interface(); !reflect.DeepEqual(got, want) {
						t.Errorf("personal user %d field %s changed: got %#v, want %#v", record.ID, reflect.TypeOf(record).Field(index).Name, got, want)
					}
				}
				t.FailNow()
			}
		}
		if identity.Kind != kind {
			t.Fatalf("identity %d kind = %q, want %q", record.ID, identity.Kind, kind)
		}
	}
}

func TestOrganizationStorageMigrationPreservesStateAndDeletedIdentities(t *testing.T) {
	db := openDatabaseSnapshotSQLiteRawTestDB(t, filepath.Join(t.TempDir(), "legacy.db"))
	if errSchema := autoMigrateSchema(db); errSchema != nil {
		t.Fatal(errSchema)
	}
	legacy := seedOrganizationMigrationUsers(t, db)
	existing := OrganizationRecord{ID: 150, Username: "existing", DeletedAt: legacy[3].DeletedAt}
	if errCreate := db.Create(&existing).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	for run := 0; run < 2; run++ {
		if errMigrate := migrateOrganizationStorage(db); errMigrate != nil {
			t.Fatal(errMigrate)
		}
		assertOrganizationMigrationUsers(t, db, legacy, false)
		var identity BillingIdentityRecord
		if errLoad := db.First(&identity, existing.ID).Error; errLoad != nil || identity.Kind != UserKindOrganization {
			t.Fatalf("existing organization identity = %+v, error = %v", identity, errLoad)
		}
	}
	identity := BillingIdentityRecord{Kind: UserKindPersonal}
	if errCreate := db.Create(&identity).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if identity.ID <= existing.ID {
		t.Fatalf("allocated ID %d reuses historical identity", identity.ID)
	}
	for _, column := range []string{"password", "email", "mfa", "passkey", "session_version"} {
		if db.Migrator().HasColumn(&OrganizationRecord{}, column) {
			t.Fatalf("organization contains authentication column %s", column)
		}
	}
}

func TestOrganizationStorageMigrationRollsBackConflictingIdentity(t *testing.T) {
	db := openDatabaseSnapshotSQLiteTestDB(t, filepath.Join(t.TempDir(), "conflict.db"))
	legacy := seedOrganizationMigrationUsers(t, db)
	if errCreate := db.Create(&BillingIdentityRecord{ID: 20, Kind: UserKindPersonal}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if errMigrate := migrateOrganizationStorage(db); errMigrate == nil || !strings.Contains(errMigrate.Error(), "kind conflicts") {
		t.Fatalf("migration error = %v, want identity conflict", errMigrate)
	}
	var users int64
	if errCount := db.Unscoped().Model(&UserRecord{}).Count(&users).Error; errCount != nil || users != int64(len(legacy)) {
		t.Fatalf("user rows after rollback = %d, error = %v", users, errCount)
	}
	var identities int64
	if errCount := db.Model(&BillingIdentityRecord{}).Count(&identities).Error; errCount != nil || identities != 1 {
		t.Fatalf("identity rows after rollback = %d, error = %v", identities, errCount)
	}
}

func TestOrganizationStorageSnapshotLegacyConversion(t *testing.T) {
	ctx := context.Background()
	for version := 23; version <= 28; version++ {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			source := openDatabaseSnapshotSQLiteTestDB(t, filepath.Join(t.TempDir(), "source.db"))
			legacy := seedOrganizationMigrationUsers(t, source)
			member := OrganizationMemberRecord{OrgUserID: 20, MemberUserID: 3, Role: OrganizationRoleOwner}
			if errCreate := source.Create(&member).Error; errCreate != nil {
				t.Fatal(errCreate)
			}
			payerID := uint(20)
			key := APIKeyRecord{APIKey: "migration-key", UserID: &payerID}
			if errCreate := source.Create(&key).Error; errCreate != nil {
				t.Fatal(errCreate)
			}
			balance := BillingBalanceRecord{ID: "migration-balance", UserID: payerID}
			if errCreate := source.Create(&balance).Error; errCreate != nil {
				t.Fatal(errCreate)
			}
			// Historical payer references include soft-deleted users and organizations.
			for _, ownerID := range []uint{3, 20, 65, 91} {
				usage := UsageRecord{Timestamp: legacy[0].CreatedAt, PayloadJSON: JSONB(`{}`)}
				if errCreate := source.Create(&usage).Error; errCreate != nil {
					t.Fatal(errCreate)
				}
				payerKey := APIKeyRecord{APIKey: fmt.Sprintf("payer-key-%d", ownerID), UserID: &ownerID}
				if errCreate := source.Create(&payerKey).Error; errCreate != nil {
					t.Fatal(errCreate)
				}
				charge := BillingChargeRecord{ID: fmt.Sprintf("charge-%d", ownerID), UsageID: usage.ID,
					PayloadHash: fmt.Sprintf("payload-%d", ownerID), UserID: &ownerID, APIKeyID: &payerKey.ID,
					PriceSnapshot: JSONB(`{}`)}
				if errCreate := source.Create(&charge).Error; errCreate != nil {
					t.Fatal(errCreate)
				}
				payerBalance := BillingBalanceRecord{ID: fmt.Sprintf("balance-%d", ownerID), UserID: ownerID}
				if errCreate := source.Create(&payerBalance).Error; errCreate != nil {
					t.Fatal(errCreate)
				}
			}
			if version == 28 {
				if errMigrate := migrateOrganizationStorage(source); errMigrate != nil {
					t.Fatal(errMigrate)
				}
			}
			path := filepath.Join(t.TempDir(), "snapshot.zip")
			if _, errExport := exportDatabaseSnapshotVersion(ctx, source, path, version); errExport != nil {
				t.Fatal(errExport)
			}
			snapshot, errOpen := OpenDatabaseSnapshot(ctx, path)
			if errOpen != nil {
				t.Fatal(errOpen)
			}
			t.Cleanup(func() {
				if errClose := snapshot.Close(); errClose != nil {
					t.Error(errClose)
				}
			})
			target := openDatabaseSnapshotSQLiteRawTestDB(t, filepath.Join(t.TempDir(), "target.db"))
			result, errImport := ImportDatabaseSnapshot(ctx, target, snapshot, nil)
			if errImport != nil {
				t.Fatal(errImport)
			}
			assertOrganizationMigrationUsers(t, target, legacy, true)
			for _, table := range result.Tables {
				if version < 28 && table.Name == "user" && table.Imported != int64(len(legacy)) {
					t.Fatalf("source user count = %d, want %d", table.Imported, len(legacy))
				}
			}
			identity := BillingIdentityRecord{Kind: UserKindOrganization}
			if errCreate := target.Create(&identity).Error; errCreate != nil || identity.ID <= 91 {
				t.Fatalf("restored identity sequence: id=%d, error=%v", identity.ID, errCreate)
			}
			assertOrganizationSnapshotPayerRelationships(t, target)
			if errOrphan := target.Model(&OrganizationMemberRecord{}).Where("id = ?", member.ID).Update("org_user_id", 3).Error; errOrphan != nil {
				t.Fatal(errOrphan)
			}
			if errValidate := validateImportedDatabaseSnapshotRelationships(target); errValidate == nil || !strings.Contains(errValidate.Error(), "organization_member.org_user_id") {
				t.Fatalf("personal organization target accepted: %v", errValidate)
			}
		})
	}
}

func assertOrganizationSnapshotPayerRelationships(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, table := range []string{"api_key", "billing_balance_record", "billing_charge"} {
		// Roll back each mutation so later checks test one orphan at a time.
		errRollback := fmt.Errorf("rollback relationship probe")
		errTransaction := db.Transaction(func(tx *gorm.DB) error {
			query := fmt.Sprintf(`UPDATE %s SET "user_id" = ? WHERE "user_id" = ?`, quoteDatabaseSnapshotIdentifier(table))
			if errUpdate := tx.Exec(query, 9999, 20).Error; errUpdate != nil {
				t.Fatal(errUpdate)
			}
			if errValidate := validateImportedDatabaseSnapshotRelationships(tx); errValidate == nil || !strings.Contains(errValidate.Error(), table+".user_id") {
				t.Errorf("%s missing payer accepted: %v", table, errValidate)
			}
			return errRollback
		})
		if errTransaction != errRollback {
			t.Fatalf("relationship probe transaction: %v", errTransaction)
		}
	}
}

func TestOrganizationStorageSnapshotLegacyRegistries(t *testing.T) {
	for version := 23; version <= 28; version++ {
		models, ok := databaseSnapshotModels(version)
		if !ok {
			t.Fatalf("version %d unsupported", version)
		}
		for _, name := range []string{"organization", "billing_identity"} {
			found := false
			for _, model := range models {
				found = found || model.name == name
			}
			if found != (version == 28) {
				t.Fatalf("version %d table %s present = %v", version, name, found)
			}
		}
		if actual := databaseSnapshotModelType(t, models, "user"); actual != reflect.TypeOf(&UserRecord{}) {
			t.Fatalf("version %d user shape changed: %v", version, actual)
		}
	}
}
