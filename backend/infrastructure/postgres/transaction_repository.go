package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/middleman/backend/internal/transaction"
)

type TransactionRepository struct {
	db *sql.DB
}

func NewTransactionRepository(db *sql.DB) *TransactionRepository {
	return &TransactionRepository{db: db}
}

func (r *TransactionRepository) FindSelectionByBuyerRequest(ctx context.Context, buyerRequestID uuid.UUID) (*transaction.Selection, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, buyer_request_id, offer_id, buyer_id, status, COALESCE(source, ''), created_at
		FROM buyer_selections WHERE buyer_request_id = $1 AND status = 'ACTIVE'
		ORDER BY created_at DESC LIMIT 1
	`, buyerRequestID.String())

	var s transaction.Selection
	var idStr, brIDStr, offerIDStr, buyerIDStr string
	err := row.Scan(&idStr, &brIDStr, &offerIDStr, &buyerIDStr, &s.Status, &s.Source, &s.CreatedAt)
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
		t                                                                   transaction.Transaction
		idStr, selectionIDStr, brIDStr, offerIDStr, buyerIDStr, vendorIDStr string
		listingIDStr                                                        sql.NullString
		paymentProvider, paymentReference                                   sql.NullString
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

// CreateSelectionWithTransaction: see transaction.Repository. One DB
// transaction: selection INSERT (guarded by uq_buyer_selections_active_request),
// transaction INSERT (guarded by uq_transactions_buyer_selection), and the
// offer's move to ACCEPTED. Any failure rolls back all three.
func (r *TransactionRepository) CreateSelectionWithTransaction(ctx context.Context, s *transaction.Selection, t *transaction.Transaction) (*transaction.Selection, *transaction.Transaction, error) {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	if s.Status == "" {
		s.Status = transaction.SelectionActive
	}
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

	dbtx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer dbtx.Rollback()

	if err := dbtx.QueryRowContext(ctx, `
		INSERT INTO buyer_selections (id, buyer_request_id, offer_id, buyer_id, status, source)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))
		RETURNING created_at
	`, s.ID.String(), s.BuyerRequestID.String(), s.OfferID.String(), s.BuyerID.String(), s.Status, s.Source).Scan(&s.CreatedAt); err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			return nil, nil, transaction.ErrSelectionExists
		}
		return nil, nil, fmt.Errorf("inserting selection: %w", err)
	}

	t.BuyerSelectionID = s.ID
	var listingID any
	if t.SupplyListingID != nil {
		listingID = t.SupplyListingID.String()
	}
	row := dbtx.QueryRowContext(ctx, `
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
	created, err := scanTransactionRow(row)
	if err != nil {
		return nil, nil, fmt.Errorf("inserting transaction: %w", err)
	}

	if _, err := dbtx.ExecContext(ctx, `
		UPDATE offers SET status = 'ACCEPTED', updated_at = now() WHERE id = $1
	`, s.OfferID.String()); err != nil {
		return nil, nil, fmt.Errorf("marking offer accepted: %w", err)
	}

	if err := dbtx.Commit(); err != nil {
		return nil, nil, err
	}
	return s, created, nil
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
