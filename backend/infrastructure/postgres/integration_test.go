package postgres

// Postgres-backed integration tests for the Phase 3.1 invariants that only a
// real database can prove (unique indexes under concurrency, advisory-lock
// claim serialization, migration 0006 on data with pre-existing duplicates).
//
// Skipped unless MAJO_TEST_DATABASE_URL points at a Postgres server where the
// test user may CREATE DATABASE. Each test runs in its own throwaway
// database, so it never touches real data. Example:
//
//	MAJO_TEST_DATABASE_URL="postgres://middleman:middleman@localhost:5432/postgres?sslmode=disable" \
//	  go test ./infrastructure/postgres/ -run Integration -v

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/offer"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/routing"
	"github.com/middleman/backend/internal/transaction"
)

func testDB(t *testing.T, migrationsDir string) *sql.DB {
	t.Helper()
	adminURL := os.Getenv("MAJO_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("MAJO_TEST_DATABASE_URL not set; skipping Postgres integration test")
	}
	ctx := context.Background()
	admin, err := Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connecting admin db: %v", err)
	}
	name := "majo_it_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating test db: %v", err)
	}
	u, _ := url.Parse(adminURL)
	u.Path = "/" + name
	db, err := Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connecting test db: %v", err)
	}
	db.SetMaxOpenConns(30)
	t.Cleanup(func() {
		db.Close()
		_, _ = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		admin.Close()
	})
	if migrationsDir != "" {
		if err := RunMigrations(ctx, db, migrationsDir); err != nil {
			t.Fatalf("migrations: %v", err)
		}
	}
	return db
}

func migrationsPath() string { return filepath.Join("..", "..", "migrations") }

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// seed creates buyer, conversation, CONFIRMED buyer request, vendor, SENT
// vendor request and an eligible response. Returns IDs.
type seeded struct{ buyer, conv, req, vendor, vr, resp uuid.UUID }

func seed(t *testing.T, db *sql.DB, phone string) seeded {
	t.Helper()
	s := seeded{uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	mustExec(t, db, `INSERT INTO buyers (id, external_user_id) VALUES ($1, $2)`, s.buyer, phone)
	mustExec(t, db, `INSERT INTO conversations (id, external_id, buyer_id) VALUES ($1, $2, $3)`, s.conv, phone, s.buyer)
	mustExec(t, db, `INSERT INTO buyer_requests (id, conversation_id, buyer_id, status, product, variant, raw_message, confirmed_at)
		VALUES ($1, $2, $3, 'CONFIRMED', 'iphone 13', '128GB', 'I need iPhone 13', now())`, s.req, s.conv, s.buyer)
	mustExec(t, db, `INSERT INTO vendors (id, business_name, whatsapp_external_id, status) VALUES ($1, 'Mike Gadgets', $2, 'ACTIVE')`, s.vendor, "v"+phone)
	mustExec(t, db, `INSERT INTO vendor_requests (id, buyer_request_id, vendor_id, public_reference, status, sent_at)
		VALUES ($1, $2, $3, $4, 'SENT', now())`, s.vr, s.req, s.vendor, "MM-"+phone[len(phone)-6:])
	mustExec(t, db, `INSERT INTO vendor_responses (id, vendor_request_id, vendor_id, status, review_state, price, raw_message)
		VALUES ($1, $2, $3, 'AVAILABLE', 'REVIEWED', 620000, 'Yes, 620k.')`, s.resp, s.vr, s.vendor)
	return s
}

func insertOffer(t *testing.T, db *sql.DB, s seeded, respID *uuid.UUID, score int, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var resp any
	if respID != nil {
		resp = respID.String()
	} else {
		r := uuid.New()
		mustExec(t, db, `INSERT INTO vendor_responses (id, vendor_request_id, vendor_id, status, review_state, price, raw_message)
			VALUES ($1, $2, $3, 'AVAILABLE', 'REVIEWED', 600000, 'x')`, r, s.vr, s.vendor)
		resp = r.String()
	}
	mustExec(t, db, `INSERT INTO offers (id, buyer_request_id, vendor_request_id, vendor_response_id, vendor_id, type, status, price, ranking_score)
		VALUES ($1, $2, $3, $4, $5, 'EXACT', $6, 620000, $7)`, id, s.req, s.vr, resp, s.vendor, status, score)
	return id
}

func TestIntegration_ConcurrentSelectionsYieldExactlyOneSelectionAndTransaction(t *testing.T) {
	db := testDB(t, migrationsPath())
	s := seed(t, db, "2348099000001")
	o1 := insertOffer(t, db, s, &s.resp, 60, "SENT")
	o2 := insertOffer(t, db, s, nil, 40, "SENT")
	repo := NewTransactionRepository(db)

	var wg sync.WaitGroup
	var mu sync.Mutex
	successes, conflicts := 0, 0
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			offerID := o1
			if i%2 == 1 {
				offerID = o2
			}
			_, _, err := repo.CreateSelectionWithTransaction(context.Background(),
				&transaction.Selection{BuyerRequestID: s.req, OfferID: offerID, BuyerID: s.buyer},
				&transaction.Transaction{BuyerRequestID: s.req, OfferID: offerID, BuyerID: s.buyer, VendorID: s.vendor, GrossAmount: 620000})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, transaction.ErrSelectionExists):
				conflicts++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	var nSel, nTx, nAccepted int
	db.QueryRow(`SELECT count(*) FROM buyer_selections WHERE buyer_request_id = $1`, s.req).Scan(&nSel)
	db.QueryRow(`SELECT count(*) FROM transactions WHERE buyer_request_id = $1`, s.req).Scan(&nTx)
	db.QueryRow(`SELECT count(*) FROM offers WHERE buyer_request_id = $1 AND status = 'ACCEPTED'`, s.req).Scan(&nAccepted)
	if successes != 1 || conflicts != 24 || nSel != 1 || nTx != 1 || nAccepted != 1 {
		t.Fatalf("expected exactly 1 success/selection/transaction/accepted offer; got successes=%d conflicts=%d selections=%d transactions=%d accepted=%d",
			successes, conflicts, nSel, nTx, nAccepted)
	}
}

func TestIntegration_ClaimForDeliveryIsExclusiveAndOrdinalsAreStable(t *testing.T) {
	db := testDB(t, migrationsPath())
	s := seed(t, db, "2348099000002")
	insertOffer(t, db, s, &s.resp, 90, "REVIEWED")
	insertOffer(t, db, s, nil, 50, "REVIEWED")
	insertOffer(t, db, s, nil, 70, "REVIEWED")
	repo := NewOfferRepository(db)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var claimed []*offer.Offer
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _, err := repo.ClaimForDelivery(context.Background(), s.req, 2*time.Minute)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			mu.Lock()
			claimed = append(claimed, c...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(claimed) != 3 {
		t.Fatalf("10 concurrent claimers must claim each offer exactly once; got %d claims", len(claimed))
	}
	seen := map[int]int{}
	for _, o := range claimed {
		seen[*o.PresentationOrdinal] = o.RankingScore
	}
	if len(seen) != 3 || seen[1] != 90 || seen[2] != 70 || seen[3] != 50 {
		t.Fatalf("expected ordinals 1..3 by ranking (90,70,50), got %v", seen)
	}

	// Failed delivery releases ordinals; a successful re-claim reuses 1..3.
	ids := []uuid.UUID{}
	for _, o := range claimed {
		ids = append(ids, o.ID)
	}
	if err := repo.MarkDeliveryFailed(context.Background(), ids, "131047"); err != nil {
		t.Fatal(err)
	}
	again, highest, err := repo.ClaimForDelivery(context.Background(), s.req, 2*time.Minute)
	if err != nil || len(again) != 3 || highest != 3 {
		t.Fatalf("expected re-claim of 3 with highest=3, got %d highest=%d err=%v", len(again), highest, err)
	}
	now := time.Now()
	if err := repo.MarkDelivered(context.Background(), ids, now, now.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// A new offer arriving later continues at 4; delivered ones are not re-claimed.
	insertOffer(t, db, s, nil, 99, "REVIEWED")
	later, highest, _ := repo.ClaimForDelivery(context.Background(), s.req, 2*time.Minute)
	if len(later) != 1 || *later[0].PresentationOrdinal != 4 || highest != 4 {
		t.Fatalf("late offer should be the only claim, as ordinal 4; got %d claims, highest=%d", len(later), highest)
	}

	ctxReq, err := repo.FindLatestPresentedRequest(context.Background(), s.conv)
	if err != nil || ctxReq == nil || *ctxReq != s.req {
		t.Fatalf("expected the delivered request as selection context, got %v err=%v", ctxReq, err)
	}
}

func TestIntegration_OneOfferPerVendorResponse(t *testing.T) {
	db := testDB(t, migrationsPath())
	s := seed(t, db, "2348099000003")
	repo := NewOfferRepository(db)
	mk := func() *offer.Offer {
		return &offer.Offer{BuyerRequestID: s.req, VendorRequestID: &s.vr, VendorResponseID: &s.resp, VendorID: s.vendor,
			Type: offer.TypeExact, Status: offer.StatusReviewed}
	}
	if _, err := repo.Create(context.Background(), mk()); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Create(context.Background(), mk()); !errors.Is(err, offer.ErrAlreadyExists) {
		t.Fatalf("second offer for the same VendorResponse: expected ErrAlreadyExists, got %v", err)
	}
}

func TestIntegration_RoutingStateAndReferenceErrors(t *testing.T) {
	db := testDB(t, migrationsPath())
	s := seed(t, db, "2348099000004")
	repo := NewRoutingRepository(db)
	ctx := context.Background()

	// Duplicate public reference vs duplicate vendor+request pair.
	other := uuid.New()
	mustExec(t, db, `INSERT INTO vendors (id, business_name, whatsapp_external_id, status) VALUES ($1, 'OJ', 'oj', 'ACTIVE')`, other)
	if _, err := repo.CreateVendorRequest(ctx, &routing.VendorRequest{BuyerRequestID: s.req, VendorID: other, PublicReference: "MM-123456"}); err != nil {
		t.Fatal(err)
	}
	second := seed(t, db, "2348099000009")
	if _, err := repo.CreateVendorRequest(ctx, &routing.VendorRequest{BuyerRequestID: second.req, VendorID: other, PublicReference: "MM-123456"}); !errors.Is(err, routing.ErrDuplicateReference) {
		t.Fatalf("expected ErrDuplicateReference, got %v", err)
	}
	if _, err := repo.CreateVendorRequest(ctx, &routing.VendorRequest{BuyerRequestID: s.req, VendorID: other, PublicReference: "MM-555555"}); !errors.Is(err, routing.ErrDuplicateVendorRequest) {
		t.Fatalf("expected ErrDuplicateVendorRequest, got %v", err)
	}

	// SEND_FAILED / PENDING requests are not "awaiting a reply".
	failed, _ := repo.CreateVendorRequest(ctx, &routing.VendorRequest{BuyerRequestID: seed(t, db, "2348099000005").req, VendorID: other, PublicReference: "MM-777001"})
	if err := repo.MarkSendFailed(ctx, failed.ID, "131047 re-engagement"); err != nil {
		t.Fatal(err)
	}
	active, _ := repo.ListActiveByVendor(ctx, other)
	if len(active) != 0 {
		t.Fatalf("PENDING/SEND_FAILED must not be active; got %d (first status %s)", len(active), active[0].Status)
	}
	got, _ := repo.FindVendorRequestByID(ctx, failed.ID)
	if got.Status != routing.VendorRequestSendFailed || got.SendError == nil || got.SendFailedAt == nil {
		t.Fatalf("expected SEND_FAILED with error+timestamp, got %+v", got)
	}
}

func TestIntegration_ConfirmTransitionAndDuplicateMessageGate(t *testing.T) {
	db := testDB(t, migrationsPath())
	s := seed(t, db, "2348099000006")
	ctx := context.Background()

	extracting := uuid.New()
	mustExec(t, db, `INSERT INTO buyer_requests (id, conversation_id, buyer_id, status, raw_message) VALUES ($1, $2, $3, 'EXTRACTING', 'x')`, extracting, s.conv, s.buyer)
	reqRepo := NewRequestRepository(db)
	if _, err := reqRepo.Confirm(ctx, extracting); !errors.Is(err, request.ErrInvalidTransition) {
		t.Fatalf("confirming an EXTRACTING request: expected ErrInvalidTransition, got %v", err)
	}
	if r, err := reqRepo.Confirm(ctx, s.req); err != nil || r.Status != request.StatusConfirmed {
		t.Fatalf("re-confirming a CONFIRMED request should be an idempotent no-op, got %v / %v", r, err)
	}

	convRepo := NewConversationRepository(db)
	msg := func() *conversation.Message {
		return &conversation.Message{ConversationID: s.conv, Direction: conversation.DirectionInbound, ExternalMessageID: "wamid.DUP", MessageType: "text", Content: map[string]any{"text": "1"}}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, dup := 0, 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := convRepo.SaveMessage(ctx, msg())
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else if errors.Is(err, conversation.ErrDuplicateMessage) {
				dup++
			} else {
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || dup != 9 {
		t.Fatalf("10 concurrent deliveries of one wamid: expected 1 saved / 9 duplicates, got %d / %d", ok, dup)
	}
}

// Migration 0006 must apply cleanly to a database that already contains the
// pre-3.1 defects (duplicate selections/transactions, duplicate offers,
// PENDING vendor requests from failed sends), preserving every row.
func TestIntegration_Migration0006OnDataWithPre31Duplicates(t *testing.T) {
	pre31 := t.TempDir()
	entries, _ := os.ReadDir(migrationsPath())
	for _, e := range entries {
		if e.Name() >= "0006" { // the pre-3.1 schema: 0001-0005 only
			continue
		}
		b, _ := os.ReadFile(filepath.Join(migrationsPath(), e.Name()))
		os.WriteFile(filepath.Join(pre31, e.Name()), b, 0o644)
	}
	db := testDB(t, pre31)
	s := seed(t, db, "2348099000007")

	// Pre-3.1 defects: two offers for one response, three selections +
	// transactions for one request, a PENDING (failed-send) vendor request.
	o1, o2 := uuid.New(), uuid.New()
	for i, id := range []uuid.UUID{o1, o2} {
		mustExec(t, db, `INSERT INTO offers (id, buyer_request_id, vendor_request_id, vendor_response_id, vendor_id, type, status, price, created_at)
			VALUES ($1, $2, $3, $4, $5, 'EXACT', 'REVIEWED', 620000, now() + make_interval(secs => $6))`, id, s.req, s.vr, s.resp, s.vendor, i)
	}
	for i := 0; i < 3; i++ {
		sel, tx := uuid.New(), uuid.New()
		mustExec(t, db, `INSERT INTO buyer_selections (id, buyer_request_id, offer_id, buyer_id, created_at) VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))`, sel, s.req, o1, s.buyer, i)
		mustExec(t, db, `INSERT INTO transactions (id, buyer_selection_id, buyer_request_id, offer_id, buyer_id, vendor_id, gross_amount) VALUES ($1, $2, $3, $4, $5, $6, 620000)`, tx, sel, s.req, o1, s.buyer, s.vendor)
	}
	pending := uuid.New()
	mustExec(t, db, `INSERT INTO vendor_requests (id, buyer_request_id, vendor_id, public_reference, status) VALUES ($1, $2, $3, 'MM-4242', 'PENDING')`,
		pending, seed(t, db, "2348099000008").req, s.vendor)

	if err := RunMigrations(context.Background(), db, migrationsPath()); err != nil {
		t.Fatalf("0006 failed on pre-3.1 data: %v", err)
	}

	count := func(q string, args ...any) int {
		var n int
		if err := db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM offers WHERE buyer_request_id = $1`, s.req); n != 2 {
		t.Fatalf("no offer may be deleted; got %d", n)
	}
	if n := count(`SELECT count(*) FROM offers WHERE id = $1 AND status = 'DUPLICATE'`, o2); n != 1 {
		t.Fatal("the later duplicate offer should be marked DUPLICATE")
	}
	if a, v := count(`SELECT count(*) FROM buyer_selections WHERE buyer_request_id = $1 AND status = 'ACTIVE'`, s.req),
		count(`SELECT count(*) FROM buyer_selections WHERE buyer_request_id = $1 AND status = 'VOID'`, s.req); a != 1 || v != 2 {
		t.Fatalf("expected 1 ACTIVE + 2 VOID selections, got %d/%d", a, v)
	}
	if n := count(`SELECT count(*) FROM transactions WHERE buyer_request_id = $1 AND status = 'CANCELLED'`, s.req); n != 2 {
		t.Fatalf("voided selections' transaction shells should be CANCELLED, got %d", n)
	}
	if n := count(`SELECT count(*) FROM audit_logs WHERE action = 'BUYER_SELECTION_VOIDED_BY_MIGRATION'`); n != 2 {
		t.Fatalf("each voided selection must be audited, got %d", n)
	}
	if n := count(`SELECT count(*) FROM vendor_requests WHERE id = $1 AND status = 'SEND_FAILED'`, pending); n != 1 {
		t.Fatal("pre-3.1 PENDING vendor request should become SEND_FAILED")
	}
	if _, err := db.Exec(`INSERT INTO buyer_selections (id, buyer_request_id, offer_id, buyer_id) VALUES ($1, $2, $3, $4)`, uuid.New(), s.req, o1, s.buyer); err == nil {
		t.Fatal("after 0006 a second ACTIVE selection must be rejected by the DB")
	}
}

// --- §23A: delivery receipts and the funnel ---

func TestIntegration_DeliveryReceiptsAreIdempotentAndFeedTheFunnel(t *testing.T) {
	db := testDB(t, migrationsPath())
	s := seed(t, db, "2348099000010")
	ctx := context.Background()
	convRepo := NewConversationRepository(db)

	// Outbound offer message persisted the way RecordingSender does it.
	if _, err := convRepo.SaveMessage(ctx, &conversation.Message{
		ConversationID: s.conv, Direction: conversation.DirectionOutbound, ExternalMessageID: "wamid.OFFER1",
		MessageType: "text", Content: map[string]any{"text": "I found 1 option…"},
		Metadata: map[string]any{"kind": "offer_delivery", "entity_type": "buyer_request", "entity_id": s.req.String()},
	}); err != nil {
		t.Fatal(err)
	}

	t0 := time.Now().UTC().Truncate(time.Second)
	tr, err := convRepo.ApplyDeliveryStatus(ctx, "wamid.OFFER1", "delivered", t0, "")
	if err != nil || tr == nil || !tr.FirstDelivered || tr.FirstRead || tr.Message.Metadata["kind"] != "offer_delivery" {
		t.Fatalf("first delivered: %+v err=%v", tr, err)
	}
	tr, _ = convRepo.ApplyDeliveryStatus(ctx, "wamid.OFFER1", "delivered", t0.Add(time.Minute), "")
	if tr.FirstDelivered {
		t.Fatal("a repeated receipt must not be a first transition")
	}
	tr, _ = convRepo.ApplyDeliveryStatus(ctx, "wamid.OFFER1", "read", t0.Add(2*time.Minute), "")
	if !tr.FirstRead || tr.FirstDelivered {
		t.Fatalf("read after delivered: expected FirstRead only, got %+v", tr)
	}
	if tr, _ := convRepo.ApplyDeliveryStatus(ctx, "wamid.NOPE", "read", t0, ""); tr != nil {
		t.Fatal("receipts for unknown messages must be ignored")
	}

	var status string
	var deliveredAt, readAt time.Time
	db.QueryRow(`SELECT delivery_status, delivered_at, read_at FROM messages WHERE external_message_id = 'wamid.OFFER1'`).Scan(&status, &deliveredAt, &readAt)
	if status != "read" || !deliveredAt.Equal(t0) || !readAt.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("expected status read, delivered_at kept at first receipt; got %s %v %v", status, deliveredAt, readAt)
	}

	// Funnel counts it.
	stages, err := NewFunnelRepository(db).Funnel(ctx, t0.Add(-24*time.Hour), t0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, st := range stages {
		got[st.Key] = st.Count
	}
	if got["buyer_request_created"] != 1 || got["buyer_request_confirmed"] != 1 || got["vendor_request_created"] != 1 ||
		got["vendor_request_sent"] != 1 || got["vendor_response_received"] != 1 ||
		got["offer_message_delivered"] != 1 || got["offer_message_read"] != 1 || got["transaction_completed"] != 0 {
		t.Fatalf("unexpected funnel counts: %v", got)
	}
	if len(stages) != 16 { // 16th: deal_confirmed (01a audit)
		t.Fatalf("expected 16 funnel stages, got %d", len(stages))
	}
}

func TestIntegration_SelectionSourceIsPersisted(t *testing.T) {
	db := testDB(t, migrationsPath())
	s := seed(t, db, "2348099000011")
	o := insertOffer(t, db, s, &s.resp, 60, "SENT")
	repo := NewTransactionRepository(db)
	if _, _, err := repo.CreateSelectionWithTransaction(context.Background(),
		&transaction.Selection{BuyerRequestID: s.req, OfferID: o, BuyerID: s.buyer, Source: transaction.SourceWhatsApp},
		&transaction.Transaction{BuyerRequestID: s.req, OfferID: o, BuyerID: s.buyer, VendorID: s.vendor}); err != nil {
		t.Fatal(err)
	}
	sel, _ := repo.FindSelectionByBuyerRequest(context.Background(), s.req)
	if sel == nil || sel.Source != transaction.SourceWhatsApp {
		t.Fatalf("expected source whatsapp_reply, got %+v", sel)
	}
}
