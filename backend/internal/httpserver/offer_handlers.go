package httpserver

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/offer"
	"github.com/middleman/backend/internal/transaction"
)

// OfferHandlers implements the buyer-offer API from PRD 3 §15-20.
type OfferHandlers struct {
	offers  offer.Repository
	service *offer.Service
	txSvc   *transaction.Service
}

func NewOfferHandlers(offers offer.Repository, service *offer.Service, txSvc *transaction.Service) *OfferHandlers {
	return &OfferHandlers{offers: offers, service: service, txSvc: txSvc}
}

type offerDTO struct {
	ID               string   `json:"id"`
	BuyerRequestID   string   `json:"buyer_request_id"`
	VendorID         string   `json:"vendor_id"`
	VendorResponseID *string  `json:"vendor_response_id"`
	SupplyListingID  *string  `json:"supply_listing_id"`
	Type             string   `json:"type"`
	Status           string   `json:"status"`
	Product          *string  `json:"product"`
	Variant          *string  `json:"variant"`
	Condition        *string  `json:"condition"`
	Price            *int64   `json:"price"`
	Currency         string   `json:"currency"`
	Quantity         *int     `json:"quantity"`
	Location         *string  `json:"location"`
	RankingScore     int      `json:"ranking_score"`
	RankingReasons   []string `json:"ranking_reasons"`
	Confidence       float64  `json:"confidence"`
	CreatedAt        string   `json:"created_at"`
}

func toOfferDTO(o *offer.Offer) offerDTO {
	dto := offerDTO{
		ID: o.ID.String(), BuyerRequestID: o.BuyerRequestID.String(), VendorID: o.VendorID.String(),
		Type: string(o.Type), Status: string(o.Status),
		Product: o.Product, Variant: o.Variant, Condition: o.Condition,
		Price: o.Price, Currency: o.Currency, Quantity: o.Quantity, Location: o.Location,
		RankingScore: o.RankingScore, RankingReasons: o.RankingReasons, Confidence: o.Confidence,
		CreatedAt: o.CreatedAt.Format(timeFormat),
	}
	if o.VendorResponseID != nil {
		s := o.VendorResponseID.String()
		dto.VendorResponseID = &s
	}
	if o.SupplyListingID != nil {
		s := o.SupplyListingID.String()
		dto.SupplyListingID = &s
	}
	return dto
}

// BuildAndList handles GET /api/requests/:id/offers — builds any newly
// eligible offers, then returns the full ranked list (PRD 3 §15-19).
func (h *OfferHandlers) BuildAndList(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	offers, err := h.service.BuildForBuyerRequest(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build offers: "+err.Error())
		return
	}
	dtos := make([]offerDTO, 0, len(offers))
	for _, o := range offers {
		dtos = append(dtos, toOfferDTO(o))
	}
	writeJSON(w, http.StatusOK, dtos)
}

// BuyerMessagePreview handles GET /api/requests/:id/offers/preview — the
// exact buyer-facing text an operator would send (PRD 3 §19/§34), with
// no internal scores/metadata.
func (h *OfferHandlers) BuyerMessagePreview(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	offers, err := h.service.BuildForBuyerRequest(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build offers: "+err.Error())
		return
	}
	label, err := h.service.BuyerRequestLabel(r.Context(), id)
	if err != nil {
		label = "your request"
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": offer.FormatBuyerMessage(label, offers)})
}

type selectOfferBody struct {
	OfferID string `json:"offer_id"`
	BuyerID string `json:"buyer_id"`
}

// Select handles POST /api/requests/:id/select — the buyer-selection ->
// transaction-shell flow (PRD 3 §20-21). Never implies payment.
func (h *OfferHandlers) Select(w http.ResponseWriter, r *http.Request) {
	buyerRequestID, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	var body selectOfferBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	offerID, err := uuid.Parse(body.OfferID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid offer_id")
		return
	}
	buyerID, err := uuid.Parse(body.BuyerID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid buyer_id")
		return
	}

	selection, tx, err := h.txSvc.SelectOffer(r.Context(), buyerRequestID, offerID, buyerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "selection failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"selection_id":   selection.ID.String(),
		"transaction_id": tx.ID.String(),
		"payment_status": string(tx.PaymentStatus), // always UNPAID here — no payment has occurred
		"gross_amount":   tx.GrossAmount,
	})
}
