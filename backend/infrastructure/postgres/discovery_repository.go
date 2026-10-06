package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/middleman/backend/internal/discovery"
	"github.com/middleman/backend/internal/distribution"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/supply"
)

// DiscoveryRepository is the PostgreSQL store for Phase 4B: the Discovery
// supply read (discovery.SupplyReader) and Distribution persistence
// (distribution.Store): consent, batches, deliveries, exposure,
// interactions, attribution and the discovery funnel.
type DiscoveryRepository struct {
	db *sql.DB
}

func NewDiscoveryRepository(db *sql.DB) *DiscoveryRepository { return &DiscoveryRepository{db: db} }

var (
	_ discovery.SupplyReader = (*DiscoveryRepository)(nil)
	_ distribution.Store     = (*DiscoveryRepository)(nil)
)

// ---- supply ---------------------------------------------------------------

// DiscoverableSupply implements discovery.SupplyReader: ACTIVE listings
// inside their lifecycle ceiling (a pre-filter only; the canonical policy
// decides) with vendor status, source product and stored media facts.
func (r *DiscoveryRepository) DiscoverableSupply(ctx context.Context, q discovery.SupplyQuery) ([]discovery.SupplyItem, error) {
	return r.supply(ctx, `status = 'ACTIVE' AND expires_at > now()`, nil)
}

// SupplyByIDs loads specific listings (any status; the policy re-evaluates).
func (r *DiscoveryRepository) SupplyByIDs(ctx context.Context, ids []uuid.UUID) ([]discovery.SupplyItem, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return r.supply(ctx, `id = ANY($1::uuid[])`, []any{pq.Array(uuidStrings(ids))})
}

func (r *DiscoveryRepository) supply(ctx context.Context, where string, args []any) ([]discovery.SupplyItem, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+listingColumns+` FROM supply_listings WHERE `+where+` ORDER BY published_at NULLS LAST, id`, args...)
	if err != nil {
		return nil, err
	}
	var listings []*supply.Listing
	for rows.Next() {
		l, err := scanListingRow(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		listings = append(listings, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(listings) == 0 {
		return nil, err
	}
	var vendorIDs, signalIDs, assetIDs []string
	for _, l := range listings {
		vendorIDs = append(vendorIDs, l.VendorID.String())
		if l.SupplySignalID != nil {
			signalIDs = append(signalIDs, l.SupplySignalID.String())
		}
		for _, m := range l.MediaReferences {
			if m.AssetID != "" {
				assetIDs = append(assetIDs, m.AssetID)
			}
		}
	}
	vendorActive := map[string]bool{}
	if err := r.eachRow(ctx, `SELECT id, status = 'ACTIVE' FROM vendors WHERE id = ANY($1::uuid[])`, []any{pq.Array(vendorIDs)}, func(sc func(...any) error) error {
		var id string
		var active bool
		if err := sc(&id, &active); err != nil {
			return err
		}
		vendorActive[id] = active
		return nil
	}); err != nil {
		return nil, err
	}
	sourceProduct := map[string]string{}
	if len(signalIDs) > 0 {
		if err := r.eachRow(ctx, `SELECT id, COALESCE(product, '') FROM supply_signals WHERE id = ANY($1::uuid[])`, []any{pq.Array(signalIDs)}, func(sc func(...any) error) error {
			var id, p string
			if err := sc(&id, &p); err != nil {
				return err
			}
			sourceProduct[id] = supply.NormalizeSearch(p)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	assetKind := map[string]string{} // only AVAILABLE stored assets count
	if len(assetIDs) > 0 {
		if err := r.eachRow(ctx, `SELECT id, kind FROM media_assets WHERE id = ANY($1::uuid[]) AND status = 'AVAILABLE'`, []any{pq.Array(assetIDs)}, func(sc func(...any) error) error {
			var id, kind string
			if err := sc(&id, &kind); err != nil {
				return err
			}
			assetKind[id] = kind
			return nil
		}); err != nil {
			return nil, err
		}
	}
	out := make([]discovery.SupplyItem, 0, len(listings))
	for _, l := range listings {
		it := discovery.SupplyItem{Listing: l, VendorActive: vendorActive[l.VendorID.String()]}
		if l.SupplySignalID != nil {
			it.SourceProduct = sourceProduct[l.SupplySignalID.String()]
		}
		for _, m := range l.MediaReferences {
			switch assetKind[m.AssetID] {
			case "image":
				it.Images++
			case "video":
				it.HasVideo = true
			}
		}
		out = append(out, it)
	}
	return out, nil
}

func (r *DiscoveryRepository) eachRow(ctx context.Context, q string, args []any, fn func(scan func(...any) error) error) error {
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows.Scan); err != nil {
			return err
		}
	}
	return rows.Err()
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

func parseUUIDs(ss []string) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ss))
	for _, s := range ss {
		if id, err := uuid.Parse(s); err == nil {
			out = append(out, id)
		}
	}
	return out
}

func optUUID(s sql.NullString) *uuid.UUID {
	if !s.Valid {
		return nil
	}
	id, err := uuid.Parse(s.String)
	if err != nil {
		return nil
	}
	return &id
}

func optTime(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func uuidArg(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

// ---- consent --------------------------------------------------------------

const preferenceColumns = `p.id, p.buyer_id, b.external_user_id, p.status, p.location, p.interests, p.wording_version, p.source,
	p.source_message_id, p.channel, p.captured_at, p.opted_out_at, p.opt_out_reason, p.updated_at`

func scanPreference(sc interface{ Scan(...any) error }) (*distribution.Preference, error) {
	var (
		p                distribution.Preference
		id, buyerID      string
		location, reason sql.NullString
		sourceMsg        sql.NullString
		optedOut         sql.NullTime
		interests        []string
	)
	if err := sc.Scan(&id, &buyerID, &p.Recipient, &p.Status, &location, pq.Array(&interests), &p.WordingVersion, &p.Source,
		&sourceMsg, &p.Channel, &p.CapturedAt, &optedOut, &reason, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.ID, _ = uuid.Parse(id)
	p.BuyerID, _ = uuid.Parse(buyerID)
	p.Location = nullStr(location)
	p.OptOutReason = nullStr(reason)
	p.SourceMessageID = optUUID(sourceMsg)
	p.OptedOutAt = optTime(optedOut)
	p.Interests = interests
	if p.Interests == nil {
		p.Interests = []string{}
	}
	return &p, nil
}

func (r *DiscoveryRepository) Preference(ctx context.Context, buyerID uuid.UUID) (*distribution.Preference, error) {
	p, err := scanPreference(r.db.QueryRowContext(ctx, `SELECT `+preferenceColumns+`
		FROM discovery_preferences p JOIN buyers b ON b.id = p.buyer_id WHERE p.buyer_id = $1 AND p.scope = 'discovery'`, buyerID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (r *DiscoveryRepository) ListPreferences(ctx context.Context) ([]*distribution.Preference, error) {
	var out []*distribution.Preference
	rows, err := r.db.QueryContext(ctx, `SELECT `+preferenceColumns+`
		FROM discovery_preferences p JOIN buyers b ON b.id = p.buyer_id ORDER BY p.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanPreference(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *DiscoveryRepository) BuyerIDByRecipient(ctx context.Context, channel, recipient string) (*uuid.UUID, error) {
	var id string
	err := r.db.QueryRowContext(ctx, `SELECT id FROM buyers WHERE channel = $1 AND external_user_id = $2`, channel, recipient).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u, _ := uuid.Parse(id)
	return &u, nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ApplyPreference implements distribution.Store: upsert + append-only
// history + event, in one transaction, serialised per buyer.
func (r *DiscoveryRepository) ApplyPreference(ctx context.Context, c distribution.PreferenceChange) (*distribution.Preference, bool, error) {
	if c.At.IsZero() {
		c.At = time.Now().UTC()
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('discovery_pref:' || $1, 0))`, c.BuyerID.String()); err != nil {
		return nil, false, err
	}
	cur, err := scanPreference(tx.QueryRowContext(ctx, `SELECT `+preferenceColumns+`
		FROM discovery_preferences p JOIN buyers b ON b.id = p.buyer_id WHERE p.buyer_id = $1 AND p.scope = 'discovery' FOR UPDATE OF p`, c.BuyerID.String()))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		cur = nil
	}
	if cur == nil && c.Status == nil {
		return nil, false, distribution.ErrNoPreference
	}

	var from *distribution.ConsentStatus
	status := distribution.OptedIn
	var location *string
	interests := []string{}
	if cur != nil {
		st := cur.Status
		from = &st
		status, location, interests = cur.Status, cur.Location, cur.Interests
	}
	if c.Status != nil {
		status = *c.Status
	}
	if c.Location != nil {
		l := strings.TrimSpace(*c.Location)
		if l == "" {
			location = nil
		} else {
			location = &l
		}
	}
	if c.Interests != nil {
		interests = c.Interests
	}
	statusChanged := cur == nil || cur.Status != status
	locChanged := cur == nil || derefOr(cur.Location) != derefOr(location)
	intChanged := cur == nil || !sameStrings(cur.Interests, interests)
	if !statusChanged && !locChanged && !intChanged {
		return cur, false, nil
	}

	var optedOut any
	var reason any
	if status == distribution.OptedOut {
		optedOut, reason = c.At, nullIfEmpty(c.Reason)
	}
	var prefID string
	if cur == nil {
		err = tx.QueryRowContext(ctx, `INSERT INTO discovery_preferences
			(buyer_id, status, location, interests, wording_version, source, source_message_id, channel, captured_at, opted_out_at, opt_out_reason)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'whatsapp', $8, $9, $10) RETURNING id`,
			c.BuyerID.String(), string(status), location, pq.Array(interests), c.WordingVersion, c.Source, uuidArg(c.SourceMessageID),
			c.At, optedOut, reason).Scan(&prefID)
	} else if statusChanged {
		prefID = cur.ID.String()
		_, err = tx.ExecContext(ctx, `UPDATE discovery_preferences SET status = $2, location = $3, interests = $4, wording_version = $5,
			source = $6, source_message_id = $7, captured_at = $8, opted_out_at = $9, opt_out_reason = $10, updated_at = now() WHERE id = $1`,
			prefID, string(status), location, pq.Array(interests), c.WordingVersion, c.Source, uuidArg(c.SourceMessageID), c.At, optedOut, reason)
	} else {
		prefID = cur.ID.String()
		_, err = tx.ExecContext(ctx, `UPDATE discovery_preferences SET location = $2, interests = $3, updated_at = now() WHERE id = $1`,
			prefID, location, pq.Array(interests))
	}
	if err != nil {
		return nil, false, err
	}
	var fromArg any
	if from != nil {
		fromArg = string(*from)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO discovery_preference_history
		(preference_id, buyer_id, from_status, to_status, source, source_message_id, wording_version, actor_type, actor_id, reason, location, interests, at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		prefID, c.BuyerID.String(), fromArg, string(status), c.Source, uuidArg(c.SourceMessageID), c.WordingVersion,
		c.ActorType, nullIfEmpty(c.ActorID), nullIfEmpty(c.Reason), location, pq.Array(interests), c.At); err != nil {
		return nil, false, err
	}
	et := event.DiscoveryPreferenceUpdated
	if statusChanged {
		et = event.DiscoveryOptedIn
		if status == distribution.OptedOut {
			et = event.DiscoveryOptedOut
		}
	}
	buyerID := c.BuyerID
	actor := event.ActorBuyer
	if c.ActorType == "OPERATOR" {
		actor = event.ActorOperator
	}
	payload := map[string]any{"buyer_id": c.BuyerID.String(), "from_status": fromArg, "to_status": string(status),
		"source": c.Source, "wording_version": c.WordingVersion, "reason": c.Reason, "location": derefOr(location), "interests": interests}
	if c.SourceMessageID != nil {
		payload["source_message_id"] = c.SourceMessageID.String()
	}
	if err := insertEventTx(ctx, tx, &event.Event{EventType: et, EntityType: "buyer", EntityID: &buyerID, ActorType: actor, Payload: payload}); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	p, err := r.Preference(ctx, c.BuyerID)
	return p, true, err
}

func derefOr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---- audience -------------------------------------------------------------

func (r *DiscoveryRepository) Audience(ctx context.Context, q distribution.AudienceQuery) ([]distribution.AudienceMember, []string, error) {
	type buyerRow struct{ id, recipient string }
	var buyers []buyerRow
	var unknown []string
	if len(q.Recipients) > 0 {
		found := map[string]string{}
		if err := r.eachRow(ctx, `SELECT id, external_user_id FROM buyers WHERE channel = 'whatsapp' AND external_user_id = ANY($1)`,
			[]any{pq.Array(q.Recipients)}, func(sc func(...any) error) error {
				var id, ext string
				if err := sc(&id, &ext); err != nil {
					return err
				}
				found[ext] = id
				return nil
			}); err != nil {
			return nil, nil, err
		}
		seen := map[string]bool{}
		for _, rcp := range q.Recipients {
			if seen[rcp] {
				continue
			}
			seen[rcp] = true
			if id, ok := found[rcp]; ok {
				buyers = append(buyers, buyerRow{id, rcp})
			} else {
				unknown = append(unknown, rcp)
			}
		}
	} else {
		if err := r.eachRow(ctx, `SELECT b.id, b.external_user_id FROM discovery_preferences p JOIN buyers b ON b.id = p.buyer_id
			ORDER BY b.external_user_id`, nil, func(sc func(...any) error) error {
			var b buyerRow
			if err := sc(&b.id, &b.recipient); err != nil {
				return err
			}
			buyers = append(buyers, b)
			return nil
		}); err != nil {
			return nil, nil, err
		}
	}
	members := make([]distribution.AudienceMember, 0, len(buyers))
	for _, b := range buyers {
		id, _ := uuid.Parse(b.id)
		m, err := r.AudienceMember(ctx, id, q)
		if err != nil {
			return nil, nil, err
		}
		if m != nil {
			members = append(members, *m)
		}
	}
	return members, unknown, nil
}

func (r *DiscoveryRepository) AudienceMember(ctx context.Context, buyerID uuid.UUID, q distribution.AudienceQuery) (*distribution.AudienceMember, error) {
	m := distribution.AudienceMember{BuyerID: buyerID, Exposed: map[uuid.UUID]time.Time{}}
	err := r.db.QueryRowContext(ctx, `SELECT external_user_id FROM buyers WHERE id = $1`, buyerID.String()).Scan(&m.Recipient)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var convID sql.NullString
	var lastIn sql.NullTime
	if err := r.db.QueryRowContext(ctx, `SELECT c.id, (SELECT max(m.created_at) FROM messages m WHERE m.conversation_id = c.id AND m.direction = 'INBOUND')
		FROM conversations c WHERE c.channel = 'whatsapp' AND c.external_id = $1`, m.Recipient).Scan(&convID, &lastIn); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	m.ConversationID = optUUID(convID)
	m.LastInboundAt = optTime(lastIn)
	if m.Preference, err = r.Preference(ctx, buyerID); err != nil {
		return nil, err
	}
	if err := r.eachRow(ctx, `SELECT category, product, budget_max, location, created_at FROM buyer_requests
		WHERE buyer_id = $1 ORDER BY created_at DESC LIMIT 5`, []any{buyerID.String()}, func(sc func(...any) error) error {
		var rr distribution.RecentRequest
		var cat, prod, loc sql.NullString
		var budget sql.NullInt64
		if err := sc(&cat, &prod, &budget, &loc, &rr.CreatedAt); err != nil {
			return err
		}
		rr.Category, rr.Product, rr.Location = nullStr(cat), nullStr(prod), nullStr(loc)
		if budget.Valid {
			b := budget.Int64
			rr.BudgetMax = &b
		}
		m.RecentRequests = append(m.RecentRequests, rr)
		return nil
	}); err != nil {
		return nil, err
	}
	negSeen := map[string]bool{}
	if err := r.eachRow(ctx, `SELECT COALESCE(category, '') FROM discovery_interactions
		WHERE buyer_id = $1 AND action = 'not_interested' AND outcome = 'APPLIED' AND created_at >= $2`,
		[]any{buyerID.String(), q.NegativeSince}, func(sc func(...any) error) error {
			var cats string
			if err := sc(&cats); err != nil {
				return err
			}
			for _, c := range strings.Split(cats, ",") {
				if c = strings.TrimSpace(c); c != "" && !negSeen[c] {
					negSeen[c] = true
					m.NegativeCategories = append(m.NegativeCategories, c)
				}
			}
			return nil
		}); err != nil {
		return nil, err
	}
	if err := r.eachRow(ctx, `SELECT listing_id, max(exposed_at) FROM discovery_exposures WHERE buyer_id = $1 AND exposed_at >= $2 GROUP BY listing_id`,
		[]any{buyerID.String(), q.ExposureSince}, func(sc func(...any) error) error {
			var id string
			var at time.Time
			if err := sc(&id, &at); err != nil {
				return err
			}
			if u, err := uuid.Parse(id); err == nil {
				m.Exposed[u] = at
			}
			return nil
		}); err != nil {
		return nil, err
	}
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM discovery_deliveries
		WHERE buyer_id = $1 AND status IN ('SENDING', 'SENT') AND claimed_at >= $2`, buyerID.String(), q.CapSince).Scan(&m.DeliveriesInCapPeriod); err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *DiscoveryRepository) HasConsentPrompt(ctx context.Context, buyerID uuid.UUID, since time.Time) (bool, error) {
	var ok bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM messages m JOIN conversations c ON c.id = m.conversation_id JOIN buyers b ON b.external_user_id = c.external_id AND b.channel = c.channel
		WHERE b.id = $1 AND m.direction = 'OUTBOUND' AND m.metadata->>'kind' = 'discovery_consent_prompt' AND m.created_at >= $2)`,
		buyerID.String(), since).Scan(&ok)
	return ok, err
}

// ---- batches & deliveries ---------------------------------------------------

const batchColumns = `id, client_key, status, mode, ranking_version, presentation_version, arms, scope, plan,
	COALESCE(created_by, ''), COALESCE(note, ''), created_at, updated_at, send_started_at, sent_at`

func scanBatch(sc interface{ Scan(...any) error }) (*distribution.Batch, error) {
	var (
		b               distribution.Batch
		id              string
		arms            []string
		scope, plan     []byte
		started, sentAt sql.NullTime
	)
	if err := sc.Scan(&id, &b.ClientKey, &b.Status, &b.Mode, &b.RankingVersion, &b.PresentationVersion, pq.Array(&arms), &scope, &plan,
		&b.CreatedBy, &b.Note, &b.CreatedAt, &b.UpdatedAt, &started, &sentAt); err != nil {
		return nil, err
	}
	b.ID, _ = uuid.Parse(id)
	b.Arms = arms
	_ = json.Unmarshal(scope, &b.Scope)
	_ = json.Unmarshal(plan, &b.Plan)
	b.SendStartedAt, b.SentAt = optTime(started), optTime(sentAt)
	return &b, nil
}

const deliveryColumns = `id, batch_id, buyer_id, recipient, arm, status, dedupe_key, consent_status, preference_id, listing_ids,
	candidates, COALESCE(presentation, ''), COALESCE(skip_reason, ''), COALESCE(error, ''), message_id, COALESCE(provider_message_id, ''),
	planned_at, claimed_at, sent_at, first_interaction_at, last_interaction_at, interaction_count, COALESCE(last_action, '')`

func scanDelivery(sc interface{ Scan(...any) error }) (*distribution.Delivery, error) {
	var (
		d                                distribution.Delivery
		id, batchID, buyerID             string
		prefID, msgID                    sql.NullString
		listingIDs                       []string
		cands                            []byte
		claimed, sent, firstInt, lastInt sql.NullTime
	)
	if err := sc.Scan(&id, &batchID, &buyerID, &d.Recipient, &d.Arm, &d.Status, &d.DedupeKey, &d.ConsentStatus, &prefID, pq.Array(&listingIDs),
		&cands, &d.Presentation, &d.SkipReason, &d.Error, &msgID, &d.ProviderMessageID,
		&d.PlannedAt, &claimed, &sent, &firstInt, &lastInt, &d.InteractionCount, &d.LastAction); err != nil {
		return nil, err
	}
	d.ID, _ = uuid.Parse(id)
	d.BatchID, _ = uuid.Parse(batchID)
	d.BuyerID, _ = uuid.Parse(buyerID)
	d.PreferenceID, d.MessageID = optUUID(prefID), optUUID(msgID)
	d.ListingIDs = parseUUIDs(listingIDs)
	_ = json.Unmarshal(cands, &d.Candidates)
	d.ClaimedAt, d.SentAt, d.FirstInteractionAt, d.LastInteractionAt = optTime(claimed), optTime(sent), optTime(firstInt), optTime(lastInt)
	return &d, nil
}

func (r *DiscoveryRepository) CreateBatch(ctx context.Context, b *distribution.Batch, ds []*distribution.Delivery) (*distribution.Batch, bool, error) {
	scope, _ := json.Marshal(b.Scope)
	plan, _ := json.Marshal(b.Plan)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `INSERT INTO discovery_batches
		(id, client_key, status, mode, ranking_version, presentation_version, arms, scope, plan, created_by, note)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (client_key) DO NOTHING RETURNING id`,
		b.ID.String(), b.ClientKey, b.Status, string(b.Mode), b.RankingVersion, b.PresentationVersion, pq.Array(b.Arms), scope, plan,
		nullIfEmpty(b.CreatedBy), nullIfEmpty(b.Note)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		tx.Rollback()
		existing, err := scanBatch(r.db.QueryRowContext(ctx, `SELECT `+batchColumns+` FROM discovery_batches WHERE client_key = $1`, b.ClientKey))
		return existing, false, err
	}
	if err != nil {
		return nil, false, err
	}
	for _, d := range ds {
		cands, _ := json.Marshal(d.Candidates)
		if _, err := tx.ExecContext(ctx, `INSERT INTO discovery_deliveries
			(id, batch_id, buyer_id, recipient, arm, status, dedupe_key, consent_status, preference_id, listing_ids, candidates, planned_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) ON CONFLICT (dedupe_key) DO NOTHING`,
			d.ID.String(), b.ID.String(), d.BuyerID.String(), d.Recipient, d.Arm, d.Status, d.DedupeKey, d.ConsentStatus,
			uuidArg(d.PreferenceID), pq.Array(uuidStrings(d.ListingIDs)), cands, d.PlannedAt); err != nil {
			return nil, false, fmt.Errorf("inserting delivery: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	saved, err := r.FindBatch(ctx, b.ID)
	return saved, true, err
}

func (r *DiscoveryRepository) FindBatch(ctx context.Context, id uuid.UUID) (*distribution.Batch, error) {
	b, err := scanBatch(r.db.QueryRowContext(ctx, `SELECT `+batchColumns+` FROM discovery_batches WHERE id = $1`, id.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

func (r *DiscoveryRepository) ListBatches(ctx context.Context, limit int) ([]*distribution.Batch, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+batchColumns+` FROM discovery_batches ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*distribution.Batch
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (r *DiscoveryRepository) ListDeliveries(ctx context.Context, batchID uuid.UUID) ([]*distribution.Delivery, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+deliveryColumns+` FROM discovery_deliveries WHERE batch_id = $1 ORDER BY recipient, id`, batchID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*distribution.Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r *DiscoveryRepository) FindDelivery(ctx context.Context, id uuid.UUID) (*distribution.Delivery, error) {
	d, err := scanDelivery(r.db.QueryRowContext(ctx, `SELECT `+deliveryColumns+` FROM discovery_deliveries WHERE id = $1`, id.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return d, err
}

func (r *DiscoveryRepository) BeginBatchSend(ctx context.Context, id uuid.UUID, at time.Time) (bool, error) {
	res, err := r.db.ExecContext(ctx, `UPDATE discovery_batches SET status = 'SENDING', send_started_at = $2, updated_at = now()
		WHERE id = $1 AND (status = 'PLANNED' OR (status = 'SENDING' AND updated_at < now() - interval '15 minutes'))`, id.String(), at)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (r *DiscoveryRepository) FinishBatch(ctx context.Context, id uuid.UUID, status string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE discovery_batches SET status = $2, sent_at = $3, updated_at = now() WHERE id = $1`, id.String(), status, at)
	return err
}

// ClaimDelivery implements distribution.Store (see the interface). The
// buyer-scoped advisory lock serialises concurrent sends to one buyer, so
// two batches can never both pass the cap or expose the same listing.
func (r *DiscoveryRepository) ClaimDelivery(ctx context.Context, deliveryID uuid.UUID, rules distribution.ClaimRules, at time.Time) (distribution.ClaimResult, error) {
	var res distribution.ClaimResult
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	var buyerID, status string
	if err := tx.QueryRowContext(ctx, `SELECT buyer_id, status FROM discovery_deliveries WHERE id = $1 FOR UPDATE`, deliveryID.String()).Scan(&buyerID, &status); err != nil {
		return res, err
	}
	if status != distribution.DeliveryPlanned {
		res.SkipReason = distribution.ReasonDeliveryNotPending
		return res, nil
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('discovery_buyer:' || $1, 0))`, buyerID); err != nil {
		return res, err
	}
	skip := func(reason string) (distribution.ClaimResult, error) {
		if _, err := tx.ExecContext(ctx, `UPDATE discovery_deliveries SET status = 'SKIPPED', skip_reason = $2, updated_at = now() WHERE id = $1`,
			deliveryID.String(), reason); err != nil {
			return res, err
		}
		res.SkipReason = reason
		return res, tx.Commit()
	}
	var consent sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT status FROM discovery_preferences WHERE buyer_id = $1 AND scope = 'discovery'`, buyerID).Scan(&consent); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return res, err
	}
	switch {
	case !consent.Valid:
		return skip(distribution.ReasonNoConsent)
	case consent.String != string(distribution.OptedIn):
		return skip(distribution.ReasonOptedOut)
	}
	var recent int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM discovery_deliveries
		WHERE buyer_id = $1 AND id <> $2 AND status IN ('SENDING', 'SENT') AND claimed_at >= $3`, buyerID, deliveryID.String(), rules.CapSince).Scan(&recent); err != nil {
		return res, err
	}
	if recent >= rules.MaxPerBuyer {
		return skip(distribution.ReasonFrequencyCap)
	}
	exposed := map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT listing_id FROM discovery_exposures
		WHERE buyer_id = $1 AND listing_id = ANY($2::uuid[]) AND exposed_at >= $3`, buyerID, pq.Array(uuidStrings(rules.Keep)), rules.ExposureSince)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return res, err
		}
		exposed[id] = true
	}
	rows.Close()
	for _, id := range rules.Keep {
		if exposed[id.String()] {
			res.Deduped = append(res.Deduped, id)
		} else {
			res.Listings = append(res.Listings, id)
		}
	}
	if len(res.Listings) == 0 {
		return skip(distribution.ReasonDedupe)
	}
	for _, id := range res.Listings {
		if _, err := tx.ExecContext(ctx, `INSERT INTO discovery_exposures (delivery_id, buyer_id, listing_id, exposed_at) VALUES ($1, $2, $3, $4)
			ON CONFLICT DO NOTHING`, deliveryID.String(), buyerID, id.String(), at); err != nil {
			return res, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE discovery_deliveries SET status = 'SENDING', claimed_at = $2, listing_ids = $3, updated_at = now() WHERE id = $1`,
		deliveryID.String(), at, pq.Array(uuidStrings(res.Listings))); err != nil {
		return res, err
	}
	res.Claimed = true
	return res, tx.Commit()
}

func (r *DiscoveryRepository) SkipDelivery(ctx context.Context, deliveryID uuid.UUID, reason string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE discovery_deliveries SET status = 'SKIPPED', skip_reason = $2, updated_at = now()
		WHERE id = $1 AND status = 'PLANNED'`, deliveryID.String(), reason)
	return err
}

func (r *DiscoveryRepository) MarkDeliverySent(ctx context.Context, deliveryID uuid.UUID, presentation string, messageID *uuid.UUID, providerMessageID string, at time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE discovery_deliveries SET status = 'SENT', presentation = $2, message_id = $3, provider_message_id = $4,
		sent_at = $5, updated_at = now() WHERE id = $1 AND status = 'SENDING'`,
		deliveryID.String(), presentation, uuidArg(messageID), nullIfEmpty(providerMessageID), at)
	return err
}

func (r *DiscoveryRepository) MarkDeliveryFailed(ctx context.Context, deliveryID uuid.UUID, presentation, errText string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM discovery_exposures WHERE delivery_id = $1`, deliveryID.String()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE discovery_deliveries SET status = 'FAILED', presentation = $2, error = $3, updated_at = now()
		WHERE id = $1 AND status IN ('PLANNED', 'SENDING')`, deliveryID.String(), nullIfEmpty(presentation), truncateText(errText, 1000)); err != nil {
		return err
	}
	return tx.Commit()
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---- interactions -----------------------------------------------------------

func (r *DiscoveryRepository) RecordInteraction(ctx context.Context, in *distribution.Interaction) (bool, error) {
	if in.ID == uuid.Nil {
		in.ID = uuid.New()
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `INSERT INTO discovery_interactions
		(id, inbound_message_id, buyer_id, delivery_id, raw_id, action, listing_ref, listing_id, category, outcome, reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (inbound_message_id) DO NOTHING RETURNING id`,
		in.ID.String(), in.InboundMessageID.String(), in.BuyerID.String(), uuidArg(in.DeliveryID), in.RawID, nullIfEmpty(in.Action),
		nullIfEmpty(in.ListingRef), uuidArg(in.ListingID), nullIfEmpty(in.Category), in.Outcome, nullIfEmpty(in.Reason)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if in.Outcome == distribution.OutcomeApplied && in.DeliveryID != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE discovery_deliveries SET interaction_count = interaction_count + 1,
			first_interaction_at = COALESCE(first_interaction_at, now()), last_interaction_at = now(), last_action = $2, updated_at = now()
			WHERE id = $1`, in.DeliveryID.String(), in.Action); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func (r *DiscoveryRepository) SetInteractionRequest(ctx context.Context, interactionID, buyerRequestID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `UPDATE discovery_interactions SET buyer_request_id = $2 WHERE id = $1`, interactionID.String(), buyerRequestID.String())
	return err
}

func (r *DiscoveryRepository) ListInteractions(ctx context.Context, deliveryID uuid.UUID) ([]*distribution.Interaction, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, inbound_message_id, buyer_id, delivery_id, raw_id, COALESCE(action, ''), COALESCE(listing_ref, ''),
		listing_id, COALESCE(category, ''), outcome, COALESCE(reason, ''), buyer_request_id, created_at
		FROM discovery_interactions WHERE delivery_id = $1 ORDER BY created_at, id`, deliveryID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*distribution.Interaction{}
	for rows.Next() {
		var in distribution.Interaction
		var id, msg, buyer string
		var del, lst, req sql.NullString
		if err := rows.Scan(&id, &msg, &buyer, &del, &in.RawID, &in.Action, &in.ListingRef, &lst, &in.Category, &in.Outcome, &in.Reason, &req, &in.CreatedAt); err != nil {
			return nil, err
		}
		in.ID, _ = uuid.Parse(id)
		in.InboundMessageID, _ = uuid.Parse(msg)
		in.BuyerID, _ = uuid.Parse(buyer)
		in.DeliveryID, in.ListingID, in.BuyerRequestID = optUUID(del), optUUID(lst), optUUID(req)
		out = append(out, &in)
	}
	return out, rows.Err()
}

// ---- attribution & funnel ---------------------------------------------------

func (r *DiscoveryRepository) Attribution(ctx context.Context, deliveryID uuid.UUID) (*distribution.Attribution, error) {
	d, err := r.FindDelivery(ctx, deliveryID)
	if err != nil || d == nil {
		return nil, err
	}
	a := &distribution.Attribution{DeliveryID: d.ID, BatchID: d.BatchID, Arm: d.Arm, Messages: []distribution.AttributedMessage{},
		Requests: []distribution.AttributedRequest{}, Offers: []distribution.AttributedOffer{}, Selections: []distribution.AttributedSelection{},
		Transactions: []distribution.AttributedTransaction{}}
	if err := r.eachRow(ctx, `SELECT id, COALESCE(external_message_id, ''), COALESCE(metadata->>'message_kind', message_type), created_at, delivered_at, read_at, failed_at
		FROM messages WHERE direction = 'OUTBOUND' AND metadata->>'entity_type' = 'discovery_delivery' AND metadata->>'entity_id' = $1
		ORDER BY created_at, id`, []any{d.ID.String()}, func(sc func(...any) error) error {
		var m distribution.AttributedMessage
		var id string
		var del, rd, fl sql.NullTime
		if err := sc(&id, &m.ProviderMessageID, &m.Kind, &m.SentAt, &del, &rd, &fl); err != nil {
			return err
		}
		m.MessageID, _ = uuid.Parse(id)
		m.DeliveredAt, m.ReadAt, m.FailedAt = optTime(del), optTime(rd), optTime(fl)
		a.Messages = append(a.Messages, m)
		return nil
	}); err != nil {
		return nil, err
	}
	if a.Interactions, err = r.ListInteractions(ctx, d.ID); err != nil {
		return nil, err
	}
	var reqIDs []string
	if err := r.eachRow(ctx, `SELECT id, status, origin, listing_id, created_at FROM buyer_requests WHERE discovery_delivery_id = $1 ORDER BY created_at`,
		[]any{d.ID.String()}, func(sc func(...any) error) error {
			var x distribution.AttributedRequest
			var id string
			var lst sql.NullString
			if err := sc(&id, &x.Status, &x.Origin, &lst, &x.CreatedAt); err != nil {
				return err
			}
			x.ID, _ = uuid.Parse(id)
			x.ListingID = optUUID(lst)
			reqIDs = append(reqIDs, id)
			a.Requests = append(a.Requests, x)
			return nil
		}); err != nil {
		return nil, err
	}
	if len(reqIDs) == 0 {
		return a, nil
	}
	ids := pq.Array(reqIDs)
	if err := r.eachRow(ctx, `SELECT id, buyer_request_id, supply_listing_id, vendor_request_id, status, price FROM offers
		WHERE buyer_request_id = ANY($1::uuid[]) ORDER BY created_at`, []any{ids}, func(sc func(...any) error) error {
		var o distribution.AttributedOffer
		var id, br string
		var lst, vr sql.NullString
		var price sql.NullInt64
		if err := sc(&id, &br, &lst, &vr, &o.Status, &price); err != nil {
			return err
		}
		o.ID, _ = uuid.Parse(id)
		o.BuyerRequestID, _ = uuid.Parse(br)
		o.SupplyListingID, o.VendorRequestID = optUUID(lst), optUUID(vr)
		if price.Valid {
			p := price.Int64
			o.Price = &p
		}
		a.Offers = append(a.Offers, o)
		return nil
	}); err != nil {
		return nil, err
	}
	if err := r.eachRow(ctx, `SELECT id, offer_id FROM buyer_selections WHERE buyer_request_id = ANY($1::uuid[]) AND status = 'ACTIVE'`,
		[]any{ids}, func(sc func(...any) error) error {
			var s distribution.AttributedSelection
			var id, off string
			if err := sc(&id, &off); err != nil {
				return err
			}
			s.ID, _ = uuid.Parse(id)
			s.OfferID, _ = uuid.Parse(off)
			a.Selections = append(a.Selections, s)
			return nil
		}); err != nil {
		return nil, err
	}
	if err := r.eachRow(ctx, `SELECT id, offer_id, supply_listing_id, status, payment_status, gross_amount FROM transactions
		WHERE buyer_request_id = ANY($1::uuid[]) ORDER BY created_at`, []any{ids}, func(sc func(...any) error) error {
		var t distribution.AttributedTransaction
		var id, off string
		var lst sql.NullString
		if err := sc(&id, &off, &lst, &t.Status, &t.PaymentStatus, &t.GrossAmount); err != nil {
			return err
		}
		t.ID, _ = uuid.Parse(id)
		t.OfferID, _ = uuid.Parse(off)
		t.SupplyListingID = optUUID(lst)
		a.Transactions = append(a.Transactions, t)
		return nil
	}); err != nil {
		return nil, err
	}
	return a, nil
}

// Funnel implements distribution.Store: counted from the system-of-record
// tables (deliveries, messages receipts, interactions, requests, offers,
// selections, transactions). batchID nil = all batches.
func (r *DiscoveryRepository) Funnel(ctx context.Context, batchID *uuid.UUID) (*distribution.Funnel, error) {
	f := &distribution.Funnel{Exclusions: map[string]int{}, ByArm: map[string]int{}}
	scope := `TRUE`
	var args []any
	if batchID != nil {
		scope = `d.batch_id = $1`
		args = []any{batchID.String()}
	}
	bScope := `TRUE`
	if batchID != nil {
		bScope = `b.id = $1`
	}
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE status = 'OPTED_IN'), count(*) FILTER (WHERE status = 'OPTED_OUT') FROM discovery_preferences`).
		Scan(&f.OptedIn, &f.OptedOut); err != nil {
		return nil, err
	}
	if err := r.eachRow(ctx, `SELECT plan FROM discovery_batches b WHERE `+bScope, args, func(sc func(...any) error) error {
		var raw []byte
		if err := sc(&raw); err != nil {
			return err
		}
		var p distribution.PlanReport
		_ = json.Unmarshal(raw, &p)
		f.Batches++
		f.EligibleAudience += p.AudienceEligible
		f.EligibleListings += p.EligibleListings
		for k, v := range p.ExcludedBuyers {
			f.Exclusions["buyer:"+k] += v
		}
		for k, v := range p.ListingExclusions {
			f.Exclusions["listing:"+k] += v
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := r.db.QueryRowContext(ctx, `SELECT count(*), count(*) FILTER (WHERE status IN ('SENDING', 'SENT', 'FAILED')),
		count(*) FILTER (WHERE status = 'SENT'), count(*) FILTER (WHERE status = 'FAILED'), count(*) FILTER (WHERE status = 'SKIPPED'),
		count(*) FILTER (WHERE status = 'SENT' AND EXISTS (SELECT 1 FROM messages m WHERE m.direction = 'OUTBOUND'
			AND m.metadata->>'entity_type' = 'discovery_delivery' AND m.metadata->>'entity_id' = d.id::text AND m.delivered_at IS NOT NULL)),
		count(*) FILTER (WHERE status = 'SENT' AND EXISTS (SELECT 1 FROM messages m WHERE m.direction = 'OUTBOUND'
			AND m.metadata->>'entity_type' = 'discovery_delivery' AND m.metadata->>'entity_id' = d.id::text AND m.read_at IS NOT NULL)),
		count(*) FILTER (WHERE interaction_count > 0)
		FROM discovery_deliveries d WHERE `+scope, args...).Scan(&f.DeliveriesPlanned, &f.DeliveriesAttempt, &f.DeliveriesSent,
		&f.DeliveriesFailed, &f.DeliveriesSkipped, &f.Delivered, &f.Read, &f.Interacted); err != nil {
		return nil, err
	}
	for k, v := range map[string]string{"skipped:": `status = 'SKIPPED'`, "failed:": `status = 'FAILED'`} {
		if err := r.eachRow(ctx, `SELECT COALESCE(skip_reason, CASE WHEN status = 'FAILED' THEN 'send_failed' END, 'unknown'), count(*)
			FROM discovery_deliveries d WHERE `+scope+` AND `+v+` GROUP BY 1`, args, func(sc func(...any) error) error {
			var reason string
			var n int
			if err := sc(&reason, &n); err != nil {
				return err
			}
			f.Exclusions[k+reason] += n
			return nil
		}); err != nil {
			return nil, err
		}
	}
	if err := r.eachRow(ctx, `SELECT arm, count(*) FROM discovery_deliveries d WHERE `+scope+` AND status = 'SENT' GROUP BY arm`, args,
		func(sc func(...any) error) error {
			var arm string
			var n int
			if err := sc(&arm, &n); err != nil {
				return err
			}
			f.ByArm[arm] = n
			return nil
		}); err != nil {
		return nil, err
	}
	if err := r.db.QueryRowContext(ctx, `SELECT
		count(*) FILTER (WHERE i.outcome = 'APPLIED' AND i.action IN ('listing', 'video')),
		count(DISTINCT (i.delivery_id, i.listing_id)) FILTER (WHERE i.outcome = 'APPLIED' AND i.action = 'ask'),
		count(*) FILTER (WHERE i.outcome = 'APPLIED' AND i.action = 'stop'),
		count(*) FILTER (WHERE i.outcome = 'UNRESOLVED')
		FROM discovery_interactions i LEFT JOIN discovery_deliveries d ON d.id = i.delivery_id
		WHERE `+strings.Replace(scope, "d.batch_id", "d.batch_id", 1), args...).Scan(&f.ListingViewed, &f.Asked, &f.StopsFromDiscovery, &f.Unresolved); err != nil {
		return nil, err
	}
	if err := r.db.QueryRowContext(ctx, `WITH reqs AS (
			SELECT br.id, br.status FROM buyer_requests br JOIN discovery_deliveries d ON d.id = br.discovery_delivery_id
			WHERE br.origin = 'discovery' AND `+scope+`)
		SELECT (SELECT count(*) FROM reqs), (SELECT count(*) FROM reqs WHERE status = 'CONFIRMED'),
			(SELECT count(*) FROM offers o WHERE o.buyer_request_id IN (SELECT id FROM reqs)),
			(SELECT count(*) FROM buyer_selections s WHERE s.status = 'ACTIVE' AND s.buyer_request_id IN (SELECT id FROM reqs)),
			(SELECT count(*) FROM transactions t WHERE t.buyer_request_id IN (SELECT id FROM reqs))`, args...).
		Scan(&f.RequestsCreated, &f.RequestsConfirmed, &f.Offers, &f.Selections, &f.Transactions); err != nil {
		return nil, err
	}
	return f, nil
}
