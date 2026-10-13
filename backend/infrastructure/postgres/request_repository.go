package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/middleman/backend/internal/request"
)

type RequestRepository struct {
	db *sql.DB
}

func NewRequestRepository(db *sql.DB) *RequestRepository {
	return &RequestRepository{db: db}
}

// scanRow is shared by every method that reads a full buyer_requests row.
func scanRequestRow(scanner interface {
	Scan(dest ...any) error
}) (*request.BuyerRequest, error) {
	var (
		r                            request.BuyerRequest
		idStr, convStr, buyerStr     string
		originStr                    sql.NullString
		category, product, variant   sql.NullString
		budgetMin, budgetMax         sql.NullInt64
		location, condition, urgency sql.NullString
		quantity                     sql.NullInt32
		prefsBytes                   []byte
		extractedBytes               []byte
		confirmedAt                  sql.NullTime
		origin                       string
		listingID, deliveryID        sql.NullString
	)

	err := scanner.Scan(
		&idStr, &convStr, &buyerStr, &originStr,
		&r.Status,
		&category, &product, &variant,
		&budgetMin, &budgetMax, &r.Currency,
		&location, &condition, &quantity, &urgency, &prefsBytes,
		&r.RawMessage, &extractedBytes, &r.Confidence,
		&r.Completeness,
		&r.CreatedAt, &r.UpdatedAt, &confirmedAt,
		&origin, &listingID, &deliveryID,
	)
	if err != nil {
		return nil, err
	}

	r.ID, err = uuid.Parse(idStr)
	if err != nil {
		return nil, err
	}
	r.ConversationID, err = uuid.Parse(convStr)
	if err != nil {
		return nil, err
	}
	r.BuyerID, err = uuid.Parse(buyerStr)
	if err != nil {
		return nil, err
	}
	if originStr.Valid {
		if oid, err := uuid.Parse(originStr.String); err == nil {
			r.OriginMessageID = &oid
		}
	}

	r.Origin = request.Origin(origin)
	if listingID.Valid {
		if id, err := uuid.Parse(listingID.String); err == nil {
			r.ListingID = &id
		}
	}
	if deliveryID.Valid {
		if id, err := uuid.Parse(deliveryID.String); err == nil {
			r.DiscoveryDeliveryID = &id
		}
	}
	if category.Valid {
		r.Category = &category.String
	}
	if product.Valid {
		r.Product = &product.String
	}
	if variant.Valid {
		r.Variant = &variant.String
	}
	if budgetMin.Valid {
		r.BudgetMin = &budgetMin.Int64
	}
	if budgetMax.Valid {
		r.BudgetMax = &budgetMax.Int64
	}
	if location.Valid {
		r.Location = &location.String
	}
	if condition.Valid {
		r.Condition = &condition.String
	}
	if quantity.Valid {
		q := int(quantity.Int32)
		r.Quantity = &q
	}
	if urgency.Valid {
		r.Urgency = &urgency.String
	}
	if confirmedAt.Valid {
		r.ConfirmedAt = &confirmedAt.Time
	}

	_ = json.Unmarshal(prefsBytes, &r.Preferences)
	if r.Preferences == nil {
		r.Preferences = []string{}
	}
	_ = json.Unmarshal(extractedBytes, &r.ExtractedData)
	if r.ExtractedData == nil {
		r.ExtractedData = map[string]any{}
	}

	return &r, nil
}

const requestColumns = `
	id, conversation_id, buyer_id, origin_message_id,
	status,
	category, product, variant,
	budget_min, budget_max, currency,
	location, condition, quantity, urgency, preferences,
	raw_message, extracted_data, confidence,
	completeness,
	created_at, updated_at, confirmed_at,
	origin, listing_id, discovery_delivery_id
`

func (r *RequestRepository) Create(ctx context.Context, req *request.BuyerRequest) (*request.BuyerRequest, error) {
	if req.ID == uuid.Nil {
		req.ID = uuid.New()
	}
	if req.Currency == "" {
		req.Currency = "NGN"
	}
	if req.Preferences == nil {
		req.Preferences = []string{}
	}
	if req.ExtractedData == nil {
		req.ExtractedData = map[string]any{}
	}

	prefsBytes, _ := json.Marshal(req.Preferences)
	extractedBytes, _ := json.Marshal(req.ExtractedData)

	var originID any
	if req.OriginMessageID != nil {
		originID = req.OriginMessageID.String()
	}
	if req.Origin == "" {
		req.Origin = request.OriginMessage
	}
	uuidOrNil := func(id *uuid.UUID) any {
		if id == nil {
			return nil
		}
		return id.String()
	}

	row := r.db.QueryRowContext(ctx, `
		INSERT INTO buyer_requests (
			id, conversation_id, buyer_id, origin_message_id, status,
			raw_message, extracted_data, confidence, completeness, currency,
			preferences, origin, listing_id, discovery_delivery_id,
			category, product, variant, budget_min, budget_max, location, condition, quantity
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
		ON CONFLICT (discovery_delivery_id, listing_id) WHERE origin = 'discovery' DO NOTHING
		RETURNING `+requestColumns,
		req.ID.String(), req.ConversationID.String(), req.BuyerID.String(), originID, req.Status,
		req.RawMessage, extractedBytes, req.Confidence, req.Completeness, req.Currency,
		prefsBytes, string(req.Origin), uuidOrNil(req.ListingID), uuidOrNil(req.DiscoveryDeliveryID),
		req.Category, req.Product, req.Variant, req.BudgetMin, req.BudgetMax, req.Location, req.Condition, req.Quantity,
	)

	created, err := scanRequestRow(row)
	if err == sql.ErrNoRows {
		return nil, request.ErrDuplicate
	}
	return created, err
}

// FindDiscoveryAsk implements request.Repository.
func (r *RequestRepository) FindDiscoveryAsk(ctx context.Context, deliveryID, listingID uuid.UUID) (*request.BuyerRequest, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+requestColumns+` FROM buyer_requests
		WHERE origin = 'discovery' AND discovery_delivery_id = $1 AND listing_id = $2`, deliveryID.String(), listingID.String())
	req, err := scanRequestRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return req, err
}

func (r *RequestRepository) FindByID(ctx context.Context, id uuid.UUID) (*request.BuyerRequest, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+requestColumns+` FROM buyer_requests WHERE id = $1`, id.String())
	return scanRequestRow(row)
}

func (r *RequestRepository) List(ctx context.Context, statusFilter *request.Status) ([]*request.BuyerRequest, error) {
	return r.ListPage(ctx, statusFilter, 0, 0)
}

// ListPage is List with LIMIT/OFFSET (limit 0 = no limit), for the paginated
// dashboard view.
func (r *RequestRepository) ListPage(ctx context.Context, statusFilter *request.Status, limit, offset int) ([]*request.BuyerRequest, error) {
	var rows *sql.Rows
	var err error
	page := ""
	if limit > 0 {
		page = fmt.Sprintf(" LIMIT %d OFFSET %d", limit, offset)
	}

	if statusFilter != nil {
		rows, err = r.db.QueryContext(ctx, `
			SELECT `+requestColumns+` FROM buyer_requests
			WHERE status = $1
			ORDER BY
				CASE status WHEN 'NEEDS_REVIEW' THEN 0 WHEN 'EXTRACTING' THEN 1 ELSE 2 END,
				created_at DESC, id
		`+page, *statusFilter)
	} else {
		rows, err = r.db.QueryContext(ctx, `
			SELECT `+requestColumns+` FROM buyer_requests
			ORDER BY
				CASE status WHEN 'NEEDS_REVIEW' THEN 0 WHEN 'EXTRACTING' THEN 1 ELSE 2 END,
				created_at DESC, id
		`+page)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*request.BuyerRequest
	for rows.Next() {
		req, err := scanRequestRow(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, req)
	}
	return results, rows.Err()
}

// UpdateExtraction persists the AI's output. extracted_data is set once
// here and never touched again by corrections (PRD 1 §19).
func (r *RequestRepository) UpdateExtraction(ctx context.Context, id uuid.UUID, fields request.Fields, extractedData map[string]any, confidence float64, newStatus request.Status) (*request.BuyerRequest, error) {
	extractedBytes, _ := json.Marshal(extractedData)
	prefsBytes, _ := json.Marshal(nonNilPrefs(fields.Preferences))

	completeness := computeCompletenessFromFields(fields)

	row := r.db.QueryRowContext(ctx, `
		UPDATE buyer_requests SET
			status = $2,
			category = $3, product = $4, variant = $5,
			budget_min = $6, budget_max = $7,
			location = $8, condition = $9, quantity = $10, urgency = $11,
			preferences = $12,
			extracted_data = $13, confidence = $14,
			completeness = $15,
			updated_at = now()
		WHERE id = $1
		RETURNING `+requestColumns,
		id.String(), newStatus,
		fields.Category, fields.Product, fields.Variant,
		fields.BudgetMin, fields.BudgetMax,
		fields.Location, fields.Condition, fields.Quantity, fields.Urgency,
		prefsBytes,
		extractedBytes, confidence,
		completeness,
	)

	return scanRequestRow(row)
}

// UpdateFields applies an operator correction. extracted_data is left
// untouched — only the current structured fields change.
func (r *RequestRepository) UpdateFields(ctx context.Context, id uuid.UUID, fields request.Fields) (*request.BuyerRequest, error) {
	current, err := r.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}

	merged := mergeFields(current, fields)
	prefsBytes, _ := json.Marshal(merged.Preferences)
	completeness := computeCompletenessFromFields(merged)

	row := r.db.QueryRowContext(ctx, `
		UPDATE buyer_requests SET
			category = $2, product = $3, variant = $4,
			budget_min = $5, budget_max = $6,
			location = $7, condition = $8, quantity = $9, urgency = $10,
			preferences = $11,
			completeness = $12,
			status = CASE WHEN status = 'CONFIRMED' THEN status ELSE 'NEEDS_REVIEW' END,
			updated_at = now()
		WHERE id = $1
		RETURNING `+requestColumns,
		id.String(),
		merged.Category, merged.Product, merged.Variant,
		merged.BudgetMin, merged.BudgetMax,
		merged.Location, merged.Condition, merged.Quantity, merged.Urgency,
		prefsBytes,
		completeness,
	)

	return scanRequestRow(row)
}

// SetStatus implements request.Repository.
func (r *RequestRepository) SetStatus(ctx context.Context, id uuid.UUID, from []request.Status, to request.Status) (*request.BuyerRequest, error) {
	fs := make([]string, len(from))
	for i, f := range from {
		fs[i] = string(f)
	}
	row := r.db.QueryRowContext(ctx, `UPDATE buyer_requests SET status = $2, updated_at = now()
		WHERE id = $1 AND status = ANY($3) RETURNING `+requestColumns, id.String(), string(to), pq.Array(fs))
	req, err := scanRequestRow(row)
	if err == sql.ErrNoRows {
		return nil, request.ErrInvalidTransition
	}
	return req, err
}

// Confirm transitions NEEDS_REVIEW (or AWAITING_BUYER) -> CONFIRMED. Confirming an already
// CONFIRMED request is an idempotent no-op (confirmed_at is preserved).
// Any other current status (e.g. EXTRACTING, before the AI result exists)
// returns request.ErrInvalidTransition.
func (r *RequestRepository) Confirm(ctx context.Context, id uuid.UUID) (*request.BuyerRequest, error) {
	row := r.db.QueryRowContext(ctx, `
		UPDATE buyer_requests SET
			status = 'CONFIRMED',
			confirmed_at = COALESCE(confirmed_at, now()),
			updated_at = now()
		WHERE id = $1 AND status IN ('NEEDS_REVIEW', 'AWAITING_BUYER', 'CONFIRMED')
		RETURNING `+requestColumns,
		id.String(),
	)
	req, err := scanRequestRow(row)
	if err == sql.ErrNoRows {
		return nil, request.ErrInvalidTransition
	}
	return req, err
}

func (r *RequestRepository) FindLatestOpenByConversation(ctx context.Context, conversationID uuid.UUID) (*request.BuyerRequest, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT `+requestColumns+` FROM buyer_requests
		WHERE conversation_id = $1 AND status != 'CONFIRMED'
		ORDER BY created_at DESC
		LIMIT 1
	`, conversationID.String())

	req, err := scanRequestRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return req, err
}

// --- helpers ---

func nonNilPrefs(p []string) []string {
	if p == nil {
		return []string{}
	}
	return p
}

func computeCompletenessFromFields(f request.Fields) request.Completeness {
	hasProduct := f.Product != nil && *f.Product != ""
	hasBudget := f.BudgetMin != nil || f.BudgetMax != nil
	hasLocation := f.Location != nil && *f.Location != ""

	switch {
	case hasProduct && hasBudget && hasLocation:
		return request.CompletenessComplete
	case hasProduct:
		return request.CompletenessPartial
	default:
		return request.CompletenessInsufficient
	}
}

// mergeFields applies only the non-nil fields from `patch` on top of `current`,
// so a PATCH request can update a single field without clobbering the rest.
func mergeFields(current *request.BuyerRequest, patch request.Fields) request.Fields {
	result := request.Fields{
		Category:    current.Category,
		Product:     current.Product,
		Variant:     current.Variant,
		BudgetMin:   current.BudgetMin,
		BudgetMax:   current.BudgetMax,
		Location:    current.Location,
		Condition:   current.Condition,
		Quantity:    current.Quantity,
		Urgency:     current.Urgency,
		Preferences: current.Preferences,
	}

	if patch.Category != nil {
		result.Category = patch.Category
	}
	if patch.Product != nil {
		result.Product = patch.Product
	}
	if patch.Variant != nil {
		result.Variant = patch.Variant
	}
	if patch.BudgetMin != nil {
		result.BudgetMin = patch.BudgetMin
	}
	if patch.BudgetMax != nil {
		result.BudgetMax = patch.BudgetMax
	}
	if patch.Location != nil {
		result.Location = patch.Location
	}
	if patch.Condition != nil {
		result.Condition = patch.Condition
	}
	if patch.Quantity != nil {
		result.Quantity = patch.Quantity
	}
	if patch.Urgency != nil {
		result.Urgency = patch.Urgency
	}
	if patch.Preferences != nil {
		result.Preferences = patch.Preferences
	}

	return result
}
