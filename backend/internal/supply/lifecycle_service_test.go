package supply

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/middleman/backend/internal/event"
)

// Phase 4A.5 service behaviour (fake repository; Postgres covered separately).

func TestLifecycle_PublishStartsTheCeiling(t *testing.T) {
	f := newCurationFixture(t)
	got, err := f.svc.Publish(context.Background(), f.id, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.PublishedAt == nil || !got.ExpiresAt.Equal(got.PublishedAt.Add(60*24*time.Hour)) {
		t.Fatalf("expires_at must be published_at + 60 days: %v / %v", got.PublishedAt, got.ExpiresAt)
	}
	if f.repo.curationEvents[0].Payload["lifecycle_expires_at"] == nil {
		t.Fatal("publication event records the lifecycle ceiling")
	}
}

func TestLifecycle_OperatorVerifyMovesFreshnessOnly(t *testing.T) {
	f := newCurationFixture(t)
	pub, _ := f.svc.Publish(context.Background(), f.id, nil, nil)
	old := time.Now().Add(-100 * time.Hour)
	f.repo.listings[f.id].LastVerifiedAt = &old
	got, err := f.svc.Verify(context.Background(), f.id, strp("called the vendor"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastVerifiedAt.After(old) || !got.ExpiresAt.Equal(pub.ExpiresAt) || got.Status != ListingActive {
		t.Fatalf("verify moves last_verified_at only: %+v", got)
	}
	if MustDefaultPolicy().EvaluateListing(got, nil, time.Now()).Freshness.State != Fresh {
		t.Fatal("verified listing is fresh")
	}
	n := len(f.repo.curationEvents)
	ev := f.repo.curationEvents[n-1]
	a := f.repo.curationAudits[len(f.repo.curationAudits)-1]
	if ev.EventType != event.SupplyListingVerified || ev.Payload["source"] != "operator" || a.Action != "SUPPLY_LISTING_VERIFIED" ||
		a.NewValue["note"] != "called the vendor" {
		t.Fatalf("verification event/audit: %+v / %+v", ev, a)
	}
}

func TestLifecycle_ExpiredListingCannotBeRevived(t *testing.T) {
	f := newCurationFixture(t)
	if _, err := f.svc.Publish(context.Background(), f.id, nil, nil); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute)
	f.repo.listings[f.id].ExpiresAt = past // lifecycle ceiling reached; status still ACTIVE
	before := len(f.repo.curationEvents)
	if _, err := f.svc.Verify(context.Background(), f.id, nil, nil, nil); !errors.Is(err, ErrLifecycleExpired) {
		t.Fatalf("verify on an expired listing: %v", err)
	}
	if _, err := f.svc.Edit(context.Background(), f.id, ListingEdit{Price: i64p(1)}, nil); !errors.Is(err, ErrLifecycleExpired) {
		t.Fatalf("edit on an expired listing: %v", err)
	}
	got := f.get()
	if !got.ExpiresAt.Equal(past) || got.Status != ListingActive || len(f.repo.curationEvents) != before {
		t.Fatal("nothing may change an expired listing's lifecycle")
	}
	// Withdrawal (reject) of an expired listing is still allowed: it only closes it.
	if _, err := f.svc.Reject(context.Background(), f.id, strp("sold offline"), nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycle_VerifyStatuses(t *testing.T) {
	for _, st := range []ListingStatus{ListingSold, ListingUnavailable, ListingArchived, ListingExpired} {
		f := newCurationFixture(t)
		f.repo.listings[f.id].Status = st
		if _, err := f.svc.Verify(context.Background(), f.id, nil, nil, nil); !errors.As(err, new(ErrInvalidTransition)) {
			t.Fatalf("%s: %v", st, err)
		}
	}
	f := newCurationFixture(t) // a DRAFT can be verified (still inside its provisional ceiling)
	if _, err := f.svc.Verify(context.Background(), f.id, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}
