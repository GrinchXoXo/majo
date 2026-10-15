package routing

// Pilot batch item 3: vendor requests outside WhatsApp's 24-hour window.
//
// Free-form text only reaches a vendor who wrote to Majo in the last 24
// hours; anything else is refused by Meta (131047) and the vendor is lost to
// the request. With an approved utility template configured
// (VENDOR_REQUEST_TEMPLATE_NAME), Majo sends the request as that template
// when the vendor's window is closed (or unknown), and retries once as the
// template when a free-text send is refused for the window. With no template
// configured nothing changes. The template wording to submit to Meta is in
// docs/pilot/whatsapp-templates.md.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/channel"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/supply"
)

// VendorTemplate names the approved Meta template. Empty Name = disabled.
type VendorTemplate struct {
	Name     string
	Language string
}

// RichSender sends a rendered channel message and records it
// (conversation.RecordingSender implements it).
type RichSender interface {
	SendRecorded(ctx context.Context, to string, m channel.Message) (*uuid.UUID, string, error)
}

// InboundWindow reports the last time a contact wrote to Majo.
type InboundWindow interface {
	LastInboundAt(ctx context.Context, channel, externalID string) (*time.Time, error)
}

// vendorWindowOpenFor is how long after a vendor's last message free text is
// still sent. Kept below Meta's 24 hours so a send never lands just after the
// window closed.
const vendorWindowOpenFor = 23*time.Hour + 30*time.Minute

// SetVendorTemplate enables template sends for vendor requests.
func (s *Service) SetVendorTemplate(t VendorTemplate, sender RichSender, window InboundWindow) {
	if t.Language == "" {
		t.Language = "en"
	}
	s.vendorTemplate, s.richSender, s.inboundWindow = t, sender, window
}

func (s *Service) templateEnabled() bool {
	return s.vendorTemplate.Name != "" && s.richSender != nil
}

// vendorWindowOpen: true when the vendor wrote recently. Unknown counts as
// closed (a template always arrives; free text might not).
func (s *Service) vendorWindowOpen(ctx context.Context, to string) bool {
	if s.inboundWindow == nil {
		return false
	}
	at, err := s.inboundWindow.LastInboundAt(ctx, "whatsapp", to)
	if err != nil || at == nil {
		return false
	}
	return s.clock().Sub(*at) < vendorWindowOpenFor
}

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

// sendVendorRequest sends one vendor request: free text inside the window,
// the approved template outside it. It returns how it was sent ("text" or
// "template") and the channel error, unchanged, so callers classify it.
func (s *Service) sendVendorRequest(ctx context.Context, purpose conversation.OutboundPurpose, to string, vr *VendorRequest,
	req *request.BuyerRequest, listing *supply.Listing, text string) (string, error) {
	withVia := func(via string) context.Context {
		p := purpose
		p.Extra = map[string]any{}
		for k, v := range purpose.Extra {
			p.Extra[k] = v
		}
		p.Extra["sent_as"] = via
		return conversation.WithOutboundPurpose(ctx, p)
	}
	if !s.templateEnabled() {
		return "text", s.messaging.SendText(withVia("text"), to, text)
	}
	if s.vendorWindowOpen(ctx, to) {
		err := s.messaging.SendText(withVia("text"), to, text)
		if err == nil || !channel.Classify(err).TemplateRequired {
			return "text", err
		}
		s.logger.Info("vendor window closed after all; retrying as template", "vendor_request_id", vr.ID)
	}
	m := channel.Message{Kind: channel.KindTemplate, Template: &channel.Template{
		Name: s.vendorTemplate.Name, Language: s.vendorTemplate.Language,
		BodyParams: VendorTemplateParams(vr.PublicReference, req, listing),
	}}
	_, _, err := s.richSender.SendRecorded(withVia("template"), to, m)
	return "template", err
}

// VendorTemplateParams fills the template's four body variables:
//
//	{{1}} what the buyer wants, {{2}} budget, {{3}} location, {{4}} reference.
//
// Meta rejects empty variables and variables with new lines, tabs or more
// than four spaces in a row, so every value is cleaned and never empty.
func VendorTemplateParams(ref string, req *request.BuyerRequest, listing *supply.Listing) []string {
	var item, budget string
	if listing != nil {
		parts := []string{"your listing " + listing.PublicReference + ":", derefStr(listing.Product)}
		parts = appendIf(parts, listing.Variant, listing.Condition)
		item = strings.Join(parts, " ")
		budget = "Not stated"
		if listing.Price != nil {
			budget = "Your listed price " + FormatNaira(*listing.Price)
		}
	} else {
		parts := []string{derefStr(req.Product)}
		parts = appendIf(parts, req.Variant, req.Condition)
		if req.Quantity != nil && *req.Quantity > 1 {
			parts = append(parts, fmt.Sprintf("x%d", *req.Quantity))
		}
		for _, p := range req.Preferences {
			if strings.TrimSpace(p) != "" {
				parts = append(parts, p)
			}
		}
		item = strings.Join(parts, ", ")
		budget = budgetText(req.BudgetMin, req.BudgetMax)
	}
	loc := "Not stated"
	if req != nil && req.Location != nil {
		loc = *req.Location
	}
	return []string{templateValue(item, 160), templateValue(budget, 60), templateValue(loc, 60), templateValue(ref, 20)}
}

func appendIf(parts []string, vals ...*string) []string {
	for _, v := range vals {
		if v != nil && strings.TrimSpace(*v) != "" {
			parts = append(parts, *v)
		}
	}
	return parts
}

func budgetText(lo, hi *int64) string {
	switch {
	case hi == nil && lo == nil:
		return "Not stated"
	case lo != nil && hi != nil && *lo != *hi:
		return FormatNaira(*lo) + " to " + FormatNaira(*hi)
	case hi != nil:
		return "Up to " + FormatNaira(*hi)
	default:
		return "From " + FormatNaira(*lo)
	}
}

// FormatNaira writes ₦45,000.
func FormatNaira(v int64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprint(v)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-₦" + b.String()
	}
	return "₦" + b.String()
}

// templateValue makes a value acceptable to Meta: one line, no tabs, no
// run of more than one space, at most max characters, never empty.
func templateValue(v string, max int) string {
	v = strings.Join(strings.Fields(v), " ")
	if r := []rune(v); len(r) > max {
		v = strings.TrimSpace(string(r[:max-1])) + "…"
	}
	if v == "" {
		return "Not stated"
	}
	return v
}
