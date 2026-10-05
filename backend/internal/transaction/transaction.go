// Package transaction owns the boundary between "buyer chose an offer"
// and an eventual real transaction/payment/fulfilment engine. Per PRD 3
// §20-21: BuyerSelection and Transaction are deliberately distinct —
// selecting an offer must never be conflated with money having moved.
// No payment execution happens in this package; PaymentProvider
// (infrastructure/payment) is invoked by a future phase, not this one.
package transaction

import (
	"context"
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
	CreatedAt      time.Time
}

type Status string

const (
	StatusCreated          Status = "CREATED"
	StatusPaymentPending   Status = "PAYMENT_PENDING"
	StatusPaid             Status = "PAID"
	StatusPaymentFailed    Status = "PAYMENT_FAILED"
	StatusFulfilling       Status = "FULFILLING"
	StatusCompleted        Status = "COMPLETED"
	StatusCancelled        Status = "CANCELLED"
	StatusRefunded         Status = "REFUNDED"
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
	FulfilmentPending   FulfilmentStatus = "PENDING"
	FulfilmentInProgress FulfilmentStatus = "IN_PROGRESS"
	FulfilmentDelivered FulfilmentStatus = "DELIVERED"
	FulfilmentCancelled FulfilmentStatus = "CANCELLED"
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
	CreateSelection(ctx context.Context, s *Selection) (*Selection, error)
	FindSelectionByBuyerRequest(ctx context.Context, buyerRequestID uuid.UUID) (*Selection, error)

	CreateTransaction(ctx context.Context, t *Transaction) (*Transaction, error)
	FindTransactionByID(ctx context.Context, id uuid.UUID) (*Transaction, error)
	FindTransactionBySelection(ctx context.Context, selectionID uuid.UUID) (*Transaction, error)
}
