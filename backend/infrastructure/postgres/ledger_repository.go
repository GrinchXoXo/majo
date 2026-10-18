package postgres

// Pilot batch item 8: the double-entry ledger (migration 0019). The schema
// enforces balance, currency and append-only; this repository writes an
// entry, its postings and any missing accounts in one transaction.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/middleman/backend/internal/ledger"
	"github.com/middleman/backend/internal/monetization"
)

// LedgerRepository implements ledger.Repository.
type LedgerRepository struct{ db *sql.DB }

func NewLedgerRepository(db *sql.DB) *LedgerRepository { return &LedgerRepository{db: db} }

func nullUUID(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

// Post writes the entry once per idempotency key.
func (r *LedgerRepository) Post(ctx context.Context, e *ledger.Entry) (*ledger.Entry, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback() //nolint:errcheck

	for _, a := range e.Accounts {
		if err := ensureAccount(ctx, tx, a); err != nil {
			return nil, false, err
		}
	}
	var line any
	if e.RevenueLine != nil {
		line = string(*e.RevenueLine)
	}
	var id string
	err = tx.QueryRowContext(ctx, `INSERT INTO ledger_entries (idempotency_key, kind, revenue_line, description, deal_id, vendor_id, buyer_id,
			payment_ref, reverses_entry_id, occurred_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), $9, $10, $11)
		ON CONFLICT (idempotency_key) DO NOTHING RETURNING id`,
		e.IdempotencyKey, e.Kind, line, e.Description, nullUUID(e.DealID), nullUUID(e.VendorID), nullUUID(e.BuyerID),
		e.PaymentRef, nullUUID(e.ReversesEntryID), e.OccurredAt, e.CreatedBy).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		existing, err := r.Entry(ctx, e.IdempotencyKey)
		if err != nil || existing == nil {
			return existing, false, err
		}
		// Same key, different content: a bug or a reused reference, never
		// "already done".
		if !ledger.SameEntry(existing, e) {
			return nil, false, fmt.Errorf("%w: %s", ledger.ErrIdempotencyConflict, e.IdempotencyKey)
		}
		return existing, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("ledger: inserting entry: %w", err)
	}
	for _, p := range e.Postings {
		res, err := tx.ExecContext(ctx, `INSERT INTO ledger_postings (entry_id, account_id, amount_kobo, currency)
			SELECT $1, id, $3, $4 FROM ledger_accounts WHERE code = $2`, id, p.Account, p.AmountKobo, p.Currency)
		if err != nil {
			return nil, false, fmt.Errorf("ledger: posting to %s: %w", p.Account, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil, false, fmt.Errorf("ledger: unknown account %s", p.Account)
		}
	}
	if err := tx.Commit(); err != nil { // the balance trigger runs here
		return nil, false, fmt.Errorf("ledger: %w", err)
	}
	saved, err := r.Entry(ctx, e.IdempotencyKey)
	return saved, true, err
}

func ensureAccount(ctx context.Context, tx *sql.Tx, a ledger.Account) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_accounts (code, name, type, owner_type, owner_id, currency, holds_customer_funds)
		VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (code) DO NOTHING`,
		a.Code, a.Name, string(a.Type), a.OwnerType, nullUUID(a.OwnerID), a.Currency, a.HoldsCustomerFunds); err != nil {
		return fmt.Errorf("ledger: account %s: %w", a.Code, err)
	}
	var typ, cur string
	var customer bool
	if err := tx.QueryRowContext(ctx, `SELECT type, currency, holds_customer_funds FROM ledger_accounts WHERE code = $1`, a.Code).
		Scan(&typ, &cur, &customer); err != nil {
		return err
	}
	if typ != string(a.Type) || cur != a.Currency || customer != a.HoldsCustomerFunds {
		return fmt.Errorf("ledger: account %s exists with different type/currency/customer-funds flag", a.Code)
	}
	return nil
}

// Entry loads an entry with its postings by idempotency key (nil if none).
func (r *LedgerRepository) Entry(ctx context.Context, key string) (*ledger.Entry, error) {
	var e ledger.Entry
	var id string
	var line, payRef sql.NullString
	var deal, vendor, buyer, rev sql.NullString
	err := r.db.QueryRowContext(ctx, `SELECT id, idempotency_key, kind, revenue_line, description, deal_id, vendor_id, buyer_id,
			payment_ref, reverses_entry_id, occurred_at, created_by
		FROM ledger_entries WHERE idempotency_key = $1`, key).
		Scan(&id, &e.IdempotencyKey, &e.Kind, &line, &e.Description, &deal, &vendor, &buyer, &payRef, &rev, &e.OccurredAt, &e.CreatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.ID, _ = uuid.Parse(id)
	e.OccurredAt = e.OccurredAt.UTC()
	if line.Valid {
		l := monetization.Line(line.String)
		e.RevenueLine = &l
	}
	e.PaymentRef = payRef.String
	for dst, src := range map[**uuid.UUID]sql.NullString{&e.DealID: deal, &e.VendorID: vendor, &e.BuyerID: buyer, &e.ReversesEntryID: rev} {
		if src.Valid {
			u, _ := uuid.Parse(src.String)
			*dst = &u
		}
	}
	rows, err := r.db.QueryContext(ctx, `SELECT a.code, p.amount_kobo, p.currency FROM ledger_postings p
		JOIN ledger_accounts a ON a.id = p.account_id WHERE p.entry_id = $1 ORDER BY p.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p ledger.Posting
		if err := rows.Scan(&p.Account, &p.AmountKobo, &p.Currency); err != nil {
			return nil, err
		}
		e.Postings = append(e.Postings, p)
	}
	return &e, rows.Err()
}

// AccountsHoldCustomerFunds: does any of these existing accounts hold
// customer funds?
func (r *LedgerRepository) AccountsHoldCustomerFunds(ctx context.Context, codes []string) (bool, error) {
	var held bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM ledger_accounts WHERE code = ANY($1) AND holds_customer_funds)`,
		pq.Array(codes)).Scan(&held)
	return held, err
}

// Balance of an account, debit-positive (0 for an unknown account).
func (r *LedgerRepository) Balance(ctx context.Context, code string) (int64, error) {
	var b int64
	err := r.db.QueryRowContext(ctx, `SELECT COALESCE(sum(p.amount_kobo), 0) FROM ledger_postings p
		JOIN ledger_accounts a ON a.id = p.account_id WHERE a.code = $1`, code).Scan(&b)
	return b, err
}

// Summary for the dashboard: account balances and the number of entries.
func (r *LedgerRepository) Summary(ctx context.Context) (entries int, balances map[string]int64, err error) {
	if err = r.db.QueryRowContext(ctx, `SELECT count(*) FROM ledger_entries`).Scan(&entries); err != nil {
		return 0, nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT code, debit_minus_credit_kobo FROM ledger_balances ORDER BY code`)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	balances = map[string]int64{}
	for rows.Next() {
		var c string
		var b int64
		if err := rows.Scan(&c, &b); err != nil {
			return 0, nil, err
		}
		balances[c] = b
	}
	return entries, balances, rows.Err()
}
