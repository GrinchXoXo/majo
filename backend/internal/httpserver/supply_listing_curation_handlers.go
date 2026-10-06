package httpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/supply"
)

// Phase 4A Step 4: operator curation endpoints.
//
//	POST  /api/supply-listings                    signal -> DRAFT (unchanged behaviour)
//	GET   /api/supply-listings/{id}               listing + provenance + media + publish blockers (operator-only)
//	PATCH /api/supply-listings/{id}               explicit marketplace fields only (never status)
//	POST  /api/supply-listings/{id}/publish       DRAFT -> ACTIVE (the publication gate)
//	POST  /api/supply-listings/{id}/activate      legacy alias of /publish (same operation)
//	POST  /api/supply-listings/{id}/reject        DRAFT|ACTIVE -> ARCHIVED (+ reason)
//	GET   /api/supply-listings/{id}/media/{asset} preview bytes of one of THIS listing's assets
//
// Errors keep the existing {"error": "..."} shape and add a stable "code";
// raw database errors are logged, never returned.

const maxCurationBody = 64 << 10

type createListingBody struct {
	SupplySignalID string `json:"supply_signal_id"`
}

// CreateFromSignal handles POST /api/supply-listings: an operator builds the
// DRAFT for a signal (Phase 4A.3 CreateFromSignal). It never publishes.
func (h *SupplyListingHandlers) CreateFromSignal(w http.ResponseWriter, r *http.Request) {
	var body createListingBody
	if err := json.NewDecoder(io.LimitReader(r.Body, maxCurationBody)).Decode(&body); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	signalID, err := parseUUID(body.SupplySignalID)
	if err != nil {
		writeErrorCode(w, http.StatusBadRequest, "BAD_REQUEST", "invalid supply_signal_id")
		return
	}
	listing, err := h.service.PublishFromSignal(r.Context(), signalID, nil)
	var inel supply.ErrIneligible
	switch {
	case errors.As(err, &inel):
		writeErrorCode(w, http.StatusUnprocessableEntity, "INELIGIBLE", err.Error())
	case errors.Is(err, supply.ErrSignalNotFound):
		writeErrorCode(w, http.StatusNotFound, "NOT_FOUND", err.Error())
	case err != nil:
		h.internalError(w, r, "creating listing from signal", err)
	default:
		writeJSON(w, http.StatusCreated, toListingDTO(listing))
	}
}

// --- detail ---

type curationVendorDTO struct {
	ID              string `json:"id"`
	BusinessName    string `json:"business_name"`
	Status          string `json:"status"`
	PrimaryLocation string `json:"primary_location"`
}

type curationSignalDTO struct {
	ID                 string  `json:"id"`
	Category           *string `json:"category"`
	Product            *string `json:"product"`
	Variant            *string `json:"variant"`
	Condition          *string `json:"condition"`
	Price              *int64  `json:"price"`
	Currency           string  `json:"currency"`
	Quantity           *int    `json:"quantity"`
	Location           *string `json:"location"`
	AvailabilityStatus string  `json:"availability_status"`
	RawMessage         string  `json:"raw_message"`
	Confidence         float64 `json:"confidence"`
	CreatedAt          string  `json:"created_at"`
	ExpiresAt          string  `json:"expires_at"`
}

type curationSourceDTO struct {
	Position          int     `json:"position"`
	MessageID         string  `json:"message_id"`
	ExternalMessageID *string `json:"external_message_id"`
	MessageType       string  `json:"message_type"`
	Text              *string `json:"text"`
	MediaAssetID      *string `json:"media_asset_id"`
	ReceivedAt        string  `json:"received_at"`
}

type curationMediaDTO struct {
	Order     int    `json:"order"`
	AssetID   string `json:"asset_id"`
	Type      string `json:"type"`
	Role      string `json:"role,omitempty"`
	Primary   bool   `json:"primary"`
	Status    string `json:"status"` // AVAILABLE | REMOVED | MISSING
	Available bool   `json:"available"`
	MimeType  string `json:"mime_type,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	// PreviewPath is relative to the API base; fetch it with the operator
	// Authorization header. Empty when the media is unavailable.
	PreviewPath string `json:"preview_path,omitempty"`
}

type listingDetailDTO struct {
	listingDTO
	Vendor         *curationVendorDTO  `json:"vendor"`
	SourceSignal   *curationSignalDTO  `json:"source_signal"`
	SourceMessages []curationSourceDTO `json:"source_messages"`
	Media          []curationMediaDTO  `json:"media"`
	Published      bool                `json:"published"`
	Rejected       bool                `json:"rejected"`
	// PublishBlockers lists why a DRAFT cannot be published yet (empty =
	// publishable). Always empty for non-DRAFT listings.
	PublishBlockers []supply.FieldError `json:"publish_blockers"`
}

// Get handles GET /api/supply-listings/{id}. The listing fields are the
// existing DTO (unchanged); provenance, media and blockers are additive.
func (h *SupplyListingHandlers) Get(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	v, err := h.curation.CurationView(r.Context(), id)
	if err != nil {
		h.internalError(w, r, "loading listing", err)
		return
	}
	if v == nil {
		writeErrorCode(w, http.StatusNotFound, "NOT_FOUND", "listing not found")
		return
	}
	l := v.Listing
	out := listingDetailDTO{
		listingDTO: toListingDTO(l), SourceMessages: []curationSourceDTO{}, Media: []curationMediaDTO{},
		Published: l.PublishedAt != nil, Rejected: l.RejectedAt != nil, PublishBlockers: []supply.FieldError{},
	}
	if v.Vendor != nil {
		out.Vendor = &curationVendorDTO{ID: v.Vendor.ID.String(), BusinessName: v.Vendor.BusinessName,
			Status: v.Vendor.Status, PrimaryLocation: v.Vendor.PrimaryLocation}
	}
	if s := v.Signal; s != nil {
		out.SourceSignal = &curationSignalDTO{ID: s.ID.String(), Category: s.Category, Product: s.Product,
			Variant: s.Variant, Condition: s.Condition, Price: s.Price, Currency: s.Currency, Quantity: s.Quantity,
			Location: s.Location, AvailabilityStatus: s.AvailabilityStatus, RawMessage: s.RawMessage,
			Confidence: s.Confidence, CreatedAt: s.CreatedAt.Format(timeFormat), ExpiresAt: s.ExpiresAt.Format(timeFormat)}
	}
	for _, src := range v.Sources {
		d := curationSourceDTO{Position: src.Position, MessageID: src.MessageID.String(),
			ExternalMessageID: src.ExternalMessageID, MessageType: src.MessageType, Text: src.Text,
			ReceivedAt: src.ReceivedAt.Format(timeFormat)}
		if src.MediaAssetID != nil {
			a := src.MediaAssetID.String()
			d.MediaAssetID = &a
		}
		out.SourceMessages = append(out.SourceMessages, d)
	}
	for _, m := range v.Media {
		d := curationMediaDTO{Order: m.Order, AssetID: m.AssetID, Type: m.Type, Role: m.Role, Primary: m.Primary,
			Status: m.Status, Available: m.Available(), MimeType: m.MimeType, SizeBytes: m.SizeBytes}
		if d.Available {
			d.PreviewPath = "/api/supply-listings/" + l.ID.String() + "/media/" + m.AssetID
		}
		out.Media = append(out.Media, d)
	}
	if l.Status == supply.ListingDraft {
		blockers, err := h.service.PublishCheck(r.Context(), l, time.Now())
		if err != nil {
			h.internalError(w, r, "checking publishability", err)
			return
		}
		if blockers != nil {
			out.PublishBlockers = blockers
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// --- edit ---

// patchListingBody is the complete list of operator-editable fields.
// Unknown fields (status, vendor_id, supply_signal_id, public_reference,
// timestamps, ...) are rejected with 400, not ignored.
type patchListingBody struct {
	Category            *string    `json:"category"`
	Product             *string    `json:"product"`
	Variant             *string    `json:"variant"`
	Condition           *string    `json:"condition"`
	Price               *int64     `json:"price"`
	Currency            *string    `json:"currency"`
	Quantity            *int       `json:"quantity"`
	Location            *string    `json:"location"`
	Description         *string    `json:"description"`
	MediaOrder          []string   `json:"media_order"`
	PrimaryMediaAssetID *string    `json:"primary_media_asset_id"`
	ExpectedUpdatedAt   *time.Time `json:"expected_updated_at"`
}

func decodeStrict(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxCurationBody))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		b = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Patch handles PATCH /api/supply-listings/{id}.
func (h *SupplyListingHandlers) Patch(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	var body patchListingBody
	if err := decodeStrict(r, &body); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "BAD_REQUEST",
			"invalid request body (editable fields: category, product, variant, condition, price, currency, quantity, location, description, media_order, primary_media_asset_id; plus expected_updated_at): "+err.Error())
		return
	}
	updated, err := h.service.Edit(r.Context(), id, supply.ListingEdit{
		Category: body.Category, Product: body.Product, Variant: body.Variant, Condition: body.Condition,
		Price: body.Price, Currency: body.Currency, Quantity: body.Quantity, Location: body.Location,
		Description: body.Description, MediaOrder: body.MediaOrder, PrimaryAssetID: body.PrimaryMediaAssetID,
		ExpectedUpdatedAt: body.ExpectedUpdatedAt,
	}, nil)
	if err != nil {
		h.curationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toListingDTO(updated))
}

// --- transitions ---

type transitionBody struct {
	ExpectedUpdatedAt *time.Time `json:"expected_updated_at"`
	Reason            *string    `json:"reason"`
}

// PublishListing handles POST /api/supply-listings/{id}/publish (and the
// legacy /activate alias): the only route to ACTIVE.
func (h *SupplyListingHandlers) PublishListing(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	var body transitionBody
	if err := decodeStrict(r, &body); err != nil || body.Reason != nil {
		writeErrorCode(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body (allowed: expected_updated_at)")
		return
	}
	l, err := h.service.Publish(r.Context(), id, body.ExpectedUpdatedAt, nil)
	if err != nil {
		h.curationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toListingDTO(l))
}

// Reject handles POST /api/supply-listings/{id}/reject.
func (h *SupplyListingHandlers) Reject(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	var body transitionBody
	if err := decodeStrict(r, &body); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body (allowed: reason, expected_updated_at)")
		return
	}
	l, err := h.service.Reject(r.Context(), id, body.Reason, body.ExpectedUpdatedAt, nil)
	if err != nil {
		h.curationError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toListingDTO(l))
}

// --- media preview ---

// Media handles GET /api/supply-listings/{id}/media/{asset}: the bytes of
// one AVAILABLE MediaAsset referenced by this listing, read through the
// storage.Provider boundary. Only the listing's own media can be fetched,
// and no storage credentials or provider URLs reach the browser.
func (h *SupplyListingHandlers) Media(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	assetID, err := uuid.Parse(r.PathValue("asset"))
	if err != nil {
		writeErrorCode(w, http.StatusBadRequest, "BAD_REQUEST", "invalid asset id")
		return
	}
	l, err := h.listings.FindByID(r.Context(), id)
	if err != nil {
		h.internalError(w, r, "loading listing", err)
		return
	}
	if l == nil {
		writeErrorCode(w, http.StatusNotFound, "NOT_FOUND", "listing not found")
		return
	}
	referenced := false
	for _, m := range l.MediaReferences {
		if m.AssetID == assetID.String() {
			referenced = true
			break
		}
	}
	if !referenced {
		writeErrorCode(w, http.StatusNotFound, "NOT_FOUND", "media is not part of this listing")
		return
	}
	states, err := h.service.MediaAssetStates(r.Context(), []uuid.UUID{assetID})
	if err != nil {
		h.internalError(w, r, "loading media asset", err)
		return
	}
	st, ok := states[assetID]
	if !ok || st.OwnerVendorID != l.VendorID || st.Status != "AVAILABLE" {
		writeErrorCode(w, http.StatusNotFound, "MEDIA_UNAVAILABLE", "media is unavailable")
		return
	}
	data, err := h.store.Get(r.Context(), st.StorageKey)
	if err != nil {
		h.logger.Warn("listing media unreadable from storage", "listing_id", id, "media_asset_id", assetID, "error", err)
		writeErrorCode(w, http.StatusNotFound, "MEDIA_UNAVAILABLE", "media is unavailable")
		return
	}
	w.Header().Set("Content-Type", st.MimeType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "inline")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// --- errors ---

func (h *SupplyListingHandlers) curationError(w http.ResponseWriter, r *http.Request, err error) {
	var verr supply.ValidationError
	var perr supply.ErrNotPublishable
	var terr supply.ErrInvalidTransition
	switch {
	case errors.Is(err, supply.ErrListingNotFound):
		writeErrorCode(w, http.StatusNotFound, "NOT_FOUND", "listing not found")
	case errors.Is(err, supply.ErrStale):
		writeErrorCode(w, http.StatusConflict, "STALE", err.Error())
	case errors.As(err, &terr):
		writeErrorCode(w, http.StatusConflict, "INVALID_TRANSITION", err.Error())
	case errors.As(err, &verr):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error(), "code": "VALIDATION", "problems": verr.Problems})
	case errors.As(err, &perr):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error(), "code": "NOT_PUBLISHABLE", "problems": perr.Reasons})
	default:
		h.internalError(w, r, "listing curation", err)
	}
}

func (h *SupplyListingHandlers) internalError(w http.ResponseWriter, r *http.Request, what string, err error) {
	h.logger.ErrorContext(r.Context(), what+" failed", "path", r.URL.Path, "error", err)
	writeErrorCode(w, http.StatusInternalServerError, "INTERNAL", what+" failed")
}
