package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/autonomy"
	"github.com/middleman/backend/internal/channel"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/jobs"
	"github.com/middleman/backend/internal/routing"
	"github.com/middleman/backend/internal/supply"
)

// ---- delivery failure handling ----------------------------------------------------
//
// Failures are classified by channel.Classify (one taxonomy, from the
// provider's structured error code). Per class:
//   - retryable (transport, rate limit, provider unavailable): bounded durable
//     retry (AUTO_RETRY_MAX_ATTEMPTS), first delay per class;
//   - vendor request not retryable / retries exhausted: this vendor cannot
//     be reached now — the buyer's request continues with alternative
//     vendors (RecoverRequest) and the operator sees only what needs a person;
//   - offers blocked by the customer-service window (or another
//     recipient-level policy): kept deliverable and delivered when the buyer
//     next writes — no free-form send outside the window, no template assumed;
//   - invalid recipient / invalid request / account-level / auth-config /
//     unknown: recorded and raised as an operator exception.

// ClassifySendError returns the failure class of a send error (kept for
// callers and tests that only need the class).
func ClassifySendError(err error) channel.FailureClass {
	if err == nil {
		return ""
	}
	return channel.Classify(err).Class
}

// VendorRequestSendFailed implements routing.SendFailureListener (the send
// was rejected synchronously; the request is SEND_FAILED).
func (e *Engine) VendorRequestSendFailed(ctx context.Context, vr *routing.VendorRequest, err error) {
	e.vendorRequestFailed(ctx, vr.ID, vr.BuyerRequestID, vr.PublicReference, channel.Classify(err), err)
}

// vendorRequestUndelivered: the provider reported an accepted vendor
// request as failed (webhook). The request stops counting as awaiting a reply.
func (e *Engine) vendorRequestUndelivered(ctx context.Context, vrID uuid.UUID, f channel.Failure, err error) {
	if e.vrs == nil {
		return
	}
	ok, merr := e.vrs.MarkUndelivered(ctx, vrID, err.Error())
	if merr != nil {
		e.logger.Error("marking vendor request undelivered", "vendor_request_id", vrID, "error", merr)
		return
	}
	if !ok {
		return // answered, expired or cancelled meanwhile: nothing to recover
	}
	vr, ferr := e.vrs.FindVendorRequestByID(ctx, vrID)
	if ferr != nil || vr == nil {
		return
	}
	e.vendorRequestFailed(ctx, vr.ID, vr.BuyerRequestID, vr.PublicReference, f, err)
}

func (e *Engine) vendorRequestFailed(ctx context.Context, vrID, brid uuid.UUID, ref string, f channel.Failure, err error) {
	if f.Retry && e.cfg.Flags.AutoRetryDelivery {
		e.scheduleRetry(ctx, JobVendorResend, "vendor_request_id", vrID, &brid, err, f.FirstDelay)
		return
	}
	e.recordDelivery(ctx, "vendor_request", vrID, &brid, autonomy.OutcomeRejected, fallbackForClass(f), string(f.Class),
		map[string]any{"error": err.Error(), "class": string(f.Class), "code": f.Code, "template_required": f.TemplateRequired, "template_configured": false})
	detail := map[string]any{"reference": ref}
	if f.Retry { // retryable, but automatic retry is switched off
		e.raise(ctx, autonomy.SystemException, autonomy.ReasonDeliveryRetryExhausted, "vendor_request", vrID, &brid,
			map[string]any{"error": err.Error(), "class": string(f.Class), "reference": ref, "why": "automatic retry is disabled (AUTO_RETRY_DELIVERY=false)"})
	} else {
		if f.Class == channel.FailPolicy {
			detail["why"] = "the channel refused the vendor message (e.g. the vendor's 24h window is closed and no approved utility template is configured)"
		}
		e.raiseForFailure(ctx, f, "vendor_request", vrID, &brid, err.Error(), detail)
	}
	// The buyer's request does not wait on a vendor Majo cannot reach.
	e.enqueueRecoveryCheck(ctx, vrID, "send_failed")
}

func (e *Engine) scheduleRetry(ctx context.Context, jobType, key string, id uuid.UUID, brid *uuid.UUID, cause error, delay time.Duration) {
	if e.jobs == nil {
		return
	}
	delay = e.firstDelay(delay)
	payload := map[string]any{key: id.String()}
	if brid != nil {
		payload["buyer_request_id"] = brid.String()
	}
	created, err := e.jobs.Enqueue(ctx, &jobs.Job{Type: jobType, IdempotencyKey: key + ":" + id.String() + ":" + uuid.NewString(),
		Payload: payload, MaxAttempts: e.cfg.Params.RetryMaxAttempts, RunAt: e.now().UTC().Add(delay)})
	if err != nil {
		e.logger.Error("scheduling delivery retry", "type", jobType, "id", id, "error", err)
		return
	}
	if created {
		f := channel.Classify(cause)
		e.recordDelivery(ctx, strings.TrimSuffix(key, "_id"), id, brid, autonomy.OutcomeDeferred, autonomy.RetryRequired, "retry_scheduled",
			map[string]any{"error": cause.Error(), "class": string(f.Class), "code": f.Code, "max_attempts": e.cfg.Params.RetryMaxAttempts, "first_delay": delay.String()})
		eid := id
		_ = e.events.Write(ctx, &event.Event{EventType: event.DeliveryRetryScheduled, EntityType: strings.TrimSuffix(key, "_id"), EntityID: &eid,
			ActorType: event.ActorSystem, Payload: event.Merge(map[string]any{"job_type": jobType, "error": cause.Error(), "class": string(f.Class)},
				idPayload("buyer_request_id", brid))})
	}
}

// firstDelay applies AUTO_RETRY_FIRST_DELAY (one first delay for every
// class, for tests and operations) over the class default.
func (e *Engine) firstDelay(classDelay time.Duration) time.Duration {
	if o := e.cfg.Params.RetryFirstDelayOverride; o > 0 {
		return o
	}
	if classDelay <= 0 {
		return retryFirstDelay
	}
	return classDelay
}

// retryFirstDelay is the default wait before the first retry (later ones
// use the durable worker's exponential backoff). Per-class delays come from
// channel.Failure.FirstDelay.
var retryFirstDelay = 5 * time.Second

func (e *Engine) handleVendorResend(ctx context.Context, j *jobs.Job) error {
	id, err := uuid.Parse(fmt.Sprint(j.Payload["vendor_request_id"]))
	if err != nil {
		return jobs.Permanent(err)
	}
	if err := e.router.ResendVendorRequest(ctx, id); err != nil {
		if !channel.Classify(err).Retry {
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
	e.offerDeliveryFailed(ctx, buyerRequestID, channel.Classify(err), err)
}

func (e *Engine) offerDeliveryFailed(ctx context.Context, buyerRequestID uuid.UUID, f channel.Failure, err error) {
	brid := buyerRequestID
	if f.Retry && e.cfg.Flags.AutoRetryDelivery {
		e.scheduleRetry(ctx, JobOfferRedelivery, "buyer_request_id", buyerRequestID, &brid, err, f.FirstDelay)
		return
	}
	result := map[string]any{"error": err.Error(), "class": string(f.Class), "code": f.Code, "template_required": f.TemplateRequired, "template_configured": false}
	if queueable(f) && e.notices != nil {
		// A compliant later route exists (the buyer's next message re-opens
		// the window; a fixed configuration; a retry): the offers stay
		// deliverable and are delivered then.
		if req, rerr := e.reqRepo.FindByID(ctx, buyerRequestID); rerr == nil && req != nil {
			n, _, qerr := e.notices.QueueNotice(ctx, &autonomy.Notice{ConversationID: req.ConversationID, BuyerRequestID: &brid,
				Kind: autonomy.NoticeKindOffers, FailureClass: string(f.Class), FailureCode: f.Code, LastError: err.Error()})
			if qerr == nil && n != nil {
				result["notice_id"] = n.ID.String()
				reason := "offers_deferred_until_buyer_writes"
				if f.Retry {
					reason = "offers_deferred_retry_disabled"
				}
				e.recordDelivery(ctx, "buyer_request", buyerRequestID, &brid, autonomy.OutcomeDeferred, fallbackForClass(f), reason, result)
				if f.Class == channel.FailAuthConfig || f.Class == channel.FailUnknown || f.Retry {
					e.raiseForOffers(ctx, f, brid, err)
				}
				return
			}
		}
	}
	e.recordDelivery(ctx, "buyer_request", buyerRequestID, &brid, autonomy.OutcomeRejected, fallbackForClass(f), string(f.Class), result)
	e.raiseForOffers(ctx, f, brid, err)
}

func (e *Engine) raiseForOffers(ctx context.Context, f channel.Failure, brid uuid.UUID, err error) {
	if f.Retry {
		e.raise(ctx, autonomy.SystemException, autonomy.ReasonDeliveryRetryExhausted, "buyer_request", brid, &brid,
			map[string]any{"error": err.Error(), "class": string(f.Class), "why": "offers could not be delivered and automatic retry is disabled or exhausted; they will go out when the buyer next writes"})
		return
	}
	e.raiseForFailure(ctx, f, "buyer_request", brid, &brid, err.Error(), map[string]any{"why": "offers could not be delivered to the buyer"})
}

// offersUndelivered: the provider reported an offer message as failed after
// accepting it. The offers it carried are returned to a deliverable state
// (the buyer never saw them) and handled like a rejected delivery.
func (e *Engine) offersUndelivered(ctx context.Context, buyerRequestID uuid.UUID, meta map[string]any, f channel.Failure, errText string) {
	var ids []uuid.UUID
	if raw, ok := meta["offer_ids"].([]any); ok {
		for _, v := range raw {
			if id := parseOptUUID(v); id != nil {
				ids = append(ids, *id)
			}
		}
	}
	if e.offerReverter != nil && len(ids) > 0 {
		if n, err := e.offerReverter.RevertUndelivered(ctx, ids, "delivery failed: "+errText); err != nil {
			e.logger.Error("reverting undelivered offers", "buyer_request_id", buyerRequestID, "error", err)
			return
		} else if n == 0 {
			return // selected / withdrawn meanwhile: nothing to redeliver
		}
	}
	e.offerDeliveryFailed(ctx, buyerRequestID, f, &channel.ProviderError{Provider: "whatsapp", Code: f.Code, Body: "delivery failed: " + errText})
}

func (e *Engine) handleOfferRedelivery(ctx context.Context, j *jobs.Job) error {
	id, err := uuid.Parse(fmt.Sprint(j.Payload["buyer_request_id"]))
	if err != nil {
		return jobs.Permanent(err)
	}
	if e.offerDeliverer == nil {
		return jobs.Permanent(errors.New("offer redelivery not wired"))
	}
	if e.held(ctx, id) {
		return nil // paused by an operator: delivered after the hold is released
	}
	if err := e.offerDeliverer(ctx, id); err != nil {
		if !channel.Classify(err).Retry {
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

// onRetryExhausted: bounded recovery failed. Vendor request: the buyer's
// request continues with alternatives (and the operator is told the vendor
// is unreachable). Offers: kept for the buyer's next message, and raised.
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
	f := channel.Classify(err)
	reason := autonomy.ReasonDeliveryRetryExhausted
	if f.Class == channel.FailPolicy {
		reason = autonomy.ReasonProviderPolicy
	}
	e.recordDelivery(ctx, subjectType, *id, brid, autonomy.OutcomeFailed, autonomy.SystemException, reason,
		map[string]any{"error": err.Error(), "attempts": j.Attempts, "class": string(f.Class)})
	eid := *id
	_ = e.events.Write(ctx, &event.Event{EventType: event.DeliveryRetryExhausted, EntityType: subjectType, EntityID: &eid, ActorType: event.ActorSystem,
		Payload: event.Merge(map[string]any{"job_type": j.Type, "attempts": j.Attempts, "error": err.Error()}, idPayload("buyer_request_id", brid))})
	e.raise(ctx, autonomy.SystemException, reason, subjectType, *id, brid, map[string]any{"error": err.Error(), "attempts": j.Attempts, "job_type": j.Type})
	switch {
	case j.Type == JobVendorResend:
		e.enqueueRecoveryCheck(ctx, *id, "send_retry_exhausted")
	case j.Type == JobOfferRedelivery && brid != nil && e.notices != nil:
		if req, rerr := e.reqRepo.FindByID(ctx, *brid); rerr == nil && req != nil {
			_, _, _ = e.notices.QueueNotice(ctx, &autonomy.Notice{ConversationID: req.ConversationID, BuyerRequestID: brid,
				Kind: autonomy.NoticeKindOffers, FailureClass: string(f.Class), FailureCode: f.Code, LastError: err.Error()})
		}
	}
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
	// 01a audit: a restricted item never goes live without an operator,
	// however trusted the vendor (the same terms buyers' requests are held for).
	var text []string
	for _, f := range []*string{l.Category, l.Product, l.Variant, l.Description} {
		if f != nil {
			text = append(text, *f)
		}
	}
	if term := RestrictedTerm(strings.Join(text, " "), e.cfg.Params.RestrictedTerms); term != "" {
		_ = e.store.RecordDecision(ctx, &autonomy.Decision{WorkflowType: "listing", SubjectType: "supply_listing", SubjectID: l.ID,
			Kind: autonomy.DecAutoPublish, Outcome: autonomy.OutcomeRejected, Fallback: autonomy.PolicyRejection,
			Reason: "restricted_item", Result: map[string]any{"term": term}, Actor: "SYSTEM"})
		return // stays a DRAFT for curation
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
