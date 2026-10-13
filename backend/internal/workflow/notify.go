package workflow

// Buyer notices and delivery-failure handling (Phase 4C hardening).
//
// Every buyer-facing workflow message goes through notify. A failed send is
// classified (channel.Classify) and handled per class — never retried
// unboundedly, never sent as free-form text outside the customer-service
// window, and never allowed to change the commerce state:
//
//   - retryable (transport / rate limit / provider unavailable): the notice
//     is kept PENDING and resent by a bounded durable job; if the retries
//     run out it stays PENDING for the buyer's next message;
//   - window closed (Meta 131047) and other recipient-level policy refusals:
//     kept PENDING and delivered when the buyer next writes (that re-opens
//     the window). No template is assumed: Majo has no approved, configured
//     buyer-update template, and the decision log says so;
//   - invalid recipient / invalid request / account-level restriction /
//     auth-or-config / unknown: recorded, and an operator exception is
//     raised (there is no compliant autonomous action).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/autonomy"
	"github.com/middleman/backend/internal/channel"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/jobs"
)

// Durable job types for notices.
const (
	JobNoticeResend = "BUYER_NOTICE_RESEND" // bounded retry of a transiently failed notice
	JobNoticeFlush  = "BUYER_NOTICE_FLUSH"  // the buyer wrote: deliver what was waiting for them
)

// channelSubject identifies the WhatsApp channel as an exception subject
// (account-level failures raise ONE open exception, not one per message).
var channelSubject = uuid.NewSHA1(uuid.NameSpaceURL, []byte("majo:channel:whatsapp"))

// requeueKinds are the buyer-facing workflow messages worth delivering late
// (an acknowledgement or a reply to a message is not).
var requeueKinds = map[string]bool{"routing_update": true, "no_match": true, KindVendorTimeoutNotice: true,
	"clarification_exhausted": true, "context_answer": true}

// staleableKinds are status updates that progress can make wrong.
var staleableKinds = map[string]bool{"routing_update": true, "no_match": true, KindVendorTimeoutNotice: true}

// KindVendorTimeoutNotice is the outbound kind of the "no reply yet" update.
const KindVendorTimeoutNotice = "vendor_timeout"

// notify sends a buyer-facing workflow message. It returns "sent", "queued",
// "failed" or "suppressed" (the request is on hold).
func (e *Engine) notify(ctx context.Context, conversationID uuid.UUID, requestID *uuid.UUID, kind, text string) string {
	if requestID != nil && e.held(ctx, *requestID) {
		e.recordNotice(ctx, conversationID, requestID, autonomy.OutcomeDeferred, "", "held_message_suppressed", map[string]any{"kind": kind})
		return "suppressed"
	}
	conv, err := e.convs.FindByID(ctx, conversationID)
	if err != nil || conv == nil {
		e.logger.Error("loading conversation for workflow message", "conversation_id", conversationID, "error", err)
		return "failed"
	}
	et, eid := "conversation", &conversationID
	if requestID != nil {
		et, eid = "buyer_request", requestID
	}
	sendCtx := conversation.WithOutboundPurpose(ctx, conversation.OutboundPurpose{Kind: kind, EntityType: et, EntityID: eid})
	if err := e.messenger.SendText(sendCtx, conv.ExternalID, text); err != nil {
		e.logger.Error("sending workflow message", "kind", kind, "error", err)
		return e.noticeFailed(ctx, conversationID, requestID, kind, text, channel.Classify(err), err.Error(), nil)
	}
	return "sent"
}

// noticeFailed applies the per-class handling to an undelivered notice.
func (e *Engine) noticeFailed(ctx context.Context, conversationID uuid.UUID, requestID *uuid.UUID, kind, text string,
	f channel.Failure, errText string, sourceMessageID *uuid.UUID) string {
	result := map[string]any{"kind": kind, "class": string(f.Class), "code": f.Code, "retry": f.Retry,
		"template_required": f.TemplateRequired, "template_configured": false, "error": errText}
	if !queueable(f) || e.notices == nil {
		e.recordNotice(ctx, conversationID, requestID, autonomy.OutcomeFailed, autonomy.SystemException, "notice_undeliverable", result)
		e.raiseForFailure(ctx, f, "conversation", conversationID, requestID, errText, map[string]any{"kind": kind})
		return "failed"
	}
	n, created, err := e.notices.QueueNotice(ctx, &autonomy.Notice{ConversationID: conversationID, BuyerRequestID: requestID, Kind: kind,
		Body: text, FailureClass: string(f.Class), FailureCode: f.Code, LastError: errText, SourceMessageID: sourceMessageID})
	if err != nil || n == nil {
		e.logger.Error("queueing buyer notice", "error", err)
		e.recordNotice(ctx, conversationID, requestID, autonomy.OutcomeFailed, autonomy.SystemException, "notice_queue_failed", result)
		return "failed"
	}
	result["notice_id"] = n.ID.String()
	if !created {
		e.recordNotice(ctx, conversationID, requestID, autonomy.OutcomeRejected, "", "duplicate_notice_prevented", result)
		return "queued"
	}
	reason := "queued_until_buyer_writes"
	fb := autonomy.PolicyRejection
	if f.Retry {
		reason, fb = "queued_for_retry", autonomy.TemporaryProviderFailure
		e.enqueueNoticeResend(ctx, n, e.firstDelay(f.FirstDelay))
	}
	e.recordNotice(ctx, conversationID, requestID, autonomy.OutcomeDeferred, fb, reason, result)
	nid := n.ID
	_ = e.events.Write(ctx, &event.Event{EventType: event.BuyerNoticeQueued, EntityType: "pending_notice", EntityID: &nid, ActorType: event.ActorSystem,
		Payload: event.Merge(map[string]any{"kind": kind, "class": string(f.Class), "code": f.Code, "conversation_id": conversationID.String()},
			idPayload("buyer_request_id", requestID))})
	if f.Class == channel.FailAuthConfig || f.Class == channel.FailUnknown {
		e.raiseForFailure(ctx, f, "conversation", conversationID, requestID, errText, map[string]any{"kind": kind, "notice_id": n.ID.String()})
	}
	return "queued"
}

func (e *Engine) recordNotice(ctx context.Context, conversationID uuid.UUID, requestID *uuid.UUID, outcome string, fb autonomy.Fallback, reason string, result map[string]any) {
	if err := e.store.RecordDecision(ctx, &autonomy.Decision{WorkflowType: "delivery", SubjectType: "conversation", SubjectID: conversationID,
		BuyerRequestID: requestID, Kind: autonomy.DecNotifyBuyer, Outcome: outcome, Fallback: fb, Reason: reason, PolicyVersion: autonomy.PolicyVersion,
		Result: result, Actor: "SYSTEM"}); err != nil {
		e.logger.Error("recording notice decision", "error", err)
	}
}

// raiseForFailure raises the operator exception a failure class needs.
// Account-level classes raise one exception on the channel (deduplicated
// while open), not one per message.
func (e *Engine) raiseForFailure(ctx context.Context, f channel.Failure, subjectType string, subjectID uuid.UUID, brid *uuid.UUID, errText string, detail map[string]any) {
	d := map[string]any{"class": string(f.Class), "code": f.Code, "error": errText}
	for k, v := range detail {
		d[k] = v
	}
	switch {
	case f.Class == channel.FailAuthConfig:
		d["why"] = "the WhatsApp account rejected the send for an authentication, permission, registration, billing or template-configuration reason"
		e.raise(ctx, autonomy.SystemException, autonomy.ReasonProviderAuthConfig, "channel", channelSubject, nil, d)
	case f.AccountLevel:
		d["why"] = "the WhatsApp account is restricted by an integrity or quality enforcement"
		e.raise(ctx, autonomy.SystemException, autonomy.ReasonProviderAccount, "channel", channelSubject, nil, d)
	case f.Class == channel.FailInvalidRecipient:
		d["why"] = "the stored number cannot receive WhatsApp messages; the contact details must be corrected"
		e.raise(ctx, autonomy.SystemException, autonomy.ReasonInvalidRecipient, subjectType, subjectID, brid, d)
	case f.Class == channel.FailInvalidRequest:
		d["why"] = "the provider rejected a message Majo built as malformed (a defect)"
		e.raise(ctx, autonomy.SystemException, autonomy.ReasonInvalidOutbound, subjectType, subjectID, brid, d)
	case f.Class == channel.FailUnknown:
		d["why"] = "the provider returned an error Majo cannot classify safely; it was not retried"
		e.raise(ctx, autonomy.SystemException, autonomy.ReasonUnclassifiedProvider, subjectType, subjectID, brid, d)
	case f.Class == channel.FailPolicy:
		e.raise(ctx, autonomy.SystemException, autonomy.ReasonProviderPolicy, subjectType, subjectID, brid, d)
	}
}

func (e *Engine) enqueueNoticeResend(ctx context.Context, n *autonomy.Notice, delay time.Duration) {
	if e.jobs == nil {
		return
	}
	if _, err := e.jobs.Enqueue(ctx, &jobs.Job{Type: JobNoticeResend, IdempotencyKey: "notice-resend:" + n.ID.String(),
		Payload: map[string]any{"notice_id": n.ID.String()}, MaxAttempts: e.cfg.Params.RetryMaxAttempts, RunAt: e.now().UTC().Add(delay)}); err != nil {
		e.logger.Error("enqueueing notice resend", "notice_id", n.ID, "error", err)
	}
}

// errNoticeRetry marks a retryable resend failure (the job backs off).
var errNoticeRetry = errors.New("notice resend failed")

func (e *Engine) handleNoticeResend(ctx context.Context, j *jobs.Job) error {
	id := parseOptUUID(j.Payload["notice_id"])
	if id == nil || e.notices == nil {
		return jobs.Permanent(errors.New("malformed notice payload"))
	}
	var out error
	err := e.store.WithLock(ctx, "notices:"+id.String(), func(ctx context.Context) error {
		n, err := e.notices.FindNotice(ctx, *id)
		if err != nil || n == nil || n.Status != autonomy.NoticePending {
			return err
		}
		f, sent := e.deliverNotice(ctx, n)
		if sent || !f.Retry {
			return nil // delivered, or the class waits for the buyer's next message
		}
		out = fmt.Errorf("%w: %s", errNoticeRetry, f.Class)
		return nil
	})
	if err != nil {
		return err
	}
	return out
}

// onNoticeRetryExhausted: bounded retries ran out. The notice stays
// PENDING (delivered when the buyer next writes): no further sends.
func (e *Engine) onNoticeRetryExhausted(ctx context.Context, j *jobs.Job, err error) {
	id := parseOptUUID(j.Payload["notice_id"])
	if id == nil || e.notices == nil {
		return
	}
	n, ferr := e.notices.FindNotice(ctx, *id)
	if ferr != nil || n == nil {
		return
	}
	e.recordNotice(ctx, n.ConversationID, n.BuyerRequestID, autonomy.OutcomeDeferred, autonomy.TemporaryProviderFailure,
		"retry_exhausted_waiting_for_buyer", map[string]any{"notice_id": n.ID.String(), "attempts": j.Attempts, "error": err.Error()})
}

// deliverNotice sends one PENDING notice. It never sends a notice for a
// held request or one older than AUTO_PENDING_NOTICE_TTL (cancelled).
func (e *Engine) deliverNotice(ctx context.Context, n *autonomy.Notice) (channel.Failure, bool) {
	if e.now().Sub(n.CreatedAt) > e.cfg.Params.PendingNoticeTTL {
		if ok, _ := e.notices.MarkNotice(ctx, n.ID, autonomy.NoticeCancelled, "expired before a compliant route existed"); ok {
			e.recordNotice(ctx, n.ConversationID, n.BuyerRequestID, autonomy.OutcomeRejected, autonomy.PolicyRejection, "notice_expired",
				map[string]any{"notice_id": n.ID.String(), "kind": n.Kind})
		}
		return channel.Failure{}, false
	}
	if n.BuyerRequestID != nil && e.held(ctx, *n.BuyerRequestID) {
		return channel.Failure{}, false // stays PENDING; delivered after the hold is released
	}
	if staleableKinds[n.Kind] && n.BuyerRequestID != nil && e.recovery != nil {
		// A status update overtaken by progress (an option arrived, or the
		// buyer chose one) would mislead: it is cancelled, not sent late.
		if f, err := e.recovery.RecoveryFacts(ctx, *n.BuyerRequestID, unsentGrace); err == nil && f != nil &&
			(f.HasSelection || f.HasTransaction || f.ActiveOffers > 0 || f.Usable > 0) {
			if ok, _ := e.notices.MarkNotice(ctx, n.ID, autonomy.NoticeCancelled, "overtaken by progress"); ok {
				e.recordNotice(ctx, n.ConversationID, n.BuyerRequestID, autonomy.OutcomeRejected, "", "notice_overtaken_by_progress",
					map[string]any{"notice_id": n.ID.String(), "kind": n.Kind})
			}
			return channel.Failure{}, false
		}
	}
	var err error
	if n.Kind == autonomy.NoticeKindOffers {
		if n.BuyerRequestID == nil || e.offerDeliverer == nil {
			_, _ = e.notices.MarkNotice(ctx, n.ID, autonomy.NoticeFailed, "offer redelivery not wired")
			return channel.Failure{}, false
		}
		err = e.offerDeliverer(ctx, *n.BuyerRequestID)
	} else {
		conv, cerr := e.convs.FindByID(ctx, n.ConversationID)
		if cerr != nil || conv == nil {
			return channel.Failure{Class: channel.FailTransient, Retry: true}, false
		}
		body := n.Body
		if e.now().Sub(n.CreatedAt) > time.Hour {
			body = "Earlier update: " + body
		}
		et, eid := "conversation", &n.ConversationID
		if n.BuyerRequestID != nil {
			et, eid = "buyer_request", n.BuyerRequestID
		}
		sendCtx := conversation.WithOutboundPurpose(ctx, conversation.OutboundPurpose{Kind: n.Kind, EntityType: et, EntityID: eid,
			Extra: map[string]any{"notice_id": n.ID.String(), "late": true}})
		err = e.messenger.SendText(sendCtx, conv.ExternalID, body)
	}
	if err != nil {
		f := channel.Classify(err)
		status := autonomy.NoticePending
		if !queueable(f) {
			status = autonomy.NoticeFailed
			e.raiseForFailure(ctx, f, "conversation", n.ConversationID, n.BuyerRequestID, err.Error(), map[string]any{"notice_id": n.ID.String()})
		}
		_, _ = e.notices.MarkNotice(ctx, n.ID, status, err.Error())
		e.recordNotice(ctx, n.ConversationID, n.BuyerRequestID, autonomy.OutcomeFailed, fallbackForClass(f), "notice_send_failed",
			map[string]any{"notice_id": n.ID.String(), "class": string(f.Class), "code": f.Code, "status": status})
		return f, false
	}
	if ok, _ := e.notices.MarkNotice(ctx, n.ID, autonomy.NoticeSent, ""); ok {
		e.recordNotice(ctx, n.ConversationID, n.BuyerRequestID, autonomy.OutcomeApplied, "", "notice_delivered",
			map[string]any{"notice_id": n.ID.String(), "kind": n.Kind, "queued_for": e.now().Sub(n.CreatedAt).Round(time.Second).String()})
		nid := n.ID
		_ = e.events.Write(ctx, &event.Event{EventType: event.BuyerNoticeSent, EntityType: "pending_notice", EntityID: &nid, ActorType: event.ActorSystem,
			Payload: event.Merge(map[string]any{"kind": n.Kind}, idPayload("buyer_request_id", n.BuyerRequestID))})
	}
	return channel.Failure{}, true
}

// BuyerWrote is called for every inbound buyer message (the service window
// is open again): notices waiting for the buyer are delivered by a durable
// job, and requests whose question never reached the buyer are re-advanced.
func (e *Engine) BuyerWrote(ctx context.Context, conversationID, messageID uuid.UUID) {
	if e.jobs == nil || e.notices == nil {
		return
	}
	if _, err := e.jobs.Enqueue(ctx, &jobs.Job{Type: JobNoticeFlush, IdempotencyKey: "notice-flush:" + messageID.String(),
		Payload: map[string]any{"conversation_id": conversationID.String()}, MaxAttempts: 3, RunAt: e.now().UTC()}); err != nil {
		e.logger.Error("enqueueing notice flush", "conversation_id", conversationID, "error", err)
	}
}

func (e *Engine) handleNoticeFlush(ctx context.Context, j *jobs.Job) error {
	conv := parseOptUUID(j.Payload["conversation_id"])
	if conv == nil || e.notices == nil {
		return jobs.Permanent(errors.New("malformed flush payload"))
	}
	ns, err := e.notices.PendingNotices(ctx, *conv)
	if err != nil {
		return err
	}
	for _, n := range ns {
		_ = e.store.WithLock(ctx, "notices:"+n.ID.String(), func(ctx context.Context) error {
			cur, err := e.notices.FindNotice(ctx, n.ID)
			if err != nil || cur == nil || cur.Status != autonomy.NoticePending {
				return err
			}
			e.deliverNotice(ctx, cur)
			return nil
		})
	}
	if e.ctxStore != nil {
		e.readvanceConversation(ctx, *conv)
	}
	return nil
}

// queueable: a later delivery of the same notice can succeed and is
// compliant (a retry, the buyer's next message, or a fixed configuration).
// Wrong numbers, malformed messages and account-level enforcement are not.
func queueable(f channel.Failure) bool {
	switch f.Class {
	case channel.FailInvalidRecipient, channel.FailInvalidRequest:
		return false
	case channel.FailPolicy:
		return !f.AccountLevel
	}
	return true
}

func fallbackForClass(f channel.Failure) autonomy.Fallback {
	switch {
	case f.Retry:
		return autonomy.TemporaryProviderFailure
	case f.Class == channel.FailPolicy && !f.AccountLevel:
		return autonomy.PolicyRejection
	}
	return autonomy.SystemException
}

// ---- provider-reported (asynchronous) delivery failures ----------------------------

// OutboundFailed implements conversation.OutboundFailureListener: Meta
// accepted a message and later reported it failed. Handled like a rejected
// send of the same purpose.
func (e *Engine) OutboundFailed(ctx context.Context, of conversation.OutboundFailure) {
	f := channel.ClassifyCode(of.Code, 0)
	errText := strings.TrimSpace(fmt.Sprintf("%d %s", of.Code, of.Title))
	detail := map[string]any{"message_id": of.MessageID.String(), "kind": of.Kind, "class": string(f.Class), "code": of.Code, "title": of.Title}
	mid := of.MessageID
	_ = e.events.Write(ctx, &event.Event{EventType: event.OutboundDeliveryFailed, EntityType: "message", EntityID: &mid, ActorType: event.ActorSystem,
		Payload: detail})
	switch {
	case of.Kind == "vendor_request" && of.EntityID != nil:
		e.vendorRequestUndelivered(ctx, *of.EntityID, f, &channel.ProviderError{Provider: "whatsapp", Code: of.Code, Title: of.Title,
			Body: "delivery failed: " + errText})
	case of.Kind == "offer_delivery" && of.EntityID != nil:
		e.offersUndelivered(ctx, *of.EntityID, of.Metadata, f, errText)
	case of.Kind == "clarification":
		e.questionUndelivered(ctx, of, f, errText)
	case requeueKinds[of.Kind]:
		var brid *uuid.UUID
		if of.EntityType == "buyer_request" {
			brid = of.EntityID
		}
		e.noticeFailed(ctx, of.ConversationID, brid, of.Kind, of.Text, f, errText, &mid)
	default:
		// Acknowledgements, replies and vendor questions: recorded only (a
		// late copy would not help; vendor requests recover via timeouts).
		e.recordNotice(ctx, of.ConversationID, nil, autonomy.OutcomeFailed, fallbackForClass(f), "message_undelivered_recorded", detail)
	}
}

// questionUndelivered: Majo's question never reached the buyer. It is closed
// as SUPERSEDED (not counted as asked) and re-asked when the buyer next
// writes — never in a loop.
func (e *Engine) questionUndelivered(ctx context.Context, of conversation.OutboundFailure, f channel.Failure, errText string) {
	cid := parseOptUUID(of.Metadata["clarification_id"])
	if cid == nil {
		return
	}
	at := e.now().UTC()
	if err := e.store.ResolveClarification(ctx, *cid, autonomy.ClarSuperseded, nil, "", map[string]any{"reason": "undelivered",
		"class": string(f.Class), "code": f.Code, "error": errText}, at); err != nil && !errors.Is(err, autonomy.ErrClarificationNotOpen) {
		e.logger.Error("closing undelivered clarification", "clarification_id", cid, "error", err)
		return
	}
	var brid *uuid.UUID
	if of.EntityType == "buyer_request" {
		brid = of.EntityID
	}
	e.recordNotice(ctx, of.ConversationID, brid, autonomy.OutcomeDeferred, fallbackForClass(f), "question_undelivered_reask_on_next_message",
		map[string]any{"clarification_id": cid.String(), "class": string(f.Class), "code": f.Code})
	if !f.Retry && !f.WaitForRecipient {
		e.raiseForFailure(ctx, f, "conversation", of.ConversationID, brid, errText, map[string]any{"clarification_id": cid.String()})
	}
}
