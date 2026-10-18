package routing

import (
	"context"
	"testing"

	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/supply"
	"github.com/middleman/backend/internal/vendor"
)

func TestVendorKeyword(t *testing.T) {
	for in, want := range map[string]string{"STOP": "STOP", " stop. ": "STOP", "Stop Majo": "STOP", "unsubscribe": "STOP", "remove me!": "STOP",
		"START": "START", "start again": "START", "Stop please": "STOP", "pls stop": "STOP", "STOP o 🙏": "STOP", "add me back": "START", "stop by 5pm": "", "I have stopped selling iphone": "", "yes": "", "": ""} {
		if got := vendorKeyword(in); got != want {
			t.Errorf("vendorKeyword(%q) = %q, want %q", in, got, want)
		}
	}
}

// 01a audit: STOP deactivates the vendor and is never read as a reply or
// supply; START asks to come back without re-activating anyone; a blocked
// vendor stays blocked.
func TestVendorStopAndStart(t *testing.T) {
	svc, v, in := classifier(t, 1, "iphone 13")
	msgs, events := svc.messaging.(*fakeMessaging), svc.events.(*fakeEvents)
	ctx := context.Background()

	if resp := svc.ProcessVendorMessageFrom(ctx, v, supply.Inbound{}, "STOP"); resp != nil || len(in.texts) != 0 {
		t.Fatal("STOP is not a reply and not supply")
	}
	if v.Status != vendor.StatusInactive {
		t.Fatalf("vendor deactivated, got %s", v.Status)
	}
	if len(msgs.sent) != 1 || msgs.sent[0] != msgVendorStopped {
		t.Fatalf("vendor told: %v", msgs.sent)
	}
	found := false
	for _, e := range events.events {
		found = found || (e.EventType == event.VendorDeactivated && e.Payload["reason"] == "vendor_opt_out")
	}
	if !found {
		t.Fatal("opt-out recorded")
	}

	svc.ProcessVendorMessageFrom(ctx, v, supply.Inbound{}, "start")
	if v.Status != vendor.StatusInactive || msgs.sent[len(msgs.sent)-1] != msgVendorStart {
		t.Fatalf("START asks, an operator re-activates: %s %v", v.Status, msgs.sent)
	}
	if events.events[len(events.events)-1].EventType != event.VendorOptInRequested {
		t.Fatal("opt-in request recorded")
	}

	v.Status = vendor.StatusBlocked
	svc.ProcessVendorMessageFrom(ctx, v, supply.Inbound{}, "STOP")
	if v.Status != vendor.StatusBlocked {
		t.Fatal("a block is never undone or changed by texting")
	}
}
