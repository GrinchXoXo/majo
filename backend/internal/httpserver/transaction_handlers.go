package httpserver

import (
	"net/http"

	"github.com/middleman/backend/internal/transaction"
)

// TransactionHandlers exposes the transaction boundary (PRD 3 §21/§30) —
// read-only in Phase 3; no payment execution.
type TransactionHandlers struct {
	repo transaction.Repository
}

func NewTransactionHandlers(repo transaction.Repository) *TransactionHandlers {
	return &TransactionHandlers{repo: repo}
}

type transactionDTO struct {
	ID               string `json:"id"`
	BuyerSelectionID string `json:"buyer_selection_id"`
	BuyerRequestID   string `json:"buyer_request_id"`
	OfferID          string `json:"offer_id"`
	VendorID         string `json:"vendor_id"`
	Status           string `json:"status"`
	PaymentStatus    string `json:"payment_status"`
	FulfilmentStatus string `json:"fulfilment_status"`
	GrossAmount      int64  `json:"gross_amount"`
	PlatformFee      int64  `json:"platform_fee"`
	VendorAmount     int64  `json:"vendor_amount"`
	Currency         string `json:"currency"`
	CreatedAt        string `json:"created_at"`
}

// Get handles GET /api/transactions/:id
func (h *TransactionHandlers) Get(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}
	t, err := h.repo.FindTransactionByID(r.Context(), id)
	if err != nil || t == nil {
		writeError(w, http.StatusNotFound, "transaction not found")
		return
	}
	writeJSON(w, http.StatusOK, transactionDTO{
		ID: t.ID.String(), BuyerSelectionID: t.BuyerSelectionID.String(), BuyerRequestID: t.BuyerRequestID.String(),
		OfferID: t.OfferID.String(), VendorID: t.VendorID.String(),
		Status: string(t.Status), PaymentStatus: string(t.PaymentStatus), FulfilmentStatus: string(t.FulfilmentStatus),
		GrossAmount: t.GrossAmount, PlatformFee: t.PlatformFee, VendorAmount: t.VendorAmount, Currency: t.Currency,
		CreatedAt: t.CreatedAt.Format(timeFormat),
	})
}
