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

type SupplyListingRepository struct {
	db *sql.DB
}

func NewSupplyListingRepository(db *sql.DB) *SupplyListingRepository {
	return &SupplyListingRepository{db: db}
}

const listingColumns = `
	id, vendor_id, supply_signal_id, category, product, normalized_product, variant, condition,
	price, currency, quantity, location, description, media_references, status,
	source_channel, source_message_id, extraction_method, confidence,
	published_at, last_verified_at, expires_at, created_at, updated_at
`

func scanListingRow(scanner interface{ Scan(dest ...any) error }) (*supply.Listing, error) {
	var (
		l                                                       supply.Listing
		idStr, vendorIDStr                                      string
		signalIDStr, sourceMsgIDStr                             sql.NullString
		category, product, normalizedProduct, variant, condition sql.NullString
		price                                                   sql.NullInt64
		quantity                                                sql.NullInt32
		location, description                                  sql.NullString
		mediaBytes                                              []byte
		publishedAt, lastVerifiedAt                             sql.NullTime
	)

	err := scanner.Scan(
		&idStr, &vendorIDStr, &signalIDStr, &category, &product, &normalizedProduct, &variant, &condition,
		&price, &l.Currency, &quantity, &location, &description, &mediaBytes, &l.Status,
		&l.SourceChannel, &sourceMsgIDStr, &l.ExtractionMethod, &l.Confidence,
		&publishedAt, &lastVerifiedAt, &l.ExpiresAt, &l.CreatedAt, &l.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	l.ID, _ = uuid.Parse(idStr)
	l.VendorID, _ = uuid.Parse(vendorIDStr)
	if signalIDStr.Valid {
		if sid, err := uuid.Parse(signalIDStr.String); err == nil {
			l.SupplySignalID = &sid
		}
	}
	if sourceMsgIDStr.Valid {
		if mid, err := uuid.Parse(sourceMsgIDStr.String); err == nil {
			l.SourceMessageID = &mid
		}
	}
	if category.Valid {
		l.Category = &category.String
	}
	if product.Valid {
		l.Product = &product.String
	}
	if normalizedProduct.Valid {
		l.NormalizedProduct = &normalizedProduct.String
	}
	if variant.Valid {
		l.Variant = &variant.String
	}
	if condition.Valid {
		l.Condition = &condition.String
	}
	if price.Valid {
		l.Price = &price.Int64
	}
	if quantity.Valid {
		q := int(quantity.Int32)
		l.Quantity = &q
	}
	if location.Valid {
		l.Location = &location.String
	}
	if description.Valid {
		l.Description = &description.String
	}
	if publishedAt.Valid {
		l.PublishedAt = &publishedAt.Time
	}
	if lastVerifiedAt.Valid {
		l.LastVerifiedAt = &lastVerifiedAt.Time
	}
	_ = json.Unmarshal(mediaBytes, &l.MediaReferences)
	if l.MediaReferences == nil {
		l.MediaReferences = []supply.MediaReference{}
	}

	return &l, nil
}

func normalizeProductForListing(s *string) *string {
	if s == nil {
		return nil
	}
	n := strings.ToLower(strings.Join(strings.Fields(*s), ""))
	return &n
}

func (r *SupplyListingRepository) Create(ctx context.Context, l *supply.Listing) (*supply.Listing, error) {
	if l.ID == uuid.Nil {
		l.ID = uuid.New()
	}
	if l.Currency == "" {
		l.Currency = "NGN"
	}
	if l.Status == "" {
		l.Status = supply.ListingDraft
	}
	if l.SourceChannel == "" {
		l.SourceChannel = "whatsapp"
	}
	if l.ExtractionMethod == "" {
		l.ExtractionMethod = "heuristic"
	}
	if l.MediaReferences == nil {
		l.MediaReferences = []supply.MediaReference{}
	}
	if l.ExpiresAt.IsZero() {
		l.ExpiresAt = time.Now().UTC().AddDate(0, 0, 7)
	}

	normalized := l.NormalizedProduct
	if normalized == nil {
		normalized = normalizeProductForListing(l.Product)
	}
	mediaBytes, _ := json.Marshal(l.MediaReferences)

	var signalID, sourceMsgID any
	if l.SupplySignalID != nil {
		signalID = l.SupplySignalID.String()
	}
	if l.SourceMessageID != nil {
		sourceMsgID = l.SourceMessageID.String()
	}

	row := r.db.QueryRowContext(ctx, `
		INSERT INTO supply_listings (
			id, vendor_id, supply_signal_id, category, product, normalized_product, variant, condition,
			price, currency, quantity, location, description, media_references, status,
			source_channel, source_message_id, extraction_method, confidence, expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		RETURNING `+listingColumns,
		l.ID.String(), l.VendorID.String(), signalID, l.Category, l.Product, normalized, l.Variant, l.Condition,
		l.Price, l.Currency, l.Quantity, l.Location, l.Description, mediaBytes, l.Status,
		l.SourceChannel, sourceMsgID, l.ExtractionMethod, l.Confidence, l.ExpiresAt,
	)

	return scanListingRow(row)
}

func (r *SupplyListingRepository) Update(ctx context.Context, id uuid.UUID, fields supply.ListingFields) (*supply.Listing, error) {
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
		addSet("normalized_product", *normalizeProductForListing(fields.Product))
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
	if fields.Description != nil {
		addSet("description", *fields.Description)
	}
	if fields.Status != nil {
		addSet("status", *fields.Status)
		if *fields.Status == supply.ListingActive {
			addSet("published_at", time.Now().UTC())
		}
	}
	addSet("updated_at", time.Now().UTC())

	query := fmt.Sprintf("UPDATE supply_listings SET %s WHERE id = $%d RETURNING "+listingColumns,
		strings.Join(setClauses, ", "), argN)
	args = append(args, id.String())

	row := r.db.QueryRowContext(ctx, query, args...)
	return scanListingRow(row)
}

func (r *SupplyListingRepository) FindByID(ctx context.Context, id uuid.UUID) (*supply.Listing, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+listingColumns+` FROM supply_listings WHERE id = $1`, id.String())
	l, err := scanListingRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return l, err
}

func (r *SupplyListingRepository) FindBySignalID(ctx context.Context, signalID uuid.UUID) (*supply.Listing, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+listingColumns+` FROM supply_listings WHERE supply_signal_id = $1`, signalID.String())
	l, err := scanListingRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return l, err
}

// FindRecentDuplicate implements PRD 3 §11's deterministic duplicate
// detection: same vendor, same normalized product+variant+price, not yet
// expired. No ML — just an equality match on the fields that matter.
func (r *SupplyListingRepository) FindRecentDuplicate(ctx context.Context, vendorID uuid.UUID, normalizedProduct string, variant *string, price *int64) (*supply.Listing, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT `+listingColumns+` FROM supply_listings
		WHERE vendor_id = $1 AND normalized_product = $2
		  AND variant IS NOT DISTINCT FROM $3
		  AND price IS NOT DISTINCT FROM $4
		  AND expires_at > now()
		  AND status NOT IN ('EXPIRED', 'ARCHIVED')
		ORDER BY created_at DESC
		LIMIT 1
	`, vendorID.String(), normalizedProduct, variant, price)

	l, err := scanListingRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return l, err
}

func (r *SupplyListingRepository) Search(ctx context.Context, q supply.ListingQuery) ([]*supply.Listing, error) {
	query := `SELECT ` + listingColumns + ` FROM supply_listings WHERE expires_at > now()`
	var args []any
	argN := 1

	add := func(clause string, val any) {
		query += fmt.Sprintf(" AND %s $%d", clause, argN)
		args = append(args, val)
		argN++
	}

	if q.Category != nil {
		add("category =", strings.ToLower(*q.Category))
	}
	if q.NormalizedProduct != nil {
		add("normalized_product LIKE", "%"+strings.ToLower(*q.NormalizedProduct)+"%")
	}
	if q.PriceMin != nil {
		add("price >=", *q.PriceMin)
	}
	if q.PriceMax != nil {
		add("price <=", *q.PriceMax)
	}
	if q.Location != nil {
		add("location =", *q.Location)
	}
	if q.Condition != nil {
		add("condition =", *q.Condition)
	}
	if q.VendorID != nil {
		add("vendor_id =", q.VendorID.String())
	}
	if q.Status != nil {
		add("status =", *q.Status)
	} else {
		query += " AND status = 'ACTIVE'"
	}

	query += " ORDER BY created_at DESC LIMIT 100"

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*supply.Listing
	for rows.Next() {
		l, err := scanListingRow(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, l)
	}
	return results, rows.Err()
}

func (r *SupplyListingRepository) ListByVendor(ctx context.Context, vendorID uuid.UUID) ([]*supply.Listing, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+listingColumns+` FROM supply_listings WHERE vendor_id = $1 ORDER BY created_at DESC
	`, vendorID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*supply.Listing
	for rows.Next() {
		l, err := scanListingRow(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, l)
	}
	return results, rows.Err()
}
