package ledger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/monetization"
	"github.com/middleman/backend/internal/payments"
)

var now = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

// fakeRepo applies postings to in-memory balances.
type fakeRepo struct {
	bal      map[string]int64
	accounts map[string]Account
	keys     map[string]*Entry
}

func newFake() *fakeRepo {
	return &fakeRepo{bal: map[string]int64{}, accounts: map[string]Account{}, keys: map[string]*Entry{}}
}

func (f *fakeRepo) Post(_ context.Context, e *Entry) (*Entry, bool, error) {
	if old, ok := f.keys[e.IdempotencyKey]; ok {
		return old, false, nil
	}
	for _, a := range e.Accounts {
		f.accounts[a.Code] = a
	}
	for _, p := range e.Postings {
		f.bal[p.Account] += p.AmountKobo
	}
	e.ID = uuid.New()
	f.keys[e.IdempotencyKey] = e
	return e, true, nil
}

func (f *fakeRepo) AccountsHoldCustomerFunds(_ context.Context, codes []string) (bool, error) {
	for _, c := range codes {
		if f.accounts[c].HoldsCustomerFunds {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeRepo) Balance(_ context.Context, code string) (int64, error) { return f.bal[code], nil }

func on(stage monetization.Stage, provider payments.Kind, lines ...monetization.Line) monetization.Settings {
	s := monetization.Settings{Stage: stage, Provider: provider, Enabled: map[monetization.Line]bool{}}
	for _, l := range lines {
		s.Enabled[l] = true
	}
	return s
}

func TestValidate(t *testing.T) {
	ok := LeadCreditPurchase(uuid.New(), 10, 300000, "Bachs", "ref1", now)
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := func(mut func(*Entry)) error {
		e := LeadCreditPurchase(uuid.New(), 10, 300000, "Bachs", "ref1", now)
		mut(e)
		return e.Validate()
	}
	for name, mut := range map[string]func(*Entry){
		"unbalanced":  func(e *Entry) { e.Postings[0].AmountKobo++ },
		"one posting": func(e *Entry) { e.Postings = e.Postings[:1] },
		"zero":        func(e *Entry) { e.Postings = append(e.Postings, Posting{Account: "x", Currency: NGN}) },
		"no key":      func(e *Entry) { e.IdempotencyKey = "" },
		"no time":     func(e *Entry) { e.OccurredAt = time.Time{} },
		"no currency": func(e *Entry) { e.Postings[0].Currency = "" },
		"two currencies": func(e *Entry) {
			e.Postings = append(e.Postings, Posting{Account: "usd", AmountKobo: 5, Currency: "USD"}, Posting{Account: "ngn", AmountKobo: -5, Currency: NGN})
		},
	} {
		if bad(mut) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestEveryJournalEntryBalances(t *testing.T) {
	v, b, d := uuid.New(), uuid.New(), uuid.New()
	svc, _ := ServicePayment(monetization.VendorPro, 300000, "Bachs", "p1", now)
	rel, _ := EscrowRelease(b, v, d, 5000000, 400000, "h1", now)
	for _, e := range []*Entry{
		LeadCreditPurchase(v, 10, 250000, "Bachs", "p0", now), LeadCreditSpend(v, d, 25000, now), svc,
		ProviderSettlement("Bachs", "s1", 300000, 4500, now), SplitFee(d, 150000, "Bachs", "p2", now),
		EscrowDeposit(b, d, 5000000, "Partner", "h1", now), rel, EscrowPayout(v, 4600000, 400000, "Partner", "o1", now),
		EscrowRefund(b, d, 5000000, "Partner", "r1", now),
	} {
		if err := e.Validate(); err != nil {
			t.Errorf("%s: %v", e.Kind, err)
		}
		for _, p := range e.Postings {
			found := false
			for _, a := range e.Accounts {
				found = found || a.Code == p.Account
			}
			if !found {
				t.Errorf("%s: posting to %s without the account definition", e.Kind, p.Account)
			}
		}
	}
	if _, err := ServicePayment(monetization.Escrow, 1, "x", "y", now); err == nil {
		t.Fatal("escrow is not a service payment")
	}
	if _, err := EscrowRelease(b, v, d, 100, 100, "h", now); err == nil {
		t.Fatal("commission must be below the deposit")
	}
}

func TestPolicy(t *testing.T) {
	ctx := context.Background()
	v := uuid.New()

	// Stage A, everything off: nothing posts.
	s := NewService(monetization.Default(), newFake())
	if _, _, err := s.Post(ctx, LeadCreditPurchase(v, 10, 250000, "Bachs", "p0", now)); !errors.Is(err, ErrLineOff) {
		t.Fatalf("line off: %v", err)
	}
	// A settlement belongs to no line, but Majo has no provider in Stage A
	// anyway; the ledger itself allows it (it is Majo's own money).
	if _, _, err := s.Post(ctx, ProviderSettlement("Bachs", "s0", 1000, 15, now)); err != nil {
		t.Fatalf("own money: %v", err)
	}

	// Stage B with lead credits on: credits post; escrow never does.
	repo := newFake()
	s = NewService(on(monetization.StageB, payments.KindPaymentLink, monetization.LeadCredits), repo)
	if _, created, err := s.Post(ctx, LeadCreditPurchase(v, 10, 250000, "Bachs", "p1", now)); err != nil || !created {
		t.Fatalf("credits: %v", err)
	}
	if _, created, _ := s.Post(ctx, LeadCreditPurchase(v, 10, 250000, "Bachs", "p1", now)); created {
		t.Fatal("same payment twice is one entry")
	}
	d := uuid.New()
	if _, _, err := s.Post(ctx, LeadCreditSpend(v, d, 25000, now)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Balance(ctx, CreditsAccount(v).Code); got != -225000 {
		t.Fatalf("9 credits left (credit balance), got %d", got)
	}
	if got, _ := s.Balance(ctx, RevenueAccount(monetization.LeadCredits).Code); got != -25000 {
		t.Fatalf("one credit earned, got %d", got)
	}
	if _, _, err := s.Post(ctx, EscrowDeposit(uuid.New(), d, 5000, "Partner", "h1", now)); !errors.Is(err, ErrLineOff) {
		t.Fatalf("escrow off: %v", err)
	}
	// Even an entry with no line may not touch customer funds before Stage C.
	sneaky := EscrowDeposit(uuid.New(), d, 5000, "Partner", "h2", now)
	sneaky.RevenueLine = nil
	if _, _, err := s.Post(ctx, sneaky); !errors.Is(err, ErrCustomerFunds) {
		t.Fatalf("customer funds: %v", err)
	}
	repo.accounts["customer:escrow:partner"] = EscrowAccount("Partner") // already exists in the database
	sneaky2 := &Entry{IdempotencyKey: "k", Kind: "x", Description: "x", OccurredAt: now, CreatedBy: "t",
		Postings: []Posting{{Account: "customer:escrow:partner", AmountKobo: 5, Currency: NGN}, {Account: "majo:bank", AmountKobo: -5, Currency: NGN}}}
	if _, _, err := s.Post(ctx, sneaky2); !errors.Is(err, ErrCustomerFunds) {
		t.Fatalf("existing customer-funds account: %v", err)
	}

	// Stage C with escrow on: the whole deposit lifecycle nets to zero
	// except Majo's commission.
	repo = newFake()
	s = NewService(on(monetization.StageC, payments.KindEscrowPartner, monetization.Escrow), repo)
	b := uuid.New()
	rel, _ := EscrowRelease(b, v, d, 5000000, 400000, "h1", now)
	for _, e := range []*Entry{EscrowDeposit(b, d, 5000000, "Partner", "h1", now), rel, EscrowPayout(v, 4600000, 400000, "Partner", "o1", now)} {
		if _, _, err := s.Post(ctx, e); err != nil {
			t.Fatalf("%s: %v", e.Kind, err)
		}
	}
	for code, want := range map[string]int64{
		EscrowAccount("Partner").Code: 0, BuyerDepositAccount(b).Code: 0, VendorPayableAccount(v).Code: 0,
		RevenueAccount(monetization.Escrow).Code: -400000, CashAccount("Partner").Code: 400000,
	} {
		if got := repo.bal[code]; got != want {
			t.Errorf("%s = %d, want %d", code, got, want)
		}
	}
}

func TestReverse(t *testing.T) {
	e := LeadCreditPurchase(uuid.New(), 10, 250000, "Bachs", "p1", now)
	e.ID = uuid.New()
	r := Reverse(e, "rev:p1", "duplicate charge", "operator:Godyes", now)
	if err := r.Validate(); err != nil || *r.ReversesEntryID != e.ID || r.Postings[0].AmountKobo != -250000 {
		t.Fatalf("%+v %v", r, err)
	}
}

// Independent review: a revenue account cannot be posted to with the line
// off by leaving RevenueLine empty; same key + other content is a conflict.
func TestRevenueAccountNeedsItsLine(t *testing.T) {
	s := NewService(on(monetization.StageB, payments.KindPaymentLink, monetization.LeadCredits), newFake())
	e, _ := ServicePayment(monetization.VendorPro, 1000, "Bachs", "p9", now)
	e.RevenueLine = nil
	if _, _, err := s.Post(context.Background(), e); !errors.Is(err, ErrLineOff) {
		t.Fatalf("VENDOR_PRO is off: %v", err)
	}
	a := LeadCreditPurchase(uuid.New(), 1, 100, "Bachs", "p1", now)
	b := LeadCreditPurchase(*a.VendorID, 1, 100, "Bachs", "p1", now)
	if !SameEntry(a, b) {
		t.Fatal("same content")
	}
	b.Postings[0].AmountKobo, b.Postings[1].AmountKobo = 200, -200
	if SameEntry(a, b) {
		t.Fatal("different amount")
	}
}
