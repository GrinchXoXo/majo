package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/routing"
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

var _ workflow.RecoveryReader = (*WorkflowRepository)(nil)

// RecoveryFacts implements workflow.RecoveryReader (Phase 4C hardening):
// one consistent read of what recovery decides on.
func (r *WorkflowRepository) RecoveryFacts(ctx context.Context, buyerRequestID uuid.UUID, unsentGrace time.Duration) (*workflow.RecoveryFacts, error) {
	f := &workflow.RecoveryFacts{}
	var status string
	err := r.db.QueryRowContext(ctx, `SELECT br.status,
		EXISTS (SELECT 1 FROM buyer_selections s WHERE s.buyer_request_id = br.id),
		EXISTS (SELECT 1 FROM transactions t WHERE t.buyer_request_id = br.id),
		(SELECT count(*) FROM vendor_requests v WHERE v.buyer_request_id = br.id),
		(SELECT count(*) FROM vendor_requests v WHERE v.buyer_request_id = br.id
			AND (v.status = 'SENT' OR (v.status = 'PENDING' AND v.created_at > now() - make_interval(secs => $2))
			     -- a rejected send with an automatic retry still pending is still awaited
			     OR (v.status = 'SEND_FAILED' AND EXISTS (SELECT 1 FROM jobs j WHERE j.job_type = 'VENDOR_REQUEST_RESEND'
			         AND j.status IN ('PENDING', 'RUNNING') AND j.payload->>'vendor_request_id' = v.id::text)))),
		(SELECT count(*) FROM vendor_responses x JOIN vendor_requests v ON v.id = x.vendor_request_id WHERE v.buyer_request_id = br.id
			AND x.status IN ('AVAILABLE', 'CAN_SOURCE', 'ALTERNATIVE') AND x.review_state <> 'NEEDS_REVIEW'),
		(SELECT count(*) FROM vendor_responses x JOIN vendor_requests v ON v.id = x.vendor_request_id WHERE v.buyer_request_id = br.id
			AND x.review_state = 'NEEDS_REVIEW' AND x.status <> 'UNAVAILABLE'),
		(SELECT count(*) FROM offers o WHERE o.buyer_request_id = br.id AND o.status IN ('REVIEWED', 'SHORTLISTED', 'SENDING', 'SENT')
			AND (o.expires_at IS NULL OR o.expires_at > now())),
		EXISTS (SELECT 1 FROM buyer_requests n WHERE n.conversation_id = br.conversation_id AND n.id <> br.id AND n.created_at > br.created_at
			AND n.status = 'CONFIRMED' AND br.product IS NOT NULL AND lower(n.product) = lower(br.product)),
		EXISTS (SELECT 1 FROM workflow_holds h WHERE h.buyer_request_id = br.id AND h.released_at IS NULL)
		FROM buyer_requests br WHERE br.id = $1`, buyerRequestID.String(), unsentGrace.Seconds()).Scan(&status, &f.HasSelection, &f.HasTransaction,
		&f.Contacted, &f.Awaiting, &f.Usable, &f.Undecided, &f.ActiveOffers, &f.Superseded, &f.Held)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("buyer request %s not found", buyerRequestID)
	}
	if err != nil {
		return nil, err
	}
	f.Status = request.Status(status)
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT vendor_id FROM vendor_requests WHERE buyer_request_id = $1`, buyerRequestID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if u, err := uuid.Parse(id); err == nil {
			f.ContactedVendors = append(f.ContactedVendors, u)
		}
	}
	return f, rows.Err()
}

// TimeoutCandidates implements workflow.RecoveryReader.
func (r *WorkflowRepository) TimeoutCandidates(ctx context.Context, unsentGrace time.Duration, limit int) ([]workflow.TimeoutCandidate, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT v.id, v.status, v.sent_at, v.created_at FROM vendor_requests v
		JOIN buyer_requests br ON br.id = v.buyer_request_id
		WHERE v.status = 'SENT'
		   OR (v.status = 'PENDING' AND v.created_at < now() - make_interval(secs => $1))
		   OR (v.status IN ('EXPIRED', 'SEND_FAILED') AND br.status = 'CONFIRMED' AND v.created_at > now() - interval '30 days'
		       AND NOT EXISTS (SELECT 1 FROM workflow_decisions d WHERE d.subject_type = 'vendor_request' AND d.subject_id = v.id
		                       AND d.decision = 'VENDOR_REQUEST_TIMEOUT')
		       AND NOT EXISTS (SELECT 1 FROM workflow_decisions d WHERE d.buyer_request_id = br.id AND d.decision = 'RECOVER_REQUEST'
		                       AND d.inputs->>'vendor_request_id' = v.id::text))
		ORDER BY v.created_at LIMIT $2`, unsentGrace.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []workflow.TimeoutCandidate
	for rows.Next() {
		var (
			c      workflow.TimeoutCandidate
			id, st string
			sent   sql.NullTime
		)
		if err := rows.Scan(&id, &st, &sent, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.VendorRequestID, _ = uuid.Parse(id)
		c.Status = routing.VendorRequestStatus(st)
		c.SentAt = optTime(sent)
		out = append(out, c)
	}
	return out, rows.Err()
}

// RequestMessages is the message trail of one request for the operator
// (Phase 4C hardening): every outbound message about the request or its
// vendor requests, with its delivery state (sent / delivered / read /
// failed). Messages that never left (rejected sends) are not stored as
// messages; they appear in the decision log and pending_notices.
func (r *WorkflowRepository) RequestMessages(ctx context.Context, buyerRequestID uuid.UUID) ([]map[string]any, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT m.id, COALESCE(m.metadata->>'kind', ''), COALESCE(m.metadata->>'entity_type', ''),
			COALESCE(m.delivery_status, 'sent'), m.created_at, m.delivered_at, m.read_at, m.failed_at, COALESCE(m.delivery_error, ''),
			left(COALESCE(m.content->>'text', ''), 300)
		FROM messages m
		WHERE m.direction = 'OUTBOUND' AND (m.metadata->>'entity_id' = $1
			OR m.metadata->>'entity_id' IN (SELECT id::text FROM vendor_requests WHERE buyer_request_id = $1::uuid))
		ORDER BY m.created_at`, buyerRequestID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var (
			id, kind, et, status, errText, text string
			at                                  time.Time
			delivered, read, failed             sql.NullTime
		)
		if err := rows.Scan(&id, &kind, &et, &status, &at, &delivered, &read, &failed, &errText, &text); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"message_id": id, "kind": kind, "entity_type": et, "delivery_status": status, "sent_at": at,
			"delivered_at": optTime(delivered), "read_at": optTime(read), "failed_at": optTime(failed), "error": errText, "text": text})
	}
	return out, rows.Err()
}
