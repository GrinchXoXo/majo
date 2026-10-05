// Package marketplace is the buyer-side application layer that closes the
// Phase 3.1 loop:
//
//	VendorResponse -> Offer -> delivered to buyer -> buyer reply ->
//	context resolution -> BuyerSelection -> Transaction
//
// It orchestrates existing domain services (request, routing, offer,
// transaction); it owns no tables and adds no business rules those
// services don't already enforce. Three pieces:
//
//   - OfferDelivery (delivery.go): builds eligible offers for a confirmed
//     request and delivers each one to the buyer exactly once. It is wired
//     as routing's ResponseListener, so it runs whenever a vendor reply is
//     recorded, corrected or associated — never because someone read an
//     endpoint.
//   - ContextResolver (resolver.go): the deterministic seam that decides
//     whether an inbound buyer message is a new request or an action
//     against an existing marketplace context. Phase 3.1 supports exactly
//     one action: offer selection by number. No LLM is involved.
//   - BuyerInbound (inbound.go): executes the resolution — the existing
//     new-request pipeline, or transaction.Service.Select — and replies.
//
// The AI service is never called on the selection path, and nothing here
// lets AI output invoke a mutating domain operation.
package marketplace

import (
	"context"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/transaction"
)

// Messenger is the minimal outbound surface this package needs (mirrors the
// local MessagingProvider interfaces in request/ and routing/).
type Messenger interface {
	SendText(ctx context.Context, to, body string) error
}

// SelectionReader is the read side of the transaction repository the loop
// needs (has this request already been decided?).
type SelectionReader interface {
	FindSelectionByBuyerRequest(ctx context.Context, buyerRequestID uuid.UUID) (*transaction.Selection, error)
}
