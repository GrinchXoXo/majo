package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/supply"
)

type SupplyListingRepository struct {
	db *sql.DB
	// newRef generates candidate public references (injectable for the
	// collision test). Uniqueness is enforced by the database.
	newRef func() (string, error)
}

func NewSupplyListingRepository(db *sql.DB) *SupplyListingRepository {
	return &SupplyListingRepository{db: db, newRef: supply.NewPublicReference}
}

// maxRefAttempts bounds public-reference collision retries. With ~1.07e9
// values a single collision is already rare; eight in a row means a bug.
const maxRefAttempts = 8

const listingColumns = `
	id, vendor_id, supply_signal_id, category, product, normalized_product, variant, condition,
	price, currency, quantity, location, description, media_references, status,
	source_channel, source_message_id, extraction_method, confidence,
	published_at, last_verified_at, expires_at, created_at, updated_at, public_reference,
	rejected_at, rejection_reason
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
		rejectedAt                                              sql.NullTime
		rejectionReason                                         sql.NullString
	)

	err := scanner.Scan(
		&idStr, &vendorIDStr, &signalIDStr, &category, &product, &normalizedProduct, &variant, &condition,
		&price, &l.Currency, &quantity, &location, &description, &mediaBytes, &l.Status,
		&l.SourceChannel, &sourceMsgIDStr, &l.ExtractionMethod, &l.Confidence,
		&publishedAt, &lastVerifiedAt, &l.ExpiresAt, &l.CreatedAt, &l.UpdatedAt, &l.PublicReference,
		&rejectedAt, &rejectionReason,
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
	if rejectedAt.Valid {
		l.RejectedAt = &rejectedAt.Time
	}
	if rejectionReason.Valid {
		l.RejectionReason = &rejectionReason.String
	}
	_ = json.Unmarshal(mediaBytes, &l.MediaReferences)
	if l.MediaReferences == nil {
		l.MediaReferences = []supply.MediaReference{}
	}

	return &l, nil
}

// normalizeProductForListing is the compact search form (Phase 4A.3:
// supply.NormalizeSearch; Phase 3 only removed spaces).
func normalizeProductForListing(s *string) *string {
	if s == nil {
		return nil
	}
	n := supply.NormalizeSearch(*s)
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

	for attempt := 1; ; attempt++ {
		ref, err := r.newRef()
		if err != nil {
			return nil, err
		}
		out, err := scanListingRow(r.db.QueryRowContext(ctx, insertListingSQL,
			l.ID.String(), l.VendorID.String(), signalID, l.Category, l.Product, normalized, l.Variant, l.Condition,
			l.Price, l.Currency, l.Quantity, l.Location, l.Description, mediaBytes, l.Status,
			l.SourceChannel, sourceMsgID, l.ExtractionMethod, l.Confidence, l.ExpiresAt, l.LastVerifiedAt, ref))
		if isRefCollision(err) && attempt < maxRefAttempts {
			continue
		}
		return out, err
	}
}

const insertListingSQL = `
	INSERT INTO supply_listings (
		id, vendor_id, supply_signal_id, category, product, normalized_product, variant, condition,
		price, currency, quantity, location, description, media_references, status,
		source_channel, source_message_id, extraction_method, confidence, expires_at, last_verified_at, public_reference
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
	RETURNING ` + listingColumns

func isRefCollision(err error) bool {
	var pe *pq.Error
	return errors.As(err, &pe) && pe.Code == "23505" && pe.Constraint == "supply_listings_public_reference_key"
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
	if fields.Currency != nil {
		addSet("currency", *fields.Currency)
	}
	if fields.MediaReferences != nil {
		b, err := json.Marshal(*fields.MediaReferences)
		if err != nil {
			return nil, err
		}
		addSet("media_references", b)
	}
	// Status is not a field: transitions go through Curate (Phase 4A Step 4).
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
	row := r.db.QueryRowContext(ctx, `SELECT `+listingColumns+` FROM supply_listings WHERE supply_signal_id = $1
		ORDER BY (status = 'ARCHIVED'), created_at LIMIT 1`, signalID.String())
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

	// Phase 4A.3: the query is normalized exactly like the stored values
	// (supply.NormalizeSearch), so "iphone 13", "iPhone 13" and "iphone13"
	// all find "iPhone 13", and "BENIN CITY" finds "Benin City".
	if q.Category != nil {
		add("lower(category) =", strings.ToLower(strings.TrimSpace(*q.Category)))
	}
	if q.NormalizedProduct != nil {
		add("normalized_product LIKE", "%"+supply.NormalizeSearch(*q.NormalizedProduct)+"%")
	}
	if q.PriceMin != nil {
		add("price >=", *q.PriceMin)
	}
	if q.PriceMax != nil {
		add("price <=", *q.PriceMax)
	}
	if q.Location != nil {
		add("regexp_replace(lower(location), '[^[:alnum:]]+', '', 'g') =", supply.NormalizeSearch(*q.Location))
	}
	if q.Condition != nil {
		add("lower(condition) =", strings.ToLower(strings.TrimSpace(*q.Condition)))
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

// --- Phase 4A.3 ---------------------------------------------------------------

// CreateFromSignal: see supply.ListingRepository. One transaction holding a
// per-vendor advisory lock, so concurrent conversions of the same signal,
// or of two duplicate signals, are serialised; the partial unique index on
// supply_signal_id backs the same-signal case at the database level.
func (r *SupplyListingRepository) CreateFromSignal(ctx context.Context, w supply.ListingWrite) (*supply.Listing, supply.ListingOutcome, error) {
	l := w.Listing
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('supply_listing:' || $1))`, l.VendorID.String()); err != nil {
		return nil, "", err
	}

	commit := func(out *supply.Listing, outcome supply.ListingOutcome, ev *event.Event) (*supply.Listing, supply.ListingOutcome, error) {
		if ev != nil {
			if err := insertEventTx(ctx, tx, ev); err != nil {
				return nil, "", fmt.Errorf("writing %s: %w", ev.EventType, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, "", err
		}
		return out, outcome, nil
	}

	// 1. Already built from this signal. A live listing is preferred; an
	//    ARCHIVED one (operator-rejected, Phase 4A Step 4) also counts, so a
	//    rejected signal is never re-drafted when it gains late media. The
	//    row is locked so a media sync cannot overwrite a concurrent
	//    operator media reorder.
	existing, err := scanListingRow(tx.QueryRowContext(ctx, `SELECT `+listingColumns+` FROM supply_listings
		WHERE supply_signal_id = $1 ORDER BY (status = 'ARCHIVED'), created_at LIMIT 1 FOR UPDATE`, l.SupplySignalID.String()))
	if err == nil {
		added := missingMedia(existing.MediaReferences, l.MediaReferences)
		if existing.Status != supply.ListingDraft || len(added) == 0 {
			return commit(existing, supply.ListingExisting, nil)
		}
		merged, _ := json.Marshal(append(existing.MediaReferences, added...))
		updated, err := scanListingRow(tx.QueryRowContext(ctx, `UPDATE supply_listings SET media_references = $2, updated_at = now()
			WHERE id = $1 RETURNING `+listingColumns, existing.ID.String(), merged))
		if err != nil {
			return nil, "", err
		}
		var ev *event.Event
		if w.OnMediaSync != nil {
			ev = w.OnMediaSync(updated, added)
		}
		return commit(updated, supply.ListingMediaSynced, ev)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, "", err
	}

	// 2. Phase 3 duplicate (same vendor + normalized product + variant + price,
	//    not expired/archived): advance freshness only. The comparison is
	//    made against the listing's current values OR its source signal's
	//    values (Phase 4A Step 4), so a listing whose product/price an
	//    operator corrected is still recognised as the same supply and is
	//    refreshed, not duplicated; the correction itself is never touched.
	dup, err := scanListingRow(tx.QueryRowContext(ctx, `
		SELECT `+listingColumns+` FROM supply_listings l
		WHERE l.vendor_id = $1 AND l.expires_at > now() AND l.status NOT IN ('EXPIRED', 'ARCHIVED')
		  AND (
		    (l.normalized_product = $2 AND l.variant IS NOT DISTINCT FROM $3 AND l.price IS NOT DISTINCT FROM $4)
		    OR EXISTS (SELECT 1 FROM supply_signals s WHERE s.id = l.supply_signal_id
		        AND regexp_replace(lower(COALESCE(s.product, '')), '[^[:alnum:]]+', '', 'g') = $2
		        AND s.variant IS NOT DISTINCT FROM $3 AND s.price IS NOT DISTINCT FROM $4)
		  )
		ORDER BY l.created_at DESC LIMIT 1 FOR UPDATE`, l.VendorID.String(), *l.NormalizedProduct, l.Variant, l.Price))
	if err == nil {
		refreshed, err := scanListingRow(tx.QueryRowContext(ctx, `
			UPDATE supply_listings
			SET last_verified_at = GREATEST(COALESCE(last_verified_at, $2), $2),
			    expires_at = GREATEST(expires_at, $3), updated_at = now()
			WHERE id = $1 RETURNING `+listingColumns, dup.ID.String(), l.LastVerifiedAt, l.ExpiresAt))
		if err != nil {
			return nil, "", err
		}
		var ev *event.Event
		if w.OnRefresh != nil {
			ev = w.OnRefresh(refreshed)
		}
		return commit(refreshed, supply.ListingRefreshed, ev)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, "", err
	}

	// 3. New DRAFT with a unique public reference (savepoint + retry).
	if l.ID == uuid.Nil {
		l.ID = uuid.New()
	}
	mediaBytes, _ := json.Marshal(l.MediaReferences)
	if l.MediaReferences == nil {
		mediaBytes = []byte("[]")
	}
	var sourceMsg any
	if l.SourceMessageID != nil {
		sourceMsg = l.SourceMessageID.String()
	}
	var created *supply.Listing
	for attempt := 1; ; attempt++ {
		ref, err := r.newRef()
		if err != nil {
			return nil, "", err
		}
		if _, err := tx.ExecContext(ctx, `SAVEPOINT listing_ref`); err != nil {
			return nil, "", err
		}
		created, err = scanListingRow(tx.QueryRowContext(ctx, insertListingSQL,
			l.ID.String(), l.VendorID.String(), l.SupplySignalID.String(), l.Category, l.Product, *l.NormalizedProduct,
			l.Variant, l.Condition, l.Price, l.Currency, l.Quantity, l.Location, l.Description, mediaBytes, l.Status,
			l.SourceChannel, sourceMsg, l.ExtractionMethod, l.Confidence, l.ExpiresAt, l.LastVerifiedAt, ref))
		if err == nil {
			break
		}
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT listing_ref`); rbErr != nil {
			return nil, "", rbErr
		}
		if !isRefCollision(err) || attempt >= maxRefAttempts {
			return nil, "", err
		}
	}
	var ev *event.Event
	if w.OnCreate != nil {
		ev = w.OnCreate(created)
	}
	return commit(created, supply.ListingCreated, ev)
}

// missingMedia returns the refs in `want` whose asset is not in `have`, in order.
func missingMedia(have, want []supply.MediaReference) []supply.MediaReference {
	seen := map[string]bool{}
	for _, m := range have {
		seen[m.AssetID] = true
	}
	var out []supply.MediaReference
	for _, m := range want {
		if m.AssetID != "" && !seen[m.AssetID] {
			seen[m.AssetID] = true
			out = append(out, m)
		}
	}
	return out
}

// SignalMedia implements supply.SignalMediaReader: the signal's evidence
// media in source order, de-duplicated, as references to AVAILABLE
// MediaAssets; assets in any other state are reported as unusable.
func (r *SupplyListingRepository) SignalMedia(ctx context.Context, signalID uuid.UUID) ([]supply.MediaReference, []uuid.UUID, *uuid.UUID, error) {
	var first *uuid.UUID
	var fm sql.NullString
	if err := r.db.QueryRowContext(ctx, `SELECT message_id FROM supply_signal_sources WHERE supply_signal_id = $1
		ORDER BY position LIMIT 1`, signalID.String()).Scan(&fm); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil, err
	}
	if fm.Valid {
		id := uuid.MustParse(fm.String)
		first = &id
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT a.id, a.kind, a.storage_key, a.status
		FROM supply_signal_sources ss JOIN media_assets a ON a.id = ss.media_asset_id
		WHERE ss.supply_signal_id = $1
		ORDER BY ss.position`, signalID.String())
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	refs := []supply.MediaReference{}
	var unusable []uuid.UUID
	seen := map[string]bool{}
	for rows.Next() {
		var id, kind, key, status string
		if err := rows.Scan(&id, &kind, &key, &status); err != nil {
			return nil, nil, nil, err
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		if status != "AVAILABLE" {
			unusable = append(unusable, uuid.MustParse(id))
			continue
		}
		refs = append(refs, supply.MediaReference{Type: kind, StorageKey: key, AssetID: id})
	}
	return refs, unusable, first, rows.Err()
}
