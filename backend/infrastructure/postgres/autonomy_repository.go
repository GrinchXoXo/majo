package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/autonomy"
)

// AutonomyRepository is the PostgreSQL store for the autonomous runtime
// (Phase 4C correction): clarifications, the decision log, the operator
// exception queue and per-workflow advisory locks.
type AutonomyRepository struct {
	db *sql.DB
}

func NewAutonomyRepository(db *sql.DB) *AutonomyRepository { return &AutonomyRepository{db: db} }

var _ autonomy.Store = (*AutonomyRepository)(nil)

const clarificationColumns = `id, party, conversation_id, buyer_id, vendor_id, buyer_request_id, vendor_request_id, vendor_response_id,
	kind, COALESCE(field, ''), question, options, context, status, attempts, question_message_id, answer_message_id,
	COALESCE(answer_text, ''), resolution, asked_at, expires_at, resolved_at`

func scanClarification(sc interface{ Scan(...any) error }) (*autonomy.Clarification, error) {
	var (
		c                                              autonomy.Clarification
		id, party                                      string
		conv, buyer, vendor, br, vr, vresp, qmsg, amsg sql.NullString
		opts, cctx, res                                []byte
		resolved                                       sql.NullTime
	)
	if err := sc.Scan(&id, &party, &conv, &buyer, &vendor, &br, &vr, &vresp, &c.Kind, &c.Field, &c.Question, &opts, &cctx,
		&c.Status, &c.Attempts, &qmsg, &amsg, &c.AnswerText, &res, &c.AskedAt, &c.ExpiresAt, &resolved); err != nil {
		return nil, err
	}
	c.ID, _ = uuid.Parse(id)
	c.Party = autonomy.Party(party)
	c.ConversationID, c.BuyerID, c.VendorID = optUUID(conv), optUUID(buyer), optUUID(vendor)
	c.BuyerRequestID, c.VendorRequestID, c.VendorResponseID = optUUID(br), optUUID(vr), optUUID(vresp)
	c.QuestionMessageID, c.AnswerMessageID = optUUID(qmsg), optUUID(amsg)
	_ = json.Unmarshal(opts, &c.Options)
	_ = json.Unmarshal(cctx, &c.Context)
	if len(res) > 0 {
		_ = json.Unmarshal(res, &c.Resolution)
	}
	if c.Options == nil {
		c.Options = []autonomy.Option{}
	}
	if c.Context == nil {
		c.Context = map[string]any{}
	}
	c.ResolvedAt = optTime(resolved)
	return &c, nil
}

// OpenClarification implements autonomy.Store: any OPEN question for the
// same party conversation is superseded in the same transaction.
func (r *AutonomyRepository) OpenClarification(ctx context.Context, c *autonomy.Clarification) (*autonomy.Clarification, error) {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	if c.Attempts < 1 {
		c.Attempts = 1
	}
	if c.Options == nil {
		c.Options = []autonomy.Option{}
	}
	if c.Context == nil {
		c.Context = map[string]any{}
	}
	opts, _ := json.Marshal(c.Options)
	cctx, _ := json.Marshal(c.Context)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	switch c.Party {
	case autonomy.PartyBuyer:
		if c.ConversationID == nil {
			return nil, fmt.Errorf("buyer clarification needs a conversation")
		}
		_, err = tx.ExecContext(ctx, `UPDATE clarifications SET status = 'SUPERSEDED', resolved_at = now(), updated_at = now()
			WHERE party = 'BUYER' AND conversation_id = $1 AND status = 'OPEN'`, c.ConversationID.String())
	case autonomy.PartyVendor:
		if c.VendorID == nil {
			return nil, fmt.Errorf("vendor clarification needs a vendor")
		}
		_, err = tx.ExecContext(ctx, `UPDATE clarifications SET status = 'SUPERSEDED', resolved_at = now(), updated_at = now()
			WHERE party = 'VENDOR' AND vendor_id = $1 AND status = 'OPEN'`, c.VendorID.String())
	default:
		return nil, fmt.Errorf("unknown clarification party %q", c.Party)
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO clarifications
		(id, party, conversation_id, buyer_id, vendor_id, buyer_request_id, vendor_request_id, vendor_response_id, kind, field,
		 question, options, context, status, attempts, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'OPEN', $14, $15)`,
		c.ID.String(), string(c.Party), uuidArg(c.ConversationID), uuidArg(c.BuyerID), uuidArg(c.VendorID), uuidArg(c.BuyerRequestID),
		uuidArg(c.VendorRequestID), uuidArg(c.VendorResponseID), c.Kind, nullIfEmpty(c.Field), c.Question, opts, cctx, c.Attempts, c.ExpiresAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return r.findClarification(ctx, c.ID)
}

// OpenClarificationIfFree implements autonomy.Store.
func (r *AutonomyRepository) OpenClarificationIfFree(ctx context.Context, c *autonomy.Clarification) (*autonomy.Clarification, *autonomy.Clarification, error) {
	if c.Party != autonomy.PartyBuyer || c.ConversationID == nil {
		opened, err := r.OpenClarification(ctx, c)
		return opened, nil, err
	}
	var blocking *autonomy.Clarification
	var opened *autonomy.Clarification
	err := r.WithLock(ctx, "conversation-question:"+c.ConversationID.String(), func(ctx context.Context) error {
		cur, err := scanClarification(r.db.QueryRowContext(ctx, `SELECT `+clarificationColumns+` FROM clarifications
			WHERE party = 'BUYER' AND conversation_id = $1 AND status = 'OPEN' AND expires_at > now()`, c.ConversationID.String()))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && (cur.BuyerRequestID == nil || c.BuyerRequestID == nil || *cur.BuyerRequestID != *c.BuyerRequestID) {
			blocking = cur
			return nil
		}
		opened, err = r.OpenClarification(ctx, c)
		return err
	})
	return opened, blocking, err
}

func (r *AutonomyRepository) findClarification(ctx context.Context, id uuid.UUID) (*autonomy.Clarification, error) {
	return scanClarification(r.db.QueryRowContext(ctx, `SELECT `+clarificationColumns+` FROM clarifications WHERE id = $1`, id.String()))
}

func (r *AutonomyRepository) SetClarificationMessage(ctx context.Context, id, messageID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `UPDATE clarifications SET question_message_id = $2, updated_at = now() WHERE id = $1`, id.String(), messageID.String())
	return err
}

func (r *AutonomyRepository) openWhere(ctx context.Context, where string, arg string, now time.Time) (*autonomy.Clarification, error) {
	// Expired questions are closed lazily (no scheduler).
	if _, err := r.db.ExecContext(ctx, `UPDATE clarifications SET status = 'EXPIRED', resolved_at = now(), updated_at = now()
		WHERE `+where+` AND status = 'OPEN' AND expires_at <= $2`, arg, now); err != nil {
		return nil, err
	}
	c, err := scanClarification(r.db.QueryRowContext(ctx, `SELECT `+clarificationColumns+` FROM clarifications
		WHERE `+where+` AND status = 'OPEN' ORDER BY asked_at DESC LIMIT 1`, arg))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (r *AutonomyRepository) OpenBuyerClarification(ctx context.Context, conversationID uuid.UUID, now time.Time) (*autonomy.Clarification, error) {
	return r.openWhere(ctx, `party = 'BUYER' AND conversation_id = $1`, conversationID.String(), now)
}

func (r *AutonomyRepository) OpenVendorClarification(ctx context.Context, vendorID uuid.UUID, now time.Time) (*autonomy.Clarification, error) {
	return r.openWhere(ctx, `party = 'VENDOR' AND vendor_id = $1`, vendorID.String(), now)
}

func (r *AutonomyRepository) ResolveClarification(ctx context.Context, id uuid.UUID, status string, answerMessageID *uuid.UUID, answer string, resolution map[string]any, at time.Time) error {
	res, _ := json.Marshal(resolution)
	out, err := r.db.ExecContext(ctx, `UPDATE clarifications SET status = $2, answer_message_id = $3, answer_text = $4, resolution = $5,
		resolved_at = $6, updated_at = now() WHERE id = $1 AND status = 'OPEN'`,
		id.String(), status, uuidArg(answerMessageID), nullIfEmpty(truncateText(answer, 1000)), res, at)
	if err != nil {
		return err
	}
	if n, _ := out.RowsAffected(); n == 0 {
		return autonomy.ErrClarificationNotOpen
	}
	return nil
}

func (r *AutonomyRepository) BumpClarification(ctx context.Context, id uuid.UUID, question string, expiresAt time.Time) (*autonomy.Clarification, error) {
	out, err := r.db.ExecContext(ctx, `UPDATE clarifications SET attempts = attempts + 1, question = $2, expires_at = $3, asked_at = now(),
		updated_at = now() WHERE id = $1 AND status = 'OPEN'`, id.String(), question, expiresAt)
	if err != nil {
		return nil, err
	}
	if n, _ := out.RowsAffected(); n == 0 {
		return nil, autonomy.ErrClarificationNotOpen
	}
	return r.findClarification(ctx, id)
}

func (r *AutonomyRepository) ClarificationsForRequest(ctx context.Context, buyerRequestID uuid.UUID) ([]*autonomy.Clarification, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+clarificationColumns+` FROM clarifications WHERE buyer_request_id = $1 ORDER BY asked_at, id`, buyerRequestID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*autonomy.Clarification{}
	for rows.Next() {
		c, err := scanClarification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---- decisions ------------------------------------------------------------------

func (r *AutonomyRepository) RecordDecision(ctx context.Context, d *autonomy.Decision) error {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	if d.PolicyVersion == "" {
		d.PolicyVersion = autonomy.PolicyVersion
	}
	if d.Actor == "" {
		d.Actor = "SYSTEM"
	}
	in, _ := json.Marshal(nonNilMap(d.Inputs))
	res, _ := json.Marshal(nonNilMap(d.Result))
	var conf any
	if d.Confidence != nil {
		conf = *d.Confidence
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO workflow_decisions
		(id, workflow_type, subject_type, subject_id, buyer_request_id, decision, outcome, fallback, reason, confidence, policy_version, inputs, result, actor)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		d.ID.String(), d.WorkflowType, d.SubjectType, d.SubjectID.String(), uuidArg(d.BuyerRequestID), d.Kind, d.Outcome,
		nullIfEmpty(string(d.Fallback)), nullIfEmpty(d.Reason), conf, d.PolicyVersion, in, res, d.Actor)
	return err
}

func nonNilMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func (r *AutonomyRepository) DecisionsForRequest(ctx context.Context, buyerRequestID uuid.UUID) ([]*autonomy.Decision, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, workflow_type, subject_type, subject_id, buyer_request_id, decision, outcome,
		COALESCE(fallback, ''), COALESCE(reason, ''), confidence, policy_version, inputs, result, actor, created_at
		FROM workflow_decisions WHERE buyer_request_id = $1 ORDER BY created_at, id`, buyerRequestID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*autonomy.Decision{}
	for rows.Next() {
		var (
			d            autonomy.Decision
			id, subj, fb string
			br           sql.NullString
			conf         sql.NullFloat64
			in, res      []byte
		)
		if err := rows.Scan(&id, &d.WorkflowType, &d.SubjectType, &subj, &br, &d.Kind, &d.Outcome, &fb, &d.Reason, &conf,
			&d.PolicyVersion, &in, &res, &d.Actor, &d.CreatedAt); err != nil {
			return nil, err
		}
		d.ID, _ = uuid.Parse(id)
		d.SubjectID, _ = uuid.Parse(subj)
		d.BuyerRequestID = optUUID(br)
		d.Fallback = autonomy.Fallback(fb)
		if conf.Valid {
			v := conf.Float64
			d.Confidence = &v
		}
		_ = json.Unmarshal(in, &d.Inputs)
		_ = json.Unmarshal(res, &d.Result)
		out = append(out, &d)
	}
	return out, rows.Err()
}

// ---- exceptions -------------------------------------------------------------------

const exceptionColumns = `id, class, reason, subject_type, subject_id, buyer_request_id, detail, status, created_at, resolved_at,
	COALESCE(resolved_by, ''), COALESCE(resolution, '')`

func scanException(sc interface{ Scan(...any) error }) (*autonomy.Exception, error) {
	var (
		e               autonomy.Exception
		id, class, subj string
		br              sql.NullString
		detail          []byte
		resolved        sql.NullTime
	)
	if err := sc.Scan(&id, &class, &e.Reason, &e.SubjectType, &subj, &br, &detail, &e.Status, &e.CreatedAt, &resolved, &e.ResolvedBy, &e.Resolution); err != nil {
		return nil, err
	}
	e.ID, _ = uuid.Parse(id)
	e.Class = autonomy.Fallback(class)
	e.SubjectID, _ = uuid.Parse(subj)
	e.BuyerRequestID = optUUID(br)
	_ = json.Unmarshal(detail, &e.Detail)
	e.ResolvedAt = optTime(resolved)
	return &e, nil
}

// RaiseException implements autonomy.Store. Idempotent per open (subject,
// reason): a repeat returns the open exception.
func (r *AutonomyRepository) RaiseException(ctx context.Context, e *autonomy.Exception) (*autonomy.Exception, error) {
	if !e.Class.NeedsOperator() {
		return nil, fmt.Errorf("exception class %q does not need an operator", e.Class)
	}
	detail, _ := json.Marshal(nonNilMap(e.Detail))
	var id string
	err := r.db.QueryRowContext(ctx, `INSERT INTO workflow_exceptions (class, reason, subject_type, subject_id, buyer_request_id, detail)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (subject_type, subject_id, reason) WHERE status = 'OPEN' DO NOTHING RETURNING id`,
		string(e.Class), e.Reason, e.SubjectType, e.SubjectID.String(), uuidArg(e.BuyerRequestID), detail).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// Another writer holds the open exception; it may not be committed yet.
		for i := 0; i < 5; i++ {
			ex, ferr := scanException(r.db.QueryRowContext(ctx, `SELECT `+exceptionColumns+` FROM workflow_exceptions
				WHERE subject_type = $1 AND subject_id = $2 AND reason = $3 AND status = 'OPEN'`, e.SubjectType, e.SubjectID.String(), e.Reason))
			if !errors.Is(ferr, sql.ErrNoRows) {
				return ex, ferr
			}
			time.Sleep(20 * time.Millisecond)
		}
		return nil, fmt.Errorf("open exception for %s %s (%s) is not visible yet", e.SubjectType, e.SubjectID, e.Reason)
	}
	if err != nil {
		return nil, err
	}
	return scanException(r.db.QueryRowContext(ctx, `SELECT `+exceptionColumns+` FROM workflow_exceptions WHERE id = $1`, id))
}

func (r *AutonomyRepository) ListExceptions(ctx context.Context, status string, limit int) ([]*autonomy.Exception, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT ` + exceptionColumns + ` FROM workflow_exceptions`
	args := []any{limit}
	if status != "" {
		q += ` WHERE status = $2`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC LIMIT $1`
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*autonomy.Exception{}
	for rows.Next() {
		e, err := scanException(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *AutonomyRepository) ResolveException(ctx context.Context, id uuid.UUID, by, resolution string, at time.Time) error {
	out, err := r.db.ExecContext(ctx, `UPDATE workflow_exceptions SET status = 'RESOLVED', resolved_at = $2, resolved_by = $3, resolution = $4
		WHERE id = $1 AND status = 'OPEN'`, id.String(), at, nullIfEmpty(by), nullIfEmpty(resolution))
	if err != nil {
		return err
	}
	if n, _ := out.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// WithLock implements autonomy.Store with a session-level advisory lock held
// on one dedicated connection for the duration of fn. Waiting never pins a
// connection: pg_try_advisory_lock is retried with backoff (the connection is
// returned to the pool between tries) and gives up after lockWait, so a burst
// can never exhaust the pool and hang (the caller's job is retried).
func (r *AutonomyRepository) WithLock(ctx context.Context, key string, fn func(ctx context.Context) error) error {
	deadline := time.Now().Add(lockWait)
	backoff := 10 * time.Millisecond
	for {
		conn, err := r.db.Conn(ctx)
		if err != nil {
			return err
		}
		var got bool
		if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, "workflow:"+key).Scan(&got); err != nil {
			conn.Close()
			return err
		}
		if got {
			defer conn.Close()
			defer func() {
				_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, "workflow:"+key)
			}()
			return fn(ctx)
		}
		conn.Close()
		if time.Now().After(deadline) {
			return fmt.Errorf("workflow lock %q busy for %s", key, lockWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 200*time.Millisecond {
			backoff *= 2
		}
	}
}

// lockWait bounds how long a workflow step waits for another step on the
// same request/vendor/conversation.
var lockWait = 30 * time.Second

// Metrics computes the autonomy report from system-of-record tables
// (events with their actor, clarifications, decisions, exceptions, vendor
// requests, selections). since nil = all time.
func (r *AutonomyRepository) Metrics(ctx context.Context, since *time.Time) (*autonomy.Metrics, error) {
	from := time.Unix(0, 0).UTC()
	if since != nil {
		from = *since
	}
	m := &autonomy.Metrics{Since: since, ByClass: map[string]int{}, InterventionsByReason: map[string]int{},
		FailuresByCategory: map[string]int{}, ExceptionsByReason: map[string]int{}, Decisions: map[string]int{},
		ClarificationsByParty: map[string]autonomy.Rate{}}
	rows, err := r.db.QueryContext(ctx, `SELECT br.id,
		EXISTS (SELECT 1 FROM events e WHERE e.entity_type = 'buyer_request' AND e.entity_id = br.id AND e.event_type = 'REQUEST_CONFIRMED' AND e.actor_type = 'OPERATOR'),
		EXISTS (SELECT 1 FROM events e JOIN vendor_requests v ON v.id = e.entity_id WHERE e.entity_type = 'vendor_request' AND v.buyer_request_id = br.id
			AND e.event_type = 'VENDORS_ROUTED' AND e.actor_type = 'OPERATOR'),
		EXISTS (SELECT 1 FROM events e WHERE e.entity_type = 'buyer_request' AND e.entity_id = br.id AND e.event_type = 'CLARIFICATION_SENT' AND e.actor_type = 'OPERATOR'),
		EXISTS (SELECT 1 FROM events e JOIN vendor_responses vr ON vr.id = e.entity_id JOIN vendor_requests v ON v.id = vr.vendor_request_id
			WHERE e.entity_type = 'vendor_response' AND v.buyer_request_id = br.id AND e.event_type = 'VENDOR_RESPONSE_ASSOCIATION_CORRECTED' AND e.actor_type = 'OPERATOR'),
		EXISTS (SELECT 1 FROM events e WHERE e.entity_type = 'buyer_request' AND e.entity_id = br.id AND e.event_type = 'REQUEST_CORRECTED' AND e.actor_type = 'OPERATOR')
		OR EXISTS (SELECT 1 FROM events e JOIN vendor_responses vr ON vr.id = e.entity_id JOIN vendor_requests v ON v.id = vr.vendor_request_id
			WHERE e.entity_type = 'vendor_response' AND v.buyer_request_id = br.id AND e.event_type = 'VENDOR_RESPONSE_CORRECTED' AND e.actor_type = 'OPERATOR'),
		EXISTS (SELECT 1 FROM buyer_selections s WHERE s.buyer_request_id = br.id AND s.source <> 'whatsapp_reply'),
		EXISTS (SELECT 1 FROM clarifications c WHERE c.status = 'ANSWERED' AND (c.buyer_request_id = br.id
			OR c.vendor_request_id IN (SELECT id FROM vendor_requests WHERE buyer_request_id = br.id))),
		EXISTS (SELECT 1 FROM workflow_decisions d WHERE d.buyer_request_id = br.id AND d.decision = 'RETRY_DELIVERY' AND d.reason = 'recovered'),
		EXISTS (SELECT 1 FROM workflow_exceptions x WHERE x.buyer_request_id = br.id AND x.class = 'SYSTEM_EXCEPTION'),
		EXISTS (SELECT 1 FROM transactions t WHERE t.buyer_request_id = br.id),
		EXISTS (SELECT 1 FROM vendor_requests v WHERE v.buyer_request_id = br.id)
		OR EXISTS (SELECT 1 FROM workflow_decisions d WHERE d.buyer_request_id = br.id AND (d.decision = 'NO_MATCH'
			OR (d.decision = 'ROUTE_REQUEST' AND d.outcome = 'DEFERRED')))
		FROM buyer_requests br WHERE br.created_at >= $1`, from)
	if err != nil {
		return nil, err
	}
	var autonomousProgressed, txTotal, txAutonomous, intervened int
	for rows.Next() {
		var id string
		var f autonomy.WorkflowFacts
		if err := rows.Scan(&id, &f.OperatorConfirmed, &f.OperatorRouted, &f.OperatorClarified, &f.OperatorAssociated, &f.OperatorCorrected,
			&f.OperatorSelected, &f.Clarified, &f.Recovered, &f.SystemException, &f.HasTransaction, &f.Progressed); err != nil {
			rows.Close()
			return nil, err
		}
		m.Workflows++
		class := autonomy.Classify(f)
		m.ByClass[class]++
		if autonomy.AutonomousClass(class) && f.Progressed {
			autonomousProgressed++
		}
		if f.HasTransaction {
			txTotal++
			if autonomy.AutonomousClass(class) {
				txAutonomous++
			}
		}
		if class == autonomy.ClassOperatorIntervention {
			intervened++
		}
		for reason, on := range map[string]bool{"manual_confirmation": f.OperatorConfirmed, "manual_routing": f.OperatorRouted,
			"ambiguous_intent": f.OperatorClarified, "vendor_association": f.OperatorAssociated, "operator_correction": f.OperatorCorrected,
			"operator_selection": f.OperatorSelected, "system_failure": f.SystemException} {
			if on {
				m.InterventionsByReason[reason]++
			}
		}
	}
	rows.Close()
	m.AutonomousRequestHandling = autonomy.NewRate(autonomousProgressed, m.Workflows)
	m.AutonomousCompletion = autonomy.NewRate(txAutonomous, txTotal)
	m.OperatorIntervention = autonomy.NewRate(intervened, m.Workflows)

	var routedOp, routedAll, confOp, confAll int
	if err := r.db.QueryRowContext(ctx, `SELECT
		count(*) FILTER (WHERE event_type = 'VENDORS_ROUTED' AND actor_type = 'OPERATOR'),
		count(*) FILTER (WHERE event_type = 'VENDORS_ROUTED'),
		count(*) FILTER (WHERE event_type = 'REQUEST_CONFIRMED' AND actor_type = 'OPERATOR'),
		count(*) FILTER (WHERE event_type = 'REQUEST_CONFIRMED')
		FROM events WHERE created_at >= $1 AND event_type IN ('VENDORS_ROUTED', 'REQUEST_CONFIRMED')`, from).Scan(&routedOp, &routedAll, &confOp, &confAll); err != nil {
		return nil, err
	}
	m.ManualRouting = autonomy.NewRate(routedOp, routedAll)
	m.ManualConfirmation = autonomy.NewRate(confOp, confAll)

	var answered, issued int
	if err := r.eachRowAuto(ctx, `SELECT party, count(*) FILTER (WHERE status = 'ANSWERED'), count(*) FILTER (WHERE status <> 'CANCELLED')
		FROM clarifications WHERE asked_at >= $1 GROUP BY party`, []any{from}, func(sc func(...any) error) error {
		var party string
		var a, i int
		if err := sc(&party, &a, &i); err != nil {
			return err
		}
		answered += a
		issued += i
		m.ClarificationsByParty[party] = autonomy.NewRate(a, i)
		return nil
	}); err != nil {
		return nil, err
	}
	m.ClarificationResolution = autonomy.NewRate(answered, issued)

	var recovered, scheduled int
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE reason = 'recovered'), count(*) FILTER (WHERE reason = 'retry_scheduled')
		FROM workflow_decisions WHERE decision = 'RETRY_DELIVERY' AND created_at >= $1`, from).Scan(&recovered, &scheduled); err != nil {
		return nil, err
	}
	m.AutomaticRecovery = autonomy.NewRate(recovered, scheduled)

	if err := r.eachRowAuto(ctx, `SELECT decision || ':' || outcome, COALESCE(fallback, ''), count(*) FROM workflow_decisions
		WHERE created_at >= $1 GROUP BY 1, 2`, []any{from}, func(sc func(...any) error) error {
		var k, fb string
		var n int
		if err := sc(&k, &fb, &n); err != nil {
			return err
		}
		m.Decisions[k] += n
		if fb != "" && fb != string(autonomy.ClarificationRequired) && fb != string(autonomy.RetryRequired) {
			m.FailuresByCategory[fb] += n
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := r.eachRowAuto(ctx, `SELECT reason, status, count(*) FROM workflow_exceptions WHERE created_at >= $1 GROUP BY 1, 2`, []any{from},
		func(sc func(...any) error) error {
			var reason, status string
			var n int
			if err := sc(&reason, &status, &n); err != nil {
				return err
			}
			m.ExceptionsByReason[reason] += n
			if status == autonomy.ExceptionOpen {
				m.OpenExceptions += n
			}
			return nil
		}); err != nil {
		return nil, err
	}
	if err := r.hardeningMetrics(ctx, m, from); err != nil {
		return nil, err
	}
	return m, nil
}

func (r *AutonomyRepository) eachRowAuto(ctx context.Context, q string, args []any, fn func(scan func(...any) error) error) error {
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows.Scan); err != nil {
			return err
		}
	}
	return rows.Err()
}
