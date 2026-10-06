package postgres

// Phase 4A.1 Postgres integration tests: durable jobs (claim, SKIP LOCKED,
// lease reclaim, retry, terminal failure) and media ingestion end to end
// (signed Meta-format webhook -> MediaRef -> MEDIA_DOWNLOAD job -> worker ->
// fake Graph media endpoint -> validation -> SHA-256 -> local
// storage.Provider -> media_assets -> MEDIA_ASSET_STORED).
//
// Real PostgreSQL, real webhook handler, real WhatsApp provider code, real
// local storage provider on a temp dir. Only Meta itself is faked
// (whatsapptest.FakeGraph). Gated by MAJO_TEST_DATABASE_URL like the rest.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/infrastructure/storage"
	"github.com/middleman/backend/infrastructure/whatsapp"
	"github.com/middleman/backend/infrastructure/whatsapp/whatsapptest"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/httpserver"
	"github.com/middleman/backend/internal/jobs"
	"github.com/middleman/backend/internal/media"
)

const (
	mediaToken  = "it-access-token"
	mediaSecret = "it-app-secret"
)

var (
	photo1 = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("ladies-bag-front")...)
	photo2 = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("ladies-bag-side")...)
	photo3 = append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, []byte("ladies-bag-inside")...)
	clip   = append([]byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'm', 'p', '4', '2'}, []byte("bag-video")...)
)

// flakyStorage wraps the real provider and fails the next N Puts.
type flakyStorage struct {
	storage.Provider
	failPuts atomic.Int32
	puts     atomic.Int32
}

func (f *flakyStorage) Put(ctx context.Context, key string, data []byte, ct string) error {
	f.puts.Add(1)
	if f.failPuts.Load() > 0 {
		f.failPuts.Add(-1)
		return errors.New("storage temporarily unavailable")
	}
	return f.Provider.Put(ctx, key, data, ct)
}

// flakyAssets wraps the real repository and fails the next N StoreAndLinks
// (simulates the database going away after the object was stored).
type flakyAssets struct {
	*MediaAssetRepository
	failStores atomic.Int32
}

func (f *flakyAssets) StoreAndLink(ctx context.Context, a *media.Asset, msg uuid.UUID, ev *event.Event) (*media.Asset, bool, error) {
	if f.failStores.Load() > 0 {
		f.failStores.Add(-1)
		return nil, false, errors.New("driver: bad connection")
	}
	return f.MediaAssetRepository.StoreAndLink(ctx, a, msg, ev)
}

type mediaEnv struct {
	t           *testing.T
	db          *sql.DB
	graph       *whatsapptest.FakeGraph
	root        string
	store       *flakyStorage
	assets      *flakyAssets
	jobs        *JobRepository
	webhook     *httpserver.WhatsAppWebhookHandler
	maxAttempts int
	backoff     time.Duration
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newMediaEnv(t *testing.T) *mediaEnv {
	t.Helper()
	db := testDB(t, migrationsPath())
	g := whatsapptest.New(mediaToken)
	t.Cleanup(g.Close)
	root := t.TempDir()
	local, err := storage.NewLocalProvider(root)
	if err != nil {
		t.Fatal(err)
	}
	e := &mediaEnv{t: t, db: db, graph: g, root: root, store: &flakyStorage{Provider: local},
		assets: &flakyAssets{MediaAssetRepository: NewMediaAssetRepository(db)}, jobs: NewJobRepository(db),
		maxAttempts: 5, backoff: 5 * time.Millisecond}
	conv := NewConversationRepository(db)
	e.webhook = httpserver.NewWhatsAppWebhookHandler(e.provider(), conv, conv, NewBuyerRepository(db), NewVendorRepository(db),
		NewEventWriter(db), nil, nil, nil, "verify", false, discard())
	e.webhook.SetMediaIngestion(e.ingestor())
	return e
}

func (e *mediaEnv) provider() *whatsapp.CloudAPIProvider {
	return whatsapp.NewCloudAPIProvider(e.graph.URL(), mediaToken, "PHONE", "verify", mediaSecret)
}

// ingestor builds a fresh Ingestor, as a newly started process would.
func (e *mediaEnv) ingestor() *media.Ingestor {
	return media.NewIngestor(e.jobs, e.assets, e.store, map[string]media.Downloader{"whatsapp": e.provider()},
		NewEventWriter(e.db), media.Limits{MaxImageBytes: 1024, MaxVideoBytes: 4096}, e.maxAttempts, discard())
}

func (e *mediaEnv) worker(id string, lease time.Duration) *jobs.Worker {
	ing := e.ingestor()
	w := jobs.NewWorker(e.jobs, jobs.WorkerConfig{ID: id, PollInterval: 5 * time.Millisecond, Lease: lease,
		Retry: jobs.RetryPolicy{BaseDelay: e.backoff, MaxDelay: 4 * e.backoff}}, discard())
	w.Register(media.JobTypeDownload, ing.Handle, ing.OnFailure)
	return w
}

// drain runs a worker until no MEDIA_DOWNLOAD job is PENDING/RUNNING.
func (e *mediaEnv) drain(w *jobs.Worker) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ran, err := w.RunOnce(context.Background(), "drain")
		if err != nil {
			e.t.Fatalf("worker: %v", err)
		}
		if !ran {
			if e.count(`SELECT count(*) FROM jobs WHERE status IN ('PENDING','RUNNING')`) == 0 {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	e.t.Fatal("jobs did not drain")
}

func (e *mediaEnv) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (e *mediaEnv) vendor(name, wa string) uuid.UUID {
	id := uuid.New()
	mustExec(e.t, e.db, `INSERT INTO vendors (id, business_name, whatsapp_external_id, status) VALUES ($1, $2, $3, 'ACTIVE')`, id, name, wa)
	return id
}

func mediaPayload(from, wamid, kind, mediaID, mime, caption string) string {
	return fmt.Sprintf(`{"entry":[{"changes":[{"value":{"contacts":[{"wa_id":%q,"profile":{"name":"Vendor"}}],"messages":[{"from":%q,"id":%q,"timestamp":"1700000000","type":%q,%q:{"id":%q,"mime_type":%q,"caption":%q}}]}}]}]}`,
		from, from, wamid, kind, kind, mediaID, mime, caption)
}

// post delivers a Meta-signed webhook through the real handler.
func (e *mediaEnv) post(body string) {
	e.t.Helper()
	mac := hmac.New(sha256.New, []byte(mediaSecret))
	mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/webhooks/whatsapp", bytes.NewBufferString(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	e.webhook.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("webhook returned %d", rec.Code)
	}
}

func (e *mediaEnv) messageID(wamid string) uuid.UUID {
	var s string
	if err := e.db.QueryRow(`SELECT id FROM messages WHERE external_message_id = $1`, wamid).Scan(&s); err != nil {
		e.t.Fatalf("message %s: %v", wamid, err)
	}
	return uuid.MustParse(s)
}

func (e *mediaEnv) linkedAsset(wamid string) *media.Asset {
	e.t.Helper()
	var s sql.NullString
	e.db.QueryRow(`SELECT media_asset_id FROM messages WHERE external_message_id = $1`, wamid).Scan(&s)
	if !s.Valid {
		return nil
	}
	a, err := e.assets.FindByID(context.Background(), uuid.MustParse(s.String))
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *mediaEnv) files() []string {
	var out []string
	_ = filepath.WalkDir(e.root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(e.root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

func (e *mediaEnv) job(wamid string) *jobs.Job {
	j, err := e.jobs.FindByKey(context.Background(), media.JobTypeDownload, e.messageID(wamid).String())
	if err != nil || j == nil {
		e.t.Fatalf("job for %s: %v %v", wamid, j, err)
	}
	return j
}

// --- 37.1 / 37.2 / 37.16 / §55 acceptance scenario ------------------------------

func TestIntegration_MediaAcceptance_ThreePhotosStoredWithProvenance(t *testing.T) {
	e := newMediaEnv(t)
	vendor := e.vendor("Ada Bags", "2348031110001")
	e.graph.AddMedia("wa-media-1", photo1, "image/jpeg")
	e.graph.AddMedia("wa-media-2", photo2, "image/jpeg")
	e.graph.AddMedia("wa-media-3", photo3, "image/png")

	payloads := []string{
		mediaPayload("2348031110001", "wamid.P1", "image", "wa-media-1", "image/jpeg", "Imported ladies bags ₦18,500"),
		mediaPayload("2348031110001", "wamid.P2", "image", "wa-media-2", "image/jpeg", ""),
		mediaPayload("2348031110001", "wamid.P3", "image", "wa-media-3", "image/png", ""),
	}
	for _, p := range payloads {
		e.post(p)
	}
	e.post(payloads[0]) // Meta redelivers the first message

	if n := e.count(`SELECT count(*) FROM messages WHERE direction = 'INBOUND'`); n != 3 {
		t.Fatalf("expected 3 inbound messages, got %d", n)
	}
	if n := e.count(`SELECT count(*) FROM jobs WHERE job_type = 'MEDIA_DOWNLOAD' AND status = 'PENDING'`); n != 3 {
		t.Fatalf("expected 3 durable jobs before any worker ran, got %d", n)
	}
	if n := e.count(`SELECT count(*) FROM media_assets`); n != 0 {
		t.Fatal("the webhook must not download media itself")
	}

	e.drain(e.worker("w1", time.Minute))

	want := map[string][]byte{"wamid.P1": photo1, "wamid.P2": photo2, "wamid.P3": photo3}
	ids := map[string]string{"wamid.P1": "wa-media-1", "wamid.P2": "wa-media-2", "wamid.P3": "wa-media-3"}
	for wamid, data := range want {
		a := e.linkedAsset(wamid)
		if a == nil {
			t.Fatalf("%s: no asset linked", wamid)
		}
		sha := media.SHA256Hex(data)
		if a.OwnerVendorID != vendor || a.SourceMessageID != e.messageID(wamid) || a.ProviderMediaID != ids[wamid] ||
			a.Provider != "whatsapp" || a.SHA256 != sha || a.SizeBytes != int64(len(data)) || a.Kind != media.KindImage ||
			a.Status != media.StatusAvailable || a.ProviderMediaExpiresAt != nil || a.StoredAt.IsZero() {
			t.Fatalf("%s: asset fields wrong: %+v", wamid, a)
		}
		stored, err := os.ReadFile(filepath.Join(e.root, filepath.FromSlash(a.StorageKey)))
		if err != nil || media.SHA256Hex(stored) != sha {
			t.Fatalf("%s: stored object missing or wrong: %v", wamid, err)
		}
		if j := e.job(wamid); j.Status != jobs.StatusSucceeded || j.Attempts != 1 || j.CompletedAt == nil {
			t.Fatalf("%s: job %+v", wamid, j)
		}
	}
	if a := e.linkedAsset("wamid.P3"); a.MimeType != "image/png" || !strings.HasSuffix(a.StorageKey, ".png") {
		t.Fatalf("mime must come from the bytes: %+v", a)
	}
	if n := e.count(`SELECT count(*) FROM media_assets`); n != 3 {
		t.Fatalf("expected 3 assets, got %d", n)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE event_type = 'MEDIA_ASSET_STORED'`); n != 3 {
		t.Fatalf("expected 3 MEDIA_ASSET_STORED events, got %d", n)
	}
	// Event payload carries the identifiers and points at the asset.
	if n := e.count(`SELECT count(*) FROM events ev JOIN media_assets a ON a.id = ev.entity_id
		WHERE ev.event_type = 'MEDIA_ASSET_STORED' AND ev.entity_type = 'media_asset'
		  AND ev.payload->>'media_asset_id' = a.id::text AND ev.payload->>'source_message_id' = a.source_message_id::text
		  AND ev.payload->>'vendor_id' = a.owner_vendor_id::text AND ev.payload->>'sha256' = a.sha256
		  AND (ev.payload->>'size_bytes')::bigint = a.size_bytes AND ev.payload->>'mime_type' = a.mime_type
		  AND ev.payload->>'kind' = a.kind AND ev.payload->>'provider_media_id' = a.provider_media_id`); n != 3 {
		t.Fatalf("event payloads do not match their assets (%d/3)", n)
	}
	// Traceable: asset -> message -> WhatsApp sender, and asset -> vendor.
	if n := e.count(`SELECT count(*) FROM media_assets a JOIN messages m ON m.id = a.source_message_id
		JOIN conversations c ON c.id = m.conversation_id JOIN vendors v ON v.id = a.owner_vendor_id
		WHERE c.external_id = '2348031110001' AND v.whatsapp_external_id = c.external_id`); n != 3 {
		t.Fatalf("provenance chain broken (%d/3)", n)
	}
	// The caption stays on the MediaRef; nothing became supply.
	if n := e.count(`SELECT count(*) FROM supply_signals`) + e.count(`SELECT count(*) FROM supply_listings`); n != 0 {
		t.Fatal("4A.1 must not create supply signals or listings")
	}

	// Everything redelivered after processing: still one of everything.
	for _, p := range payloads {
		e.post(p)
	}
	e.drain(e.worker("w2", time.Minute))
	if e.count(`SELECT count(*) FROM jobs`) != 3 || e.count(`SELECT count(*) FROM media_assets`) != 3 ||
		e.count(`SELECT count(*) FROM events WHERE event_type = 'MEDIA_ASSET_STORED'`) != 3 || len(e.files()) != 3 {
		t.Fatal("redelivery created duplicate jobs, assets, events or objects")
	}
}

// --- 37.3 / 37.4 --------------------------------------------------------------

func TestIntegration_MediaDedupeIsVendorScoped(t *testing.T) {
	e := newMediaEnv(t)
	vA := e.vendor("Vendor A", "2348031110011")
	vB := e.vendor("Vendor B", "2348031110022")
	for _, id := range []string{"a-1", "a-2", "b-1"} {
		e.graph.AddMedia(id, photo1, "image/jpeg") // identical bytes everywhere
	}
	e.post(mediaPayload("2348031110011", "wamid.A1", "image", "a-1", "image/jpeg", ""))
	e.post(mediaPayload("2348031110011", "wamid.A2", "image", "a-2", "image/jpeg", "same pic again"))
	e.post(mediaPayload("2348031110022", "wamid.B1", "image", "b-1", "image/jpeg", ""))
	e.drain(e.worker("w", time.Minute))

	a1, a2, b1 := e.linkedAsset("wamid.A1"), e.linkedAsset("wamid.A2"), e.linkedAsset("wamid.B1")
	if a1 == nil || a2 == nil || b1 == nil {
		t.Fatal("every message must resolve to an asset")
	}
	if a1.ID != a2.ID || a1.SourceMessageID != e.messageID("wamid.A1") || a1.OwnerVendorID != vA {
		t.Fatalf("same vendor + same bytes must be one asset sourced from the first message: %+v %+v", a1, a2)
	}
	if b1.ID == a1.ID || b1.OwnerVendorID != vB || b1.StorageKey == a1.StorageKey || b1.SHA256 != a1.SHA256 ||
		b1.SourceMessageID != e.messageID("wamid.B1") {
		t.Fatalf("vendor B must own a separate asset: %+v", b1)
	}
	if e.count(`SELECT count(*) FROM media_assets`) != 2 || e.count(`SELECT count(*) FROM events WHERE event_type = 'MEDIA_ASSET_STORED'`) != 2 || len(e.files()) != 2 {
		t.Fatal("expected exactly 2 assets / 2 events / 2 objects")
	}
	// The database enforces the boundary, not just the application.
	_, err := e.db.Exec(`INSERT INTO media_assets (owner_vendor_id, kind, mime_type, size_bytes, sha256, storage_key, source_message_id, provider, provider_media_id)
		VALUES ($1, 'image', 'image/jpeg', 1, $2, 'other-key', $3, 'whatsapp', 'x')`, vA, a1.SHA256, e.messageID("wamid.A2"))
	if err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("expected (owner_vendor_id, sha256) unique violation, got %v", err)
	}
}

// --- 37.5 / 37.7 / retries exhausted ----------------------------------------------

func TestIntegration_MediaTransientFailuresRetryThenSucceed(t *testing.T) {
	e := newMediaEnv(t)
	e.vendor("V", "2348031110031")
	e.graph.AddMedia("m", photo1, "image/jpeg")
	e.graph.FailMetadata("m", 503, 503)
	e.post(mediaPayload("2348031110031", "wamid.R1", "image", "m", "image/jpeg", ""))

	w := e.worker("w", time.Minute)
	if ran, _ := w.RunOnce(context.Background(), "w/0"); !ran {
		t.Fatal("expected a due job")
	}
	j := e.job("wamid.R1")
	if j.Status != jobs.StatusPending || j.Attempts != 1 || !strings.Contains(j.LastError, "503") || !j.RunAt.After(j.CreatedAt) {
		t.Fatalf("after first 503: expected durable retry, got %+v", j)
	}
	e.drain(w)
	j = e.job("wamid.R1")
	if j.Status != jobs.StatusSucceeded || j.Attempts != 3 || j.LastError != "" {
		t.Fatalf("expected success on attempt 3, got %+v", j)
	}
	if e.count(`SELECT count(*) FROM media_assets`) != 1 {
		t.Fatal("expected exactly one asset")
	}
}

func TestIntegration_MediaRateLimitSchedulesRetry(t *testing.T) {
	e := newMediaEnv(t)
	e.backoff = time.Hour
	e.vendor("V", "2348031110041")
	e.graph.AddMedia("m", photo1, "image/jpeg")
	e.graph.FailMetadata("m", 429)
	e.post(mediaPayload("2348031110041", "wamid.RL", "image", "m", "image/jpeg", ""))
	_, _ = e.worker("w", time.Minute).RunOnce(context.Background(), "w/0")
	j := e.job("wamid.RL")
	if j.Status != jobs.StatusPending || j.Attempts != 1 || !strings.Contains(j.LastError, "429") ||
		j.RunAt.Before(time.Now().Add(20*time.Minute)) {
		t.Fatalf("expected a retry scheduled ~30-60 min out, got %+v", j)
	}
	if ran, _ := e.worker("w2", time.Minute).RunOnce(context.Background(), "w2/0"); ran {
		t.Fatal("job must not run before its backoff elapses")
	}
}

func TestIntegration_MediaRetriesAreBounded(t *testing.T) {
	e := newMediaEnv(t)
	e.maxAttempts = 3
	e.webhook.SetMediaIngestion(e.ingestor()) // max_attempts is fixed on the job at enqueue time
	e.vendor("V", "2348031110051")
	e.graph.AddMedia("m", photo1, "image/jpeg")
	e.graph.FailMetadata("m", 503, 503, 503, 503, 503)
	e.post(mediaPayload("2348031110051", "wamid.X", "image", "m", "image/jpeg", ""))
	e.drain(e.worker("w", time.Minute))
	j := e.job("wamid.X")
	if j.Status != jobs.StatusFailed || j.Attempts != 3 || !strings.HasPrefix(j.LastError, "attempts exhausted (3/3)") || j.CompletedAt == nil {
		t.Fatalf("expected terminal failure after 3 attempts, got %+v", j)
	}
	if e.count(`SELECT count(*) FROM events WHERE event_type = 'MEDIA_INGESTION_FAILED' AND (payload->>'attempts')::int = 3`) != 1 {
		t.Fatal("expected one MEDIA_INGESTION_FAILED event recording 3 attempts")
	}
}

// --- 37.6 / 37.11 / 37.12 / 37.13 / §57 expired URL --------------------------------

func TestIntegration_MediaPermanentFailuresAreTerminalAndVisible(t *testing.T) {
	cases := []struct {
		name, errContains string
		setup             func(g *whatsapptest.FakeGraph)
	}{
		{"provider media expired (404)", "status 404", func(g *whatsapptest.FakeGraph) {
			g.AddMedia("m", photo1, "image/jpeg")
			g.FailMetadata("m", 404)
		}},
		{"oversized", "exceeds limit", func(g *whatsapptest.FakeGraph) {
			g.AddMedia("m", append(append([]byte(nil), photo1...), make([]byte, 2048)...), "image/jpeg")
		}},
		{"unsupported mime", "unsupported or unrecognised content type", func(g *whatsapptest.FakeGraph) {
			g.AddMedia("m", []byte("GIF89a-animated"), "image/gif")
		}},
		{"video bytes declared as image", "does not match the declared media kind", func(g *whatsapptest.FakeGraph) {
			g.AddMedia("m", clip, "image/jpeg")
		}},
		{"malformed provider response", "malformed metadata", func(g *whatsapptest.FakeGraph) {
			g.MalformedMetadata("m", "<html>gateway</html>")
		}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newMediaEnv(t)
			wa := fmt.Sprintf("23480311100%02d", 60+i)
			e.vendor("V", wa)
			c.setup(e.graph)
			e.post(mediaPayload(wa, "wamid.F", "image", "m", "image/jpeg", ""))
			e.drain(e.worker("w", time.Minute))

			j := e.job("wamid.F")
			if j.Status != jobs.StatusFailed || j.Attempts != 1 || !strings.Contains(j.LastError, c.errContains) || j.CompletedAt == nil {
				t.Fatalf("expected immediate terminal failure containing %q, got %+v", c.errContains, j)
			}
			if e.count(`SELECT count(*) FROM media_assets`) != 0 || len(e.files()) != 0 || e.linkedAsset("wamid.F") != nil {
				t.Fatal("a failed ingestion must leave no asset, object or link")
			}
			if e.count(`SELECT count(*) FROM events WHERE event_type = 'MEDIA_ASSET_STORED'`) != 0 {
				t.Fatal("no success event on failure")
			}
			if e.count(`SELECT count(*) FROM events WHERE event_type = 'MEDIA_INGESTION_FAILED' AND entity_type = 'message' AND entity_id = $1 AND payload->>'provider_media_id' = 'm'`, e.messageID("wamid.F")) != 1 {
				t.Fatal("expected one MEDIA_INGESTION_FAILED event on the message")
			}
			if e.count(`SELECT count(*) FROM jobs WHERE status = 'FAILED'`) != 1 {
				t.Fatal("failed jobs are kept, never deleted")
			}
		})
	}
}

// --- 37.8 / §57 storage unavailable, DB unavailable after storage -------------------

func TestIntegration_MediaStorageAndDatabaseFailuresRetrySafely(t *testing.T) {
	e := newMediaEnv(t)
	e.vendor("V", "2348031110071")
	e.graph.AddMedia("s", photo1, "image/jpeg")
	e.graph.AddMedia("d", photo2, "image/jpeg")

	e.store.failPuts.Store(1)
	e.post(mediaPayload("2348031110071", "wamid.S", "image", "s", "image/jpeg", ""))
	e.drain(e.worker("w", time.Minute))
	if j := e.job("wamid.S"); j.Status != jobs.StatusSucceeded || j.Attempts != 2 {
		t.Fatalf("storage failure: expected success on retry, got %+v", j)
	}

	e.assets.failStores.Store(1) // object written, then the DB write fails
	putsBefore := e.store.puts.Load()
	e.post(mediaPayload("2348031110071", "wamid.D", "image", "d", "image/jpeg", ""))
	e.drain(e.worker("w2", time.Minute))
	if j := e.job("wamid.D"); j.Status != jobs.StatusSucceeded || j.Attempts != 2 {
		t.Fatalf("DB failure after storage: expected success on retry, got %+v", j)
	}
	if e.store.puts.Load()-putsBefore != 2 {
		t.Fatalf("expected the retry to re-put to the same key (2 puts), got %d", e.store.puts.Load()-putsBefore)
	}
	if e.count(`SELECT count(*) FROM media_assets`) != 2 || len(e.files()) != 2 ||
		e.count(`SELECT count(*) FROM events WHERE event_type = 'MEDIA_ASSET_STORED'`) != 2 {
		t.Fatalf("expected 2 assets / 2 objects / 2 events, got %d / %v", e.count(`SELECT count(*) FROM media_assets`), e.files())
	}
}

// --- 37.9 / §35 restart, §57 worker killed halfway -----------------------------

func TestIntegration_MediaJobSurvivesRestartAndKilledWorker(t *testing.T) {
	e := newMediaEnv(t)
	e.vendor("V", "2348031110081")
	e.graph.AddMedia("m1", photo1, "image/jpeg")
	e.graph.AddMedia("m2", photo2, "image/jpeg")

	// Process 1 receives the webhook and dies before any worker runs.
	e.post(mediaPayload("2348031110081", "wamid.K1", "image", "m1", "image/jpeg", ""))
	// Process 1 also had claimed a second job and was killed mid-download.
	e.post(mediaPayload("2348031110081", "wamid.K2", "image", "m2", "image/jpeg", ""))
	ctx := context.Background()
	dead, err := e.jobs.Claim(ctx, []string{media.JobTypeDownload}, "process-1", time.Minute)
	if err != nil || dead == nil {
		t.Fatalf("claim: %v", err)
	}

	// Process 2 starts from scratch (new ingestor, new worker, same DB).
	const lease = 300 * time.Millisecond
	p2 := e.worker("process-2", lease)
	e.drainSome(p2, 1) // the never-started job is recovered at once
	if e.count(`SELECT count(*) FROM jobs WHERE status = 'SUCCEEDED'`) != 1 {
		t.Fatal("persisted job was not recovered after restart")
	}
	if j, _ := e.jobs.FindByKey(ctx, media.JobTypeDownload, dead.IdempotencyKey); j.Status != jobs.StatusRunning {
		t.Fatalf("killed worker's job must stay RUNNING until its lease expires, got %s", j.Status)
	}
	time.Sleep(lease + 100*time.Millisecond)
	e.drain(p2)
	j, _ := e.jobs.FindByKey(ctx, media.JobTypeDownload, dead.IdempotencyKey)
	if j.Status != jobs.StatusSucceeded || j.Attempts != 2 {
		t.Fatalf("abandoned job should be reclaimed and completed on attempt 2, got %+v", j)
	}
	// The dead worker coming back late cannot overwrite the new owner's result.
	if err := e.jobs.Complete(ctx, dead.ID, "process-1"); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("expected ErrLeaseLost for the stale worker, got %v", err)
	}
	if e.count(`SELECT count(*) FROM media_assets`) != 2 {
		t.Fatal("expected both assets stored")
	}
}

// drainSome runs RunOnce until n jobs ran.
func (e *mediaEnv) drainSome(w *jobs.Worker, n int) {
	e.t.Helper()
	for ran := 0; ran < n; {
		ok, err := w.RunOnce(context.Background(), "x")
		if err != nil {
			e.t.Fatal(err)
		}
		if ok {
			ran++
		}
	}
}

// --- 37.10 / §13 concurrency --------------------------------------------------

func TestIntegration_JobClaimIsExclusiveUnderConcurrency(t *testing.T) {
	db := testDB(t, migrationsPath())
	repo := NewJobRepository(db)
	ctx := context.Background()
	for i := 0; i < 40; i++ {
		if created, err := repo.Enqueue(ctx, &jobs.Job{Type: "TEST", IdempotencyKey: fmt.Sprint(i), MaxAttempts: 1}); err != nil || !created {
			t.Fatal(err)
		}
	}
	if created, _ := repo.Enqueue(ctx, &jobs.Job{Type: "TEST", IdempotencyKey: "0", MaxAttempts: 1}); created {
		t.Fatal("duplicate idempotency key must not create a job")
	}

	var mu sync.Mutex
	seen := map[uuid.UUID]int{}
	var wg sync.WaitGroup
	for wk := 0; wk < 10; wk++ {
		wg.Add(1)
		go func(wk int) {
			defer wg.Done()
			for {
				j, err := repo.Claim(ctx, []string{"TEST"}, fmt.Sprint("w", wk), time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if j == nil {
					return
				}
				mu.Lock()
				seen[j.ID]++
				mu.Unlock()
			}
		}(wk)
	}
	wg.Wait()
	if len(seen) != 40 {
		t.Fatalf("expected all 40 jobs claimed, got %d", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("job %s claimed %d times", id, n)
		}
	}

	// SKIP LOCKED: a row locked by another transaction is skipped, not waited on.
	mustExec(t, db, `UPDATE jobs SET status = 'PENDING', locked_by = NULL WHERE job_type = 'TEST' AND idempotency_key IN ('0', '1')`)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var lockedID string
	if err := tx.QueryRow(`SELECT id FROM jobs WHERE idempotency_key = '0' FOR UPDATE`).Scan(&lockedID); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	j, err := repo.Claim(cctx, []string{"TEST"}, "other", time.Minute)
	if err != nil || j == nil || j.ID.String() == lockedID || j.IdempotencyKey != "1" {
		t.Fatalf("expected the unlocked job to be claimed without blocking, got %+v %v", j, err)
	}
}

func TestIntegration_MediaConcurrentWorkersCreateNoDuplicates(t *testing.T) {
	e := newMediaEnv(t)
	e.vendor("V", "2348031110091")
	// 6 messages, 3 distinct images, each image sent twice: racing workers
	// must converge on 3 assets with every message linked.
	imgs := [][]byte{photo1, photo2, photo3}
	for i := 0; i < 6; i++ {
		id := fmt.Sprint("c", i)
		e.graph.AddMedia(id, imgs[i%3], "image/jpeg")
		e.post(mediaPayload("2348031110091", "wamid.C"+fmt.Sprint(i), "image", id, "image/jpeg", ""))
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		w := e.worker(fmt.Sprint("proc", i), time.Minute)
		wg.Add(1)
		go func() { defer wg.Done(); w.Run(ctx) }()
	}
	deadline := time.Now().Add(10 * time.Second)
	for e.count(`SELECT count(*) FROM jobs WHERE status <> 'SUCCEEDED'`) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	if n := e.count(`SELECT count(*) FROM jobs WHERE status = 'SUCCEEDED' AND attempts = 1`); n != 6 {
		t.Fatalf("expected 6 jobs each succeeded once, got %d", n)
	}
	if e.count(`SELECT count(*) FROM media_assets`) != 3 || e.count(`SELECT count(*) FROM events WHERE event_type = 'MEDIA_ASSET_STORED'`) != 3 || len(e.files()) != 3 {
		t.Fatalf("expected 3 assets/events/objects, got %d/%d/%d", e.count(`SELECT count(*) FROM media_assets`),
			e.count(`SELECT count(*) FROM events WHERE event_type = 'MEDIA_ASSET_STORED'`), len(e.files()))
	}
	if e.count(`SELECT count(*) FROM messages WHERE media_asset_id IS NOT NULL`) != 6 {
		t.Fatal("every message must be linked to its asset")
	}

	// Two workers racing for one due job: exactly one claims it.
	e.graph.AddMedia("solo", clip, "video/mp4")
	e.post(mediaPayload("2348031110091", "wamid.SOLO", "video", "solo", "video/mp4", ""))
	var claimed atomic.Int32
	var wg2 sync.WaitGroup
	for i := 0; i < 2; i++ {
		w := e.worker(fmt.Sprint("race", i), time.Minute)
		wg2.Add(1)
		go func(i int) {
			defer wg2.Done()
			if ran, _ := w.RunOnce(context.Background(), fmt.Sprint("race", i)); ran {
				claimed.Add(1)
			}
		}(i)
	}
	wg2.Wait()
	if claimed.Load() != 1 {
		t.Fatalf("expected exactly one worker to claim the job, got %d", claimed.Load())
	}
	if a := e.linkedAsset("wamid.SOLO"); a == nil || a.Kind != media.KindVideo || a.MimeType != "video/mp4" {
		t.Fatalf("video asset: %+v", a)
	}
}

// --- startup backfill ----------------------------------------------------------

func TestIntegration_MediaBackfillEnqueuesOnlyUnlinkedVendorImagesAndVideos(t *testing.T) {
	e := newMediaEnv(t)
	e.vendor("V", "2348031110101")
	e.graph.AddMedia("bf-1", photo1, "image/jpeg")
	ctx := context.Background()
	conv := NewConversationRepository(e.db)
	vc, _ := conv.FindOrCreateByExternalID(ctx, "whatsapp", "2348031110101")
	bc, _ := conv.FindOrCreateByExternalID(ctx, "whatsapp", "2348031119999") // a buyer
	insert := func(c uuid.UUID, wamid, kind, ref string, age time.Duration) {
		mustExec(t, e.db, `INSERT INTO messages (conversation_id, direction, external_message_id, message_type, content, created_at)
			VALUES ($1, 'INBOUND', $2, $3, jsonb_build_object('text', '', 'media', jsonb_build_object('kind', $3::text, 'provider', 'whatsapp', 'provider_ref', $4::text)), now() - $5::interval)`,
			c, wamid, kind, ref, fmt.Sprintf("%d seconds", int(age.Seconds())))
	}
	insert(vc.ID, "wamid.BF1", "image", "bf-1", time.Hour)      // vendor image, crash before enqueue -> backfilled
	insert(vc.ID, "wamid.BF2", "audio", "bf-2", time.Hour)      // vendor voice note -> not ingested in 4A.1
	insert(vc.ID, "wamid.BF3", "image", "bf-3", 8*24*time.Hour) // outside the window
	insert(bc.ID, "wamid.BF4", "image", "bf-4", time.Hour)      // buyer media -> not vendor-owned

	n, err := e.ingestor().Backfill(ctx, time.Now().Add(-7*24*time.Hour), 100)
	if err != nil || n != 1 {
		t.Fatalf("expected 1 backfilled job, got %d %v", n, err)
	}
	if n, _ := e.ingestor().Backfill(ctx, time.Now().Add(-7*24*time.Hour), 100); n != 0 {
		t.Fatalf("backfill must be idempotent, second run enqueued %d", n)
	}
	e.drain(e.worker("w", time.Minute))
	if a := e.linkedAsset("wamid.BF1"); a == nil || a.ProviderMediaID != "bf-1" {
		t.Fatal("backfilled media was not stored")
	}
	if e.count(`SELECT count(*) FROM jobs`) != 1 {
		t.Fatal("only the vendor image in the window may be enqueued")
	}
}

// --- schema guards ---------------------------------------------------------------

func TestIntegration_Migration0008Constraints(t *testing.T) {
	e := newMediaEnv(t)
	v := e.vendor("V", "2348031110111")
	e.graph.AddMedia("g", photo1, "image/jpeg")
	e.post(mediaPayload("2348031110111", "wamid.G", "image", "g", "image/jpeg", ""))
	msg := e.messageID("wamid.G")
	ins := `INSERT INTO media_assets (owner_vendor_id, kind, mime_type, size_bytes, sha256, storage_key, source_message_id, provider, provider_media_id)
		VALUES ($1, $2, 'image/jpeg', $3, $4, $5, $6, 'whatsapp', 'x')`
	good := strings.Repeat("a", 64)
	for name, args := range map[string][]any{
		"audio kind rejected":    {v, "audio", 1, good, "k1", msg},
		"zero size rejected":     {v, "image", 0, good, "k2", msg},
		"non-hex sha rejected":   {v, "image", 1, strings.Repeat("Z", 64), "k3", msg},
		"unknown vendor":         {uuid.New(), "image", 1, good, "k4", msg},
		"unknown source message": {v, "image", 1, good, "k5", uuid.New()},
	} {
		if _, err := e.db.Exec(ins, args...); err == nil {
			t.Fatalf("%s: insert unexpectedly succeeded", name)
		}
	}
	if _, err := e.db.Exec(ins, v, "image", 1, good, "k6", msg); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(ins, v, "image", 1, strings.Repeat("b", 64), "k6", msg); err == nil || !strings.Contains(err.Error(), "storage_key") {
		t.Fatalf("storage_key must be unique, got %v", err)
	}
	if _, err := e.db.Exec(`INSERT INTO jobs (job_type, idempotency_key, max_attempts, status) VALUES ('T', 'k', 1, 'DONE')`); err == nil {
		t.Fatal("unknown job status must be rejected")
	}
}

// §42 (media step prompt): the stored asset stays retrievable after the
// provider's media reference has expired, and re-running the job does not
// go back to the provider.
func TestIntegration_MediaRetrievableAfterProviderMediaExpired(t *testing.T) {
	e := newMediaEnv(t)
	e.vendor("V", "2348031110121")
	e.graph.AddMedia("exp", photo1, "image/jpeg")
	e.post(mediaPayload("2348031110121", "wamid.EXP", "image", "exp", "image/jpeg", ""))
	e.drain(e.worker("w", time.Minute))
	a := e.linkedAsset("wamid.EXP")
	if a == nil {
		t.Fatal("asset not stored")
	}
	// Meta's media ID and URL are now gone.
	e.graph.FailMetadata("exp", 404, 404, 404)
	e.graph.FailDownload("exp", 404, 404, 404)
	got, err := e.store.Get(context.Background(), a.StorageKey)
	if err != nil || media.SHA256Hex(got) != a.SHA256 {
		t.Fatalf("stored asset must not depend on the provider: %v", err)
	}
	metaBefore, _ := e.graph.Calls("exp")
	j := e.job("wamid.EXP")
	if err := e.ingestor().Handle(context.Background(), j); err != nil {
		t.Fatalf("re-running a completed media job must be a no-op, got %v", err)
	}
	if metaAfter, _ := e.graph.Calls("exp"); metaAfter != metaBefore {
		t.Fatal("re-run must not contact the provider again")
	}
}
