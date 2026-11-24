package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// OrganizationRecord is a billing owner, not an authentication account. IDs share
// the billing identity namespace with personal users, including legacy payer IDs.
type OrganizationRecord struct {
	ID                             uint     `gorm:"column:id;primaryKey;autoIncrement:false"`
	Username                       string   `gorm:"column:username;not null;index;uniqueIndex:idx_organization_username_active_unique,where:deleted_at IS NULL"`
	OrganizationChannelGroups      JSONB    `gorm:"column:organization_channel_groups"`
	OrganizationModelGroups        JSONB    `gorm:"column:organization_model_groups"`
	OrganizationMemberKeysDisabled bool     `gorm:"column:organization_member_keys_disabled;not null;default:false"`
	Credits                        float64  `gorm:"column:credits;not null;default:0"`
	CreditsUnlimited               bool     `gorm:"column:credits_unlimited;not null;default:false"`
	BillingMultiplier              *float64 `gorm:"column:billing_multiplier"`
	Timezone                       string   `gorm:"column:timezone;not null;default:Asia/Shanghai"`

	ResetAllowanceMode      string     `gorm:"column:reset_allowance_mode;not null;default:''"`
	ResetAllowanceAmount    int64      `gorm:"column:reset_allowance_amount;not null;default:0"`
	ResetAllowanceAvailable int64      `gorm:"column:reset_allowance_available;not null;default:0"`
	ResetAllowanceExpiresAt *time.Time `gorm:"column:reset_allowance_expires_at"`
	ResetAllowanceRenewsAt  *time.Time `gorm:"column:reset_allowance_renews_at"`

	Limit5hCredits  *float64 `gorm:"column:limit_5h_credits"`
	WindowMode5h    string   `gorm:"column:window_mode_5h;not null;default:first_use"`
	Limit1dCredits  *float64 `gorm:"column:limit_1d_credits"`
	WindowMode1d    string   `gorm:"column:window_mode_1d;not null;default:first_use"`
	Limit7dCredits  *float64 `gorm:"column:limit_7d_credits"`
	WindowMode7d    string   `gorm:"column:window_mode_7d;not null;default:first_use"`
	Limit30dCredits *float64 `gorm:"column:limit_30d_credits"`
	WindowMode30d   string   `gorm:"column:window_mode_30d;not null;default:first_use"`
	WeekResetDay    int      `gorm:"column:week_reset_day;not null;default:1"`
	WeekResetHour   int      `gorm:"column:week_reset_hour;not null;default:0"`

	PeriodWindowStart5h  *time.Time     `gorm:"column:period_window_start_5h"`
	PeriodWindowStart1d  *time.Time     `gorm:"column:period_window_start_1d"`
	PeriodWindowStart7d  *time.Time     `gorm:"column:period_window_start_7d"`
	PeriodWindowStart30d *time.Time     `gorm:"column:period_window_start_30d"`
	UsageEpoch5h         *time.Time     `gorm:"column:usage_epoch_5h"`
	UsageEpoch1d         *time.Time     `gorm:"column:usage_epoch_1d"`
	UsageEpoch7d         *time.Time     `gorm:"column:usage_epoch_7d"`
	UsageEpoch30d        *time.Time     `gorm:"column:usage_epoch_30d"`
	CreatedAt            time.Time      `gorm:"column:created_at"`
	UpdatedAt            time.Time      `gorm:"column:updated_at"`
	DeletedAt            gorm.DeletedAt `gorm:"column:deleted_at;index"`
}

// TableName returns the dedicated organization table.
func (OrganizationRecord) TableName() string { return "organization" }

// organizationFromUser copies only organization metadata and billing state.
// It deliberately preserves legacy values and soft deletion without normalization.
func organizationFromUser(record *UserRecord) *OrganizationRecord {
	if record == nil {
		return nil
	}
	return &OrganizationRecord{
		ID: record.ID, Username: record.Username,
		OrganizationChannelGroups:      cloneJSONB(record.OrganizationChannelGroups),
		OrganizationModelGroups:        cloneJSONB(record.OrganizationModelGroups),
		OrganizationMemberKeysDisabled: record.OrganizationMemberKeysDisabled,
		Credits:                        record.Credits, CreditsUnlimited: record.CreditsUnlimited,
		BillingMultiplier: record.BillingMultiplier, Timezone: record.Timezone,
		ResetAllowanceMode: record.ResetAllowanceMode, ResetAllowanceAmount: record.ResetAllowanceAmount,
		ResetAllowanceAvailable: record.ResetAllowanceAvailable,
		ResetAllowanceExpiresAt: record.ResetAllowanceExpiresAt, ResetAllowanceRenewsAt: record.ResetAllowanceRenewsAt,
		Limit5hCredits: record.Limit5hCredits, WindowMode5h: record.WindowMode5h,
		Limit1dCredits: record.Limit1dCredits, WindowMode1d: record.WindowMode1d,
		Limit7dCredits: record.Limit7dCredits, WindowMode7d: record.WindowMode7d,
		Limit30dCredits: record.Limit30dCredits, WindowMode30d: record.WindowMode30d,
		WeekResetDay: record.WeekResetDay, WeekResetHour: record.WeekResetHour,
		PeriodWindowStart5h: record.PeriodWindowStart5h, PeriodWindowStart1d: record.PeriodWindowStart1d,
		PeriodWindowStart7d: record.PeriodWindowStart7d, PeriodWindowStart30d: record.PeriodWindowStart30d,
		UsageEpoch5h: record.UsageEpoch5h, UsageEpoch1d: record.UsageEpoch1d,
		UsageEpoch7d: record.UsageEpoch7d, UsageEpoch30d: record.UsageEpoch30d,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, DeletedAt: record.DeletedAt,
	}
}

// userRecord is an explicit compatibility DTO for API and period-policy helpers.
// It must never be saved as a user row.
func (record *OrganizationRecord) userRecord() *UserRecord {
	if record == nil {
		return nil
	}
	return &UserRecord{
		ID: record.ID, Kind: UserKindOrganization, Username: record.Username,
		OrganizationChannelGroups:      cloneJSONB(record.OrganizationChannelGroups),
		OrganizationModelGroups:        cloneJSONB(record.OrganizationModelGroups),
		OrganizationMemberKeysDisabled: record.OrganizationMemberKeysDisabled,
		Credits:                        record.Credits, CreditsUnlimited: record.CreditsUnlimited,
		BillingMultiplier: record.BillingMultiplier, Timezone: record.Timezone,
		ResetAllowanceMode: record.ResetAllowanceMode, ResetAllowanceAmount: record.ResetAllowanceAmount,
		ResetAllowanceAvailable: record.ResetAllowanceAvailable,
		ResetAllowanceExpiresAt: record.ResetAllowanceExpiresAt, ResetAllowanceRenewsAt: record.ResetAllowanceRenewsAt,
		Limit5hCredits: record.Limit5hCredits, WindowMode5h: record.WindowMode5h,
		Limit1dCredits: record.Limit1dCredits, WindowMode1d: record.WindowMode1d,
		Limit7dCredits: record.Limit7dCredits, WindowMode7d: record.WindowMode7d,
		Limit30dCredits: record.Limit30dCredits, WindowMode30d: record.WindowMode30d,
		WeekResetDay: record.WeekResetDay, WeekResetHour: record.WeekResetHour,
		PeriodWindowStart5h: record.PeriodWindowStart5h, PeriodWindowStart1d: record.PeriodWindowStart1d,
		PeriodWindowStart7d: record.PeriodWindowStart7d, PeriodWindowStart30d: record.PeriodWindowStart30d,
		UsageEpoch5h: record.UsageEpoch5h, UsageEpoch1d: record.UsageEpoch1d,
		UsageEpoch7d: record.UsageEpoch7d, UsageEpoch30d: record.UsageEpoch30d,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, DeletedAt: record.DeletedAt,
	}
}

// applyOrganizationUpdate uses the legacy DTO only for shared billing validation.
// Authentication and approval fields in UserUpdate are intentionally ignored.
func applyOrganizationUpdate(record *UserRecord, update UserUpdate) error {
	if update.Username != nil {
		name := strings.TrimSpace(*update.Username)
		if name == "" {
			return fmt.Errorf("username is required")
		}
		record.Username = name
	}
	if update.Credits != nil {
		record.Credits = *update.Credits
	}
	if update.CreditsUnlimited != nil {
		record.CreditsUnlimited = *update.CreditsUnlimited
	}
	return applyUserPeriodLimitUpdate(record, update)
}

// UpdateOrganization patches the dedicated billing owner without writing a user.
func (r *Repository) UpdateOrganization(ctx context.Context, id uint, update UserUpdate) (*UserRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	if id == 0 {
		return nil, ErrOrganizationNotFound
	}
	ctx = contextOrBackground(ctx)
	var result *UserRecord
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		stored := &OrganizationRecord{}
		errFirst := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).First(stored).Error
		if errors.Is(errFirst, gorm.ErrRecordNotFound) {
			return ErrOrganizationNotFound
		}
		if errFirst != nil {
			return errFirst
		}
		record := stored.userRecord()
		if errApply := applyOrganizationUpdate(record, update); errApply != nil {
			return errApply
		}
		stored = organizationFromUser(record)
		if errSave := tx.Save(stored).Error; errSave != nil {
			return errSave
		}
		result = stored.userRecord()
		return nil
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return result, nil
}
