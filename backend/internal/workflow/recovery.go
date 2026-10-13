package workflow

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/autonomy"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/jobs"
	"github.com/middleman/backend/internal/routing"
	"github.com/middleman/backend/internal/supply"
)

// ---- delivery failure classification ---------------------------------------------

// SendFailure classes.
const (
	SendTransient = "transient" // network, 429, 5xx: retry with backoff
	SendPolicy    = "policy"    // the platform refuses (24h window closed, template required, blocked)
	SendPermanent = "permanent" // bad request / auth: retrying cannot help
)

var statusPattern = regexp.MustCompile(`status (\d{3})`)

// policyCodes are Meta errors meaning "not allowed now": 131047 re-engagement
// (24h window closed), 131026 undeliverable, 131050 user stopped marketing,
// 470 legacy re-engagement.
var policyCodes = []string{"131047", "131026", "131050", "(#470)", "re-engagement"}

// ClassifySendError classifies a channel send error from its text (the
// provider includes Meta's status and error body).
func ClassifySendError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	for _, c := range policyCodes {
		if strings.Contains(msg, c) {
			return SendPolicy
		}
	}
	if m := statusPattern.FindStringSubmatch(msg); m != nil {
		code, _ := strconv.Atoi(m[1])
		switch {
		case code == 429 || code >= 500:
			return SendTransient
		default:
			return SendPermanent
		}
	}
	return SendTransient // connection errors, timeouts
}

// ---- vendor request recovery -----------------------------------------------------

// VendorRequestSendFailed implements routing.SendFailureListener.
func (e *Engine) VendorRequestSendFailed(ctx context.Context, vr *routing.VendorRequest, err error) {
	brid := vr.BuyerRequestID
	class := ClassifySendError(err)
	if class == SendPolicy || class == SendPermanent || !e.cfg.Flags.AutoRetryDelivery {
		reason := autonomy.ReasonProviderPolicy
		if class != SendPolicy {
			reason = autonomy.ReasonVendorUnreachable
		}
		if !e.cfg.Flags.AutoRetryDelivery && class == SendTransient {
			reason = autonomy.ReasonDeliveryRetryExhausted
		}
		e.recordDelivery(ctx, "vendor_request", vr.ID, &brid, autonomy.OutcomeRejected, fallbackFor(class), class, map[string]any{"error": err.Error()})
		e.raise(ctx, autonomy.SystemException, reason, "vendor_request", vr.ID, &brid, map[string]any{"error": err.Error(), "class": class,
			"reference": vr.PublicReference, "why": "the channel refused the vendor message; Majo cannot reach this vendor on its own"})
		return
	}
	e.scheduleRetry(ctx, JobVendorResend, "vendor_request_id", vr.ID, &brid, err)
}

func fallbackFor(class string) autonomy.Fallback {
	if class == SendPolicy {
		return autonomy.PolicyRejection
	}
	if class == SendTransient {
		return autonomy.TemporaryProviderFailure
	}
	return autonomy.SystemException
}

func (e *Engine) scheduleRetry(ctx context.Context, jobType, key string, id uuid.UUID, brid *uuid.UUID, cause error) {
	if e.jobs == nil {
		return
	}
	payload := map[string]any{key: id.String()}
	if brid != nil {
		payload["buyer_request_id"] = brid.String()
	}
	created, err := e.jobs.Enqueue(ctx, &jobs.Job{Type: jobType, IdempotencyKey: key + ":" + id.String() + ":" + uuid.NewString(),
		Payload: payload, MaxAttempts: e.cfg.Params.RetryMaxAttempts, RunAt: e.now().UTC().Add(retryFirstDelay)})
	if err != nil {
		e.logger.Error("scheduling delivery retry", "type", jobType, "id", id, "error", err)
		return
	}
	if created {
		e.recordDelivery(ctx, strings.TrimSuffix(key, "_id"), id, brid, autonomy.OutcomeDeferred, autonomy.RetryRequired, "retry_scheduled",
			map[string]any{"error": cause.Error(), "max_attempts": e.cfg.Params.RetryMaxAttempts})
		eid := id
		_ = e.events.Write(ctx, &event.Event{EventType: event.DeliveryRetryScheduled, EntityType: strings.TrimSuffix(key, "_id"), EntityID: &eid,
			ActorType: event.ActorSystem, Payload: event.Merge(map[string]any{"job_type": jobType, "error": cause.Error()}, idPayload("buyer_request_id", brid))})
	}
}

// retryFirstDelay is the wait before the first retry (later ones use the
// durable worker's exponential backoff).
var retryFirstDelay = 5 * time.Second

func (e *Engine) handleVendorResend(ctx context.Context, j *jobs.Job) error {
	id, err := uuid.Parse(fmt.Sprint(j.Payload["vendor_request_id"]))
	if err != nil {
		return jobs.Permanent(err)
	}
	if err := e.router.ResendVendorRequest(ctx, id); err != nil {
		if ClassifySendError(err) != SendTransient {
			return jobs.Permanent(err)
		}
		return err
	}
	e.recovered(ctx, j, "vendor_request", id)
	return nil
}

// OfferDeliveryFailed is called by the offer loop when a delivery to the
// buyer was rejected (offers are back in a deliverable state).
func (e *Engine) OfferDeliveryFailed(ctx context.Context, buyerRequestID uuid.UUID, err error) {
	brid := buyerRequestID
	class := ClassifySendError(err)
	if class != SendTransient || !e.cfg.Flags.AutoRetryDelivery {
		reason := autonomy.ReasonProviderPolicy
		if class == SendPermanent {
			reason = autonomy.ReasonWorkflowFailure
		} else if class == SendTransient {
			reason = autonomy.ReasonDeliveryRetryExhausted
		}
		e.recordDelivery(ctx, "buyer_request", buyerRequestID, &brid, autonomy.OutcomeRejected, fallbackFor(class), class, map[string]any{"error": err.Error()})
		e.raise(ctx, autonomy.SystemException, reason, "buyer_request", buyerRequestID, &brid, map[string]any{"error": err.Error(), "class": class,
			"why": "offers could not be delivered to the buyer"})
		return
	}
	e.scheduleRetry(ctx, JobOfferRedelivery, "buyer_request_id", buyerRequestID, &brid, err)
}

func (e *Engine) handleOfferRedelivery(ctx context.Context, j *jobs.Job) error {
	id, err := uuid.Parse(fmt.Sprint(j.Payload["buyer_request_id"]))
	if err != nil {
		return jobs.Permanent(err)
	}
	if e.offerDeliverer == nil {
		return jobs.Permanent(errors.New("offer redelivery not wired"))
	}
	if err := e.offerDeliverer(ctx, id); err != nil {
		if ClassifySendError(err) != SendTransient {
			return jobs.Permanent(err)
		}
		return err
	}
	e.recovered(ctx, j, "buyer_request", id)
	return nil
}

func (e *Engine) recovered(ctx context.Context, j *jobs.Job, subjectType string, id uuid.UUID) {
	brid := parseOptUUID(j.Payload["buyer_request_id"])
	e.recordDelivery(ctx, subjectType, id, brid, autonomy.OutcomeApplied, "", "recovered", map[string]any{"attempts": j.Attempts, "job_type": j.Type})
	eid := id
	_ = e.events.Write(ctx, &event.Event{EventType: event.DeliveryRecovered, EntityType: subjectType, EntityID: &eid, ActorType: event.ActorSystem,
		Payload: event.Merge(map[string]any{"job_type": j.Type, "attempts": j.Attempts}, idPayload("buyer_request_id", brid))})
}

// onRetryExhausted: recovery failed for good -> operator exception.
func (e *Engine) onRetryExhausted(ctx context.Context, j *jobs.Job, err error) {
	subjectType, key := "buyer_request", "buyer_request_id"
	if j.Type == JobVendorResend {
		subjectType, key = "vendor_request", "vendor_request_id"
	}
	id := parseOptUUID(j.Payload[key])
	if id == nil {
		return
	}
	brid := parseOptUUID(j.Payload["buyer_request_id"])
	class := ClassifySendError(err)
	reason := autonomy.ReasonDeliveryRetryExhausted
	if class == SendPolicy {
		reason = autonomy.ReasonProviderPolicy
	}
	e.recordDelivery(ctx, subjectType, *id, brid, autonomy.OutcomeFailed, autonomy.SystemException, reason, map[string]any{"error": err.Error(), "attempts": j.Attempts})
	eid := *id
	_ = e.events.Write(ctx, &event.Event{EventType: event.DeliveryRetryExhausted, EntityType: subjectType, EntityID: &eid, ActorType: event.ActorSystem,
		Payload: event.Merge(map[string]any{"job_type": j.Type, "attempts": j.Attempts, "error": err.Error()}, idPayload("buyer_request_id", brid))})
	e.raise(ctx, autonomy.SystemException, reason, subjectType, *id, brid, map[string]any{"error": err.Error(), "attempts": j.Attempts, "job_type": j.Type})
}

func (e *Engine) recordDelivery(ctx context.Context, subjectType string, id uuid.UUID, brid *uuid.UUID, outcome string, fb autonomy.Fallback, reason string, result map[string]any) {
	if err := e.store.RecordDecision(ctx, &autonomy.Decision{WorkflowType: "delivery", SubjectType: subjectType, SubjectID: id, BuyerRequestID: brid,
		Kind: autonomy.DecRetryDelivery, Outcome: outcome, Fallback: fb, Reason: reason, Result: result, Actor: "SYSTEM"}); err != nil {
		e.logger.Error("recording delivery decision", "error", err)
	}
}

func parseOptUUID(v any) *uuid.UUID {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return nil
	}
	return &id
}

// ---- trusted-vendor auto-publish ------------------------------------------------

// Trust thresholds (Architecture Lock gate: ≥ 95% of a vendor's last 20
// drafts approved unchanged).
const (
	TrustWindow   = 20
	TrustMinRatio = 0.95
)

// VendorTrust is a vendor's curation record (operator decisions only).
type VendorTrust struct {
	Decided           int `json:"decided"`            // last ≤20 drafts an operator published or rejected
	ApprovedUnchanged int `json:"approved_unchanged"` // published with no operator edit
}

// Trusted reports whether the lock's evidence gate holds.
func (t VendorTrust) Trusted() bool {
	return t.Decided >= TrustWindow && float64(t.ApprovedUnchanged) >= TrustMinRatio*float64(t.Decided)
}

// ListingPublisher is the curation surface auto-publish needs.
type ListingPublisher interface {
	PublishAs(ctx context.Context, listingID uuid.UUID, expected *time.Time, operatorID *uuid.UUID, actor event.ActorType) (*supply.Listing, error)
	VendorTrust(ctx context.Context, vendorID uuid.UUID) (VendorTrust, error)
}

// SetListingPublisher wires trusted-vendor auto-publishing.
func (e *Engine) SetListingPublisher(p ListingPublisher) { e.publisher = p }

// DraftReady implements supply.DraftListener.
func (e *Engine) DraftReady(ctx context.Context, l *supply.Listing) {
	if !e.cfg.Flags.AutoPublishTrustedVendorLists || e.publisher == nil || e.jobs == nil {
		return
	}
	if _, err := e.jobs.Enqueue(ctx, &jobs.Job{Type: JobListingPublish, IdempotencyKey: "publish:" + l.ID.String(),
		Payload: map[string]any{"supply_listing_id": l.ID.String(), "vendor_id": l.VendorID.String()}, MaxAttempts: 3, RunAt: e.now().UTC()}); err != nil {
		e.logger.Error("enqueueing auto-publish", "listing_id", l.ID, "error", err)
	}
}

func (e *Engine) handleListingPublish(ctx context.Context, j *jobs.Job) error {
	lid, vid := parseOptUUID(j.Payload["supply_listing_id"]), parseOptUUID(j.Payload["vendor_id"])
	if lid == nil || vid == nil {
		return jobs.Permanent(errors.New("malformed auto-publish payload"))
	}
	record := func(outcome string, fb autonomy.Fallback, reason string, res map[string]any) {
		_ = e.store.RecordDecision(ctx, &autonomy.Decision{WorkflowType: "listing", SubjectType: "supply_listing", SubjectID: *lid,
			Kind: autonomy.DecAutoPublish, Outcome: outcome, Fallback: fb, Reason: reason, Result: res, Actor: "SYSTEM"})
	}
	trust, err := e.publisher.VendorTrust(ctx, *vid)
	if err != nil {
		return err
	}
	tm := map[string]any{"decided": trust.Decided, "approved_unchanged": trust.ApprovedUnchanged, "window": TrustWindow, "min_ratio": TrustMinRatio}
	if !trust.Trusted() {
		record(autonomy.OutcomeRejected, autonomy.PolicyRejection, "vendor_not_trusted_yet", tm)
		return nil // stays a DRAFT for curation
	}
	if _, err := e.publisher.PublishAs(ctx, *lid, nil, nil, event.ActorSystem); err != nil {
		var np supply.ErrNotPublishable
		if errors.As(err, &np) {
			tm["reasons"] = np.Reasons
			record(autonomy.OutcomeRejected, autonomy.PolicyRejection, "not_publishable", tm)
			return nil
		}
		return err
	}
	record(autonomy.OutcomeApplied, "", "trusted_vendor", tm)
	return nil
}
