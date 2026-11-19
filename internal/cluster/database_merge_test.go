package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestMergedSchemaUpgradesBothHistories(t *testing.T) {
	for _, history := range []struct {
		name    string
		version int64
		models  []databaseModel
		local   bool
	}{
		{"local13", 13, databaseSnapshotV10Models, true},
		{"upstream17", 17, databaseSnapshotV17Models, false},
		{"local18", 18, databaseSnapshotLocalV18Models, true},
		{"upstream19", 19, databaseSnapshotUpstreamV18Models, false},
		{"local20", 20, databaseSnapshotV20Models, true},
		{"upstream20", 20, withUsageModel[UsageRecord](databaseSnapshotUpstreamV18Models), false},
		{"local21", 21, databaseSnapshotV21Models, true},
	} {
		t.Run(history.name, func(t *testing.T) {
			ctx := context.Background()
			db := openDatabaseSnapshotSQLiteRawTestDB(t, filepath.Join(t.TempDir(), "upgrade.db"))
			for _, model := range history.models {
				if errMigrate := db.AutoMigrate(model.newRecord()); errMigrate != nil {
					t.Fatal(errMigrate)
				}
			}
			if errMigrate := db.AutoMigrate(&schemaMigrationRecord{}); errMigrate != nil {
				t.Fatal(errMigrate)
			}
			if errCreate := db.Create(&schemaMigrationRecord{ID: 1, Version: history.version, UpdatedAt: time.Now().UTC()}).Error; errCreate != nil {
				t.Fatal(errCreate)
			}
			user := map[string]any{"id": 1, "username": "existing", "password": "hash", "credits": 123.0, "credits_unlimited": false, "email_version": 0, "session_version": 0, "week_reset_hour": 0, "timezone": "Asia/Shanghai", "window_mode_5h": "first_use", "window_mode_1d": "first_use", "window_mode_7d": "first_use", "window_mode_30d": "first_use", "week_reset_day": 1}
			if history.local {
				user["approval_pending"] = true
				user["reset_allowance_mode"] = "monthly"
				user["reset_allowance_amount"] = 9
				user["reset_allowance_available"] = 4
			} else {
				user["billing_multiplier"] = 0.75
			}
			if errCreate := db.Table("user").Create(user).Error; errCreate != nil {
				t.Fatal(errCreate)
			}
			charge := map[string]any{"id": "old-charge", "usage_id": 1, "payload_hash": "old-hash", "amount": 3.0, "price_snapshot": "{}", "created_at": time.Now().UTC()}
			// Snapshot v15+ charge shapes always carry base amounts and multipliers.
			billedCharge := !history.local || history.version >= 15
			if billedCharge {
				charge["base_amount"] = 4.0
				charge["billing_multiplier"] = 0.75
			}
			if errCreate := db.Table("billing_charge").Create(charge).Error; errCreate != nil {
				t.Fatal(errCreate)
			}
			// Exercise the same version coordinator used by PostgreSQL without needing a live server.
			store := newFakeSchemaMigrationStore(schemaMigrationState{Version: history.version, Initialized: true})
			result, errCoordinate := coordinateSchemaMigration(ctx, currentDatabaseVersion, time.Millisecond, time.Second, false, store.operations())
			if errCoordinate != nil || !result.MigrationCommitted || result.CurrentState.Version != currentDatabaseVersion {
				t.Fatalf("version upgrade = %+v, %v", result, errCoordinate)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if errMigrate := AutoMigrate(db); errMigrate != nil {
					t.Fatal(errMigrate)
				}
			}
			var restored UserRecord
			if errLoad := db.First(&restored, 1).Error; errLoad != nil {
				t.Fatal(errLoad)
			}
			if restored.Username != "existing" || restored.Password != "hash" || restored.Credits != 123 || restored.ApprovalPending != history.local {
				t.Fatalf("user state changed: %+v", restored)
			}
			if history.local {
				if restored.ResetAllowanceMode != "monthly" || restored.ResetAllowanceAmount != 9 || restored.ResetAllowanceAvailable != 4 || restored.BillingMultiplier != nil {
					t.Fatalf("local state changed: %+v", restored)
				}
			} else if restored.BillingMultiplier == nil || *restored.BillingMultiplier != 0.75 || restored.ResetAllowanceAmount != 0 {
				t.Fatalf("upstream state changed: %+v", restored)
			}
			var migratedCharge BillingChargeRecord
			if errLoad := db.First(&migratedCharge, "id = ?", "old-charge").Error; errLoad != nil {
				t.Fatal(errLoad)
			}
			wantBase, wantMultiplier := 3.0, 1.0
			if billedCharge {
				wantBase, wantMultiplier = 4, 0.75
			}
			if migratedCharge.Amount != 3 || migratedCharge.BaseAmount != wantBase || migratedCharge.BillingMultiplier != wantMultiplier {
				t.Fatalf("charge changed: %+v", migratedCharge)
			}
			for _, table := range []string{"codex_reset_redemptions", "billing_recharge_request", "codex_refresh_journal", "cluster_rebalance_gate"} {
				if !db.Migrator().HasTable(table) {
					t.Fatalf("missing merged table %s", table)
				}
			}
		})
	}
}

func TestMergedSnapshotHistoriesRoundTrip(t *testing.T) {
	ctx := context.Background()
	source := openDatabaseSnapshotSQLiteTestDB(t, filepath.Join(t.TempDir(), "source.db"))
	multiplier := 0.6
	user := UserRecord{Username: "snapshot-user", Password: "hash", ApprovalPending: true, ResetAllowanceMode: "monthly", ResetAllowanceAmount: 7, ResetAllowanceAvailable: 3, BillingMultiplier: &multiplier}
	if errCreate := source.Create(&user).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	now := time.Now().UTC()
	if errCreate := source.Create(&CodexResetRedemptionRecord{CredentialID: "credential", RequestKeyHash: "request-hash", CreatedAt: now}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if errCreate := source.Create(&BillingRechargeRequestRecord{UserID: user.ID, Amount: 25, Status: "pending", CreatedAt: now, UpdatedAt: now}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	if errCreate := source.Create(&CodexRefreshRecord{Grant: "grant-hash", Owner: "owner", Receipt: JSONB(`{"stage":"pending"}`)}).Error; errCreate != nil {
		t.Fatal(errCreate)
	}
	for version := 6; version <= currentDatabaseVersion; version++ {
		for _, upstream := range []bool{false, true} {
			collision := version >= 7 && version <= 13 || version == 18 || version == 20
			if upstream && !collision {
				continue
			}
			t.Run(fmt.Sprintf("v%d/upstream=%t", version, upstream), func(t *testing.T) {
				models, _ := databaseSnapshotModels(version)
				if upstream && version == 18 {
					models = databaseSnapshotUpstreamV18Models
				} else if upstream && version == 20 {
					models = withUsageModel[UsageRecord](databaseSnapshotUpstreamV18Models)
				} else if upstream {
					models = databaseSnapshotV6Models
				}
				// Upstream v19 has no local counterpart.
				upstreamHistory := upstream || version == 19
				merged := version == 18 || version >= 20
				path := filepath.Join(t.TempDir(), "snapshot.zip")
				if _, errExport := exportDatabaseSnapshotModels(ctx, source, path, version, models); errExport != nil {
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
				if _, errImport := ImportDatabaseSnapshot(ctx, target, snapshot, nil); errImport != nil {
					t.Fatal(errImport)
				}
				var restored UserRecord
				if errLoad := target.First(&restored, user.ID).Error; errLoad != nil {
					t.Fatal(errLoad)
				}
				local := !upstreamHistory && (version >= 8 && version <= 13 || merged)
				allowance := !upstreamHistory && (version >= 10 && version <= 13 || merged)
				billing := version >= 15
				for _, table := range []struct {
					name    string
					present bool
				}{
					{"codex_reset_redemptions", !upstreamHistory && (version >= 7 && version <= 13 || merged)},
					{"billing_recharge_request", !upstreamHistory && (version >= 9 && version <= 13 || merged)},
					{"codex_refresh_journal", version >= 17},
				} {
					var count int64
					if errCount := target.Table(table.name).Count(&count).Error; errCount != nil {
						t.Fatal(errCount)
					}
					wantCount := int64(0)
					if table.present {
						wantCount = 1
					}
					if count != wantCount {
						t.Fatalf("%s restored rows = %d, want %d", table.name, count, wantCount)
					}
				}
				if restored.Username != user.Username || restored.Password != "hash" || restored.ApprovalPending != local {
					t.Fatalf("approval round trip: %+v", restored)
				}
				if allowance && (restored.ResetAllowanceAmount != 7 || restored.ResetAllowanceAvailable != 3) || !allowance && restored.ResetAllowanceAmount != 0 {
					t.Fatalf("allowance round trip: %+v", restored)
				}
				if billing && (restored.BillingMultiplier == nil || *restored.BillingMultiplier != multiplier) || !billing && restored.BillingMultiplier != nil {
					t.Fatalf("billing round trip: %+v", restored)
				}
			})
		}
	}
}

func TestMergedSnapshotRejectsMislabeledHistory(t *testing.T) {
	for _, version := range []int{7, 8, 9, 10, 11, 12, 13, 18, 20} {
		local, _ := databaseSnapshotModels(version)
		manifest := DatabaseSnapshotManifest{FormatVersion: version}
		for _, model := range local {
			manifest.Tables = append(manifest.Tables, DatabaseSnapshotManifestTable{Name: model.name})
		}
		selected, _ := databaseSnapshotManifestModels(manifest)
		if !reflect.DeepEqual(databaseSnapshotModelNames(selected), databaseSnapshotModelNames(local)) {
			t.Fatalf("v%d local registry lost", version)
		}
		// A local-only field must never be silently accepted by the upstream registry.
		for _, model := range databaseSnapshotV6Models {
			if model.name != "user" {
				continue
			}
			if errDecode := decodeStrictJSON([]byte(`{"Username":"bad","approval_pending":true}`), model.newRecord()); errDecode == nil {
				t.Fatal("upstream snapshot accepted local-only field")
			}
		}
	}
	// Local v10..v13 used an explicit lowercase JSON name for approval_pending.
	raw, errMarshal := json.Marshal(&databaseSnapshotV13UserRecord{ApprovalPending: true})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var fields map[string]json.RawMessage
	if errDecode := json.Unmarshal(raw, &fields); errDecode != nil {
		t.Fatal(errDecode)
	}
	if string(fields["approval_pending"]) != "true" {
		t.Fatalf("historical approval field changed: %s", raw)
	}
}

func databaseSnapshotModelNames(models []databaseModel) []string {
	names := make([]string, len(models))
	for index, model := range models {
		names[index] = model.name
	}
	return names
}
