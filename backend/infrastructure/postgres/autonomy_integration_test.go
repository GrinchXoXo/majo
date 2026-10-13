package postgres

// Phase 4C correction: Postgres integration tests for the autonomous
// runtime's state — clarifications (one open per party conversation,
// supersede, lazy expiry, single winner), the decision log, the exception
// queue (idempotent per open subject+reason), advisory locks, vendor
// request expiry, buyer context and vendor trust.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/autonomy"
	"github.com/middleman/backend/internal/workflow"
)

func TestIntegration_ClarificationsOneOpenPerConversationAndSingleWinner(t *testing.T) {
	db := testDB(t, migrationsPath())
	ctx := context.Background()
	s := seed(t, db, "2348011112222")
	repo := NewAutonomyRepository(db)
	now := time.Now().UTC()
	open := func(q string) *autonomy.Clarification {
		c, err := repo.OpenClarification(ctx, &autonomy.Clarification{Party: autonomy.PartyBuyer, ConversationID: &s.conv, BuyerID: &s.buyer,
			BuyerRequestID: &s.req, Kind: autonomy.KindMissingField, Field: "budget", Question: q, ExpiresAt: now.Add(time.Hour)})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		return c
	}
	first := open("What's your budget?")
	second := open("Which city?")
	cur, err := repo.OpenBuyerClarification(ctx, s.conv, now)
	if err != nil || cur == nil || cur.ID != second.ID {
		t.Fatalf("the newest question is the open one: %+v %v", cur, err)
	}
	var st string
	_ = db.QueryRow(`SELECT status FROM clarifications WHERE id = $1`, first.ID).Scan(&st)
	if st != autonomy.ClarSuperseded {
		t.Fatalf("older question superseded, got %s", st)
	}

	// Two concurrent answers: exactly one resolves the question.
	var wins int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := repo.ResolveClarification(ctx, second.ID, autonomy.ClarAnswered, nil, "Lagos", nil, time.Now()); err == nil {
				atomic.AddInt32(&wins, 1)
			} else if !errors.Is(err, autonomy.ErrClarificationNotOpen) {
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("exactly one answer wins, got %d", wins)
	}

	// Lazy expiry: an old open question is closed when read.
	third := open("Size?")
	if _, err := repo.OpenBuyerClarification(ctx, s.conv, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	_ = db.QueryRow(`SELECT status FROM clarifications WHERE id = $1`, third.ID).Scan(&st)
	if st != autonomy.ClarExpired {
		t.Fatalf("expired, got %s", st)
	}
	cs, _ := repo.ClarificationsForRequest(ctx, s.req)
	if len(cs) != 3 {
		t.Fatalf("history keeps every question, got %d", len(cs))
	}
}

func TestIntegration_ExceptionsIdempotentAndOnlyOperatorClasses(t *testing.T) {
	db := testDB(t, migrationsPath())
	ctx := context.Background()
	s := seed(t, db, "2348011113333")
	repo := NewAutonomyRepository(db)
	if _, err := repo.RaiseException(ctx, &autonomy.Exception{Class: autonomy.ClarificationRequired, Reason: "x", SubjectType: "buyer_request", SubjectID: s.req}); err == nil {
		t.Fatalf("clarification is never an operator exception")
	}
	e1, err := repo.RaiseException(ctx, &autonomy.Exception{Class: autonomy.SystemException, Reason: autonomy.ReasonDeliveryRetryExhausted,
		SubjectType: "vendor_request", SubjectID: s.vr, BuyerRequestID: &s.req})
	if err != nil {
		t.Fatal(err)
	}
	e2, err := repo.RaiseException(ctx, &autonomy.Exception{Class: autonomy.SystemException, Reason: autonomy.ReasonDeliveryRetryExhausted,
		SubjectType: "vendor_request", SubjectID: s.vr, BuyerRequestID: &s.req})
	if err != nil || e2.ID != e1.ID {
		t.Fatalf("same open subject+reason -> same exception: %v", err)
	}
	if err := repo.ResolveException(ctx, e1.ID, "operator", "called the vendor", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := repo.ResolveException(ctx, e1.ID, "operator", "again", time.Now()); err == nil {
		t.Fatalf("resolving twice must fail")
	}
	e3, _ := repo.RaiseException(ctx, &autonomy.Exception{Class: autonomy.SystemException, Reason: autonomy.ReasonDeliveryRetryExhausted,
		SubjectType: "vendor_request", SubjectID: s.vr})
	if e3.ID == e1.ID {
		t.Fatalf("after resolution a new failure is a new exception")
	}
	open, _ := repo.ListExceptions(ctx, autonomy.ExceptionOpen, 10)
	if len(open) != 1 {
		t.Fatalf("one open, got %d", len(open))
	}
}

func TestIntegration_WithLockSerialises(t *testing.T) {
	db := testDB(t, migrationsPath())
	repo := NewAutonomyRepository(db)
	ctx := context.Background()
	var inside, maxInside int32
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = repo.WithLock(ctx, "request:same", func(ctx context.Context) error {
				n := atomic.AddInt32(&inside, 1)
				for {
					m := atomic.LoadInt32(&maxInside)
					if n <= m || atomic.CompareAndSwapInt32(&maxInside, m, n) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
				atomic.AddInt32(&inside, -1)
				return nil
			})
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Fatalf("one workflow step at a time per key, saw %d", maxInside)
	}
}

func TestIntegration_ExpireStaleVendorRequestsAndCounts(t *testing.T) {
	db := testDB(t, migrationsPath())
	ctx := context.Background()
	s := seed(t, db, "2348011114444")
	mustExec(t, db, `UPDATE vendor_requests SET sent_at = now() - interval '4 days' WHERE id = $1`, s.vr)
	repo := NewRoutingRepository(db)
	n, err := repo.CountOpenByVendor(ctx, []uuid.UUID{s.vendor}, time.Now().Add(-72*time.Hour))
	if err != nil || n[s.vendor] != 0 {
		t.Fatalf("old request does not count as open in the window: %v %v", n, err)
	}
	exp, err := repo.ExpireStale(ctx, s.vendor, time.Now().Add(-72*time.Hour))
	if err != nil || len(exp) != 1 {
		t.Fatalf("expired: %v %v", exp, err)
	}
	active, _ := repo.ListActiveByVendor(ctx, s.vendor)
	if len(active) != 0 {
		t.Fatalf("expired request no longer awaits a reply")
	}
	var st string
	_ = db.QueryRow(`SELECT status FROM vendor_requests WHERE id = $1`, s.vr).Scan(&st)
	if st != "EXPIRED" {
		t.Fatalf("status %s", st)
	}
}

func TestIntegration_BuyerContextOffersRequestsAndKnownLocation(t *testing.T) {
	db := testDB(t, migrationsPath())
	ctx := context.Background()
	s := seed(t, db, "2348011115555")
	mustExec(t, db, `UPDATE buyer_requests SET location = 'Benin City', category = 'electronics' WHERE id = $1`, s.req)
	o := insertOffer(t, db, s, &s.resp, 60, "SENT")
	mustExec(t, db, `UPDATE offers SET presentation_ordinal = 1, sent_at = now() WHERE id = $1`, o)
	bc, err := NewWorkflowRepository(db).BuyerContext(ctx, s.conv, s.buyer, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var offers, reqs int
	for _, it := range bc.Items {
		switch it.Kind {
		case workflow.ItemOffer:
			offers++
			if it.Product != "iphone 13" || it.Price == nil || *it.Price != 620000 || it.Ordinal != 1 {
				t.Fatalf("offer item falls back to the request's product: %+v", it)
			}
		case workflow.ItemRequest:
			reqs++
		}
	}
	if offers != 1 || reqs != 1 || bc.KnownLocation != "Benin City" {
		t.Fatalf("context: offers=%d requests=%d location=%q", offers, reqs, bc.KnownLocation)
	}
	ids, err := NewWorkflowRepository(db).PendingRequests(ctx, time.Now().Add(-time.Hour), 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id == s.req {
			t.Fatalf("a routed request is not pending")
		}
	}
}

func TestIntegration_VendorTrustCountsOnlyOperatorDecisions(t *testing.T) {
	db := testDB(t, migrationsPath())
	ctx := context.Background()
	s := seed(t, db, "2348011116666")
	tr, err := NewWorkflowRepository(db).VendorTrust(ctx, s.vendor)
	if err != nil || tr.Decided != 0 || tr.Trusted() {
		t.Fatalf("no history -> not trusted: %+v %v", tr, err)
	}
}

func TestIntegration_AutonomyMetricsCountActors(t *testing.T) {
	db := testDB(t, migrationsPath())
	ctx := context.Background()
	s := seed(t, db, "2348011117777")
	mustExec(t, db, `INSERT INTO events (event_type, entity_type, entity_id, actor_type) VALUES ('REQUEST_CONFIRMED', 'buyer_request', $1, 'OPERATOR')`, s.req)
	mustExec(t, db, `INSERT INTO events (event_type, entity_type, entity_id, actor_type) VALUES ('VENDORS_ROUTED', 'vendor_request', $1, 'OPERATOR')`, s.vr)
	s2 := seed(t, db, "2348011118888")
	mustExec(t, db, `INSERT INTO events (event_type, entity_type, entity_id, actor_type) VALUES ('REQUEST_CONFIRMED', 'buyer_request', $1, 'SYSTEM')`, s2.req)
	mustExec(t, db, `INSERT INTO events (event_type, entity_type, entity_id, actor_type) VALUES ('VENDORS_ROUTED', 'vendor_request', $1, 'SYSTEM')`, s2.vr)
	m, err := NewAutonomyRepository(db).Metrics(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.ManualRouting.N != 1 || m.ManualRouting.Of != 2 || m.ManualConfirmation.N != 1 || m.OperatorIntervention.N != 1 || m.Workflows != 2 {
		t.Fatalf("metrics: %+v", m)
	}
	if m.ByClass[autonomy.ClassOperatorIntervention] != 1 || m.ByClass[autonomy.ClassAutonomous] != 1 {
		t.Fatalf("classes: %+v", m.ByClass)
	}
	if m.InterventionsByReason["manual_routing"] != 1 || m.InterventionsByReason["manual_confirmation"] != 1 {
		t.Fatalf("reasons kept separate: %+v", m.InterventionsByReason)
	}
}
