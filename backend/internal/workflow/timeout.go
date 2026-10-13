package workflow

// Vendor-request deadlines and request recovery (Phase 4C hardening).
//
// A vendor request that is sent gets a durable deadline job (sent_at +
// VENDOR_REQUEST_TTL). When it fires, the request is expired with a
// conditional update (exactly one worker wins; a reply that arrived first
// wins over the expiry). Then the buyer's request is re-evaluated under its
// workflow lock: if every contacted vendor has gone quiet, could not be
// reached, or declined, and the buyer has no options yet, Majo contacts
// eligible alternative vendors (bounded per round and in total), or asks the
// buyer whether to search other areas (only through the canonical routing
// policy, which already applies the geographic expansion policy), or tells
// the buyer the honest status. It never declares the item unavailable just
// because a vendor did not reply, and a late reply still becomes an offer.
//
// The same evaluation runs when a vendor request could not be delivered,
// when its send retries ran out, and when a vendor declines.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/autonomy"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/jobs"
	"github.com/middleman/backend/internal/matching"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/routing"
)

// JobVendorTimeout runs a vendor request's deadline / recovery check.
const JobVendorTimeout = "VENDOR_REQUEST_TIMEOUT"

// unsentGrace: a PENDING vendor request older than this was never sent (the
// process died between creating and sending it).
const unsentGrace = 10 * time.Minute

// VendorRequestTimeouts is the vendor-request persistence the deadline job
// needs (postgres.RoutingRepository).
type VendorRequestTimeouts interface {
	FindVendorRequestByID(ctx context.Context, id uuid.UUID) (*routing.VendorRequest, error)
	ExpireOne(ctx context.Context, id uuid.UUID, cutoff time.Time) (bool, error)
	MarkUndelivered(ctx context.Context, id uuid.UUID, reason string) (bool, error)
}

// RecoveryFacts is the persisted state recovery decides on.
type RecoveryFacts struct {
	Status           request.Status `json:"status"`
	HasSelection     bool           `json:"has_selection"`
	HasTransaction   bool           `json:"has_transaction"`
	Contacted        int            `json:"contacted"`         // vendor requests ever created for the request
	Awaiting         int            `json:"awaiting"`          // SENT, or PENDING younger than unsentGrace
	Usable           int            `json:"usable_responses"`  // eligible vendor replies
	Undecided        int            `json:"undecided_replies"` // replies still being clarified / reviewed
	ActiveOffers     int            `json:"active_offers"`
	Superseded       bool           `json:"superseded"` // a newer confirmed request for the same product exists
	Held             bool           `json:"held"`
	ContactedVendors []uuid.UUID    `json:"-"`
}

// TimeoutCandidate is a vendor request the startup sweep must (re)schedule.
type TimeoutCandidate struct {
	VendorRequestID uuid.UUID
	Status          routing.VendorRequestStatus
	SentAt          *time.Time
	CreatedAt       time.Time
}

// RecoveryReader reads recovery state (postgres.WorkflowRepository).
type RecoveryReader interface {
	RecoveryFacts(ctx context.Context, buyerRequestID uuid.UUID, unsentGrace time.Duration) (*RecoveryFacts, error)
	// TimeoutCandidates: SENT requests (any age), PENDING older than
	// unsentGrace, and EXPIRED / SEND_FAILED requests of confirmed buyer
	// requests (last 30 days) that have no timeout decision yet.
	TimeoutCandidates(ctx context.Context, unsentGrace time.Duration, limit int) ([]TimeoutCandidate, error)
}

// ---- routing.LifecycleListener ---------------------------------------------------

// VendorRequestSent schedules the request's deadline.
func (e *Engine) VendorRequestSent(ctx context.Context, vr *routing.VendorRequest) {
	at := e.now().UTC().Add(e.cfg.Params.VendorRequestTTL)
	e.scheduleTimeout(ctx, vr.ID, at, "deadline")
}

// VendorRequestExpired: the vendor path noticed an overdue request first
// (it expired it while handling the vendor's next message). Recovery runs.
func (e *Engine) VendorRequestExpired(ctx context.Context, vr *routing.VendorRequest) {
	e.scheduleTimeout(ctx, vr.ID, e.now().UTC(), "expired_on_vendor_message")
}

func (e *Engine) enqueueRecoveryCheck(ctx context.Context, vrID uuid.UUID, trigger string) {
	e.scheduleTimeout(ctx, vrID, e.now().UTC(), trigger)
}

func (e *Engine) scheduleTimeout(ctx context.Context, vrID uuid.UUID, at time.Time, trigger string) {
	if e.jobs == nil {
		return
	}
	// One job per (request, trigger, minute): a repeated trigger is a no-op;
	// the handler is idempotent anyway.
	key := fmt.Sprintf("vr-timeout:%s:%s:%d", vrID, trigger, at.Unix()/60)
	if _, err := e.jobs.Enqueue(ctx, &jobs.Job{Type: JobVendorTimeout, IdempotencyKey: key,
		Payload: map[string]any{"vendor_request_id": vrID.String(), "trigger": trigger}, MaxAttempts: e.cfg.JobMaxAttempts, RunAt: at}); err != nil {
		e.logger.Error("scheduling vendor request deadline", "vendor_request_id", vrID, "error", err)
	}
}

// RecoverTimeouts (startup) schedules deadlines for vendor requests whose
// job is missing (sent before this version, or lost) and recovery checks for
// overdue / undelivered ones. Idempotent: the handler deduplicates.
func (e *Engine) RecoverTimeouts(ctx context.Context) {
	if e.recovery == nil {
		return
	}
	cands, err := e.recovery.TimeoutCandidates(ctx, unsentGrace, 1000)
	if err != nil {
		e.logger.Error("vendor request deadline sweep", "error", err)
		return
	}
	now := e.now().UTC()
	for _, c := range cands {
		at := now
		if c.Status == routing.VendorRequestSent && c.SentAt != nil {
			if d := c.SentAt.Add(e.cfg.Params.VendorRequestTTL); d.After(now) {
				at = d
			}
		}
		trigger := "sweep_" + strings.ToLower(string(c.Status))
		if at.After(now) {
			trigger = "deadline"
		}
		e.scheduleTimeout(ctx, c.VendorRequestID, at, trigger)
	}
	if len(cands) > 0 {
		e.logger.Info("vendor request deadlines scheduled", "count", len(cands))
	}
}

// handleVendorTimeout is the deadline / recovery job.
func (e *Engine) handleVendorTimeout(ctx context.Context, j *jobs.Job) error {
	id := parseOptUUID(j.Payload["vendor_request_id"])
	if id == nil || e.vrs == nil {
		return jobs.Permanent(errors.New("malformed vendor timeout payload"))
	}
	return e.HandleVendorTimeout(ctx, *id, fmt.Sprint(j.Payload["trigger"]))
}

// HandleVendorTimeout processes one vendor request's deadline (or a
// recovery trigger for it). Safe to run any number of times, concurrently.
func (e *Engine) HandleVendorTimeout(ctx context.Context, vrID uuid.UUID, trigger string) error {
	if e.vrs == nil {
		return nil
	}
	id := &vrID
	vr, err := e.vrs.FindVendorRequestByID(ctx, *id)
	if err != nil {
		return err
	}
	if vr == nil {
		return jobs.Permanent(fmt.Errorf("vendor request %s not found", id))
	}
	now := e.now().UTC()
	ttl := e.cfg.Params.VendorRequestTTL
	switch vr.Status {
	case routing.VendorRequestSent:
		sent := vr.CreatedAt
		if vr.SentAt != nil {
			sent = *vr.SentAt
		}
		deadline := sent.Add(ttl)
		if now.Before(deadline) {
			// Early (re-sent later, or the TTL changed): move the deadline.
			e.scheduleTimeout(ctx, vr.ID, deadline, "deadline")
			return nil
		}
		won, err := e.vrs.ExpireOne(ctx, vr.ID, now.Add(-ttl))
		if err != nil {
			return err
		}
		if !won {
			// A reply (or another worker) got there first: re-read.
			if vr, err = e.vrs.FindVendorRequestByID(ctx, *id); err != nil || vr == nil {
				return err
			}
			if vr.Status != routing.VendorRequestExpired && vr.Status != routing.VendorRequestSendFailed {
				e.timeoutDecision(ctx, vr, autonomy.OutcomeRejected, statusReason(vr.Status), trigger, nil)
				return nil
			}
			// Expired by another worker: fall through to the (idempotent) recovery check.
		} else {
			e.timeoutDecision(ctx, vr, autonomy.OutcomeApplied, autonomy.TimeoutExpired, trigger,
				map[string]any{"deadline": deadline.Format(time.RFC3339), "ttl": ttl.String()})
			vid := vr.ID
			_ = e.events.Write(ctx, &event.Event{EventType: event.VendorRequestExpired, EntityType: "vendor_request", EntityID: &vid,
				ActorType: event.ActorSystem, Payload: map[string]any{"buyer_request_id": vr.BuyerRequestID.String(), "vendor_id": vr.VendorID.String(),
					"reference": vr.PublicReference, "ttl": ttl.String(), "detected_by": "deadline"}})
		}
	case routing.VendorRequestPending:
		if now.Sub(vr.CreatedAt) < unsentGrace {
			e.scheduleTimeout(ctx, vr.ID, vr.CreatedAt.Add(unsentGrace), "unsent")
			return nil
		}
		won, err := e.vrs.MarkUndelivered(ctx, vr.ID, "never sent: the send did not complete")
		if err != nil {
			return err
		}
		if won {
			e.timeoutDecision(ctx, vr, autonomy.OutcomeApplied, autonomy.TimeoutNeverSent, trigger, nil)
		}
	case routing.VendorRequestResponded, routing.VendorRequestCancelled:
		if trigger == "deadline" {
			e.timeoutDecision(ctx, vr, autonomy.OutcomeRejected, statusReason(vr.Status), trigger, nil)
		}
		if vr.Status == routing.VendorRequestCancelled {
			return nil
		}
		// A reply exists: the recovery check below decides whether it helps
		// the buyer (a decline does not).
		if trigger == "deadline" {
			return nil
		}
	}
	return e.RecoverRequest(ctx, vr.BuyerRequestID, "vendor_request_"+strings.ToLower(string(vr.Status))+":"+trigger, &vr.ID)
}

func statusReason(s routing.VendorRequestStatus) string {
	switch s {
	case routing.VendorRequestResponded:
		return autonomy.TimeoutAlreadyResponded
	case routing.VendorRequestCancelled:
		return autonomy.TimeoutCancelled
	}
	return "status_" + strings.ToLower(string(s))
}

func (e *Engine) timeoutDecision(ctx context.Context, vr *routing.VendorRequest, outcome, reason, trigger string, result map[string]any) {
	brid := vr.BuyerRequestID
	in := map[string]any{"trigger": trigger, "status": string(vr.Status), "reference": vr.PublicReference, "vendor_id": vr.VendorID.String()}
	if err := e.store.RecordDecision(ctx, &autonomy.Decision{WorkflowType: "vendor_request", SubjectType: "vendor_request", SubjectID: vr.ID,
		BuyerRequestID: &brid, Kind: autonomy.DecVendorTimeout, Outcome: outcome, Reason: reason, PolicyVersion: autonomy.PolicyVersion,
		Inputs: in, Result: result, Actor: "SYSTEM"}); err != nil {
		e.logger.Error("recording vendor timeout decision", "error", err)
	}
}

// ResponseNotUsable is called by the offer loop when a vendor reply turned
// out not to be usable (declined / unavailable): if no contacted vendor can
// still help, the request is recovered like a timeout.
func (e *Engine) ResponseNotUsable(ctx context.Context, buyerRequestID, vendorRequestID uuid.UUID) {
	e.enqueueRecoveryCheck(ctx, vendorRequestID, "vendor_declined")
}

// RecoverRequest re-evaluates a confirmed request whose vendors went quiet,
// were unreachable or declined. Serialised per request; idempotent: a state
// already handled (same number of contacted vendors) is not handled twice.
func (e *Engine) RecoverRequest(ctx context.Context, buyerRequestID uuid.UUID, trigger string, vrID *uuid.UUID) error {
	if e.recovery == nil {
		return nil
	}
	return e.store.WithLock(ctx, "request:"+buyerRequestID.String(), func(ctx context.Context) error {
		req, err := e.reqRepo.FindByID(ctx, buyerRequestID)
		if err != nil {
			return err
		}
		if req == nil {
			return jobs.Permanent(fmt.Errorf("buyer request %s not found", buyerRequestID))
		}
		facts, err := e.recovery.RecoveryFacts(ctx, buyerRequestID, unsentGrace)
		if err != nil {
			return err
		}
		inputs := map[string]any{"trigger": trigger, "contacted": facts.Contacted, "awaiting": facts.Awaiting, "usable_responses": facts.Usable,
			"undecided_replies": facts.Undecided, "active_offers": facts.ActiveOffers, "status": string(facts.Status)}
		if vrID != nil {
			inputs["vendor_request_id"] = vrID.String()
		}
		rec := func(outcome string, fb autonomy.Fallback, reason string, result map[string]any) {
			e.decide(ctx, req, autonomy.DecRecoverRequest, outcome, fb, reason, inputs, result, nil)
		}
		switch {
		case facts.Held:
			rec(autonomy.OutcomeDeferred, "", autonomy.RecoverHeld, nil)
			return nil
		case facts.Status != request.StatusConfirmed:
			rec(autonomy.OutcomeRejected, autonomy.PolicyRejection, autonomy.RecoverNotActive, nil)
			return nil
		case facts.HasSelection || facts.HasTransaction:
			rec(autonomy.OutcomeRejected, "", autonomy.RecoverFulfilled, nil)
			return nil
		case facts.Usable > 0 || facts.ActiveOffers > 0:
			rec(autonomy.OutcomeRejected, "", autonomy.RecoverHasOptions, nil)
			return nil
		case facts.Awaiting > 0 || facts.Undecided > 0:
			rec(autonomy.OutcomeDeferred, "", autonomy.RecoverAwaiting, nil)
			return nil
		case facts.Superseded:
			rec(autonomy.OutcomeRejected, "", autonomy.RecoverSuperseded, nil)
			return nil
		}
		ds, err := e.store.DecisionsForRequest(ctx, req.ID)
		if err != nil {
			return err
		}
		rounds := 0
		for _, d := range ds {
			if d.Kind != autonomy.DecRecoverRequest || d.Outcome != autonomy.OutcomeApplied {
				continue
			}
			if n, ok := asInt(d.Inputs["contacted"]); ok && n == facts.Contacted {
				// This exact state was already handled (a retried job, a second
				// worker, a second trigger for the same silence).
				rec(autonomy.OutcomeRejected, "", autonomy.RecoverDuplicate, map[string]any{"handled_by": d.ID.String()})
				return nil
			}
			if d.Reason == autonomy.RecoverRerouted {
				rounds++
			}
		}
		p := e.cfg.Params
		inputs["rounds"] = rounds
		switch {
		case !e.cfg.Flags.AutoRecoverStalledRequests:
			// Recovery switched off: the operator runs this step (control
			// plane); the silence is recorded, nothing is sent.
			rec(autonomy.OutcomeRejected, autonomy.PolicyRejection, autonomy.RecoverDisabled, nil)
			return nil
		case !e.cfg.Flags.AutoRouteMatchedRequests:
			// Nobody new may be contacted automatically, but the buyer is
			// not left in a silent dead end.
			return e.tellBuyerNoReply(ctx, req, facts, rec, autonomy.RecoverDisabled)
		case rounds >= p.RecoveryMaxRounds:
			return e.tellBuyerNoReply(ctx, req, facts, rec, autonomy.RecoverMaxRounds)
		case facts.Contacted >= p.MaxVendorsPerRequest:
			return e.tellBuyerNoReply(ctx, req, facts, rec, autonomy.RecoverContactLimit)
		}
		capN := p.RouteMaxVendors
		if left := p.MaxVendorsPerRequest - facts.Contacted; left < capN {
			capN = left
		}
		exclude := map[uuid.UUID]bool{}
		for _, v := range facts.ContactedVendors {
			exclude[v] = true
		}
		broadened := wasBroadened(ds)
		plan, explained, err := e.planRouting(ctx, req, exclude)
		if err != nil {
			rec(autonomy.OutcomeFailed, autonomy.SystemException, autonomy.RecoverStepFailed, map[string]any{"step": "matching", "error": err.Error()})
			return err
		}
		inputs["candidates"], inputs["excluded"], inputs["broadened_before"] = explained, plan.Excluded, broadened
		route := plan.Route
		if broadened {
			route = append(append([]matching.Candidate{}, plan.Route...), plan.NonLocal...)
		}
		if len(route) > capN {
			route = route[:capN]
		}
		if len(route) > 0 {
			vrs, err := e.routeTo(ctx, req, route, "recovery_reroute")
			if err != nil {
				rec(autonomy.OutcomeFailed, autonomy.SystemException, autonomy.RecoverStepFailed, map[string]any{"step": "routing", "error": err.Error()})
				return err
			}
			if len(vrs) > 0 {
				rec(autonomy.OutcomeApplied, "", autonomy.RecoverRerouted, map[string]any{"vendor_requests": refsOf(vrs), "round": rounds + 1})
				e.recoveryEvent(ctx, req, autonomy.RecoverRerouted, map[string]any{"vendors": len(vrs), "round": rounds + 1})
				e.say(ctx, req.ConversationID, &req.ID, "routing_update", fmt.Sprintf("The %s I asked about %s %s replied yet, so I'm checking with %d other %s now 👀",
					plural(facts.Contacted, "seller", "sellers"), describe(req), plural(facts.Contacted, "hasn't", "haven't"), len(vrs), plural(len(vrs), "seller", "sellers")))
				return nil
			}
		}
		if !broadened && len(plan.NonLocal) > 0 && e.cfg.Flags.AutoClarifyRequests && !e.broadenExcluded(req) && !broadenDeclined(ds) {
			if asked, err := e.askBroadenAfterSilence(ctx, req, facts, inputs); err != nil || asked {
				if asked {
					rec(autonomy.OutcomeApplied, autonomy.ClarificationRequired, autonomy.RecoverBroadenAsked, map[string]any{"non_local": len(plan.NonLocal)})
					e.recoveryEvent(ctx, req, autonomy.RecoverBroadenAsked, nil)
				}
				return err
			}
		}
		return e.tellBuyerNoReply(ctx, req, facts, rec, autonomy.RecoverBuyerTold)
	})
}

func (e *Engine) recoveryEvent(ctx context.Context, req *request.BuyerRequest, what string, extra map[string]any) {
	rid := req.ID
	_ = e.events.Write(ctx, &event.Event{EventType: event.RequestRecovered, EntityType: "buyer_request", EntityID: &rid, ActorType: event.ActorSystem,
		Payload: event.Merge(req.FunnelDims(), map[string]any{"outcome": what}, extra)})
}

// tellBuyerNoReply: no safe automatic next step — the buyer hears the real
// status and what they can do. The request stays open: a late reply still
// becomes an offer. Never "unavailable".
func (e *Engine) tellBuyerNoReply(ctx context.Context, req *request.BuyerRequest, facts *RecoveryFacts,
	rec func(string, autonomy.Fallback, string, map[string]any), reason string) error {
	text := fmt.Sprintf("I haven't got a confirmed option for %s yet 😔 The %s I contacted %s replied with one. I'll pass on any reply that still comes in. "+
		"If you'd like, change the budget, condition or area — or tell me something else to look for — and I'll search again.",
		describe(req), plural(facts.Contacted, "seller", "sellers"), plural(facts.Contacted, "hasn't", "haven't"))
	status := e.notify(ctx, req.ConversationID, &req.ID, KindVendorTimeoutNotice, text)
	rec(autonomy.OutcomeApplied, "", reason, map[string]any{"buyer_notice": status})
	e.recoveryEvent(ctx, req, reason, map[string]any{"buyer_notice": status})
	return nil
}

// askBroadenAfterSilence asks whether to search other areas (only if the
// conversation's question slot is free; otherwise the buyer is told).
func (e *Engine) askBroadenAfterSilence(ctx context.Context, req *request.BuyerRequest, facts *RecoveryFacts, inputs map[string]any) (bool, error) {
	q := fmt.Sprintf("The %s I asked about %s%s %s replied yet. I can check sellers in other areas too. Want me to? (Yes / No)",
		plural(facts.Contacted, "seller", "sellers"), describe(req), locSuffix(req), plural(facts.Contacted, "hasn't", "haven't"))
	conv, bid, rid := req.ConversationID, req.BuyerID, req.ID
	c, blocking, err := e.store.OpenClarificationIfFree(ctx, &autonomy.Clarification{Party: autonomy.PartyBuyer, ConversationID: &conv, BuyerID: &bid,
		BuyerRequestID: &rid, Kind: autonomy.KindBroadenScope, Question: q, Status: autonomy.ClarOpen, Attempts: 1,
		Options:   []autonomy.Option{{Key: "yes", Label: "Yes"}, {Key: "no", Label: "No"}},
		Context:   map[string]any{"after_recovery": true, "contacted": facts.Contacted},
		ExpiresAt: e.now().UTC().Add(e.cfg.Params.ClarificationTTL)})
	if err != nil {
		return false, err
	}
	if blocking != nil {
		inputs["broaden_blocked_by"] = blocking.ID.String()
		return false, nil
	}
	if err := e.requests.ClarifyAs(ctx, req.ID, q, event.ActorSystem, map[string]any{"clarification_id": c.ID.String(), "kind": autonomy.KindBroadenScope}); err != nil {
		_ = e.store.ResolveClarification(ctx, c.ID, autonomy.ClarCancelled, nil, "", map[string]any{"send_error": err.Error()}, e.now().UTC())
		return false, nil // the buyer is told instead (that notice is queued if delivery fails)
	}
	return true, nil
}

func (e *Engine) broadenExcluded(req *request.BuyerRequest) bool {
	cat := strings.ToLower(strings.TrimSpace(deref(req.Category)))
	for _, c := range e.cfg.Params.BroadenExcludedCategories {
		if c == cat && cat != "" {
			return true
		}
	}
	return false
}

func wasBroadened(ds []*autonomy.Decision) bool {
	for _, d := range ds {
		if d.Kind == autonomy.DecRouteRequest && d.Outcome == autonomy.OutcomeApplied && d.Reason == "routing_policy_broadened" {
			return true
		}
	}
	return false
}

func broadenDeclined(ds []*autonomy.Decision) bool {
	for _, d := range ds {
		if d.Kind == autonomy.DecNoMatch && d.Reason == "buyer_declined_broader_search" {
			return true
		}
	}
	return false
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}
