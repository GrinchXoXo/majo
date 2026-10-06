package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/marketplace"
	"github.com/middleman/backend/internal/offer"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/transaction"
)

// OfferHandlers implements the buyer-offer API from PRD 3 §15-20.
//
// Phase 3.1: GET endpoints are strictly read-only. Building and delivering
// offers happens automatically when vendor replies arrive (routing ->
// marketplace.OfferDelivery); POST /offers/deliver is the explicit operator
// action to retry a failed delivery.
type OfferHandlers struct {
	offers   offer.Repository
	requests request.Repository
	txSvc    *transaction.Service
	txRepo   transaction.Repository
	delivery *marketplace.OfferDelivery
}

func NewOfferHandlers(offers offer.Repository, requests request.Repository, txSvc *transaction.Service, txRepo transaction.Repository, delivery *marketplace.OfferDelivery) *OfferHandlers {
	return &OfferHandlers{offers: offers, requests: requests, txSvc: txSvc, txRepo: txRepo, delivery: delivery}
}

type offerDTO struct {
	ID                  string   `json:"id"`
	BuyerRequestID      string   `json:"buyer_request_id"`
	VendorID            string   `json:"vendor_id"`
	VendorResponseID    *string  `json:"vendor_response_id"`
	SupplyListingID     *string  `json:"supply_listing_id"`
	Type                string   `json:"type"`
	Status              string   `json:"status"`
	Product             *string  `json:"product"`
	Variant             *string  `json:"variant"`
	Condition           *string  `json:"condition"`
	Price               *int64   `json:"price"`
	Currency            string   `json:"currency"`
	Quantity            *int     `json:"quantity"`
	Location            *string  `json:"location"`
	RankingScore        int      `json:"ranking_score"`
	RankingReasons      []string `json:"ranking_reasons"`
	Confidence          float64  `json:"confidence"`
	CreatedAt           string   `json:"created_at"`
	PresentationOrdinal *int     `json:"presentation_ordinal"`
	SentAt              *string  `json:"sent_at"`
	ExpiresAt           *string  `json:"expires_at"`
	DeliveryAttempts    int      `json:"delivery_attempts"`
	LastDeliveryError   *string  `json:"last_delivery_error"`
}

func toOfferDTO(o *offer.Offer) offerDTO {
	dto := offerDTO{
		ID: o.ID.String(), BuyerRequestID: o.BuyerRequestID.String(), VendorID: o.VendorID.String(),
		Type: string(o.Type), Status: string(o.Status),
		Product: o.Product, Variant: o.Variant, Condition: o.Condition,
		Price: o.Price, Currency: o.Currency, Quantity: o.Quantity, Location: o.Location,
		RankingScore: o.RankingScore, RankingReasons: o.RankingReasons, Confidence: o.Confidence,
		CreatedAt:           o.CreatedAt.Format(timeFormat),
		PresentationOrdinal: o.PresentationOrdinal, DeliveryAttempts: o.DeliveryAttempts, LastDeliveryError: o.LastDeliveryError,
	}
	if o.VendorResponseID != nil {
		s := o.VendorResponseID.String()
		dto.VendorResponseID = &s
	}
	if o.SupplyListingID != nil {
		s := o.SupplyListingID.String()
		dto.SupplyListingID = &s
	}
	if o.SentAt != nil {
		s := o.SentAt.Format(timeFormat)
		dto.SentAt = &s
	}
	if o.ExpiresAt != nil {
		s := o.ExpiresAt.Format(timeFormat)
		dto.ExpiresAt = &s
	}
	return dto
}

// List handles GET /api/requests/:id/offers — read-only (Phase 3.1: no
// longer builds offers as a side effect).
func (h *OfferHandlers) List(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	offers, err := h.offers.ListByBuyerRequest(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list offers")
		return
	}
	dtos := make([]offerDTO, 0, len(offers))
	for _, o := range offers {
		dtos = append(dtos, toOfferDTO(o))
	}
	writeJSON(w, http.StatusOK, dtos)
}

// BuyerMessagePreview handles GET /api/requests/:id/offers/preview —
// read-only: renders the offers the buyer has been (or would be) shown,
// with no internal scores/metadata.
func (h *OfferHandlers) BuyerMessagePreview(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	offers, err := h.offers.ListByBuyerRequest(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list offers")
		return
	}
	req, err := h.requests.FindByID(r.Context(), id)
	if err != nil || req == nil {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	var shown []*offer.Offer
	highest := 0
	for _, o := range offers {
		if o.Status == offer.StatusWithdrawn || o.Status == offer.StatusDuplicate {
			continue
		}
		cp := *o
		if cp.PresentationOrdinal == nil {
			n := highest + 1 // preview numbering for not-yet-delivered offers
			cp.PresentationOrdinal = &n
		}
		if *cp.PresentationOrdinal > highest {
			highest = *cp.PresentationOrdinal
		}
		shown = append(shown, &cp)
	}
	msg := "No offers yet."
	if len(shown) > 0 {
		msg = offer.FormatOfferDelivery(marketplace.SummaryOf(req), shown, highest, false)
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": msg})
}

// Deliver handles POST /api/requests/:id/offers/deliver — explicit,
// idempotent operator action: build any newly eligible offers and deliver
// every not-yet-delivered one (e.g. retry after OFFER_SEND_FAILED).
// Already-delivered offers are never re-sent.
func (h *OfferHandlers) Deliver(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	res, err := h.delivery.DeliverPendingOffers(r.Context(), id)
	switch {
	case errors.Is(err, request.ErrNotConfirmed):
		writeError(w, http.StatusConflict, "request must be CONFIRMED before offers can be delivered")
	case errors.Is(err, marketplace.ErrDeliveryFailed):
		writeError(w, http.StatusBadGateway, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, "delivery failed: "+err.Error())
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

type selectOfferBody struct {
	OfferID string `json:"offer_id"`
	BuyerID string `json:"buyer_id"`
}

// Select handles POST /api/requests/:id/select — operator override of the
// buyer-selection -> transaction-shell flow (PRD 3 §20-21). The normal path
// is the buyer replying on WhatsApp. Same guards apply: confirmed request,
// buyer owns it, at most one selection, offer still selectable. Never
// implies payment.
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

	res, err := h.txSvc.Select(r.Context(), buyerRequestID, offerID, buyerID, transaction.SourceOperator)
	switch {
	case errors.Is(err, request.ErrNotConfirmed):
		writeError(w, http.StatusConflict, "request must be CONFIRMED before an offer can be selected")
		return
	case errors.Is(err, transaction.ErrNotRequestOwner):
		writeError(w, http.StatusForbidden, "buyer does not own this request")
		return
	case errors.Is(err, transaction.ErrAlreadySelected):
		writeError(w, http.StatusConflict, "a different offer was already selected for this request")
		return
	case errors.Is(err, transaction.ErrOfferNotSelectable):
		writeError(w, http.StatusConflict, "offer is no longer selectable (withdrawn, expired or already decided)")
		return
	case errors.Is(err, transaction.ErrOfferNotFound):
		writeError(w, http.StatusNotFound, "offer not found for this request")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "selection failed: "+err.Error())
		return
	}

	status := http.StatusCreated
	if res.AlreadyRecorded {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"selection_id":     res.Selection.ID.String(),
		"transaction_id":   res.Transaction.ID.String(),
		"payment_status":   string(res.Transaction.PaymentStatus), // always UNPAID here — no payment has occurred
		"gross_amount":     res.Transaction.GrossAmount,
		"already_recorded": res.AlreadyRecorded,
	})
}

// Selection handles GET /api/requests/:id/selection — the request's ACTIVE
// buyer selection and its transaction shell, for the dashboard.
func (h *OfferHandlers) Selection(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	sel, err := h.txRepo.FindSelectionByBuyerRequest(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load selection")
		return
	}
	if sel == nil {
		writeJSON(w, http.StatusOK, map[string]any{"selection": nil, "transaction": nil})
		return
	}
	tx, err := h.txRepo.FindTransactionBySelection(r.Context(), sel.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load transaction")
		return
	}
	out := map[string]any{
		"selection": map[string]any{
			"id": sel.ID.String(), "offer_id": sel.OfferID.String(), "buyer_id": sel.BuyerID.String(),
			"status": string(sel.Status), "created_at": sel.CreatedAt.Format(timeFormat),
		},
		"transaction": nil,
	}
	if tx != nil {
		out["transaction"] = map[string]any{
			"id": tx.ID.String(), "status": string(tx.Status), "payment_status": string(tx.PaymentStatus),
			"fulfilment_status": string(tx.FulfilmentStatus), "gross_amount": tx.GrossAmount,
			"platform_fee": tx.PlatformFee, "vendor_amount": tx.VendorAmount, "currency": tx.Currency,
			"created_at": tx.CreatedAt.Format(timeFormat),
		}
	}
	writeJSON(w, http.StatusOK, out)
}
