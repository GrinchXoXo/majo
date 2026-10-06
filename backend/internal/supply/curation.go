package supply

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/audit"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/vendor"
)

// Phase 4A Step 4: operator curation of SupplyListings.
//
//	DRAFT ──Publish──▶ ACTIVE          (SUPPLY_LISTING_PUBLISHED, once)
//	DRAFT ──Reject───▶ ARCHIVED        (rejected_at + reason, SUPPLY_LISTING_REJECTED)
//	ACTIVE ─Reject───▶ ARCHIVED        (operator withdraws a published listing)
//	DRAFT/ACTIVE ─Edit─▶ same status   (SUPPLY_LISTING_UPDATED + audit)
//
// Publish on ACTIVE and Reject on a rejected listing are idempotent no-ops
// (no event, no audit, no timestamp change). Every other transition is
// refused with ErrInvalidTransition. ARCHIVED is terminal: there is no
// republish route (the existing state machine has none).
//
// The operator edits the SupplyListing only. The SupplySignal, its source
// messages and MediaAssets are evidence and are never written here.

// SupportedCurrencies are the currencies Majo handles today. Every
// existing table defaults to NGN and nothing else is produced anywhere.
var SupportedCurrencies = map[string]bool{"NGN": true}

const (
	maxShortField  = 200
	maxDescription = 2000
	maxReason      = 500
)

var (
	ErrListingNotFound = errors.New("listing not found")
	// ErrStale: the caller's expected_updated_at no longer matches (another
	// tab or operator changed the listing first).
	ErrStale = errors.New("listing was changed by someone else; reload and retry")
)

// ErrInvalidTransition: the operation is not allowed from the listing's status.
type ErrInvalidTransition struct {
	Op   string
	From ListingStatus
}

func (e ErrInvalidTransition) Error() string {
	return fmt.Sprintf("cannot %s a listing in status %s", e.Op, e.From)
}

// FieldError is one validation problem.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError lists every edit problem (the edit is not applied).
type ValidationError struct{ Problems []FieldError }

func (e ValidationError) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = p.Field + ": " + p.Message
	}
	return "invalid edit: " + strings.Join(parts, "; ")
}

// ErrNotPublishable lists why a DRAFT cannot be published (it stays DRAFT).
type ErrNotPublishable struct{ Reasons []FieldError }

func (e ErrNotPublishable) Error() string {
	parts := make([]string, len(e.Reasons))
	for i, p := range e.Reasons {
		parts[i] = p.Field + ": " + p.Message
	}
	return "listing cannot be published: " + strings.Join(parts, "; ")
}

// CurationChange is what a CurationDecision asks Curate to apply.
type CurationChange struct {
	Fields ListingFields
	// Publish: status = ACTIVE, published_at = COALESCE(published_at, now()).
	Publish bool
	// Reject: status = ARCHIVED, rejected_at = now(), rejection_reason = RejectReason.
	Reject       bool
	RejectReason *string
	Audit        *audit.Entry
	Event        *event.Event
}

// CurationDecision inspects the locked current row and returns the change
// to apply (nil = nothing to do) or an error (nothing is written).
type CurationDecision func(current *Listing) (*CurationChange, error)

// MediaAssetState is what curation needs to know about a referenced asset.
type MediaAssetState struct {
	OwnerVendorID uuid.UUID
	Kind          string
	MimeType      string
	SizeBytes     int64
	Status        string
	StorageKey    string
}

// ListingEdit is the explicit set of operator-editable fields. Anything not
// here (IDs, vendor, source signal, public reference, status, timestamps,
// media identity) cannot be edited.
type ListingEdit struct {
	Category    *string
	Product     *string
	Variant     *string
	Condition   *string
	Price       *int64
	Currency    *string
	Quantity    *int
	Location    *string
	Description *string
	// MediaOrder, when set, must be exactly the listing's current asset IDs
	// in the new order (no additions, no removals).
	MediaOrder []string
	// PrimaryAssetID, when set, must be one of the listing's image assets.
	PrimaryAssetID *string
	// ExpectedUpdatedAt, when set, must equal the listing's updated_at.
	ExpectedUpdatedAt *time.Time
}

func (e ListingEdit) empty() bool {
	return e.Category == nil && e.Product == nil && e.Variant == nil && e.Condition == nil &&
		e.Price == nil && e.Currency == nil && e.Quantity == nil && e.Location == nil &&
		e.Description == nil && e.MediaOrder == nil && e.PrimaryAssetID == nil
}

func checkStale(current *Listing, expected *time.Time) error {
	if expected != nil && !current.UpdatedAt.Equal(*expected) {
		return ErrStale
	}
	return nil
}

func cleanText(field string, v *string, max int, probs *[]FieldError) *string {
	if v == nil {
		return nil
	}
	t := strings.TrimSpace(*v)
	switch {
	case t == "":
		*probs = append(*probs, FieldError{field, "must not be empty"})
	case utf8.RuneCountInString(t) > max:
		*probs = append(*probs, FieldError{field, fmt.Sprintf("must be at most %d characters", max)})
	}
	return &t
}

// validateEdit checks an edit against the current listing and returns the
// normalized field changes.
func validateEdit(current *Listing, e ListingEdit) (ListingFields, error) {
	var probs []FieldError
	f := ListingFields{
		Category:    cleanText("category", e.Category, maxShortField, &probs),
		Product:     cleanText("product", e.Product, maxShortField, &probs),
		Variant:     cleanText("variant", e.Variant, maxShortField, &probs),
		Condition:   cleanText("condition", e.Condition, maxShortField, &probs),
		Location:    cleanText("location", e.Location, maxShortField, &probs),
		Description: cleanText("description", e.Description, maxDescription, &probs),
		Price:       e.Price,
		Quantity:    e.Quantity,
	}
	if e.Price != nil && *e.Price <= 0 {
		probs = append(probs, FieldError{"price", "must be greater than 0"})
	}
	if e.Quantity != nil && *e.Quantity <= 0 {
		probs = append(probs, FieldError{"quantity", "must be greater than 0"})
	}
	if e.Currency != nil {
		c := strings.ToUpper(strings.TrimSpace(*e.Currency))
		if !SupportedCurrencies[c] {
			probs = append(probs, FieldError{"currency", "unsupported currency (supported: NGN)"})
		}
		f.Currency = &c
	}

	if e.MediaOrder != nil || e.PrimaryAssetID != nil {
		refs, mprobs := reorderMedia(current.MediaReferences, e.MediaOrder, e.PrimaryAssetID)
		probs = append(probs, mprobs...)
		if len(mprobs) == 0 {
			f.MediaReferences = &refs
		}
	}
	if len(probs) > 0 {
		return ListingFields{}, ValidationError{Problems: probs}
	}
	return f, nil
}

// reorderMedia applies a new order and/or primary selection to the
// listing's existing references. Only existing references can be used:
// nothing is added, removed or duplicated.
func reorderMedia(current []MediaReference, order []string, primary *string) ([]MediaReference, []FieldError) {
	byID := make(map[string]MediaReference, len(current))
	for _, m := range current {
		byID[m.AssetID] = m
	}
	out := make([]MediaReference, 0, len(current))
	if order != nil {
		if len(order) != len(current) {
			return nil, []FieldError{{"media_order", "must list exactly the listing's current media"}}
		}
		seen := map[string]bool{}
		for _, id := range order {
			m, ok := byID[id]
			if !ok || id == "" || seen[id] {
				return nil, []FieldError{{"media_order", "must list exactly the listing's current media"}}
			}
			seen[id] = true
			out = append(out, m)
		}
	} else {
		out = append(out, current...)
	}
	if primary != nil {
		m, ok := byID[*primary]
		if !ok || *primary == "" {
			return nil, []FieldError{{"primary_media_asset_id", "is not one of this listing's media"}}
		}
		if m.Type != "image" {
			return nil, []FieldError{{"primary_media_asset_id", "must be an image"}}
		}
		for i := range out {
			out[i].Role = ""
			if out[i].AssetID == *primary {
				out[i].Role = MediaRolePrimary
			}
		}
	}
	return out, nil
}

// diffListing returns the before/after values of the fields f changes.
func diffListing(cur *Listing, f ListingFields) (prev, next map[string]any) {
	prev, next = map[string]any{}, map[string]any{}
	str := func(name string, old, nw *string) {
		if nw != nil && notEqualStrPtr(old, nw) {
			prev[name], next[name] = derefStrPtr(old), *nw
		}
	}
	str("category", cur.Category, f.Category)
	str("product", cur.Product, f.Product)
	str("variant", cur.Variant, f.Variant)
	str("condition", cur.Condition, f.Condition)
	str("location", cur.Location, f.Location)
	str("description", cur.Description, f.Description)
	if f.Price != nil && notEqualInt64Ptr(cur.Price, f.Price) {
		prev["price"], next["price"] = derefInt64Ptr(cur.Price), *f.Price
	}
	if f.Quantity != nil && (cur.Quantity == nil || *cur.Quantity != *f.Quantity) {
		var old any
		if cur.Quantity != nil {
			old = *cur.Quantity
		}
		prev["quantity"], next["quantity"] = old, *f.Quantity
	}
	if f.Currency != nil && cur.Currency != *f.Currency {
		prev["currency"], next["currency"] = cur.Currency, *f.Currency
	}
	if f.MediaReferences != nil && !sameMedia(cur.MediaReferences, *f.MediaReferences) {
		prev["media"], next["media"] = mediaSummary(cur.MediaReferences), mediaSummary(*f.MediaReferences)
	}
	return prev, next
}

func sameMedia(a, b []MediaReference) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].AssetID != b[i].AssetID || a[i].Role != b[i].Role {
			return false
		}
	}
	return true
}

func mediaSummary(refs []MediaReference) []map[string]any {
	out := make([]map[string]any, len(refs))
	for i, m := range refs {
		out[i] = map[string]any{"asset_id": m.AssetID, "type": m.Type, "role": m.Role}
	}
	return out
}

// onlyChanged drops fields whose value would not change.
func onlyChanged(f ListingFields, changed map[string]any) ListingFields {
	keep := func(name string) bool { _, ok := changed[name]; return ok }
	var out ListingFields
	if keep("category") {
		out.Category = f.Category
	}
	if keep("product") {
		out.Product = f.Product
	}
	if keep("variant") {
		out.Variant = f.Variant
	}
	if keep("condition") {
		out.Condition = f.Condition
	}
	if keep("location") {
		out.Location = f.Location
	}
	if keep("description") {
		out.Description = f.Description
	}
	if keep("price") {
		out.Price = f.Price
	}
	if keep("quantity") {
		out.Quantity = f.Quantity
	}
	if keep("currency") {
		out.Currency = f.Currency
	}
	if keep("media") {
		out.MediaReferences = f.MediaReferences
	}
	return out
}

func changedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Edit applies an operator correction to a DRAFT or ACTIVE listing. The
// status, published_at and source evidence are untouched; an edit never
// publishes. A no-op edit writes nothing.
func (s *ListingService) Edit(ctx context.Context, listingID uuid.UUID, e ListingEdit, operatorID *uuid.UUID) (*Listing, error) {
	return s.listings.Curate(ctx, listingID, func(cur *Listing) (*CurationChange, error) {
		if err := checkStale(cur, e.ExpectedUpdatedAt); err != nil {
			return nil, err
		}
		if cur.Status != ListingDraft && cur.Status != ListingActive {
			return nil, ErrInvalidTransition{Op: "edit", From: cur.Status}
		}
		if e.empty() {
			return nil, nil
		}
		f, err := validateEdit(cur, e)
		if err != nil {
			return nil, err
		}
		prev, next := diffListing(cur, f)
		if len(next) == 0 {
			return nil, nil
		}
		id := cur.ID
		return &CurationChange{
			Fields: onlyChanged(f, next),
			Audit: &audit.Entry{
				ActorType: "OPERATOR", ActorID: operatorID, Action: "SUPPLY_LISTING_CORRECTED",
				EntityType: "supply_listing", EntityID: &id, PreviousValue: prev, NewValue: next,
				Metadata: map[string]any{"public_reference": cur.PublicReference, "status": string(cur.Status)},
			},
			Event: &event.Event{
				EventType: event.SupplyListingUpdated, EntityType: "supply_listing", EntityID: &id,
				ActorType: event.ActorOperator, ActorID: operatorID,
				Payload: map[string]any{"previous": prev, "new": next, "changed": changedKeys(next),
					"public_reference": cur.PublicReference, "status": string(cur.Status)},
			},
		}, nil
	})
}

// PublishCheck returns why the listing cannot cross the publication gate
// (empty = publishable). It is the single definition of "publishable".
func (s *ListingService) PublishCheck(ctx context.Context, l *Listing, now time.Time) ([]FieldError, error) {
	var r []FieldError
	if l.Product == nil || strings.TrimSpace(*l.Product) == "" {
		r = append(r, FieldError{"product", "is required"})
	}
	if l.Price == nil {
		r = append(r, FieldError{"price", "is required"})
	} else if *l.Price <= 0 {
		r = append(r, FieldError{"price", "must be greater than 0"})
	}
	if !SupportedCurrencies[l.Currency] {
		r = append(r, FieldError{"currency", "unsupported currency"})
	}
	if l.Quantity != nil && *l.Quantity <= 0 {
		r = append(r, FieldError{"quantity", "must be greater than 0"})
	}
	if l.Location != nil && strings.TrimSpace(*l.Location) == "" {
		r = append(r, FieldError{"location", "must not be blank (leave it unset instead)"})
	}
	if !l.ExpiresAt.After(now) {
		r = append(r, FieldError{"expires_at", "the supply evidence has expired"})
	}

	v, err := s.vendors.FindByID(ctx, l.VendorID)
	if err != nil {
		return nil, fmt.Errorf("loading vendor: %w", err)
	}
	switch {
	case v == nil:
		r = append(r, FieldError{"vendor", "does not exist"})
	case v.Status != vendor.StatusActive:
		r = append(r, FieldError{"vendor", "is " + string(v.Status) + ", not ACTIVE"})
	}

	if l.SupplySignalID == nil {
		r = append(r, FieldError{"supply_signal_id", "listing has no source signal"})
	} else {
		sig, err := s.signals.FindByID(ctx, *l.SupplySignalID)
		if err != nil {
			return nil, fmt.Errorf("loading source signal: %w", err)
		}
		if sig == nil {
			r = append(r, FieldError{"supply_signal_id", "source signal does not exist"})
		} else if sig.VendorID != l.VendorID {
			r = append(r, FieldError{"supply_signal_id", "source signal belongs to another vendor"})
		}
	}

	mr, err := s.mediaProblems(ctx, l)
	if err != nil {
		return nil, err
	}
	return append(r, mr...), nil
}

// mediaProblems: every reference must point at an existing MediaAsset owned
// by the listing's vendor, and the primary image must be AVAILABLE. Media
// itself is optional (Architecture Lock V2 does not require it).
func (s *ListingService) mediaProblems(ctx context.Context, l *Listing) ([]FieldError, error) {
	if len(l.MediaReferences) == 0 {
		return nil, nil
	}
	var r []FieldError
	ids := make([]uuid.UUID, 0, len(l.MediaReferences))
	for _, m := range l.MediaReferences {
		id, err := uuid.Parse(m.AssetID)
		if err != nil {
			r = append(r, FieldError{"media", "a media reference has no valid asset"})
			continue
		}
		ids = append(ids, id)
	}
	states, err := s.media.MediaAssetStates(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("loading media assets: %w", err)
	}
	for _, id := range ids {
		st, ok := states[id]
		switch {
		case !ok:
			r = append(r, FieldError{"media", "asset " + id.String() + " does not exist"})
		case st.OwnerVendorID != l.VendorID:
			r = append(r, FieldError{"media", "asset " + id.String() + " belongs to another vendor"})
		}
	}
	if p := PrimaryMedia(l.MediaReferences); p != nil {
		if id, err := uuid.Parse(p.AssetID); err == nil {
			if st, ok := states[id]; ok && st.Status != "AVAILABLE" {
				r = append(r, FieldError{"primary_media_asset_id", "the primary image is " + st.Status + "; choose another"})
			}
		}
	}
	return r, nil
}

// Publish is the publication gate: the only way a listing becomes ACTIVE.
// DRAFT -> ACTIVE once every PublishCheck passes; already ACTIVE -> no-op.
// SUPPLY_LISTING_PUBLISHED and the audit entry are written in the same
// transaction as the transition, so concurrent publishes yield one event.
// Publishing sends nothing to anyone and creates no offer or request.
func (s *ListingService) Publish(ctx context.Context, listingID uuid.UUID, expectedUpdatedAt *time.Time, operatorID *uuid.UUID) (*Listing, error) {
	return s.listings.Curate(ctx, listingID, func(cur *Listing) (*CurationChange, error) {
		if cur.Status == ListingActive {
			return nil, nil // already published: idempotent, nothing re-emitted
		}
		if err := checkStale(cur, expectedUpdatedAt); err != nil {
			return nil, err
		}
		if cur.Status != ListingDraft {
			return nil, ErrInvalidTransition{Op: "publish", From: cur.Status}
		}
		reasons, err := s.PublishCheck(ctx, cur, time.Now())
		if err != nil {
			return nil, err
		}
		if len(reasons) > 0 {
			return nil, ErrNotPublishable{Reasons: reasons}
		}
		id := cur.ID
		payload := map[string]any{
			"public_reference": cur.PublicReference, "vendor_id": cur.VendorID.String(),
			"supply_signal_id": cur.SupplySignalID.String(), "from_status": string(cur.Status),
			"media_count": len(cur.MediaReferences),
		}
		if p := PrimaryMedia(cur.MediaReferences); p != nil {
			payload["primary_media_asset_id"] = p.AssetID
		}
		return &CurationChange{
			Publish: true,
			Audit: &audit.Entry{
				ActorType: "OPERATOR", ActorID: operatorID, Action: "SUPPLY_LISTING_PUBLISHED",
				EntityType: "supply_listing", EntityID: &id,
				PreviousValue: map[string]any{"status": string(cur.Status)},
				NewValue:      map[string]any{"status": string(ListingActive)},
				Metadata:      map[string]any{"public_reference": cur.PublicReference},
			},
			Event: &event.Event{
				EventType: event.SupplyListingPublished, EntityType: "supply_listing", EntityID: &id,
				ActorType: event.ActorOperator, ActorID: operatorID, Payload: payload,
			},
		}, nil
	})
}

// Reject archives a DRAFT (or withdraws an ACTIVE) listing with an optional
// reason. The SupplySignal, source messages and MediaAssets are untouched.
// Rejecting an already rejected listing is a no-op.
func (s *ListingService) Reject(ctx context.Context, listingID uuid.UUID, reason *string, expectedUpdatedAt *time.Time, operatorID *uuid.UUID) (*Listing, error) {
	var clean *string
	if reason != nil {
		t := strings.TrimSpace(*reason)
		if utf8.RuneCountInString(t) > maxReason {
			return nil, ValidationError{Problems: []FieldError{{"reason", fmt.Sprintf("must be at most %d characters", maxReason)}}}
		}
		if t != "" {
			clean = &t
		}
	}
	return s.listings.Curate(ctx, listingID, func(cur *Listing) (*CurationChange, error) {
		if cur.Status == ListingArchived && cur.RejectedAt != nil {
			return nil, nil // already rejected: idempotent
		}
		if err := checkStale(cur, expectedUpdatedAt); err != nil {
			return nil, err
		}
		if cur.Status != ListingDraft && cur.Status != ListingActive {
			return nil, ErrInvalidTransition{Op: "reject", From: cur.Status}
		}
		id := cur.ID
		return &CurationChange{
			Reject: true, RejectReason: clean,
			Audit: &audit.Entry{
				ActorType: "OPERATOR", ActorID: operatorID, Action: "SUPPLY_LISTING_REJECTED",
				EntityType: "supply_listing", EntityID: &id,
				PreviousValue: map[string]any{"status": string(cur.Status)},
				NewValue:      map[string]any{"status": string(ListingArchived), "rejection_reason": derefStrPtr(clean)},
				Metadata:      map[string]any{"public_reference": cur.PublicReference},
			},
			Event: &event.Event{
				EventType: event.SupplyListingRejected, EntityType: "supply_listing", EntityID: &id,
				ActorType: event.ActorOperator, ActorID: operatorID,
				Payload: map[string]any{"public_reference": cur.PublicReference, "from_status": string(cur.Status),
					"reason": derefStrPtr(clean), "supply_signal_id": uuidPtrString(cur.SupplySignalID)},
			},
		}, nil
	})
}

func uuidPtrString(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

// MediaAssetStates exposes the asset lookup used by publication checks, so
// the media preview applies exactly the same ownership/availability rules.
func (s *ListingService) MediaAssetStates(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]MediaAssetState, error) {
	return s.media.MediaAssetStates(ctx, ids)
}
