package transaction

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/audit"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/offer"
)

type fakeTxRepo struct {
	selections   map[uuid.UUID]*Selection
	transactions map[uuid.UUID]*Transaction
}

func newFakeTxRepo() *fakeTxRepo {
	return &fakeTxRepo{selections: map[uuid.UUID]*Selection{}, transactions: map[uuid.UUID]*Transaction{}}
}
func (f *fakeTxRepo) CreateSelection(ctx context.Context, s *Selection) (*Selection, error) {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	f.selections[s.ID] = s
	return s, nil
}
func (f *fakeTxRepo) FindSelectionByBuyerRequest(ctx context.Context, buyerRequestID uuid.UUID) (*Selection, error) {
	for _, s := range f.selections {
		if s.BuyerRequestID == buyerRequestID {
			return s, nil
		}
	}
	return nil, nil
}
func (f *fakeTxRepo) CreateTransaction(ctx context.Context, t *Transaction) (*Transaction, error) {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	f.transactions[t.ID] = t
	return t, nil
}
func (f *fakeTxRepo) FindTransactionByID(ctx context.Context, id uuid.UUID) (*Transaction, error) {
	return f.transactions[id], nil
}
func (f *fakeTxRepo) FindTransactionBySelection(ctx context.Context, selectionID uuid.UUID) (*Transaction, error) {
	for _, t := range f.transactions {
		if t.BuyerSelectionID == selectionID {
			return t, nil
		}
	}
	return nil, nil
}

type fakeOfferRepoForTx struct{ offers map[uuid.UUID]*offer.Offer }

func (f *fakeOfferRepoForTx) Create(ctx context.Context, o *offer.Offer) (*offer.Offer, error) { return o, nil }
func (f *fakeOfferRepoForTx) Update(ctx context.Context, id uuid.UUID, fields offer.Fields) (*offer.Offer, error) {
	return nil, nil
}
func (f *fakeOfferRepoForTx) FindByID(ctx context.Context, id uuid.UUID) (*offer.Offer, error) {
	return f.offers[id], nil
}
func (f *fakeOfferRepoForTx) ListByBuyerRequest(ctx context.Context, buyerRequestID uuid.UUID) ([]*offer.Offer, error) {
	return nil, nil
}
func (f *fakeOfferRepoForTx) FindByVendorResponseID(ctx context.Context, vendorResponseID uuid.UUID) (*offer.Offer, error) {
	return nil, nil
}

type fakeEventsForTx struct{ events []*event.Event }

func (f *fakeEventsForTx) Write(ctx context.Context, e *event.Event) error {
	f.events = append(f.events, e)
	return nil
}

type fakeAuditsForTx struct{ entries []*audit.Entry }

func (f *fakeAuditsForTx) Write(ctx context.Context, e *audit.Entry) error {
	f.entries = append(f.entries, e)
	return nil
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func i64p(i int64) *int64      { return &i }

func TestSelectOffer_CreatesSelectionAndTransaction(t *testing.T) {
	repo := newFakeTxRepo()
	buyerReqID, offerID, buyerID, vendorID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	offerRepo := &fakeOfferRepoForTx{offers: map[uuid.UUID]*offer.Offer{
		offerID: {ID: offerID, BuyerRequestID: buyerReqID, VendorID: vendorID, Price: i64p(620000)},
	}}
	events := &fakeEventsForTx{}
	audits := &fakeAuditsForTx{}
	svc := NewService(repo, offerRepo, events, audits, testLogger())

	selection, tx, err := svc.SelectOffer(context.Background(), buyerReqID, offerID, buyerID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if selection.OfferID != offerID {
		t.Error("expected selection to reference the chosen offer")
	}
	if tx.PaymentStatus != PaymentUnpaid {
		t.Errorf("PRD 3 §20: selecting an offer must NEVER imply payment — expected UNPAID, got %s", tx.PaymentStatus)
	}
	if tx.Status != StatusCreated {
		t.Errorf("expected transaction status CREATED, got %s", tx.Status)
	}
	if tx.GrossAmount != 620000 {
		t.Errorf("expected gross amount 620000, got %d", tx.GrossAmount)
	}
	expectedFee := int64(620000) * DefaultTakeRateBps / 10000
	if tx.PlatformFee != expectedFee {
		t.Errorf("expected platform fee %d (4%%), got %d", expectedFee, tx.PlatformFee)
	}
	if tx.VendorAmount != tx.GrossAmount-tx.PlatformFee {
		t.Errorf("expected vendor_amount = gross - fee, got vendor=%d gross=%d fee=%d", tx.VendorAmount, tx.GrossAmount, tx.PlatformFee)
	}

	// PRD 3 §20/§38-18: selection must be auditable.
	if len(audits.entries) != 1 {
		t.Fatalf("expected 1 audit entry for the selection, got %d", len(audits.entries))
	}
	if audits.entries[0].Action != "OFFER_ACCEPTED" {
		t.Errorf("expected OFFER_ACCEPTED audit action, got %s", audits.entries[0].Action)
	}

	foundSelectionEvent, foundTxEvent := false, false
	for _, e := range events.events {
		if e.EventType == event.BuyerSelectionCreated {
			foundSelectionEvent = true
		}
		if e.EventType == event.TransactionCreated {
			foundTxEvent = true
		}
	}
	if !foundSelectionEvent || !foundTxEvent {
		t.Errorf("expected both BUYER_SELECTION_CREATED and TRANSACTION_CREATED events, got selection=%v tx=%v", foundSelectionEvent, foundTxEvent)
	}
}

func TestSelectOffer_RejectsOfferFromDifferentBuyerRequest(t *testing.T) {
	repo := newFakeTxRepo()
	realBuyerReq, wrongBuyerReq := uuid.New(), uuid.New()
	offerID, buyerID := uuid.New(), uuid.New()
	offerRepo := &fakeOfferRepoForTx{offers: map[uuid.UUID]*offer.Offer{
		offerID: {ID: offerID, BuyerRequestID: realBuyerReq, Price: i64p(100000)},
	}}
	svc := NewService(repo, offerRepo, &fakeEventsForTx{}, &fakeAuditsForTx{}, testLogger())

	_, _, err := svc.SelectOffer(context.Background(), wrongBuyerReq, offerID, buyerID)
	if err == nil {
		t.Fatal("expected an error when selecting an offer that belongs to a different buyer request")
	}
}

func TestSelectOffer_TransactionBoundaryDoesNotImplyPaymentEvenWithZeroPrice(t *testing.T) {
	// Edge case: an offer with no price set (price-on-request) must still
	// produce a well-formed, clearly-UNPAID transaction shell rather than
	// erroring or defaulting to some ambiguous paid state.
	repo := newFakeTxRepo()
	buyerReqID, offerID, buyerID := uuid.New(), uuid.New(), uuid.New()
	offerRepo := &fakeOfferRepoForTx{offers: map[uuid.UUID]*offer.Offer{
		offerID: {ID: offerID, BuyerRequestID: buyerReqID}, // no price
	}}
	svc := NewService(repo, offerRepo, &fakeEventsForTx{}, &fakeAuditsForTx{}, testLogger())

	_, tx, err := svc.SelectOffer(context.Background(), buyerReqID, offerID, buyerID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tx.PaymentStatus != PaymentUnpaid || tx.GrossAmount != 0 {
		t.Errorf("expected UNPAID/zero-amount transaction, got status=%s amount=%d", tx.PaymentStatus, tx.GrossAmount)
	}
}
