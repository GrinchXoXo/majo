package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/offer"
)

type OfferRepository struct {
	db *sql.DB
}

func NewOfferRepository(db *sql.DB) *OfferRepository {
	return &OfferRepository{db: db}
}

const offerColumns = `
	id, buyer_request_id, vendor_request_id, vendor_response_id, supply_listing_id, vendor_id,
	type, status, product, variant, condition, price, currency, quantity, location, notes,
	ranking_score, ranking_reasons, confidence, created_at, updated_at, expires_at,
	presentation_ordinal, sent_at, delivery_attempts, last_delivery_error, last_delivery_failed_at
`

func scanOfferRow(scanner interface{ Scan(dest ...any) error }) (*offer.Offer, error) {
	var (
		o                                            offer.Offer
		idStr, buyerRequestIDStr, vendorIDStr        string
		vendorRequestIDStr, vendorResponseIDStr      sql.NullString
		supplyListingIDStr                           sql.NullString
		product, variant, condition, location, notes sql.NullString
		price                                        sql.NullInt64
		quantity                                     sql.NullInt32
		reasonsBytes                                 []byte
		expiresAt                                    sql.NullTime
		ordinal                                      sql.NullInt32
		sentAt, lastFailedAt                         sql.NullTime
		lastDeliveryError                            sql.NullString
	)

	err := scanner.Scan(
		&idStr, &buyerRequestIDStr, &vendorRequestIDStr, &vendorResponseIDStr, &supplyListingIDStr, &vendorIDStr,
		&o.Type, &o.Status, &product, &variant, &condition, &price, &o.Currency, &quantity, &location, &notes,
		&o.RankingScore, &reasonsBytes, &o.Confidence, &o.CreatedAt, &o.UpdatedAt, &expiresAt,
		&ordinal, &sentAt, &o.DeliveryAttempts, &lastDeliveryError, &lastFailedAt,
	)
	if err != nil {
		return nil, err
	}

	o.ID, _ = uuid.Parse(idStr)
	o.BuyerRequestID, _ = uuid.Parse(buyerRequestIDStr)
	o.VendorID, _ = uuid.Parse(vendorIDStr)
	if vendorRequestIDStr.Valid {
		if v, err := uuid.Parse(vendorRequestIDStr.String); err == nil {
			o.VendorRequestID = &v
		}
	}
	if vendorResponseIDStr.Valid {
		if v, err := uuid.Parse(vendorResponseIDStr.String); err == nil {
			o.VendorResponseID = &v
		}
	}
	if supplyListingIDStr.Valid {
		if v, err := uuid.Parse(supplyListingIDStr.String); err == nil {
			o.SupplyListingID = &v
		}
	}
	if product.Valid {
		o.Product = &product.String
	}
	if variant.Valid {
		o.Variant = &variant.String
	}
	if condition.Valid {
		o.Condition = &condition.String
	}
	if price.Valid {
		o.Price = &price.Int64
	}
	if quantity.Valid {
		q := int(quantity.Int32)
		o.Quantity = &q
	}
	if location.Valid {
		o.Location = &location.String
	}
	if notes.Valid {
		o.Notes = &notes.String
	}
	if expiresAt.Valid {
		o.ExpiresAt = &expiresAt.Time
	}
	if ordinal.Valid {
		n := int(ordinal.Int32)
		o.PresentationOrdinal = &n
	}
	if sentAt.Valid {
		o.SentAt = &sentAt.Time
	}
	if lastDeliveryError.Valid {
		o.LastDeliveryError = &lastDeliveryError.String
	}
	if lastFailedAt.Valid {
		o.LastDeliveryFailedAt = &lastFailedAt.Time
	}
	_ = json.Unmarshal(reasonsBytes, &o.RankingReasons)
	if o.RankingReasons == nil {
		o.RankingReasons = []string{}
	}

	return &o, nil
}

func (r *OfferRepository) Create(ctx context.Context, o *offer.Offer) (*offer.Offer, error) {
	if o.ID == uuid.Nil {
		o.ID = uuid.New()
	}
	if o.Currency == "" {
		o.Currency = "NGN"
	}
	if o.Status == "" {
		o.Status = offer.StatusDraft
	}
	if o.RankingReasons == nil {
		o.RankingReasons = []string{}
	}
	reasonsBytes, _ := json.Marshal(o.RankingReasons)

	var vendorRequestID, vendorResponseID, supplyListingID any
	if o.VendorRequestID != nil {
		vendorRequestID = o.VendorRequestID.String()
	}
	if o.VendorResponseID != nil {
		vendorResponseID = o.VendorResponseID.String()
	}
	if o.SupplyListingID != nil {
		supplyListingID = o.SupplyListingID.String()
	}

	row := r.db.QueryRowContext(ctx, `
		INSERT INTO offers (
			id, buyer_request_id, vendor_request_id, vendor_response_id, supply_listing_id, vendor_id,
			type, status, product, variant, condition, price, currency, quantity, location, notes,
			ranking_score, ranking_reasons, confidence, expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		ON CONFLICT (vendor_response_id) WHERE vendor_response_id IS NOT NULL AND status <> 'DUPLICATE' DO NOTHING
		RETURNING `+offerColumns,
		o.ID.String(), o.BuyerRequestID.String(), vendorRequestID, vendorResponseID, supplyListingID, o.VendorID.String(),
		o.Type, o.Status, o.Product, o.Variant, o.Condition, o.Price, o.Currency, o.Quantity, o.Location, o.Notes,
		o.RankingScore, reasonsBytes, o.Confidence, o.ExpiresAt,
	)

	created, err := scanOfferRow(row)
	if err == sql.ErrNoRows {
		// ON CONFLICT DO NOTHING: an offer for this VendorResponse exists.
		return nil, offer.ErrAlreadyExists
	}
	return created, err
}

func (r *OfferRepository) Update(ctx context.Context, id uuid.UUID, fields offer.Fields) (*offer.Offer, error) {
	setClauses := []string{}
	args := []any{}
	argN := 1

	addSet := func(col string, val any) {
		setClauses = append(setClauses, fmt.Sprintf("%s = $%d", col, argN))
		args = append(args, val)
		argN++
	}

	if fields.Status != nil {
		addSet("status", *fields.Status)
	}
	if fields.Notes != nil {
		addSet("notes", *fields.Notes)
	}
	addSet("updated_at", time.Now().UTC())

	query := fmt.Sprintf("UPDATE offers SET %s WHERE id = $%d RETURNING "+offerColumns,
		strings.Join(setClauses, ", "), argN)
	args = append(args, id.String())

	row := r.db.QueryRowContext(ctx, query, args...)
	return scanOfferRow(row)
}

func (r *OfferRepository) FindByID(ctx context.Context, id uuid.UUID) (*offer.Offer, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+offerColumns+` FROM offers WHERE id = $1`, id.String())
	o, err := scanOfferRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return o, err
}

func (r *OfferRepository) ListByBuyerRequest(ctx context.Context, buyerRequestID uuid.UUID) ([]*offer.Offer, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+offerColumns+` FROM offers
		WHERE buyer_request_id = $1 AND status <> 'DUPLICATE'
		ORDER BY presentation_ordinal ASC NULLS LAST, ranking_score DESC, created_at ASC
	`, buyerRequestID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*offer.Offer
	for rows.Next() {
		o, err := scanOfferRow(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, o)
	}
	return results, rows.Err()
}

func (r *OfferRepository) FindByVendorResponseID(ctx context.Context, vendorResponseID uuid.UUID) (*offer.Offer, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT `+offerColumns+` FROM offers
		WHERE vendor_response_id = $1 AND status <> 'DUPLICATE'
		ORDER BY created_at ASC LIMIT 1`, vendorResponseID.String())
	o, err := scanOfferRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return o, err
}

// --- Phase 3.1: offer delivery (offer.DeliveryRepository) ---

// ClaimForDelivery: see offer.DeliveryRepository. A transaction-scoped
// advisory lock keyed on the buyer request serializes concurrent claimers
// for the same request, so ordinal assignment (max+1) cannot race.
func (r *OfferRepository) ClaimForDelivery(ctx context.Context, buyerRequestID uuid.UUID, staleAfter time.Duration) ([]*offer.Offer, int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('offer_delivery:' || $1))`, buyerRequestID.String()); err != nil {
		return nil, 0, fmt.Errorf("acquiring delivery lock: %w", err)
	}

	var maxOrdinal int
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(presentation_ordinal), 0) FROM offers WHERE buyer_request_id = $1
	`, buyerRequestID.String()).Scan(&maxOrdinal); err != nil {
		return nil, 0, err
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT `+offerColumns+` FROM offers
		WHERE buyer_request_id = $1
		  AND (status = 'REVIEWED' OR (status = 'SENDING' AND updated_at < now() - make_interval(secs => $2)))
		ORDER BY ranking_score DESC, created_at ASC, id ASC
		FOR UPDATE
	`, buyerRequestID.String(), staleAfter.Seconds())
	if err != nil {
		return nil, 0, err
	}
	var candidates []*offer.Offer
	for rows.Next() {
		o, err := scanOfferRow(rows)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		candidates = append(candidates, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	var claimed []*offer.Offer
	for _, o := range candidates {
		ordinal := 0
		if o.PresentationOrdinal != nil {
			ordinal = *o.PresentationOrdinal // reclaimed stale SENDING keeps its number
		} else {
			maxOrdinal++
			ordinal = maxOrdinal
		}
		row := tx.QueryRowContext(ctx, `
			UPDATE offers
			SET status = 'SENDING', presentation_ordinal = $2,
			    delivery_attempts = delivery_attempts + 1, updated_at = now()
			WHERE id = $1
			RETURNING `+offerColumns, o.ID.String(), ordinal)
		updated, err := scanOfferRow(row)
		if err != nil {
			return nil, 0, err
		}
		claimed = append(claimed, updated)
	}

	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return claimed, maxOrdinal, nil
}

func (r *OfferRepository) MarkDelivered(ctx context.Context, offerIDs []uuid.UUID, sentAt, expiresAt time.Time) error {
	if len(offerIDs) == 0 {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE offers
		SET status = 'SENT', sent_at = $2, expires_at = $3, updated_at = now()
		WHERE id = ANY($1::uuid[]) AND status = 'SENDING'
	`, uuidArray(offerIDs), sentAt, expiresAt)
	return err
}

func (r *OfferRepository) MarkDeliveryFailed(ctx context.Context, offerIDs []uuid.UUID, deliveryErr string) error {
	if len(offerIDs) == 0 {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
		UPDATE offers
		SET status = 'REVIEWED', presentation_ordinal = NULL,
		    last_delivery_error = $2, last_delivery_failed_at = now(), updated_at = now()
		WHERE id = ANY($1::uuid[]) AND status = 'SENDING'
	`, uuidArray(offerIDs), deliveryErr)
	return err
}

func (r *OfferRepository) FindLatestPresentedRequest(ctx context.Context, conversationID uuid.UUID) (*uuid.UUID, error) {
	var idStr string
	err := r.db.QueryRowContext(ctx, `
		SELECT o.buyer_request_id
		FROM offers o
		JOIN buyer_requests br ON br.id = o.buyer_request_id
		WHERE br.conversation_id = $1
		  AND o.presentation_ordinal IS NOT NULL
		  AND o.sent_at IS NOT NULL
		ORDER BY o.sent_at DESC
		LIMIT 1
	`, conversationID.String()).Scan(&idStr)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func uuidArray(ids []uuid.UUID) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = id.String()
	}
	return "{" + strings.Join(parts, ",") + "}"
}
