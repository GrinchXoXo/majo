package media_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/infrastructure/whatsapp"
	"github.com/middleman/backend/infrastructure/whatsapp/whatsapptest"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/jobs"
	"github.com/middleman/backend/internal/media"
)

var (
	jpeg  = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("bag-photo-1")...)
	jpeg2 = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("bag-photo-2")...)
	gif   = []byte("GIF89a....")
)

// ---- fakes -----------------------------------------------------------------

// fakeStorage records Put calls and can be scripted to fail.
type fakeStorage struct {
	mu       sync.Mutex
	objects  map[string][]byte
	puts     []string
	failures []error
}

func newFakeStorage() *fakeStorage { return &fakeStorage{objects: map[string][]byte{}} }

func (s *fakeStorage) Put(ctx context.Context, key string, data []byte, ct string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts = append(s.puts, key)
	if len(s.failures) > 0 {
		err := s.failures[0]
		s.failures = s.failures[1:]
		return err
	}
	s.objects[key] = append([]byte(nil), data...)
	return nil
}
func (s *fakeStorage) Get(ctx context.Context, key string) ([]byte, error) {
	return s.objects[key], nil
}
func (s *fakeStorage) Delete(ctx context.Context, key string) error {
	delete(s.objects, key)
	return nil
}
func (s *fakeStorage) GenerateURL(ctx context.Context, key string) (string, error) {
	return "", errors.New("no")
}

type permanentErr struct{}

func (permanentErr) Error() string   { return "bucket does not exist" }
func (permanentErr) Permanent() bool { return true }

// memAssets mirrors the Postgres repository's semantics in memory.
type memAssets struct {
	mu            sync.Mutex
	assets        []*media.Asset
	links         map[uuid.UUID]uuid.UUID // message -> asset
	events        []*event.Event
	failStoreOnce bool
}

func newMemAssets() *memAssets { return &memAssets{links: map[uuid.UUID]uuid.UUID{}} }

func (m *memAssets) FindByVendorAndSHA256(ctx context.Context, v uuid.UUID, sha string) (*media.Asset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.assets {
		if a.OwnerVendorID == v && a.SHA256 == sha {
			return a, nil
		}
	}
	return nil, nil
}
func (m *memAssets) MessageAssetID(ctx context.Context, msg uuid.UUID) (*uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.links[msg]; ok {
		return &id, nil
	}
	return nil, nil
}
func (m *memAssets) LinkMessage(ctx context.Context, msg, asset uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.links[msg] = asset
	return nil
}
func (m *memAssets) StoreAndLink(ctx context.Context, a *media.Asset, msg uuid.UUID, ev *event.Event) (*media.Asset, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failStoreOnce {
		m.failStoreOnce = false
		return nil, false, errors.New("connection refused (db down)")
	}
	for _, x := range m.assets {
		if x.OwnerVendorID == a.OwnerVendorID && x.SHA256 == a.SHA256 {
			m.links[msg] = x.ID
			return x, false, nil
		}
	}
	m.assets = append(m.assets, a)
	m.links[msg] = a.ID
	id := a.ID
	ev.EntityID = &id
	m.events = append(m.events, ev)
	return a, true, nil
}
func (m *memAssets) UnlinkedVendorMedia(ctx context.Context, since time.Time, limit int) ([]media.UnlinkedMedia, error) {
	return nil, nil
}

type memJobs struct {
	mu   sync.Mutex
	jobs map[string]*jobs.Job
}

func (j *memJobs) Enqueue(ctx context.Context, job *jobs.Job) (bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.jobs == nil {
		j.jobs = map[string]*jobs.Job{}
	}
	k := job.Type + "|" + job.IdempotencyKey
	if _, ok := j.jobs[k]; ok {
		return false, nil
	}
	c := *job
	c.ID = uuid.New()
	j.jobs[k] = &c
	return true, nil
}
func (j *memJobs) Claim(context.Context, []string, string, time.Duration) (*jobs.Job, error) {
	return nil, nil
}
func (j *memJobs) Complete(context.Context, uuid.UUID, string) error                 { return nil }
func (j *memJobs) Retry(context.Context, uuid.UUID, string, time.Time, string) error { return nil }
func (j *memJobs) Fail(context.Context, uuid.UUID, string, string) error             { return nil }

type recEvents struct{ all []*event.Event }

func (r *recEvents) Write(ctx context.Context, e *event.Event) error {
	r.all = append(r.all, e)
	return nil
}

// ---- harness ---------------------------------------------------------------

type harness struct {
	graph  *whatsapptest.FakeGraph
	store  *fakeStorage
	assets *memAssets
	jobs   *memJobs
	events *recEvents
	ing    *media.Ingestor
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	g := whatsapptest.New("tok")
	t.Cleanup(g.Close)
	h := &harness{graph: g, store: newFakeStorage(), assets: newMemAssets(), jobs: &memJobs{}, events: &recEvents{}}
	wa := whatsapp.NewCloudAPIProvider(g.URL(), "tok", "PHONE", "v", "s")
	h.ing = media.NewIngestor(h.jobs, h.assets, h.store, map[string]media.Downloader{"whatsapp": wa}, h.events,
		media.Limits{MaxImageBytes: 1024, MaxVideoBytes: 2048}, 5, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return h
}

// enqueue simulates the webhook hand-off and returns the job the worker would run.
func (h *harness) enqueue(t *testing.T, vendor, msg uuid.UUID, mediaID, kind string) *jobs.Job {
	t.Helper()
	if err := h.ing.EnqueueDownload(context.Background(), vendor, msg,
		&conversation.MediaRef{Kind: kind, Provider: "whatsapp", ProviderRef: mediaID, MimeType: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}
	j := h.jobs.jobs[media.JobTypeDownload+"|"+msg.String()]
	if j != nil {
		j.Attempts++
	}
	return j
}

// ---- tests -----------------------------------------------------------------

func TestIngest_HappyPathStoresAssetWithProvenanceAndEvent(t *testing.T) {
	h := newHarness(t)
	vendor, msg := uuid.New(), uuid.New()
	h.graph.AddMedia("media-123", jpeg, "image/jpeg")
	j := h.enqueue(t, vendor, msg, "media-123", "image")

	if err := h.ing.Handle(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if len(h.assets.assets) != 1 {
		t.Fatalf("expected 1 asset, got %d", len(h.assets.assets))
	}
	a := h.assets.assets[0]
	sha := media.SHA256Hex(jpeg)
	if a.OwnerVendorID != vendor || a.SourceMessageID != msg || a.ProviderMediaID != "media-123" || a.Provider != "whatsapp" ||
		a.SHA256 != sha || a.Kind != media.KindImage || a.MimeType != "image/jpeg" || a.SizeBytes != int64(len(jpeg)) ||
		a.StorageKey != "vendors/"+vendor.String()+"/media/"+sha+".jpg" || a.ProviderMediaExpiresAt != nil {
		t.Fatalf("asset fields wrong: %+v", a)
	}
	if got := h.store.objects[a.StorageKey]; string(got) != string(jpeg) {
		t.Fatal("stored bytes differ from downloaded bytes")
	}
	if h.assets.links[msg] != a.ID {
		t.Fatal("message not linked to asset")
	}
	if len(h.assets.events) != 1 || h.assets.events[0].EventType != event.MediaAssetStored || *h.assets.events[0].EntityID != a.ID {
		t.Fatalf("expected one MEDIA_ASSET_STORED for the asset, got %+v", h.assets.events)
	}
	p := h.assets.events[0].Payload
	for _, k := range []string{"media_asset_id", "source_message_id", "vendor_id", "kind", "mime_type", "size_bytes", "sha256", "provider_media_id"} {
		if _, ok := p[k]; !ok {
			t.Fatalf("event payload missing %q: %v", k, p)
		}
	}
}

func TestIngest_RerunAfterSuccessIsANoOpWithoutRedownload(t *testing.T) {
	h := newHarness(t)
	vendor, msg := uuid.New(), uuid.New()
	h.graph.AddMedia("m1", jpeg, "image/jpeg")
	j := h.enqueue(t, vendor, msg, "m1", "image")
	_ = h.ing.Handle(context.Background(), j)
	// Crash before the job was acknowledged: the job runs again.
	if err := h.ing.Handle(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if meta, dl := h.graph.Calls("m1"); meta != 1 || dl != 1 {
		t.Fatalf("expected no second download, got metadata=%d download=%d", meta, dl)
	}
	if len(h.assets.assets) != 1 || len(h.assets.events) != 1 || len(h.store.puts) != 1 {
		t.Fatalf("duplicate effects: assets=%d events=%d puts=%d", len(h.assets.assets), len(h.assets.events), len(h.store.puts))
	}
}

func TestIngest_EnqueueIsIdempotentPerMessageAndSkipsUnsupportedKinds(t *testing.T) {
	h := newHarness(t)
	vendor, msg := uuid.New(), uuid.New()
	h.enqueue(t, vendor, msg, "m1", "image")
	h.enqueue(t, vendor, msg, "m1", "image") // redelivered webhook
	if len(h.jobs.jobs) != 1 {
		t.Fatalf("expected one job per message, got %d", len(h.jobs.jobs))
	}
	for _, kind := range []string{"audio", "document", "sticker"} {
		h.enqueue(t, vendor, uuid.New(), "x", kind)
	}
	if len(h.jobs.jobs) != 1 {
		t.Fatalf("audio/document/sticker must not be ingested in 4A.1, got %d jobs", len(h.jobs.jobs))
	}
}

func TestIngest_SameVendorSameBytesReusesAsset(t *testing.T) {
	h := newHarness(t)
	vendor, m1, m2 := uuid.New(), uuid.New(), uuid.New()
	h.graph.AddMedia("a", jpeg, "image/jpeg")
	h.graph.AddMedia("b", jpeg, "image/jpeg") // same bytes, new provider media ID
	_ = h.ing.Handle(context.Background(), h.enqueue(t, vendor, m1, "a", "image"))
	if err := h.ing.Handle(context.Background(), h.enqueue(t, vendor, m2, "b", "image")); err != nil {
		t.Fatal(err)
	}
	if len(h.assets.assets) != 1 || len(h.assets.events) != 1 || len(h.store.puts) != 1 {
		t.Fatalf("expected one logical asset: assets=%d events=%d puts=%d", len(h.assets.assets), len(h.assets.events), len(h.store.puts))
	}
	a := h.assets.assets[0]
	if a.SourceMessageID != m1 || h.assets.links[m1] != a.ID || h.assets.links[m2] != a.ID {
		t.Fatal("provenance: first message is the source; both messages link to the asset")
	}
}

func TestIngest_SameBytesDifferentVendorsStaySeparate(t *testing.T) {
	h := newHarness(t)
	vA, vB, mA, mB := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	h.graph.AddMedia("a", jpeg, "image/jpeg")
	h.graph.AddMedia("b", jpeg, "image/jpeg")
	_ = h.ing.Handle(context.Background(), h.enqueue(t, vA, mA, "a", "image"))
	_ = h.ing.Handle(context.Background(), h.enqueue(t, vB, mB, "b", "image"))
	if len(h.assets.assets) != 2 {
		t.Fatalf("expected two assets (one per vendor), got %d", len(h.assets.assets))
	}
	a, b := h.assets.assets[0], h.assets.assets[1]
	if a.OwnerVendorID != vA || b.OwnerVendorID != vB || a.ID == b.ID || a.StorageKey == b.StorageKey || a.SHA256 != b.SHA256 {
		t.Fatalf("ownership violated: %+v / %+v", a, b)
	}
	if h.assets.links[mB] != b.ID {
		t.Fatal("vendor B's message must link to vendor B's asset, never vendor A's")
	}
}

func TestIngest_FailureClassification(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(h *harness)
		permanent bool
	}{
		{"graph 503", func(h *harness) { h.graph.FailMetadata("m", 503) }, false},
		{"graph 429", func(h *harness) { h.graph.FailMetadata("m", 429) }, false},
		{"graph 404 media expired", func(h *harness) { h.graph.FailMetadata("m", 404) }, true},
		{"oversized", func(h *harness) { h.graph.AddMedia("m", append(jpeg, make([]byte, 2000)...), "image/jpeg") }, true},
		{"unsupported mime", func(h *harness) { h.graph.AddMedia("m", gif, "image/gif") }, true},
		{"corrupt metadata", func(h *harness) { h.graph.MalformedMetadata("m", "<html>") }, true},
		{"storage transient", func(h *harness) { h.store.failures = []error{errors.New("disk busy")} }, false},
		{"storage permanent", func(h *harness) { h.store.failures = []error{permanentErr{}} }, true},
		{"db down after storage", func(h *harness) { h.assets.failStoreOnce = true }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.graph.AddMedia("m", jpeg, "image/jpeg")
			c.setup(h)
			err := h.ing.Handle(context.Background(), h.enqueue(t, uuid.New(), uuid.New(), "m", "image"))
			if err == nil {
				t.Fatal("expected an error")
			}
			if jobs.IsPermanent(err) != c.permanent {
				t.Fatalf("permanent=%v want %v: %v", jobs.IsPermanent(err), c.permanent, err)
			}
			if len(h.assets.assets) != 0 || len(h.assets.events) != 0 {
				t.Fatal("no asset or success event may exist after a failure")
			}
		})
	}
}

func TestIngest_StorageSucceededDBFailedRetryConvergesOnOneObject(t *testing.T) {
	h := newHarness(t)
	vendor, msg := uuid.New(), uuid.New()
	h.graph.AddMedia("m", jpeg, "image/jpeg")
	h.assets.failStoreOnce = true
	j := h.enqueue(t, vendor, msg, "m", "image")
	if err := h.ing.Handle(context.Background(), j); err == nil || jobs.IsPermanent(err) {
		t.Fatalf("expected retryable error, got %v", err)
	}
	if err := h.ing.Handle(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if len(h.store.puts) != 2 || h.store.puts[0] != h.store.puts[1] || len(h.store.objects) != 1 {
		t.Fatalf("retry must target the same storage key: puts=%v objects=%d", h.store.puts, len(h.store.objects))
	}
	if len(h.assets.assets) != 1 || len(h.assets.events) != 1 {
		t.Fatalf("expected one asset and one event, got %d/%d", len(h.assets.assets), len(h.assets.events))
	}
}

func TestIngest_SecondDifferentImageCreatesSecondAsset(t *testing.T) {
	h := newHarness(t)
	vendor := uuid.New()
	h.graph.AddMedia("a", jpeg, "image/jpeg")
	h.graph.AddMedia("b", jpeg2, "image/jpeg")
	_ = h.ing.Handle(context.Background(), h.enqueue(t, vendor, uuid.New(), "a", "image"))
	_ = h.ing.Handle(context.Background(), h.enqueue(t, vendor, uuid.New(), "b", "image"))
	if len(h.assets.assets) != 2 {
		t.Fatalf("different bytes must be different assets, got %d", len(h.assets.assets))
	}
}

func TestIngest_MalformedPayloadIsPermanent(t *testing.T) {
	h := newHarness(t)
	err := h.ing.Handle(context.Background(), &jobs.Job{ID: uuid.New(), Payload: map[string]any{"message_id": "nope"}})
	if !jobs.IsPermanent(err) {
		t.Fatalf("expected permanent error, got %v", err)
	}
}

func TestIngest_OnFailureWritesEvent(t *testing.T) {
	h := newHarness(t)
	msg := uuid.New()
	j := h.enqueue(t, uuid.New(), msg, "m", "image")
	j.LastError = "whatsapp media metadata: status 404"
	h.ing.OnFailure(context.Background(), j, jobs.Permanent(errors.New("404")))
	if len(h.events.all) != 1 || h.events.all[0].EventType != event.MediaIngestionFailed || *h.events.all[0].EntityID != msg {
		t.Fatalf("expected MEDIA_INGESTION_FAILED on the message, got %+v", h.events.all)
	}
	if h.events.all[0].Payload["error"] != j.LastError || h.events.all[0].Payload["permanent"] != true {
		t.Fatalf("payload: %v", h.events.all[0].Payload)
	}
}
