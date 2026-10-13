package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/middleman/backend/internal/workflow"
)

// WorkflowRepository builds the autonomous runtime's context from persisted
// state (Phase 4C correction): what the buyer was shown and asked for, the
// buyer's known location, requests waiting for a step, and vendor trust.
// Context is constructed by targeted queries — never by dumping tables.
type WorkflowRepository struct {
	db *sql.DB
}

func NewWorkflowRepository(db *sql.DB) *WorkflowRepository { return &WorkflowRepository{db: db} }

var _ workflow.ContextStore = (*WorkflowRepository)(nil)

// BuyerContext implements workflow.ContextStore. Items newest first:
// presented offers, listings in sent discovery deliveries, earlier requests.
func (r *WorkflowRepository) BuyerContext(ctx context.Context, conversationID, buyerID uuid.UUID, since time.Time) (*workflow.BuyerContext, error) {
	bc := &workflow.BuyerContext{ConversationID: conversationID, BuyerID: buyerID}

	// Offers presented in this conversation.
	rows, err := r.db.QueryContext(ctx, `SELECT o.id, o.buyer_request_id, o.presentation_ordinal, COALESCE(o.product, br.product, ''), COALESCE(o.variant, br.variant, ''),
			COALESCE(o.condition, br.condition, ''), o.price, o.status, o.sent_at, COALESCE(br.category, ''), COALESCE(br.location, ''), o.supply_listing_id
		FROM offers o JOIN buyer_requests br ON br.id = o.buyer_request_id
		WHERE br.conversation_id = $1 AND o.sent_at >= $2 AND o.presentation_ordinal IS NOT NULL
		ORDER BY o.sent_at DESC, o.presentation_ordinal`, conversationID.String(), since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var (
			it      workflow.ContextItem
			id, br  string
			price   sql.NullInt64
			listing sql.NullString
		)
		if err := rows.Scan(&id, &br, &it.Ordinal, &it.Product, &it.Variant, &it.Condition, &price, &it.OfferStatus, &it.At, &it.Category,
			&it.Location, &listing); err != nil {
			rows.Close()
			return nil, err
		}
		it.Kind = workflow.ItemOffer
		oid, _ := uuid.Parse(id)
		bid, _ := uuid.Parse(br)
		it.OfferID, it.BuyerRequestID = &oid, &bid
		it.ListingID = optUUID(listing)
		it.Group = "offers:" + br
		if price.Valid {
			p := price.Int64
			it.Price = &p
		}
		bc.Items = append(bc.Items, it)
	}
	rows.Close()

	// Listings shown in discovery deliveries sent to this buyer.
	var focus string
	_ = r.db.QueryRowContext(ctx, `SELECT COALESCE(listing_id::text, '') FROM discovery_interactions
		WHERE buyer_id = $1 AND outcome = 'APPLIED' AND listing_id IS NOT NULL AND created_at >= $2
		ORDER BY created_at DESC LIMIT 1`, buyerID.String(), since).Scan(&focus)
	rows, err = r.db.QueryContext(ctx, `SELECT d.id, d.sent_at, l.id, l.public_reference, COALESCE(l.category, ''), COALESCE(l.product, ''),
			COALESCE(l.variant, ''), COALESCE(l.condition, ''), COALESCE(l.description, ''), COALESCE(l.location, ''), l.price
		FROM discovery_deliveries d JOIN LATERAL unnest(d.listing_ids) AS lid(id) ON TRUE JOIN supply_listings l ON l.id = lid.id
		WHERE d.buyer_id = $1 AND d.status = 'SENT' AND d.sent_at >= $2
		ORDER BY d.sent_at DESC, l.public_reference`, buyerID.String(), since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var (
			it       workflow.ContextItem
			did, lid string
			price    sql.NullInt64
		)
		if err := rows.Scan(&did, &it.At, &lid, &it.Ref, &it.Category, &it.Product, &it.Variant, &it.Condition, &it.Description, &it.Location, &price); err != nil {
			rows.Close()
			return nil, err
		}
		it.Kind = workflow.ItemListing
		d, _ := uuid.Parse(did)
		l, _ := uuid.Parse(lid)
		it.DeliveryID, it.ListingID = &d, &l
		it.Group = "delivery:" + did
		it.Focus = lid == focus
		if price.Valid {
			p := price.Int64
			it.Price = &p
		}
		bc.Items = append(bc.Items, it)
	}
	rows.Close()

	// What the buyer asked for earlier in this conversation.
	rows, err = r.db.QueryContext(ctx, `SELECT id, status, COALESCE(category, ''), COALESCE(product, ''), COALESCE(variant, ''),
			COALESCE(condition, ''), COALESCE(location, ''), created_at
		FROM buyer_requests WHERE conversation_id = $1 AND created_at >= $2 AND product IS NOT NULL AND product <> ''
		ORDER BY created_at DESC LIMIT 10`, conversationID.String(), since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var it workflow.ContextItem
		var id string
		if err := rows.Scan(&id, &it.RequestStatus, &it.Category, &it.Product, &it.Variant, &it.Condition, &it.Location, &it.At); err != nil {
			rows.Close()
			return nil, err
		}
		it.Kind = workflow.ItemRequest
		rid, _ := uuid.Parse(id)
		it.BuyerRequestID = &rid
		it.Group = "requests"
		bc.Items = append(bc.Items, it)
	}
	rows.Close()
	sortItemsNewestFirst(bc.Items)

	// Known location: the buyer's discovery preference, else their latest
	// request that named one.
	var loc sql.NullString
	err = r.db.QueryRowContext(ctx, `SELECT COALESCE(
			(SELECT location FROM discovery_preferences WHERE buyer_id = $1 AND location IS NOT NULL AND location <> '' LIMIT 1),
			(SELECT location FROM buyer_requests WHERE buyer_id = $1 AND location IS NOT NULL AND location <> '' ORDER BY created_at DESC LIMIT 1))`,
		buyerID.String()).Scan(&loc)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	bc.KnownLocation = loc.String
	return bc, nil
}

func sortItemsNewestFirst(items []workflow.ContextItem) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].At.After(items[j-1].At); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// PendingRequests implements workflow.ContextStore (startup recovery).
func (r *WorkflowRepository) PendingRequests(ctx context.Context, since time.Time, staleExtraction time.Duration) ([]uuid.UUID, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT br.id FROM buyer_requests br
		WHERE br.created_at >= $1 AND (
			br.status = 'NEEDS_REVIEW'
			OR (br.status = 'EXTRACTING' AND br.created_at < now() - $2::interval)
			OR (br.status = 'CONFIRMED' AND NOT EXISTS (SELECT 1 FROM vendor_requests vr WHERE vr.buyer_request_id = br.id)
				AND NOT EXISTS (SELECT 1 FROM workflow_decisions d WHERE d.buyer_request_id = br.id
					AND (d.decision = 'NO_MATCH' OR (d.decision = 'ROUTE_REQUEST' AND d.reason = 'no_local_match')))))
		AND NOT EXISTS (SELECT 1 FROM workflow_exceptions x WHERE x.buyer_request_id = br.id AND x.status = 'OPEN')
		ORDER BY br.created_at`, since, staleExtraction.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if u, err := uuid.Parse(id); err == nil {
			out = append(out, u)
		}
	}
	return out, rows.Err()
}

// StalledRequests implements workflow.ContextStore.
func (r *WorkflowRepository) StalledRequests(ctx context.Context, conversationID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT br.id FROM buyer_requests br
		WHERE br.conversation_id = $1 AND (
			(br.status = 'NEEDS_REVIEW' AND (SELECT d.reason FROM workflow_decisions d WHERE d.buyer_request_id = br.id
				ORDER BY d.created_at DESC LIMIT 1) = 'another_question_open')
			OR (br.status = 'AWAITING_BUYER' AND (SELECT c.status FROM clarifications c WHERE c.buyer_request_id = br.id
				ORDER BY c.asked_at DESC LIMIT 1) = 'SUPERSEDED'))
		ORDER BY br.created_at`, conversationID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if u, err := uuid.Parse(id); err == nil {
			out = append(out, u)
		}
	}
	return out, rows.Err()
}

// VendorTrust counts a vendor's last 20 operator curation decisions and how
// many were approvals with no operator edit (auto-publish evidence gate).
// Listings Majo published itself never count as evidence.
func (r *WorkflowRepository) VendorTrust(ctx context.Context, vendorID uuid.UUID) (workflow.VendorTrust, error) {
	var t workflow.VendorTrust
	err := r.db.QueryRowContext(ctx, `WITH decided AS (
			SELECT l.id, l.status, COALESCE(l.published_at, l.rejected_at) AS decided_at
			FROM supply_listings l
			WHERE l.vendor_id = $1 AND (l.rejected_at IS NOT NULL OR EXISTS (SELECT 1 FROM events e
				WHERE e.entity_type = 'supply_listing' AND e.entity_id = l.id AND e.event_type = 'SUPPLY_LISTING_PUBLISHED' AND e.actor_type = 'OPERATOR'))
			ORDER BY COALESCE(l.published_at, l.rejected_at) DESC LIMIT $2)
		SELECT count(*), count(*) FILTER (WHERE d.status <> 'ARCHIVED' AND NOT EXISTS (SELECT 1 FROM events e
			WHERE e.entity_type = 'supply_listing' AND e.entity_id = d.id AND e.event_type = 'SUPPLY_LISTING_UPDATED' AND e.actor_type = 'OPERATOR'))
		FROM decided d`, vendorID.String(), workflow.TrustWindow).Scan(&t.Decided, &t.ApprovedUnchanged)
	return t, err
}

// keep pq imported for array helpers used by future context queries.
var _ = pq.Array
