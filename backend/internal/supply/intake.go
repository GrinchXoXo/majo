package supply

// Phase 4A.2 intake: turns classified vendor supply messages into
// SupplySignals, grouping media + captions + text deterministically.
//
// Lifecycle:
//
//	routing classifies a vendor message as supply content
//	  -> SubmitText / SubmitMedia record a candidate (supply_candidate_messages)
//	text with no open media before it -> signal created immediately (the
//	  existing fast text path, now with its source message recorded)
//	otherwise -> durable SUPPLY_GROUPING jobs evaluate the submission:
//	  * when the vendor's text closes it (immediately),
//	  * when the grouping window has elapsed,
//	  * when a member photo/video finishes downloading (MediaSettled).
//	A submission becomes a signal only once it is final (closed or window
//	elapsed) and every member's media download is final, so a signal never
//	references media that is not stored.
//
// Idempotency: supply_signal_sources.message_id is unique; creating a signal
// and its sources and its SUPPLY_SIGNAL_CREATED event is one transaction, so
// any number of jobs/workers evaluating the same submission yield one signal
// and one event.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/infrastructure/ai"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/jobs"
)

// JobTypeGroup is the durable job type that evaluates a submission.
const JobTypeGroup = "SUPPLY_GROUPING"

// settleGrace is added to the window before a submission is considered
// final, to absorb webhook delivery delay. Not a product parameter.
const settleGrace = 15 * time.Second

// lookbackWindows: how many windows of history a submission is evaluated
// against. Segmentation is sequential, so its start point must be stable:
// any gap longer than the window resets a submission, and 10 windows of
// continuous sending covers any realistic burst. Longer unbroken chains are
// a documented limitation (the unique source constraint still prevents
// duplicate signals).
const lookbackWindows = 10

// ErrSourcesTaken: a message in the submission is already evidence of a
// signal (another worker or path got there first). Callers treat it as done.
var ErrSourcesTaken = errors.New("supply: source message already belongs to a signal")

// Inbound identifies the message being submitted.
type Inbound struct {
	MessageID      uuid.UUID
	ConversationID uuid.UUID
	At             time.Time // vendor send time (Meta timestamp)
}

// Source is one piece of a signal's evidence, in order.
type Source struct {
	MessageID    uuid.UUID
	Position     int
	MediaAssetID *uuid.UUID
}

// Evidence is a signal's provenance.
type Evidence struct {
	SourceMessageIDs []uuid.UUID
	MediaAssetIDs    []uuid.UUID // stored assets, in source order, de-duplicated
}

// GroupingRepository is the persistence boundary for grouping.
type GroupingRepository interface {
	// RecordCandidate stores the classification once per message (no-op if present).
	RecordCandidate(ctx context.Context, c *Candidate) error
	Candidate(ctx context.Context, messageID uuid.UUID) (*Candidate, error)
	// Timeline returns the vendor conversation's candidates and breaks
	// (non-candidate inbound messages) sent within [from, to], ordered.
	Timeline(ctx context.Context, vendorID, conversationID uuid.UUID, from, to time.Time) ([]Candidate, []Break, error)
	// CreateSignalWithSources inserts the signal, its sources and created
	// (EntityID set to the signal) in one transaction. ErrSourcesTaken if any
	// source message already belongs to a signal (nothing is written).
	CreateSignalWithSources(ctx context.Context, s *Signal, sources []Source, created *event.Event) (*Signal, error)
	// AppendSources adds evidence to an existing signal plus updated, in one
	// transaction. ErrSourcesTaken as above.
	AppendSources(ctx context.Context, signalID uuid.UUID, sources []Source, updated *event.Event) error
	Evidence(ctx context.Context, signalID uuid.UUID) (*Evidence, error)
}

// Extractor is the existing supply extraction (Python /extract-supply).
type Extractor interface {
	ExtractSupply(ctx context.Context, text string) (*ai.SupplyExtractionResult, error)
}

// ActivityRecorder updates vendor counters (vendor.Repository satisfies it).
type ActivityRecorder interface {
	RecordSupplySubmission(ctx context.Context, vendorID uuid.UUID) error
}

// Intake groups vendor supply content into SupplySignals.
type Intake struct {
	repo        GroupingRepository
	extractor   Extractor
	jobs        jobs.Store
	vendors     ActivityRecorder
	window      time.Duration
	maxAttempts int
	logger      *slog.Logger
	now         func() time.Time
}

func NewIntake(repo GroupingRepository, extractor Extractor, js jobs.Store, vendors ActivityRecorder,
	window time.Duration, maxAttempts int, logger *slog.Logger) *Intake {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	return &Intake{repo: repo, extractor: extractor, jobs: js, vendors: vendors, window: window,
		maxAttempts: maxAttempts, logger: logger, now: time.Now}
}

// Window is the configured grouping window.
func (in *Intake) Window() time.Duration { return in.window }

// IsSupplyMediaKind: only images and video are supply media in 4A.2.
func IsSupplyMediaKind(kind string) bool { return kind == "image" || kind == "video" }

func flags(r *ai.SupplyExtractionResult) (product, price bool) {
	if r == nil {
		return false, false
	}
	return r.Product != nil && *r.Product != "", r.Price != nil && *r.Price > 0
}

// SubmitText handles a vendor text that routing classified as supply (no
// active VendorRequest). If photos/videos sent just before it are still an
// open submission, the text joins them (it is their caption); otherwise the
// signal is created immediately, exactly as before 4A.2.
func (in *Intake) SubmitText(ctx context.Context, vendorID uuid.UUID, ref Inbound, text string) error {
	res, extErr := in.extractor.ExtractSupply(ctx, text)
	hp, hpr := false, false
	if extErr == nil {
		hp, hpr = flags(res)
	}
	at := in.at(ref.At)
	if err := in.repo.RecordCandidate(ctx, &Candidate{MessageID: ref.MessageID, VendorID: vendorID, ConversationID: ref.ConversationID,
		Kind: CandidateText, MessageAt: at, Text: text, HasProduct: hp, HasPrice: hpr, Reason: "no_active_vendor_request"}); err != nil {
		return fmt.Errorf("recording supply candidate: %w", err)
	}
	log := in.logger.With("vendor_id", vendorID, "message_id", ref.MessageID)

	cands, breaks, err := in.repo.Timeline(ctx, vendorID, ref.ConversationID, at.Add(-lookbackWindows*in.window), at)
	if err != nil {
		return fmt.Errorf("loading supply timeline: %w", err)
	}
	groups := Segment(cands, breaks, in.window)
	if gi := GroupOf(groups, ref.MessageID); gi >= 0 && groups[gi].SignalID == nil && len(groups[gi].Members) > 1 {
		// Joins photos/videos sent just before it: evaluate as one submission.
		log.Info("supply text joins an open media submission", "members", len(groups[gi].Members))
		return in.enqueue(ctx, ref.MessageID, "text", in.now())
	} else if gi >= 0 && groups[gi].SignalID != nil {
		return nil // already evidence (redelivery)
	}

	// Fast path: a standalone text submission, signal now.
	src := []Source{{MessageID: ref.MessageID, Position: 0}}
	_, err = in.create(ctx, vendorID, text, res, extErr, src, nil, "text_only", log)
	return err
}

// SubmitMedia records a vendor photo/video classified as supply content and
// schedules the window check. ext is the caption's extraction if routing
// already computed it (nil otherwise).
func (in *Intake) SubmitMedia(ctx context.Context, vendorID uuid.UUID, ref Inbound, caption, reason string, ext *ai.SupplyExtractionResult) error {
	if ext == nil && strings.TrimSpace(caption) != "" {
		if r, err := in.extractor.ExtractSupply(ctx, caption); err == nil {
			ext = r
		} else {
			in.logger.Warn("caption extraction failed; product/price flags unknown", "message_id", ref.MessageID, "error", err)
		}
	}
	hp, hpr := flags(ext)
	at := in.at(ref.At)
	if err := in.repo.RecordCandidate(ctx, &Candidate{MessageID: ref.MessageID, VendorID: vendorID, ConversationID: ref.ConversationID,
		Kind: CandidateMedia, MessageAt: at, Text: caption, HasProduct: hp, HasPrice: hpr, Reason: reason}); err != nil {
		return fmt.Errorf("recording supply candidate: %w", err)
	}
	return in.enqueue(ctx, ref.MessageID, "window", at.Add(in.window+settleGrace))
}

// MediaSettled is called by media ingestion when a message's download
// reached a final state (stored, deduplicated or failed).
func (in *Intake) MediaSettled(ctx context.Context, messageID uuid.UUID) error {
	c, err := in.repo.Candidate(ctx, messageID)
	if err != nil || c == nil {
		return err // not supply content: nothing to do
	}
	return in.enqueue(ctx, messageID, "settled", in.now())
}

func (in *Intake) enqueue(ctx context.Context, messageID uuid.UUID, trigger string, runAt time.Time) error {
	_, err := in.jobs.Enqueue(ctx, &jobs.Job{
		Type: JobTypeGroup, IdempotencyKey: messageID.String() + ":" + trigger, RunAt: runAt, MaxAttempts: in.maxAttempts,
		Payload: map[string]any{"message_id": messageID.String(), "trigger": trigger},
	})
	if err != nil {
		return fmt.Errorf("enqueueing supply grouping: %w", err)
	}
	return nil
}

func (in *Intake) at(t time.Time) time.Time {
	if t.IsZero() {
		return in.now().UTC()
	}
	return t.UTC()
}

// Handle is the SUPPLY_GROUPING job handler: evaluate the submission that
// contains the message and materialise it if it is final and settled.
func (in *Intake) Handle(ctx context.Context, j *jobs.Job) error {
	idStr, _ := j.Payload["message_id"].(string)
	messageID, err := uuid.Parse(idStr)
	if err != nil {
		return jobs.Permanent(fmt.Errorf("malformed grouping payload: %w", err))
	}
	c, err := in.repo.Candidate(ctx, messageID)
	if err != nil {
		return err
	}
	if c == nil {
		return jobs.Permanent(fmt.Errorf("message %s is not a supply candidate", messageID))
	}
	log := in.logger.With("job_id", j.ID, "trigger", j.Payload["trigger"], "vendor_id", c.VendorID, "message_id", messageID)

	cands, breaks, err := in.repo.Timeline(ctx, c.VendorID, c.ConversationID, c.MessageAt.Add(-lookbackWindows*in.window), c.MessageAt.Add(2*in.window))
	if err != nil {
		return fmt.Errorf("loading supply timeline: %w", err)
	}
	groups := Segment(cands, breaks, in.window)
	gi := GroupOf(groups, messageID)
	if gi < 0 {
		return fmt.Errorf("message %s missing from its own timeline", messageID)
	}
	g := groups[gi]
	ids := memberIDs(g)
	if g.SignalID != nil {
		log.Debug("submission already materialised", "supply_signal_id", *g.SignalID)
		return nil
	}
	final := g.Closed || gi < len(groups)-1 || !in.now().Before(g.First().MessageAt.Add(in.window+settleGrace))
	if !final {
		log.Info("submission still open; waiting for the grouping window", "source_message_ids", ids)
		return nil
	}
	if !g.Settled() {
		log.Info("submission waiting for media downloads to finish", "source_message_ids", ids)
		return nil
	}

	sources := make([]Source, len(g.Members))
	for i, m := range g.Members {
		sources[i] = Source{MessageID: m.MessageID, Position: i, MediaAssetID: m.MediaAssetID}
	}

	if !g.HasText() {
		// Media without any caption or text: no product/price evidence.
		// Late-arrival rule: photos sent right after a text signal (caption
		// first, photos after) are added to that signal.
		if gi > 0 && groups[gi-1].SignalID != nil && g.StartReason == StartAfterSignal &&
			g.First().MessageAt.Sub(groups[gi-1].First().MessageAt) <= in.window {
			prev := groups[gi-1]
			return in.appendLate(ctx, *prev.SignalID, c.VendorID, len(prev.Members), g, log)
		}
		log.Info("media-only submission has no text evidence; media kept, no supply signal created",
			"source_message_ids", ids, "media_asset_ids", assetIDs(g))
		return nil
	}

	texts := make([]string, 0, len(g.Members))
	for _, m := range g.Members {
		if t := strings.TrimSpace(m.Text); t != "" {
			texts = append(texts, t)
		}
	}
	combined := strings.Join(texts, "\n")
	res, extErr := in.extractor.ExtractSupply(ctx, combined)
	_, err = in.create(ctx, c.VendorID, combined, res, extErr, sources, &g, "grouped:"+g.StartReason, log)
	return err
}

func memberIDs(g Group) []string {
	out := make([]string, len(g.Members))
	for i, m := range g.Members {
		out[i] = m.MessageID.String()
	}
	return out
}

func assetIDs(g Group) []string {
	var out []string
	seen := map[uuid.UUID]bool{}
	for _, m := range g.Members {
		if m.MediaAssetID != nil && !seen[*m.MediaAssetID] {
			seen[*m.MediaAssetID] = true
			out = append(out, m.MediaAssetID.String())
		}
	}
	return out
}

func unavailableMedia(g *Group) []string {
	if g == nil {
		return nil
	}
	var out []string
	for _, m := range g.Members {
		if m.Kind == CandidateMedia && m.MediaAssetID == nil {
			out = append(out, m.MessageID.String())
		}
	}
	return out
}

// create persists the signal from the extraction of its textual evidence.
// Mirrors the existing text path's field mapping and failure handling
// (extraction failure keeps the raw evidence and records the error).
func (in *Intake) create(ctx context.Context, vendorID uuid.UUID, raw string, res *ai.SupplyExtractionResult, extErr error,
	sources []Source, g *Group, grouping string, log *slog.Logger) (*Signal, error) {
	s := &Signal{VendorID: vendorID, RawMessage: raw, AvailabilityStatus: AvailabilityAvailable, ExtractedData: map[string]any{}}
	if extErr != nil {
		log.Error("supply extraction failed, persisting raw evidence for review", "error", extErr)
		s.ExtractedData["error"] = extErr.Error()
	} else {
		s.Category, s.Product, s.Variant, s.Condition, s.Location = res.Category, res.Product, res.Variant, res.Condition, res.Location
		s.Price, s.Quantity, s.Confidence = res.Price, res.Quantity, res.Confidence
		if res.AvailabilityStatus != "" {
			s.AvailabilityStatus = Availability(res.AvailabilityStatus)
		}
		s.ExtractedData = map[string]any{
			"category": res.Category, "product": res.Product, "variant": res.Variant,
			"condition": res.Condition, "price": res.Price, "quantity": res.Quantity,
			"location": res.Location, "availability_status": res.AvailabilityStatus,
		}
	}
	srcIDs := make([]string, len(sources))
	var media []string
	seen := map[uuid.UUID]bool{}
	for i, src := range sources {
		srcIDs[i] = src.MessageID.String()
		if src.MediaAssetID != nil && !seen[*src.MediaAssetID] {
			seen[*src.MediaAssetID] = true
			media = append(media, src.MediaAssetID.String())
		}
	}
	// Interpretation metadata only; the evidence itself is the source rows.
	s.ExtractedData["extraction_source"] = "ai-service /extract-supply"
	s.ExtractedData["grouping"] = grouping

	ev := &event.Event{
		EventType: event.SupplySignalCreated, EntityType: "supply_signal", ActorType: event.ActorVendor, ActorID: &vendorID,
		Payload: event.Merge(map[string]any{
			"raw_message": raw, "vendor_id": vendorID.String(), "category": s.Category, "product": s.Product,
			"variant": s.Variant, "price": s.Price, "location": s.Location, "availability": string(s.AvailabilityStatus),
			"confidence": s.Confidence, "channel": "whatsapp", "grouping": grouping,
		}, map[string]any{"source_message_ids": srcIDs}),
	}
	if len(media) > 0 {
		ev.Payload["media_asset_ids"] = media
	}
	if u := unavailableMedia(g); len(u) > 0 {
		ev.Payload["media_unavailable_message_ids"] = u
	}

	created, err := in.repo.CreateSignalWithSources(ctx, s, sources, ev)
	if errors.Is(err, ErrSourcesTaken) {
		log.Info("submission already became a signal elsewhere; nothing to do", "source_message_ids", srcIDs)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("persisting supply signal: %w", err)
	}
	if in.vendors != nil {
		_ = in.vendors.RecordSupplySubmission(ctx, vendorID)
	}
	log.Info("supply signal created", "supply_signal_id", created.ID, "grouping", grouping,
		"source_message_ids", srcIDs, "media_asset_ids", media, "product", strOr(created.Product))
	return created, nil
}

func (in *Intake) appendLate(ctx context.Context, signalID, vendorID uuid.UUID, offset int, g Group, log *slog.Logger) error {
	sources := make([]Source, len(g.Members))
	srcIDs := make([]string, len(g.Members))
	for i, m := range g.Members {
		sources[i] = Source{MessageID: m.MessageID, Position: offset + i, MediaAssetID: m.MediaAssetID}
		srcIDs[i] = m.MessageID.String()
	}
	ev := &event.Event{
		EventType: event.SupplySignalUpdated, EntityType: "supply_signal", EntityID: &signalID, ActorType: event.ActorVendor, ActorID: &vendorID,
		Payload: event.Merge(map[string]any{"supply_signal_id": signalID.String(), "vendor_id": vendorID.String(),
			"change": "media_added_late", "channel": "whatsapp"}, map[string]any{"added_source_message_ids": srcIDs}),
	}
	if a := assetIDs(g); len(a) > 0 {
		ev.Payload["added_media_asset_ids"] = a
	}
	if err := in.repo.AppendSources(ctx, signalID, sources, ev); errors.Is(err, ErrSourcesTaken) {
		return nil
	} else if err != nil {
		return fmt.Errorf("adding late media to supply signal: %w", err)
	}
	log.Info("late media added to existing supply signal", "supply_signal_id", signalID,
		"added_source_message_ids", srcIDs, "added_media_asset_ids", assetIDs(g))
	return nil
}

func strOr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
