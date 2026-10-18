package routing

// 01a audit (trust and safety): the vendor invite promises "You can leave
// at any time by replying STOP", and Meta requires opt-outs from
// business-initiated messages to be honoured. A vendor who sends STOP is
// made INACTIVE at once (no more requests, matched or templated). START asks
// to come back; an operator re-activates them (they are re-vetted, and a
// BLOCKED vendor cannot undo a block by texting).

import (
	"context"
	"strings"

	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/vendor"
)

const (
	msgVendorStopped = "Okay 👍 You won't get any more buyer requests from Majo. If you change your mind, reply START."
	msgVendorStart   = "Thanks 🙏 The Majo team will switch your buyer requests back on shortly."
)

// vendorKeyword reads a whole message as STOP or START ("" otherwise): the
// keyword (or phrase) plus at most polite filler ("pls stop", "stop o",
// "STOP please"), never a sentence that merely contains the word.
func vendorKeyword(text string) string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !(r >= 'a' && r <= 'z') })
	var core []string
	for _, w := range words {
		if !keywordFiller[w] {
			core = append(core, w)
		}
	}
	switch strings.Join(core, " ") {
	case "stop", "unsubscribe", "remove me", "opt out", "optout", "stop requests", "stop sending", "stop messages", "stop sending me requests":
		return "STOP"
	case "start", "resume", "unstop", "start again", "start requests", "add me back":
		return "START"
	}
	return ""
}

var keywordFiller = map[string]bool{"please": true, "pls": true, "plz": true, "abeg": true, "o": true, "oh": true, "majo": true,
	"now": true, "thanks": true, "thank": true, "you": true, "kindly": true, "sir": true, "ma": true}

// HandleVendorKeyword applies STOP/START from a vendor of any status but
// BLOCKED (the webhook calls it for PENDING and INACTIVE vendors, who
// otherwise take the buyer path); it reports whether the text was one.
func (s *Service) HandleVendorKeyword(ctx context.Context, v *vendor.Vendor, text string) bool {
	if v == nil || v.Status == vendor.StatusBlocked {
		return false
	}
	return s.handleVendorKeyword(ctx, v, text)
}

// handleVendorKeyword applies STOP/START; it reports whether it did.
func (s *Service) handleVendorKeyword(ctx context.Context, v *vendor.Vendor, text string) bool {
	kw := vendorKeyword(text)
	if kw == "" {
		return false
	}
	reply := msgVendorStart
	switch kw {
	case "STOP":
		reply = msgVendorStopped
		if v.Status == vendor.StatusActive || v.Status == vendor.StatusPending {
			inactive := vendor.StatusInactive
			if _, err := s.vendors.Update(ctx, v.ID, vendor.Fields{Status: &inactive}); err != nil {
				s.logger.Error("vendor opt-out: deactivating", "vendor_id", v.ID, "error", err)
				return true // never treat STOP as a reply or as supply
			}
			_ = s.events.Write(ctx, &event.Event{EventType: event.VendorDeactivated, EntityType: "vendor", EntityID: &v.ID,
				ActorType: event.ActorVendor, ActorID: &v.ID, Payload: map[string]any{"reason": "vendor_opt_out", "previous_status": string(v.Status)}})
		}
	case "START":
		_ = s.events.Write(ctx, &event.Event{EventType: event.VendorOptInRequested, EntityType: "vendor", EntityID: &v.ID,
			ActorType: event.ActorVendor, ActorID: &v.ID, Payload: map[string]any{"status": string(v.Status)}})
	}
	if v.WhatsAppExternalID != "" && s.messaging != nil {
		sendCtx := conversation.WithOutboundPurpose(ctx, conversation.OutboundPurpose{Kind: "vendor_opt_" + strings.ToLower(kw),
			EntityType: "vendor", EntityID: &v.ID})
		if err := s.messaging.SendText(sendCtx, v.WhatsAppExternalID, reply); err != nil {
			s.logger.Warn("vendor opt-out: reply not sent", "vendor_id", v.ID, "error", err)
		}
	}
	return true
}
