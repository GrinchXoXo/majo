package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/infrastructure/ai"
	"github.com/middleman/backend/internal/autonomy"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/jobs"
	"github.com/middleman/backend/internal/matching"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/routing"
	"github.com/middleman/backend/internal/supply"
	"github.com/middleman/backend/internal/vendor"
)

// Durable job types (Postgres job table: survives restarts, retried with backoff).
const (
	JobAdvance         = "WORKFLOW_ADVANCE"      // run the next autonomous decision for a request
	JobVendorResend    = "VENDOR_REQUEST_RESEND" // automatic recovery of a rejected vendor request
	JobOfferRedelivery = "OFFER_DELIVERY_RETRY"  // automatic recovery of a rejected offer delivery
	JobListingPublish  = "LISTING_AUTO_PUBLISH"  // trusted-vendor auto-publish evaluation
)

// ---- dependencies (small interfaces over existing services) -------------------

// Requests is request.Service.
type Requests interface {
	Create(ctx context.Context, n request.NewRequest) (*request.BuyerRequest, bool, error)
	ConfirmAs(ctx context.Context, id uuid.UUID, actor event.ActorType, reason string) (*request.BuyerRequest, error)
	ApplyClarification(ctx context.Context, id uuid.UUID, patch request.Fields, extra map[string]any) (*request.BuyerRequest, error)
	AwaitBuyer(ctx context.Context, id uuid.UUID) (*request.BuyerRequest, error)
	ClarifyAs(ctx context.Context, id uuid.UUID, question string, actor event.ActorType, extra map[string]any) error
	RunExtraction(ctx context.Context, id uuid.UUID)
}

// Router is routing.Service.
type Router interface {
	SuggestVendors(ctx context.Context, buyerRequestID uuid.UUID) ([]matching.Candidate, *request.BuyerRequest, error)
	RouteAs(ctx context.Context, req *request.BuyerRequest, vendorIDs []uuid.UUID, scores map[uuid.UUID]int, breakdowns map[uuid.UUID]map[string]any, actor event.ActorType, reason string) ([]*routing.VendorRequest, error)
	ResendVendorRequest(ctx context.Context, vendorRequestID uuid.UUID) error
}

// RoutingReader reads vendor requests (postgres.RoutingRepository).
type RoutingReader interface {
	ListVendorRequestsByBuyerRequest(ctx context.Context, buyerRequestID uuid.UUID) ([]*routing.VendorRequest, error)
	CountOpenByVendor(ctx context.Context, vendorIDs []uuid.UUID, since time.Time) (map[uuid.UUID]int, error)
}

// ContextStore builds buyer context from persisted state.
type ContextStore interface {
	BuyerContext(ctx context.Context, conversationID, buyerID uuid.UUID, since time.Time) (*BuyerContext, error)
	// PendingRequests returns requests that may be waiting for an autonomous
	// step (startup recovery): NEEDS_REVIEW, CONFIRMED without vendor
	// requests, or EXTRACTING older than staleExtraction.
	PendingRequests(ctx context.Context, since time.Time, staleExtraction time.Duration) ([]uuid.UUID, error)
	// StalledRequests returns this conversation's requests waiting for a
	// question slot: NEEDS_REVIEW whose latest decision deferred a question,
	// or AWAITING_BUYER whose latest question was superseded.
	StalledRequests(ctx context.Context, conversationID uuid.UUID) ([]uuid.UUID, error)
}

// Listings reads listings; Vendors reads vendors.
type Listings interface {
	FindByID(ctx context.Context, id uuid.UUID) (*supply.Listing, error)
}
type Vendors interface {
	FindByID(ctx context.Context, id uuid.UUID) (*vendor.Vendor, error)
}

// Extractor is the AI extraction service (proposes field values only).
type Extractor interface {
	Extract(ctx context.Context, req ai.ExtractionRequest) (*ai.ExtractionResult, error)
}

// Messenger sends text (the recording sender).
type Messenger interface {
	SendText(ctx context.Context, to, body string) error
}

// Conversations resolves a conversation's channel address.
type Conversations interface {
	FindByID(ctx context.Context, id uuid.UUID) (*conversation.Conversation, error)
}

// Enquirer turns a typed reference to a discovery listing into the same
// "Ask about it" enquiry a tap creates (distribution.Service).
type Enquirer interface {
	EnquireFromContext(ctx context.Context, buyerID, conversationID, messageID uuid.UUID, replyTo string, deliveryID, listingID uuid.UUID) bool
}

// Config holds the flags and parameters.
type Config struct {
	Flags          autonomy.Flags
	Params         autonomy.Params
	JobMaxAttempts int
}

// Engine is the autonomous process manager.
type Engine struct {
	cfg       Config
	store     autonomy.Store
	ctxStore  ContextStore
	requests  Requests
	reqRepo   request.Repository
	router    Router
	routes    RoutingReader
	listings  Listings
	vendors   Vendors
	extractor Extractor
	messenger Messenger
	convs     Conversations
	enquirer  Enquirer
	policy    *supply.EligibilityPolicy
	jobs      jobs.Store
	events    event.Writer
	logger    *slog.Logger
	now       func() time.Time

	paths          BuyerPaths
	offerDeliverer func(ctx context.Context, buyerRequestID uuid.UUID) error
	publisher      ListingPublisher
	async          func(func(context.Context)) // in-process runner for buyer answers

	// Phase 4C hardening.
	vrs           VendorRequestTimeouts
	recovery      RecoveryReader
	notices       autonomy.NoticeStore
	holds         autonomy.HoldStore
	offerReverter OfferReverter
}

// OfferReverter returns offers whose delivery the provider later reported
// as failed to a deliverable state (postgres.OfferRepository).
type OfferReverter interface {
	RevertUndelivered(ctx context.Context, offerIDs []uuid.UUID, reason string) (int, error)
}

// Deps groups the constructor arguments.
type Deps struct {
	Store     autonomy.Store
	Context   ContextStore
	Requests  Requests
	ReqRepo   request.Repository
	Router    Router
	Routes    RoutingReader
	Listings  Listings
	Vendors   Vendors
	Extractor Extractor
	Messenger Messenger
	Convs     Conversations
	Enquirer  Enquirer
	Policy    *supply.EligibilityPolicy
	Jobs      jobs.Store
	Events    event.Writer
	Logger    *slog.Logger
	Async     func(func(context.Context))
	// Phase 4C hardening (all optional; nil disables the capability).
	VendorRequests VendorRequestTimeouts
	Recovery       RecoveryReader
	Notices        autonomy.NoticeStore
	Holds          autonomy.HoldStore
	OfferReverter  OfferReverter
}

// New builds the engine.
func New(cfg Config, d Deps) *Engine {
	if cfg.JobMaxAttempts <= 0 {
		cfg.JobMaxAttempts = 8
	}
	async := d.Async
	if async == nil {
		async = func(f func(context.Context)) { go f(context.Background()) }
	}
	return &Engine{cfg: cfg, store: d.Store, ctxStore: d.Context, requests: d.Requests, reqRepo: d.ReqRepo, router: d.Router,
		routes: d.Routes, listings: d.Listings, vendors: d.Vendors, extractor: d.Extractor, messenger: d.Messenger, convs: d.Convs,
		enquirer: d.Enquirer, policy: d.Policy, jobs: d.Jobs, events: d.Events, logger: d.Logger, now: time.Now, async: async,
		vrs: d.VendorRequests, recovery: d.Recovery, notices: d.Notices, holds: d.Holds, offerReverter: d.OfferReverter}
}

// SetClock replaces the clock (tests).
func (e *Engine) SetClock(now func() time.Time) { e.now = now }

// SetOfferDeliverer wires automatic offer redelivery.
func (e *Engine) SetOfferDeliverer(f func(ctx context.Context, buyerRequestID uuid.UUID) error) {
	e.offerDeliverer = f
}

// Config returns the active configuration.
func (e *Engine) Config() Config { return e.cfg }

// Register adds the engine's durable job handlers to the worker.
func (e *Engine) Register(w *jobs.Worker) {
	w.Register(JobAdvance, e.handleAdvanceJob, e.onAdvanceExhausted)
	w.Register(JobBuyerAnswer, e.handleAnswerJob, nil)
	w.Register(JobVendorResend, e.handleVendorResend, e.onRetryExhausted)
	w.Register(JobOfferRedelivery, e.handleOfferRedelivery, e.onRetryExhausted)
	w.Register(JobListingPublish, e.handleListingPublish, nil)
	w.Register(JobVendorTimeout, e.handleVendorTimeout, e.onRecoveryExhausted)
	w.Register(JobNoticeResend, e.handleNoticeResend, e.onNoticeRetryExhausted)
	w.Register(JobNoticeFlush, e.handleNoticeFlush, nil)
	w.Register(JobRequestRecovery, e.handleRequestRecovery, e.onRecoveryExhausted)
}

// ---- request.Listener ---------------------------------------------------------

// RequestReady implements request.Listener: fields are known; decide next.
func (e *Engine) RequestReady(ctx context.Context, req *request.BuyerRequest) {
	e.enqueueAdvance(ctx, req.ID, "ready")
}

// RequestConfirmed implements request.Listener: route (whoever confirmed —
// an operator who resolved an exception hands the request back to Majo).
func (e *Engine) RequestConfirmed(ctx context.Context, req *request.BuyerRequest, actor event.ActorType) {
	// A question still open for this request is moot now (e.g. an operator
	// confirmed it): close it so a late answer cannot rewrite the request.
	if c, err := e.store.OpenBuyerClarification(ctx, req.ConversationID, e.now().UTC()); err == nil && c != nil &&
		c.BuyerRequestID != nil && *c.BuyerRequestID == req.ID {
		_ = e.store.ResolveClarification(ctx, c.ID, autonomy.ClarCancelled, nil, "", map[string]any{"reason": "request_confirmed", "by": string(actor)}, e.now().UTC())
	}
	e.enqueueAdvance(ctx, req.ID, "confirmed")
}

// Readvance re-runs the next step for a request (e.g. after an operator
// resolved its exception).
func (e *Engine) Readvance(ctx context.Context, id uuid.UUID) { e.enqueueAdvance(ctx, id, "readvance") }

// onAdvanceExhausted: a workflow step kept failing -> operator exception
// (never a silently stuck request).
func (e *Engine) onAdvanceExhausted(ctx context.Context, j *jobs.Job, err error) {
	id := parseOptUUID(j.Payload["buyer_request_id"])
	if id == nil {
		return
	}
	e.raise(ctx, autonomy.SystemException, autonomy.ReasonWorkflowFailure, "buyer_request", *id, id,
		map[string]any{"step": "advance", "error": err.Error(), "attempts": j.Attempts})
}

func (e *Engine) enqueueAdvance(ctx context.Context, id uuid.UUID, trigger string) {
	if e.jobs == nil {
		return
	}
	// Every trigger is its own job (a key reused after an earlier job
	// completed would silently drop the step). Duplicate runs are harmless:
	// Advance is idempotent and serialised per request.
	key := fmt.Sprintf("advance:%s:%s:%s", id, trigger, uuid.NewString())
	if _, err := e.jobs.Enqueue(ctx, &jobs.Job{Type: JobAdvance, IdempotencyKey: key,
		Payload: map[string]any{"buyer_request_id": id.String(), "trigger": trigger}, MaxAttempts: e.cfg.JobMaxAttempts, RunAt: e.now().UTC()}); err != nil {
		e.logger.Error("enqueueing workflow advance", "buyer_request_id", id, "error", err)
	}
}

func (e *Engine) handleAdvanceJob(ctx context.Context, j *jobs.Job) error {
	id, err := uuid.Parse(fmt.Sprint(j.Payload["buyer_request_id"]))
	if err != nil {
		return jobs.Permanent(fmt.Errorf("bad buyer_request_id: %w", err))
	}
	return e.Advance(ctx, id)
}

// Advance runs the next autonomous decision for one request (serialised per
// request; idempotent: safe to run any number of times).
func (e *Engine) Advance(ctx context.Context, id uuid.UUID) error {
	return e.store.WithLock(ctx, "request:"+id.String(), func(ctx context.Context) error {
		req, err := e.reqRepo.FindByID(ctx, id)
		if err != nil {
			return err
		}
		if req == nil {
			return jobs.Permanent(fmt.Errorf("buyer request %s not found", id))
		}
		if e.held(ctx, id) {
			// Paused by an operator: no automatic step (resume re-advances).
			e.decide(ctx, req, autonomy.DecHold, autonomy.OutcomeDeferred, "", "advance_skipped_while_held", map[string]any{"status": string(req.Status)}, nil, nil)
			return nil
		}
		switch req.Status {
		case request.StatusNeedsReview:
			return e.decideConfirmation(ctx, req)
		case request.StatusConfirmed:
			return e.decideRouting(ctx, req)
		case request.StatusExtracting:
			if e.now().Sub(req.CreatedAt) > 2*time.Minute {
				e.requests.RunExtraction(ctx, req.ID) // recovery: notifies Ready again
			}
		case request.StatusAwaitingBuyer:
			// Waiting for the buyer — unless its question was superseded by
			// another conversation topic: then ask again (once it is free).
			if interrupted, err := e.questionInterrupted(ctx, req); err != nil || !interrupted {
				return err
			}
			return e.decideConfirmation(ctx, req)
		}
		return nil
	})
}

// Recover re-runs the next step for requests left mid-flow by a restart.
func (e *Engine) Recover(ctx context.Context) {
	if e.ctxStore == nil {
		return
	}
	ids, err := e.ctxStore.PendingRequests(ctx, e.now().Add(-7*24*time.Hour), 2*time.Minute)
	if err != nil {
		e.logger.Error("workflow recovery scan", "error", err)
		return
	}
	for _, id := range ids {
		e.enqueueAdvance(ctx, id, "recovery")
	}
	if len(ids) > 0 {
		e.logger.Info("workflow recovery enqueued", "requests", len(ids))
	}
}

// ---- confirmation --------------------------------------------------------------

func (e *Engine) decideConfirmation(ctx context.Context, req *request.BuyerRequest) error {
	f, p := e.cfg.Flags, e.cfg.Params
	if !f.AutoConfirmClearRequests {
		e.decide(ctx, req, autonomy.DecConfirmRequest, autonomy.OutcomeRejected, autonomy.PolicyRejection, "auto_confirm_disabled", nil, nil, nil)
		return nil
	}
	cs, err := e.store.ClarificationsForRequest(ctx, req.ID)
	if err != nil {
		return err
	}
	h := HistoryFrom(cs)
	known := ""
	if bc, err := e.ctxStore.BuyerContext(ctx, req.ConversationID, req.BuyerID, e.now().Add(-p.ContextWindow)); err == nil && bc != nil {
		known = bc.KnownLocation
	}
	a := Assess(req, known, h, p)
	conf := req.Confidence
	inputs := map[string]any{"completeness": string(req.Completeness), "asked": h.Asked, "skipped": h.Skipped, "known_location": known,
		"understanding": a.Outcome}
	if a.Hold {
		// Restricted item: never confirmed or routed automatically. The
		// request stays in review; an operator decides (override confirm,
		// or leave it). The buyer already got the neutral acknowledgement.
		e.decide(ctx, req, autonomy.DecRestrictedItem, autonomy.OutcomeRejected, autonomy.SafetyException, a.Reason, inputs, nil, &conf)
		e.raise(ctx, autonomy.SafetyException, autonomy.ReasonRestrictedItem, "buyer_request", req.ID, &req.ID,
			map[string]any{"reason": a.Reason, "why": "the request names an item on the restricted-items list; it is not routed automatically"})
		return nil
	}

	if a.FillLocation != "" || a.FillVariant != "" {
		var patch request.Fields
		res := map[string]any{}
		if a.FillLocation != "" {
			loc := a.FillLocation
			patch.Location, res["location"] = &loc, loc
		}
		if a.FillVariant != "" {
			v := a.FillVariant
			patch.Variant, res["variant"] = &v, v
		}
		if _, err := e.requests.ApplyClarification(ctx, req.ID, patch, map[string]any{"source": "context"}); err != nil {
			return err
		}
		e.decide(ctx, req, autonomy.DecContinueRequest, autonomy.OutcomeApplied, "", a.Reason, inputs, res, &conf)
		return nil // ApplyClarification notified Ready: the next run confirms
	}
	if a.Clear {
		if _, err := e.requests.ConfirmAs(ctx, req.ID, event.ActorSystem, "auto_clear_request"); err != nil {
			if errors.Is(err, request.ErrInvalidTransition) {
				return nil
			}
			return err
		}
		e.decide(ctx, req, autonomy.DecConfirmRequest, autonomy.OutcomeApplied, "", a.Reason, inputs, nil, &conf)
		return nil
	}
	if a.Missing == "" { // genuinely unclear after asking: stop asking, stay with the buyer
		e.decide(ctx, req, autonomy.DecConfirmRequest, autonomy.OutcomeRejected, autonomy.GenuineAmbiguity, a.Reason, inputs, nil, &conf)
		e.say(ctx, req.ConversationID, &req.ID, "clarification_exhausted",
			"I couldn't quite work out what you're looking for 🙏 Send me the item (and your area) whenever you're ready and I'll search.")
		_, _ = e.requests.AwaitBuyer(ctx, req.ID)
		return nil
	}
	if !f.AutoClarifyRequests {
		e.decide(ctx, req, autonomy.DecAskClarification, autonomy.OutcomeRejected, autonomy.PolicyRejection, "auto_clarify_disabled",
			inputs, map[string]any{"missing": a.Missing}, &conf)
		return nil
	}
	if h.Total >= p.MaxClarifications {
		if strings.TrimSpace(deref(req.Product)) != "" {
			// Enough questions: proceed with what is known.
			if _, err := e.requests.ConfirmAs(ctx, req.ID, event.ActorSystem, "clarifications_exhausted_proceeding"); err != nil && !errors.Is(err, request.ErrInvalidTransition) {
				return err
			}
			e.decide(ctx, req, autonomy.DecConfirmRequest, autonomy.OutcomeApplied, "", "clarifications_exhausted_proceeding", inputs, map[string]any{"missing": a.Missing}, &conf)
			return nil
		}
		e.decide(ctx, req, autonomy.DecAskClarification, autonomy.OutcomeRejected, autonomy.GenuineAmbiguity, "max_clarifications", inputs, nil, &conf)
		e.say(ctx, req.ConversationID, &req.ID, "clarification_exhausted",
			"I couldn't quite work out what you're looking for 🙏 Send me the item (and your area) whenever you're ready and I'll search.")
		_, _ = e.requests.AwaitBuyer(ctx, req.ID)
		return nil
	}
	return e.askBuyer(ctx, req, a.Missing, a.Question, a.Options, inputs)
}

// askBuyer opens a missing-field clarification and sends the question.
func (e *Engine) askBuyer(ctx context.Context, req *request.BuyerRequest, field, question string, options []string, inputs map[string]any) error {
	conv, bid, rid := req.ConversationID, req.BuyerID, req.ID
	var opts []autonomy.Option
	for i, o := range options {
		opts = append(opts, autonomy.Option{Key: fmt.Sprint(i + 1), Label: o})
	}
	// One question at a time per conversation: if another request's
	// question is open, this one waits (re-advanced when that one closes),
	// so an answer can never be merged into the wrong request.
	c, blocking, err := e.store.OpenClarificationIfFree(ctx, &autonomy.Clarification{Party: autonomy.PartyBuyer, ConversationID: &conv, BuyerID: &bid,
		BuyerRequestID: &rid, Kind: autonomy.KindMissingField, Field: field, Question: question, Status: autonomy.ClarOpen, Attempts: 1,
		Options: opts, ExpiresAt: e.now().UTC().Add(e.cfg.Params.ClarificationTTL)})
	if err != nil {
		return err
	}
	if blocking != nil {
		e.decide(ctx, req, autonomy.DecAskClarification, autonomy.OutcomeDeferred, autonomy.ClarificationRequired, "another_question_open", inputs,
			map[string]any{"field": field, "waiting_for": blocking.ID.String()}, nil)
		return nil
	}
	if err := e.requests.ClarifyAs(ctx, req.ID, question, event.ActorSystem, map[string]any{"clarification_id": c.ID.String(), "field": field}); err != nil {
		// The question could not be delivered: close it and retry the step.
		_ = e.store.ResolveClarification(ctx, c.ID, autonomy.ClarCancelled, nil, "", map[string]any{"send_error": err.Error()}, e.now().UTC())
		e.decide(ctx, req, autonomy.DecAskClarification, autonomy.OutcomeFailed, autonomy.TemporaryProviderFailure, "question_not_sent", inputs, nil, nil)
		return fmt.Errorf("sending clarification: %w", err)
	}
	if _, err := e.requests.AwaitBuyer(ctx, req.ID); err != nil && !errors.Is(err, request.ErrInvalidTransition) {
		return err
	}
	e.decide(ctx, req, autonomy.DecAskClarification, autonomy.OutcomeDeferred, autonomy.ClarificationRequired, "missing_"+field, inputs,
		map[string]any{"clarification_id": c.ID.String(), "field": field, "question": question}, nil)
	return nil
}

// ---- routing -------------------------------------------------------------------

func (e *Engine) decideRouting(ctx context.Context, req *request.BuyerRequest) error {
	existing, err := e.routes.ListVendorRequestsByBuyerRequest(ctx, req.ID)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		return nil // already routed (by Majo or an operator)
	}
	if done, err := e.routingSettled(ctx, req.ID); err != nil || done {
		return err
	}
	if req.Origin == request.OriginDiscovery && req.ListingID != nil {
		if !e.cfg.Flags.AutoRouteDiscoveryRequests {
			e.decide(ctx, req, autonomy.DecRouteRequest, autonomy.OutcomeRejected, autonomy.PolicyRejection, "auto_route_discovery_disabled", nil, nil, nil)
			return nil
		}
		l, err := e.listings.FindByID(ctx, *req.ListingID)
		if err != nil {
			return err
		}
		if l != nil {
			v, err := e.vendors.FindByID(ctx, l.VendorID)
			if err != nil {
				return err
			}
			if v != nil && v.Status == vendor.StatusActive {
				vrs, err := e.router.RouteAs(ctx, req, []uuid.UUID{v.ID}, nil, nil, event.ActorSystem, "listing_vendor")
				if err != nil {
					return err
				}
				e.decide(ctx, req, autonomy.DecRouteRequest, autonomy.OutcomeApplied, "", "listing_vendor",
					map[string]any{"listing_reference": l.PublicReference, "vendor_id": v.ID.String()}, map[string]any{"vendor_requests": refsOf(vrs)}, nil)
				return nil
			}
		}
		// The listing's vendor is gone: fall through to matched supply.
		if !e.cfg.Flags.AutoRouteMatchedRequests {
			e.decide(ctx, req, autonomy.DecRouteRequest, autonomy.OutcomeRejected, autonomy.PolicyRejection, "listing_vendor_unavailable_auto_route_matched_disabled", nil, nil, nil)
			return nil
		}
	} else if !e.cfg.Flags.AutoRouteMatchedRequests {
		e.decide(ctx, req, autonomy.DecRouteRequest, autonomy.OutcomeRejected, autonomy.PolicyRejection, "auto_route_matched_disabled", nil, nil, nil)
		return nil
	}
	return e.routeMatched(ctx, req, false)
}

// routingSettled: a previous run already ended routing for this request
// (no match told, or a broaden question pending/declined).
func (e *Engine) routingSettled(ctx context.Context, id uuid.UUID) (bool, error) {
	ds, err := e.store.DecisionsForRequest(ctx, id)
	if err != nil {
		return false, err
	}
	for _, d := range ds {
		if d.Kind == autonomy.DecNoMatch || (d.Kind == autonomy.DecRouteRequest && d.Reason == "no_local_match") {
			return true, nil
		}
	}
	return false, nil
}

// routeMatched runs matching and the routing policy. broaden=true contacts
// eligible vendors outside the buyer's area (the buyer agreed).
func (e *Engine) routeMatched(ctx context.Context, req *request.BuyerRequest, broaden bool) error {
	return e.routeMatchedWith(ctx, req, broaden, nil, e.cfg.Params.RouteMaxVendors)
}

// planRouting runs matching (minus vendors already contacted) and the
// deterministic routing policy.
func (e *Engine) planRouting(ctx context.Context, req *request.BuyerRequest, exclude map[uuid.UUID]bool) (RoutePlan, []map[string]any, error) {
	all, _, err := e.router.SuggestVendors(ctx, req.ID)
	if err != nil {
		return RoutePlan{}, nil, err
	}
	cands := make([]matching.Candidate, 0, len(all))
	for _, c := range all {
		if !exclude[c.Vendor.ID] {
			cands = append(cands, c)
		}
	}
	ids := make([]uuid.UUID, len(cands))
	for i, c := range cands {
		ids[i] = c.Vendor.ID
	}
	open := map[uuid.UUID]int{}
	if len(ids) > 0 {
		if open, err = e.routes.CountOpenByVendor(ctx, ids, e.now().Add(-e.cfg.Params.VendorRequestTTL)); err != nil {
			return RoutePlan{}, nil, err
		}
	}
	plan := RoutingPolicy(req, cands, open, e.policy, e.cfg.Params, e.now().UTC())
	explained := make([]map[string]any, 0, len(cands))
	for _, c := range cands {
		explained = append(explained, Explain(c))
	}
	return plan, explained, nil
}

// routeTo contacts the chosen candidates through the routing service.
func (e *Engine) routeTo(ctx context.Context, req *request.BuyerRequest, route []matching.Candidate, reason string) ([]*routing.VendorRequest, error) {
	vendorIDs := make([]uuid.UUID, len(route))
	scores, breakdowns := map[uuid.UUID]int{}, map[uuid.UUID]map[string]any{}
	for i, c := range route {
		vendorIDs[i] = c.Vendor.ID
		scores[c.Vendor.ID] = c.Score
		bd := map[string]any{}
		for k, v := range c.Breakdown {
			bd[k] = v
		}
		breakdowns[c.Vendor.ID] = bd
	}
	return e.router.RouteAs(ctx, req, vendorIDs, scores, breakdowns, event.ActorSystem, reason)
}

// routeMatchedWith: exclude = vendors already contacted (recovery), capN =
// the most vendors to contact now.
func (e *Engine) routeMatchedWith(ctx context.Context, req *request.BuyerRequest, broaden bool, exclude map[uuid.UUID]bool, capN int) error {
	plan, explained, err := e.planRouting(ctx, req, exclude)
	if err != nil {
		return err
	}
	route := plan.Route
	if broaden {
		route = append(append([]matching.Candidate{}, plan.Route...), plan.NonLocal...)
	}
	if len(route) > capN {
		route = route[:capN]
	}
	inputs := map[string]any{"candidates": explained, "excluded": plan.Excluded, "broaden": broaden,
		"min_score": e.cfg.Params.RouteMinScore, "max_vendors": capN}
	if len(exclude) > 0 {
		inputs["already_contacted"] = len(exclude)
	}

	if len(route) > 0 {
		reason := "routing_policy"
		if broaden {
			reason = "routing_policy_broadened"
		}
		vrs, err := e.routeTo(ctx, req, route, reason)
		if err != nil {
			return err
		}
		e.decide(ctx, req, autonomy.DecRouteRequest, autonomy.OutcomeApplied, "", reason, inputs, map[string]any{"vendor_requests": refsOf(vrs)}, nil)
		n := len(vrs)
		if n > 0 {
			e.say(ctx, req.ConversationID, &req.ID, "routing_update", fmt.Sprintf("I'm checking with %d %s for %s now 👀 I'll send you options as soon as they reply.",
				n, plural(n, "seller", "sellers"), describe(req)))
		}
		return nil
	}
	if !broaden && len(plan.NonLocal) > 0 && e.cfg.Flags.AutoClarifyRequests && !e.broadenExcluded(req) {
		q := fmt.Sprintf("I couldn't find %s in %s%s right now. I can check sellers in other areas too. Want me to? (Yes / No)",
			describe(req), deref(req.Location), budgetSuffix(req))
		conv, bid, rid := req.ConversationID, req.BuyerID, req.ID
		c, err := e.store.OpenClarification(ctx, &autonomy.Clarification{Party: autonomy.PartyBuyer, ConversationID: &conv, BuyerID: &bid,
			BuyerRequestID: &rid, Kind: autonomy.KindBroadenScope, Question: q, Status: autonomy.ClarOpen, Attempts: 1,
			Options:   []autonomy.Option{{Key: "yes", Label: "Yes"}, {Key: "no", Label: "No"}},
			ExpiresAt: e.now().UTC().Add(e.cfg.Params.ClarificationTTL)})
		if err != nil {
			return err
		}
		if err := e.requests.ClarifyAs(ctx, req.ID, q, event.ActorSystem, map[string]any{"clarification_id": c.ID.String(), "kind": autonomy.KindBroadenScope}); err != nil {
			_ = e.store.ResolveClarification(ctx, c.ID, autonomy.ClarCancelled, nil, "", map[string]any{"send_error": err.Error()}, e.now().UTC())
			return err
		}
		e.decide(ctx, req, autonomy.DecRouteRequest, autonomy.OutcomeDeferred, autonomy.ClarificationRequired, "no_local_match", inputs,
			map[string]any{"clarification_id": c.ID.String(), "non_local": len(plan.NonLocal)}, nil)
		return nil
	}
	// No suitable seller: tell the buyer honestly (not an operator event).
	text := fmt.Sprintf("I couldn't find a seller for %s%s right now 😔 Tell me if you'd like to change anything — budget, condition or area — and I'll search again.",
		describe(req), locSuffix(req))
	e.say(ctx, req.ConversationID, &req.ID, "no_match", text)
	e.decide(ctx, req, autonomy.DecNoMatch, autonomy.OutcomeApplied, "", "no_eligible_vendor", inputs, nil, nil)
	rid := req.ID
	_ = e.events.Write(ctx, &event.Event{EventType: event.NoMatchNotified, EntityType: "buyer_request", EntityID: &rid, ActorType: event.ActorSystem,
		Payload: event.Merge(req.FunnelDims(), map[string]any{"candidates": len(explained), "broadened": broaden})})
	return nil
}

func refsOf(vrs []*routing.VendorRequest) []string {
	out := make([]string, 0, len(vrs))
	for _, v := range vrs {
		out = append(out, v.PublicReference)
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func describe(req *request.BuyerRequest) string {
	d := strings.TrimSpace(deref(req.Product) + " " + deref(req.Variant))
	if d == "" {
		return "that"
	}
	return d
}

func locSuffix(req *request.BuyerRequest) string {
	if l := strings.TrimSpace(deref(req.Location)); l != "" {
		return " in " + l
	}
	return ""
}

func budgetSuffix(req *request.BuyerRequest) string {
	if req.BudgetMax != nil {
		return " within ₦" + thousands(*req.BudgetMax)
	}
	return ""
}

// ---- shared helpers --------------------------------------------------------------

// say sends a buyer-facing text attributed to the request. A failed send
// is classified and handled by notify (queued, retried or raised).
func (e *Engine) say(ctx context.Context, conversationID uuid.UUID, requestID *uuid.UUID, kind, text string) {
	e.notify(ctx, conversationID, requestID, kind, text)
}

func (e *Engine) decide(ctx context.Context, req *request.BuyerRequest, kind, outcome string, fb autonomy.Fallback, reason string,
	inputs, result map[string]any, conf *float64) {
	rid := req.ID
	if err := e.store.RecordDecision(ctx, &autonomy.Decision{WorkflowType: "buyer_request", SubjectType: "buyer_request", SubjectID: rid,
		BuyerRequestID: &rid, Kind: kind, Outcome: outcome, Fallback: fb, Reason: reason, Confidence: conf,
		PolicyVersion: autonomy.PolicyVersion, Inputs: inputs, Result: result, Actor: "SYSTEM"}); err != nil {
		e.logger.Error("recording workflow decision", "buyer_request_id", rid, "decision", kind, "error", err)
	}
}

func (e *Engine) raise(ctx context.Context, class autonomy.Fallback, reason, subjectType string, subjectID uuid.UUID, brid *uuid.UUID, detail map[string]any) {
	ex, err := e.store.RaiseException(ctx, &autonomy.Exception{Class: class, Reason: reason, SubjectType: subjectType, SubjectID: subjectID,
		BuyerRequestID: brid, Detail: detail})
	if err != nil {
		e.logger.Error("raising workflow exception", "reason", reason, "error", err)
		return
	}
	id := ex.ID
	_ = e.events.Write(ctx, &event.Event{EventType: event.WorkflowExceptionRaised, EntityType: "workflow_exception", EntityID: &id,
		ActorType: event.ActorSystem, Payload: event.Merge(map[string]any{"class": string(class), "reason": reason,
			"subject_type": subjectType, "subject_id": subjectID.String()}, detail)})
}

// SetEnquirer wires discovery enquiries by text (distribution.Service).
func (e *Engine) SetEnquirer(q Enquirer) { e.enquirer = q }

// questionInterrupted: the request waits for the buyer but its question is
// no longer open because another topic superseded it (not because the buyer
// went quiet or Majo gave up).
func (e *Engine) questionInterrupted(ctx context.Context, req *request.BuyerRequest) (bool, error) {
	if strings.TrimSpace(deref(req.Product)) == "" {
		// Nothing was understood (e.g. a greeting) and the buyer moved on to
		// something else: do not chase them with the old question.
		return false, nil
	}
	cs, err := e.store.ClarificationsForRequest(ctx, req.ID)
	if err != nil || len(cs) == 0 {
		return false, err
	}
	last := cs[len(cs)-1]
	return last.Status == autonomy.ClarSuperseded, nil
}

// readvanceConversation re-runs requests in the conversation that were
// waiting for a free question slot (or whose question was interrupted).
func (e *Engine) readvanceConversation(ctx context.Context, conversationID uuid.UUID) {
	ids, err := e.ctxStore.StalledRequests(ctx, conversationID)
	if err != nil {
		e.logger.Error("loading stalled requests", "conversation_id", conversationID, "error", err)
		return
	}
	for _, id := range ids {
		e.enqueueAdvance(ctx, id, "question_slot_free")
	}
}
