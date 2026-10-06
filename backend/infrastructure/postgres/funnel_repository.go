package postgres

import (
	"context"
	"database/sql"
	"fmt"
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
