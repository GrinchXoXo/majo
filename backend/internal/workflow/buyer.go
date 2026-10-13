package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/infrastructure/ai"
	"github.com/middleman/backend/internal/autonomy"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/jobs"
	"github.com/middleman/backend/internal/matching"
	"github.com/middleman/backend/internal/request"
)

// BuyerText is a buyer's free-text message (already persisted).
type BuyerText struct {
	ConversationID uuid.UUID
	BuyerID        uuid.UUID
	MessageID      uuid.UUID
	ReplyTo        string
	Text           string
}

// Callbacks are the normal buyer paths the workflow hands back to when a
// message is not an answer or a reference (marketplace.BuyerInbound).
type Callbacks struct {
	// Default runs the ordinary path: offer selection by number, or a new request.
	Default func(ctx context.Context)
	// Select records the buyer's explicit choice of an offer.
	Select func(ctx context.Context, buyerRequestID, offerID uuid.UUID, ordinal int)
}

// BuyerPaths is the ordinary buyer pipeline (marketplace.BuyerInbound): used
// by durable answer jobs, which cannot carry closures.
type BuyerPaths interface {
	HandleDefault(ctx context.Context, m BuyerText)
	SelectOffer(ctx context.Context, m BuyerText, buyerRequestID, offerID uuid.UUID, ordinal int)
}

// SetBuyerPaths wires the ordinary buyer pipeline.
func (e *Engine) SetBuyerPaths(p BuyerPaths) { e.paths = p }

func (e *Engine) callbacks(m BuyerText) Callbacks {
	return Callbacks{
		Default: func(ctx context.Context) {
			if e.paths != nil {
				e.paths.HandleDefault(ctx, m)
			}
		},
		Select: func(ctx context.Context, brID, offerID uuid.UUID, ordinal int) {
			if e.paths != nil {
				e.paths.SelectOffer(ctx, m, brID, offerID, ordinal)
			}
		},
	}
}

// HandleBuyerText is the context-resolution step for a buyer's text,
// before the ordinary path. It returns true when it took the message:
//   - an answer to Majo's open question (a durable job: extraction may be
//     needed, and an answer must survive a restart), or
//   - a reference to something already shown ("how much is the black one?",
//     "I want that one", "something cheaper").
//
// Anything else returns false and continues unchanged. With
// AUTO_CLARIFY_REQUESTS off nothing is taken (the pre-autonomy path).
func (e *Engine) HandleBuyerText(ctx context.Context, m BuyerText) bool {
	if !e.cfg.Flags.AutoClarifyRequests {
		return false
	}
	now := e.now().UTC()
	c, err := e.store.OpenBuyerClarification(ctx, m.ConversationID, now)
	if err != nil {
		e.logger.Error("loading buyer clarification", "conversation_id", m.ConversationID, "error", err)
	} else if c != nil {
		// A number while Majo asked "Do you want option 2?" is a selection:
		// the ordinary resolver records it; the question is answered.
		if c.Kind == autonomy.KindConfirmOption && isSelectionNumber(m.Text) {
			_ = e.closeBuyerClarification(ctx, c, autonomy.ClarAnswered, m, map[string]any{"via": "selection_number"})
			return false
		}
		// Options were presented after Majo asked its question: a bare number
		// now picks an option (the conversation moved on), not an answer.
		if c.Kind == autonomy.KindMissingField && isSelectionNumber(m.Text) && e.offersSince(ctx, m, c.AskedAt) {
			_ = e.closeBuyerClarification(ctx, c, autonomy.ClarSuperseded, m, map[string]any{"reason": "offer_selection"})
			return false
		}
		if e.enqueueAnswer(ctx, m) {
			return true
		}
		e.async(func(bg context.Context) { e.ProcessAnswer(bg, m) })
		return true
	}
	return e.handleReference(ctx, m, e.callbacks(m))
}

// JobBuyerAnswer processes a buyer message while a question is open.
const JobBuyerAnswer = "BUYER_ANSWER"

func (e *Engine) enqueueAnswer(ctx context.Context, m BuyerText) bool {
	if e.jobs == nil {
		return false
	}
	_, err := e.jobs.Enqueue(ctx, &jobs.Job{Type: JobBuyerAnswer, IdempotencyKey: "answer:" + m.MessageID.String(),
		Payload: map[string]any{"conversation_id": m.ConversationID.String(), "buyer_id": m.BuyerID.String(), "message_id": m.MessageID.String(),
			"reply_to": m.ReplyTo, "text": m.Text}, MaxAttempts: e.cfg.JobMaxAttempts, RunAt: e.now().UTC()})
	if err != nil {
		e.logger.Error("enqueueing buyer answer", "message_id", m.MessageID, "error", err)
		return false
	}
	return true
}

func (e *Engine) handleAnswerJob(ctx context.Context, j *jobs.Job) error {
	m := BuyerText{ReplyTo: fmt.Sprint(j.Payload["reply_to"]), Text: fmt.Sprint(j.Payload["text"])}
	for k, dst := range map[string]*uuid.UUID{"conversation_id": &m.ConversationID, "buyer_id": &m.BuyerID, "message_id": &m.MessageID} {
		id := parseOptUUID(j.Payload[k])
		if id == nil {
			return jobs.Permanent(fmt.Errorf("malformed answer payload: %s", k))
		}
		*dst = *id
	}
	e.ProcessAnswer(ctx, m)
	return nil
}

// ProcessAnswer handles a buyer message that arrived while a question was
// open, serialised per conversation. If the question was meanwhile answered
// (two quick messages) the message is not dropped: it takes the reference
// or ordinary path instead.
func (e *Engine) ProcessAnswer(ctx context.Context, m BuyerText) {
	cb := e.callbacks(m)
	var c *autonomy.Clarification
	err := e.store.WithLock(ctx, "conversation:"+m.ConversationID.String(), func(ctx context.Context) error {
		var err error
		c, err = e.store.OpenBuyerClarification(ctx, m.ConversationID, e.now().UTC())
		if err != nil || c == nil {
			return err
		}
		e.answerBuyer(ctx, m, c, cb)
		return nil
	})
	if err != nil {
		e.logger.Error("processing buyer answer", "message_id", m.MessageID, "error", err)
	}
	if c == nil && err == nil {
		if !e.handleReference(ctx, m, cb) {
			cb.Default(ctx)
		}
	}
}

var selectionNumber = func(s string) bool {
	t := autonomy.Normalize(s)
	t = strings.TrimPrefix(t, "#")
	if len(t) == 0 || len(t) > 2 {
		return false
	}
	for _, r := range t {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isSelectionNumber(s string) bool { return selectionNumber(s) }

// ---- answers to Majo's questions ------------------------------------------------

func (e *Engine) offersSince(ctx context.Context, m BuyerText, at time.Time) bool {
	bc, err := e.ctxStore.BuyerContext(ctx, m.ConversationID, m.BuyerID, at)
	if err != nil || bc == nil {
		return false
	}
	for _, it := range bc.Items {
		if it.Kind == ItemOffer && it.At.After(at) {
			return true
		}
	}
	return false
}

// liveReference: the message refers to something Majo actually showed.
func (e *Engine) liveReference(ctx context.Context, m BuyerText) bool {
	ref := ParseReference(m.Text)
	if ref.Intent == IntentNone {
		return false
	}
	bc, err := e.ctxStore.BuyerContext(ctx, m.ConversationID, m.BuyerID, e.now().Add(-e.cfg.Params.ContextWindow))
	if err != nil || bc == nil {
		return false
	}
	return len(ResolveReference(ref, bc.Items)) > 0
}

func (e *Engine) answerBuyer(ctx context.Context, m BuyerText, c *autonomy.Clarification, cb Callbacks) {
	switch c.Kind {
	case autonomy.KindMissingField:
		e.answerMissingField(ctx, m, c, cb)
	case autonomy.KindWhichItem:
		opt := MatchItemOption(m.Text, c.Options)
		if opt == nil {
			e.notAnAnswer(ctx, m, c, cb)
			return
		}
		item, ok := itemOf(opt)
		if !ok {
			e.notAnAnswer(ctx, m, c, cb)
			return
		}
		if e.closeBuyerClarification(ctx, c, autonomy.ClarAnswered, m, map[string]any{"option": opt.Key, "ref": item.Ref}) != nil {
			return
		}
		intent, _ := c.Context["intent"].(string)
		e.actOnItem(ctx, m, item, intent, cb)
	case autonomy.KindConfirmOption:
		switch {
		case StrongYes(m.Text):
			item, ok := itemOfContext(c.Context)
			if !ok || item.OfferID == nil || item.BuyerRequestID == nil {
				e.notAnAnswer(ctx, m, c, cb)
				return
			}
			if e.closeBuyerClarification(ctx, c, autonomy.ClarAnswered, m, map[string]any{"selected": true}) != nil {
				return
			}
			cb.Select(ctx, *item.BuyerRequestID, *item.OfferID, item.Ordinal)
		case YesNo(m.Text) < 0:
			if e.closeBuyerClarification(ctx, c, autonomy.ClarAnswered, m, map[string]any{"selected": false}) != nil {
				return
			}
			e.say(ctx, m.ConversationID, nil, "clarification_ack", "No problem 👍 Reply with another option number, or tell me what you're looking for.")
		default:
			e.notAnAnswer(ctx, m, c, cb)
		}
	case autonomy.KindBroadenScope:
		yn := YesNo(m.Text)
		if yn == 0 || c.BuyerRequestID == nil {
			e.notAnAnswer(ctx, m, c, cb)
			return
		}
		if e.closeBuyerClarification(ctx, c, autonomy.ClarAnswered, m, map[string]any{"broaden": yn > 0}) != nil {
			return
		}
		rid := *c.BuyerRequestID
		if err := e.store.WithLock(ctx, "request:"+rid.String(), func(ctx context.Context) error {
			req, err := e.reqRepo.FindByID(ctx, rid)
			if err != nil || req == nil {
				return err
			}
			if yn < 0 {
				e.decide(ctx, req, autonomy.DecNoMatch, autonomy.OutcomeApplied, "", "buyer_declined_broader_search", nil, nil, nil)
				e.say(ctx, m.ConversationID, &req.ID, "clarification_ack", "Okay 👍 Tell me if you'd like me to look for something else.")
				return nil
			}
			existing, err := e.routes.ListVendorRequestsByBuyerRequest(ctx, req.ID)
			if err != nil || len(existing) > 0 || !e.cfg.Flags.AutoRouteMatchedRequests {
				return err // already routed (or routing is switched off)
			}
			return e.routeMatched(ctx, req, true)
		}); err != nil {
			e.logger.Error("broadened routing", "buyer_request_id", rid, "error", err)
			e.raise(ctx, autonomy.SystemException, autonomy.ReasonWorkflowFailure, "buyer_request", rid, &rid, map[string]any{"step": "broadened_routing", "error": err.Error()})
		}
	case autonomy.KindAskSeller:
		yn := YesNo(m.Text)
		if yn == 0 {
			e.notAnAnswer(ctx, m, c, cb)
			return
		}
		if e.closeBuyerClarification(ctx, c, autonomy.ClarAnswered, m, map[string]any{"ask_seller": yn > 0}) != nil {
			return
		}
		if yn < 0 {
			e.say(ctx, m.ConversationID, nil, "clarification_ack", "Okay 👍")
			return
		}
		if item, ok := itemOfContext(c.Context); ok {
			e.enquire(ctx, m, item)
		}
	default:
		e.notAnAnswer(ctx, m, c, cb)
	}
}

// notAnAnswer: the message does not answer the open question. A clear new
// request, or a question about something Majo showed, supersedes the
// question; otherwise Majo re-asks once, then lets the question go and takes
// the message down the ordinary path.
func (e *Engine) notAnAnswer(ctx context.Context, m BuyerText, c *autonomy.Clarification, cb Callbacks) {
	if c.Attempts >= 2 || looksLikeNewRequest(m.Text) || e.liveReference(ctx, m) {
		if e.closeBuyerClarification(ctx, c, autonomy.ClarSuperseded, m, map[string]any{"reason": "not_an_answer"}) != nil {
			return
		}
		if !e.handleReference(ctx, m, cb) {
			cb.Default(ctx)
		}
		return
	}
	q := "Sorry, I didn't catch that 🙏 " + c.Question
	if _, err := e.store.BumpClarification(ctx, c.ID, q, e.now().UTC().Add(e.cfg.Params.ClarificationTTL)); err != nil {
		cb.Default(ctx)
		return
	}
	e.say(ctx, m.ConversationID, c.BuyerRequestID, "clarification", q)
}

// looksLikeNewRequest: long enough and has request verbs/nouns.
func looksLikeNewRequest(text string) bool {
	t := strings.ToLower(text)
	return len(strings.Fields(t)) >= 3 && (strings.Contains(t, "need") || strings.Contains(t, "looking for") || strings.Contains(t, "want") ||
		strings.Contains(t, "buy") || strings.Contains(t, "find me"))
}

// answerMissingField merges the buyer's answer into the open request. The
// answer is tried FIRST ("I need it in Lekki" answers "which area?"); only a
// product question can be pre-empted by a question about something shown.
func (e *Engine) answerMissingField(ctx context.Context, m BuyerText, c *autonomy.Clarification, cb Callbacks) {
	if c.BuyerRequestID == nil {
		_ = e.closeBuyerClarification(ctx, c, autonomy.ClarSuperseded, m, map[string]any{"reason": "no_request"})
		cb.Default(ctx)
		return
	}
	req, err := e.reqRepo.FindByID(ctx, *c.BuyerRequestID)
	if err != nil || req == nil {
		_ = e.closeBuyerClarification(ctx, c, autonomy.ClarSuperseded, m, map[string]any{"reason": "request_missing"})
		cb.Default(ctx)
		return
	}
	if IsSkip(m.Text) {
		if e.closeBuyerClarification(ctx, c, autonomy.ClarAnswered, m, map[string]any{"skipped": true, "field": c.Field}) != nil {
			return
		}
		e.continueRequest(ctx, req, request.Fields{}, c, m)
		return
	}
	if c.Field == FieldProduct && e.liveReference(ctx, m) {
		e.notAnAnswer(ctx, m, c, cb)
		return
	}
	ex, xerr := e.extractor.Extract(ctx, ai.ExtractionRequest{Text: m.Text, ConversationID: m.ConversationID.String()})
	if xerr != nil {
		ex = &ai.ExtractionResult{}
	}
	// A different product while Majo asked about something else is a new
	// request, not an answer.
	if c.Field != FieldProduct && ex.Product != nil && strings.TrimSpace(deref(req.Product)) != "" {
		exact, family := matching.ProductRelevance(*ex.Product, deref(req.Product))
		if !exact && !family && looksLikeNewRequest(m.Text) {
			if e.closeBuyerClarification(ctx, c, autonomy.ClarSuperseded, m, map[string]any{"reason": "new_request"}) != nil {
				return
			}
			cb.Default(ctx)
			return
		}
	}
	patch, got := answerPatch(c.Field, m.Text, ex, req)
	if !got {
		if e.liveReference(ctx, m) {
			e.notAnAnswer(ctx, m, c, cb)
			return
		}
		if c.Attempts >= 2 {
			if e.closeBuyerClarification(ctx, c, autonomy.ClarExhausted, m, map[string]any{"field": c.Field}) != nil {
				return
			}
			e.continueRequest(ctx, req, patch, c, m)
			return
		}
		q := "Sorry, I didn't catch that 🙏 " + c.Question
		if _, err := e.store.BumpClarification(ctx, c.ID, q, e.now().UTC().Add(e.cfg.Params.ClarificationTTL)); err == nil {
			e.say(ctx, m.ConversationID, &req.ID, "clarification", q)
		}
		return
	}
	if e.closeBuyerClarification(ctx, c, autonomy.ClarAnswered, m, map[string]any{"field": c.Field}) != nil {
		return // a concurrent answer won
	}
	e.continueRequest(ctx, req, patch, c, m)
}

// continueRequest merges the answer under the request's lock. An answer
// that arrives after the request was confirmed (an operator overrode) never
// rewrites what vendors were already asked about.
func (e *Engine) continueRequest(ctx context.Context, req *request.BuyerRequest, patch request.Fields, c *autonomy.Clarification, m BuyerText) {
	err := e.store.WithLock(ctx, "request:"+req.ID.String(), func(ctx context.Context) error {
		cur, err := e.reqRepo.FindByID(ctx, req.ID)
		if err != nil || cur == nil {
			return err
		}
		if cur.Status == request.StatusConfirmed {
			e.decide(ctx, cur, autonomy.DecContinueRequest, autonomy.OutcomeRejected, autonomy.PolicyRejection, "answer_after_confirmation",
				map[string]any{"field": c.Field, "answer": m.Text}, nil, nil)
			return nil
		}
		if _, err := e.requests.ApplyClarification(ctx, req.ID, patch, map[string]any{"clarification_id": c.ID.String(), "field": c.Field,
			"answer_message_id": m.MessageID.String()}); err != nil {
			return err
		}
		e.decide(ctx, cur, autonomy.DecContinueRequest, autonomy.OutcomeApplied, "", "clarification_answered",
			map[string]any{"field": c.Field, "answer": m.Text}, nil, nil)
		return nil
	})
	if err != nil {
		e.logger.Error("applying clarification", "buyer_request_id", req.ID, "error", err)
		e.raise(ctx, autonomy.SystemException, autonomy.ReasonWorkflowFailure, "buyer_request", req.ID, &req.ID,
			map[string]any{"step": "apply_clarification", "error": err.Error()})
	}
}

// answerPatch builds the field update from an answer: the asked-about field
// (from extraction, else the short raw answer), plus any other field the
// answer states that the request lacks. got=false: the asked field was not
// understood.
func answerPatch(field, text string, ex *ai.ExtractionResult, req *request.BuyerRequest) (request.Fields, bool) {
	var p request.Fields
	raw := strings.TrimSpace(text)
	short := func(n int) *string {
		if l := len([]rune(raw)); l >= 2 && l <= n {
			s := raw
			return &s
		}
		return nil
	}
	got := false
	switch field {
	case FieldProduct:
		if ex.Product != nil {
			p.Product, got = ex.Product, true
		} else if s := short(60); s != nil {
			p.Product, got = s, true
		}
		if ex.Category != nil && req.Category == nil {
			p.Category = ex.Category
		}
		if ex.Variant != nil && req.Variant == nil {
			p.Variant = ex.Variant
		}
	case FieldLocation:
		if ex.Location != nil {
			p.Location, got = ex.Location, true
		} else if loc := placeFromAnswer(raw); loc != "" {
			p.Location, got = &loc, true
		}
	case FieldSpec:
		if ex.Variant != nil {
			p.Variant, got = ex.Variant, true
		} else if s := short(40); s != nil {
			p.Variant, got = s, true
		}
	case FieldBudget:
		if ex.BudgetMax != nil || ex.BudgetMin != nil {
			p.BudgetMax, p.BudgetMin, got = ex.BudgetMax, ex.BudgetMin, true
		}
	}
	if field == "" {
		// An operator's free-form question: whatever the answer states.
		p.Category, p.Product, p.Variant, p.Location, p.Condition, p.Quantity = ex.Category, ex.Product, ex.Variant, ex.Location, ex.Condition, ex.Quantity
		p.BudgetMin, p.BudgetMax = ex.BudgetMin, ex.BudgetMax
		got = ex.Product != nil || ex.Variant != nil || ex.Location != nil || ex.BudgetMax != nil || ex.BudgetMin != nil || ex.Condition != nil || ex.Quantity != nil
		return p, got
	}
	// Other facts volunteered in the same answer fill gaps only.
	if field != FieldLocation && ex.Location != nil && req.Location == nil {
		p.Location = ex.Location
	}
	if field != FieldBudget && (ex.BudgetMax != nil || ex.BudgetMin != nil) && req.BudgetMax == nil && req.BudgetMin == nil {
		p.BudgetMax, p.BudgetMin = ex.BudgetMax, ex.BudgetMin
	}
	if ex.Condition != nil && req.Condition == nil {
		p.Condition = ex.Condition
	}
	if ex.Quantity != nil && req.Quantity == nil {
		p.Quantity = ex.Quantity
	}
	return p, got
}

func (e *Engine) closeBuyerClarification(ctx context.Context, c *autonomy.Clarification, status string, m BuyerText, resolution map[string]any) error {
	msgID := m.MessageID
	if err := e.store.ResolveClarification(ctx, c.ID, status, &msgID, m.Text, resolution, e.now().UTC()); err != nil {
		return err
	}
	// The conversation's question slot is free: requests that were waiting
	// for it continue.
	defer e.readvanceConversation(ctx, m.ConversationID)
	cid := c.ID
	t := event.ClarificationAnswered
	if status != autonomy.ClarAnswered {
		t = event.ClarificationClosed
	}
	bid := m.BuyerID
	_ = e.events.Write(ctx, &event.Event{EventType: t, EntityType: "clarification", EntityID: &cid, ActorType: event.ActorBuyer, ActorID: &bid,
		Payload: event.Merge(map[string]any{"party": "BUYER", "kind": c.Kind, "field": c.Field, "status": status, "attempts": c.Attempts},
			resolution, idPayload("buyer_request_id", c.BuyerRequestID))})
	return nil
}

func idPayload(k string, id *uuid.UUID) map[string]any {
	if id == nil {
		return nil
	}
	return map[string]any{k: id.String()}
}

// ---- references to things already shown ----------------------------------------

func (e *Engine) handleReference(ctx context.Context, m BuyerText, cb Callbacks) bool {
	if !e.cfg.Flags.AutoClarifyRequests {
		return false // context resolution of buyer text is part of autonomous clarification
	}
	ref := ParseReference(m.Text)
	if ref.Intent == IntentNone {
		return false
	}
	bc, err := e.ctxStore.BuyerContext(ctx, m.ConversationID, m.BuyerID, e.now().Add(-e.cfg.Params.ContextWindow))
	if err != nil || bc == nil {
		return false
	}
	items := ResolveReference(ref, bc.Items)
	switch len(items) {
	case 0:
		return false // not about anything Majo showed: ordinary request
	case 1:
		e.recordReference(ctx, m, ref, items, "resolved")
		e.actOnItem(ctx, m, items[0], ref.Intent, cb)
		return true
	}
	if !e.cfg.Flags.AutoClarifyRequests {
		return false
	}
	q, opts := WhichOptions(items, ref.Intent)
	conv, bid := m.ConversationID, m.BuyerID
	c, err := e.store.OpenClarification(ctx, &autonomy.Clarification{Party: autonomy.PartyBuyer, ConversationID: &conv, BuyerID: &bid,
		Kind: autonomy.KindWhichItem, Question: q, Options: opts, Context: map[string]any{"intent": ref.Intent, "text": m.Text},
		Status: autonomy.ClarOpen, Attempts: 1, ExpiresAt: e.now().UTC().Add(e.cfg.Params.ClarificationTTL)})
	if err != nil {
		e.logger.Error("opening which-item clarification", "error", err)
		return false
	}
	e.recordReference(ctx, m, ref, items, "ambiguous:"+c.ID.String())
	e.say(ctx, m.ConversationID, nil, "clarification", q)
	return true
}

func (e *Engine) recordReference(ctx context.Context, m BuyerText, ref Reference, items []ContextItem, outcome string) {
	refs := make([]string, 0, len(items))
	for _, it := range items {
		refs = append(refs, it.Kind+":"+firstNonEmpty(it.Ref, it.Label()))
	}
	oc := autonomy.OutcomeApplied
	fb := autonomy.FallbackNone
	if strings.HasPrefix(outcome, "ambiguous") {
		oc, fb = autonomy.OutcomeDeferred, autonomy.ClarificationRequired
	}
	var brid *uuid.UUID
	if len(items) == 1 && items[0].BuyerRequestID != nil {
		brid = items[0].BuyerRequestID
	}
	if err := e.store.RecordDecision(ctx, &autonomy.Decision{WorkflowType: "conversation", SubjectType: "message", SubjectID: m.MessageID,
		BuyerRequestID: brid, Kind: autonomy.DecReferenceItem, Outcome: oc, Fallback: fb, Reason: outcome,
		Inputs: map[string]any{"text": m.Text, "intent": ref.Intent, "noun": ref.Noun, "attrs": keys(ref.Attrs)},
		Result: map[string]any{"items": refs}, Actor: "SYSTEM"}); err != nil {
		e.logger.Error("recording reference decision", "error", err)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// actOnItem carries out what the buyer asked about one resolved item.
func (e *Engine) actOnItem(ctx context.Context, m BuyerText, it ContextItem, intent string, cb Callbacks) {
	switch intent {
	case IntentPrice:
		e.answerPrice(ctx, m, it)
	case IntentCheaper:
		e.findCheaper(ctx, m, it)
	default: // want / availability
		switch it.Kind {
		case ItemListing:
			e.enquire(ctx, m, it)
		case ItemOffer:
			e.confirmOption(ctx, m, it)
		default:
			e.wantRequestItem(ctx, m, it)
		}
	}
}

func (e *Engine) answerPrice(ctx context.Context, m BuyerText, it ContextItem) {
	switch {
	case it.Kind == ItemOffer && it.Price != nil:
		e.reply(ctx, m, it, fmt.Sprintf("Option %d (%s) is ₦%s. Reply %d to choose it.", it.Ordinal, it.Label(), thousands(*it.Price), it.Ordinal))
	case it.Kind == ItemListing && it.Price != nil:
		e.reply(ctx, m, it, fmt.Sprintf("%s (%s) is listed at ₦%s. Want me to check with the seller that it's still available? (Yes / No)",
			it.Label(), it.Ref, thousands(*it.Price)))
		e.openItemQuestion(ctx, m, it, autonomy.KindAskSeller, "Want me to check with the seller that it's still available? (Yes / No)")
	case it.Kind == ItemListing:
		e.reply(ctx, m, it, fmt.Sprintf("The seller didn't list a price for %s (%s). Want me to ask them? (Yes / No)", it.Label(), it.Ref))
		e.openItemQuestion(ctx, m, it, autonomy.KindAskSeller, "Want me to ask the seller for the price? (Yes / No)")
	default:
		e.reply(ctx, m, it, "I don't have a price for that yet — I'll send options with prices as soon as sellers reply 👀")
	}
}

func (e *Engine) confirmOption(ctx context.Context, m BuyerText, it ContextItem) {
	if it.OfferStatus != "" && it.OfferStatus != "SENT" {
		e.reply(ctx, m, it, fmt.Sprintf("Sorry, option %d is no longer available. Tell me what you're looking for and I'll search again 👀", it.Ordinal))
		return
	}
	price := ""
	if it.Price != nil {
		price = ", ₦" + thousands(*it.Price)
	}
	q := fmt.Sprintf("Do you want option %d (%s%s)? Reply %d or YES to choose it.", it.Ordinal, it.Label(), price, it.Ordinal)
	e.openItemQuestion(ctx, m, it, autonomy.KindConfirmOption, q)
	e.reply(ctx, m, it, q)
}

func (e *Engine) openItemQuestion(ctx context.Context, m BuyerText, it ContextItem, kind, q string) {
	conv, bid := m.ConversationID, m.BuyerID
	ttl := e.cfg.Params.ClarificationTTL
	if kind == autonomy.KindConfirmOption && ttl > confirmOptionTTL {
		ttl = confirmOptionTTL // a "yes" hours later must not select anything
	}
	if _, err := e.store.OpenClarification(ctx, &autonomy.Clarification{Party: autonomy.PartyBuyer, ConversationID: &conv, BuyerID: &bid,
		BuyerRequestID: it.BuyerRequestID, Kind: kind, Question: q, Context: map[string]any{"item": it},
		Options: []autonomy.Option{{Key: "yes", Label: "Yes"}, {Key: "no", Label: "No"}},
		Status:  autonomy.ClarOpen, Attempts: 1, ExpiresAt: e.now().UTC().Add(ttl)}); err != nil {
		e.logger.Error("opening item clarification", "error", err)
	}
}

func (e *Engine) enquire(ctx context.Context, m BuyerText, it ContextItem) {
	if e.enquirer == nil || it.DeliveryID == nil || it.ListingID == nil ||
		!e.enquirer.EnquireFromContext(ctx, m.BuyerID, m.ConversationID, m.MessageID, m.ReplyTo, *it.DeliveryID, *it.ListingID) {
		e.reply(ctx, m, it, fmt.Sprintf("Sorry, %s is no longer available. Tell me what you're looking for and I'll search 👀", firstNonEmpty(it.Ref, it.Label())))
	}
}

// confirmOptionTTL bounds how long "Do you want option 2?" stays answerable.
const confirmOptionTTL = 2 * time.Hour

// findCheaper starts a new request for the same item below its price (the
// same BuyerRequest model; fields come from context, so no extraction).
func (e *Engine) findCheaper(ctx context.Context, m BuyerText, it ContextItem) {
	f := &request.Fields{}
	set := func(s string) *string {
		if s == "" {
			return nil
		}
		v := s
		return &v
	}
	f.Product, f.Variant, f.Condition, f.Category = set(it.Product), set(it.Variant), set(it.Condition), set(it.Category)
	if it.Price != nil {
		max := *it.Price - 1
		f.BudgetMax = &max
	}
	if it.Location != "" {
		f.Location = set(it.Location)
	}
	if f.Product == nil || it.Price == nil {
		e.reply(ctx, m, it, "Cheaper than what budget? Tell me roughly how much you want to spend and I'll search 👀")
		return
	}
	msgID := m.MessageID
	req, _, err := e.requests.Create(ctx, request.NewRequest{Origin: request.OriginMessage, ConversationID: m.ConversationID, BuyerID: m.BuyerID,
		OriginMessageID: &msgID, RawText: m.Text, Fields: f, Confidence: 0.9,
		ExtractedData: map[string]any{"source": "context_cheaper", "reference": firstNonEmpty(it.Ref, it.Label()), "reference_kind": it.Kind},
		EventExtra:    map[string]any{"derived_from": it.Kind + ":" + firstNonEmpty(it.Ref, it.Label())}})
	if err != nil {
		e.logger.Error("creating cheaper request", "error", err)
		e.reply(ctx, m, it, "Sorry, I couldn't start that search just now. Please try again in a moment.")
		return
	}
	text := fmt.Sprintf("Got it — I'll look for %s", it.Label())
	if it.Price != nil {
		text += " under ₦" + thousands(*it.Price)
	}
	e.say(ctx, m.ConversationID, &req.ID, "acknowledgement", text+" 👀")
}

// wantRequestItem: the buyer refers to something they asked for earlier.
func (e *Engine) wantRequestItem(ctx context.Context, m BuyerText, it ContextItem) {
	if it.BuyerRequestID != nil && (it.RequestStatus == string(request.StatusConfirmed) || it.RequestStatus == string(request.StatusAwaitingBuyer) ||
		it.RequestStatus == string(request.StatusNeedsReview) || it.RequestStatus == string(request.StatusExtracting)) {
		e.reply(ctx, m, it, fmt.Sprintf("I'm already on it for %s 👀 I'll send you options as soon as sellers reply.", it.Label()))
		return
	}
	e.findSimilar(ctx, m, it)
}

func (e *Engine) findSimilar(ctx context.Context, m BuyerText, it ContextItem) {
	set := func(s string) *string {
		if s == "" {
			return nil
		}
		v := s
		return &v
	}
	f := &request.Fields{Product: set(it.Product), Variant: set(it.Variant), Condition: set(it.Condition), Category: set(it.Category), Location: set(it.Location)}
	if f.Product == nil {
		e.reply(ctx, m, it, "Which item do you mean? Tell me and I'll search 👀")
		return
	}
	msgID := m.MessageID
	req, _, err := e.requests.Create(ctx, request.NewRequest{Origin: request.OriginMessage, ConversationID: m.ConversationID, BuyerID: m.BuyerID,
		OriginMessageID: &msgID, RawText: m.Text, Fields: f, Confidence: 0.9,
		ExtractedData: map[string]any{"source": "context_reference", "reference_kind": it.Kind}})
	if err != nil {
		e.logger.Error("creating request from reference", "error", err)
		return
	}
	e.say(ctx, m.ConversationID, &req.ID, "acknowledgement", fmt.Sprintf("Got it — I'm looking for %s again 👀", it.Label()))
}

func (e *Engine) reply(ctx context.Context, m BuyerText, it ContextItem, text string) {
	e.say(ctx, m.ConversationID, it.BuyerRequestID, "context_answer", text)
	mid := m.MessageID
	_ = e.events.Write(ctx, &event.Event{EventType: event.ContextAnswerSent, EntityType: "message", EntityID: &mid, ActorType: event.ActorSystem,
		Payload: event.Merge(map[string]any{"item_kind": it.Kind, "reference": it.Ref, "text": text}, idPayload("buyer_request_id", it.BuyerRequestID))})
}

func itemOf(o *autonomy.Option) (ContextItem, bool) {
	return itemOfContext(o.Value)
}

func itemOfContext(m map[string]any) (ContextItem, bool) {
	raw, ok := m["item"]
	if !ok {
		return ContextItem{}, false
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return ContextItem{}, false
	}
	var it ContextItem
	if err := json.Unmarshal(b, &it); err != nil {
		return ContextItem{}, false
	}
	return it, true
}

// OperatorClarify is the control-plane question: an operator asks the buyer
// something; Majo records it as an open clarification so the answer
// continues the same request (instead of becoming a new one).
func (e *Engine) OperatorClarify(ctx context.Context, requestID uuid.UUID, question string) error {
	req, err := e.reqRepo.FindByID(ctx, requestID)
	if err != nil {
		return err
	}
	if req == nil {
		return fmt.Errorf("buyer request %s not found", requestID)
	}
	if strings.TrimSpace(question) == "" {
		a := Assess(req, "", History{Asked: map[string]int{}, Skipped: map[string]bool{}}, e.cfg.Params)
		question = a.Question
		if question == "" {
			question = "Could you share a bit more detail so I can find the best options?"
		}
	}
	conv, bid, rid := req.ConversationID, req.BuyerID, req.ID
	c, err := e.store.OpenClarification(ctx, &autonomy.Clarification{Party: autonomy.PartyBuyer, ConversationID: &conv, BuyerID: &bid,
		BuyerRequestID: &rid, Kind: autonomy.KindMissingField, Question: question, Context: map[string]any{"asked_by": "operator"},
		Status: autonomy.ClarOpen, Attempts: 1, ExpiresAt: e.now().UTC().Add(e.cfg.Params.ClarificationTTL)})
	if err != nil {
		return err
	}
	if err := e.requests.ClarifyAs(ctx, requestID, question, event.ActorOperator, map[string]any{"clarification_id": c.ID.String()}); err != nil {
		_ = e.store.ResolveClarification(ctx, c.ID, autonomy.ClarCancelled, nil, "", map[string]any{"send_error": err.Error()}, e.now().UTC())
		return err
	}
	if req.Status == request.StatusNeedsReview {
		_, _ = e.requests.AwaitBuyer(ctx, requestID)
	}
	return nil
}

var placeLead = regexp.MustCompile(`(?i)^(?:.*\b(?:in|at|around|near|for)\s+)`)

// placeFromAnswer takes the place out of a free answer to "which area?":
// "I need it in Lekki" -> "Lekki", "around Ikeja abeg" -> "Ikeja abeg".
func placeFromAnswer(raw string) string {
	t := strings.TrimSpace(strings.TrimRight(raw, ".!?🙏 "))
	t = strings.TrimSpace(placeLead.ReplaceAllString(t, ""))
	for _, suffix := range []string{" abeg", " please", " pls", " o"} {
		t = strings.TrimSuffix(t, suffix)
	}
	if n := len([]rune(t)); n < 2 || n > 40 {
		return ""
	}
	return t
}
