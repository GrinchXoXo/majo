package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/middleman/backend/internal/funnel"
)

// FunnelRepository implements funnel.Reader from the system-of-record
// tables. Each stage counts entities whose stage timestamp falls inside the
// window (activity counts, not a per-request cohort; cohort analysis can be
// done later from the same columns — see docs/analytics/funnel.md).
type FunnelRepository struct{ db *sql.DB }

func NewFunnelRepository(db *sql.DB) *FunnelRepository { return &FunnelRepository{db: db} }

type stageQuery struct {
	key, label, source, note, sql string
}

var funnelStages = []stageQuery{
	{"buyer_request_created", "Buyer request created", "buyer_requests.created_at", "",
		`SELECT count(*) FROM buyer_requests WHERE created_at >= $1 AND created_at < $2`},
	{"buyer_request_confirmed", "Buyer request confirmed", "buyer_requests.confirmed_at", "",
		`SELECT count(*) FROM buyer_requests WHERE confirmed_at >= $1 AND confirmed_at < $2`},
	{"vendor_request_created", "Vendor request created", "vendor_requests.created_at", "",
		`SELECT count(*) FROM vendor_requests WHERE created_at >= $1 AND created_at < $2`},
	{"vendor_request_sent", "Vendor request accepted by WhatsApp", "vendor_requests.sent_at", "accepted by the API, not yet proof of delivery",
		`SELECT count(*) FROM vendor_requests WHERE sent_at >= $1 AND sent_at < $2`},
	{"vendor_request_send_failed", "Vendor request rejected by WhatsApp", "vendor_requests.send_failed_at", "e.g. 24h window closed (131047)",
		`SELECT count(*) FROM vendor_requests WHERE send_failed_at >= $1 AND send_failed_at < $2`},
	{"vendor_request_delivered", "Vendor request delivered", "messages.delivered_at (kind=vendor_request)", "from WhatsApp delivery receipts",
		`SELECT count(*) FROM messages WHERE direction = 'OUTBOUND' AND metadata->>'kind' = 'vendor_request' AND delivered_at >= $1 AND delivered_at < $2`},
	{"vendor_response_received", "Vendor response received", "vendor_responses.created_at (matched to a request)", "",
		`SELECT count(*) FROM vendor_responses WHERE vendor_request_id IS NOT NULL AND created_at >= $1 AND created_at < $2`},
	{"offer_created", "Offer created", "offers.created_at", "",
		`SELECT count(*) FROM offers WHERE status <> 'DUPLICATE' AND created_at >= $1 AND created_at < $2`},
	{"offer_sent", "Offer sent to buyer", "offers.sent_at", "accepted by the API",
		`SELECT count(*) FROM offers WHERE sent_at >= $1 AND sent_at < $2`},
	{"offer_message_delivered", "Offer message delivered", "messages.delivered_at (kind=offer_delivery)", "one message may carry several offers",
		`SELECT count(*) FROM messages WHERE direction = 'OUTBOUND' AND metadata->>'kind' = 'offer_delivery' AND delivered_at >= $1 AND delivered_at < $2`},
	{"offer_message_read", "Offer message read", "messages.read_at (kind=offer_delivery)", "only observable when the buyer has read receipts on — a lower bound, never 'unseen'",
		`SELECT count(*) FROM messages WHERE direction = 'OUTBOUND' AND metadata->>'kind' = 'offer_delivery' AND read_at >= $1 AND read_at < $2`},
	{"offer_selected", "Offer selected", "buyer_selections.created_at (ACTIVE)", "",
		`SELECT count(*) FROM buyer_selections WHERE status = 'ACTIVE' AND created_at >= $1 AND created_at < $2`},
	{"offer_selected_via_whatsapp", "…of which by buyer reply on WhatsApp", "buyer_selections.source = whatsapp_reply", "the rest are operator overrides",
		`SELECT count(*) FROM buyer_selections WHERE status = 'ACTIVE' AND source = 'whatsapp_reply' AND created_at >= $1 AND created_at < $2`},
	{"transaction_created", "Transaction created", "transactions.created_at", "payment is not executed yet (UNPAID)",
		`SELECT count(*) FROM transactions WHERE status <> 'CANCELLED' AND created_at >= $1 AND created_at < $2`},
	{"transaction_completed", "Transaction completed", "transactions.status = COMPLETED (updated_at)", "no code path completes a transaction yet — always 0 until fulfilment exists",
		`SELECT count(*) FROM transactions WHERE status = 'COMPLETED' AND updated_at >= $1 AND updated_at < $2`},
}

func (r *FunnelRepository) Funnel(ctx context.Context, since, until time.Time) ([]funnel.Stage, error) {
	out := make([]funnel.Stage, 0, len(funnelStages))
	for _, q := range funnelStages {
		var n int64
		if err := r.db.QueryRowContext(ctx, q.sql, since, until).Scan(&n); err != nil {
			return nil, fmt.Errorf("funnel stage %s: %w", q.key, err)
		}
		out = append(out, funnel.Stage{Key: q.key, Label: q.label, Count: n, Source: q.source, Note: q.note})
	}
	return out, nil
}

// supplyQuery: $1 = since, $2 = until, $3 = freshness interval (text,
// passed only to queries that use it). Each sql returns (value, of); of is
// NULL for plain counts. "now" snapshot queries still reference $1/$2
// (as a no-op) so every query takes the same window arguments.
type supplyQuery struct {
	key, label, kind, scope, source, note, sql string
}

var supplyQueries = []supplyQuery{
	{"vendor_submissions", "Vendor submissions (supply signals)", "count", "window", "supply_signals.created_at", "",
		`SELECT count(*), NULL::bigint FROM supply_signals WHERE created_at >= $1 AND created_at < $2`},
	{"submissions_per_submitting_vendor", "Submissions per submitting vendor", "per", "window", "supply_signals / distinct vendor_id", "",
		`SELECT count(*), count(DISTINCT vendor_id) FROM supply_signals WHERE created_at >= $1 AND created_at < $2`},
	{"valid_supply_rate", "Valid supply (product and price)", "rate", "window", "supply_signals with product and price / all", "",
		`SELECT count(*) FILTER (WHERE COALESCE(btrim(product), '') <> '' AND price IS NOT NULL), count(*)
		 FROM supply_signals WHERE created_at >= $1 AND created_at < $2`},
	{"vendor_supply_repeat_rate", "Vendors submitting on 2+ days", "rate", "window", "vendors with signals on >= 2 distinct UTC days / vendors with signals", "",
		`SELECT count(*) FILTER (WHERE days >= 2), count(*) FROM (
		   SELECT vendor_id, count(DISTINCT (created_at AT TIME ZONE 'UTC')::date) AS days
		   FROM supply_signals WHERE created_at >= $1 AND created_at < $2 GROUP BY vendor_id) v`},
	{"listings_drafted", "Listings drafted", "count", "window", "supply_listings.created_at", "",
		`SELECT count(*), NULL::bigint FROM supply_listings WHERE created_at >= $1 AND created_at < $2`},
	{"media_complete_rate", "Media-complete listings (1+ image)", "rate", "window", "supply_listings with an image media reference / listings drafted", "",
		`SELECT count(*) FILTER (WHERE EXISTS (SELECT 1 FROM jsonb_array_elements(media_references) m WHERE m->>'type' = 'image')), count(*)
		 FROM supply_listings WHERE created_at >= $1 AND created_at < $2`},
	{"listings_published", "Listings published", "count", "window", "supply_listings.published_at", "operator publication (SUPPLY_LISTING_PUBLISHED)",
		`SELECT count(*), NULL::bigint FROM supply_listings WHERE published_at >= $1 AND published_at < $2`},
	{"listings_rejected", "Drafts rejected", "count", "window", "supply_listings.rejected_at (never published)", "reasons: supply_listings.rejection_reason",
		`SELECT count(*), NULL::bigint FROM supply_listings WHERE rejected_at >= $1 AND rejected_at < $2 AND published_at IS NULL`},
	{"listing_approval_rate", "Listing approval rate", "rate", "window", "published / (published + drafts rejected)", "curation decisions made in the period",
		`SELECT (SELECT count(*) FROM supply_listings WHERE published_at >= $1 AND published_at < $2),
		        (SELECT count(*) FROM supply_listings WHERE published_at >= $1 AND published_at < $2)
		      + (SELECT count(*) FROM supply_listings WHERE rejected_at >= $1 AND rejected_at < $2 AND published_at IS NULL)`},
	{"listing_rejection_rate", "Listing rejection rate", "rate", "window", "drafts rejected / (published + drafts rejected)", "",
		`SELECT (SELECT count(*) FROM supply_listings WHERE rejected_at >= $1 AND rejected_at < $2 AND published_at IS NULL),
		        (SELECT count(*) FROM supply_listings WHERE published_at >= $1 AND published_at < $2)
		      + (SELECT count(*) FROM supply_listings WHERE rejected_at >= $1 AND rejected_at < $2 AND published_at IS NULL)`},
	{"listings_withdrawn", "Published listings withdrawn", "count", "window", "supply_listings.rejected_at (after publication)", "",
		`SELECT count(*), NULL::bigint FROM supply_listings WHERE rejected_at >= $1 AND rejected_at < $2 AND published_at IS NOT NULL`},
	{"stale_listing_rate", "Stale live listings", "rate", "now", "ACTIVE listings inside their lifecycle not verified within the freshness window / ACTIVE inside their lifecycle",
		"freshness window is SUPPLY_FRESHNESS_WINDOW [PARAM]; stale is not expired: these stay eligible and need reconfirmation",
		`SELECT count(*) FILTER (WHERE last_verified_at IS NULL OR last_verified_at < now() - $3::interval), count(*)
		 FROM supply_listings WHERE status = 'ACTIVE' AND expires_at > now() AND $1::timestamptz IS NOT NULL AND $2::timestamptz IS NOT NULL`},
	{"lifecycle_expired_listings", "ACTIVE listings past their lifecycle", "count", "now", "status ACTIVE and expires_at <= now",
		"logically expired (not eligible) though the status still says ACTIVE; kept as history, no expiry job rewrites the status",
		`SELECT count(*), NULL::bigint FROM supply_listings WHERE status = 'ACTIVE' AND expires_at <= now() AND $1::timestamptz IS NOT NULL AND $2::timestamptz IS NOT NULL`},
	{"usable_listings_per_active_vendor", "Usable listings per active vendor", "per", "now", "ACTIVE unexpired listings of ACTIVE vendors / ACTIVE vendors", "",
		`SELECT (SELECT count(*) FROM supply_listings l JOIN vendors v ON v.id = l.vendor_id
		         WHERE l.status = 'ACTIVE' AND l.expires_at > now() AND v.status = 'ACTIVE' AND $1::timestamptz IS NOT NULL AND $2::timestamptz IS NOT NULL),
		        (SELECT count(*) FROM vendors WHERE status = 'ACTIVE')`},
	{"vendor_response_rate", "Vendor response rate", "rate", "window", "vendor requests sent in the period with at least one matched response / vendor requests sent in the period", "responses may arrive after the period ends",
		`SELECT count(*) FILTER (WHERE EXISTS (SELECT 1 FROM vendor_responses r WHERE r.vendor_request_id = vr.id)), count(*)
		 FROM vendor_requests vr WHERE vr.sent_at >= $1 AND vr.sent_at < $2`},
}

// Supply implements funnel.SupplyReader (Architecture Lock V2 §K.2, the
// rows Phase 4A can measure; re-confirmation and discovery-eligible volume
// arrive with 4B).
func (r *FunnelRepository) Supply(ctx context.Context, since, until time.Time, freshness time.Duration) ([]funnel.SupplyMetric, error) {
	out := make([]funnel.SupplyMetric, 0, len(supplyQueries))
	interval := fmt.Sprintf("%d seconds", int64(freshness.Seconds()))
	for _, q := range supplyQueries {
		var v int64
		var of sql.NullInt64
		args := []any{since, until}
		query := q.sql
		if strings.Contains(query, "$3") {
			args = append(args, interval)
		}
		if err := r.db.QueryRowContext(ctx, query, args...).Scan(&v, &of); err != nil {
			return nil, fmt.Errorf("supply metric %s: %w", q.key, err)
		}
		m := funnel.SupplyMetric{Key: q.key, Label: q.label, Kind: q.kind, Value: v, Scope: q.scope, Source: q.source, Note: q.note}
		if of.Valid {
			d := of.Int64
			m.Of = &d
		}
		out = append(out, m)
	}
	return out, nil
}
