package postgres

// Pilot batch item 5: deals (migration 0018).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/deal"
)

// DealRepository implements deal.Repository.
type DealRepository struct {
	db *sql.DB
}

func NewDealRepository(db *sql.DB) *DealRepository { return &DealRepository{db: db} }

const dealColumns = `d.id, d.transaction_id, d.buyer_request_id, d.buyer_id, d.vendor_id, d.conversation_id, d.vendor_request_id,
	d.item, d.reference, d.amount, d.status, d.ask_at, d.ask_attempts, d.buyer_asked_at, d.vendor_asked_at,
	d.buyer_unreachable, d.vendor_unreachable, d.buyer_answer, d.buyer_answered_at, d.vendor_answer, d.vendor_answered_at,
	d.confirmed_at, d.deal_number, d.free, d.buyer_rating, d.buyer_rated_at, d.closed_at, d.created_at,
	COALESCE((SELECT business_name FROM vendors v WHERE v.id = d.vendor_id), '')`

type rowScanner interface{ Scan(...any) error }

func scanDeal(row rowScanner) (*deal.Deal, error) {
	var d deal.Deal
	var id, tx, br, buyer, vendor, conv string
	var vr sql.NullString
	var amount, number, rating sql.NullInt64
	var status string
	var buyerAns, vendorAns sql.NullString
	var bAsked, vAsked, bAns, vAns, confirmed, rated, closed sql.NullTime
	if err := row.Scan(&id, &tx, &br, &buyer, &vendor, &conv, &vr, &d.Item, &d.Reference, &amount, &status, &d.AskAt, &d.AskAttempts,
		&bAsked, &vAsked, &d.BuyerUnreachable, &d.VendorUnreach, &buyerAns, &bAns, &vendorAns, &vAns, &confirmed, &number, &d.Free,
		&rating, &rated, &closed, &d.CreatedAt, &d.VendorName); err != nil {
		return nil, err
	}
	d.ID, _ = uuid.Parse(id)
	d.TransactionID, _ = uuid.Parse(tx)
	d.BuyerRequestID, _ = uuid.Parse(br)
	d.BuyerID, _ = uuid.Parse(buyer)
	d.VendorID, _ = uuid.Parse(vendor)
	d.ConversationID, _ = uuid.Parse(conv)
	if vr.Valid {
		u, _ := uuid.Parse(vr.String)
		d.VendorRequestID = &u
	}
	if amount.Valid {
		d.Amount = &amount.Int64
	}
	d.Status = deal.Status(status)
	d.BuyerAskedAt, d.VendorAskedAt = nt(bAsked), nt(vAsked)
	d.BuyerAnsweredAt, d.VendorAnsweredAt = nt(bAns), nt(vAns)
	d.ConfirmedAt, d.BuyerRatedAt, d.ClosedAt = nt(confirmed), nt(rated), nt(closed)
	if buyerAns.Valid {
		d.BuyerAnswer = &buyerAns.String
	}
	if vendorAns.Valid {
		d.VendorAnswer = &vendorAns.String
	}
	if number.Valid {
		n := int(number.Int64)
		d.DealNumber = &n
	}
	if rating.Valid {
		r := int(rating.Int64)
		d.BuyerRating = &r
	}
	d.AskAt, d.CreatedAt = d.AskAt.UTC(), d.CreatedAt.UTC()
	return &d, nil
}

func nt(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	return &u
}

func (r *DealRepository) getQ(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id uuid.UUID, lock bool) (*deal.Deal, error) {
	sfx := ""
	if lock {
		sfx = " FOR UPDATE OF d"
	}
	d, err := scanDeal(q.QueryRowContext(ctx, `SELECT `+dealColumns+` FROM deals d WHERE d.id = $1`+sfx, id.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return d, err
}

// Get implements deal.Repository.
func (r *DealRepository) Get(ctx context.Context, id uuid.UUID) (*deal.Deal, error) {
	return r.getQ(ctx, r.db, id, false)
}

// Create implements deal.Repository (idempotent per transaction).
func (r *DealRepository) Create(ctx context.Context, d *deal.Deal) (*deal.Deal, bool, error) {
	var vr any
	if d.VendorRequestID != nil {
		vr = d.VendorRequestID.String()
	}
	var id string
	err := r.db.QueryRowContext(ctx, `INSERT INTO deals (transaction_id, buyer_request_id, buyer_id, vendor_id, conversation_id, vendor_request_id,
			item, reference, amount, status, ask_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'PENDING', $10)
		ON CONFLICT (transaction_id) DO NOTHING RETURNING id`,
		d.TransactionID.String(), d.BuyerRequestID.String(), d.BuyerID.String(), d.VendorID.String(), d.ConversationID.String(), vr,
		d.Item, d.Reference, d.Amount, d.AskAt).Scan(&id)
	created := true
	if errors.Is(err, sql.ErrNoRows) {
		created = false
		err = r.db.QueryRowContext(ctx, `SELECT id FROM deals WHERE transaction_id = $1`, d.TransactionID.String()).Scan(&id)
	}
	if err != nil {
		return nil, false, err
	}
	u, _ := uuid.Parse(id)
	saved, err := r.Get(ctx, u)
	return saved, created, err
}

func partyCols(party string) (asked, answer, answeredAt, unreachable string, err error) {
	switch party {
	case deal.Buyer:
		return "buyer_asked_at", "buyer_answer", "buyer_answered_at", "buyer_unreachable", nil
	case deal.Vendor:
		return "vendor_asked_at", "vendor_answer", "vendor_answered_at", "vendor_unreachable", nil
	}
	return "", "", "", "", fmt.Errorf("unknown party %q", party)
}

// MarkAsked implements deal.Repository.
func (r *DealRepository) MarkAsked(ctx context.Context, id uuid.UUID, party string, at time.Time) error {
	asked, _, _, _, err := partyCols(party)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `UPDATE deals SET `+asked+` = $2, status = CASE WHEN status = 'PENDING' THEN 'ASKED' ELSE status END,
		updated_at = now() WHERE id = $1`, id.String(), at)
	return err
}

// MarkUnreachable implements deal.Repository.
func (r *DealRepository) MarkUnreachable(ctx context.Context, id uuid.UUID, party string) error {
	_, _, _, unreachable, err := partyCols(party)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `UPDATE deals SET `+unreachable+` = true, updated_at = now() WHERE id = $1`, id.String())
	return err
}

// BumpAskAttempts implements deal.Repository.
func (r *DealRepository) BumpAskAttempts(ctx context.Context, id uuid.UUID) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `UPDATE deals SET ask_attempts = ask_attempts + 1, updated_at = now() WHERE id = $1 RETURNING ask_attempts`,
		id.String()).Scan(&n)
	return n, err
}

// RecordAnswer implements deal.Repository.
func (r *DealRepository) RecordAnswer(ctx context.Context, id uuid.UUID, party, answer string, at time.Time, freeLimit int) (*deal.Deal, deal.Status, bool, error) {
	_, answerCol, answeredCol, _, err := partyCols(party)
	if err != nil {
		return nil, "", false, err
	}
	if answer != deal.Yes && answer != deal.No && answer != deal.NotYet {
		return nil, "", false, fmt.Errorf("unknown answer %q", answer)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", false, err
	}
	defer tx.Rollback()
	cur, err := r.getQ(ctx, tx, id, true)
	if err != nil || cur == nil {
		return cur, "", false, err
	}
	prev := cur.Status
	if prev.Terminal() || cur.ClosedAt != nil {
		return cur, prev, false, tx.Commit()
	}
	if party == deal.Buyer {
		cur.BuyerAnswer = &answer
	} else {
		cur.VendorAnswer = &answer
	}
	next := deal.Resolve(cur.BuyerAnswer, cur.VendorAnswer, false)
	if next == deal.StatusConfirmed {
		// Status and number change together (the table checks it): the next
		// number, no gaps and no duplicates under concurrency (one advisory
		// lock serialises numbering only).
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('majo_deal_number'))`); err != nil {
			return nil, "", false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE deals SET `+answerCol+` = $2, `+answeredCol+` = $3, status = 'CONFIRMED',
				deal_number = n.next, free = n.next <= $4, confirmed_at = $3, updated_at = now()
			FROM (SELECT COALESCE(max(deal_number), 0) + 1 AS next FROM deals) n
			WHERE id = $1`, id.String(), answer, at, freeLimit); err != nil {
			return nil, "", false, err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE deals SET `+answerCol+` = $2, `+answeredCol+` = $3, status = $4, updated_at = now() WHERE id = $1`,
		id.String(), answer, at, string(next)); err != nil {
		return nil, "", false, err
	}
	d, err := r.getQ(ctx, tx, id, false)
	if err != nil {
		return nil, "", false, err
	}
	return d, prev, true, tx.Commit()
}

// Close implements deal.Repository: the final status from the answers given.
func (r *DealRepository) Close(ctx context.Context, id uuid.UUID, at time.Time) (*deal.Deal, deal.Status, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	cur, err := r.getQ(ctx, tx, id, true)
	if err != nil || cur == nil {
		return cur, "", err
	}
	prev := cur.Status
	if cur.ClosedAt != nil {
		return cur, prev, tx.Commit()
	}
	final := prev
	if !prev.Terminal() {
		final = deal.Resolve(cur.BuyerAnswer, cur.VendorAnswer, true)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE deals SET status = $2, closed_at = $3, updated_at = now() WHERE id = $1`,
		id.String(), string(final), at); err != nil {
		return nil, "", err
	}
	d, err := r.getQ(ctx, tx, id, false)
	if err != nil {
		return nil, "", err
	}
	return d, prev, tx.Commit()
}

// SetRating implements deal.Repository (the first rating counts; only a
// buyer who said YES is asked).
func (r *DealRepository) SetRating(ctx context.Context, id uuid.UUID, score int, at time.Time) (*deal.Deal, bool, error) {
	res, err := r.db.ExecContext(ctx, `UPDATE deals SET buyer_rating = $2, buyer_rated_at = $3, updated_at = now()
		WHERE id = $1 AND buyer_rating IS NULL`, id.String(), score, at)
	if err != nil {
		return nil, false, err
	}
	k, _ := res.RowsAffected()
	d, err := r.Get(ctx, id)
	return d, k == 1, err
}

// Summary implements deal.Repository.
func (r *DealRepository) Summary(ctx context.Context, freeLimit int) (*deal.Summary, error) {
	s := &deal.Summary{ByStatus: map[string]int{}, FreeLimit: freeLimit}
	rows, err := r.db.QueryContext(ctx, `SELECT status, count(*) FROM deals GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		s.ByStatus[st] = n
		s.Total += n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var avg sql.NullFloat64
	if err := r.db.QueryRowContext(ctx, `SELECT
			count(*) FILTER (WHERE status = 'CONFIRMED'),
			count(*) FILTER (WHERE free),
			count(buyer_rating),
			avg(buyer_rating)::float8,
			count(*) FILTER (WHERE buyer_rating <= 2),
			COALESCE(sum(amount) FILTER (WHERE status = 'CONFIRMED'), 0)
		FROM deals`).Scan(&s.Confirmed, &s.FreeUsed, &s.Ratings, &avg, &s.LowRatings, &s.ConfirmedValue); err != nil {
		return nil, err
	}
	if avg.Valid {
		v := float64(int(avg.Float64*10+0.5)) / 10
		s.AverageRating = &v
	}
	s.FreeRemaining = freeLimit - s.FreeUsed
	if s.FreeRemaining < 0 {
		s.FreeRemaining = 0
	}
	return s, nil
}

// List implements deal.Repository (newest first; status "" = all).
func (r *DealRepository) List(ctx context.Context, status string, limit, offset int) ([]*deal.Deal, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM deals WHERE ($1 = '' OR status = $1)`, status).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+dealColumns+` FROM deals d WHERE ($1 = '' OR d.status = $1)
		ORDER BY d.created_at DESC, d.id LIMIT $2 OFFSET $3`, status, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*deal.Deal
	for rows.Next() {
		d, err := scanDeal(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, d)
	}
	return out, total, rows.Err()
}

// PublicReference implements deal.References.
func (r *DealRepository) PublicReference(ctx context.Context, vendorRequestID uuid.UUID) (string, error) {
	var ref string
	err := r.db.QueryRowContext(ctx, `SELECT public_reference FROM vendor_requests WHERE id = $1`, vendorRequestID.String()).Scan(&ref)
	return ref, err
}
