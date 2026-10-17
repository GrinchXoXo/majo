package monetization

import (
	"strings"
	"testing"

	"github.com/middleman/backend/internal/payments"
)

func TestDefaultIsAllOff(t *testing.T) {
	s := Default()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, l := range Lines {
		if s.On(l) {
			t.Fatalf("%s on by default", l)
		}
		if _, ok := Requirements[l]; !ok {
			t.Fatalf("%s has no requirement", l)
		}
	}
	if s.CustomerFundsAllowed() {
		t.Fatal("no customer funds in Stage A")
	}
}

func TestGates(t *testing.T) {
	cases := []struct {
		stage    Stage
		provider payments.Kind
		line     Line
		wantErr  string
	}{
		{StageA, payments.KindNone, LeadCredits, "needs stage B"},
		{StageB, payments.KindNone, LeadCredits, "needs PAYMENTS_PROVIDER"},
		{StageB, payments.KindPaymentLink, LeadCredits, ""},
		{StageB, payments.KindPaymentLink, SplitPayments, "needs PAYMENTS_PROVIDER"},
		{StageB, payments.KindSplit, SplitPayments, ""},
		{StageB, payments.KindEscrowPartner, Escrow, "needs stage C"},
		{StageC, payments.KindPaymentLink, Escrow, "needs PAYMENTS_PROVIDER"},
		{StageC, payments.KindEscrowPartner, Escrow, ""},
		{StageA, payments.KindNone, Line("COMMISSION"), "unknown revenue line"},
	}
	for _, c := range cases {
		s := Settings{Stage: c.stage, Provider: c.provider, Enabled: map[Line]bool{c.line: true}}
		err := s.Validate()
		if c.wantErr == "" && err != nil || c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
			t.Fatalf("%s/%s/%s: %v (want %q)", c.stage, c.provider, c.line, err, c.wantErr)
		}
	}
	if !(Settings{Stage: StageC, Provider: payments.KindEscrowPartner, Enabled: map[Line]bool{Escrow: true}}).CustomerFundsAllowed() {
		t.Fatal("escrow allows customer funds")
	}
}

func TestParseStage(t *testing.T) {
	for in, want := range map[string]Stage{"": StageA, "a": StageA, "B": StageB, " c ": StageC} {
		if got, err := ParseStage(in); err != nil || got != want {
			t.Fatalf("%q %v %v", in, got, err)
		}
	}
	if _, err := ParseStage("D"); err == nil {
		t.Fatal("D refused")
	}
	if EnvName(LeadCredits) != "FEATURE_LEAD_CREDITS" {
		t.Fatal(EnvName(LeadCredits))
	}
}
