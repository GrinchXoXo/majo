package workflow

// Operator holds (Phase 4C hardening): the control plane can pause one
// request's automation without touching the rest of Majo. While held, Majo
// takes no automatic step for the request — no routing or re-routing, no
// question, no automatic offer delivery, no queued notice — but the
// buyer's messages are still recorded and operator overrides still work
// (they call the same application services). Resuming hands the request
// back: the next step runs, vendor deadlines that passed are re-checked and
// offers that became deliverable are delivered.

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
	"github.com/middleman/backend/internal/request"
)

// ErrHoldReasonRequired: pausing and resuming are audited decisions.
var ErrHoldReasonRequired = errors.New("a reason is required")

// held reports whether the request is paused (errors count as not held:
// the hold store is the control plane, never a reason to stall commerce).
func (e *Engine) held(ctx context.Context, buyerRequestID uuid.UUID) bool {
	if e.globallyPaused(ctx) {
		return true // pilot batch item 4: a global pause holds every request
	}
	if e.holds == nil {
		return false
	}
	h, err := e.holds.ActiveHold(ctx, buyerRequestID)
	if err != nil {
		e.logger.Error("reading workflow hold", "buyer_request_id", buyerRequestID, "error", err)
		return false
	}
	return h != nil
}

// Held implements marketplace's automatic-delivery guard.
func (e *Engine) Held(ctx context.Context, buyerRequestID uuid.UUID) bool {
	return e.held(ctx, buyerRequestID)
}

// Pause places an operator hold on one request.
func (e *Engine) Pause(ctx context.Context, buyerRequestID uuid.UUID, by, reason string) (*autonomy.Hold, bool, error) {
	if e.holds == nil {
		return nil, false, errors.New("holds are not configured")
	}
	if strings.TrimSpace(reason) == "" {
		return nil, false, ErrHoldReasonRequired
	}
	req, err := e.reqRepo.FindByID(ctx, buyerRequestID)
	if err != nil {
		return nil, false, err
	}
	if req == nil {
		return nil, false, fmt.Errorf("buyer request %s not found", buyerRequestID)
	}
	h, created, err := e.holds.PlaceHold(ctx, buyerRequestID, by, reason)
	if err != nil {
		return nil, false, err
	}
	if created {
		e.holdDecision(ctx, req, "paused", by, reason)
		rid := req.ID
		_ = e.events.Write(ctx, &event.Event{EventType: event.WorkflowHeld, EntityType: "buyer_request", EntityID: &rid, ActorType: event.ActorOperator,
			Payload: event.Merge(req.FunnelDims(), map[string]any{"reason": reason, "by": by})})
	}
	return h, created, nil
}

// Resume releases the hold and hands the request back to Majo.
func (e *Engine) Resume(ctx context.Context, buyerRequestID uuid.UUID, by, reason string) (bool, error) {
	if e.holds == nil {
		return false, errors.New("holds are not configured")
	}
	if strings.TrimSpace(reason) == "" {
		return false, ErrHoldReasonRequired
	}
	released, err := e.holds.ReleaseHold(ctx, buyerRequestID, by, reason)
	if err != nil || !released {
		return released, err
	}
	req, err := e.reqRepo.FindByID(ctx, buyerRequestID)
	if err != nil || req == nil {
		return true, err
	}
	e.holdDecision(ctx, req, "resumed", by, reason)
	rid := req.ID
	_ = e.events.Write(ctx, &event.Event{EventType: event.WorkflowResumed, EntityType: "buyer_request", EntityID: &rid, ActorType: event.ActorOperator,
		Payload: event.Merge(req.FunnelDims(), map[string]any{"reason": reason, "by": by})})
	e.enqueueAdvance(ctx, req.ID, "resumed")
	if req.Status == request.StatusConfirmed {
		// Deadlines that passed while held, replies that were declines, and
		// offers that became deliverable are picked up again.
		e.enqueueRequestRecovery(ctx, req.ID, "resumed")
		if e.offerDeliverer != nil {
			if err := e.offerDeliverer(ctx, req.ID); err != nil && !errors.Is(err, request.ErrNotConfirmed) {
				e.logger.Error("delivering offers after resume", "buyer_request_id", req.ID, "error", err)
			}
		}
	}
	if e.notices != nil {
		e.BuyerWrote(ctx, req.ConversationID, uuid.New()) // flush notices kept while held (window permitting)
	}
	return true, nil
}

func (e *Engine) holdDecision(ctx context.Context, req *request.BuyerRequest, what, by, reason string) {
	rid := req.ID
	if err := e.store.RecordDecision(ctx, &autonomy.Decision{WorkflowType: "buyer_request", SubjectType: "buyer_request", SubjectID: rid,
		BuyerRequestID: &rid, Kind: autonomy.DecHold, Outcome: autonomy.OutcomeApplied, Reason: what, PolicyVersion: autonomy.PolicyVersion,
		Inputs: map[string]any{"by": by, "reason": reason}, Actor: "OPERATOR"}); err != nil {
		e.logger.Error("recording hold decision", "error", err)
	}
}

// enqueueRequestRecovery runs the recovery check for a request directly
// (not tied to one vendor request).
func (e *Engine) enqueueRequestRecovery(ctx context.Context, buyerRequestID uuid.UUID, trigger string) {
	if e.jobs == nil {
		return
	}
	if _, err := e.jobs.Enqueue(ctx, &jobs.Job{Type: JobRequestRecovery, IdempotencyKey: "recover:" + buyerRequestID.String() + ":" + uuid.NewString(),
		Payload: map[string]any{"buyer_request_id": buyerRequestID.String(), "trigger": trigger}, MaxAttempts: e.cfg.JobMaxAttempts, RunAt: e.now().UTC()}); err != nil {
		e.logger.Error("enqueueing request recovery", "buyer_request_id", buyerRequestID, "error", err)
	}
}

// JobRequestRecovery re-evaluates one request (resume, operator).
const JobRequestRecovery = "REQUEST_RECOVERY"

func (e *Engine) handleRequestRecovery(ctx context.Context, j *jobs.Job) error {
	id := parseOptUUID(j.Payload["buyer_request_id"])
	if id == nil {
		return jobs.Permanent(errors.New("malformed recovery payload"))
	}
	return e.RecoverRequest(ctx, *id, fmt.Sprint(j.Payload["trigger"]), nil)
}

// onRecoveryExhausted: re-evaluating a request kept failing (matching or
// routing errors): the control plane is told; nothing is sent twice.
func (e *Engine) onRecoveryExhausted(ctx context.Context, j *jobs.Job, err error) {
	var brid *uuid.UUID
	if id := parseOptUUID(j.Payload["buyer_request_id"]); id != nil {
		brid = id
	} else if vrID := parseOptUUID(j.Payload["vendor_request_id"]); vrID != nil && e.vrs != nil {
		if vr, ferr := e.vrs.FindVendorRequestByID(ctx, *vrID); ferr == nil && vr != nil {
			b := vr.BuyerRequestID
			brid = &b
		}
	}
	if brid == nil {
		return
	}
	e.raise(ctx, autonomy.SystemException, autonomy.ReasonRecoveryFailed, "buyer_request", *brid, brid,
		map[string]any{"error": err.Error(), "attempts": j.Attempts, "job_type": j.Type,
			"why": "Majo could not complete the next step after the vendors went quiet; nothing was sent twice"})
}

// ---- global automation pause (pilot batch item 4) ------------------------------
//
// One operator switch for an incident ("Majo is sending the wrong thing,
// stop everything"): while paused every request is treated as held (see the
// top of this file for what that stops), and Majo asks vendors nothing.
// Inbound messages are still recorded and operator overrides still work.
// Resuming re-runs the next step of everything left mid-flow.

// globallyPaused: errors count as not paused, like holds (the control plane
// must never be the reason commerce stalls); they are logged.
func (e *Engine) globallyPaused(ctx context.Context) bool {
	if e.pauses == nil {
		return false
	}
	p, err := e.pauses.ActivePause(ctx)
	if err != nil {
		e.logger.Error("reading automation pause", "error", err)
		return false
	}
	return p != nil
}

// AutomationPaused reports whether all automation is paused (vendor runtime hook).
func (e *Engine) AutomationPaused(ctx context.Context) bool { return e.globallyPaused(ctx) }

// PauseAll pauses all automation. Idempotent: pausing twice keeps the first pause.
func (e *Engine) PauseAll(ctx context.Context, by, reason string) (*autonomy.AutomationPause, bool, error) {
	if e.pauses == nil {
		return nil, false, errors.New("the global pause is not configured")
	}
	if strings.TrimSpace(reason) == "" {
		return nil, false, ErrHoldReasonRequired
	}
	p, created, err := e.pauses.PlacePause(ctx, by, reason)
	if err != nil {
		return nil, false, err
	}
	if created {
		e.pauseDecision(ctx, p.ID, "paused_all", by, reason)
		pid := p.ID
		_ = e.events.Write(ctx, &event.Event{EventType: event.AutomationPaused, EntityType: "automation_pause", EntityID: &pid,
			ActorType: event.ActorOperator, Payload: map[string]any{"reason": reason, "by": by}})
		e.logger.Warn("ALL AUTOMATION PAUSED by operator", "by", by, "reason", reason)
	}
	return p, created, nil
}

// ResumeAll releases the global pause and re-runs what was left mid-flow:
// the next step of every open request, vendor deadlines that passed, and
// offers that became deliverable. Requests with their own hold stay held.
func (e *Engine) ResumeAll(ctx context.Context, by, reason string) (bool, error) {
	if e.pauses == nil {
		return false, errors.New("the global pause is not configured")
	}
	if strings.TrimSpace(reason) == "" {
		return false, ErrHoldReasonRequired
	}
	active, err := e.pauses.ActivePause(ctx)
	if err != nil {
		return false, err
	}
	released, err := e.pauses.ReleasePause(ctx, by, reason)
	if err != nil || !released {
		return released, err
	}
	var pid uuid.UUID
	if active != nil {
		pid = active.ID
	}
	e.pauseDecision(ctx, pid, "resumed_all", by, reason)
	_ = e.events.Write(ctx, &event.Event{EventType: event.AutomationResumed, EntityType: "automation_pause", EntityID: &pid,
		ActorType: event.ActorOperator, Payload: map[string]any{"reason": reason, "by": by}})
	e.logger.Warn("automation resumed by operator", "by", by, "reason", reason)

	e.Recover(ctx) // next step of requests waiting to be confirmed or routed
	ids, err := e.pauses.OpenConfirmedRequests(ctx, e.now().Add(-7*24*time.Hour))
	if err != nil {
		e.logger.Error("listing open requests after resume", "error", err)
		return true, nil
	}
	for _, id := range ids {
		if e.held(ctx, id) {
			continue // its own operator hold still applies
		}
		e.enqueueRequestRecovery(ctx, id, "resumed_all")
		if e.offerDeliverer != nil {
			if err := e.offerDeliverer(ctx, id); err != nil && !errors.Is(err, request.ErrNotConfirmed) {
				e.logger.Error("delivering offers after resume", "buyer_request_id", id, "error", err)
			}
		}
	}
	return true, nil
}

func (e *Engine) pauseDecision(ctx context.Context, pauseID uuid.UUID, what, by, reason string) {
	if err := e.store.RecordDecision(ctx, &autonomy.Decision{WorkflowType: "control", SubjectType: "automation_pause", SubjectID: pauseID,
		Kind: autonomy.DecHold, Outcome: autonomy.OutcomeApplied, Reason: what, PolicyVersion: autonomy.PolicyVersion,
		Inputs: map[string]any{"by": by, "reason": reason, "scope": "global"}, Actor: "OPERATOR"}); err != nil {
		e.logger.Error("recording pause decision", "error", err)
	}
}
