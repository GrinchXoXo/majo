package distribution

import (
	"strings"
	"testing"
	"time"

	"github.com/middleman/backend/internal/discovery"
	"github.com/middleman/backend/internal/presentation"
)

func validConfig() Config {
	w, _ := ParseWindow("", "Africa/Lagos")
	return Config{Mode: ModeInternal, InternalRecipients: map[string]bool{"2348100000001": true},
		Arms: []presentation.Arm{presentation.ArmA, presentation.ArmB}, MaxDeliveriesPerBuyer: 1, CapPeriod: time.Hour,
		ExposureWindow: time.Hour, ListingsPerDelivery: 3, Window: w, InteractionTTL: time.Hour, NegativeWindow: time.Hour,
		ConsentWording: "v1", EnquiryConfirmation: ConfirmByOperator, Ranking: discovery.DefaultParams(), OutboundMediaTTL: time.Hour}
}

func TestConfigValidate(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Config){
		"not available in Phase 4B":              func(c *Config) { c.Mode = ModeOn },
		"CONTROLLED_PILOT":                       func(c *Config) { c.Mode = ModeControlledPilot },
		"OFF or INTERNAL":                        func(c *Config) { c.Mode = "MAYBE" },
		"requires DISCOVERY_INTERNAL_RECIPIENTS": func(c *Config) { c.InternalRecipients = nil },
		"not a WhatsApp number":                  func(c *Config) { c.InternalRecipients = map[string]bool{"+234 810": true} },
		"unknown or repeated arm":                func(c *Config) { c.Arms = []presentation.Arm{"ARM_Z"} },
		"LISTINGS_PER_DELIVERY":                  func(c *Config) { c.ListingsPerDelivery = 9 },
		"ENQUIRY_CONFIRMATION":                   func(c *Config) { c.EnquiryConfirmation = "sometimes" },
		"30-day":                                 func(c *Config) { c.OutboundMediaTTL = 31 * 24 * time.Hour },
		"TEMPLATE_LANGUAGE":                      func(c *Config) { c.Template = presentation.TemplateConfig{Name: "majo_finds"} },
	}
	for want, mod := range bad {
		c := validConfig()
		mod(&c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
	off := validConfig()
	off.Mode, off.InternalRecipients = ModeOff, nil
	if err := off.Validate(); err != nil {
		t.Fatalf("OFF needs no recipients: %v", err)
	}
}

func TestPermitsIsTheStaffOnlyBoundary(t *testing.T) {
	c := validConfig()
	if ok, _ := c.Permits("2348100000001"); !ok {
		t.Fatal("internal recipient")
	}
	if ok, r := c.Permits("2348000000000"); ok || r != ReasonNotInternalRecipient {
		t.Fatal("real buyers are never permitted in INTERNAL")
	}
	c.Mode = ModeOff
	if ok, r := c.Permits("2348100000001"); ok || r != ReasonDiscoveryOff || c.CanSend() {
		t.Fatal("OFF permits nobody")
	}
}

func TestDeliveryWindow(t *testing.T) {
	w, err := ParseWindow("18:00-20:00", "Africa/Lagos")
	if err != nil {
		t.Fatal(err)
	}
	lagos, _ := time.LoadLocation("Africa/Lagos")
	at := func(h, m int) time.Time { return time.Date(2026, 10, 6, h, m, 0, 0, lagos) }
	for _, c := range []struct {
		t  time.Time
		ok bool
	}{{at(17, 59), false}, {at(18, 0), true}, {at(19, 59), true}, {at(20, 0), false}, {at(18, 30).UTC(), true}} {
		if w.Allows(c.t) != c.ok {
			t.Errorf("%v: %v", c.t, !c.ok)
		}
	}
	night, _ := ParseWindow("22:00-02:00", "Africa/Lagos")
	if !night.Allows(at(23, 0)) || !night.Allows(at(1, 0)) || night.Allows(at(12, 0)) {
		t.Fatal("window across midnight")
	}
	for _, bad := range []string{"6pm-8pm", "25:00-26:00", "18:00-18:00"} {
		if _, err := ParseWindow(bad, "Africa/Lagos"); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
	if _, err := ParseWindow("", "Mars/Olympus"); err == nil {
		t.Fatal("bad time zone")
	}
	open, _ := ParseWindow("", "Africa/Lagos")
	if !open.Allows(at(3, 0)) {
		t.Fatal("no window = any time")
	}
}

func TestStopKeyword(t *testing.T) {
	for _, s := range []string{"STOP", "stop", " Stop. ", "unsubscribe"} {
		if !IsStopKeyword(s) {
			t.Errorf("%q", s)
		}
	}
	for _, s := range []string{"stop sending me phones please", "1", "stopwatch"} {
		if IsStopKeyword(s) {
			t.Errorf("%q is not STOP", s)
		}
	}
}
