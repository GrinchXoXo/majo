// Package ai defines the AIProvider boundary between the Go backend and the
// Python extraction service. Per PRD 0.2 §5 and PRD 0 FINAL §22: the Go
// application must never know which LLM/provider is used underneath — it
// only knows this interface.
package ai

import "context"

// ExtractionRequest is what the Go backend sends to the AI layer.
type ExtractionRequest struct {
	Text          string `json:"text"`
	ConversationID string `json:"conversation_id,omitempty"`
}

// ExtractionResult mirrors the placeholder contract from PRD 0 FINAL §22.
// Fields are pointers/omitted-friendly because the AI may not be confident
// about every field — the system must preserve uncertainty rather than
// invent facts (PRD 0.3 §14).
type ExtractionResult struct {
	Category     *string  `json:"category"`
	Product      *string  `json:"product"`
	Variant      *string  `json:"variant"`
	BudgetMin    *int64   `json:"budget_min"`
	BudgetMax    *int64   `json:"budget_max"`
	Location     *string  `json:"location"`
	Condition    *string  `json:"condition"`
	Quantity     *int     `json:"quantity"`
	Urgency      *string  `json:"urgency"`
	Preferences  []string `json:"preferences"`
	Confidence   float64  `json:"confidence"`
	// Phase 4C hardening: interpretation signals (the AI service reports
	// them; the deterministic policy decides). Empty when not provided.
	OtherProducts []string `json:"other_products,omitempty"`
	Ambiguities   []string `json:"ambiguities,omitempty"`
}

// Provider is the interface all business logic depends on.
// InitialLLMProvider (http_provider.go) is the only implementation for now.
type Provider interface {
	Extract(ctx context.Context, req ExtractionRequest) (*ExtractionResult, error)
	ExtractSupply(ctx context.Context, text string) (*SupplyExtractionResult, error)
	ExtractVendorResponse(ctx context.Context, text string) (*VendorResponseExtractionResult, error)
	Healthy(ctx context.Context) error
}

// SupplyExtractionResult mirrors the Python /extract-supply contract
// (PRD 2 §13-14).
type SupplyExtractionResult struct {
	Category           *string `json:"category"`
	Product            *string `json:"product"`
	Variant            *string `json:"variant"`
	Condition          *string `json:"condition"`
	Price              *int64  `json:"price"`
	Quantity           *int    `json:"quantity"`
	Location           *string `json:"location"`
	AvailabilityStatus string  `json:"availability_status"`
	Confidence         float64 `json:"confidence"`
}

// VendorResponseExtractionResult mirrors the Python /extract-vendor-response
// contract (PRD 2 §34-38, extended by PRD 2.1 §1/§8 with ReviewState and
// ExtractionMethod).
type VendorResponseExtractionResult struct {
	Status           string  `json:"status"`
	ReviewState      string  `json:"review_state"`
	ExtractionMethod string  `json:"extraction_method"`
	Product          *string `json:"product"`
	Variant          *string `json:"variant"`
	Condition        *string `json:"condition"`
	Price            *int64  `json:"price"`
	Quantity         *int    `json:"quantity"`
	Location         *string `json:"location"`
	Notes            *string `json:"notes"`
	Confidence       float64 `json:"confidence"`
}
