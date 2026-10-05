package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/routing"
)

type RoutingRepository struct {
	db *sql.DB
}

func NewRoutingRepository(db *sql.DB) *RoutingRepository {
	return &RoutingRepository{db: db}
}

const vendorRequestColumns = `
	id, buyer_request_id, vendor_id, public_reference, status, match_score, match_breakdown, created_at, sent_at
`

func scanVendorRequestRow(scanner interface{ Scan(dest ...any) error }) (*routing.VendorRequest, error) {
	var (
		vr                          routing.VendorRequest
		idStr, brIDStr, vendorIDStr string
		matchScore                  sql.NullInt32
		breakdownBytes              []byte
		sentAt                      sql.NullTime
	)

	err := scanner.Scan(&idStr, &brIDStr, &vendorIDStr, &vr.PublicReference, &vr.Status, &matchScore, &breakdownBytes, &vr.CreatedAt, &sentAt)
	if err != nil {
		return nil, err
	}

	vr.ID, _ = uuid.Parse(idStr)
	vr.BuyerRequestID, _ = uuid.Parse(brIDStr)
	vr.VendorID, _ = uuid.Parse(vendorIDStr)
	if matchScore.Valid {
		s := int(matchScore.Int32)
		vr.MatchScore = &s
	}
	_ = json.Unmarshal(breakdownBytes, &vr.MatchBreakdown)
	if vr.MatchBreakdown == nil {
		vr.MatchBreakdown = map[string]any{}
	}
	if sentAt.Valid {
		vr.SentAt = &sentAt.Time
	}

	return &vr, nil
}

func (r *RoutingRepository) CreateVendorRequest(ctx context.Context, vr *routing.VendorRequest) (*routing.VendorRequest, error) {
	if vr.ID == uuid.Nil {
		vr.ID = uuid.New()
	}
	if vr.Status == "" {
		vr.Status = routing.VendorRequestPending
	}
	if vr.MatchBreakdown == nil {
		vr.MatchBreakdown = map[string]any{}
	}
	breakdownBytes, _ := json.Marshal(vr.MatchBreakdown)

	row := r.db.QueryRowContext(ctx, `
		INSERT INTO vendor_requests (id, buyer_request_id, vendor_id, public_reference, status, match_score, match_breakdown)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING `+vendorRequestColumns,
		vr.ID.String(), vr.BuyerRequestID.String(), vr.VendorID.String(), vr.PublicReference, vr.Status, vr.MatchScore, breakdownBytes,
	)
	return scanVendorRequestRow(row)
}

func (r *RoutingRepository) MarkSent(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE vendor_requests SET status = 'SENT', sent_at = now() WHERE id = $1
	`, id.String())
	return err
}

func (r *RoutingRepository) FindVendorRequestByID(ctx context.Context, id uuid.UUID) (*routing.VendorRequest, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+vendorRequestColumns+` FROM vendor_requests WHERE id = $1`, id.String())
	vr, err := scanVendorRequestRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return vr, err
}

// FindVendorRequestByPublicReference supports PRD 2.1 §4 priority 1:
// explicit "MM-1042"-style reference parsing. Returns nil (no error) if
// the reference simply doesn't exist — the caller decides how to react,
// rather than this method guessing.
func (r *RoutingRepository) FindVendorRequestByPublicReference(ctx context.Context, ref string) (*routing.VendorRequest, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+vendorRequestColumns+` FROM vendor_requests WHERE public_reference = $1`, ref)
	vr, err := scanVendorRequestRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return vr, err
}

func (r *RoutingRepository) ListVendorRequestsByBuyerRequest(ctx context.Context, buyerRequestID uuid.UUID) ([]*routing.VendorRequest, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+vendorRequestColumns+` FROM vendor_requests WHERE buyer_request_id = $1 ORDER BY created_at DESC
	`, buyerRequestID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*routing.VendorRequest
	for rows.Next() {
		vr, err := scanVendorRequestRow(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, vr)
	}
	return results, rows.Err()
}

// ListActiveByVendor returns every VendorRequest still awaiting a
// response for this vendor (PENDING or SENT). Per PRD 2.1 §4, the caller
// must inspect the length of this list itself — this method does not
// collapse ambiguity by picking one.
func (r *RoutingRepository) ListActiveByVendor(ctx context.Context, vendorID uuid.UUID) ([]*routing.VendorRequest, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+vendorRequestColumns+` FROM vendor_requests
		WHERE vendor_id = $1 AND status IN ('PENDING', 'SENT')
		ORDER BY sent_at DESC NULLS LAST, created_at DESC
	`, vendorID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*routing.VendorRequest
	for rows.Next() {
		vr, err := scanVendorRequestRow(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, vr)
	}
	return results, rows.Err()
}

// ListByVendor returns every VendorRequest for this vendor regardless of
// status (PRD 2.1 §5/§11 dashboard association picker).
func (r *RoutingRepository) ListByVendor(ctx context.Context, vendorID uuid.UUID) ([]*routing.VendorRequest, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+vendorRequestColumns+` FROM vendor_requests
		WHERE vendor_id = $1
		ORDER BY created_at DESC
	`, vendorID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*routing.VendorRequest
	for rows.Next() {
		vr, err := scanVendorRequestRow(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, vr)
	}
	return results, rows.Err()
}

const vendorResponseColumns = `
	id, vendor_request_id, vendor_id, status, review_state, product, variant, condition,
	price, currency, quantity, location, notes,
	raw_message, extracted_data, confidence, extraction_method, created_at, updated_at
`

func scanVendorResponseRow(scanner interface{ Scan(dest ...any) error }) (*routing.VendorResponse, error) {
	var (
		resp                                          routing.VendorResponse
		idStr, vendorIDStr                            string
		vrIDStr                                       sql.NullString
		product, variant, condition, location, notes  sql.NullString
		price                                         sql.NullInt64
		quantity                                      sql.NullInt32
		extractedBytes                                []byte
	)

	err := scanner.Scan(
		&idStr, &vrIDStr, &vendorIDStr, &resp.Status, &resp.ReviewState, &product, &variant, &condition,
		&price, &resp.Currency, &quantity, &location, &notes,
		&resp.RawMessage, &extractedBytes, &resp.Confidence, &resp.ExtractionMethod, &resp.CreatedAt, &resp.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	resp.ID, _ = uuid.Parse(idStr)
	resp.VendorID, _ = uuid.Parse(vendorIDStr)
	if vrIDStr.Valid {
		if vid, err := uuid.Parse(vrIDStr.String); err == nil {
			resp.VendorRequestID = &vid
		}
	}
	if product.Valid {
		resp.Product = &product.String
	}
	if variant.Valid {
		resp.Variant = &variant.String
	}
	if condition.Valid {
		resp.Condition = &condition.String
	}
	if price.Valid {
		resp.Price = &price.Int64
	}
	if quantity.Valid {
		q := int(quantity.Int32)
		resp.Quantity = &q
	}
	if location.Valid {
		resp.Location = &location.String
	}
	if notes.Valid {
		resp.Notes = &notes.String
	}
	_ = json.Unmarshal(extractedBytes, &resp.ExtractedData)
	if resp.ExtractedData == nil {
		resp.ExtractedData = map[string]any{}
	}

	return &resp, nil
}

// CreateVendorResponse persists a response. VendorRequestID may be nil
// (PRD 2.1 §4/§13: unmatched responses are still recorded, never
// discarded). Only when it is set does the associated VendorRequest
// transition to RESPONDED.
func (r *RoutingRepository) CreateVendorResponse(ctx context.Context, resp *routing.VendorResponse) (*routing.VendorResponse, error) {
	if resp.ID == uuid.Nil {
		resp.ID = uuid.New()
	}
	if resp.Currency == "" {
		resp.Currency = "NGN"
	}
	if resp.ReviewState == "" {
		resp.ReviewState = routing.ReviewNeedsReview
	}
	if resp.ExtractionMethod == "" {
		resp.ExtractionMethod = routing.ExtractionHeuristic
	}
	if resp.ExtractedData == nil {
		resp.ExtractedData = map[string]any{}
	}
	extractedBytes, _ := json.Marshal(resp.ExtractedData)

	var vendorRequestID any
	if resp.VendorRequestID != nil {
		vendorRequestID = resp.VendorRequestID.String()
	}

	row := r.db.QueryRowContext(ctx, `
		INSERT INTO vendor_responses (
			id, vendor_request_id, vendor_id, status, review_state, product, variant, condition,
			price, currency, quantity, location, notes,
			raw_message, extracted_data, confidence, extraction_method
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		RETURNING `+vendorResponseColumns,
		resp.ID.String(), vendorRequestID, resp.VendorID.String(), resp.Status, resp.ReviewState,
		resp.Product, resp.Variant, resp.Condition,
		resp.Price, resp.Currency, resp.Quantity, resp.Location, resp.Notes,
		resp.RawMessage, extractedBytes, resp.Confidence, resp.ExtractionMethod,
	)

	created, err := scanVendorResponseRow(row)
	if err != nil {
		return nil, err
	}

	if resp.VendorRequestID != nil {
		if _, err := r.db.ExecContext(ctx, `UPDATE vendor_requests SET status = 'RESPONDED' WHERE id = $1`, resp.VendorRequestID.String()); err != nil {
			return nil, fmt.Errorf("marking vendor request responded: %w", err)
		}
	}

	return created, nil
}

func (r *RoutingRepository) UpdateVendorResponse(ctx context.Context, id uuid.UUID, fields routing.ResponseFields) (*routing.VendorResponse, error) {
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
	if fields.ReviewState != nil {
		addSet("review_state", *fields.ReviewState)
	}
	if fields.Product != nil {
		addSet("product", *fields.Product)
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
	if fields.Notes != nil {
		addSet("notes", *fields.Notes)
	}
	if fields.ExtractionMethod != nil {
		addSet("extraction_method", *fields.ExtractionMethod)
	}
	addSet("updated_at", time.Now().UTC())

	query := fmt.Sprintf("UPDATE vendor_responses SET %s WHERE id = $%d RETURNING "+vendorResponseColumns,
		strings.Join(setClauses, ", "), argN)
	args = append(args, id.String())

	row := r.db.QueryRowContext(ctx, query, args...)
	return scanVendorResponseRow(row)
}

func (r *RoutingRepository) FindResponseByID(ctx context.Context, id uuid.UUID) (*routing.VendorResponse, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+vendorResponseColumns+` FROM vendor_responses WHERE id = $1`, id.String())
	resp, err := scanVendorResponseRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return resp, err
}

func (r *RoutingRepository) FindResponseByVendorRequest(ctx context.Context, vendorRequestID uuid.UUID) (*routing.VendorResponse, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT `+vendorResponseColumns+` FROM vendor_responses WHERE vendor_request_id = $1
	`, vendorRequestID.String())
	resp, err := scanVendorResponseRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return resp, err
}

// ListUnmatched returns responses with no VendorRequest association yet
// — the operator's manual-association queue (PRD 2.1 §5).
func (r *RoutingRepository) ListUnmatched(ctx context.Context) ([]*routing.VendorResponse, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT `+vendorResponseColumns+` FROM vendor_responses
		WHERE vendor_request_id IS NULL
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*routing.VendorResponse
	for rows.Next() {
		resp, err := scanVendorResponseRow(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, resp)
	}
	return results, rows.Err()
}

// UpdateAssociation implements the operator's manual-association action
// (PRD 2.1 §5). Returns the previous vendor_request_id (nil if it was
// previously unmatched) so the caller can write an accurate audit record.
func (r *RoutingRepository) UpdateAssociation(ctx context.Context, responseID uuid.UUID, newVendorRequestID uuid.UUID) (*uuid.UUID, *routing.VendorResponse, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	var prevStr sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT vendor_request_id FROM vendor_responses WHERE id = $1`, responseID.String()).Scan(&prevStr); err != nil {
		return nil, nil, fmt.Errorf("loading current association: %w", err)
	}
	var previous *uuid.UUID
	if prevStr.Valid {
		if pid, err := uuid.Parse(prevStr.String); err == nil {
			previous = &pid
		}
	}

	row := tx.QueryRowContext(ctx, `
		UPDATE vendor_responses SET vendor_request_id = $1, updated_at = now() WHERE id = $2
		RETURNING `+vendorResponseColumns,
		newVendorRequestID.String(), responseID.String(),
	)
	updated, err := scanVendorResponseRow(row)
	if err != nil {
		return nil, nil, fmt.Errorf("updating association: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE vendor_requests SET status = 'RESPONDED' WHERE id = $1`, newVendorRequestID.String()); err != nil {
		return nil, nil, fmt.Errorf("marking newly-associated vendor request responded: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}

	return previous, updated, nil
}
