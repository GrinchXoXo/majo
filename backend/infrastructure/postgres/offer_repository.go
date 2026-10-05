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
	ranking_score, ranking_reasons, confidence, created_at, updated_at, expires_at
`

func scanOfferRow(scanner interface{ Scan(dest ...any) error }) (*offer.Offer, error) {
	var (
		o                                             offer.Offer
		idStr, buyerRequestIDStr, vendorIDStr         string
		vendorRequestIDStr, vendorResponseIDStr       sql.NullString
		supplyListingIDStr                            sql.NullString
		product, variant, condition, location, notes  sql.NullString
		price                                         sql.NullInt64
		quantity                                      sql.NullInt32
		reasonsBytes                                  []byte
		expiresAt                                     sql.NullTime
	)

	err := scanner.Scan(
		&idStr, &buyerRequestIDStr, &vendorRequestIDStr, &vendorResponseIDStr, &supplyListingIDStr, &vendorIDStr,
		&o.Type, &o.Status, &product, &variant, &condition, &price, &o.Currency, &quantity, &location, &notes,
		&o.RankingScore, &reasonsBytes, &o.Confidence, &o.CreatedAt, &o.UpdatedAt, &expiresAt,
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
		RETURNING `+offerColumns,
		o.ID.String(), o.BuyerRequestID.String(), vendorRequestID, vendorResponseID, supplyListingID, o.VendorID.String(),
		o.Type, o.Status, o.Product, o.Variant, o.Condition, o.Price, o.Currency, o.Quantity, o.Location, o.Notes,
		o.RankingScore, reasonsBytes, o.Confidence, o.ExpiresAt,
	)

	return scanOfferRow(row)
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
		SELECT `+offerColumns+` FROM offers WHERE buyer_request_id = $1 ORDER BY ranking_score DESC, created_at ASC
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
	row := r.db.QueryRowContext(ctx, `SELECT `+offerColumns+` FROM offers WHERE vendor_response_id = $1`, vendorResponseID.String())
	o, err := scanOfferRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return o, err
}
