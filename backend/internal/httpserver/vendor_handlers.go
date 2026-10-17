package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/supply"
	"github.com/middleman/backend/internal/vendor"
)

// VendorHandlers implements the operator-facing vendor management API
// from PRD 2 §6, §51-53.
type VendorHandlers struct {
	vendors  vendor.Repository
	supplies supply.Repository
	events   event.Writer
}

func NewVendorHandlers(vendors vendor.Repository, supplies supply.Repository, events event.Writer) *VendorHandlers {
	return &VendorHandlers{vendors: vendors, supplies: supplies, events: events}
}

type vendorDTO struct {
	ID                 string   `json:"id"`
	BusinessName       string   `json:"business_name"`
	WhatsAppExternalID string   `json:"whatsapp_external_id"`
	Phone              string   `json:"phone"`
	Status             string   `json:"status"`
	PrimaryLocation    string   `json:"primary_location"`
	Notes              string   `json:"notes"`
	Categories         []string `json:"categories"`
	ServiceAreas       []string `json:"service_areas"`
	RequestsReceived   int      `json:"requests_received"`
	ResponsesReceived  int      `json:"responses_received"`
	CreatedAt          string   `json:"created_at"`
	Warnings           []string `json:"warnings,omitempty"`
}

func toVendorDTO(v *vendor.Vendor) vendorDTO {
	return vendorDTO{
		ID: v.ID.String(), BusinessName: v.BusinessName, WhatsAppExternalID: v.WhatsAppExternalID,
		Phone: v.Phone, Status: string(v.Status), PrimaryLocation: v.PrimaryLocation, Notes: v.Notes,
		Categories: v.Categories, ServiceAreas: v.ServiceAreas,
		RequestsReceived: v.RequestsReceived, ResponsesReceived: v.ResponsesReceived,
		CreatedAt: v.CreatedAt.Format(timeFormat),
	}
}

type createVendorBody struct {
	BusinessName       string   `json:"business_name"`
	WhatsAppExternalID string   `json:"whatsapp_external_id"`
	Phone              string   `json:"phone"`
	PrimaryLocation    string   `json:"primary_location"`
	Notes              string   `json:"notes"`
	Categories         []string `json:"categories"`
	ServiceAreas       []string `json:"service_areas"`
}

// Create handles POST /api/vendors. New vendors start PENDING per PRD 2
// §6; the operator activates them explicitly once vetted.
func (h *VendorHandlers) Create(w http.ResponseWriter, r *http.Request) {
	var body createVendorBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(body.BusinessName) == "" || body.WhatsAppExternalID == "" {
		writeError(w, http.StatusBadRequest, "business_name and whatsapp_external_id are required")
		return
	}
	// 01a audit: store the number the way WhatsApp reports it, or the
	// vendor's replies are never recognised as theirs.
	waID, err := vendor.NormalizeWhatsAppID(body.WhatsAppExternalID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if existing, _ := h.vendors.FindByWhatsAppID(r.Context(), waID); existing != nil {
		writeError(w, http.StatusConflict, "a vendor with this WhatsApp number already exists: "+existing.BusinessName)
		return
	}
	categories, unknown := vendor.NormalizeCategories(body.Categories)

	v := &vendor.Vendor{
		BusinessName:       strings.TrimSpace(body.BusinessName),
		WhatsAppExternalID: waID,
		Phone:              body.Phone,
		PrimaryLocation:    strings.Join(strings.Fields(body.PrimaryLocation), " "),
		Notes:              body.Notes,
		Status:             vendor.StatusPending,
		Categories:         categories,
		ServiceAreas:       vendor.NormalizeAreas(body.ServiceAreas),
	}

	created, err := h.vendors.Create(r.Context(), v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create vendor")
		return
	}

	if err := h.events.Write(r.Context(), &event.Event{
		EventType: event.VendorCreated, EntityType: "vendor", EntityID: &created.ID, ActorType: event.ActorOperator, ActorID: actorID(r),
	}); err != nil {
		// Non-fatal — vendor was created successfully.
	}

	dto := toVendorDTO(created)
	dto.Warnings = categoryWarnings(unknown)
	writeJSON(w, http.StatusCreated, dto)
}

// categoryWarnings explains categories no buyer request will carry.
func categoryWarnings(unknown []string) []string {
	if len(unknown) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("these categories match no buyer request: %s. Use: %s",
		strings.Join(unknown, ", "), strings.Join(vendor.KnownCategories, ", "))}
}

// List handles GET /api/vendors?status=ACTIVE&category=electronics
func (h *VendorHandlers) List(w http.ResponseWriter, r *http.Request) {
	var statusFilter *vendor.Status
	if s := r.URL.Query().Get("status"); s != "" {
		st := vendor.Status(s)
		statusFilter = &st
	}
	var categoryFilter *string
	if c := r.URL.Query().Get("category"); c != "" {
		categoryFilter = &c
	}

	vendors, err := h.vendors.List(r.Context(), statusFilter, categoryFilter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list vendors")
		return
	}

	dtos := make([]vendorDTO, 0, len(vendors))
	for _, v := range vendors {
		dtos = append(dtos, toVendorDTO(v))
	}
	writeJSON(w, http.StatusOK, dtos)
}

// Get handles GET /api/vendors/:id
func (h *VendorHandlers) Get(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	v, err := h.vendors.FindByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "vendor not found")
		return
	}
	writeJSON(w, http.StatusOK, toVendorDTO(v))
}

type patchVendorBody struct {
	BusinessName    *string  `json:"business_name"`
	Phone           *string  `json:"phone"`
	Status          *string  `json:"status"`
	PrimaryLocation *string  `json:"primary_location"`
	Notes           *string  `json:"notes"`
	Categories      []string `json:"categories"`
	ServiceAreas    []string `json:"service_areas"`
}

// Patch handles PATCH /api/vendors/:id — edit, activate, deactivate, block
// (PRD 2 §6).
func (h *VendorHandlers) Patch(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}

	var body patchVendorBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	categories, unknown := vendor.NormalizeCategories(body.Categories)
	fields := vendor.Fields{
		BusinessName: body.BusinessName, Phone: body.Phone, PrimaryLocation: body.PrimaryLocation,
		Notes: body.Notes, Categories: categories, ServiceAreas: vendor.NormalizeAreas(body.ServiceAreas),
	}
	if body.Status != nil {
		st := vendor.Status(strings.ToUpper(strings.TrimSpace(*body.Status)))
		if !vendor.ValidStatus(st) {
			writeError(w, http.StatusBadRequest, "status must be PENDING, ACTIVE, INACTIVE or BLOCKED")
			return
		}
		fields.Status = &st
		body.Status = (*string)(&st)
	}

	updated, err := h.vendors.Update(r.Context(), id, fields)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update vendor")
		return
	}

	if body.Status != nil {
		evtType := event.VendorUpdated
		if *body.Status == string(vendor.StatusActive) {
			evtType = event.VendorActivated
		} else if *body.Status == string(vendor.StatusInactive) || *body.Status == string(vendor.StatusBlocked) {
			evtType = event.VendorDeactivated
		}
		_ = h.events.Write(r.Context(), &event.Event{EventType: evtType, EntityType: "vendor", EntityID: &id, ActorType: event.ActorOperator, ActorID: actorID(r)})
	}

	dto := toVendorDTO(updated)
	dto.Warnings = categoryWarnings(unknown)
	writeJSON(w, http.StatusOK, dto)
}

type supplySignalDTO struct {
	ID                 string  `json:"id"`
	Category           *string `json:"category"`
	Product            *string `json:"product"`
	Variant            *string `json:"variant"`
	Condition          *string `json:"condition"`
	Price              *int64  `json:"price"`
	Quantity           *int    `json:"quantity"`
	Location           *string `json:"location"`
	AvailabilityStatus string  `json:"availability_status"`
	RawMessage         string  `json:"raw_message"`
	Confidence         float64 `json:"confidence"`
	CreatedAt          string  `json:"created_at"`
	ExpiresAt          string  `json:"expires_at"`
}

// Supply handles GET /api/vendors/:id/supply
func (h *VendorHandlers) Supply(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}

	signals, err := h.supplies.ListByVendor(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list supply")
		return
	}

	dtos := make([]supplySignalDTO, 0, len(signals))
	for _, s := range signals {
		dtos = append(dtos, supplySignalDTO{
			ID: s.ID.String(), Category: s.Category, Product: s.Product, Variant: s.Variant, Condition: s.Condition,
			Price: s.Price, Quantity: s.Quantity, Location: s.Location, AvailabilityStatus: string(s.AvailabilityStatus),
			RawMessage: s.RawMessage, Confidence: s.Confidence,
			CreatedAt: s.CreatedAt.Format(timeFormat), ExpiresAt: s.ExpiresAt.Format(timeFormat),
		})
	}
	writeJSON(w, http.StatusOK, dtos)
}
