// Package distribution is Majo's Distribution capability (Phase 4B,
// Architecture Lock V2 §G): who receives what, when, how and how often.
//
// Distribution owns audience, consent enforcement, frequency caps, dedupe,
// delivery windows, batches, delivery records, presentation arms, outbound
// sending and attribution. It never ranks: it calls Discovery for relevance
// and the canonical supply eligibility policy stays upstream of both.
//
// Phase 4B is the INTERNAL foundation only: a feature gate (OFF | INTERNAL)
// and an allowlist of staff recipients are a hard boundary against
// messaging real buyers; batches are operator-triggered (no scheduler).
package distribution

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/middleman/backend/internal/discovery"
	"github.com/middleman/backend/internal/presentation"
)

// Mode is the discovery feature gate.
type Mode string

const (
	ModeOff      Mode = "OFF"
	ModeInternal Mode = "INTERNAL"
	// Rollout states after 4B; refused at startup in this phase.
	ModeControlledPilot Mode = "CONTROLLED_PILOT"
	ModeOn              Mode = "ON"
)

// SessionWindow is WhatsApp's customer-service window [PLATFORM]: inside it
// session (interactive/media) messages may be sent; outside it only an
// approved template.
const SessionWindow = 24 * time.Hour

// Window is a daily delivery window in a time zone. Unset = any time.
type Window struct {
	Set        bool
	Start, End int // minutes after local midnight
	Loc        *time.Location
	Raw        string
}

var windowPattern = regexp.MustCompile(`^(\d{2}):(\d{2})-(\d{2}):(\d{2})$`)

// ParseWindow parses "HH:MM-HH:MM" ("" = no window). A window may cross
// midnight ("22:00-02:00").
func ParseWindow(s, tz string) (Window, error) {
	s = strings.TrimSpace(s)
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return Window{}, fmt.Errorf("DISCOVERY_TIMEZONE %q: %v", tz, err)
	}
	if s == "" {
		return Window{Loc: loc}, nil
	}
	m := windowPattern.FindStringSubmatch(s)
	if m == nil {
		return Window{}, fmt.Errorf("DISCOVERY_DELIVERY_WINDOW must look like 18:00-20:00, got %q", s)
	}
	n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
	if n(1) > 23 || n(3) > 23 || n(2) > 59 || n(4) > 59 {
		return Window{}, fmt.Errorf("DISCOVERY_DELIVERY_WINDOW %q is not a valid time range", s)
	}
	w := Window{Set: true, Start: n(1)*60 + n(2), End: n(3)*60 + n(4), Loc: loc, Raw: s}
	if w.Start == w.End {
		return Window{}, fmt.Errorf("DISCOVERY_DELIVERY_WINDOW %q is empty", s)
	}
	return w, nil
}

// Allows reports whether t falls inside the window.
func (w Window) Allows(t time.Time) bool {
	if !w.Set {
		return true
	}
	lt := t.In(w.Loc)
	m := lt.Hour()*60 + lt.Minute()
	if w.Start < w.End {
		return m >= w.Start && m < w.End
	}
	return m >= w.Start || m < w.End // crosses midnight
}

// Enquiry confirmation modes (Lock §O-1).
const (
	ConfirmByOperator = "operator"
	ConfirmAuto       = "auto"
)

// Config is the Distribution configuration. Every value is a [PARAM].
type Config struct {
	Mode                  Mode
	InternalRecipients    map[string]bool
	Arms                  []presentation.Arm
	MaxDeliveriesPerBuyer int
	CapPeriod             time.Duration
	ExposureWindow        time.Duration
	ListingsPerDelivery   int
	Window                Window
	InteractionTTL        time.Duration
	NegativeWindow        time.Duration
	Template              presentation.TemplateConfig
	ConsentWording        string
	EnquiryConfirmation   string
	Ranking               discovery.RankingParams
	OutboundMediaTTL      time.Duration
}

var (
	recipientPattern = regexp.MustCompile(`^\d{6,15}$`)
	wordingPattern   = regexp.MustCompile(`^[a-z0-9._-]{1,20}$`)
)

// Validate fails clearly on any invalid or not-yet-allowed setting.
func (c Config) Validate() error {
	switch c.Mode {
	case ModeOff, ModeInternal:
	case ModeControlledPilot, ModeOn:
		return fmt.Errorf("DISCOVERY_MODE=%s is not available in Phase 4B: only OFF and INTERNAL (staff test recipients) are allowed until the controlled pilot (Phase 4C)", c.Mode)
	default:
		return fmt.Errorf("DISCOVERY_MODE must be OFF or INTERNAL, got %q", c.Mode)
	}
	if c.Mode == ModeInternal && len(c.InternalRecipients) == 0 {
		return fmt.Errorf("DISCOVERY_MODE=INTERNAL requires DISCOVERY_INTERNAL_RECIPIENTS (staff test numbers)")
	}
	for r := range c.InternalRecipients {
		if !recipientPattern.MatchString(r) {
			return fmt.Errorf("DISCOVERY_INTERNAL_RECIPIENTS: %q is not a WhatsApp number (digits only)", r)
		}
	}
	if len(c.Arms) == 0 {
		return fmt.Errorf("DISCOVERY_ARMS must name at least one presentation arm")
	}
	seen := map[presentation.Arm]bool{}
	for _, a := range c.Arms {
		if !presentation.KnownArm(a) || seen[a] {
			return fmt.Errorf("DISCOVERY_ARMS: unknown or repeated arm %q (known: ARM_A, ARM_B)", a)
		}
		seen[a] = true
	}
	if c.MaxDeliveriesPerBuyer < 1 || c.CapPeriod <= 0 || c.ExposureWindow <= 0 {
		return fmt.Errorf("discovery frequency caps must be positive")
	}
	if c.ListingsPerDelivery < 1 || c.ListingsPerDelivery > 8 {
		return fmt.Errorf("DISCOVERY_LISTINGS_PER_DELIVERY must be between 1 and 8, got %d", c.ListingsPerDelivery)
	}
	if c.InteractionTTL <= 0 || c.NegativeWindow <= 0 || c.OutboundMediaTTL <= 0 {
		return fmt.Errorf("discovery windows must be positive")
	}
	if c.OutboundMediaTTL > 29*24*time.Hour {
		return fmt.Errorf("DISCOVERY_OUTBOUND_MEDIA_TTL must stay below the provider's 30-day media ID lifetime")
	}
	if !wordingPattern.MatchString(c.ConsentWording) {
		return fmt.Errorf("DISCOVERY_CONSENT_WORDING must be a short lowercase version id like v1, got %q", c.ConsentWording)
	}
	if c.EnquiryConfirmation != ConfirmByOperator && c.EnquiryConfirmation != ConfirmAuto {
		return fmt.Errorf("DISCOVERY_ENQUIRY_CONFIRMATION must be operator or auto, got %q", c.EnquiryConfirmation)
	}
	if c.Template.Name != "" && c.Template.Language == "" {
		return fmt.Errorf("DISCOVERY_TEMPLATE_LANGUAGE is required with DISCOVERY_TEMPLATE_NAME")
	}
	return nil
}

// Gate exclusions.
const (
	ReasonDiscoveryOff         = "discovery_off"
	ReasonNotInternalRecipient = "not_internal_recipient"
)

// Permits is the hard staff-only boundary: in OFF nobody may receive
// discovery; in INTERNAL only allowlisted staff recipients.
func (c Config) Permits(recipient string) (bool, string) {
	switch c.Mode {
	case ModeInternal:
		if c.InternalRecipients[recipient] {
			return true, ""
		}
		return false, ReasonNotInternalRecipient
	default:
		return false, ReasonDiscoveryOff
	}
}

// CanSend reports whether batches may be sent at all.
func (c Config) CanSend() bool { return c.Mode == ModeInternal }
