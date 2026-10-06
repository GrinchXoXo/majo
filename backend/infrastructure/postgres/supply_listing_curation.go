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

	"github.com/middleman/backend/internal/audit"
	"github.com/middleman/backend/internal/supply"
)

// Curate implements supply.ListingRepository.Curate (Phase 4A Step 4): one
// transaction, the listing row locked FOR UPDATE, so concurrent publishes /
// rejects / edits of the same listing are serialised and each decision sees
// the latest row. The transition, the audit entry and the event commit
// together or not at all.
func (r *SupplyListingRepository) Curate(ctx context.Context, id uuid.UUID, decide supply.CurationDecision) (*supply.Listing, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	cur, err := scanListingRow(tx.QueryRowContext(ctx,
		`SELECT `+listingColumns+` FROM supply_listings WHERE id = $1 FOR UPDATE`, id.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, supply.ErrListingNotFound
	}
	if err != nil {
		return nil, err
	}

	ch, err := decide(cur)
	if err != nil {
		return nil, err
	}
	if ch == nil {
		return cur, tx.Commit()
	}
	if ch.Publish && ch.Reject {
		return nil, fmt.Errorf("curation change cannot both publish and reject")
	}

	set := []string{}
	args := []any{}
	add := func(col string, val any) {
		args = append(args, val)
		set = append(set, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	f := ch.Fields
	if f.Category != nil {
		add("category", *f.Category)
	}
	if f.Product != nil {
		add("product", *f.Product)
		add("normalized_product", supply.NormalizeSearch(*f.Product))
	}
	if f.Variant != nil {
		add("variant", *f.Variant)
	}
	if f.Condition != nil {
		add("condition", *f.Condition)
	}
	if f.Price != nil {
		add("price", *f.Price)
	}
	if f.Currency != nil {
		add("currency", *f.Currency)
	}
	if f.Quantity != nil {
		add("quantity", *f.Quantity)
	}
	if f.Location != nil {
		add("location", *f.Location)
	}
	if f.Description != nil {
		add("description", *f.Description)
	}
	if f.MediaReferences != nil {
		b, err := json.Marshal(*f.MediaReferences)
		if err != nil {
			return nil, err
		}
		add("media_references", b)
	}
	if ch.Publish {
		// published_at is the FIRST publication; never reset.
		set = append(set, "status = 'ACTIVE'", "published_at = COALESCE(published_at, now())")
	}
	if ch.Reject {
		set = append(set, "status = 'ARCHIVED'", "rejected_at = now()")
		var reason any
		if ch.RejectReason != nil {
			reason = *ch.RejectReason
		}
		add("rejection_reason", reason)
	}
	set = append(set, "updated_at = now()")
	args = append(args, id.String())
	updated, err := scanListingRow(tx.QueryRowContext(ctx, fmt.Sprintf(
		"UPDATE supply_listings SET %s WHERE id = $%d RETURNING "+listingColumns, strings.Join(set, ", "), len(args)), args...))
	if err != nil {
		return nil, err
	}

	if ch.Audit != nil {
		if err := insertAuditTx(ctx, tx, ch.Audit); err != nil {
			return nil, fmt.Errorf("writing audit %s: %w", ch.Audit.Action, err)
		}
	}
	if ch.Event != nil {
		if err := insertEventTx(ctx, tx, ch.Event); err != nil {
			return nil, fmt.Errorf("writing %s: %w", ch.Event.EventType, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

// insertAuditTx is AuditWriter.Write inside a caller's transaction.
func insertAuditTx(ctx context.Context, tx *sql.Tx, e *audit.Entry) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if e.Metadata == nil {
		e.Metadata = map[string]any{}
	}
	var prev, next any // untyped nil -> SQL NULL (see AuditWriter.Write)
	if e.PreviousValue != nil {
		b, err := json.Marshal(e.PreviousValue)
		if err != nil {
			return err
		}
		prev = b
	}
	if e.NewValue != nil {
		b, err := json.Marshal(e.NewValue)
		if err != nil {
			return err
		}
		next = b
	}
	meta, err := json.Marshal(e.Metadata)
	if err != nil {
		return err
	}
	var entityID, actorID any
	if e.EntityID != nil {
		entityID = e.EntityID.String()
	}
	if e.ActorID != nil {
		actorID = e.ActorID.String()
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO audit_logs (id, actor_type, actor_id, action, entity_type, entity_id, previous_value, new_value, metadata, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, e.ID.String(), e.ActorType, actorID, e.Action, e.EntityType, entityID, prev, next, meta, e.CreatedAt)
	return err
}

// MediaAssetStates implements supply.SignalMediaReader.MediaAssetStates.
func (r *SupplyListingRepository) MediaAssetStates(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]supply.MediaAssetState, error) {
	out := map[uuid.UUID]supply.MediaAssetState{}
	if len(ids) == 0 {
		return out, nil
	}
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = id.String()
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, owner_vendor_id, kind, mime_type, size_bytes, status, storage_key
		FROM media_assets WHERE id = ANY($1::uuid[])`, pq.Array(strs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, owner string
		var st supply.MediaAssetState
		if err := rows.Scan(&id, &owner, &st.Kind, &st.MimeType, &st.SizeBytes, &st.Status, &st.StorageKey); err != nil {
			return nil, err
		}
		st.OwnerVendorID = uuid.MustParse(owner)
		out[uuid.MustParse(id)] = st
	}
	return out, rows.Err()
}

// CurationView implements supply.CurationReader.
func (r *SupplyListingRepository) CurationView(ctx context.Context, listingID uuid.UUID) (*supply.CurationView, error) {
	l, err := r.FindByID(ctx, listingID)
	if err != nil || l == nil {
		return nil, err
	}
	v := &supply.CurationView{Listing: l, Sources: []supply.CurationSource{}, Media: []supply.CurationMedia{}}

	var vend supply.CurationVendor
	var vid string
	err = r.db.QueryRowContext(ctx, `SELECT id, business_name, status, COALESCE(primary_location, '') FROM vendors WHERE id = $1`,
		l.VendorID.String()).Scan(&vid, &vend.BusinessName, &vend.Status, &vend.PrimaryLocation)
	switch {
	case err == nil:
		vend.ID = uuid.MustParse(vid)
		v.Vendor = &vend
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}

	if l.SupplySignalID != nil {
		var s supply.CurationSignal
		var sid string
		var cat, prod, variant, cond, loc sql.NullString
		var price sql.NullInt64
		var qty sql.NullInt32
		err = r.db.QueryRowContext(ctx, `
			SELECT id, category, product, variant, condition, price, currency, quantity, location,
			       availability_status, raw_message, confidence, created_at, expires_at
			FROM supply_signals WHERE id = $1`, l.SupplySignalID.String()).Scan(
			&sid, &cat, &prod, &variant, &cond, &price, &s.Currency, &qty, &loc,
			&s.AvailabilityStatus, &s.RawMessage, &s.Confidence, &s.CreatedAt, &s.ExpiresAt)
		switch {
		case err == nil:
			s.ID = uuid.MustParse(sid)
			s.Category, s.Product, s.Variant, s.Condition, s.Location = nullStr(cat), nullStr(prod), nullStr(variant), nullStr(cond), nullStr(loc)
			if price.Valid {
				s.Price = &price.Int64
			}
			if qty.Valid {
				q := int(qty.Int32)
				s.Quantity = &q
			}
			v.Signal = &s
		case !errors.Is(err, sql.ErrNoRows):
			return nil, err
		}

		rows, err := r.db.QueryContext(ctx, `
			SELECT ss.position, m.id, m.external_message_id, m.message_type,
			       COALESCE(m.content->>'text', m.content->'media'->>'caption'), ss.media_asset_id, m.created_at
			FROM supply_signal_sources ss JOIN messages m ON m.id = ss.message_id
			WHERE ss.supply_signal_id = $1 ORDER BY ss.position`, l.SupplySignalID.String())
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var src supply.CurationSource
			var mid string
			var ext, text, asset sql.NullString
			if err := rows.Scan(&src.Position, &mid, &ext, &src.MessageType, &text, &asset, &src.ReceivedAt); err != nil {
				return nil, err
			}
			src.MessageID = uuid.MustParse(mid)
			src.ExternalMessageID, src.Text = nullStr(ext), nullStr(text)
			if asset.Valid {
				a := uuid.MustParse(asset.String)
				src.MediaAssetID = &a
			}
			v.Sources = append(v.Sources, src)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	ids := []uuid.UUID{}
	for _, m := range l.MediaReferences {
		if id, err := uuid.Parse(m.AssetID); err == nil {
			ids = append(ids, id)
		}
	}
	states, err := r.MediaAssetStates(ctx, ids)
	if err != nil {
		return nil, err
	}
	primary := supply.PrimaryMedia(l.MediaReferences)
	for i, m := range l.MediaReferences {
		cm := supply.CurationMedia{Order: i, AssetID: m.AssetID, Type: m.Type, Role: m.Role,
			Primary: primary != nil && primary.AssetID == m.AssetID}
		if id, err := uuid.Parse(m.AssetID); err == nil {
			if st, ok := states[id]; ok && st.OwnerVendorID == l.VendorID {
				cm.Exists, cm.Status, cm.MimeType, cm.SizeBytes = true, st.Status, st.MimeType, st.SizeBytes
			}
		}
		if !cm.Exists {
			cm.Status = "MISSING"
		}
		v.Media = append(v.Media, cm)
	}
	return v, nil
}

func nullStr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

// MatchingEvidence implements supply.MatchingEvidenceReader. Listings must
// be ACTIVE and unexpired; their vendor's status is checked by matching.
func (r *SupplyListingRepository) MatchingEvidence(ctx context.Context, vendorIDs []uuid.UUID) (*supply.MatchingEvidence, error) {
	out := &supply.MatchingEvidence{ListingsByVendor: map[uuid.UUID][]supply.MatchableListing{}, SupersededSignals: map[uuid.UUID]bool{}}
	if len(vendorIDs) == 0 {
		return out, nil
	}
	ids := make([]string, len(vendorIDs))
	for i, id := range vendorIDs {
		ids[i] = id.String()
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+listingColumns+` FROM supply_listings
		WHERE vendor_id = ANY($1::uuid[]) AND status = 'ACTIVE' AND expires_at > now()
		ORDER BY vendor_id, published_at, id`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var signalIDs []string
	for rows.Next() {
		l, err := scanListingRow(rows)
		if err != nil {
			return nil, err
		}
		out.ListingsByVendor[l.VendorID] = append(out.ListingsByVendor[l.VendorID], supply.MatchableListing{Listing: l})
		if l.SupplySignalID != nil {
			signalIDs = append(signalIDs, l.SupplySignalID.String())
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(signalIDs) > 0 {
		sources := map[uuid.UUID]string{}
		prow, err := r.db.QueryContext(ctx, `SELECT id, COALESCE(product, '') FROM supply_signals WHERE id = ANY($1::uuid[])`, pq.Array(signalIDs))
		if err != nil {
			return nil, err
		}
		for prow.Next() {
			var id, product string
			if err := prow.Scan(&id, &product); err != nil {
				prow.Close()
				return nil, err
			}
			sources[uuid.MustParse(id)] = supply.NormalizeSearch(product)
		}
		prow.Close()
		if err := prow.Err(); err != nil {
			return nil, err
		}
		for vid, ls := range out.ListingsByVendor {
			for i := range ls {
				if sid := ls[i].Listing.SupplySignalID; sid != nil {
					ls[i].SourceProduct = sources[*sid]
				}
			}
			out.ListingsByVendor[vid] = ls
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	srows, err := r.db.QueryContext(ctx, `SELECT supply_signal_id FROM supply_listings
		WHERE vendor_id = ANY($1::uuid[]) AND supply_signal_id IS NOT NULL
		  AND (status = 'ACTIVE' OR rejected_at IS NOT NULL)`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	for srows.Next() {
		var sid string
		if err := srows.Scan(&sid); err != nil {
			return nil, err
		}
		out.SupersededSignals[uuid.MustParse(sid)] = true
	}
	return out, srows.Err()
}
