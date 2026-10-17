// Package transaction owns the boundary between "buyer chose an offer"
// and an eventual real transaction/payment/fulfilment engine. Per PRD 3
// §20-21: BuyerSelection and Transaction are deliberately distinct —
// selecting an offer must never be conflated with money having moved.
// No payment execution happens in this package; PaymentProvider
// (internal/payments) is invoked by a future phase, not this one.
package transaction

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Selection records that a buyer chose a specific Offer. Auditable,
// reversible in spirit (a future phase could allow re-selection), and
// intentionally minimal — it carries no payment or fulfilment meaning.
type Selection struct {
	ID             uuid.UUID
	BuyerRequestID uuid.UUID
	OfferID        uuid.UUID
	BuyerID        uuid.UUID
	Status         SelectionStatus
	// Source records how the selection was made (Phase 3.1 §23A), so organic
	// buyer selections can be told apart from operator overrides:
	// SourceWhatsApp | SourceOperator | SourceAPI.
	Source    string
	CreatedAt time.Time
}

// Selection sources.
const (
	SourceWhatsApp = "whatsapp_reply"
	SourceOperator = "dashboard_operator"
	SourceAPI      = "api"
)

// SelectionStatus: ACTIVE is the single live selection for a BuyerRequest
// (DB-enforced, Phase 3.1). VOID marks pre-3.1 duplicates retired by
// migration 0006.
type SelectionStatus string

const (
	SelectionActive SelectionStatus = "ACTIVE"
	SelectionVoid   SelectionStatus = "VOID"
)

// Phase 3.1 selection errors.
var (
	// ErrSelectionExists is returned by the repository when the DB unique
	// index rejects a second ACTIVE selection for the same BuyerRequest.
	ErrSelectionExists = errors.New("buyer request already has a selection")
	// ErrAlreadySelected: the buyer already chose a DIFFERENT offer for
	// this request. One request -> at most one selection.
	ErrAlreadySelected = errors.New("a different offer was already selected for this request")
	// ErrNotRequestOwner: the selecting buyer does not own the request.
	ErrNotRequestOwner = errors.New("buyer does not own this buyer request")
	// ErrOfferNotSelectable: the offer is withdrawn, expired, or otherwise
	// no longer selectable.
	ErrOfferNotSelectable = errors.New("offer is no longer available for selection")
	// ErrOfferNotFound: no such offer for this request.
	ErrOfferNotFound = errors.New("offer not found for this buyer request")
)

type Status string

const (
	StatusCreated           Status = "CREATED"
	StatusPaymentPending    Status = "PAYMENT_PENDING"
	StatusPaid              Status = "PAID"
	StatusPaymentFailed     Status = "PAYMENT_FAILED"
	StatusFulfilling        Status = "FULFILLING"
	StatusCompleted         Status = "COMPLETED"
	StatusCancelled         Status = "CANCELLED"
	StatusRefunded          Status = "REFUNDED"
	StatusPartiallyRefunded Status = "PARTIALLY_REFUNDED"
)

type PaymentStatus string

const (
	PaymentUnpaid            PaymentStatus = "UNPAID"
	PaymentPending           PaymentStatus = "PAYMENT_PENDING"
	PaymentPaid              PaymentStatus = "PAID"
	PaymentFailed            PaymentStatus = "PAYMENT_FAILED"
	PaymentRefunded          PaymentStatus = "REFUNDED"
	PaymentPartiallyRefunded PaymentStatus = "PARTIALLY_REFUNDED"
)

type FulfilmentStatus string

const (
	FulfilmentPending    FulfilmentStatus = "PENDING"
	FulfilmentInProgress FulfilmentStatus = "IN_PROGRESS"
	FulfilmentDelivered  FulfilmentStatus = "DELIVERED"
	FulfilmentCancelled  FulfilmentStatus = "CANCELLED"
)

// Transaction is the structural boundary for a future real
// payment/settlement engine (PRD 3 §21-24). Amounts are split from day
// one — gross/platform_fee/payment_fee/vendor_amount — so commission
// logic can be introduced later without a schema migration. No field
// here implies money has actually moved; PaymentStatus tracks that
// separately and starts at UNPAID.
type Transaction struct {
	ID               uuid.UUID
	BuyerSelectionID uuid.UUID
	BuyerRequestID   uuid.UUID
	OfferID          uuid.UUID
	BuyerID          uuid.UUID
	VendorID         uuid.UUID
	SupplyListingID  *uuid.UUID

	Status           Status
	PaymentStatus    PaymentStatus
	FulfilmentStatus FulfilmentStatus

	GrossAmount  int64
	PlatformFee  int64
	PaymentFee   int64
	VendorAmount int64
	RefundAmount int64
	Currency     string

	PaymentProvider  *string
	PaymentReference *string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Repository is the persistence boundary for selections + transactions.
type Repository interface {
	// CreateSelectionWithTransaction atomically (one DB transaction)
	// inserts the ACTIVE BuyerSelection, its Transaction shell, and moves
	// the chosen offer to ACCEPTED. Returns ErrSelectionExists if the
	// request already has an ACTIVE selection (DB unique index) — nothing
	// is written in that case. Phase 3.1 replaced the separate
	// CreateSelection/CreateTransaction calls, which could leave a
	// selection without a transaction and allowed unlimited duplicates.
	CreateSelectionWithTransaction(ctx context.Context, s *Selection, t *Transaction) (*Selection, *Transaction, error)
	// FindSelectionByBuyerRequest returns the ACTIVE selection, or nil.
	FindSelectionByBuyerRequest(ctx context.Context, buyerRequestID uuid.UUID) (*Selection, error)

	FindTransactionByID(ctx context.Context, id uuid.UUID) (*Transaction, error)
	FindTransactionBySelection(ctx context.Context, selectionID uuid.UUID) (*Transaction, error)
}
