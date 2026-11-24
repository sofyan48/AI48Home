package cluster

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// OrganizationChannelGroupsOf returns credential scopes; empty means unrestricted.
func OrganizationChannelGroupsOf(org *UserRecord) ([]uint, error) {
	if org == nil {
		return []uint{}, nil
	}
	groups, errGroups := apiKeyChannelsFromJSON(org.OrganizationChannelGroups)
	if errGroups != nil {
		return nil, errGroups
	}
	groups = normalizeChannelGroupIDs(groups)
	if groups == nil {
		groups = []uint{}
	}
	return groups, nil
}

// organizationKeyChannels overrides caller bindings for organization-owned keys.
// Call inside the key mutation transaction so scope changes and key writes serialize.
func organizationKeyChannels(ctx context.Context, tx *gorm.DB, userID *uint, channels *[]uint) (*[]uint, error) {
	if userID == nil || *userID == 0 {
		return channels, nil
	}
	org, errOrg := getOrganizationTx(ctx, tx, *userID)
	if errors.Is(errOrg, ErrOrganizationNotFound) {
		return channels, nil
	}
	if errOrg != nil {
		return nil, errOrg
	}
	groups, errGroups := OrganizationChannelGroupsOf(org)
	if errGroups != nil {
		return nil, errGroups
	}
	return &groups, nil
}

// SetOrganizationChannelGroups atomically applies credential scopes to all keys,
// including deleted keys so a restore cannot resurrect an obsolete binding.
func (r *Repository) SetOrganizationChannelGroups(ctx context.Context, orgID uint, groups []uint) ([]uint, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	groups = normalizeChannelGroupIDs(groups)
	raw, errJSON := apiKeyChannelsJSON(groups)
	if errJSON != nil {
		return nil, errJSON
	}
	ctx = contextOrBackground(ctx)
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if errLock := lockAPIKeyMutationTransaction(tx); errLock != nil {
			return errLock
		}
		if _, errOrg := getOrganizationTx(ctx, tx, orgID); errOrg != nil {
			return errOrg
		}
		if errGroups := ensureAPIKeyGroupsNotDeleted(tx, &groups, nil); errGroups != nil {
			return errGroups
		}
		if errUpdate := tx.Model(&OrganizationRecord{}).Where("id = ?", orgID).Update("organization_channel_groups", raw).Error; errUpdate != nil {
			return errUpdate
		}
		if errKeys := tx.Unscoped().Model(&APIKeyRecord{}).Where("user_id = ?", orgID).Update("channels", raw).Error; errKeys != nil {
			return errKeys
		}
		return appendEvent(tx, "config", "upsert", configAPIKeysRootKey, time.Now().UTC().UnixNano())
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return groups, nil
}

// effectiveAPIKeyChannels resolves the organization policy even for stale or
// imported key bindings. Model-detail channel restrictions still intersect it.
func effectiveAPIKeyChannels(ctx context.Context, db *gorm.DB, record *APIKeyRecord) ([]uint, error) {
	channels, errChannels := organizationKeyChannels(ctx, db, record.UserID, nil)
	if errChannels != nil {
		return nil, errChannels
	}
	if channels != nil {
		return *channels, nil
	}
	return apiKeyChannelsFromJSON(record.Channels)
}
