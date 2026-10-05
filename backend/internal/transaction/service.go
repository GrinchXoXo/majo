package transaction

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/audit"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/offer"
)

// DefaultTakeRateBps is the illustrative 4% take rate mentioned in the
// product docs (Master PRD §31: "3-5% take rate... 4% as a useful
// working example"). Expressed in basis points so the math stays in
// integers. This is NOT a business decision baked into the domain model —
// it's a single named constant, trivially replaceable, and Phase 3 never
// actually charges anyone (no PaymentProvider is invoked).
const DefaultTakeRateBps = 400 // 4.00%

// Service creates the transaction boundary described in PRD 3 §20-24:
// selecting an offer is a distinct, auditable step from a transaction
// existing, which is itself distinct from any money having moved
// (PaymentStatus starts and stays UNPAID — no PaymentProvider is called
// here).
type Service struct {
	repo   Repository
	offers offer.Repository
	events event.Writer
	audits audit.Writer
	logger *slog.Logger
}

func NewService(repo Repository, offers offer.Repository, events event.Writer, audits audit.Writer, logger *slog.Logger) *Service {
	return &Service{repo: repo, offers: offers, events: events, audits: audits, logger: logger}
}

// SelectOffer records a buyer's choice and creates the Transaction
// shell. Per PRD 3 §20: this must never be confused with payment —
// PaymentStatus is always UNPAID coming out of this call, and no
// PaymentProvider is invoked.
func (s *Service) SelectOffer(ctx context.Context, buyerRequestID, offerID, buyerID uuid.UUID) (*Selection, *Transaction, error) {
	o, err := s.offers.FindByID(ctx, offerID)
	if err != nil {
		return nil, nil, fmt.Errorf("loading offer: %w", err)
	}
	if o == nil {
		return nil, nil, fmt.Errorf("offer %s not found", offerID)
	}
	if o.BuyerRequestID != buyerRequestID {
		return nil, nil, fmt.Errorf("offer %s does not belong to buyer request %s", offerID, buyerRequestID)
	}

	selection, err := s.repo.CreateSelection(ctx, &Selection{
		BuyerRequestID: buyerRequestID, OfferID: offerID, BuyerID: buyerID,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("creating selection: %w", err)
	}

	if err := s.events.Write(ctx, &event.Event{
		EventType: event.BuyerSelectionCreated, EntityType: "buyer_selection", EntityID: &selection.ID,
		ActorType: event.ActorBuyer, ActorID: &buyerID,
		Payload: map[string]any{"offer_id": offerID.String(), "buyer_request_id": buyerRequestID.String()},
	}); err != nil {
		s.logger.Error("writing BUYER_SELECTION_CREATED event", "error", err)
	}

	if err := s.audits.Write(ctx, &audit.Entry{
		ActorType: "BUYER", ActorID: &buyerID, Action: "OFFER_ACCEPTED",
		EntityType: "offer", EntityID: &offerID,
		NewValue: map[string]any{"selection_id": selection.ID.String()},
	}); err != nil {
		s.logger.Error("writing audit for offer acceptance", "error", err)
	}

	gross := int64(0)
	if o.Price != nil {
		gross = *o.Price
	}
	platformFee := gross * DefaultTakeRateBps / 10000
	vendorAmount := gross - platformFee

	tx, err := s.repo.CreateTransaction(ctx, &Transaction{
		BuyerSelectionID: selection.ID, BuyerRequestID: buyerRequestID, OfferID: offerID,
		BuyerID: buyerID, VendorID: o.VendorID, SupplyListingID: o.SupplyListingID,
		Status: StatusCreated, PaymentStatus: PaymentUnpaid, FulfilmentStatus: FulfilmentPending,
		GrossAmount: gross, PlatformFee: platformFee, VendorAmount: vendorAmount,
	})
	if err != nil {
		return selection, nil, fmt.Errorf("creating transaction: %w", err)
	}

	if err := s.events.Write(ctx, &event.Event{
		EventType: event.TransactionCreated, EntityType: "transaction", EntityID: &tx.ID,
		ActorType: event.ActorSystem,
		Payload: map[string]any{"gross_amount": gross, "platform_fee": platformFee, "payment_status": string(tx.PaymentStatus)},
	}); err != nil {
		s.logger.Error("writing TRANSACTION_CREATED event", "error", err)
	}

	return selection, tx, nil
}
