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
