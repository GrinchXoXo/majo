package postgres

import (
	"context"
	"database/sql"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/transaction"
)

type TransactionRepository struct {
	db *sql.DB
}

func NewTransactionRepository(db *sql.DB) *TransactionRepository {
	return &TransactionRepository{db: db}
}

func (r *TransactionRepository) CreateSelection(ctx context.Context, s *transaction.Selection) (*transaction.Selection, error) {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	row := r.db.QueryRowContext(ctx, `
		INSERT INTO buyer_selections (id, buyer_request_id, offer_id, buyer_id)
		VALUES ($1, $2, $3, $4)
		RETURNING id, buyer_request_id, offer_id, buyer_id, created_at
	`, s.ID.String(), s.BuyerRequestID.String(), s.OfferID.String(), s.BuyerID.String())

	var idStr, brIDStr, offerIDStr, buyerIDStr string
	if err := row.Scan(&idStr, &brIDStr, &offerIDStr, &buyerIDStr, &s.CreatedAt); err != nil {
		return nil, err
	}
	s.ID, _ = uuid.Parse(idStr)
	s.BuyerRequestID, _ = uuid.Parse(brIDStr)
	s.OfferID, _ = uuid.Parse(offerIDStr)
	s.BuyerID, _ = uuid.Parse(buyerIDStr)
	return s, nil
}

func (r *TransactionRepository) FindSelectionByBuyerRequest(ctx context.Context, buyerRequestID uuid.UUID) (*transaction.Selection, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, buyer_request_id, offer_id, buyer_id, created_at
		FROM buyer_selections WHERE buyer_request_id = $1
		ORDER BY created_at DESC LIMIT 1
	`, buyerRequestID.String())

	var s transaction.Selection
	var idStr, brIDStr, offerIDStr, buyerIDStr string
	err := row.Scan(&idStr, &brIDStr, &offerIDStr, &buyerIDStr, &s.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.ID, _ = uuid.Parse(idStr)
	s.BuyerRequestID, _ = uuid.Parse(brIDStr)
	s.OfferID, _ = uuid.Parse(offerIDStr)
	s.BuyerID, _ = uuid.Parse(buyerIDStr)
	return &s, nil
}

const transactionColumns = `
	id, buyer_selection_id, buyer_request_id, offer_id, buyer_id, vendor_id, supply_listing_id,
	status, payment_status, fulfilment_status,
	gross_amount, platform_fee, payment_fee, vendor_amount, refund_amount, currency,
	payment_provider, payment_reference, created_at, updated_at
`

func scanTransactionRow(scanner interface{ Scan(dest ...any) error }) (*transaction.Transaction, error) {
	var (
		t                                                          transaction.Transaction
		idStr, selectionIDStr, brIDStr, offerIDStr, buyerIDStr, vendorIDStr string
		listingIDStr                                              sql.NullString
		paymentProvider, paymentReference                         sql.NullString
	)

	err := scanner.Scan(
		&idStr, &selectionIDStr, &brIDStr, &offerIDStr, &buyerIDStr, &vendorIDStr, &listingIDStr,
		&t.Status, &t.PaymentStatus, &t.FulfilmentStatus,
		&t.GrossAmount, &t.PlatformFee, &t.PaymentFee, &t.VendorAmount, &t.RefundAmount, &t.Currency,
		&paymentProvider, &paymentReference, &t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	t.ID, _ = uuid.Parse(idStr)
	t.BuyerSelectionID, _ = uuid.Parse(selectionIDStr)
	t.BuyerRequestID, _ = uuid.Parse(brIDStr)
	t.OfferID, _ = uuid.Parse(offerIDStr)
	t.BuyerID, _ = uuid.Parse(buyerIDStr)
	t.VendorID, _ = uuid.Parse(vendorIDStr)
	if listingIDStr.Valid {
		if v, err := uuid.Parse(listingIDStr.String); err == nil {
			t.SupplyListingID = &v
		}
	}
	if paymentProvider.Valid {
		t.PaymentProvider = &paymentProvider.String
	}
	if paymentReference.Valid {
		t.PaymentReference = &paymentReference.String
	}

	return &t, nil
}

func (r *TransactionRepository) CreateTransaction(ctx context.Context, t *transaction.Transaction) (*transaction.Transaction, error) {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	if t.Currency == "" {
		t.Currency = "NGN"
	}
	if t.Status == "" {
		t.Status = transaction.StatusCreated
	}
	if t.PaymentStatus == "" {
		t.PaymentStatus = transaction.PaymentUnpaid
	}
	if t.FulfilmentStatus == "" {
		t.FulfilmentStatus = transaction.FulfilmentPending
	}

	var listingID any
	if t.SupplyListingID != nil {
		listingID = t.SupplyListingID.String()
	}

	row := r.db.QueryRowContext(ctx, `
		INSERT INTO transactions (
			id, buyer_selection_id, buyer_request_id, offer_id, buyer_id, vendor_id, supply_listing_id,
			status, payment_status, fulfilment_status,
			gross_amount, platform_fee, payment_fee, vendor_amount, refund_amount, currency
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		RETURNING `+transactionColumns,
		t.ID.String(), t.BuyerSelectionID.String(), t.BuyerRequestID.String(), t.OfferID.String(), t.BuyerID.String(), t.VendorID.String(), listingID,
		t.Status, t.PaymentStatus, t.FulfilmentStatus,
		t.GrossAmount, t.PlatformFee, t.PaymentFee, t.VendorAmount, t.RefundAmount, t.Currency,
	)

	return scanTransactionRow(row)
}

func (r *TransactionRepository) FindTransactionByID(ctx context.Context, id uuid.UUID) (*transaction.Transaction, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+transactionColumns+` FROM transactions WHERE id = $1`, id.String())
	t, err := scanTransactionRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return t, err
}

func (r *TransactionRepository) FindTransactionBySelection(ctx context.Context, selectionID uuid.UUID) (*transaction.Transaction, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+transactionColumns+` FROM transactions WHERE buyer_selection_id = $1`, selectionID.String())
	t, err := scanTransactionRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return t, err
}
