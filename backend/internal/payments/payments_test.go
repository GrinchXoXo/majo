package payments

import (
	"context"
	"errors"
	"testing"
)

func TestOnlyNoneIsBuilt(t *testing.T) {
	p, err := New(KindNone)
	if err != nil || p.Kind() != KindNone {
		t.Fatalf("%v %v", p, err)
	}
	if _, err := p.CreateCheckout(context.Background(), CheckoutRequest{Reference: "r", AmountKobo: 1, Currency: "NGN"}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("none takes no money: %v", err)
	}
	if _, err := p.ParseWebhook(nil, []byte("{}")); !errors.Is(err, ErrDisabled) {
		t.Fatal("none accepts no webhook")
	}
	for _, k := range []Kind{KindPaymentLink, KindSplit, KindEscrowPartner} {
		if _, err := New(k); err == nil {
			t.Fatalf("%s must be refused until it is built", k)
		}
	}
}

func TestParseKind(t *testing.T) {
	for in, want := range map[string]Kind{"": KindNone, "none": KindNone, " Payment_Link ": KindPaymentLink, "split": KindSplit, "escrow_partner": KindEscrowPartner} {
		if got, err := ParseKind(in); err != nil || got != want {
			t.Fatalf("%q: %v %v", in, got, err)
		}
	}
	if _, err := ParseKind("paystack"); err == nil {
		t.Fatal("unknown kind refused")
	}
}

func TestCheckoutValidate(t *testing.T) {
	ok := CheckoutRequest{Reference: "lc_1", AmountKobo: 30000, Currency: "NGN"}
	if ok.Validate() != nil {
		t.Fatal(ok.Validate())
	}
	for _, bad := range []CheckoutRequest{{AmountKobo: 1, Currency: "NGN"}, {Reference: "r", Currency: "NGN"}, {Reference: "r", AmountKobo: -5, Currency: "NGN"}, {Reference: "r", AmountKobo: 1, Currency: "naira"}} {
		if bad.Validate() == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}
