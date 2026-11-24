package cluster

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestOrganizationMemberResetAllowanceMigration(t *testing.T) {
	db := openDatabaseSnapshotSQLiteRawTestDB(t, filepath.Join(t.TempDir(), "legacy.db"))
	if errExec := db.Exec(`CREATE TABLE organization_member (id integer PRIMARY KEY, org_user_id integer NOT NULL, member_user_id integer NOT NULL, role varchar(16) NOT NULL)`).Error; errExec != nil {
		t.Fatal(errExec)
	}
	if errExec := db.Exec(`INSERT INTO organization_member (id, org_user_id, member_user_id, role) VALUES (1, 2, 3, 'member')`).Error; errExec != nil {
		t.Fatal(errExec)
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	var member OrganizationMemberRecord
	if errFirst := db.First(&member, 1).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if member.ResetAllowanceMode != "" || member.ResetAllowanceAmount != 0 || member.ResetAllowanceAvailable != 0 || member.ResetAllowanceExpiresAt != nil || member.ResetAllowanceRenewsAt != nil || member.UsageEpoch5h != nil || member.UsageEpoch1d != nil || member.UsageEpoch7d != nil || member.UsageEpoch30d != nil {
		t.Fatalf("migration: %+v", member)
	}
	if errUpdate := db.Model(&member).Updates(map[string]any{"reset_allowance_mode": "monthly", "reset_allowance_amount": 3, "reset_allowance_available": 2}).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if errMigrate := AutoMigrate(db); errMigrate != nil {
		t.Fatal(errMigrate)
	}
	if errFirst := db.First(&member, 1).Error; errFirst != nil {
		t.Fatal(errFirst)
	}
	if member.ResetAllowanceAvailable != 2 || member.ResetAllowanceAmount != 3 {
		t.Fatalf("repeat migration: %+v", member)
	}
}

func TestOrganizationMemberResetAllowanceIsolation(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()
	user := createPeriodLimitUser(t, repo, ctx, "reset-member", 50)
	other := createPeriodLimitUser(t, repo, ctx, "reset-other", 50)
	org := createOrganizationTestOrg(t, repo, "reset-org", 100)
	addOrganizationTestMember(t, repo, org.ID, user.ID, OrganizationRoleMember)
	addOrganizationTestMember(t, repo, org.ID, other.ID, OrganizationRoleMember)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	limit := OptionalFloatUpdate{Set: true, Value: 10}
	first, calendar := PeriodWindowModeFirstUse, PeriodWindowModeCalendar
	update := OrganizationMemberUpdate{Limit5hCredits: limit, Limit1dCredits: limit, Limit7dCredits: limit, WindowMode1d: &first, WindowMode7d: &calendar}
	member, errUpdate := repo.UpdateOrganizationMember(ctx, org.ID, user.ID, update)
	if errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if _, errUpdateOther := repo.UpdateOrganizationMember(ctx, org.ID, other.ID, update); errUpdateOther != nil {
		t.Fatal(errUpdateOther)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	if errUpdateState := db.Model(member).Updates(map[string]any{"period_window_start_1d": old, "period_window_start_30d": old, "usage_epoch_30d": old}).Error; errUpdateState != nil {
		t.Fatal(errUpdateState)
	}
	createOrganizationTestCharge(t, repo, org.ID, user.ID, 3, old)
	createOrganizationTestCharge(t, repo, org.ID, other.ID, 2, old.Add(time.Nanosecond))
	if _, errPersonal := repo.SetUserResetAllowance(ctx, user.ID, ResetAllowanceOneTime, 4, nil, now); errPersonal != nil {
		t.Fatal(errPersonal)
	}
	if _, errSet := repo.SetOrganizationMemberResetAllowance(ctx, org.ID, user.ID, ResetAllowanceOneTime, 1, nil, now); errSet != nil {
		t.Fatal(errSet)
	}
	saved, windows, errConsume := repo.ConsumeOrganizationMemberResetAllowance(ctx, org.ID, user.ID, now)
	if errConsume != nil {
		t.Fatal(errConsume)
	}
	if saved.ResetAllowanceAvailable != 0 || saved.PeriodWindowStart1d != nil || saved.UsageEpoch5h == nil || saved.UsageEpoch1d == nil || saved.UsageEpoch7d == nil || !saved.UsageEpoch5h.Equal(now) || saved.PeriodWindowStart30d == nil || !saved.UsageEpoch30d.Equal(old) {
		t.Fatalf("saved: %+v", saved)
	}
	for _, window := range windows {
		if window.Limit != nil && window.Used != 0 {
			t.Fatalf("reset window: %+v", window)
		}
	}
	if _, _, errAgain := repo.ConsumeOrganizationMemberResetAllowance(ctx, org.ID, user.ID, now); !errors.Is(errAgain, ErrResetAllowanceUnavailable) {
		t.Fatalf("exhaustion: %v", errAgain)
	}
	personal, errPersonal := repo.GetUser(ctx, user.ID)
	if errPersonal != nil || personal.ResetAllowanceAvailable != 4 || personal.UsageEpoch5h != nil || personal.Credits != 50 {
		t.Fatalf("personal changed: %+v %v", personal, errPersonal)
	}
	orgSaved, errOrg := repo.GetOrganization(ctx, org.ID)
	if errOrg != nil || orgSaved.UsageEpoch5h != nil || orgSaved.Credits != 100 {
		t.Fatalf("org changed: %+v %v", orgSaved, errOrg)
	}
	otherWindows, errOther := repo.OrganizationMemberSpend(ctx, org.ID, other.ID, now)
	if errOther != nil || otherWindows[0].Used != 2 {
		t.Fatalf("other changed: %+v %v", otherWindows, errOther)
	}
	total, errTotal := repo.SumUserChargeAmountSince(ctx, org.ID, old.Add(-time.Second))
	if errTotal != nil || total != 5 {
		t.Fatalf("history changed: %v %v", total, errTotal)
	}
}

func TestOrganizationMemberResetAllowanceGuardedConsume(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()
	user := createPeriodLimitUser(t, repo, ctx, "guard-member", 0)
	org := createOrganizationTestOrg(t, repo, "guard-org", 0)
	addOrganizationTestMember(t, repo, org.ID, user.ID, OrganizationRoleMember)
	if _, errUpdate := repo.UpdateOrganizationMember(ctx, org.ID, user.ID, OrganizationMemberUpdate{Limit5hCredits: OptionalFloatUpdate{Set: true, Value: 10}}); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	if _, errSet := repo.SetOrganizationMemberResetAllowance(ctx, org.ID, user.ID, ResetAllowanceOneTime, 1, nil, now); errSet != nil {
		t.Fatal(errSet)
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	start := make(chan struct{})
	for index := 0; index < 8; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, errConsume := repo.ConsumeOrganizationMemberResetAllowance(ctx, org.ID, user.ID, now)
			results <- errConsume
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for errConsume := range results {
		if errConsume == nil {
			successes++
		} else if !errors.Is(errConsume, ErrResetAllowanceUnavailable) {
			t.Fatal(errConsume)
		}
	}
	if successes != 1 {
		t.Fatalf("successful consumes = %d", successes)
	}
	// A post-update status failure must roll back both renewal/decrement and epochs.
	if _, errSet := repo.SetOrganizationMemberResetAllowance(ctx, org.ID, user.ID, ResetAllowanceMonthly, 2, nil, now); errSet != nil {
		t.Fatal(errSet)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	if errDrop := db.Migrator().DropTable(&BillingChargeRecord{}); errDrop != nil {
		t.Fatal(errDrop)
	}
	if _, _, errConsume := repo.ConsumeOrganizationMemberResetAllowance(ctx, org.ID, user.ID, now.AddDate(0, 1, 0)); errConsume == nil {
		t.Fatal("expected status failure")
	}
	saved, errSaved := repo.GetOrganizationMember(ctx, org.ID, user.ID)
	if errSaved != nil || saved.ResetAllowanceAvailable != 2 || !saved.UsageEpoch5h.Equal(now) || !saved.ResetAllowanceRenewsAt.Equal(time.Date(2026, 1, 31, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("rollback: %+v %v", saved, errSaved)
	}
}

func TestOrganizationMemberResetAllowanceRenewalAndFailures(t *testing.T) {
	repo, ctx, closeRepo := newPeriodLimitTestRepo(t)
	defer closeRepo()
	user := createPeriodLimitUser(t, repo, ctx, "renew-member", 0)
	org := createOrganizationTestOrg(t, repo, "renew-org", 0)
	addOrganizationTestMember(t, repo, org.ID, user.ID, OrganizationRoleMember)
	now := time.Date(2026, 1, 31, 15, 59, 0, 0, time.UTC)
	timezone := "Asia/Shanghai"
	if _, errUpdate := repo.UpdateOrganization(ctx, org.ID, UserUpdate{Timezone: &timezone}); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	for _, test := range []struct {
		mode   string
		amount int64
		expiry *time.Time
	}{{"bad", 1, nil}, {ResetAllowanceOneTime, -1, nil}, {ResetAllowanceMonthly, 1, &now}} {
		if _, errSet := repo.SetOrganizationMemberResetAllowance(ctx, org.ID, user.ID, test.mode, test.amount, test.expiry, now); errSet == nil {
			t.Fatalf("accepted %+v", test)
		}
	}
	if _, errSet := repo.SetOrganizationMemberResetAllowance(ctx, org.ID, user.ID, ResetAllowanceMonthly, 1, nil, now); errSet != nil {
		t.Fatal(errSet)
	}
	if _, _, errConsume := repo.ConsumeOrganizationMemberResetAllowance(ctx, org.ID, user.ID, now); !errors.Is(errConsume, ErrResetAllowanceNoWindows) {
		t.Fatalf("no windows: %v", errConsume)
	}
	if _, errUpdate := repo.UpdateOrganizationMember(ctx, org.ID, user.ID, OrganizationMemberUpdate{Limit5hCredits: OptionalFloatUpdate{Set: true, Value: 0}}); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	member, _, errConsume := repo.ConsumeOrganizationMemberResetAllowance(ctx, org.ID, user.ID, now)
	boundary := now.Add(time.Minute)
	if errConsume != nil || member.ResetAllowanceAvailable != 0 || !member.ResetAllowanceRenewsAt.Equal(boundary) {
		t.Fatalf("consume: %+v %v", member, errConsume)
	}
	status := OrganizationMemberResetAllowanceStatusAt(member, boundary)
	if status.Available != 1 || member.ResetAllowanceAvailable != 0 {
		t.Fatalf("read-only renewal: %+v", status)
	}
	member, _, errConsume = repo.ConsumeOrganizationMemberResetAllowance(ctx, org.ID, user.ID, boundary)
	if errConsume != nil || member.ResetAllowanceAvailable != 0 || !member.ResetAllowanceRenewsAt.Equal(time.Date(2026, 2, 28, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("renewal: %+v %v", member, errConsume)
	}
	if _, errSet := repo.SetOrganizationMemberResetAllowance(ctx, org.ID, user.ID, ResetAllowanceOneTime, 1, &boundary, now); errSet != nil {
		t.Fatal(errSet)
	}
	if _, _, errExpired := repo.ConsumeOrganizationMemberResetAllowance(ctx, org.ID, user.ID, boundary); !errors.Is(errExpired, ErrResetAllowanceUnavailable) {
		t.Fatalf("expiry: %v", errExpired)
	}
	if _, errSet := repo.SetOrganizationMemberResetAllowance(ctx, org.ID, user.ID, ResetAllowanceOneTime, 0, nil, now); errSet != nil {
		t.Fatal(errSet)
	}
	if _, _, errZero := repo.ConsumeOrganizationMemberResetAllowance(ctx, org.ID, user.ID, now); !errors.Is(errZero, ErrResetAllowanceUnavailable) {
		t.Fatalf("zero: %v", errZero)
	}
	if _, _, errMissing := repo.ConsumeOrganizationMemberResetAllowance(ctx, org.ID, user.ID+999, now); !errors.Is(errMissing, ErrOrganizationMemberNotFound) {
		t.Fatalf("missing: %v", errMissing)
	}
}
