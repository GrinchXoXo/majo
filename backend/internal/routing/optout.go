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

// vendorKeyword reads a whole message as STOP or START ("" otherwise).
func vendorKeyword(text string) string {
	t := strings.ToLower(strings.Join(strings.FieldsFunc(text, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}), " "))
	switch t {
	case "stop", "unsubscribe", "stop majo", "remove me", "opt out", "optout", "stop requests", "stop sending":
		return "STOP"
	case "start", "resume", "unstop", "start majo", "start again", "start requests":
		return "START"
	}
	return ""
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
