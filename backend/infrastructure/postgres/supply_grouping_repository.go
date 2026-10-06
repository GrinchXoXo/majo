package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/supply"
)

// SupplyGroupingRepository is the PostgreSQL implementation of
// supply.GroupingRepository (Phase 4A.2).
type SupplyGroupingRepository struct {
	db *sql.DB
}

func NewSupplyGroupingRepository(db *sql.DB) *SupplyGroupingRepository {
	return &SupplyGroupingRepository{db: db}
}

func (r *SupplyGroupingRepository) RecordCandidate(ctx context.Context, c *supply.Candidate) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO supply_candidate_messages (message_id, vendor_id, conversation_id, kind, message_at, has_product, has_price, reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (message_id) DO NOTHING
	`, c.MessageID.String(), c.VendorID.String(), c.ConversationID.String(), string(c.Kind), c.MessageAt, c.HasProduct, c.HasPrice, c.Reason)
	return err
}

// candidateSelect yields everything grouping needs for a candidate: its
// text/caption (from the immutable message), signal membership, stored
// asset, and whether its media download is final.
const candidateSelect = `
	SELECT c.message_id, c.vendor_id, c.conversation_id, c.kind, c.message_at, m.created_at,
	       COALESCE(CASE WHEN c.kind = 'text' THEN m.content->>'text' ELSE m.content->'media'->>'caption' END, ''),
	       c.has_product, c.has_price, c.reason, s.supply_signal_id, m.media_asset_id,
	       (c.kind = 'text' OR m.media_asset_id IS NOT NULL OR EXISTS (
	           SELECT 1 FROM jobs j WHERE j.job_type = 'MEDIA_DOWNLOAD' AND j.idempotency_key = m.id::text
	              AND j.status IN ('SUCCEEDED', 'FAILED')))
	FROM supply_candidate_messages c
	JOIN messages m ON m.id = c.message_id
	LEFT JOIN supply_signal_sources s ON s.message_id = c.message_id`

func scanCandidate(row interface{ Scan(...any) error }) (*supply.Candidate, error) {
	var (
		c                   supply.Candidate
		mid, vid, cid, kind string
		signalID, assetID   sql.NullString
	)
	if err := row.Scan(&mid, &vid, &cid, &kind, &c.MessageAt, &c.ReceivedAt, &c.Text, &c.HasProduct, &c.HasPrice,
		&c.Reason, &signalID, &assetID, &c.MediaSettled); err != nil {
		return nil, err
	}
	c.MessageID, _ = uuid.Parse(mid)
	c.VendorID, _ = uuid.Parse(vid)
	c.ConversationID, _ = uuid.Parse(cid)
	c.Kind = supply.CandidateKind(kind)
	if signalID.Valid {
		id := uuid.MustParse(signalID.String)
		c.SignalID = &id
	}
	if assetID.Valid {
		id := uuid.MustParse(assetID.String)
		c.MediaAssetID = &id
	}
	return &c, nil
}

func (r *SupplyGroupingRepository) Candidate(ctx context.Context, messageID uuid.UUID) (*supply.Candidate, error) {
	c, err := scanCandidate(r.db.QueryRowContext(ctx, candidateSelect+` WHERE c.message_id = $1`, messageID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (r *SupplyGroupingRepository) Timeline(ctx context.Context, vendorID, conversationID uuid.UUID, from, to time.Time) ([]supply.Candidate, []supply.Break, error) {
	rows, err := r.db.QueryContext(ctx, candidateSelect+`
		WHERE c.vendor_id = $1 AND c.conversation_id = $2 AND c.message_at BETWEEN $3 AND $4
		ORDER BY c.message_at, m.created_at, c.message_id`, vendorID.String(), conversationID.String(), from, to)
	if err != nil {
		return nil, nil, err
	}
	var cands []supply.Candidate
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		cands = append(cands, *c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	// Breaks: other inbound messages in the same conversation (replies to
	// VendorRequests, ambiguous media, voice notes ...). The created_at bound
	// keeps the scan on the conversation's recent rows.
	brows, err := r.db.QueryContext(ctx, `
		SELECT COALESCE((m.metadata->>'provider_timestamp')::timestamptz, m.created_at), m.created_at
		FROM messages m
		WHERE m.conversation_id = $1 AND m.direction = 'INBOUND'
		  AND m.created_at BETWEEN $2::timestamptz - interval '1 day' AND $3::timestamptz + interval '1 day'
		  AND COALESCE((m.metadata->>'provider_timestamp')::timestamptz, m.created_at) BETWEEN $2 AND $3
		  AND NOT EXISTS (SELECT 1 FROM supply_candidate_messages c WHERE c.message_id = m.id)
		ORDER BY 1, 2`, conversationID.String(), from, to)
	if err != nil {
		return nil, nil, err
	}
	defer brows.Close()
	var breaks []supply.Break
	for brows.Next() {
		var b supply.Break
		if err := brows.Scan(&b.At, &b.ReceivedAt); err != nil {
			return nil, nil, err
		}
		breaks = append(breaks, b)
	}
	return cands, breaks, brows.Err()
}

func isSourceConflict(err error) bool {
	var pe *pq.Error
	return errors.As(err, &pe) && pe.Code == "23505" && pe.Constraint == "supply_signal_sources_message_id_key"
}

func insertSources(ctx context.Context, tx *sql.Tx, signalID uuid.UUID, sources []supply.Source) error {
	for _, s := range sources {
		var asset any
		if s.MediaAssetID != nil {
			asset = s.MediaAssetID.String()
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO supply_signal_sources (supply_signal_id, position, message_id, media_asset_id)
			VALUES ($1, $2, $3, $4)`, signalID.String(), s.Position, s.MessageID.String(), asset); err != nil {
			if isSourceConflict(err) {
				return supply.ErrSourcesTaken
			}
			return err
		}
	}
	return nil
}

func (r *SupplyGroupingRepository) CreateSignalWithSources(ctx context.Context, s *supply.Signal, sources []supply.Source, created *event.Event) (*supply.Signal, error) {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	if s.Currency == "" {
		s.Currency = "NGN"
	}
	if s.AvailabilityStatus == "" {
		s.AvailabilityStatus = supply.AvailabilityAvailable
	}
	if s.ExtractedData == nil {
		s.ExtractedData = map[string]any{}
	}
	if s.ExpiresAt.IsZero() {
		s.ExpiresAt = time.Now().UTC().AddDate(0, 0, 7) // same 7-day default as SupplyRepository.Create
	}
	extracted, _ := json.Marshal(s.ExtractedData)

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	out, err := scanSupplyRow(tx.QueryRowContext(ctx, `
		INSERT INTO supply_signals (
			id, vendor_id, category, product, normalized_product, variant, condition,
			price, currency, quantity, location, availability_status,
			raw_message, extracted_data, confidence, expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		RETURNING `+supplyColumns,
		s.ID.String(), s.VendorID.String(), s.Category, s.Product, normalizeProductName(s.Product), s.Variant, s.Condition,
		s.Price, s.Currency, s.Quantity, s.Location, s.AvailabilityStatus,
		s.RawMessage, extracted, s.Confidence, s.ExpiresAt))
	if err != nil {
		return nil, err
	}
	if err := insertSources(ctx, tx, out.ID, sources); err != nil {
		return nil, err
	}
	if created != nil {
		id := out.ID
		created.EntityID = &id
		if created.Payload == nil {
			created.Payload = map[string]any{}
		}
		created.Payload["supply_signal_id"] = out.ID.String()
		if err := insertEventTx(ctx, tx, created); err != nil {
			return nil, fmt.Errorf("writing %s: %w", created.EventType, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *SupplyGroupingRepository) AppendSources(ctx context.Context, signalID uuid.UUID, sources []supply.Source, updated *event.Event) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serialise evidence changes per signal and continue its ordering.
	var next int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM supply_signals WHERE id = $1 FOR UPDATE`, signalID.String()).Scan(&next); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position) + 1, 0) FROM supply_signal_sources WHERE supply_signal_id = $1`,
		signalID.String()).Scan(&next); err != nil {
		return err
	}
	for i := range sources {
		sources[i].Position = next + i
	}
	if err := insertSources(ctx, tx, signalID, sources); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE supply_signals SET updated_at = now() WHERE id = $1`, signalID.String()); err != nil {
		return err
	}
	if updated != nil {
		if err := insertEventTx(ctx, tx, updated); err != nil {
			return fmt.Errorf("writing %s: %w", updated.EventType, err)
		}
	}
	return tx.Commit()
}

func (r *SupplyGroupingRepository) Evidence(ctx context.Context, signalID uuid.UUID) (*supply.Evidence, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT message_id, media_asset_id FROM supply_signal_sources
		WHERE supply_signal_id = $1 ORDER BY position`, signalID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ev := &supply.Evidence{}
	seen := map[uuid.UUID]bool{}
	for rows.Next() {
		var mid string
		var aid sql.NullString
		if err := rows.Scan(&mid, &aid); err != nil {
			return nil, err
		}
		ev.SourceMessageIDs = append(ev.SourceMessageIDs, uuid.MustParse(mid))
		if aid.Valid {
			id := uuid.MustParse(aid.String)
			if !seen[id] {
				seen[id] = true
				ev.MediaAssetIDs = append(ev.MediaAssetIDs, id)
			}
		}
	}
	return ev, rows.Err()
}
