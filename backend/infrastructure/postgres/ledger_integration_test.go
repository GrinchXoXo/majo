package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/ledger"
	"github.com/middleman/backend/internal/monetization"
	"github.com/middleman/backend/internal/payments"
)

// Pilot batch item 8: the ledger as the database enforces it.
func TestIntegration_Ledger(t *testing.T) {
	db := testDB(t, migrationsPath())
	ctx := context.Background()
	repo := NewLedgerRepository(db)
	now := time.Now().UTC().Truncate(time.Second)
	vendorID := uuid.New()
	mustExec(t, db, `INSERT INTO vendors (id, business_name, whatsapp_external_id, status) VALUES ($1, 'Ledger Gadgets', '2348099990001', 'ACTIVE')`, vendorID)

	settings := monetization.Settings{Stage: monetization.StageB, Provider: payments.KindPaymentLink,
		Enabled: map[monetization.Line]bool{monetization.LeadCredits: true}}
	svc := ledger.NewService(settings, repo)

	// Post, read back, idempotent.
	buy := ledger.LeadCreditPurchase(vendorID, 10, 250000, "Bachs", "PAY-1", now)
	saved, created, err := svc.Post(ctx, buy)
	if err != nil || !created || saved == nil || len(saved.Postings) != 2 || saved.VendorID == nil || *saved.VendorID != vendorID {
		t.Fatalf("post: %+v %v %v", saved, created, err)
	}
	again, created, err := svc.Post(ctx, ledger.LeadCreditPurchase(vendorID, 10, 250000, "Bachs", "PAY-1", now))
	if err != nil || created || again.ID != saved.ID {
		t.Fatalf("same key is the same entry: %v %v", created, err)
	}
	if b, _ := repo.Balance(ctx, "majo:cash:bachs"); b != 250000 {
		t.Fatalf("cash %d", b)
	}

	// A reversal is a new entry; the original stays.
	if _, _, err := svc.Post(ctx, ledger.Reverse(saved, "rev:PAY-1", "test charge", "operator:test", now)); err != nil {
		t.Fatal(err)
	}
	if b, _ := repo.Balance(ctx, "majo:cash:bachs"); b != 0 {
		t.Fatalf("reversed cash %d", b)
	}

	// A settlement (also creates majo:bank for the raw checks below).
	if _, _, err := svc.Post(ctx, ledger.ProviderSettlement("Bachs", "SET-1", 100000, 1500, now)); err != nil {
		t.Fatal(err)
	}

	// The database refuses what the application must never do.
	exec := func(stmts ...string) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback() //nolint:errcheck
		for _, s := range stmts {
			if _, err := tx.ExecContext(ctx, s); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	eid := uuid.NewString()
	ins := `INSERT INTO ledger_entries (id, idempotency_key, kind, description, occurred_at, created_by) VALUES ('` + eid + `', 'raw-` + eid + `', 'raw', 'raw', now(), 'test')`
	post := func(code string, amt string) string {
		return `INSERT INTO ledger_postings (entry_id, account_id, amount_kobo, currency) SELECT '` + eid + `', id, ` + amt + `, currency FROM ledger_accounts WHERE code = '` + code + `'`
	}
	for name, c := range map[string]struct {
		stmts []string
		want  string
	}{
		"unbalanced":  {[]string{ins, post("majo:cash:bachs", "100"), post("majo:bank", "-99")}, "does not balance"},
		"one posting": {[]string{ins, post("majo:cash:bachs", "100")}, "at least 2"},
		"no postings": {[]string{ins}, "no postings"},
		"currency": {[]string{ins, `INSERT INTO ledger_postings (entry_id, account_id, amount_kobo, currency) SELECT '` + eid + `', id, 5, 'USD' FROM ledger_accounts WHERE code = 'majo:bank'`},
			"does not match"},
		"zero":     {[]string{ins, post("majo:cash:bachs", "0")}, "amount_kobo"},
		"update":   {[]string{`UPDATE ledger_postings SET amount_kobo = amount_kobo * 2`}, "append-only"},
		"delete":   {[]string{`DELETE FROM ledger_entries`}, "append-only"},
		"truncate": {[]string{`TRUNCATE ledger_postings`}, "append-only"},
	} {
		err := exec(c.stmts...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
	if err := exec(ins, post("majo:cash:bachs", "100"), post("majo:bank", "-100")); err != nil {
		t.Fatalf("a balanced raw entry is fine: %v", err)
	}

	// Customer funds are refused before escrow; an account definition
	// cannot be silently changed.
	d := uuid.New()
	sneaky := ledger.EscrowDeposit(uuid.New(), d, 5000, "Partner", "H-1", now)
	sneaky.RevenueLine = nil
	if _, _, err := svc.Post(ctx, sneaky); !errors.Is(err, ledger.ErrCustomerFunds) {
		t.Fatalf("customer funds: %v", err)
	}
	clash := ledger.LeadCreditPurchase(vendorID, 1, 100, "Bachs", "PAY-2", now)
	clash.Accounts[0].HoldsCustomerFunds = true
	if _, _, err := repo.Post(ctx, clash); err == nil || !strings.Contains(err.Error(), "different") {
		t.Fatalf("account redefinition: %v", err)
	}

	entries, balances, err := repo.Summary(ctx)
	if err != nil || entries != 4 || balances["majo:expense:payment_fees"] != 1500 {
		t.Fatalf("summary: %d %v %v", entries, balances, err)
	}
	var total int64
	_ = db.QueryRow(`SELECT COALESCE(sum(amount_kobo), 0) FROM ledger_postings`).Scan(&total)
	if total != 0 {
		t.Fatalf("the whole ledger sums to zero, got %d", total)
	}
}
