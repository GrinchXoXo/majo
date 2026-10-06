package httpserver

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/middleman/backend/infrastructure/storage"
	"github.com/middleman/backend/internal/supply"
)

// SupplyListingHandlers implements the marketplace supply API (PRD 3
// §6/§14/§30) and, since Phase 4A Step 4, operator curation: detail with
// provenance, explicit-field edit, publish, reject and media preview.
// Every write and the media preview require operator authorization.
type SupplyListingHandlers struct {
	listings supply.ListingRepository
	service  *supply.ListingService
	curation supply.CurationReader
	store    storage.Provider
	auth     *OperatorAuth
	logger   *slog.Logger
}

func NewSupplyListingHandlers(listings supply.ListingRepository, service *supply.ListingService,
	curation supply.CurationReader, store storage.Provider, auth *OperatorAuth, logger *slog.Logger) *SupplyListingHandlers {
	return &SupplyListingHandlers{listings: listings, service: service, curation: curation, store: store, auth: auth, logger: logger}
}

type mediaRefDTO struct {
	Type       string `json:"type"`
	StorageKey string `json:"storage_key,omitempty"`
	URL        string `json:"url,omitempty"`
	AssetID    string `json:"asset_id,omitempty"`
	Role       string `json:"role,omitempty"`
}

type listingDTO struct {
	ID                string        `json:"id"`
	VendorID          string        `json:"vendor_id"`
	SupplySignalID    *string       `json:"supply_signal_id"`
	PublicReference   string        `json:"public_reference"`
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
	LastVerifiedAt    *string       `json:"last_verified_at"`
	ExpiresAt         string        `json:"expires_at"`
	CreatedAt         string        `json:"created_at"`
	// UpdatedAt is full precision: send it back as expected_updated_at to
	// get stale-edit protection.
	UpdatedAt            string  `json:"updated_at"`
	RejectedAt           *string `json:"rejected_at"`
	RejectionReason      *string `json:"rejection_reason"`
	PrimaryMediaAssetID  *string `json:"primary_media_asset_id"`
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
	if l.LastVerifiedAt != nil {
		s := l.LastVerifiedAt.Format(timeFormat)
		dto.LastVerifiedAt = &s
	}
	dto.PublicReference = l.PublicReference
	dto.UpdatedAt = l.UpdatedAt.UTC().Format(time.RFC3339Nano)
	if l.RejectedAt != nil {
		s := l.RejectedAt.Format(timeFormat)
		dto.RejectedAt = &s
	}
	dto.RejectionReason = l.RejectionReason
	if p := supply.PrimaryMedia(l.MediaReferences); p != nil && p.AssetID != "" {
		id := p.AssetID
		dto.PrimaryMediaAssetID = &id
	}
	dto.MediaReferences = make([]mediaRefDTO, 0, len(l.MediaReferences))
	for _, m := range l.MediaReferences {
		dto.MediaReferences = append(dto.MediaReferences, mediaRefDTO{Type: m.Type, StorageKey: m.StorageKey, URL: m.URL, AssetID: m.AssetID, Role: m.Role})
	}
	return dto
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
