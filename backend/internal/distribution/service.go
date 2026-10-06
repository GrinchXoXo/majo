package distribution

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/channel"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/discovery"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/presentation"
	"github.com/middleman/backend/internal/request"
)

// Outbound sends a rendered message through the recording sender (every
// message is persisted with its purpose and provider message ID).
type Outbound interface {
	SendRecorded(ctx context.Context, to string, m channel.Message) (messageID *uuid.UUID, providerMessageID string, err error)
}

// Requests is the existing demand entry point (request.Service).
type Requests interface {
	Create(ctx context.Context, n request.NewRequest) (*request.BuyerRequest, bool, error)
	ConfirmAs(ctx context.Context, id uuid.UUID, actor event.ActorType, reason string) (*request.BuyerRequest, error)
}

// Service is the Distribution application service.
type Service struct {
	cfg       Config
	store     Store
	discovery *discovery.Service
	out       Outbound
	media     *MediaPublisher
	requests  Requests
	events    event.Writer
	logger    *slog.Logger
	now       func() time.Time
}

// NewService wires Distribution.
func NewService(cfg Config, store Store, disc *discovery.Service, out Outbound, mediaPub *MediaPublisher, requests Requests, events event.Writer, logger *slog.Logger) *Service {
	return &Service{cfg: cfg, store: store, discovery: disc, out: out, media: mediaPub, requests: requests, events: events, logger: logger, now: time.Now}
}

// SetClock replaces the clock (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now; s.discovery.SetClock(now) }

// Config returns the active configuration.
func (s *Service) Config() Config { return s.cfg }

// Errors.
var (
	ErrDiscoveryOff     = errors.New("discovery distribution is OFF")
	ErrOutsideWindow    = errors.New("outside the configured delivery window")
	ErrNotPermitted     = errors.New("recipient is not an internal staff recipient")
	ErrBatchNotFound    = errors.New("discovery batch not found")
	ErrDeliveryNotFound = errors.New("discovery delivery not found")
	ErrUnknownBuyer     = errors.New("no buyer with this WhatsApp number has messaged Majo")
	ErrInvalidInput     = errors.New("invalid input")
)

// PlanRequest describes a dry run or a batch.
type PlanRequest struct {
	ClientKey  string
	Scope      discovery.Scope
	Recipients []string // optional explicit audience (else every buyer with a consent record)
	CreatedBy  string
	Note       string
}

func (s *Service) audienceQuery(now time.Time, recipients []string) AudienceQuery {
	return AudienceQuery{Recipients: recipients, ExposureSince: now.Add(-s.cfg.ExposureWindow),
		CapSince: now.Add(-s.cfg.CapPeriod), NegativeSince: now.Add(-s.cfg.NegativeWindow)}
}

func (s *Service) plan(ctx context.Context, batchID uuid.UUID, pr PlanRequest) (PlanReport, []*Delivery, error) {
	now := s.now().UTC()
	members, unknown, err := s.store.Audience(ctx, s.audienceQuery(now, pr.Recipients))
	if err != nil {
		return PlanReport{}, nil, fmt.Errorf("loading audience: %w", err)
	}
	items, err := s.discovery.Load(ctx, pr.Scope)
	if err != nil {
		return PlanReport{}, nil, fmt.Errorf("loading supply: %w", err)
	}
	pl := planner{cfg: s.cfg, engine: s.discovery.Engine(), now: now}
	rep, ds := pl.Plan(batchID, pr.Scope, members, unknown, items)
	return rep, ds, nil
}

// Preview is the dry run: audience, eligible and excluded listings and
// buyers, ranking results. Nothing is persisted or sent. Allowed in any mode.
func (s *Service) Preview(ctx context.Context, pr PlanRequest) (PlanReport, error) {
	rep, _, err := s.plan(ctx, uuid.Nil, pr)
	return rep, err
}

// CreateBatch plans and persists a batch (status PLANNED, nothing sent).
// Idempotent per client key: a repeat returns the existing batch.
func (s *Service) CreateBatch(ctx context.Context, pr PlanRequest) (*Batch, bool, error) {
	if !s.cfg.CanSend() {
		return nil, false, ErrDiscoveryOff
	}
	if pr.ClientKey == "" || len(pr.ClientKey) > 100 {
		return nil, false, fmt.Errorf("%w: client_key is required (max 100 chars)", ErrInvalidInput)
	}
	id := uuid.New()
	rep, ds, err := s.plan(ctx, id, pr)
	if err != nil {
		return nil, false, err
	}
	b := &Batch{ID: id, ClientKey: pr.ClientKey, Status: BatchPlanned, Mode: s.cfg.Mode, RankingVersion: rep.RankingVersion,
		PresentationVersion: presentation.Version, Arms: rep.Arms, Scope: pr.Scope, Plan: rep, CreatedBy: pr.CreatedBy, Note: pr.Note}
	saved, created, err := s.store.CreateBatch(ctx, b, ds)
	if err != nil {
		return nil, false, err
	}
	if created {
		s.event(ctx, event.DiscoveryBatchCreated, "discovery_batch", saved.ID, event.ActorOperator, map[string]any{
			"batch_id": saved.ID.String(), "mode": string(saved.Mode), "ranking_version": saved.RankingVersion,
			"presentation_version": saved.PresentationVersion, "arms": saved.Arms, "deliveries": len(ds),
			"audience_considered": rep.AudienceConsidered, "audience_eligible": rep.AudienceEligible,
			"eligible_listings": rep.EligibleListings, "excluded_buyers": rep.ExcludedBuyers, "listing_exclusions": rep.ListingExclusions,
		})
	}
	return saved, created, nil
}

// SendReport is what a send did.
type SendReport struct {
	BatchID uuid.UUID      `json:"batch_id"`
	Status  string         `json:"status"`
	Sent    int            `json:"sent"`
	Failed  int            `json:"failed"`
	Skipped map[string]int `json:"skipped"`
	Already bool           `json:"already_sent"`
}

// SendBatch sends every PLANNED delivery of a batch. Operator-triggered
// only; refused unless the gate is INTERNAL and the delivery window is
// open. Each delivery is isolated: one failure never blanks the batch.
// Idempotent: only a PLANNED batch is sent, and only once.
func (s *Service) SendBatch(ctx context.Context, batchID uuid.UUID) (*SendReport, error) {
	if !s.cfg.CanSend() {
		return nil, ErrDiscoveryOff
	}
	now := s.now().UTC()
	if !s.cfg.Window.Allows(now) {
		return nil, ErrOutsideWindow
	}
	// A send must not be abandoned half way because the operator's HTTP
	// request was cancelled: claimed deliveries would stay SENDING (cap and
	// exposure consumed) and the rest of the batch could never be sent.
	// Cancellation is detached; a generous timeout still bounds the run.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendBatchTimeout)
	defer cancel()
	b, err := s.store.FindBatch(ctx, batchID)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, ErrBatchNotFound
	}
	rep := &SendReport{BatchID: b.ID, Skipped: map[string]int{}}
	started, err := s.store.BeginBatchSend(ctx, b.ID, now)
	if err != nil {
		return nil, err
	}
	if !started {
		rep.Status, rep.Already = b.Status, true
		return rep, nil
	}
	ds, err := s.store.ListDeliveries(ctx, b.ID)
	if err != nil {
		return nil, err
	}
	for _, d := range ds {
		if d.Status == DeliverySending && d.ClaimedAt != nil && now.Sub(*d.ClaimedAt) > staleSendingAfter {
			// A previous run was interrupted between claim and confirmation.
			// It is surfaced as FAILED (never silently re-sent: the buyer may
			// already have the message) and its cap/exposure is released.
			if err := s.store.MarkDeliveryFailed(ctx, d.ID, "", "interrupted: no send confirmation was recorded"); err != nil {
				s.logger.Error("recovering interrupted discovery delivery", "delivery_id", d.ID, "error", err)
			}
			rep.Failed++
			continue
		}
		if d.Status != DeliveryPlanned {
			continue
		}
		outcome, reason := s.sendOne(ctx, b, d)
		switch outcome {
		case DeliverySent:
			rep.Sent++
		case DeliveryFailed:
			rep.Failed++
		default:
			rep.Skipped[reason]++
		}
	}
	skipped := 0
	for _, n := range rep.Skipped {
		skipped += n
	}
	switch {
	case rep.Sent > 0 && rep.Failed == 0 && skipped == 0:
		rep.Status = BatchSent
	case rep.Sent > 0:
		rep.Status = BatchPartial
	default:
		rep.Status = BatchFailed
	}
	if err := s.store.FinishBatch(ctx, b.ID, rep.Status, s.now().UTC()); err != nil {
		return nil, err
	}
	s.event(ctx, event.DiscoveryBatchSent, "discovery_batch", b.ID, event.ActorOperator, map[string]any{
		"batch_id": b.ID.String(), "status": rep.Status, "sent": rep.Sent, "failed": rep.Failed, "skipped": rep.Skipped,
		"ranking_version": b.RankingVersion,
	})
	return rep, nil
}

const (
	// sendBatchTimeout bounds one operator-triggered send run.
	sendBatchTimeout = 10 * time.Minute
	// staleSendingAfter: a SENDING delivery/batch older than this was interrupted.
	staleSendingAfter = 15 * time.Minute
)

// deliveryContext is the fresh state used to (re)present a delivery.
type deliveryContext struct {
	member *AudienceMember
	viewer discovery.ViewerContext
	cands  []discovery.Candidate // still eligible, in planned order
	media  map[uuid.UUID]listingMedia
}

// refresh re-evaluates a delivery's listings through Discovery (canonical
// eligibility + viewer reach) at this instant: anything that expired or
// left the viewer's scope since planning is dropped.
func (s *Service) refresh(ctx context.Context, d *Delivery, ids []uuid.UUID) (*deliveryContext, error) {
	now := s.now().UTC()
	m, err := s.store.AudienceMember(ctx, d.BuyerID, s.audienceQuery(now, nil))
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, ErrUnknownBuyer
	}
	items, err := s.store.SupplyByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	viewer := ViewerFrom(*m)
	viewer.RecentlyShown = nil // already-planned listings are not down-ranked against themselves
	res := s.discovery.Engine().Run(items, viewer, discovery.Scope{Limit: len(items) + 1}, s.cfg.Ranking, now)
	byID := map[uuid.UUID]discovery.Candidate{}
	for _, c := range res.Candidates {
		byID[c.ListingID] = c
	}
	dc := &deliveryContext{member: m, viewer: viewer, media: map[uuid.UUID]listingMedia{}}
	for _, id := range ids {
		if c, ok := byID[id]; ok {
			dc.cands = append(dc.cands, c)
			dc.media[id] = mediaOf(c.Listing)
		}
	}
	return dc, nil
}

func (s *Service) sessionOpen(m *AudienceMember) bool {
	return m.LastInboundAt != nil && s.now().UTC().Sub(*m.LastInboundAt) < SessionWindow
}

func (s *Service) sendOne(ctx context.Context, b *Batch, d *Delivery) (string, string) {
	skip := func(reason string) (string, string) {
		if err := s.store.SkipDelivery(ctx, d.ID, reason); err != nil {
			s.logger.Error("skipping discovery delivery", "delivery_id", d.ID, "error", err)
		}
		s.event(ctx, event.DiscoveryDeliverySkipped, "discovery_delivery", d.ID, event.ActorSystem, map[string]any{
			"batch_id": b.ID.String(), "buyer_id": d.BuyerID.String(), "arm": d.Arm, "reason": reason,
		})
		return DeliverySkipped, reason
	}
	// Hard staff-only boundary, re-checked per recipient at send time.
	if ok, reason := s.cfg.Permits(d.Recipient); !ok {
		return skip(reason)
	}
	dc, err := s.refresh(ctx, d, d.ListingIDs)
	if err != nil {
		s.logger.Error("refreshing discovery delivery", "delivery_id", d.ID, "error", err)
		return skip(ReasonNoLongerEligible)
	}
	if len(dc.cands) == 0 {
		return skip(ReasonNoLongerEligible)
	}
	session := s.sessionOpen(dc.member)
	if !session && s.cfg.Template.Name == "" {
		return skip(ReasonTemplateRequired)
	}
	keep := make([]uuid.UUID, len(dc.cands))
	for i, c := range dc.cands {
		keep[i] = c.ListingID
	}
	claim, err := s.store.ClaimDelivery(ctx, d.ID, ClaimRules{
		CapSince: s.now().UTC().Add(-s.cfg.CapPeriod), MaxPerBuyer: s.cfg.MaxDeliveriesPerBuyer,
		ExposureSince: s.now().UTC().Add(-s.cfg.ExposureWindow), Keep: keep,
	}, s.now().UTC())
	if err != nil {
		s.logger.Error("claiming discovery delivery", "delivery_id", d.ID, "error", err)
		return DeliveryFailed, err.Error()
	}
	if !claim.Claimed {
		s.event(ctx, event.DiscoveryDeliverySkipped, "discovery_delivery", d.ID, event.ActorSystem, map[string]any{
			"batch_id": b.ID.String(), "buyer_id": d.BuyerID.String(), "arm": d.Arm, "reason": claim.SkipReason,
		})
		return DeliverySkipped, claim.SkipReason
	}
	present := map[uuid.UUID]bool{}
	for _, id := range claim.Listings {
		present[id] = true
	}
	var cands []discovery.Candidate
	for _, c := range dc.cands {
		if present[c.ListingID] {
			cands = append(cands, c)
		}
	}
	presentationKind := "session"
	if !session {
		presentationKind = "template"
	}
	msgs, err := presentation.Entry(d.ID, presentation.Arm(d.Arm), cardsFor(cands, dc.media), session, s.cfg.Template)
	if err != nil {
		_ = s.store.MarkDeliveryFailed(ctx, d.ID, presentationKind, err.Error())
		return DeliveryFailed, err.Error()
	}
	first, firstWamid, err := s.sendMessages(ctx, d, b.ID, presentationKind, msgs)
	if err != nil {
		if mErr := s.store.MarkDeliveryFailed(ctx, d.ID, presentationKind, err.Error()); mErr != nil {
			s.logger.Error("marking discovery delivery failed", "delivery_id", d.ID, "error", mErr)
		}
		s.event(ctx, event.DiscoveryDeliveryFailed, "discovery_delivery", d.ID, event.ActorSystem, map[string]any{
			"batch_id": b.ID.String(), "buyer_id": d.BuyerID.String(), "arm": d.Arm, "error": err.Error(), "presentation": presentationKind,
		})
		return DeliveryFailed, err.Error()
	}
	if err := s.store.MarkDeliverySent(ctx, d.ID, presentationKind, first, firstWamid, s.now().UTC()); err != nil {
		s.logger.Error("marking discovery delivery sent", "delivery_id", d.ID, "error", err)
	}
	refs := make([]string, len(cands))
	for i, c := range cands {
		refs[i] = c.PublicReference
	}
	s.event(ctx, event.DiscoveryDeliverySent, "discovery_delivery", d.ID, event.ActorSystem, map[string]any{
		"batch_id": b.ID.String(), "buyer_id": d.BuyerID.String(), "arm": d.Arm, "presentation": presentationKind,
		"listing_refs": refs, "deduped": len(claim.Deduped), "provider_message_id": firstWamid, "ranking_version": b.RankingVersion,
	})
	return DeliverySent, ""
}

// sendMessages publishes media and sends each message. A card whose image
// cannot be published degrades to a text card; the first message failing
// fails the delivery; a later failure is logged (the buyer already has the
// entry message).
func (s *Service) sendMessages(ctx context.Context, d *Delivery, batchID uuid.UUID, kind string, msgs []channel.Message) (*uuid.UUID, string, error) {
	var first *uuid.UUID
	firstWamid := ""
	for i, m := range msgs {
		m = s.publishMedia(ctx, d.ID, m)
		if m.Kind == channel.KindVideo && m.Media.ProviderRef == "" {
			continue // video could not be published; the follow-up prompt still goes out
		}
		did := d.ID
		sendCtx := conversation.WithOutboundPurpose(ctx, conversation.OutboundPurpose{
			Kind: "discovery_" + kind, EntityType: "discovery_delivery", EntityID: &did,
			Extra: map[string]any{"batch_id": batchID.String(), "arm": d.Arm, "message_kind": string(m.Kind), "interaction_ids": m.InteractionIDs()},
		})
		msgID, wamid, err := s.out.SendRecorded(sendCtx, d.Recipient, m)
		if err != nil {
			if i == 0 {
				return nil, "", err
			}
			s.logger.Error("sending discovery follow-up message", "delivery_id", d.ID, "error", err)
			continue
		}
		if first == nil {
			first, firstWamid = msgID, wamid
		}
	}
	return first, firstWamid, nil
}

func (s *Service) publishMedia(ctx context.Context, deliveryID uuid.UUID, m channel.Message) channel.Message {
	if s.media == nil {
		return presentation.TextOnly(m)
	}
	if m.Header != nil {
		h := *m.Header
		if err := s.media.Publish(ctx, &h); err != nil {
			s.logger.Warn("card image not published, sending text card", "delivery_id", deliveryID, "asset_id", h.AssetID, "error", err)
			return presentation.TextOnly(m)
		}
		m.Header = &h
	}
	if m.Media != nil {
		md := *m.Media
		if err := s.media.Publish(ctx, &md); err != nil {
			s.logger.Warn("media not published", "delivery_id", deliveryID, "asset_id", md.AssetID, "error", err)
		}
		m.Media = &md
	}
	return m
}

// Decision answers "is this delivery currently permitted?" without sending.
type Decision struct {
	Permitted bool     `json:"permitted"`
	Reasons   []string `json:"reasons"`
}

// Permitted evaluates gate, delivery status, window, consent, cap and dedupe now.
func (s *Service) Permitted(ctx context.Context, deliveryID uuid.UUID) (*Decision, error) {
	d, err := s.store.FindDelivery(ctx, deliveryID)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, ErrDeliveryNotFound
	}
	now := s.now().UTC()
	dec := &Decision{Reasons: []string{}}
	add := func(r string) { dec.Reasons = append(dec.Reasons, r) }
	if ok, reason := s.cfg.Permits(d.Recipient); !ok {
		add(reason)
	}
	if d.Status != DeliveryPlanned {
		add(ReasonDeliveryNotPending)
	}
	if !s.cfg.Window.Allows(now) {
		add(ReasonOutsideWindow)
	}
	m, err := s.store.AudienceMember(ctx, d.BuyerID, s.audienceQuery(now, nil))
	if err != nil {
		return nil, err
	}
	switch {
	case m == nil || m.Preference == nil:
		add(ReasonNoConsent)
	case m.Preference.Status != OptedIn:
		add(ReasonOptedOut)
	}
	if m != nil {
		if m.DeliveriesInCapPeriod >= s.cfg.MaxDeliveriesPerBuyer {
			add(ReasonFrequencyCap)
		}
		fresh := 0
		for _, id := range d.ListingIDs {
			if _, shown := m.Exposed[id]; !shown {
				fresh++
			}
		}
		if fresh == 0 {
			add(ReasonDedupe)
		}
		if !s.sessionOpen(m) && s.cfg.Template.Name == "" {
			add(ReasonTemplateRequired)
		}
	}
	dec.Permitted = len(dec.Reasons) == 0
	return dec, nil
}

func (s *Service) event(ctx context.Context, t event.Type, entityType string, id uuid.UUID, actor event.ActorType, payload map[string]any) {
	eid := id
	if err := s.events.Write(ctx, &event.Event{EventType: t, EntityType: entityType, EntityID: &eid, ActorType: actor, Payload: payload}); err != nil {
		s.logger.Error("writing discovery event", "type", t, "error", err)
	}
}

// ---- reads ----------------------------------------------------------------

func (s *Service) Batches(ctx context.Context, limit int) ([]*Batch, error) {
	return s.store.ListBatches(ctx, limit)
}

func (s *Service) Batch(ctx context.Context, id uuid.UUID) (*Batch, []*Delivery, error) {
	b, err := s.store.FindBatch(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if b == nil {
		return nil, nil, ErrBatchNotFound
	}
	ds, err := s.store.ListDeliveries(ctx, id)
	return b, ds, err
}

// DeliveryDetail is a delivery with its interactions and attribution.
type DeliveryDetail struct {
	Delivery    *Delivery    `json:"delivery"`
	Decision    *Decision    `json:"permitted_now"`
	Attribution *Attribution `json:"attribution"`
}

func (s *Service) Delivery(ctx context.Context, id uuid.UUID) (*DeliveryDetail, error) {
	d, err := s.store.FindDelivery(ctx, id)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, ErrDeliveryNotFound
	}
	dec, err := s.Permitted(ctx, id)
	if err != nil {
		return nil, err
	}
	att, err := s.store.Attribution(ctx, id)
	if err != nil {
		return nil, err
	}
	return &DeliveryDetail{Delivery: d, Decision: dec, Attribution: att}, nil
}

func (s *Service) Funnel(ctx context.Context, batchID *uuid.UUID) (*Funnel, error) {
	return s.store.Funnel(ctx, batchID)
}

func (s *Service) Preferences(ctx context.Context) ([]*Preference, error) {
	return s.store.ListPreferences(ctx)
}
