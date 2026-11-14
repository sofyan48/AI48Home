package cluster

import (
	"math"
	"strings"
	"testing"

	"gorm.io/gorm"
)

func TestBillingMergeBalanceAmountValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		amount float64
	}{
		{"nan", math.NaN()},
		{"positive infinity", math.Inf(1)},
		{"negative infinity", math.Inf(-1)},
		{"rounding overflow", math.MaxFloat64},
		{"below precision", 0.000000004},
		{"zero", 0},
		{"negative", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Validation must happen before accessing the database.
			repo := NewRepository(nil)
			_, errApply := repo.ApplyBillingBalanceRecord(t.Context(), BillingBalanceUpdate{
				UserID: 1, Type: BillingBalanceTypeRecharge, Amount: tc.amount,
			})
			if errApply == nil || !strings.Contains(errApply.Error(), "amount must be positive") {
				t.Fatalf("amount %v: error = %v, want amount validation error", tc.amount, errApply)
			}
		})
	}
}

func TestBillingMergeQuantizedLedgerAndCredits(t *testing.T) {
	repo, closeRepo := newBillingTestRepository(t, t.Context())
	defer closeRepo()
	name, initial := "merge-quantization", 10.0
	user, errCreate := repo.CreateUser(t.Context(), UserUpdate{Username: &name, Credits: &initial})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	for _, tc := range []struct {
		kind   string
		amount float64
		want   float64
	}{
		{BillingBalanceTypeRecharge, 0.123456789, 0.12345679},
		{BillingBalanceTypeDeduct, 0.000000016, 0.00000002},
		{BillingBalanceTypeRecharge, 0.1, 0.1},
	} {
		var record *BillingBalanceRecord
		errTransaction := db.Transaction(func(tx *gorm.DB) error {
			var errApply error
			// Exercise the shared helper used directly by local top-up approval.
			record, errApply = applyBillingBalanceRecordTx(t.Context(), tx, BillingBalanceUpdate{
				UserID: user.ID, Type: tc.kind, Amount: tc.amount, Note: "merge regression", Operator: "admin",
			})
			return errApply
		})
		if errTransaction != nil {
			t.Fatal(errTransaction)
		}
		delta := tc.want
		if tc.kind == BillingBalanceTypeDeduct {
			delta = -delta
		}
		wantAfter := billingRoundAmount(initial + delta)
		if record.Amount != tc.want || record.BalanceBefore != initial || record.BalanceAfter != wantAfter {
			t.Fatalf("ledger = %+v, want amount %v, balance %v -> %v", record, tc.want, initial, wantAfter)
		}
		updated, errGet := repo.GetUser(t.Context(), user.ID)
		if errGet != nil {
			t.Fatal(errGet)
		}
		if updated.Credits != wantAfter {
			t.Fatalf("credits = %v, want %v", updated.Credits, wantAfter)
		}
		initial = wantAfter
	}
}

func TestBillingMergeCreditDeltaUsesCurrentSQLBalance(t *testing.T) {
	repo, closeRepo := newBillingTestRepository(t, t.Context())
	defer closeRepo()
	name, initial := "merge-atomic", 10.0
	user, errCreate := repo.CreateUser(t.Context(), UserUpdate{Username: &name, Credits: &initial})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	// Deterministically change the balance after the helper's read but before its
	// update, demonstrating that SQL uses the current balance, not the stale read.
	changed := false
	if errRegister := db.Callback().Update().Before("gorm:update").Register("billing_merge_balance_change", func(tx *gorm.DB) {
		if changed {
			return
		}
		changed = true
		tx.AddError(tx.Session(&gorm.Session{NewDB: true}).Exec(`UPDATE "user" SET credits = credits + 2 WHERE id = ?`, user.ID).Error)
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	errTransaction := db.Transaction(func(tx *gorm.DB) error {
		before, after, errDelta := billingApplyUserCreditDeltaTx(t.Context(), tx, user.ID, -0.25)
		if errDelta != nil {
			return errDelta
		}
		if before != 12 || after != 11.75 {
			t.Errorf("balance = %v -> %v, want 12 -> 11.75", before, after)
		}
		return nil
	})
	if errTransaction != nil {
		t.Fatal(errTransaction)
	}
	if !changed {
		t.Fatal("balance change callback did not run")
	}
}

func TestBillingMergeFiniteCreditDeltaAndUnlimited(t *testing.T) {
	repo, closeRepo := newBillingTestRepository(t, t.Context())
	defer closeRepo()
	name, initial := "merge-finite", 10.0
	user, errCreate := repo.CreateUser(t.Context(), UserUpdate{Username: &name, Credits: &initial})
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	db, errDB := repo.database()
	if errDB != nil {
		t.Fatal(errDB)
	}
	for _, delta := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), math.MaxFloat64} {
		errTransaction := db.Transaction(func(tx *gorm.DB) error {
			_, _, errDelta := billingApplyUserCreditDeltaTx(t.Context(), tx, user.ID, delta)
			return errDelta
		})
		if errTransaction == nil || !strings.Contains(errTransaction.Error(), "finite") {
			t.Fatalf("delta %v: error = %v, want finite-value error", delta, errTransaction)
		}
	}
	if errUpdate := db.Model(&UserRecord{}).Where("id = ?", user.ID).Update("credits", math.Inf(1)).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	errTransaction := db.Transaction(func(tx *gorm.DB) error {
		_, _, errDelta := billingApplyUserCreditDeltaTx(t.Context(), tx, user.ID, 1)
		return errDelta
	})
	if errTransaction == nil || !strings.Contains(errTransaction.Error(), "resulting balance must be finite") {
		t.Fatalf("non-finite balance: error = %v, want finite-result error", errTransaction)
	}
	if errUpdate := db.Model(&UserRecord{}).Where("id = ?", user.ID).Updates(map[string]any{
		"credits": initial, "credits_unlimited": true,
	}).Error; errUpdate != nil {
		t.Fatal(errUpdate)
	}
	record, errApply := repo.ApplyBillingBalanceRecord(t.Context(), BillingBalanceUpdate{
		UserID: user.ID, Type: BillingBalanceTypeRecharge, Amount: 0.123456789,
	})
	if errApply != nil {
		t.Fatal(errApply)
	}
	if record.Amount != 0.12345679 || record.BalanceBefore != initial || record.BalanceAfter != initial {
		t.Fatalf("unlimited ledger = %+v, want rounded amount and unchanged balance %v", record, initial)
	}
}
