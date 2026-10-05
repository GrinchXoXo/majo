package transaction

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/audit"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/offer"
	"github.com/middleman/backend/internal/request"
)

// DefaultTakeRateBps is the illustrative 4% take rate mentioned in the
// product docs (Master PRD §31: "3-5% take rate... 4% as a useful
// working example"). Expressed in basis points so the math stays in
// integers. This is NOT a business decision baked into the domain model —
// it's a single named constant, trivially replaceable, and Phase 3 never
// actually charges anyone (no PaymentProvider is invoked).
const DefaultTakeRateBps = 400 // 4.00%

// RequestReader is the minimal BuyerRequest access the selection guards
// need (ownership + confirmed invariant).
type RequestReader interface {
	FindByID(ctx context.Context, id uuid.UUID) (*request.BuyerRequest, error)
}

// Service creates the transaction boundary described in PRD 3 §20-24:
// selecting an offer is a distinct, auditable step from a transaction
// existing, which is itself distinct from any money having moved
// (PaymentStatus starts and stays UNPAID — no PaymentProvider is called
// here).
type Service struct {
	repo     Repository
	offers   offer.Repository
	requests RequestReader
	events   event.Writer
	audits   audit.Writer
	logger   *slog.Logger
	now      func() time.Time
}

func NewService(repo Repository, offers offer.Repository, requests RequestReader, events event.Writer, audits audit.Writer, logger *slog.Logger) *Service {
	return &Service{repo: repo, offers: offers, requests: requests, events: events, audits: audits, logger: logger, now: time.Now}
}

// SelectionResult is the outcome of a selection attempt.
type SelectionResult struct {
	Selection   *Selection
	Transaction *Transaction
	// AlreadyRecorded is true when this exact offer had already been
	// selected for this request (idempotent repeat: duplicate webhook,
	// buyer sending "1" twice, operator double-click). Nothing new was
	// written.
	AlreadyRecorded bool
}

// SelectOffer records a buyer's choice and creates the Transaction shell.
// Kept for existing callers; see Select for the full result.
func (s *Service) SelectOffer(ctx context.Context, buyerRequestID, offerID, buyerID uuid.UUID) (*Selection, *Transaction, error) {
	res, err := s.Select(ctx, buyerRequestID, offerID, buyerID)
	if err != nil {
		if res != nil {
			return res.Selection, res.Transaction, err
		}
		return nil, nil, err
	}
	return res.Selection, res.Transaction, nil
}

// Select enforces, in order (Phase 3.1):
//  1. the BuyerRequest exists and is CONFIRMED (§3A-2 invariant);
//  2. the selecting buyer owns the request;
//  3. idempotency: if the request already has an ACTIVE selection, the same
//     offer returns the existing selection/transaction (AlreadyRecorded),
//     a different offer returns ErrAlreadySelected (with the existing
//     selection in the result so callers can tell the buyer what's held);
//  4. the offer belongs to the request and is still selectable;
//  5. selection + transaction + offer ACCEPTED are written atomically; a
//     concurrent duplicate loses on the DB unique index and is resolved
//     through step 3 again.
//
// Per PRD 3 §20 this never implies payment: PaymentStatus is UNPAID and no
// PaymentProvider is invoked.
func (s *Service) Select(ctx context.Context, buyerRequestID, offerID, buyerID uuid.UUID) (*SelectionResult, error) {
	req, err := s.requests.FindByID(ctx, buyerRequestID)
	if err != nil {
		return nil, fmt.Errorf("loading buyer request: %w", err)
	}
	if req == nil {
		return nil, fmt.Errorf("buyer request %s not found", buyerRequestID)
	}
	if err := req.EnsureConfirmed(); err != nil {
		return nil, err
	}
	if req.BuyerID != buyerID {
		return nil, ErrNotRequestOwner
	}

	if res, done, err := s.resolveExistingSelection(ctx, buyerRequestID, offerID); done {
		return res, err
	}

	o, err := s.offers.FindByID(ctx, offerID)
	if err != nil {
		return nil, fmt.Errorf("loading offer: %w", err)
	}
	if o == nil || o.BuyerRequestID != buyerRequestID {
		return nil, fmt.Errorf("offer %s does not belong to buyer request %s: %w", offerID, buyerRequestID, ErrOfferNotFound)
	}
	if !o.IsSelectable(s.now()) {
		return nil, ErrOfferNotSelectable
	}

	gross := int64(0)
	if o.Price != nil {
		gross = *o.Price
	}
	platformFee := gross * DefaultTakeRateBps / 10000
	vendorAmount := gross - platformFee

	selection, tx, err := s.repo.CreateSelectionWithTransaction(ctx,
		&Selection{BuyerRequestID: buyerRequestID, OfferID: offerID, BuyerID: buyerID, Status: SelectionActive},
		&Transaction{
			BuyerRequestID: buyerRequestID, OfferID: offerID,
			BuyerID: buyerID, VendorID: o.VendorID, SupplyListingID: o.SupplyListingID,
			Status: StatusCreated, PaymentStatus: PaymentUnpaid, FulfilmentStatus: FulfilmentPending,
			GrossAmount: gross, PlatformFee: platformFee, VendorAmount: vendorAmount,
		},
	)
	if errors.Is(err, ErrSelectionExists) {
		// Lost a race with a concurrent identical/competing selection —
		// the DB constraint held; resolve against what won.
		if res, done, rErr := s.resolveExistingSelection(ctx, buyerRequestID, offerID); done {
			return res, rErr
		}
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("creating selection and transaction: %w", err)
	}

	s.recordSelection(ctx, selection, tx, buyerID)
	return &SelectionResult{Selection: selection, Transaction: tx}, nil
}

// resolveExistingSelection implements the idempotency backstop. done=false
// means there is no existing selection and the caller should proceed.
func (s *Service) resolveExistingSelection(ctx context.Context, buyerRequestID, offerID uuid.UUID) (*SelectionResult, bool, error) {
	existing, err := s.repo.FindSelectionByBuyerRequest(ctx, buyerRequestID)
	if err != nil {
		return nil, true, fmt.Errorf("checking existing selection: %w", err)
	}
	if existing == nil {
		return nil, false, nil
	}
	tx, err := s.repo.FindTransactionBySelection(ctx, existing.ID)
	if err != nil {
		return nil, true, fmt.Errorf("loading existing transaction: %w", err)
	}
	res := &SelectionResult{Selection: existing, Transaction: tx, AlreadyRecorded: true}
	if existing.OfferID != offerID {
		return res, true, ErrAlreadySelected
	}
	return res, true, nil
}

func (s *Service) recordSelection(ctx context.Context, selection *Selection, tx *Transaction, buyerID uuid.UUID) {
	if err := s.events.Write(ctx, &event.Event{
		EventType: event.BuyerSelectionCreated, EntityType: "buyer_selection", EntityID: &selection.ID,
		ActorType: event.ActorBuyer, ActorID: &buyerID,
		Payload: map[string]any{"offer_id": selection.OfferID.String(), "buyer_request_id": selection.BuyerRequestID.String()},
	}); err != nil {
		s.logger.Error("writing BUYER_SELECTION_CREATED event", "error", err)
	}

	offerID := selection.OfferID
	if err := s.events.Write(ctx, &event.Event{
		EventType: event.OfferAccepted, EntityType: "offer", EntityID: &offerID,
		ActorType: event.ActorBuyer, ActorID: &buyerID,
		Payload: map[string]any{"selection_id": selection.ID.String(), "buyer_request_id": selection.BuyerRequestID.String()},
	}); err != nil {
		s.logger.Error("writing OFFER_ACCEPTED event", "error", err)
	}

	if err := s.audits.Write(ctx, &audit.Entry{
		ActorType: "BUYER", ActorID: &buyerID, Action: "OFFER_ACCEPTED",
		EntityType: "offer", EntityID: &offerID,
		NewValue: map[string]any{"selection_id": selection.ID.String()},
	}); err != nil {
		s.logger.Error("writing audit for offer acceptance", "error", err)
	}

	if err := s.events.Write(ctx, &event.Event{
		EventType: event.TransactionCreated, EntityType: "transaction", EntityID: &tx.ID,
		ActorType: event.ActorSystem,
		Payload:   map[string]any{"gross_amount": tx.GrossAmount, "platform_fee": tx.PlatformFee, "payment_status": string(tx.PaymentStatus)},
	}); err != nil {
		s.logger.Error("writing TRANSACTION_CREATED event", "error", err)
	}
}
