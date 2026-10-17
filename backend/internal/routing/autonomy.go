package routing

// Phase 4C correction: the autonomous vendor-side runtime.
//
// A vendor reply goes: context resolution -> interpretation -> known
// request/listing context -> decision. Association precedence:
//
//  1. structured ID: the WhatsApp message the vendor replied to (quoted
//     context) or an explicit MM- reference
//  2. active conversation context: the vendor's open clarification question
//  3. the single (unexpired) open VendorRequest
//  4. semantic/contextual resolution: the reply names exactly one of the
//     open requests (product, variant or listing reference)
//  5. clarification: ask the vendor which request they mean
//  6. genuine exception: only after the vendor's answer is still ambiguous
//
// A reply is never silently attached to the wrong request: when two
// interpretations remain plausible Majo asks. An unclear reply ("Which one?",
// a bare price) is answered with a targeted question built from the request
// context instead of waiting for an operator.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/infrastructure/ai"
	"github.com/middleman/backend/internal/autonomy"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/deal/answer"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/matching"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/supply"
	"github.com/middleman/backend/internal/vendor"
)

// VendorAutonomyRepository is the extra persistence the vendor runtime needs.
type VendorAutonomyRepository interface {
	// ExpireStale moves this vendor's SENT requests sent before `before` to
	// EXPIRED and returns them.
	ExpireStale(ctx context.Context, vendorID uuid.UUID, before time.Time) ([]*VendorRequest, error)
	// ListRecentlyExpiredByVendor: requests that expired at or after since
	// whose buyer request is still confirmed (late replies).
	ListRecentlyExpiredByVendor(ctx context.Context, vendorID uuid.UUID, since time.Time) ([]*VendorRequest, error)
}

// QuotedLookup resolves the business entity an outbound message was about
// from its provider message id (the recording sender stores it).
type QuotedLookup interface {
	OutboundEntityByProviderID(ctx context.Context, providerMessageID string) (entityType string, entityID *uuid.UUID, err error)
}

// VendorAutonomy configures the autonomous vendor runtime.
type VendorAutonomy struct {
	Flags  autonomy.Flags
	Params autonomy.Params
	Store  autonomy.Store
	Repo   VendorAutonomyRepository
	Quoted QuotedLookup
	Now    func() time.Time
	// Paused (pilot batch item 4): while all automation is paused Majo asks
	// vendors nothing; their replies are recorded for the operator.
	Paused func(ctx context.Context) bool
}

// VendorDealAnswers records a vendor's "did you sell?" answer (pilot batch
// item 5; deal.Service implements it).
type VendorDealAnswers interface {
	VendorAnswered(ctx context.Context, dealID uuid.UUID, ans string) error
}

// SetDeals wires the deal follow-up.
func (s *Service) SetDeals(d VendorDealAnswers) { s.deals = d }

// answerVendorDeal: yes / no / not yet answers the question. A substantive
// message (a reply to a buyer request, new stock) is not swallowed: it takes
// the normal path and the question stays open. Asked again once, then left.
func (s *Service) answerVendorDeal(ctx context.Context, v *vendor.Vendor, c *autonomy.Clarification, rawText string, ref supply.Inbound) bool {
	id, err := uuid.Parse(fmt.Sprint(c.Context["deal_id"]))
	if err != nil || s.deals == nil {
		return false
	}
	ans := answer.Parse(rawText)
	if ans == "" {
		if !isShortAnswer(rawText) || strings.ContainsAny(rawText, "0123456789₦") {
			return false // a reply of its own (stock, a price): normal path
		}
		if c.Attempts >= 2 {
			s.closeVendorClarification(ctx, c, autonomy.ClarExhausted, rawText, ref, map[string]any{"reason": "no_answer"})
			return true
		}
		q := "Sorry, I didn't catch that 🙏 " + c.Question
		if _, err := s.auto.Store.BumpClarification(ctx, c.ID, q, s.auto.Now().UTC().Add(s.auto.Params.ClarificationTTL)); err == nil {
			s.sendVendor(ctx, v, q, c, nil)
		}
		return true
	}
	s.closeVendorClarification(ctx, c, autonomy.ClarAnswered, rawText, ref, map[string]any{"answer": ans})
	if err := s.deals.VendorAnswered(ctx, id, ans); err != nil {
		s.logger.Error("recording vendor deal answer", "deal_id", id, "error", err)
	}
	return true
}

// pausedNow: all automation is paused by an operator.
func (s *Service) pausedNow(ctx context.Context) bool {
	return s.auto != nil && s.auto.Paused != nil && s.auto.Paused(ctx)
}

// SetAutonomy wires the autonomous vendor runtime (nil = legacy behaviour).
func (s *Service) SetAutonomy(a *VendorAutonomy) {
	if a != nil && a.Now == nil {
		a.Now = time.Now
	}
	s.auto = a
}

func (s *Service) autoOn() bool { return s.auto != nil && s.auto.Flags.AutoResolveVendorContext }

// Association rules recorded on the response and the decision log.
const (
	AssocQuoted        = "quoted_message"
	AssocReference     = "mm_reference"
	AssocClarification = "vendor_clarification_answer"
	AssocSingleActive  = "single_open_request"
	AssocContextMatch  = "context_match"
	AssocLateReply     = "late_reply_after_expiry"
)

// processVendorTextAutonomous is ProcessVendorMessageFrom with the
// autonomous association precedence. It returns the persisted response (nil
// when the text was supply or a clarification answer that only associated a
// previous reply).
func (s *Service) processVendorTextAutonomous(ctx context.Context, v *vendor.Vendor, ref supply.Inbound, rawText string) *VendorResponse {
	fallback := func(ctx context.Context) *VendorResponse { return s.processAsVendorResponse(ctx, v, nil, rawText) }
	return s.withVendorLock(ctx, v.ID, func(ctx context.Context) *VendorResponse {
		now := s.auto.Now().UTC()
		s.expireStale(ctx, v, now)

		// A structured pointer (quoted message / MM reference) to one of the
		// requests Majo just listed in "which request?" answers that question.
		if oc, err := s.auto.Store.OpenVendorClarification(ctx, v.ID, now); err == nil && oc != nil && oc.Kind == autonomy.KindVendorWhichRequest {
			target := s.quotedVendorRequest(ctx, v, ref.QuotedProviderMessageID)
			if target == nil {
				if m := mmReferencePattern.FindStringSubmatch(rawText); m != nil {
					if t, err := s.routing.FindVendorRequestByPublicReference(ctx, "MM-"+m[1]); err == nil && t != nil && t.VendorID == v.ID {
						target = t
					}
				}
			}
			if target != nil {
				for i := range oc.Options {
					if fmt.Sprint(oc.Options[i].Value["vendor_request_id"]) == target.ID.String() {
						if isShortAnswer(rawText) {
							return s.resolveWhichRequest(ctx, v, oc, target, rawText, ref, oc.Options[i].Key)
						}
						// A full reply quoting the request answers afresh: the
						// earlier ambiguous reply is filed under that request as
						// history (no offer from it), the new one is the reply.
						s.closeVendorClarification(ctx, oc, autonomy.ClarAnswered, rawText, ref,
							map[string]any{"vendor_request_id": target.ID.String(), "option": oc.Options[i].Key, "superseded_by_reply": true})
						if oc.VendorResponseID != nil {
							if _, _, err := s.routing.UpdateAssociation(ctx, *oc.VendorResponseID, target.ID); err == nil {
								rid := *oc.VendorResponseID
								_ = s.events.Write(ctx, &event.Event{EventType: event.VendorResponseAssociated, EntityType: "vendor_response", EntityID: &rid,
									ActorType: event.ActorSystem, Payload: map[string]any{"vendor_request_id": target.ID.String(), "rule": "superseded_by_later_reply"}})
							}
						}
						return s.respond(ctx, v, target, rawText, AssocQuoted, nil)
					}
				}
			}
		}

		// 1. Structured: the quoted message, then an explicit MM reference.
		if vr := s.quotedVendorRequest(ctx, v, ref.QuotedProviderMessageID); vr != nil {
			return s.answerOrRespond(ctx, v, vr, rawText, AssocQuoted)
		}
		if m := mmReferencePattern.FindStringSubmatch(rawText); m != nil {
			referenced, err := s.routing.FindVendorRequestByPublicReference(ctx, "MM-"+m[1])
			if err == nil && referenced != nil && referenced.VendorID == v.ID {
				return s.answerOrRespond(ctx, v, referenced, rawText, AssocReference)
			}
			// Unknown / foreign reference: never guess (PRD 2.1 §4).
			return s.processAsVendorResponse(ctx, v, nil, rawText)
		}

		// 2. The vendor's open clarification question.
		if c, err := s.auto.Store.OpenVendorClarification(ctx, v.ID, now); err != nil {
			s.logger.Error("loading vendor clarification", "vendor_id", v.ID, "error", err)
		} else if c != nil {
			if resp, handled := s.answerVendorClarification(ctx, v, c, rawText, ref); handled {
				return resp
			}
		}

		active, err := s.routing.ListActiveByVendor(ctx, v.ID)
		if err != nil {
			s.logger.Error("listing active vendor requests", "vendor_id", v.ID, "error", err)
			return nil
		}
		// A reply names a product none of the open requests is about (and a
		// price): that is the vendor advertising other stock — supply, never
		// a reply attached to the wrong request.
		var pre *ai.VendorResponseExtractionResult
		var preErr error
		if len(active) > 0 {
			pre, preErr = s.aiProvider.ExtractVendorResponse(ctx, rawText)
			if preErr == nil && pre != nil && pre.Product != nil && pre.Price != nil && !s.relevantToAny(ctx, active, *pre.Product) {
				s.recordVendorDecision(ctx, v.ID, nil, autonomy.DecInterpretVendorReply, autonomy.OutcomeApplied, "", "unrelated_product_is_supply",
					map[string]any{"product": *pre.Product, "open_requests": len(active)})
				if s.intake != nil && ref.MessageID != uuid.Nil {
					s.submitTextSupply(ctx, v, ref, rawText)
					return nil
				}
				s.processAsSupplySubmission(ctx, v, rawText)
				return nil
			}
		}
		switch len(active) {
		case 0:
			// A late reply that clearly names one request that expired
			// recently (its buyer still waiting) is still that reply: the
			// buyer gets the offer. Anything less clear stays supply.
			if late, result, xerr := s.lateReplyTarget(ctx, v, rawText, now); late != nil {
				return s.respondClassified(ctx, v, late, rawText, result, xerr, AssocLateReply, map[string]any{"late": true})
			}
			// No open request: supply submission (PRD 2 §39-40, unchanged).
			if s.intake != nil && ref.MessageID != uuid.Nil {
				s.submitTextSupply(ctx, v, ref, rawText)
				return nil
			}
			s.processAsSupplySubmission(ctx, v, rawText)
			return nil
		case 1:
			// 3. One open request.
			return s.respondClassified(ctx, v, active[0], rawText, pre, preErr, AssocSingleActive, nil)
		}

		// 4. Several open requests: does the reply name exactly one?
		result, xerr := pre, preErr
		if pick := s.pickByContext(ctx, active, rawText, result); pick != nil {
			return s.respondClassified(ctx, v, pick, rawText, result, xerr, AssocContextMatch, nil)
		}
		// 5. Ask the vendor which request (the reply is kept, unmatched).
		resp := s.persistVendorResponse(ctx, v, nil, rawText, result, xerr, map[string]any{"association": "pending_vendor_clarification"})
		if resp != nil {
			s.askWhichRequest(ctx, v, active, resp)
		}
		return resp
	}, fallback)
}

func (s *Service) withVendorLock(ctx context.Context, vendorID uuid.UUID, fn func(ctx context.Context) *VendorResponse, fallback func(ctx context.Context) *VendorResponse) *VendorResponse {
	var out *VendorResponse
	ran := false
	if err := s.auto.Store.WithLock(ctx, "vendor:"+vendorID.String(), func(ctx context.Context) error {
		ran = true
		out = fn(ctx)
		return nil
	}); err != nil && !ran {
		// Never drop a vendor's message: keep it unmatched and tell the control plane.
		s.logger.Error("vendor workflow lock", "vendor_id", vendorID, "error", err)
		if fallback != nil {
			out = fallback(ctx)
		}
		if out != nil {
			if e, rerr := s.auto.Store.RaiseException(ctx, &autonomy.Exception{Class: autonomy.SystemException, Reason: autonomy.ReasonWorkflowFailure,
				SubjectType: "vendor_response", SubjectID: out.ID, Detail: map[string]any{"step": "vendor_lock", "error": err.Error()}}); rerr == nil {
				id := e.ID
				_ = s.events.Write(ctx, &event.Event{EventType: event.WorkflowExceptionRaised, EntityType: "workflow_exception", EntityID: &id,
					ActorType: event.ActorSystem, Payload: map[string]any{"reason": autonomy.ReasonWorkflowFailure, "step": "vendor_lock"}})
			}
		}
	}
	return out
}

// expireStale: a SENT request older than VENDOR_REQUEST_TTL no longer counts
// as awaiting a reply (it would make every later message ambiguous forever).
func (s *Service) expireStale(ctx context.Context, v *vendor.Vendor, now time.Time) {
	if s.auto.Repo == nil {
		return
	}
	expired, err := s.auto.Repo.ExpireStale(ctx, v.ID, now.Add(-s.auto.Params.VendorRequestTTL))
	if err != nil {
		s.logger.Error("expiring stale vendor requests", "vendor_id", v.ID, "error", err)
		return
	}
	for _, vr := range expired {
		id := vr.ID
		_ = s.events.Write(ctx, &event.Event{EventType: event.VendorRequestExpired, EntityType: "vendor_request", EntityID: &id,
			ActorType: event.ActorSystem, Payload: map[string]any{"buyer_request_id": vr.BuyerRequestID.String(), "vendor_id": v.ID.String(),
				"reference": vr.PublicReference, "ttl": s.auto.Params.VendorRequestTTL.String(), "detected_by": "vendor_message"}})
		if s.lifecycle != nil {
			s.lifecycle.VendorRequestExpired(ctx, vr)
		}
	}
}

// lateReplyTarget: the one recently expired request (within
// AUTO_LATE_REPLY_WINDOW after its expiry) this reply names by product or
// listing reference; nil when none or several fit.
func (s *Service) lateReplyTarget(ctx context.Context, v *vendor.Vendor, rawText string, now time.Time) (*VendorRequest, *ai.VendorResponseExtractionResult, error) {
	if s.auto.Repo == nil {
		return nil, nil, nil
	}
	window := s.auto.Params.LateReplyWindow
	if window <= 0 {
		window = s.auto.Params.VendorRequestTTL
	}
	expired, err := s.auto.Repo.ListRecentlyExpiredByVendor(ctx, v.ID, now.Add(-window))
	if err != nil || len(expired) == 0 {
		return nil, nil, nil
	}
	result, xerr := s.aiProvider.ExtractVendorResponse(ctx, rawText)
	if xerr != nil || result == nil || result.Product == nil {
		return nil, nil, nil // an unnamed late message is not attached to an old request
	}
	pick := s.pickByContext(ctx, expired, rawText, result)
	if pick == nil || !s.relevantToAny(ctx, []*VendorRequest{pick}, *result.Product) {
		return nil, nil, nil
	}
	return pick, result, xerr
}

// quotedVendorRequest maps a WhatsApp reply-to (quoted) message onto the
// vendor request it was about, when it belongs to this vendor.
func (s *Service) quotedVendorRequest(ctx context.Context, v *vendor.Vendor, providerID string) *VendorRequest {
	if providerID == "" || s.auto.Quoted == nil {
		return nil
	}
	et, id, err := s.auto.Quoted.OutboundEntityByProviderID(ctx, providerID)
	if err != nil || id == nil || et != "vendor_request" {
		return nil
	}
	vr, err := s.routing.FindVendorRequestByID(ctx, *id)
	if err != nil || vr == nil || vr.VendorID != v.ID {
		return nil
	}
	return vr
}

// answerOrRespond: a structurally-associated reply. If Majo had asked this
// vendor a detail question about that same request, the reply answers it.
func (s *Service) answerOrRespond(ctx context.Context, v *vendor.Vendor, vr *VendorRequest, rawText, rule string) *VendorResponse {
	now := s.auto.Now().UTC()
	if c, err := s.auto.Store.OpenVendorClarification(ctx, v.ID, now); err == nil && c != nil && c.VendorRequestID != nil && *c.VendorRequestID == vr.ID && c.Kind == autonomy.KindVendorDetail {
		if resp, handled := s.answerVendorClarification(ctx, v, c, rawText, supply.Inbound{}); handled {
			return resp
		}
	}
	return s.respond(ctx, v, vr, rawText, rule, nil)
}

func (s *Service) respond(ctx context.Context, v *vendor.Vendor, vr *VendorRequest, rawText, rule string, extra map[string]any) *VendorResponse {
	result, err := s.aiProvider.ExtractVendorResponse(ctx, rawText)
	return s.respondClassified(ctx, v, vr, rawText, result, err, rule, extra)
}

// respondClassified persists a reply on vr, notifies the offer loop, and asks
// the vendor a targeted question when the reply is unclear.
func (s *Service) respondClassified(ctx context.Context, v *vendor.Vendor, vr *VendorRequest, rawText string, result *ai.VendorResponseExtractionResult, xerr error, rule string, extra map[string]any) *VendorResponse {
	ex := map[string]any{"association": rule}
	for k, val := range extra {
		ex[k] = val
	}
	resp := s.persistVendorResponse(ctx, v, vr, rawText, result, xerr, ex)
	if resp == nil {
		return nil
	}
	s.decide(ctx, vr, autonomy.DecAssociateVendorReply, autonomy.OutcomeApplied, "", rule, resp)
	if resp.ReviewState == ReviewNeedsReview && resp.Status == ResponseUnclassified && s.auto.Flags.AutoClarifyVendorResponses {
		s.askVendorDetail(ctx, v, vr, resp, 1)
		return resp
	}
	s.notifyResponseChanged(ctx, resp)
	return resp
}

// answerVendorClarification handles a vendor message while Majo is waiting
// for the answer to its question. handled=false: the message is not an
// answer (e.g. clearly new supply) and continues down the normal path.
func (s *Service) answerVendorClarification(ctx context.Context, v *vendor.Vendor, c *autonomy.Clarification, rawText string, ref supply.Inbound) (*VendorResponse, bool) {
	now := s.auto.Now().UTC()
	switch c.Kind {
	case answer.KindVendorConfirm:
		return nil, s.answerVendorDeal(ctx, v, c, rawText, ref)
	case autonomy.KindVendorWhichRequest:
		opt := MatchVendorOption(rawText, c.Options)
		result, _ := s.aiProvider.ExtractVendorResponse(ctx, rawText)
		substantive := result != nil && result.Status != string(ResponseUnclassified)
		// Only a short pointer ("2", "the ps5", "MM-123456") answers the
		// question; a substantive message is a reply of its own and takes the
		// normal path (never swallowed as an answer, never lost).
		if opt != nil && (isShortAnswer(rawText) || !substantive) {
			vrID, _ := uuid.Parse(fmt.Sprint(opt.Value["vendor_request_id"]))
			vr, err := s.routing.FindVendorRequestByID(ctx, vrID)
			if err != nil || vr == nil || vr.VendorID != v.ID {
				return nil, false
			}
			return s.resolveWhichRequest(ctx, v, c, vr, rawText, ref, opt.Key), true
		}
		if substantive {
			return nil, false
		}
		if c.Attempts >= s.auto.Params.VendorClarifyMaxRounds {
			s.closeVendorClarification(ctx, c, autonomy.ClarExhausted, rawText, ref, map[string]any{"reason": "no_option_matched"})
			s.raiseVendorException(ctx, c, "vendor_association_unresolved")
			return nil, true
		}
		q := "Sorry, I didn't catch which one. " + c.Question
		if _, err := s.auto.Store.BumpClarification(ctx, c.ID, q, now.Add(s.auto.Params.ClarificationTTL)); err == nil {
			s.sendVendor(ctx, v, q, c, nil)
		}
		return nil, true

	case autonomy.KindVendorDetail:
		if c.VendorRequestID == nil {
			return nil, false
		}
		vr, err := s.routing.FindVendorRequestByID(ctx, *c.VendorRequestID)
		if err != nil || vr == nil || vr.VendorID != v.ID {
			return nil, false
		}
		result, xerr := s.aiProvider.ExtractVendorResponse(ctx, rawText)
		// Never attach a message about something else to the request Majo
		// asked about: a different product, or text naming another open
		// request, takes the normal path.
		if !s.aboutSameRequest(ctx, v, vr, rawText, result) {
			return nil, false
		}
		if xerr == nil && result != nil && result.Status == string(ResponseAvailable) && result.Price == nil {
			// "Yes" to "Is it available at ₦620,000?" carries the price asked about.
			if p, ok := c.Context["price"]; ok {
				if f, ok := toInt64(p); ok {
					result.Price = &f
				}
			}
		}
		resp := s.persistVendorResponse(ctx, v, vr, rawText, result, xerr, map[string]any{"association": AssocClarification, "clarification_id": c.ID.String()})
		if resp == nil {
			return nil, true
		}
		if resp.Status == ResponseUnclassified {
			if c.Attempts >= s.auto.Params.VendorClarifyMaxRounds {
				s.closeVendorClarification(ctx, c, autonomy.ClarExhausted, rawText, ref, map[string]any{"vendor_response_id": resp.ID.String()})
				s.raiseVendorException(ctx, c, "vendor_response_unresolved")
				return resp, true
			}
			s.askVendorDetail(ctx, v, vr, resp, c.Attempts+1)
			return resp, true
		}
		s.closeVendorClarification(ctx, c, autonomy.ClarAnswered, rawText, ref, map[string]any{"vendor_response_id": resp.ID.String(), "status": string(resp.Status)})
		s.decide(ctx, vr, autonomy.DecInterpretVendorReply, autonomy.OutcomeApplied, "", "clarified", resp)
		s.notifyResponseChanged(ctx, resp)
		return resp, true
	}
	return nil, false
}

// resolveWhichRequest settles an open "which request?" question with vr: the
// pending reply is attached to vr (vendor identity checked) and handled.
func (s *Service) resolveWhichRequest(ctx context.Context, v *vendor.Vendor, c *autonomy.Clarification, vr *VendorRequest, rawText string, ref supply.Inbound, key string) *VendorResponse {
	s.closeVendorClarification(ctx, c, autonomy.ClarAnswered, rawText, ref, map[string]any{"vendor_request_id": vr.ID.String(), "option": key})
	if c.VendorResponseID == nil {
		return nil
	}
	resp, err := s.associateAs(ctx, *c.VendorResponseID, vr, AssocClarification)
	if err != nil {
		s.logger.Error("associating clarified vendor response", "error", err)
		return nil
	}
	if resp.ReviewState == ReviewNeedsReview && resp.Status == ResponseUnclassified && s.auto.Flags.AutoClarifyVendorResponses {
		s.askVendorDetail(ctx, v, vr, resp, 1)
	} else {
		s.notifyResponseChanged(ctx, resp)
	}
	return resp
}

// relevantToAny: product is what at least one of the requests asks about.
func (s *Service) relevantToAny(ctx context.Context, reqs []*VendorRequest, product string) bool {
	for _, vr := range reqs {
		req, err := s.requests.FindByID(ctx, vr.BuyerRequestID)
		if err != nil || req == nil || req.Product == nil {
			return true // unknown: do not treat as unrelated
		}
		if exact, family := matching.ProductRelevance(product, *req.Product, optStr(req.Variant)); exact || family {
			return true
		}
	}
	return false
}

// aboutSameRequest: the vendor's message can be the answer about vr — it
// names no different product and no other open request.
func (s *Service) aboutSameRequest(ctx context.Context, v *vendor.Vendor, vr *VendorRequest, rawText string, result *ai.VendorResponseExtractionResult) bool {
	req, err := s.requests.FindByID(ctx, vr.BuyerRequestID)
	if err != nil || req == nil {
		return false
	}
	if result != nil && result.Product != nil && req.Product != nil {
		exact, family := matching.ProductRelevance(*result.Product, *req.Product, optStr(req.Variant))
		if !exact && !family {
			return false
		}
	}
	active, err := s.routing.ListActiveByVendor(ctx, v.ID)
	if err != nil {
		return false
	}
	if len(active) > 0 {
		if pick := s.pickByContext(ctx, append(active, vr), rawText, result); pick != nil && pick.ID != vr.ID {
			return false
		}
	}
	return true
}

var shortWords = regexp.MustCompile(`\S+`)

// isShortAnswer: a pointer, not a reply ("2", "the ps5", "MM-123456 abeg").
func isShortAnswer(text string) bool { return len(shortWords.FindAllString(text, -1)) <= 4 }

func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		return int64(x), true
	case int64:
		return x, true
	case int:
		return int64(x), true
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		return n, err == nil
	}
	return 0, false
}

// pickByContext returns the one open request the reply names (by listing
// reference, product or variant), or nil when zero or several fit.
func (s *Service) pickByContext(ctx context.Context, active []*VendorRequest, rawText string, result *ai.VendorResponseExtractionResult) *VendorRequest {
	text := rawText
	if result != nil {
		if result.Product != nil {
			text += " " + *result.Product
		}
		if result.Variant != nil {
			text += " " + *result.Variant
		}
	}
	tokens := Tokens(text)
	best, bestScore, tie := (*VendorRequest)(nil), 0, false
	for _, vr := range active {
		score := 0
		if vr.SupplyListingID != nil && s.listings != nil {
			if l, err := s.listings.FindByID(ctx, *vr.SupplyListingID); err == nil && l != nil && strings.Contains(strings.ToUpper(rawText), strings.ToUpper(l.PublicReference)) {
				score += 100
			}
		}
		if req, err := s.requests.FindByID(ctx, vr.BuyerRequestID); err == nil && req != nil {
			score += TokenOverlap(tokens, derefStr(req.Product)+" "+optStr(req.Variant))
		}
		switch {
		case score > bestScore:
			best, bestScore, tie = vr, score, false
		case score == bestScore && score > 0:
			tie = true
		}
	}
	if best == nil || tie {
		return nil
	}
	return best
}

func optStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// askWhichRequest asks the vendor which open request an unmatched reply is
// about (numbered options, newest first, at most 5).
func (s *Service) askWhichRequest(ctx context.Context, v *vendor.Vendor, active []*VendorRequest, resp *VendorResponse) {
	if !s.auto.Flags.AutoResolveVendorContext {
		return
	}
	if s.pausedNow(ctx) {
		s.recordVendorDecision(ctx, resp.ID, nil, autonomy.DecAskVendorClarification, autonomy.OutcomeDeferred, "",
			"automation_paused", map[string]any{"open_requests": len(active)})
		return
	}
	list := append([]*VendorRequest{}, active...)
	sort.SliceStable(list, func(i, j int) bool { return timeOr(list[i].SentAt).After(timeOr(list[j].SentAt)) })
	if len(list) > 5 {
		list = list[:5]
	}
	var b strings.Builder
	b.WriteString("Thanks! Which request is that for?\n")
	var opts []autonomy.Option
	for i, vr := range list {
		label := vr.PublicReference
		if req, err := s.requests.FindByID(ctx, vr.BuyerRequestID); err == nil && req != nil {
			label = strings.TrimSpace(derefStr(req.Product)+" "+optStr(req.Variant)) + " (" + vr.PublicReference + ")"
		}
		key := strconv.Itoa(i + 1)
		fmt.Fprintf(&b, "\n%s. %s", key, label)
		opts = append(opts, autonomy.Option{Key: key, Label: label, Ref: vr.PublicReference,
			Value: map[string]any{"vendor_request_id": vr.ID.String(), "label": label}})
	}
	b.WriteString("\n\nReply with the number.")
	vid, rid := v.ID, resp.ID
	c := &autonomy.Clarification{Party: autonomy.PartyVendor, VendorID: &vid, VendorResponseID: &rid,
		Kind: autonomy.KindVendorWhichRequest, Question: b.String(), Options: opts, Status: autonomy.ClarOpen, Attempts: 1,
		ExpiresAt: s.auto.Now().UTC().Add(s.auto.Params.ClarificationTTL)}
	saved, err := s.openVendorClarification(ctx, c)
	if err != nil {
		s.logger.Error("opening vendor clarification", "vendor_id", v.ID, "error", err)
		return
	}
	s.recordVendorDecision(ctx, resp.ID, nil, autonomy.DecAskVendorClarification, autonomy.OutcomeDeferred, autonomy.ClarificationRequired,
		"several_open_requests", map[string]any{"open_requests": len(active), "clarification_id": saved.ID.String()})
	s.sendVendor(ctx, v, b.String(), saved, nil)
}

// askVendorDetail asks a targeted question about an unclear reply, built
// from the request (or listing) context.
func (s *Service) askVendorDetail(ctx context.Context, v *vendor.Vendor, vr *VendorRequest, resp *VendorResponse, attempt int) {
	if attempt > s.auto.Params.VendorClarifyMaxRounds {
		return
	}
	if s.pausedNow(ctx) {
		brid := vr.BuyerRequestID
		s.recordVendorDecision(ctx, resp.ID, &brid, autonomy.DecAskVendorClarification, autonomy.OutcomeDeferred, "",
			"automation_paused", map[string]any{"attempt": attempt, "vendor_request_id": vr.ID.String()})
		return
	}
	item := "the item"
	if req, err := s.requests.FindByID(ctx, vr.BuyerRequestID); err == nil && req != nil {
		item = strings.TrimSpace(derefStr(req.Product) + " " + optStr(req.Variant))
		if req.Condition != nil {
			item += " (" + *req.Condition + ")"
		}
	}
	q, cctx := VendorDetailQuestion(resp.RawMessage, item, resp.Price)
	q = vr.PublicReference + ": " + q
	vid, rid, vrid := v.ID, resp.ID, vr.ID
	brid := vr.BuyerRequestID
	c := &autonomy.Clarification{Party: autonomy.PartyVendor, VendorID: &vid, VendorRequestID: &vrid, VendorResponseID: &rid, BuyerRequestID: &brid,
		Kind: autonomy.KindVendorDetail, Question: q, Context: cctx, Status: autonomy.ClarOpen, Attempts: attempt,
		ExpiresAt: s.auto.Now().UTC().Add(s.auto.Params.ClarificationTTL)}
	saved, err := s.openVendorClarification(ctx, c)
	if err != nil {
		s.logger.Error("opening vendor detail clarification", "vendor_id", v.ID, "error", err)
		return
	}
	s.decide(ctx, vr, autonomy.DecAskVendorClarification, autonomy.OutcomeDeferred, autonomy.ClarificationRequired, "unclear_reply", resp)
	s.sendVendor(ctx, v, q, saved, &vrid)
}

// openVendorClarification opens c. A newer question supersedes the vendor's
// open one; if that was "which request?" with a reply still waiting, the
// reply would be orphaned — so it becomes an exception instead.
func (s *Service) openVendorClarification(ctx context.Context, c *autonomy.Clarification) (*autonomy.Clarification, error) {
	if prev, err := s.auto.Store.OpenVendorClarification(ctx, *c.VendorID, s.auto.Now().UTC()); err == nil && prev != nil &&
		prev.Kind == autonomy.KindVendorWhichRequest && prev.VendorResponseID != nil {
		defer s.raiseVendorException(ctx, prev, "vendor_association_unresolved")
	}
	return s.auto.Store.OpenClarification(ctx, c)
}

var questionPattern = regexp.MustCompile(`(?i)\?|\b(which|what|how much|how many|wetin|which one|abeg which)\b`)

// VendorDetailQuestion builds the clarifying question for an unclear vendor
// reply and the context needed to interpret the answer.
func VendorDetailQuestion(raw, item string, price *int64) (string, map[string]any) {
	cctx := map[string]any{"item": item}
	switch {
	case questionPattern.MatchString(raw):
		return fmt.Sprintf("The buyer is asking for %s. Do you have that exact item? Reply YES with your price, or NO.", item), cctx
	case price != nil:
		cctx["price"] = *price
		return fmt.Sprintf("Is the %s available at ₦%s? Reply YES or NO.", item, formatThousands(*price)), cctx
	default:
		return fmt.Sprintf("Sorry, I didn't quite get that. Do you have %s available? Reply YES with your price, or NO.", item), cctx
	}
}

func formatThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	var out []byte
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	return string(out)
}

func (s *Service) sendVendor(ctx context.Context, v *vendor.Vendor, text string, c *autonomy.Clarification, vrID *uuid.UUID) {
	entityType, entityID := "vendor", &v.ID
	if vrID != nil {
		entityType, entityID = "vendor_request", vrID
	}
	sendCtx := conversation.WithOutboundPurpose(ctx, conversation.OutboundPurpose{
		Kind: "vendor_clarification", EntityType: entityType, EntityID: entityID,
		Extra: map[string]any{"clarification_id": c.ID.String(), "kind": c.Kind},
	})
	if err := s.messaging.SendText(sendCtx, v.WhatsAppExternalID, text); err != nil {
		s.logger.Error("sending vendor clarification", "vendor_id", v.ID, "error", err)
		return
	}
	cid := c.ID
	_ = s.events.Write(ctx, &event.Event{EventType: event.VendorClarificationSent, EntityType: "clarification", EntityID: &cid,
		ActorType: event.ActorSystem, Payload: map[string]any{"vendor_id": v.ID.String(), "kind": c.Kind, "attempt": c.Attempts, "question": text}})
}

func (s *Service) closeVendorClarification(ctx context.Context, c *autonomy.Clarification, status, answer string, ref supply.Inbound, resolution map[string]any) {
	var msgID *uuid.UUID
	if ref.MessageID != uuid.Nil {
		id := ref.MessageID
		msgID = &id
	}
	if err := s.auto.Store.ResolveClarification(ctx, c.ID, status, msgID, answer, resolution, s.auto.Now().UTC()); err != nil {
		s.logger.Warn("resolving vendor clarification", "clarification_id", c.ID, "error", err)
		return
	}
	cid := c.ID
	t := event.ClarificationAnswered
	if status != autonomy.ClarAnswered {
		t = event.ClarificationClosed
	}
	_ = s.events.Write(ctx, &event.Event{EventType: t, EntityType: "clarification", EntityID: &cid, ActorType: event.ActorVendor,
		Payload: event.Merge(map[string]any{"party": "VENDOR", "kind": c.Kind, "status": status, "attempts": c.Attempts}, resolution)})
}

func (s *Service) raiseVendorException(ctx context.Context, c *autonomy.Clarification, reason string) {
	subjectType, subjectID := "clarification", c.ID
	if c.VendorResponseID != nil {
		subjectType, subjectID = "vendor_response", *c.VendorResponseID
	}
	e, err := s.auto.Store.RaiseException(ctx, &autonomy.Exception{Class: autonomy.SafetyException, Reason: reason,
		SubjectType: subjectType, SubjectID: subjectID, BuyerRequestID: c.BuyerRequestID,
		Detail: map[string]any{"clarification_id": c.ID.String(), "kind": c.Kind, "attempts": c.Attempts,
			"why": "the vendor's answers stayed ambiguous; attaching or classifying the reply would be a guess"}})
	if err != nil {
		s.logger.Error("raising vendor exception", "error", err)
		return
	}
	id := e.ID
	_ = s.events.Write(ctx, &event.Event{EventType: event.WorkflowExceptionRaised, EntityType: "workflow_exception", EntityID: &id,
		ActorType: event.ActorSystem, Payload: map[string]any{"class": string(e.Class), "reason": reason, "subject_type": subjectType, "subject_id": subjectID.String()}})
}

// associateAs attaches an unmatched response to vr as the SYSTEM (vendor
// identity is still authoritative), with an event naming the rule used.
func (s *Service) associateAs(ctx context.Context, responseID uuid.UUID, vr *VendorRequest, rule string) (*VendorResponse, error) {
	resp, err := s.routing.FindResponseByID(ctx, responseID)
	if err != nil || resp == nil {
		return nil, fmt.Errorf("loading vendor response: %v", err)
	}
	if resp.VendorID != vr.VendorID {
		return nil, ErrCrossVendorAssociation
	}
	_, updated, err := s.routing.UpdateAssociation(ctx, responseID, vr.ID)
	if err != nil {
		return nil, err
	}
	// A confident classification no longer needs review once associated.
	if updated.Status != ResponseUnclassified && updated.ReviewState == ReviewNeedsReview {
		rs := ReviewReviewed
		if u, err := s.routing.UpdateVendorResponse(ctx, responseID, ResponseFields{ReviewState: &rs}); err == nil {
			updated = u
		}
	}
	_ = s.events.Write(ctx, &event.Event{EventType: event.VendorResponseAssociated, EntityType: "vendor_response", EntityID: &responseID,
		ActorType: event.ActorSystem, Payload: map[string]any{"vendor_request_id": vr.ID.String(), "buyer_request_id": vr.BuyerRequestID.String(), "rule": rule}})
	s.decide(ctx, vr, autonomy.DecAssociateVendorReply, autonomy.OutcomeApplied, "", rule, updated)
	s.maybeReconfirmListing(ctx, vr, updated)
	return updated, nil
}

func (s *Service) decide(ctx context.Context, vr *VendorRequest, kind, outcome string, fb autonomy.Fallback, reason string, resp *VendorResponse) {
	if s.auto == nil || vr == nil {
		return
	}
	brid := vr.BuyerRequestID
	res := map[string]any{"vendor_request_id": vr.ID.String(), "reference": vr.PublicReference}
	if resp != nil {
		res["vendor_response_id"] = resp.ID.String()
		res["status"] = string(resp.Status)
		res["review_state"] = string(resp.ReviewState)
	}
	var conf *float64
	if resp != nil {
		c := resp.Confidence
		conf = &c
	}
	s.recordVendorDecision(ctx, vr.ID, &brid, kind, outcome, fb, reason, res, conf)
}

func (s *Service) recordVendorDecision(ctx context.Context, subject uuid.UUID, brid *uuid.UUID, kind, outcome string, fb autonomy.Fallback, reason string, result map[string]any, conf ...*float64) {
	d := &autonomy.Decision{WorkflowType: "vendor_message", SubjectType: "vendor_request", SubjectID: subject, BuyerRequestID: brid,
		Kind: kind, Outcome: outcome, Fallback: fb, Reason: reason, PolicyVersion: autonomy.PolicyVersion, Result: result, Actor: "SYSTEM"}
	if brid == nil {
		d.SubjectType = "vendor_response"
	}
	if len(conf) > 0 {
		d.Confidence = conf[0]
	}
	if err := s.auto.Store.RecordDecision(ctx, d); err != nil {
		s.logger.Error("recording vendor decision", "error", err)
	}
}

func timeOr(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// ResendVendorRequest re-sends the message of a SEND_FAILED vendor request
// (automatic delivery recovery). It returns the channel error unchanged so
// the caller can classify it.
func (s *Service) ResendVendorRequest(ctx context.Context, vendorRequestID uuid.UUID) error {
	vr, err := s.routing.FindVendorRequestByID(ctx, vendorRequestID)
	if err != nil || vr == nil {
		return fmt.Errorf("loading vendor request: %v", err)
	}
	if vr.Status != VendorRequestSendFailed {
		return nil // already recovered (or answered): nothing to do
	}
	req, err := s.requests.FindByID(ctx, vr.BuyerRequestID)
	if err != nil || req == nil {
		return fmt.Errorf("loading buyer request: %v", err)
	}
	v, err := s.vendors.FindByID(ctx, vr.VendorID)
	if err != nil || v == nil {
		return fmt.Errorf("loading vendor: %v", err)
	}
	var listing *supply.Listing
	if vr.SupplyListingID != nil && s.listings != nil {
		listing, _ = s.listings.FindByID(ctx, *vr.SupplyListingID)
	}
	msg, extra := vendorRequestMessage(vr, req, listing)
	extra["retry"] = true
	vrID := vr.ID
	purpose := conversation.OutboundPurpose{Kind: "vendor_request", EntityType: "vendor_request", EntityID: &vrID, Extra: extra}
	if _, err := s.sendVendorRequest(ctx, purpose, v.WhatsAppExternalID, vr, req, listing, msg); err != nil {
		_ = s.routing.MarkSendFailed(ctx, vr.ID, err.Error())
		return err
	}
	if err := s.routing.MarkSent(ctx, vr.ID); err != nil {
		return err
	}
	_ = s.vendors.RecordRequestSent(ctx, vr.VendorID)
	if s.lifecycle != nil {
		vr.Status = VendorRequestSent
		s.lifecycle.VendorRequestSent(ctx, vr)
	}
	return nil
}

// ---- deterministic text matching helpers (exported for the buyer runtime) ----

var tokenSplit = regexp.MustCompile(`[^a-z0-9₦]+`)

// stopTokens are words that never identify an item.
var stopTokens = map[string]bool{"the": true, "a": true, "an": true, "one": true, "it": true, "is": true, "for": true, "i": true,
	"me": true, "my": true, "and": true, "or": true, "of": true, "you": true, "your": true, "that": true, "this": true, "yes": true,
	"no": true, "get": true, "have": true, "am": true, "dey": true, "na": true, "abeg": true, "pls": true, "please": true,
	"want": true, "need": true, "with": true, "to": true, "in": true, "on": true, "do": true, "ok": true, "okay": true}

// Tokens splits text into lowercase identifying tokens.
func Tokens(text string) map[string]bool {
	out := map[string]bool{}
	for _, t := range tokenSplit.Split(strings.ToLower(text), -1) {
		if t == "" || stopTokens[t] {
			continue
		}
		out[t] = true
	}
	return out
}

// TokenOverlap counts how many identifying tokens of label appear in tokens.
func TokenOverlap(tokens map[string]bool, label string) int {
	n := 0
	for t := range Tokens(label) {
		if tokens[t] {
			n++
		}
	}
	return n
}

var bareNumber = regexp.MustCompile(`^\s*#?\s*(\d{1,2})\s*$`)

// MatchVendorOption maps a vendor's answer onto one option: its number, its
// MM reference, or the only option whose label it names. nil = no match.
func MatchVendorOption(text string, opts []autonomy.Option) *autonomy.Option {
	t := autonomy.Normalize(text)
	if m := bareNumber.FindStringSubmatch(t); m != nil {
		for i := range opts {
			if opts[i].Key == m[1] {
				return &opts[i]
			}
		}
		return nil
	}
	if m := mmReferencePattern.FindStringSubmatch(text); m != nil {
		for i := range opts {
			if strings.EqualFold(opts[i].Ref, "MM-"+m[1]) {
				return &opts[i]
			}
		}
	}
	tokens := Tokens(text)
	best, bestScore, tie := -1, 0, false
	for i, o := range opts {
		label := o.Label
		if l, ok := o.Value["label"].(string); ok {
			label = l
		}
		sc := TokenOverlap(tokens, label)
		switch {
		case sc > bestScore:
			best, bestScore, tie = i, sc, false
		case sc == bestScore && sc > 0:
			tie = true
		}
	}
	if best < 0 || tie {
		return nil
	}
	return &opts[best]
}

// keep imports used even if request is only referenced in docs.
var _ = request.StatusConfirmed
