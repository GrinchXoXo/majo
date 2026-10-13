package postgres

// Reuse-audit R1 integration tests: named operator credentials and the
// control-plane read views (event timeline, job queue), against real Postgres.

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/access"
	"github.com/middleman/backend/internal/controlplane"
	"github.com/middleman/backend/internal/httpserver"
)

func TestIntegrationOperatorCredentials(t *testing.T) {
	db := testDB(t, migrationsPath())
	ctx := context.Background()
	repo := NewOperatorRepository(db)

	if n, err := repo.CountActive(ctx); err != nil || n != 0 {
		t.Fatalf("fresh db: %d %v", n, err)
	}
	ada, adaTok, err := repo.Create(ctx, "Ada", access.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	_, bobTok, err := repo.Create(ctx, " Bob ", access.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(adaTok, "majo_op_") || len(adaTok) != len("majo_op_")+64 || adaTok == bobTok {
		t.Fatalf("token shape: %q %q", adaTok, bobTok)
	}
	if _, _, err := repo.Create(ctx, "Ada", access.RoleViewer); err == nil {
		t.Fatal("duplicate name must be refused")
	}
	if _, _, err := repo.Create(ctx, "Eve", access.Role("ADMIN")); err == nil {
		t.Fatal("unknown role must be refused")
	}
	// Only the hash is stored.
	var hashed, clear int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE token_hash = sha256(convert_to($1, 'UTF8'))),
		count(*) FILTER (WHERE row_to_json(operators)::text LIKE '%' || $1 || '%') FROM operators`, adaTok).Scan(&hashed, &clear); err != nil {
		t.Fatal(err)
	}
	if hashed != 1 || clear != 0 {
		t.Fatalf("token must be stored only as its SHA-256 hash: hashed=%d clear=%d", hashed, clear)
	}

	p, ok, err := repo.FindActiveByTokenHash(ctx, sha256.Sum256([]byte(adaTok)))
	if err != nil || !ok || p.Name != "Ada" || p.Role != access.RoleOperator || p.ID == nil || *p.ID != ada.ID {
		t.Fatalf("lookup ada: %+v %v %v", p, ok, err)
	}
	// A second lookup inside the minute does not need the UPDATE path.
	if p2, ok, err := repo.FindActiveByTokenHash(ctx, sha256.Sum256([]byte(adaTok))); err != nil || !ok || p2.Name != "Ada" {
		t.Fatalf("second lookup: %v %v", ok, err)
	}
	if _, ok, _ := repo.FindActiveByTokenHash(ctx, sha256.Sum256([]byte("majo_op_wrong"))); ok {
		t.Fatal("unknown token resolved")
	}
	if n, _ := repo.CountActive(ctx); n != 2 {
		t.Fatalf("active=%d", n)
	}

	// Through the HTTP guard: viewer reads, cannot write; operator writes.
	auth := httpserver.NewOperatorAuth("shared-break-glass-token")
	auth.SetStore(repo, 2, nil)
	ok204 := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }
	call := func(h http.HandlerFunc, tok string) int {
		req := httptest.NewRequest("GET", "/api/x", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}
	if c := call(auth.RequireViewer(ok204), bobTok); c != 204 {
		t.Fatalf("viewer read: %d", c)
	}
	if c := call(auth.Require(ok204), bobTok); c != 403 {
		t.Fatalf("viewer write: %d", c)
	}
	if c := call(auth.Require(ok204), adaTok); c != 204 {
		t.Fatalf("operator write: %d", c)
	}

	// Revocation takes effect immediately.
	if err := repo.Revoke(ctx, "Bob"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Revoke(ctx, "Bob"); !errors.Is(err, ErrOperatorNotFound) {
		t.Fatalf("double revoke: %v", err)
	}
	if c := call(auth.RequireViewer(ok204), bobTok); c != 401 {
		t.Fatalf("revoked viewer: %d", c)
	}
	ops, err := repo.List(ctx)
	if err != nil || len(ops) != 2 || !ops[0].Active || ops[1].Active || ops[1].RevokedAt == nil {
		t.Fatalf("list: %+v %v", ops, err)
	}
}

func TestIntegrationControlPlaneViews(t *testing.T) {
	db := testDB(t, migrationsPath())
	ctx := context.Background()
	ops := NewOperatorRepository(db)
	ada, _, err := ops.Create(ctx, "Ada", access.RoleOperator)
	if err != nil {
		t.Fatal(err)
	}
	cp := NewControlPlaneRepository(db)
	ent := uuid.New()
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 7; i++ {
		et := "REQUEST_CONFIRMED"
		if i%2 == 1 {
			et = "VENDORS_ROUTED"
		}
		mustExec(t, db, `INSERT INTO events (event_type, entity_type, entity_id, actor_type, actor_id, created_at) VALUES ($1, 'buyer_request', $2, 'OPERATOR', $3, $4)`,
			et, ent, ada.ID, base.Add(time.Duration(i)*time.Minute))
	}
	mustExec(t, db, `INSERT INTO events (event_type, entity_type, actor_type, created_at) VALUES ('VENDOR_CREATED', 'vendor', 'SYSTEM', $1)`, base.Add(time.Hour))

	rows, total, err := cp.ListEvents(ctx, controlplane.EventFilter{}, 3, 0)
	if err != nil || total != 8 || len(rows) != 3 || rows[0].EventType != "VENDOR_CREATED" {
		t.Fatalf("page 1: total=%d rows=%d err=%v", total, len(rows), err)
	}
	if rows[1].ActorName == nil || *rows[1].ActorName != "Ada" {
		t.Fatalf("actor name not joined: %+v", rows[1])
	}
	rows2, _, _ := cp.ListEvents(ctx, controlplane.EventFilter{}, 3, 3)
	if len(rows2) != 3 || !rows2[0].CreatedAt.Before(rows[2].CreatedAt) {
		t.Fatal("page 2 must continue newest-first without overlap")
	}
	f, total, err := cp.ListEvents(ctx, controlplane.EventFilter{EventType: "VENDORS_ROUTED", EntityID: &ent}, 50, 0)
	if err != nil || total != 3 || len(f) != 3 {
		t.Fatalf("filtered: %d %d %v", total, len(f), err)
	}
	types, _ := cp.EventTypes(ctx)
	if got := strings.Join(types, ","); len(types) != 3 || !strings.Contains(got, "REQUEST_CONFIRMED") || !strings.Contains(got, "VENDOR_CREATED") || !strings.Contains(got, "VENDORS_ROUTED") {
		t.Fatalf("types: %v", types)
	}

	mustExec(t, db, `INSERT INTO jobs (job_type, idempotency_key, status, max_attempts, run_at) VALUES ('MEDIA_DOWNLOAD', 'a', 'PENDING', 3, now() - interval '10 minutes')`)
	mustExec(t, db, `INSERT INTO jobs (job_type, idempotency_key, status, max_attempts, last_error, completed_at) VALUES ('MEDIA_DOWNLOAD', 'b', 'FAILED', 3, 'boom', now())`)
	mustExec(t, db, `INSERT INTO jobs (job_type, idempotency_key, status, max_attempts, completed_at) VALUES ('VENDOR_REQUEST_TIMEOUT', 'c', 'SUCCEEDED', 3, now())`)
	jobs, total, err := cp.ListJobs(ctx, controlplane.JobFilter{Status: "FAILED"}, 50, 0)
	if err != nil || total != 1 || jobs[0].LastError == nil || *jobs[0].LastError != "boom" {
		t.Fatalf("failed jobs: %+v %d %v", jobs, total, err)
	}
	sum, err := cp.JobSummary(ctx)
	if err != nil || len(sum) != 2 || sum[0].JobType != "MEDIA_DOWNLOAD" || sum[0].Pending != 1 || sum[0].Failed != 1 || sum[0].OverdueSeconds < 590 {
		t.Fatalf("summary: %+v %v", sum, err)
	}
	if sum[1].Succeeded != 1 || sum[1].OverdueSeconds != 0 {
		t.Fatalf("summary row 2: %+v", sum[1])
	}
	if n, err := cp.CountRequests(ctx, ""); err != nil || n != 0 {
		t.Fatalf("count requests: %d %v", n, err)
	}
}
