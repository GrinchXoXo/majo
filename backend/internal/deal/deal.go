// Package deal is Majo's follow-up on a buyer's choice (pilot batch item 5).
//
// In the pilot, buyers pay vendors directly: Majo introduces them at the
// moment the buyer chooses an offer (the transaction), and some hours later
// asks both "did you buy / did you sell?". Two YESes make a CONFIRMED deal,
// numbered in order of confirmation; the first FreeDeals are free (Stage A).
// The buyer then rates the seller 1-5. Disagreements and low ratings reach
// the operator. Nothing here moves money.
package deal

import (
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/deal/answer"
)

// Status of a deal.
type Status string

const (
	StatusPending    Status = "PENDING"     // waiting to ask
	StatusAsked      Status = "ASKED"       // asked; waiting for answers
	StatusConfirmed  Status = "CONFIRMED"   // both said YES
	StatusBuyerOnly  Status = "BUYER_ONLY"  // buyer YES; the vendor never answered
	StatusVendorOnly Status = "VENDOR_ONLY" // vendor YES; the buyer never answered
	StatusDeclined   Status = "DECLINED"    // no sale (a NO, and no YES against it)
	StatusDisputed   Status = "DISPUTED"    // one YES, one NO
	StatusNoAnswer   Status = "NO_ANSWER"   // nobody answered in time
)

// Answers (defined in the leaf package deal/answer, which the buyer and
// vendor runtimes import without importing this package).
const (
	Yes    = answer.Yes
	No     = answer.No
	NotYet = answer.NotYet
)

// ParseAnswer / ParseRating: see package answer.
var (
	ParseAnswer = answer.Parse
	ParseRating = answer.ParseRating
)

// Party.
const (
	Buyer  = "BUYER"
	Vendor = "VENDOR"
)

// Deal is one follow-up.
type Deal struct {
	ID               uuid.UUID  `json:"id"`
	TransactionID    uuid.UUID  `json:"transaction_id"`
	BuyerRequestID   uuid.UUID  `json:"buyer_request_id"`
	BuyerID          uuid.UUID  `json:"buyer_id"`
	VendorID         uuid.UUID  `json:"vendor_id"`
	ConversationID   uuid.UUID  `json:"conversation_id"`
	VendorRequestID  *uuid.UUID `json:"vendor_request_id,omitempty"`
	Item             string     `json:"item"`
	Reference        string     `json:"reference"`
	Amount           *int64     `json:"amount,omitempty"`
	Status           Status     `json:"status"`
	AskAt            time.Time  `json:"ask_at"`
	AskAttempts      int        `json:"ask_attempts"`
	BuyerAskedAt     *time.Time `json:"buyer_asked_at,omitempty"`
	VendorAskedAt    *time.Time `json:"vendor_asked_at,omitempty"`
	BuyerUnreachable bool       `json:"buyer_unreachable"`
	VendorUnreach    bool       `json:"vendor_unreachable"`
	BuyerAnswer      *string    `json:"buyer_answer,omitempty"`
	BuyerAnsweredAt  *time.Time `json:"buyer_answered_at,omitempty"`
	VendorAnswer     *string    `json:"vendor_answer,omitempty"`
	VendorAnsweredAt *time.Time `json:"vendor_answered_at,omitempty"`
	ConfirmedAt      *time.Time `json:"confirmed_at,omitempty"`
	DealNumber       *int       `json:"deal_number,omitempty"`
	Free             bool       `json:"free"`
	BuyerRating      *int       `json:"buyer_rating,omitempty"`
	BuyerRatedAt     *time.Time `json:"buyer_rated_at,omitempty"`
	ClosedAt         *time.Time `json:"closed_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	// Joined for display (not stored on the row).
	VendorName string `json:"vendor_name,omitempty"`
}

// Resolve is the deal status from the two answers. final=true when no more
// answers will be waited for (the answer window closed): a missing answer
// then counts as no answer. NOT_YET counts as no answer yet.
func Resolve(buyer, vendor *string, final bool) Status {
	b, v := val(buyer), val(vendor)
	switch {
	case b == Yes && v == Yes:
		return StatusConfirmed
	case (b == Yes && v == No) || (b == No && v == Yes):
		return StatusDisputed
	case b == No && v == No:
		return StatusDeclined
	}
	if !final {
		return StatusAsked
	}
	switch {
	case b == Yes:
		return StatusBuyerOnly
	case v == Yes:
		return StatusVendorOnly
	case b == No || v == No:
		return StatusDeclined
	}
	return StatusNoAnswer
}

func val(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Terminal: no further answer changes the outcome.
func (s Status) Terminal() bool {
	switch s {
	case StatusConfirmed, StatusDisputed, StatusDeclined:
		return true
	}
	return false
}
