// Package ledger is Majo's double-entry ledger (pilot batch item 8).
//
// Designed now, written to later: while every revenue line is off nothing
// posts. The schema (migration 0019) enforces balance, currency and
// append-only at the database; this package adds the business rules:
//
//   - an entry for a revenue line that is switched off is refused;
//   - an entry touching an account that holds customer funds (escrow
//     deposits, split pass-through) is refused until split payments or escrow
//     are switched on (Stage C, or Stage B with split): before that Majo
//     only books its own income.
//
// Sign convention: a posting's amount is positive for a debit, negative for
// a credit; every entry sums to zero per currency. Amounts are integer kobo.
// See docs/architecture/ledger.md for the chart of accounts and the journal
// of every revenue line.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/monetization"
)

// AccountType of an account.
type AccountType string

const (
	Asset     AccountType = "ASSET"
	Liability AccountType = "LIABILITY"
	Equity    AccountType = "EQUITY"
	Revenue   AccountType = "REVENUE"
	Expense   AccountType = "EXPENSE"
)

// Owner of an account.
const (
	OwnerMajo     = "MAJO"
	OwnerVendor   = "VENDOR"
	OwnerBuyer    = "BUYER"
	OwnerProvider = "PROVIDER"
)

// NGN is the only currency in use.
const NGN = "NGN"

// Account in the chart of accounts.
type Account struct {
	ID                 uuid.UUID   `json:"id"`
	Code               string      `json:"code"`
	Name               string      `json:"name"`
	Type               AccountType `json:"type"`
	OwnerType          string      `json:"owner_type"`
	OwnerID            *uuid.UUID  `json:"owner_id,omitempty"`
	Currency           string      `json:"currency"`
	HoldsCustomerFunds bool        `json:"holds_customer_funds"`
}

// Posting is one line of an entry: AmountKobo > 0 debits the account,
// < 0 credits it.
type Posting struct {
	Account    string `json:"account"` // account code
	AmountKobo int64  `json:"amount_kobo"`
	Currency   string `json:"currency"`
}

// Entry is one balanced journal entry.
type Entry struct {
	ID              uuid.UUID          `json:"id"`
	IdempotencyKey  string             `json:"idempotency_key"`
	Kind            string             `json:"kind"`
	RevenueLine     *monetization.Line `json:"revenue_line,omitempty"`
	Description     string             `json:"description"`
	DealID          *uuid.UUID         `json:"deal_id,omitempty"`
	VendorID        *uuid.UUID         `json:"vendor_id,omitempty"`
	BuyerID         *uuid.UUID         `json:"buyer_id,omitempty"`
	PaymentRef      string             `json:"payment_ref,omitempty"`
	ReversesEntryID *uuid.UUID         `json:"reverses_entry_id,omitempty"`
	OccurredAt      time.Time          `json:"occurred_at"`
	CreatedBy       string             `json:"created_by"`
	Postings        []Posting          `json:"postings"`
	// Accounts the entry needs; created on first use (codes must match).
	Accounts []Account `json:"-"`
}

// Validate checks an entry before it reaches the database (which checks
// again).
func (e *Entry) Validate() error {
	switch {
	case strings.TrimSpace(e.IdempotencyKey) == "":
		return errors.New("ledger: idempotency key required")
	case strings.TrimSpace(e.Kind) == "":
		return errors.New("ledger: kind required")
	case strings.TrimSpace(e.Description) == "":
		return errors.New("ledger: description required")
	case strings.TrimSpace(e.CreatedBy) == "":
		return errors.New("ledger: created_by required")
	case e.OccurredAt.IsZero():
		return errors.New("ledger: occurred_at required")
	case len(e.Postings) < 2:
		return fmt.Errorf("ledger: %d posting(s); at least 2 are required", len(e.Postings))
	}
	sums := map[string]int64{}
	for _, p := range e.Postings {
		if p.AmountKobo == 0 {
			return fmt.Errorf("ledger: zero posting to %s", p.Account)
		}
		if p.Account == "" || len(p.Currency) != 3 {
			return errors.New("ledger: posting needs an account and a currency")
		}
		sums[p.Currency] += p.AmountKobo
	}
	for cur, s := range sums {
		if s != 0 {
			return fmt.Errorf("ledger: entry does not balance in %s (off by %d kobo)", cur, s)
		}
	}
	return nil
}

// ErrLineOff: the entry belongs to a revenue line that is switched off.
var ErrLineOff = errors.New("ledger: revenue line is switched off")

// ErrCustomerFunds: the entry touches customer funds before they are allowed.
var ErrCustomerFunds = errors.New("ledger: customer funds are not allowed before split payments or escrow are switched on")

// Repository persists entries (infrastructure/postgres.LedgerRepository).
type Repository interface {
	// Post writes the entry, its postings and any missing accounts in one
	// transaction. An entry with the same idempotency key is returned as it
	// was first written, with created=false.
	Post(ctx context.Context, e *Entry) (saved *Entry, created bool, err error)
	// AccountsHoldCustomerFunds reports whether any of these existing
	// accounts holds customer funds.
	AccountsHoldCustomerFunds(ctx context.Context, codes []string) (bool, error)
	Balance(ctx context.Context, code string) (int64, error)
}

// Service posts entries under the monetization settings.
type Service struct {
	settings monetization.Settings
	repo     Repository
}

// NewService builds the ledger service.
func NewService(settings monetization.Settings, repo Repository) *Service {
	return &Service{settings: settings, repo: repo}
}

// Post validates and writes an entry.
func (s *Service) Post(ctx context.Context, e *Entry) (*Entry, bool, error) {
	if err := e.Validate(); err != nil {
		return nil, false, err
	}
	if e.RevenueLine != nil && !s.settings.On(*e.RevenueLine) {
		return nil, false, fmt.Errorf("%w: %s", ErrLineOff, *e.RevenueLine)
	}
	if !s.settings.CustomerFundsAllowed() {
		for _, a := range e.Accounts {
			if a.HoldsCustomerFunds {
				return nil, false, fmt.Errorf("%w (account %s)", ErrCustomerFunds, a.Code)
			}
		}
		codes := make([]string, 0, len(e.Postings))
		for _, p := range e.Postings {
			codes = append(codes, p.Account)
		}
		held, err := s.repo.AccountsHoldCustomerFunds(ctx, codes)
		if err != nil {
			return nil, false, err
		}
		if held {
			return nil, false, ErrCustomerFunds
		}
	}
	return s.repo.Post(ctx, e)
}

// Balance of an account, debit-positive.
func (s *Service) Balance(ctx context.Context, code string) (int64, error) {
	return s.repo.Balance(ctx, code)
}

// Reverse builds the entry that cancels e (same postings, opposite signs).
func Reverse(e *Entry, key, reason, by string, at time.Time) *Entry {
	id := e.ID
	r := &Entry{IdempotencyKey: key, Kind: e.Kind + "_reversal", RevenueLine: e.RevenueLine,
		Description: "Reversal: " + reason, DealID: e.DealID, VendorID: e.VendorID, BuyerID: e.BuyerID,
		PaymentRef: e.PaymentRef, ReversesEntryID: &id, OccurredAt: at, CreatedBy: by}
	for _, p := range e.Postings {
		r.Postings = append(r.Postings, Posting{Account: p.Account, AmountKobo: -p.AmountKobo, Currency: p.Currency})
	}
	return r
}
