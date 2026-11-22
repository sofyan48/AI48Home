package cluster

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

// allocateBillingIdentityTx reserves a payer ID shared by users and organizations.
// Callers create the owner in the same transaction so failed creation rolls back.
func allocateBillingIdentityTx(ctx context.Context, tx *gorm.DB, kind string) (uint, error) {
	identity := BillingIdentityRecord{Kind: kind}
	if errCreate := tx.WithContext(contextOrBackground(ctx)).Create(&identity).Error; errCreate != nil {
		return 0, errCreate
	}
	return identity.ID, nil
}

func billingOwnerTable(owner *UserRecord) string {
	if owner.IsOrganization() {
		return "organization"
	}
	return "user"
}

// getBillingOwnerTx resolves stable legacy payer IDs without treating organizations
// as login users. A fresh session preserves transaction and row-lock clauses.
func getBillingOwnerTx(ctx context.Context, db *gorm.DB, id uint) (*UserRecord, error) {
	user := &UserRecord{}
	errUser := db.WithContext(contextOrBackground(ctx)).Session(&gorm.Session{}).Where("id = ?", id).First(user).Error
	if errUser == nil {
		return user, nil
	}
	if !errors.Is(errUser, gorm.ErrRecordNotFound) {
		return nil, errUser
	}
	organization := &OrganizationRecord{}
	if errOrg := db.WithContext(contextOrBackground(ctx)).Session(&gorm.Session{}).Where("id = ?", id).First(organization).Error; errOrg != nil {
		return nil, errOrg
	}
	return organization.userRecord(), nil
}
