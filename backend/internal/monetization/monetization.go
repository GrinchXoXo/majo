// Package monetization holds the revenue-line feature flags and the gates
// each one needs (pilot batch item 8). Every line is off by default, and
// the server refuses to start with a line on before its stage and payment
// provider exist. So a line can only be switched on in a separate, reviewed
// change after the owner confirms the legal and business condition
// (docs/architecture/monetization-and-payments-rollout.md).
package monetization

import (
	"fmt"
	"sort"
	"strings"

	"github.com/middleman/backend/internal/payments"
)

// Line is a revenue line.
type Line string

const (
	LeadCredits   Line = "LEAD_CREDITS"
	VendorPro     Line = "VENDOR_PRO"
	VerifiedBadge Line = "VERIFIED_BADGE"
	RushRequest   Line = "RUSH_REQUEST"
	Sponsored     Line = "SPONSORED"
	SplitPayments Line = "SPLIT_PAYMENTS"
	Escrow        Line = "ESCROW"
)

// Lines in display order.
var Lines = []Line{LeadCredits, VendorPro, VerifiedBadge, RushRequest, Sponsored, SplitPayments, Escrow}

// Stage of the business (rollout doc): A = unregistered pilot, B = CAC
// business name, C = limited company with a licensed partner.
type Stage string

const (
	StageA Stage = "A"
	StageB Stage = "B"
	StageC Stage = "C"
)

func (s Stage) rank() int { return map[Stage]int{StageA: 1, StageB: 2, StageC: 3}[s] }

// ParseStage reads MONETIZATION_STAGE ("" = A).
func ParseStage(s string) (Stage, error) {
	switch st := Stage(strings.ToUpper(strings.TrimSpace(s))); st {
	case "":
		return StageA, nil
	case StageA, StageB, StageC:
		return st, nil
	}
	return StageA, fmt.Errorf("MONETIZATION_STAGE must be A, B or C, got %q", s)
}

// Requirement is what a line needs before it may be switched on.
type Requirement struct {
	Stage     Stage           `json:"stage"`
	Providers []payments.Kind `json:"providers"` // any of these
	Payer     string          `json:"payer"`
	Gate      string          `json:"gate"` // the owner's condition, in words
}

// Requirements per line.
var Requirements = map[Line]Requirement{
	LeadCredits: {StageB, []payments.Kind{payments.KindPaymentLink, payments.KindSplit}, "vendor",
		"CAC business name registered; credit price approved (test ₦100–300 per lead)"},
	VendorPro: {StageB, []payments.Kind{payments.KindPaymentLink, payments.KindSplit}, "vendor",
		"CAC business name registered; plan price approved (test ₦1,500–5,000 a month); priority only among equally eligible vendors"},
	VerifiedBadge: {StageB, []payments.Kind{payments.KindPaymentLink, payments.KindSplit}, "vendor",
		"CAC business name registered; verification procedure written down"},
	RushRequest: {StageB, []payments.Kind{payments.KindPaymentLink, payments.KindSplit}, "buyer",
		"CAC business name registered; ordinary requests stay free"},
	Sponsored: {StageB, []payments.Kind{payments.KindPaymentLink, payments.KindSplit}, "vendor",
		"CAC business name registered; always labelled \"Sponsored\"; only among already eligible vendors"},
	SplitPayments: {StageB, []payments.Kind{payments.KindSplit}, "buyer (Majo keeps only its fee)",
		"provider terms on chargebacks and refunds checked; sub-accounts accepted for a business name; scam-after-payment policy"},
	Escrow: {StageC, []payments.Kind{payments.KindEscrowPartner}, "buyer deposit (5–11% commission)",
		"limited company; licensed partner agreement signed; legal review of terms and disputes; the five open escrow decisions answered"},
}

// Settings is the monetization configuration.
type Settings struct {
	Stage    Stage         `json:"stage"`
	Provider payments.Kind `json:"provider"`
	Enabled  map[Line]bool `json:"enabled"`
}

// Default: Stage A, no provider, every line off.
func Default() Settings {
	return Settings{Stage: StageA, Provider: payments.KindNone, Enabled: map[Line]bool{}}
}

// On reports whether a line is switched on.
func (s Settings) On(l Line) bool { return s.Enabled[l] }

// CustomerFundsAllowed: may Majo's books hold buyers' or vendors' money?
// Only with split payments or escrow switched on.
func (s Settings) CustomerFundsAllowed() bool { return s.On(SplitPayments) || s.On(Escrow) }

// Validate refuses a line switched on before its stage and provider.
func (s Settings) Validate() error {
	if s.Stage.rank() == 0 {
		return fmt.Errorf("unknown monetization stage %q", s.Stage)
	}
	var problems []string
	for _, l := range Lines {
		if !s.On(l) {
			continue
		}
		r := Requirements[l]
		if s.Stage.rank() < r.Stage.rank() {
			problems = append(problems, fmt.Sprintf("%s needs stage %s (now %s): %s", l, r.Stage, s.Stage, r.Gate))
			continue
		}
		ok := false
		for _, k := range r.Providers {
			ok = ok || k == s.Provider
		}
		if !ok {
			problems = append(problems, fmt.Sprintf("%s needs PAYMENTS_PROVIDER %v (now %s)", l, r.Providers, s.Provider))
		}
	}
	for l := range s.Enabled {
		if _, known := Requirements[l]; !known {
			problems = append(problems, fmt.Sprintf("unknown revenue line %s", l))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("monetization: %s", strings.Join(problems, "; "))
	}
	return nil
}

// EnvName is the environment variable for a line: FEATURE_LEAD_CREDITS etc.
func EnvName(l Line) string { return "FEATURE_" + string(l) }
