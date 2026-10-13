package postgres

import (
	"context"
	"time"

	"github.com/middleman/backend/internal/autonomy"
)

// hardeningMetrics fills the layered metrics (Phase 4C hardening). Every
// number comes from persisted rows (requests, transactions, clarifications,
// decisions, exceptions, notices, events), never from in-memory counters.
func (r *AutonomyRepository) hardeningMetrics(ctx context.Context, m *autonomy.Metrics, from time.Time) error {
	m.Definitions = autonomy.MetricDefinitions()

	// ---- workflow level -------------------------------------------------------------
	var started, completed, completedAuto, clarified, escalated, operator, held, ended, abandoned int
	if err := r.db.QueryRowContext(ctx, `WITH w AS (
		SELECT br.id, br.status,
			EXISTS (SELECT 1 FROM transactions t WHERE t.buyer_request_id = br.id) AS done,
			EXISTS (SELECT 1 FROM clarifications c WHERE c.status <> 'CANCELLED' AND NOT COALESCE(c.resolution->>'reason' = 'undelivered', false)
				AND (c.buyer_request_id = br.id OR c.vendor_request_id IN (SELECT id FROM vendor_requests WHERE buyer_request_id = br.id))) AS clarified,
			EXISTS (SELECT 1 FROM workflow_exceptions x WHERE x.buyer_request_id = br.id) AS escalated,
			EXISTS (SELECT 1 FROM events e WHERE e.entity_type = 'buyer_request' AND e.entity_id = br.id AND e.actor_type = 'OPERATOR'
				AND e.event_type IN ('REQUEST_CONFIRMED', 'CLARIFICATION_SENT'))
			OR EXISTS (SELECT 1 FROM events e JOIN vendor_requests v ON v.id = e.entity_id WHERE e.entity_type = 'vendor_request'
				AND v.buyer_request_id = br.id AND e.event_type = 'VENDORS_ROUTED' AND e.actor_type = 'OPERATOR')
			OR EXISTS (SELECT 1 FROM events e JOIN vendor_responses vr ON vr.id = e.entity_id JOIN vendor_requests v ON v.id = vr.vendor_request_id
				WHERE e.entity_type = 'vendor_response' AND v.buyer_request_id = br.id AND e.event_type = 'VENDOR_RESPONSE_ASSOCIATION_CORRECTED'
				AND e.actor_type = 'OPERATOR') AS operator,
			EXISTS (SELECT 1 FROM workflow_holds h WHERE h.buyer_request_id = br.id) AS held,
			EXISTS (SELECT 1 FROM workflow_decisions d WHERE d.buyer_request_id = br.id AND d.outcome = 'APPLIED'
				AND (d.decision = 'NO_MATCH' OR (d.decision = 'RECOVER_REQUEST' AND d.reason IN ('buyer_told_no_reply_yet',
				'max_recovery_rounds_reached', 'vendor_contact_limit_reached', 'auto_recovery_disabled')))) AS ended,
			br.status = 'AWAITING_BUYER' AND (SELECT c.status FROM clarifications c WHERE c.buyer_request_id = br.id
				ORDER BY c.asked_at DESC LIMIT 1) IN ('EXPIRED', 'EXHAUSTED') AS abandoned,
			EXISTS (SELECT 1 FROM workflow_exceptions x WHERE x.buyer_request_id = br.id AND x.class = 'SYSTEM_EXCEPTION') AS sysfail,
			EXISTS (SELECT 1 FROM buyer_selections s WHERE s.buyer_request_id = br.id AND s.source <> 'whatsapp_reply') AS opselect
		FROM buyer_requests br WHERE br.created_at >= $1)
		SELECT count(*), count(*) FILTER (WHERE done), count(*) FILTER (WHERE done AND NOT operator AND NOT sysfail AND NOT opselect),
			count(*) FILTER (WHERE clarified), count(*) FILTER (WHERE escalated), count(*) FILTER (WHERE operator),
			count(*) FILTER (WHERE held), count(*) FILTER (WHERE ended AND NOT done), count(*) FILTER (WHERE abandoned AND NOT done)
		FROM w`, from).Scan(&started, &completed, &completedAuto, &clarified, &escalated, &operator, &held, &ended, &abandoned); err != nil {
		return err
	}
	pending := started - completed - ended - abandoned
	if pending < 0 {
		pending = 0
	}
	m.Workflow = autonomy.WorkflowMetrics{Started: started,
		Completed: autonomy.NewRate(completed, started), CompletedAutonomously: autonomy.NewRate(completedAuto, started),
		RequiringClarification: autonomy.NewRate(clarified, started), EscalatedToOperator: autonomy.NewRate(escalated, started),
		OperatorPerformedStep: autonomy.NewRate(operator, started), Held: autonomy.NewRate(held, started),
		EndedWithoutOffer: autonomy.NewRate(ended, started), Abandoned: autonomy.NewRate(abandoned, started), Pending: autonomy.NewRate(pending, started)}

	// ---- question level ---------------------------------------------------------------
	var q autonomy.QuestionMetrics
	var answered, resolved, reasked, gaveUp, superseded int
	if err := r.db.QueryRowContext(ctx, `WITH c AS (
		SELECT c.*, COALESCE(c.resolution->>'reason' = 'undelivered', false) AS undelivered,
			EXISTS (SELECT 1 FROM clarifications l WHERE l.id <> c.id AND l.asked_at > c.asked_at AND l.status <> 'CANCELLED'
				AND ((c.buyer_request_id IS NOT NULL AND l.buyer_request_id = c.buyer_request_id AND COALESCE(l.field, '') = COALESCE(c.field, '') AND l.kind = c.kind)
				  OR (c.vendor_request_id IS NOT NULL AND l.vendor_request_id = c.vendor_request_id AND l.kind = c.kind))) AS asked_again
		FROM clarifications c WHERE c.asked_at >= $1)
		SELECT count(*) FILTER (WHERE status <> 'CANCELLED' AND NOT undelivered),
			COALESCE(sum(attempts) FILTER (WHERE status <> 'CANCELLED' AND NOT undelivered), 0),
			count(*) FILTER (WHERE status = 'ANSWERED'),
			count(*) FILTER (WHERE status = 'ANSWERED' AND NOT asked_again),
			count(*) FILTER (WHERE status <> 'CANCELLED' AND NOT undelivered AND (attempts > 1 OR asked_again)),
			count(*) FILTER (WHERE status IN ('EXPIRED', 'EXHAUSTED')),
			count(*) FILTER (WHERE status = 'SUPERSEDED' AND NOT undelivered),
			count(*) FILTER (WHERE status = 'OPEN'),
			count(*) FILTER (WHERE status <> 'CANCELLED' AND NOT undelivered AND party = 'BUYER'),
			count(*) FILTER (WHERE status <> 'CANCELLED' AND NOT undelivered AND party = 'VENDOR'),
			count(*) FILTER (WHERE undelivered)
		FROM c`, from).Scan(&q.Delivered, &q.MessagesSent, &answered, &resolved, &reasked, &gaveUp, &superseded, &q.Open,
		&q.BuyerDelivered, &q.VendorDelivered, &q.UndeliveredClosed); err != nil {
		return err
	}
	q.Answered, q.Resolved = autonomy.NewRate(answered, q.Delivered), autonomy.NewRate(resolved, q.Delivered)
	q.ReAsked, q.ExpiredOrGaveUp = autonomy.NewRate(reasked, q.Delivered), autonomy.NewRate(gaveUp, q.Delivered)
	q.Superseded = autonomy.NewRate(superseded, q.Delivered)
	m.Questions = q

	// ---- recovery ----------------------------------------------------------------------
	rec := autonomy.RecoveryMetrics{RecoveryDecisions: map[string]int{}}
	var retried, recovered int
	if err := r.db.QueryRowContext(ctx, `SELECT
		count(*) FILTER (WHERE decision = 'RETRY_DELIVERY' AND reason = 'retry_scheduled'),
		count(*) FILTER (WHERE decision = 'RETRY_DELIVERY' AND reason = 'recovered'),
		count(*) FILTER (WHERE decision = 'RETRY_DELIVERY' AND outcome = 'FAILED'),
		count(*) FILTER (WHERE decision = 'VENDOR_REQUEST_TIMEOUT' AND outcome = 'APPLIED'),
		count(*) FILTER (WHERE reason IN ('duplicate_recovery_prevented', 'duplicate_notice_prevented'))
		FROM workflow_decisions WHERE created_at >= $1`, from).Scan(&retried, &recovered, &rec.DeliveryRetriesFailed,
		&rec.VendorRequestsExpired, &rec.DuplicateActionsPrevented); err != nil {
		return err
	}
	rec.DeliveryRetries = autonomy.NewRate(recovered, retried)
	if err := r.eachRowAuto(ctx, `SELECT outcome || ':' || reason, count(*) FROM workflow_decisions WHERE decision = 'RECOVER_REQUEST'
		AND created_at >= $1 GROUP BY 1`, []any{from}, func(sc func(...any) error) error {
		var k string
		var n int
		if err := sc(&k, &n); err != nil {
			return err
		}
		rec.RecoveryDecisions[k] = n
		return nil
	}); err != nil {
		return err
	}
	var reroutedReqs, reroutedWithOffer int
	if err := r.db.QueryRowContext(ctx, `SELECT count(DISTINCT d.buyer_request_id),
		count(DISTINCT d.buyer_request_id) FILTER (WHERE EXISTS (SELECT 1 FROM offers o WHERE o.buyer_request_id = d.buyer_request_id AND o.created_at > d.created_at))
		FROM workflow_decisions d WHERE d.decision = 'RECOVER_REQUEST' AND d.outcome = 'APPLIED' AND d.reason = 'rerouted_to_alternative_vendors'
		AND d.created_at >= $1`, from).Scan(&reroutedReqs, &reroutedWithOffer); err != nil {
		return err
	}
	rec.Rerouted = reroutedReqs
	rec.RerouteLedToOffer = autonomy.NewRate(reroutedWithOffer, reroutedReqs)
	m.Recovery = rec

	// ---- delivery ----------------------------------------------------------------------
	d := autonomy.DeliveryMetrics{}
	if err := r.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM workflow_decisions WHERE created_at >= $1 AND decision IN ('RETRY_DELIVERY', 'NOTIFY_BUYER')
			AND (fallback = 'POLICY_REJECTION' OR result->>'class' = 'POLICY_RESTRICTED') AND outcome IN ('REJECTED', 'DEFERRED')),
		(SELECT count(*) FROM pending_notices WHERE created_at >= $1),
		(SELECT count(*) FROM pending_notices WHERE created_at >= $1 AND status = 'SENT'),
		(SELECT count(*) FROM pending_notices WHERE created_at >= $1 AND status = 'PENDING'),
		(SELECT count(*) FROM pending_notices WHERE created_at >= $1 AND status = 'CANCELLED'),
		(SELECT count(*) FROM events WHERE created_at >= $1 AND event_type = 'OUTBOUND_DELIVERY_FAILED_CLASSIFIED'),
		(SELECT count(*) FROM workflow_exceptions WHERE created_at >= $1 AND class = 'SYSTEM_EXCEPTION'),
		(SELECT count(*) FROM workflow_exceptions WHERE created_at >= $1 AND class = 'SAFETY_EXCEPTION')`, from).Scan(&d.PolicyBlocked,
		&d.NoticesQueued, &d.NoticesDelivered, &d.NoticesPending, &d.NoticesCancelled, &d.ProviderFailures, &d.SystemExceptions, &d.SafetyExceptions); err != nil {
		return err
	}
	m.Delivery = d
	return nil
}
