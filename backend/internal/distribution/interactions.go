package distribution

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/channel"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/discovery"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/matching"
	"github.com/middleman/backend/internal/presentation"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/structid"
)

// Inbound is a structured interaction (or STOP keyword) from a buyer, as
// handed over by the channel adapter after verify/persist/dedupe.
type Inbound struct {
	BuyerID        uuid.UUID
	ConversationID uuid.UUID
	MessageID      uuid.UUID // persisted inbound message (idempotency key)
	ReplyTo        string
	RawID          string // structured ID (button/list/template payload, or typed)
	Tapped         bool   // came from an interactive reply (not typed text)
}

// Interaction reasons (UNRESOLVED changes no commerce state and no consent).
const (
	UnresolvedMalformed       = "malformed"
	UnresolvedUnknown         = "unknown_delivery"
	UnresolvedForeign         = "foreign_delivery"
	UnresolvedNotSent         = "delivery_not_sent"
	UnresolvedExpired         = "expired_delivery"
	UnresolvedListingNotShown = "listing_not_in_delivery"
	UnresolvedListingGone     = "listing_no_longer_available"
	UnresolvedConsentNoPrompt = "consent_without_prompt"
	UnresolvedConsentWording  = "unknown_consent_wording"
	UnresolvedNotSubscribed   = "not_subscribed"
	AppliedAlreadyAsked       = "already_asked"
)

// IsStopKeyword recognises an explicit typed opt-out.
func IsStopKeyword(text string) bool {
	switch strings.ToLower(strings.Trim(strings.TrimSpace(text), ".!")) {
	case "stop", "unsubscribe", "stop finds", "stop majo finds":
		return true
	}
	return false
}

// HandleStructured resolves a structured interaction. It never falls
// through to the free-text request pipeline: anything it cannot resolve
// is recorded UNRESOLVED (with the reason) and changes nothing else.
func (s *Service) HandleStructured(ctx context.Context, in Inbound) {
	id, err := structid.Parse(in.RawID)
	rec := &Interaction{InboundMessageID: in.MessageID, BuyerID: in.BuyerID, RawID: truncate(in.RawID, 300), Outcome: OutcomeUnresolved}
	if err != nil {
		rec.Reason = UnresolvedMalformed
		s.finishUnresolved(ctx, in, rec, presentation.ExpiredInteraction)
		return
	}
	rec.Action = string(id.Action)
	switch id.Action {
	case structid.ActionConsentIn, structid.ActionConsentNo:
		s.handleConsentTap(ctx, in, id, rec)
		return
	}

	rec.ListingRef = id.ListingRef
	rec.Category = id.Category
	d, err := s.store.FindDelivery(ctx, id.DeliveryID)
	if id.Action == structid.ActionStop {
		// STOP is an opt-out: it is honoured for the tapping buyer whatever
		// the state or age of the delivery it was tapped on (an expired,
		// unknown or foreign delivery must never leave someone subscribed).
		// The delivery is used for attribution only when it is theirs.
		if err == nil && d != nil && d.BuyerID == in.BuyerID {
			rec.DeliveryID = &d.ID
		} else {
			d = nil
		}
		s.applyStop(ctx, in, rec, d, SourceButton, "stop_button")
		return
	}
	switch {
	case err != nil:
		s.logger.Error("loading discovery delivery for interaction", "error", err)
		rec.Reason = UnresolvedUnknown
	case d == nil:
		rec.Reason = UnresolvedUnknown
	case d.BuyerID != in.BuyerID:
		rec.Reason = UnresolvedForeign // never reveal or act on another buyer's delivery
	case d.Status != DeliverySent || d.SentAt == nil:
		rec.Reason = UnresolvedNotSent
	case s.now().UTC().Sub(*d.SentAt) > s.cfg.InteractionTTL:
		rec.Reason = UnresolvedExpired
	}
	if rec.Reason != "" {
		if rec.Reason != UnresolvedForeign && rec.Reason != UnresolvedUnknown {
			rec.DeliveryID = &d.ID
		}
		s.finishUnresolved(ctx, in, rec, presentation.ExpiredInteraction)
		return
	}
	rec.DeliveryID = &d.ID

	var listingID *uuid.UUID
	if id.ListingRef != "" {
		for _, c := range d.Candidates {
			if c.PublicReference == id.ListingRef {
				lid := c.ListingID
				listingID = &lid
			}
		}
		if listingID == nil || !contains(d.ListingIDs, *listingID) {
			rec.Reason = UnresolvedListingNotShown
			s.finishUnresolved(ctx, in, rec, presentation.ExpiredInteraction)
			return
		}
		rec.ListingID = listingID
	}

	switch id.Action {
	case structid.ActionShow, structid.ActionCategory:
		s.applyShow(ctx, in, d, id, rec)
	case structid.ActionListing, structid.ActionVideo:
		s.applyListing(ctx, in, d, id, rec, *listingID)
	case structid.ActionAsk:
		s.applyAsk(ctx, in, d, rec, *listingID)
	case structid.ActionNotInterested:
		cats := map[string]bool{}
		for _, c := range d.Candidates {
			if c.Category != "" {
				cats[c.Category] = true
			}
		}
		var list []string
		for c := range cats {
			list = append(list, c)
		}
		rec.Category = strings.Join(sortedStrings(list), ",")
		rec.Outcome = OutcomeApplied
		if s.record(ctx, in, rec, d) {
			s.replyText(ctx, in, d, presentation.NotInterestedAck)
		}
	}
}

// applyStop applies the opt-out FIRST and acknowledges only on success, so a
// buyer is never told "Done" while still subscribed. On failure nothing is
// recorded (a retry or a second STOP can still succeed) and the buyer is
// asked to resend.
func (s *Service) applyStop(ctx context.Context, in Inbound, rec *Interaction, d *Delivery, source, reason string) {
	if err := s.optOut(ctx, in, source, reason); err != nil {
		s.logger.Error("recording discovery opt-out", "buyer_id", in.BuyerID, "error", err)
		s.replyText(ctx, in, d, "Sorry, I couldn't process that just now. Please send STOP again.")
		return
	}
	rec.Outcome = OutcomeApplied
	if !s.record(ctx, in, rec, d) {
		return
	}
	s.replyText(ctx, in, d, presentation.StopAck)
}

// HandleStopKeyword handles a typed STOP for a buyer who is opted in to
// discovery: it is answered (never turned into a BuyerRequest). It returns
// false, having done nothing, when the buyer is not subscribed (or discovery
// is off), so the message continues down the ordinary inbound path exactly as
// it did before Phase 4B.
func (s *Service) HandleStopKeyword(ctx context.Context, in Inbound) bool {
	rec := &Interaction{InboundMessageID: in.MessageID, BuyerID: in.BuyerID, RawID: "keyword:stop", Action: string(structid.ActionStop)}
	p, err := s.store.Preference(ctx, in.BuyerID)
	if err != nil {
		s.logger.Error("loading discovery preference", "error", err)
		return false
	}
	if p == nil || p.Status != OptedIn {
		return false
	}
	s.applyStop(ctx, in, rec, nil, SourceKeyword, "stop_keyword")
	return true
}

func (s *Service) handleConsentTap(ctx context.Context, in Inbound, id structid.ID, rec *Interaction) {
	// Ownership for consent IDs: a real tap on a prompt Majo sent this buyer.
	ok, err := s.store.HasConsentPrompt(ctx, in.BuyerID, s.now().UTC().Add(-s.cfg.InteractionTTL))
	if err != nil {
		s.logger.Error("checking consent prompt", "error", err)
	}
	switch {
	case !in.Tapped || !ok:
		rec.Reason = UnresolvedConsentNoPrompt
	case id.Action == structid.ActionConsentIn && id.Wording != s.cfg.ConsentWording:
		rec.Reason = UnresolvedConsentWording
	}
	if rec.Reason != "" {
		s.finishUnresolved(ctx, in, rec, presentation.ExpiredInteraction)
		return
	}
	rec.Outcome = OutcomeApplied
	if !s.record(ctx, in, rec, nil) {
		return
	}
	if id.Action == structid.ActionConsentNo {
		s.replyText(ctx, in, nil, presentation.ConsentNoAck)
		return
	}
	status := OptedIn
	msgID := in.MessageID
	if _, _, err := s.store.ApplyPreference(ctx, PreferenceChange{
		BuyerID: in.BuyerID, Status: &status, WordingVersion: id.Wording, Source: SourceButton, SourceMessageID: &msgID,
		ActorType: "BUYER", ActorID: in.BuyerID.String(), Reason: "consent_prompt_yes", At: s.now().UTC(),
	}); err != nil {
		s.logger.Error("recording discovery opt-in", "error", err)
		return
	}
	s.replyText(ctx, in, nil, presentation.ConsentInAck)
}

func (s *Service) optOut(ctx context.Context, in Inbound, source, reason string) error {
	status := OptedOut
	msgID := in.MessageID
	if _, _, err := s.store.ApplyPreference(ctx, PreferenceChange{
		BuyerID: in.BuyerID, Status: &status, WordingVersion: s.cfg.ConsentWording, Source: source, SourceMessageID: &msgID,
		ActorType: "BUYER", ActorID: in.BuyerID.String(), Reason: reason, At: s.now().UTC(),
	}); err != nil && !errors.Is(err, ErrNoPreference) {
		return err
	}
	return nil
}

// applyShow re-presents the delivery (optionally one category), with the
// listings re-evaluated now: anything expired since is not shown.
func (s *Service) applyShow(ctx context.Context, in Inbound, d *Delivery, id structid.ID, rec *Interaction) {
	dc, err := s.refresh(ctx, d, d.ListingIDs)
	if err != nil {
		s.logger.Error("refreshing delivery for show", "delivery_id", d.ID, "error", err)
		return
	}
	cands := dc.cands
	if id.Action == structid.ActionCategory {
		var kept []discovery.Candidate
		for _, c := range cands {
			if c.Listing.Category != nil && matching.CategoriesCompatible(*c.Listing.Category, id.Category) {
				kept = append(kept, c)
			}
		}
		cands = kept
	}
	rec.Outcome = OutcomeApplied
	if !s.record(ctx, in, rec, d) {
		return
	}
	if len(cands) == 0 {
		s.replyText(ctx, in, d, "Sorry, those finds are no longer available. Tell me what you're looking for and I'll search 👀")
		return
	}
	s.reply(ctx, in, d, presentation.Overview(d.ID, presentation.Arm(d.Arm), cardsFor(cands, dc.media)))
}

func (s *Service) applyListing(ctx context.Context, in Inbound, d *Delivery, id structid.ID, rec *Interaction, listingID uuid.UUID) {
	dc, err := s.refresh(ctx, d, []uuid.UUID{listingID})
	if err != nil {
		s.logger.Error("refreshing listing for card", "delivery_id", d.ID, "error", err)
		return
	}
	if len(dc.cands) == 0 {
		rec.Reason = UnresolvedListingGone
		s.finishUnresolved(ctx, in, rec, "Sorry, "+id.ListingRef+" is no longer available. Tell me what you're looking for and I'll search 👀")
		return
	}
	rec.Outcome = OutcomeApplied
	if !s.record(ctx, in, rec, d) {
		return
	}
	card := cardsFor(dc.cands, dc.media)[0]
	if id.Action == structid.ActionVideo {
		s.reply(ctx, in, d, presentation.VideoMessages(d.ID, card))
		return
	}
	s.reply(ctx, in, d, []channel.Message{presentation.CardMessage(d.ID, presentation.Arm(d.Arm), card)})
}

// applyAsk is "Ask about it": the buyer expressed interest, which becomes a
// discovery-origin BuyerRequest in the EXISTING commerce loop. Nothing is
// purchased, reserved or confirmed to the buyer as available.
func (s *Service) applyAsk(ctx context.Context, in Inbound, d *Delivery, rec *Interaction, listingID uuid.UUID) {
	dc, err := s.refresh(ctx, d, []uuid.UUID{listingID})
	if err != nil {
		s.logger.Error("refreshing listing for enquiry", "delivery_id", d.ID, "error", err)
		return
	}
	if len(dc.cands) == 0 {
		rec.Reason = UnresolvedListingGone
		s.finishUnresolved(ctx, in, rec, "Sorry, "+rec.ListingRef+" is no longer available. Tell me what you're looking for and I'll search 👀")
		return
	}
	c := dc.cands[0]
	l := c.Listing
	location := dc.viewer.Location
	if location == "" && l.Location != nil {
		location = *l.Location
	}
	fields := &request.Fields{Category: l.Category, Product: l.Product, Variant: l.Variant, Condition: l.Condition, Quantity: l.Quantity}
	if location != "" {
		fields.Location = &location
	}
	msgID := in.MessageID
	lid, did := l.ID, d.ID
	req, created, err := s.requests.Create(ctx, request.NewRequest{
		Origin: request.OriginDiscovery, ConversationID: in.ConversationID, BuyerID: in.BuyerID, OriginMessageID: &msgID,
		RawText: "Ask about it: " + l.PublicReference, Fields: fields, ListingID: &lid, DiscoveryDeliveryID: &did,
		ExtractedData: map[string]any{
			"source": "discovery_listing", "listing_reference": l.PublicReference, "listing_price": l.Price,
			"freshness_at_enquiry": string(c.Eligibility.Freshness.State), "needs_reconfirmation": c.NeedsReconfirmation(),
			"geo_stage_at_enquiry": string(c.Eligibility.Geography.Stage), "batch_id": d.BatchID.String(), "arm": d.Arm,
		},
		EventExtra: map[string]any{"discovery_batch_id": d.BatchID.String(), "arm": d.Arm, "listing_reference": l.PublicReference},
	})
	if err != nil {
		// The request is created BEFORE the tap is recorded, so a failure here
		// loses nothing: the buyer can tap again (creation is idempotent per
		// delivery+listing) and is told so.
		s.logger.Error("creating discovery-origin buyer request", "delivery_id", d.ID, "error", err)
		s.replyText(ctx, in, d, "Sorry, I couldn't pass that on just now. Please tap Ask about it again in a moment.")
		return
	}
	rec.Outcome = OutcomeApplied
	if !s.record(ctx, in, rec, d) {
		return // this inbound message was already handled
	}
	if err := s.store.SetInteractionRequest(ctx, rec.ID, req.ID); err != nil {
		s.logger.Error("linking interaction to buyer request", "error", err)
	}
	if !created {
		s.replyText(ctx, in, d, "You've already asked about "+l.PublicReference+" 👍 I'll update you as soon as the seller confirms.")
		return
	}
	s.event(ctx, event.ListingEnquiry, "buyer_request", req.ID, event.ActorBuyer, event.Merge(req.FunnelDims(), map[string]any{
		"discovery_delivery_id": d.ID.String(), "discovery_batch_id": d.BatchID.String(), "arm": d.Arm,
		"listing_reference": l.PublicReference, "needs_reconfirmation": c.NeedsReconfirmation(),
		"freshness": string(c.Eligibility.Freshness.State),
	}))
	if s.cfg.EnquiryConfirmation == ConfirmAuto {
		if _, err := s.requests.ConfirmAs(ctx, req.ID, event.ActorSystem, "auto_listing_origin"); err != nil {
			s.logger.Error("auto-confirming discovery request", "buyer_request_id", req.ID, "error", err)
		}
	}
	s.replyText(ctx, in, d, presentation.AskAck(cardsFor(dc.cands, dc.media)[0]))
}

// record persists an interaction once per inbound message. false = this
// inbound message was already handled (do nothing more).
func (s *Service) record(ctx context.Context, in Inbound, rec *Interaction, d *Delivery) bool {
	rec.ID = uuid.New()
	created, err := s.store.RecordInteraction(ctx, rec)
	if err != nil {
		s.logger.Error("recording discovery interaction", "error", err)
		return false
	}
	if !created {
		return false
	}
	payload := map[string]any{"action": rec.Action, "outcome": rec.Outcome, "reason": rec.Reason, "raw_id": rec.RawID,
		"buyer_id": in.BuyerID.String(), "listing_reference": rec.ListingRef}
	t := event.DiscoveryInteraction
	if rec.Outcome != OutcomeApplied {
		t = event.DiscoveryInteractionUnresolved
	}
	entityType, entityID := "conversation", in.ConversationID
	if d != nil {
		payload["discovery_delivery_id"] = d.ID.String()
		payload["discovery_batch_id"] = d.BatchID.String()
		payload["arm"] = d.Arm
		entityType, entityID = "discovery_delivery", d.ID
	} else if rec.DeliveryID != nil {
		payload["discovery_delivery_id"] = rec.DeliveryID.String()
	}
	if rec.ListingID != nil {
		payload["supply_listing_id"] = rec.ListingID.String()
	}
	s.event(ctx, t, entityType, entityID, event.ActorBuyer, payload)
	return true
}

func (s *Service) finishUnresolved(ctx context.Context, in Inbound, rec *Interaction, reply string) {
	rec.Outcome = OutcomeUnresolved
	if !s.record(ctx, in, rec, nil) {
		return
	}
	s.replyText(ctx, in, nil, reply)
}

func (s *Service) replyText(ctx context.Context, in Inbound, d *Delivery, text string) {
	s.reply(ctx, in, d, []channel.Message{{Kind: channel.KindText, Body: text}})
}

func (s *Service) reply(ctx context.Context, in Inbound, d *Delivery, msgs []channel.Message) {
	extra := map[string]any{}
	purpose := conversation.OutboundPurpose{Kind: "discovery_reply", EntityType: "conversation", EntityID: &in.ConversationID, Extra: extra}
	if d != nil {
		did := d.ID
		purpose.EntityType, purpose.EntityID = "discovery_delivery", &did
		extra["batch_id"] = d.BatchID.String()
		extra["arm"] = d.Arm
	}
	for _, m := range msgs {
		did := uuid.Nil
		if d != nil {
			did = d.ID
		}
		m = s.publishMedia(ctx, did, m)
		if m.Kind == channel.KindVideo && (m.Media == nil || m.Media.ProviderRef == "") {
			m = channel.Message{Kind: channel.KindText, Body: "Sorry, the video isn't available right now."}
		}
		extra["message_kind"] = string(m.Kind)
		extra["interaction_ids"] = m.InteractionIDs()
		if _, _, err := s.out.SendRecorded(conversation.WithOutboundPurpose(ctx, purpose), in.ReplyTo, m); err != nil {
			s.logger.Error("sending discovery reply", "error", err)
			return
		}
	}
}

// ---- consent operations (staff) -----------------------------------------

// InviteConsent sends the explicit opt-in prompt to an internal recipient
// who has already messaged Majo (inside the 24h window).
func (s *Service) InviteConsent(ctx context.Context, recipient string) error {
	if !s.cfg.CanSend() {
		return ErrDiscoveryOff
	}
	if ok, _ := s.cfg.Permits(recipient); !ok {
		return ErrNotPermitted
	}
	buyerID, err := s.store.BuyerIDByRecipient(ctx, "whatsapp", recipient)
	if err != nil {
		return err
	}
	if buyerID == nil {
		return ErrUnknownBuyer
	}
	sendCtx := conversation.WithOutboundPurpose(ctx, conversation.OutboundPurpose{
		Kind: "discovery_consent_prompt", EntityType: "buyer", EntityID: buyerID,
		Extra: map[string]any{"wording_version": s.cfg.ConsentWording},
	})
	_, _, err = s.out.SendRecorded(sendCtx, recipient, presentation.ConsentPrompt(s.cfg.ConsentWording))
	return err
}

// StaffPreference is an explicit staff/test consent record (INTERNAL only,
// allowlisted recipients only). It never applies to real buyers.
type StaffPreference struct {
	Recipient  string
	Status     *ConsentStatus // nil = keep (update location/interests only)
	Location   *string
	Interests  []string
	OperatorID string
	Note       string
}

// SetStaffPreference records explicit staff consent / viewer details.
func (s *Service) SetStaffPreference(ctx context.Context, sp StaffPreference) (*Preference, error) {
	if !s.cfg.CanSend() {
		return nil, ErrDiscoveryOff
	}
	if ok, _ := s.cfg.Permits(sp.Recipient); !ok {
		return nil, ErrNotPermitted
	}
	if sp.Status != nil && *sp.Status != OptedIn && *sp.Status != OptedOut {
		return nil, fmt.Errorf("%w: status must be OPTED_IN or OPTED_OUT", ErrInvalidInput)
	}
	buyerID, err := s.store.BuyerIDByRecipient(ctx, "whatsapp", sp.Recipient)
	if err != nil {
		return nil, err
	}
	if buyerID == nil {
		return nil, ErrUnknownBuyer
	}
	reason := sp.Note
	if reason == "" {
		reason = "staff test record"
	}
	p, _, err := s.store.ApplyPreference(ctx, PreferenceChange{
		BuyerID: *buyerID, Status: sp.Status, Location: sp.Location, Interests: sp.Interests,
		WordingVersion: s.cfg.ConsentWording, Source: SourceOperatorStaff, ActorType: "OPERATOR", ActorID: sp.OperatorID,
		Reason: reason, At: s.now().UTC(),
	})
	return p, err
}

func contains(ids []uuid.UUID, id uuid.UUID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func sortedStrings(xs []string) []string {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
	return xs
}
