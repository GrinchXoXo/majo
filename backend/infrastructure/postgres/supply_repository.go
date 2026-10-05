package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/supply"
)

type SupplyRepository struct {
	db *sql.DB
}

func NewSupplyRepository(db *sql.DB) *SupplyRepository {
	return &SupplyRepository{db: db}
}

const supplyColumns = `
	id, vendor_id, category, product, normalized_product, variant, condition,
	price, currency, quantity, location, availability_status,
	raw_message, extracted_data, confidence,
	created_at, updated_at, expires_at
`

func scanSupplyRow(scanner interface{ Scan(dest ...any) error }) (*supply.Signal, error) {
	var (
		s                                                    supply.Signal
		idStr, vendorIDStr                                   string
		category, product, normalizedProduct, variant, condition sql.NullString
		price                                                sql.NullInt64
		quantity                                             sql.NullInt32
		location                                             sql.NullString
		extractedBytes                                       []byte
	)

	err := scanner.Scan(
		&idStr, &vendorIDStr, &category, &product, &normalizedProduct, &variant, &condition,
		&price, &s.Currency, &quantity, &location, &s.AvailabilityStatus,
		&s.RawMessage, &extractedBytes, &s.Confidence,
		&s.CreatedAt, &s.UpdatedAt, &s.ExpiresAt,
	)
	if err != nil {
		return nil, err
	}

	s.ID, _ = uuid.Parse(idStr)
	s.VendorID, _ = uuid.Parse(vendorIDStr)

	if category.Valid {
		s.Category = &category.String
	}
	if product.Valid {
		s.Product = &product.String
	}
	if normalizedProduct.Valid {
		s.NormalizedProduct = &normalizedProduct.String
	}
	if variant.Valid {
		s.Variant = &variant.String
	}
	if condition.Valid {
		s.Condition = &condition.String
	}
	if price.Valid {
		s.Price = &price.Int64
	}
	if quantity.Valid {
		q := int(quantity.Int32)
		s.Quantity = &q
	}
	if location.Valid {
		s.Location = &location.String
	}

	_ = json.Unmarshal(extractedBytes, &s.ExtractedData)
	if s.ExtractedData == nil {
		s.ExtractedData = map[string]any{}
	}

	return &s, nil
}

func normalizeProductName(s *string) *string {
	if s == nil {
		return nil
	}
	n := strings.ToLower(strings.Join(strings.Fields(*s), ""))
	return &n
}

func (r *SupplyRepository) Create(ctx context.Context, s *supply.Signal) (*supply.Signal, error) {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	if s.Currency == "" {
		s.Currency = "NGN"
	}
	if s.AvailabilityStatus == "" {
		s.AvailabilityStatus = supply.AvailabilityAvailable
	}
	if s.ExtractedData == nil {
		s.ExtractedData = map[string]any{}
	}
	if s.ExpiresAt.IsZero() {
		s.ExpiresAt = time.Now().UTC().AddDate(0, 0, 7) // 7-day default (PRD 2 §12)
	}

	normalized := normalizeProductName(s.Product)
	extractedBytes, _ := json.Marshal(s.ExtractedData)

	row := r.db.QueryRowContext(ctx, `
		INSERT INTO supply_signals (
			id, vendor_id, category, product, normalized_product, variant, condition,
			price, currency, quantity, location, availability_status,
			raw_message, extracted_data, confidence, expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		RETURNING `+supplyColumns,
		s.ID.String(), s.VendorID.String(), s.Category, s.Product, normalized, s.Variant, s.Condition,
		s.Price, s.Currency, s.Quantity, s.Location, s.AvailabilityStatus,
		s.RawMessage, extractedBytes, s.Confidence, s.ExpiresAt,
	)

	return scanSupplyRow(row)
}

func (r *SupplyRepository) FindByID(ctx context.Context, id uuid.UUID) (*supply.Signal, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+supplyColumns+` FROM supply_signals WHERE id = $1`, id.String())
	s, err := scanSupplyRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return s, err
}

func (r *SupplyRepository) ListByVendor(ctx context.Context, vendorID uuid.UUID) ([]*supply.Signal, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+supplyColumns+` FROM supply_signals WHERE vendor_id = $1 ORDER BY created_at DESC
	`, vendorID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*supply.Signal
	for rows.Next() {
		s, err := scanSupplyRow(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, s)
	}
	return results, rows.Err()
}

func (r *SupplyRepository) FreshByNormalizedProduct(ctx context.Context, normalizedProduct string) ([]*supply.Signal, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+supplyColumns+` FROM supply_signals
		WHERE expires_at > now()
		  AND availability_status != 'EXPIRED'
		  AND (normalized_product = $1 OR normalized_product LIKE '%' || $1 || '%' OR $1 LIKE '%' || normalized_product || '%')
		ORDER BY created_at DESC
	`, normalizedProduct)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*supply.Signal
	for rows.Next() {
		s, err := scanSupplyRow(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, s)
	}
	return results, rows.Err()
}

func (r *SupplyRepository) FreshByVendor(ctx context.Context, vendorID uuid.UUID) ([]*supply.Signal, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+supplyColumns+` FROM supply_signals
		WHERE vendor_id = $1 AND expires_at > now() AND availability_status != 'EXPIRED'
		ORDER BY created_at DESC
	`, vendorID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*supply.Signal
	for rows.Next() {
		s, err := scanSupplyRow(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, s)
	}
	return results, rows.Err()
}

func (r *SupplyRepository) Update(ctx context.Context, id uuid.UUID, fields supply.SignalFields) (*supply.Signal, error) {
	setClauses := []string{}
	args := []any{}
	argN := 1

	addSet := func(col string, val any) {
		setClauses = append(setClauses, fmt.Sprintf("%s = $%d", col, argN))
		args = append(args, val)
		argN++
	}

	if fields.Category != nil {
		addSet("category", *fields.Category)
	}
	if fields.Product != nil {
		addSet("product", *fields.Product)
		addSet("normalized_product", *normalizeProductName(fields.Product))
	}
	if fields.Variant != nil {
		addSet("variant", *fields.Variant)
	}
	if fields.Condition != nil {
		addSet("condition", *fields.Condition)
	}
	if fields.Price != nil {
		addSet("price", *fields.Price)
	}
	if fields.Quantity != nil {
		addSet("quantity", *fields.Quantity)
	}
	if fields.Location != nil {
		addSet("location", *fields.Location)
	}
	if fields.Availability != nil {
		addSet("availability_status", *fields.Availability)
	}
	addSet("updated_at", time.Now().UTC())

	query := fmt.Sprintf("UPDATE supply_signals SET %s WHERE id = $%d RETURNING "+supplyColumns,
		strings.Join(setClauses, ", "), argN)
	args = append(args, id.String())

	row := r.db.QueryRowContext(ctx, query, args...)
	return scanSupplyRow(row)
}
