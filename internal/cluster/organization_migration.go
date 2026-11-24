package cluster

import (
	"fmt"
	"strings"
	"sync"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// BillingIdentityRecord reserves a payer ID permanently, including deleted owners.
// Users and organizations allocate IDs here rather than in their own tables.
type BillingIdentityRecord struct {
	ID   uint   `gorm:"column:id;primaryKey;autoIncrement"`
	Kind string `gorm:"column:kind;not null"`
}

func (BillingIdentityRecord) TableName() string { return "billing_identity" }

// migrateOrganizationStorage also runs during legacy snapshot import, after
// validating source counts but before validating relationships in the new schema.
func migrateOrganizationStorage(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		// Raw SQL intentionally includes soft-deleted owners and preserves zero
		// values instead of applying GORM's create-time defaults to billing state.
		queries := []string{
			`INSERT INTO "billing_identity" ("id", "kind") SELECT "id", CASE WHEN "kind" = 'organization' THEN 'organization' ELSE 'personal' END FROM "user" WHERE TRUE ON CONFLICT ("id") DO NOTHING`,
			`INSERT INTO "billing_identity" ("id", "kind") SELECT "id", 'organization' FROM "organization" WHERE TRUE ON CONFLICT ("id") DO NOTHING`,
		}
		for _, query := range queries {
			if errInsert := tx.Exec(query).Error; errInsert != nil {
				return fmt.Errorf("backfill billing identities: %w", errInsert)
			}
		}
		var conflicts int64
		if errCheck := tx.Raw(`SELECT COUNT(*) FROM "billing_identity" AS identity WHERE EXISTS (SELECT 1 FROM "user" AS owner WHERE owner."id" = identity."id" AND identity."kind" <> CASE WHEN owner."kind" = 'organization' THEN 'organization' ELSE 'personal' END) OR EXISTS (SELECT 1 FROM "organization" AS owner WHERE owner."id" = identity."id" AND identity."kind" <> 'organization')`).Scan(&conflicts).Error; errCheck != nil {
			return fmt.Errorf("check billing identities: %w", errCheck)
		}
		if conflicts != 0 {
			return fmt.Errorf("billing identity kind conflicts: %d", conflicts)
		}
		modelSchema, errSchema := schema.Parse(&OrganizationRecord{}, &sync.Map{}, schema.NamingStrategy{})
		if errSchema != nil {
			return fmt.Errorf("parse organization migration columns: %w", errSchema)
		}
		columns := make([]string, 0, len(modelSchema.DBNames))
		for _, column := range modelSchema.DBNames {
			columns = append(columns, quoteDatabaseSnapshotIdentifier(column))
		}
		columnList := strings.Join(columns, ", ")
		query := `INSERT INTO "organization" (` + columnList + `) SELECT ` + columnList + ` FROM "user" WHERE "kind" = 'organization'`
		if errCopy := tx.Exec(query).Error; errCopy != nil {
			return fmt.Errorf("copy legacy organizations: %w", errCopy)
		}
		if errDelete := tx.Unscoped().Where("kind = ?", UserKindOrganization).Delete(&UserRecord{}).Error; errDelete != nil {
			return fmt.Errorf("delete legacy organization users: %w", errDelete)
		}
		backend, errBackend := databaseBackendFromDB(tx)
		if errBackend != nil {
			return errBackend
		}
		return resetDatabaseSnapshotSequences(tx, backend, []databaseModel{
			newDatabaseModel[BillingIdentityRecord]("billing_identity", []string{"id"}, true, true),
		})
	})
}
