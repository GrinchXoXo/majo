package httpserver

import (
	"encoding/json"
	"net/http"

	"github.com/middleman/backend/internal/supply"
)

// SupplyListingHandlers implements the marketplace supply/discovery API
// from PRD 3 §6/§14/§30.
type SupplyListingHandlers struct {
	listings supply.ListingRepository
	service  *supply.ListingService
}

func NewSupplyListingHandlers(listings supply.ListingRepository, service *supply.ListingService) *SupplyListingHandlers {
	return &SupplyListingHandlers{listings: listings, service: service}
}

type mediaRefDTO struct {
	Type       string `json:"type"`
	StorageKey string `json:"storage_key,omitempty"`
	URL        string `json:"url,omitempty"`
}

type listingDTO struct {
	ID                string        `json:"id"`
	VendorID          string        `json:"vendor_id"`
	SupplySignalID    *string       `json:"supply_signal_id"`
	Category          *string       `json:"category"`
	Product           *string       `json:"product"`
	Variant           *string       `json:"variant"`
	Condition         *string       `json:"condition"`
	Price             *int64        `json:"price"`
	Currency          string        `json:"currency"`
	Quantity          *int          `json:"quantity"`
	Location          *string       `json:"location"`
	Description       *string       `json:"description"`
	MediaReferences   []mediaRefDTO `json:"media_references"`
	Status            string        `json:"status"`
	ExtractionMethod  string        `json:"extraction_method"`
	Confidence        float64       `json:"confidence"`
	PublishedAt       *string       `json:"published_at"`
	ExpiresAt         string        `json:"expires_at"`
	CreatedAt         string        `json:"created_at"`
}

func toListingDTO(l *supply.Listing) listingDTO {
	dto := listingDTO{
		ID: l.ID.String(), VendorID: l.VendorID.String(),
		Category: l.Category, Product: l.Product, Variant: l.Variant, Condition: l.Condition,
		Price: l.Price, Currency: l.Currency, Quantity: l.Quantity, Location: l.Location,
		Description: l.Description, Status: string(l.Status),
		ExtractionMethod: l.ExtractionMethod, Confidence: l.Confidence,
		ExpiresAt: l.ExpiresAt.Format(timeFormat), CreatedAt: l.CreatedAt.Format(timeFormat),
	}
	if l.SupplySignalID != nil {
		s := l.SupplySignalID.String()
		dto.SupplySignalID = &s
	}
	if l.PublishedAt != nil {
		s := l.PublishedAt.Format(timeFormat)
		dto.PublishedAt = &s
	}
	dto.MediaReferences = make([]mediaRefDTO, 0, len(l.MediaReferences))
	for _, m := range l.MediaReferences {
		dto.MediaReferences = append(dto.MediaReferences, mediaRefDTO{Type: m.Type, StorageKey: m.StorageKey, URL: m.URL})
	}
	return dto
}

type publishListingBody struct {
	SupplySignalID string `json:"supply_signal_id"`
}

// Publish handles POST /api/supply-listings — operator-driven publish of
// a SupplySignal into durable marketplace supply (PRD 3 §5-6).
func (h *SupplyListingHandlers) Publish(w http.ResponseWriter, r *http.Request) {
	var body publishListingBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	signalID, err := parseUUID(body.SupplySignalID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid supply_signal_id")
		return
	}

	listing, err := h.service.PublishFromSignal(r.Context(), signalID, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to publish listing: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, toListingDTO(listing))
}

// Activate handles POST /api/supply-listings/:id/activate
func (h *SupplyListingHandlers) Activate(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	listing, err := h.service.Activate(r.Context(), id, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to activate listing: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toListingDTO(listing))
}

type patchListingBody struct {
	Product     *string `json:"product"`
	Variant     *string `json:"variant"`
	Condition   *string `json:"condition"`
	Price       *int64  `json:"price"`
	Quantity    *int    `json:"quantity"`
	Location    *string `json:"location"`
	Description *string `json:"description"`
	Status      *string `json:"status"`
}

// Patch handles PATCH /api/supply-listings/:id — operator correction
// (PRD 3 §12), following the Phase 2.1 correction philosophy.
func (h *SupplyListingHandlers) Patch(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	var body patchListingBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	fields := supply.ListingFields{
		Product: body.Product, Variant: body.Variant, Condition: body.Condition,
		Price: body.Price, Quantity: body.Quantity, Location: body.Location, Description: body.Description,
	}
	if body.Status != nil {
		st := supply.ListingStatus(*body.Status)
		fields.Status = &st
	}

	updated, err := h.service.Correct(r.Context(), id, fields, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "correction failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toListingDTO(updated))
}

// Get handles GET /api/supply-listings/:id
func (h *SupplyListingHandlers) Get(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	l, err := h.listings.FindByID(r.Context(), id)
	if err != nil || l == nil {
		writeError(w, http.StatusNotFound, "listing not found")
		return
	}
	writeJSON(w, http.StatusOK, toListingDTO(l))
}

// Search handles GET /api/supply-listings?category=&product=&price_min=&price_max=&location=&condition=
// — the marketplace discovery foundation (PRD 3 §14).
func (h *SupplyListingHandlers) Search(w http.ResponseWriter, r *http.Request) {
	q := supply.ListingQuery{}
	query := r.URL.Query()

	if v := query.Get("category"); v != "" {
		q.Category = &v
	}
	if v := query.Get("product"); v != "" {
		q.NormalizedProduct = &v
	}
	if v := query.Get("location"); v != "" {
		q.Location = &v
	}
	if v := query.Get("condition"); v != "" {
		q.Condition = &v
	}
	if v := query.Get("price_min"); v != "" {
		if n, err := parseInt64(v); err == nil {
			q.PriceMin = &n
		}
	}
	if v := query.Get("price_max"); v != "" {
		if n, err := parseInt64(v); err == nil {
			q.PriceMax = &n
		}
	}
	if v := query.Get("status"); v != "" {
		st := supply.ListingStatus(v)
		q.Status = &st
	}

	results, err := h.listings.Search(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "search failed")
		return
	}
	dtos := make([]listingDTO, 0, len(results))
	for _, l := range results {
		dtos = append(dtos, toListingDTO(l))
	}
	writeJSON(w, http.StatusOK, dtos)
}

// ListByVendor handles GET /api/vendors/:id/supply-listings
func (h *SupplyListingHandlers) ListByVendor(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	results, err := h.listings.ListByVendor(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list vendor's listings")
		return
	}
	dtos := make([]listingDTO, 0, len(results))
	for _, l := range results {
		dtos = append(dtos, toListingDTO(l))
	}
	writeJSON(w, http.StatusOK, dtos)
}
