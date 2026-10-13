package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/routing"
	"github.com/middleman/backend/internal/supply"
)

// RoutingHandlers implements the matching + vendor routing API from
// PRD 2 §29, §54-56, extended by PRD 2.1 §5/§11 (association correction,
// full response detail for the dashboard).
type RoutingHandlers struct {
	routing routing.Repository
	service *routing.Service
}

func NewRoutingHandlers(routingRepo routing.Repository, service *routing.Service) *RoutingHandlers {
	return &RoutingHandlers{routing: routingRepo, service: service}
}

type matchCandidateDTO struct {
	VendorID       string         `json:"vendor_id"`
	VendorName     string         `json:"vendor_name"`
	Score          int            `json:"score"`
	MatchType      string         `json:"match_type"`
	Breakdown      map[string]int `json:"breakdown"`
	Reasons        []string       `json:"reasons"`
	WithinBudget   *bool          `json:"within_budget"`
	RelevantSupply map[string]any `json:"relevant_supply,omitempty"`
	// RelevantListing (Phase 4A) is the published listing that made this
	// vendor relevant, when listing evidence was the strongest.
	RelevantListing map[string]any `json:"relevant_listing,omitempty"`
}

// Matches handles GET /api/requests/:id/matches — the suggested vendor
// shortlist for a confirmed BuyerRequest (PRD 2 §54: "Request Matching View").
func (h *RoutingHandlers) Matches(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}

	candidates, _, err := h.service.SuggestVendors(r.Context(), id)
	if errors.Is(err, request.ErrNotConfirmed) {
		writeError(w, http.StatusConflict, "request must be CONFIRMED before matching")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to compute matches: "+err.Error())
		return
	}

	dtos := make([]matchCandidateDTO, 0, len(candidates))
	for _, c := range candidates {
		dto := matchCandidateDTO{
			VendorID: c.Vendor.ID.String(), VendorName: c.Vendor.BusinessName,
			Score: c.Score, MatchType: string(c.MatchType), Breakdown: c.Breakdown,
			Reasons: c.Reasons, WithinBudget: c.WithinBudget,
		}
		if c.RelevantSupply != nil {
			dto.RelevantSupply = map[string]any{
				"product": c.RelevantSupply.Product, "variant": c.RelevantSupply.Variant,
				"price": c.RelevantSupply.Price, "availability_status": c.RelevantSupply.AvailabilityStatus,
			}
		}
		if l := c.RelevantListing; l != nil {
			dto.RelevantListing = map[string]any{
				"id": l.ID.String(), "public_reference": l.PublicReference, "product": l.Product, "variant": l.Variant,
				"condition": l.Condition, "price": l.Price, "currency": l.Currency, "location": l.Location,
			}
			// Phase 4A.5: a published listing is not proof the item exists
			// today; a stale one needs vendor reconfirmation before a buyer
			// is told it is available.
			if el := c.ListingEligibility; el != nil {
				dto.RelevantListing["freshness"] = el.Freshness.State
				dto.RelevantListing["last_verified_at"] = el.Freshness.VerifiedAt
				dto.RelevantListing["geo_stage"] = el.Geography.Stage
				dto.RelevantListing["lifecycle_day"] = el.Lifecycle.Day
				dto.RelevantListing["needs_reconfirmation"] = el.Freshness.State != supply.Fresh
			}
		}
		dtos = append(dtos, dto)
	}

	writeJSON(w, http.StatusOK, dtos)
}

type routeRequestBody struct {
	VendorIDs []string `json:"vendor_ids"`
}

// Route handles POST /api/requests/:id/route — the operator OVERRIDE of
// routing (Majo routes automatically when policy permits; this remains for
// exceptions)
// (PRD 2 §29-33): creates VendorRequests for the approved vendors and
// sends them via WhatsApp.
func (h *RoutingHandlers) Route(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}

	var body routeRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(body.VendorIDs) == 0 {
		writeError(w, http.StatusBadRequest, "vendor_ids must not be empty")
		return
	}

	// Recompute matches to get the current scores/breakdowns for the
	// approved vendors (keeps the API stateless between suggest -> approve).
	candidates, req, err := h.service.SuggestVendors(r.Context(), id)
	if errors.Is(err, request.ErrNotConfirmed) {
		writeError(w, http.StatusConflict, "request must be CONFIRMED before routing to vendors")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load request: "+err.Error())
		return
	}
	scores := map[uuid.UUID]int{}
	breakdowns := map[uuid.UUID]map[string]any{}
	for _, c := range candidates {
		scores[c.Vendor.ID] = c.Score
		breakdown := map[string]any{}
		for k, v := range c.Breakdown {
			breakdown[k] = v
		}
		// Provenance of the evidence behind the score (Phase 4A).
		if c.RelevantListing != nil {
			breakdown["evidence"] = "listing"
			breakdown["supply_listing_id"] = c.RelevantListing.ID.String()
			breakdown["listing_reference"] = c.RelevantListing.PublicReference
		} else if c.RelevantSupply != nil {
			breakdown["evidence"] = "signal"
			breakdown["supply_signal_id"] = c.RelevantSupply.ID.String()
		}
		breakdowns[c.Vendor.ID] = breakdown
	}

	var vendorIDs []uuid.UUID
	for _, idStr := range body.VendorIDs {
		vid, err := uuid.Parse(idStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid vendor id: "+idStr)
			return
		}
		vendorIDs = append(vendorIDs, vid)
	}

	created, err := h.service.RouteToVendors(r.Context(), req, vendorIDs, scores, breakdowns)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to route to vendors: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, toVendorRequestDTOs(created))
}

type vendorRequestDTO struct {
	ID              string             `json:"id"`
	VendorID        string             `json:"vendor_id"`
	PublicReference string             `json:"public_reference"`
	Status          string             `json:"status"`
	MatchScore      *int               `json:"match_score"`
	CreatedAt       string             `json:"created_at"`
	SentAt          *string            `json:"sent_at"`
	SendError       *string            `json:"send_error"`
	SendFailedAt    *string            `json:"send_failed_at"`
	SupplyListingID *string            `json:"supply_listing_id"` // Phase 4B: listing re-confirmation
	Response        *vendorResponseDTO `json:"response,omitempty"`
}

func toVendorRequestDTOs(vrs []*routing.VendorRequest) []vendorRequestDTO {
	dtos := make([]vendorRequestDTO, 0, len(vrs))
	for _, vr := range vrs {
		dto := vendorRequestDTO{
			ID: vr.ID.String(), VendorID: vr.VendorID.String(), PublicReference: vr.PublicReference,
			Status: string(vr.Status), MatchScore: vr.MatchScore, CreatedAt: vr.CreatedAt.Format(timeFormat),
			SupplyListingID: idString(vr.SupplyListingID),
		}
		if vr.SentAt != nil {
			s := vr.SentAt.Format(timeFormat)
			dto.SentAt = &s
		}
		dto.SendError = vr.SendError
		if vr.SendFailedAt != nil {
			s := vr.SendFailedAt.Format(timeFormat)
			dto.SendFailedAt = &s
		}
		dtos = append(dtos, dto)
	}
	return dtos
}

// vendorResponseDTO exposes every field PRD 2.1 §11 requires the
// dashboard to be able to show: status, review state, all structured
// fields, confidence, extraction method, and the immutable raw message.
type vendorResponseDTO struct {
	ID               string  `json:"id"`
	VendorRequestID  *string `json:"vendor_request_id"`
	VendorID         string  `json:"vendor_id"`
	Status           string  `json:"status"`
	ReviewState      string  `json:"review_state"`
	Product          *string `json:"product"`
	Variant          *string `json:"variant"`
	Condition        *string `json:"condition"`
	Price            *int64  `json:"price"`
	Currency         string  `json:"currency"`
	Quantity         *int    `json:"quantity"`
	Location         *string `json:"location"`
	Notes            *string `json:"notes"`
	RawMessage       string  `json:"raw_message"`
	Confidence       float64 `json:"confidence"`
	ExtractionMethod string  `json:"extraction_method"`
	CreatedAt        string  `json:"created_at"`
}

func toResponseDTO(resp *routing.VendorResponse) vendorResponseDTO {
	dto := vendorResponseDTO{
		ID: resp.ID.String(), VendorID: resp.VendorID.String(),
		Status: string(resp.Status), ReviewState: string(resp.ReviewState),
		Product: resp.Product, Variant: resp.Variant, Condition: resp.Condition,
		Price: resp.Price, Currency: resp.Currency, Quantity: resp.Quantity,
		Location: resp.Location, Notes: resp.Notes,
		RawMessage: resp.RawMessage, Confidence: resp.Confidence,
		ExtractionMethod: string(resp.ExtractionMethod), CreatedAt: resp.CreatedAt.Format(timeFormat),
	}
	if resp.VendorRequestID != nil {
		s := resp.VendorRequestID.String()
		dto.VendorRequestID = &s
	}
	return dto
}

// VendorRequestsForBuyerRequest handles GET /api/requests/:id/vendor-requests
// — shows the full routing + response state for a buyer request
// (PRD 2 §55-56).
func (h *RoutingHandlers) VendorRequestsForBuyerRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}

	vrs, err := h.routing.ListVendorRequestsByBuyerRequest(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list vendor requests")
		return
	}

	dtos := toVendorRequestDTOs(vrs)
	for i, vr := range vrs {
		resp, err := h.routing.FindResponseByVendorRequest(r.Context(), vr.ID)
		if err == nil && resp != nil {
			d := toResponseDTO(resp)
			dtos[i].Response = &d
		}
	}

	writeJSON(w, http.StatusOK, dtos)
}

// Unmatched handles GET /api/vendor-responses/unmatched — the operator's
// manual-association queue (PRD 2.1 §5/§13).
func (h *RoutingHandlers) Unmatched(w http.ResponseWriter, r *http.Request) {
	responses, err := h.routing.ListUnmatched(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list unmatched responses")
		return
	}

	dtos := make([]vendorResponseDTO, 0, len(responses))
	for _, resp := range responses {
		dtos = append(dtos, toResponseDTO(resp))
	}
	writeJSON(w, http.StatusOK, dtos)
}

// VendorRequestsForVendor handles GET /api/vendors/:id/vendor-requests —
// lists a vendor's own requests so the dashboard can offer them as
// candidates in the manual-association picker (PRD 2.1 §5/§11).
func (h *RoutingHandlers) VendorRequestsForVendor(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	vrs, err := h.routing.ListByVendor(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list vendor's requests")
		return
	}
	writeJSON(w, http.StatusOK, toVendorRequestDTOs(vrs))
}

// GetVendorResponse handles GET /api/vendor-responses/:id
func (h *RoutingHandlers) GetVendorResponse(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	resp, err := h.routing.FindResponseByID(r.Context(), id)
	if err != nil || resp == nil {
		writeError(w, http.StatusNotFound, "vendor response not found")
		return
	}
	writeJSON(w, http.StatusOK, toResponseDTO(resp))
}

type patchVendorResponseBody struct {
	Status    *string `json:"status"`
	Product   *string `json:"product"`
	Variant   *string `json:"variant"`
	Condition *string `json:"condition"`
	Price     *int64  `json:"price"`
	Quantity  *int    `json:"quantity"`
	Location  *string `json:"location"`
	Notes     *string `json:"notes"`
}

// PatchVendorResponse handles PATCH /api/vendor-responses/:id — operator
// correction of an extracted vendor response (PRD 2 §38, PRD 2.1 §12).
// Per PRD 2.1 §6, if the audit trail for this correction cannot be
// written, the correction is reported as a failure rather than a
// silent-partial success.
func (h *RoutingHandlers) PatchVendorResponse(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}

	var body patchVendorResponseBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	fields := routing.ResponseFields{
		Product: body.Product, Variant: body.Variant, Condition: body.Condition,
		Price: body.Price, Quantity: body.Quantity, Location: body.Location, Notes: body.Notes,
	}
	if body.Status != nil {
		st := routing.ResponseStatus(*body.Status)
		fields.Status = &st
	}

	updated, err := h.service.CorrectResponse(r.Context(), id, fields, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "correction failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, toResponseDTO(updated))
}

type associateResponseBody struct {
	VendorRequestID string `json:"vendor_request_id"`
}

// AssociateResponse handles POST /api/vendor-responses/:id/associate —
// the operator's manual-association action for an unmatched or
// wrongly-matched response (PRD 2.1 §5).
func (h *RoutingHandlers) AssociateResponse(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}

	var body associateResponseBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	vendorRequestID, err := uuid.Parse(body.VendorRequestID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid vendor_request_id")
		return
	}

	updated, err := h.service.AssociateResponse(r.Context(), id, vendorRequestID, nil)
	if errors.Is(err, routing.ErrCrossVendorAssociation) {
		writeError(w, http.StatusConflict, "a response can only be associated with a request sent to the same vendor")
		return
	}
	if errors.Is(err, routing.ErrNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "association failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, toResponseDTO(updated))
}
