package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/vendor"
)

type VendorRepository struct {
	db *sql.DB
}

func NewVendorRepository(db *sql.DB) *VendorRepository {
	return &VendorRepository{db: db}
}

func (r *VendorRepository) Create(ctx context.Context, v *vendor.Vendor) (*vendor.Vendor, error) {
	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	if v.Status == "" {
		v.Status = vendor.StatusPending
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO vendors (id, business_name, whatsapp_external_id, phone, status, primary_location, notes)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, v.ID.String(), v.BusinessName, v.WhatsAppExternalID, v.Phone, v.Status, v.PrimaryLocation, v.Notes)
	if err != nil {
		return nil, fmt.Errorf("inserting vendor: %w", err)
	}

	if err := replaceCategoriesAndAreas(ctx, tx, v.ID, v.Categories, v.ServiceAreas); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return r.FindByID(ctx, v.ID)
}

func (r *VendorRepository) Update(ctx context.Context, id uuid.UUID, fields vendor.Fields) (*vendor.Vendor, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	setClauses := []string{}
	args := []any{}
	argN := 1

	addSet := func(col string, val any) {
		setClauses = append(setClauses, fmt.Sprintf("%s = $%d", col, argN))
		args = append(args, val)
		argN++
	}

	if fields.BusinessName != nil {
		addSet("business_name", *fields.BusinessName)
	}
	if fields.Phone != nil {
		addSet("phone", *fields.Phone)
	}
	if fields.Status != nil {
		addSet("status", *fields.Status)
	}
	if fields.PrimaryLocation != nil {
		addSet("primary_location", *fields.PrimaryLocation)
	}
	if fields.Notes != nil {
		addSet("notes", *fields.Notes)
	}

	if len(setClauses) > 0 {
		addSet("updated_at", time.Now().UTC())
		query := fmt.Sprintf("UPDATE vendors SET %s WHERE id = $%d", strings.Join(setClauses, ", "), argN)
		args = append(args, id.String())
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return nil, fmt.Errorf("updating vendor: %w", err)
		}
	}

	if fields.Categories != nil || fields.ServiceAreas != nil {
		current, err := r.FindByID(ctx, id)
		if err != nil {
			return nil, err
		}
		cats := fields.Categories
		if cats == nil {
			cats = current.Categories
		}
		areas := fields.ServiceAreas
		if areas == nil {
			areas = current.ServiceAreas
		}
		if err := replaceCategoriesAndAreas(ctx, tx, id, cats, areas); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return r.FindByID(ctx, id)
}

func replaceCategoriesAndAreas(ctx context.Context, tx *sql.Tx, vendorID uuid.UUID, categories, areas []string) error {
	if categories != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM vendor_categories WHERE vendor_id = $1`, vendorID.String()); err != nil {
			return fmt.Errorf("clearing categories: %w", err)
		}
		for _, c := range categories {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO vendor_categories (vendor_id, category) VALUES ($1, $2)
				ON CONFLICT DO NOTHING
			`, vendorID.String(), strings.ToLower(strings.TrimSpace(c))); err != nil {
				return fmt.Errorf("inserting category %s: %w", c, err)
			}
		}
	}

	if areas != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM vendor_service_areas WHERE vendor_id = $1`, vendorID.String()); err != nil {
			return fmt.Errorf("clearing service areas: %w", err)
		}
		for _, a := range areas {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO vendor_service_areas (vendor_id, location) VALUES ($1, $2)
				ON CONFLICT DO NOTHING
			`, vendorID.String(), a); err != nil {
				return fmt.Errorf("inserting service area %s: %w", a, err)
			}
		}
	}

	return nil
}

const vendorColumns = `
	id, business_name, whatsapp_external_id, phone, status, primary_location, notes,
	requests_received, responses_received, last_response_at, last_message_at, last_supply_submission_at,
	created_at, updated_at
`

func scanVendorRow(scanner interface{ Scan(dest ...any) error }) (*vendor.Vendor, error) {
	var (
		v                          vendor.Vendor
		idStr                      string
		phone, primaryLoc, notes   sql.NullString
		lastResp, lastMsg, lastSup sql.NullTime
	)

	err := scanner.Scan(
		&idStr, &v.BusinessName, &v.WhatsAppExternalID, &phone, &v.Status, &primaryLoc, &notes,
		&v.RequestsReceived, &v.ResponsesReceived, &lastResp, &lastMsg, &lastSup,
		&v.CreatedAt, &v.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	v.ID, err = uuid.Parse(idStr)
	if err != nil {
		return nil, err
	}
	if phone.Valid {
		v.Phone = phone.String
	}
	if primaryLoc.Valid {
		v.PrimaryLocation = primaryLoc.String
	}
	if notes.Valid {
		v.Notes = notes.String
	}
	if lastResp.Valid {
		v.LastResponseAt = &lastResp.Time
	}
	if lastMsg.Valid {
		v.LastMessageAt = &lastMsg.Time
	}
	if lastSup.Valid {
		v.LastSupplySubmissionAt = &lastSup.Time
	}

	return &v, nil
}

func (r *VendorRepository) loadCategoriesAndAreas(ctx context.Context, v *vendor.Vendor) error {
	catRows, err := r.db.QueryContext(ctx, `SELECT category FROM vendor_categories WHERE vendor_id = $1`, v.ID.String())
	if err != nil {
		return err
	}
	defer catRows.Close()
	v.Categories = []string{}
	for catRows.Next() {
		var c string
		if err := catRows.Scan(&c); err != nil {
			return err
		}
		v.Categories = append(v.Categories, c)
	}

	areaRows, err := r.db.QueryContext(ctx, `SELECT location FROM vendor_service_areas WHERE vendor_id = $1`, v.ID.String())
	if err != nil {
		return err
	}
	defer areaRows.Close()
	v.ServiceAreas = []string{}
	for areaRows.Next() {
		var a string
		if err := areaRows.Scan(&a); err != nil {
			return err
		}
		v.ServiceAreas = append(v.ServiceAreas, a)
	}

	return nil
}

func (r *VendorRepository) FindByID(ctx context.Context, id uuid.UUID) (*vendor.Vendor, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+vendorColumns+` FROM vendors WHERE id = $1`, id.String())
	v, err := scanVendorRow(row)
	if err != nil {
		return nil, err
	}
	if err := r.loadCategoriesAndAreas(ctx, v); err != nil {
		return nil, err
	}
	return v, nil
}

func (r *VendorRepository) FindByWhatsAppID(ctx context.Context, whatsappID string) (*vendor.Vendor, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+vendorColumns+` FROM vendors WHERE whatsapp_external_id = $1`, whatsappID)
	v, err := scanVendorRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := r.loadCategoriesAndAreas(ctx, v); err != nil {
		return nil, err
	}
	return v, nil
}

func (r *VendorRepository) List(ctx context.Context, statusFilter *vendor.Status, categoryFilter *string) ([]*vendor.Vendor, error) {
	query := `SELECT DISTINCT v.id, v.business_name, v.whatsapp_external_id, v.phone, v.status,
		v.primary_location, v.notes, v.requests_received, v.responses_received,
		v.last_response_at, v.last_message_at, v.last_supply_submission_at,
		v.created_at, v.updated_at
		FROM vendors v`
	var conditions []string
	var args []any
	argN := 1

	if categoryFilter != nil {
		query += ` JOIN vendor_categories vc ON vc.vendor_id = v.id`
		conditions = append(conditions, fmt.Sprintf("vc.category = $%d", argN))
		args = append(args, strings.ToLower(*categoryFilter))
		argN++
	}
	if statusFilter != nil {
		conditions = append(conditions, fmt.Sprintf("v.status = $%d", argN))
		args = append(args, *statusFilter)
		argN++
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY v.created_at DESC"

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*vendor.Vendor
	for rows.Next() {
		v, err := scanVendorRow(rows)
		if err != nil {
			return nil, err
		}
		if err := r.loadCategoriesAndAreas(ctx, v); err != nil {
			return nil, err
		}
		results = append(results, v)
	}
	return results, rows.Err()
}

func (r *VendorRepository) ActiveByCategory(ctx context.Context, category string) ([]*vendor.Vendor, error) {
	active := vendor.StatusActive
	cat := category
	return r.List(ctx, &active, &cat)
}

func (r *VendorRepository) RecordMessageActivity(ctx context.Context, vendorID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `UPDATE vendors SET last_message_at = now(), updated_at = now() WHERE id = $1`, vendorID.String())
	return err
}

func (r *VendorRepository) RecordSupplySubmission(ctx context.Context, vendorID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `UPDATE vendors SET last_supply_submission_at = now(), updated_at = now() WHERE id = $1`, vendorID.String())
	return err
}

func (r *VendorRepository) RecordResponseActivity(ctx context.Context, vendorID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE vendors SET responses_received = responses_received + 1, last_response_at = now(), updated_at = now()
		WHERE id = $1
	`, vendorID.String())
	return err
}

func (r *VendorRepository) RecordRequestSent(ctx context.Context, vendorID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE vendors SET requests_received = requests_received + 1, updated_at = now()
		WHERE id = $1
	`, vendorID.String())
	return err
}
