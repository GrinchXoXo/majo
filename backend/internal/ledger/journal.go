package ledger

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/monetization"
)

// Chart of accounts. Codes are stable identifiers; one account per code.

// CashAccount: money collected by a payment provider, not yet settled to
// Majo's bank (the provider's clearing balance).
func CashAccount(provider string) Account {
	p := slug(provider)
	return Account{Code: "majo:cash:" + p, Name: "Payments held at " + provider, Type: Asset, OwnerType: OwnerMajo, Currency: NGN}
}

// BankAccount: Majo's own bank account.
func BankAccount() Account {
	return Account{Code: "majo:bank", Name: "Majo bank account", Type: Asset, OwnerType: OwnerMajo, Currency: NGN}
}

// RevenueAccount for a revenue line.
func RevenueAccount(l monetization.Line) Account {
	return Account{Code: "majo:revenue:" + strings.ToLower(string(l)), Name: "Revenue: " + string(l), Type: Revenue, OwnerType: OwnerMajo, Currency: NGN}
}

// FeesAccount: payment provider fees Majo pays.
func FeesAccount() Account {
	return Account{Code: "majo:expense:payment_fees", Name: "Payment provider fees", Type: Expense, OwnerType: OwnerMajo, Currency: NGN}
}

// CreditsAccount: a vendor's unused, prepaid lead credits. Majo owes the
// service, not money (deferred revenue), so these are not customer funds.
func CreditsAccount(vendorID uuid.UUID) Account {
	id := vendorID
	return Account{Code: "majo:deferred:lead_credits:" + vendorID.String(), Name: "Prepaid lead credits", Type: Liability,
		OwnerType: OwnerVendor, OwnerID: &id, Currency: NGN}
}

// EscrowAccount: buyers' deposits held by the licensed partner (customer funds).
func EscrowAccount(partner string) Account {
	return Account{Code: "customer:escrow:" + slug(partner), Name: "Deposits held by " + partner, Type: Asset, OwnerType: OwnerProvider,
		Currency: NGN, HoldsCustomerFunds: true}
}

// BuyerDepositAccount: what is owed back to a buyer (customer funds).
func BuyerDepositAccount(buyerID uuid.UUID) Account {
	id := buyerID
	return Account{Code: "customer:buyer_deposits:" + buyerID.String(), Name: "Buyer deposit", Type: Liability, OwnerType: OwnerBuyer,
		OwnerID: &id, Currency: NGN, HoldsCustomerFunds: true}
}

// VendorPayableAccount: what is owed to a vendor from a deposit (customer funds).
func VendorPayableAccount(vendorID uuid.UUID) Account {
	id := vendorID
	return Account{Code: "customer:vendor_payable:" + vendorID.String(), Name: "Owed to vendor", Type: Liability, OwnerType: OwnerVendor,
		OwnerID: &id, Currency: NGN, HoldsCustomerFunds: true}
}

func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

func entry(key, kind string, line *monetization.Line, desc string, at time.Time, by string, accounts ...Account) *Entry {
	return &Entry{IdempotencyKey: key, Kind: kind, RevenueLine: line, Description: desc, OccurredAt: at, CreatedBy: by, Accounts: accounts}
}

func dr(a Account, kobo int64) Posting {
	return Posting{Account: a.Code, AmountKobo: kobo, Currency: a.Currency}
}
func cr(a Account, kobo int64) Posting {
	return Posting{Account: a.Code, AmountKobo: -kobo, Currency: a.Currency}
}

func lineP(l monetization.Line) *monetization.Line { return &l }

// --- Journal: one function per business event (docs/architecture/ledger.md) ---

// LeadCreditPurchase: a vendor pays for a bundle of credits. Cash at the
// provider rises; Majo owes the vendor that many introductions.
func LeadCreditPurchase(vendorID uuid.UUID, credits int, amountKobo int64, provider, paymentRef string, at time.Time) *Entry {
	cash, owed := CashAccount(provider), CreditsAccount(vendorID)
	e := entry("lead_credit_purchase:"+paymentRef, "lead_credit_purchase", lineP(monetization.LeadCredits),
		fmt.Sprintf("%d lead credits bought", credits), at, "payments", cash, owed)
	e.VendorID, e.PaymentRef = &vendorID, paymentRef
	e.Postings = []Posting{dr(cash, amountKobo), cr(owed, amountKobo)}
	return e
}

// LeadCreditSpend: one credit is used when a buyer picks the vendor and
// contact details are exchanged; that is when Majo earns it.
func LeadCreditSpend(vendorID, dealID uuid.UUID, valueKobo int64, at time.Time) *Entry {
	owed, rev := CreditsAccount(vendorID), RevenueAccount(monetization.LeadCredits)
	e := entry("lead_credit_spend:"+dealID.String(), "lead_credit_spend", lineP(monetization.LeadCredits),
		"Lead credit used for an introduction", at, "deal", owed, rev)
	e.VendorID, e.DealID = &vendorID, &dealID
	e.Postings = []Posting{dr(owed, valueKobo), cr(rev, valueKobo)}
	return e
}

// ServicePayment: a one-off or monthly payment for Majo's own service:
// VENDOR_PRO, VERIFIED_BADGE, RUSH_REQUEST or SPONSORED.
func ServicePayment(l monetization.Line, amountKobo int64, provider, paymentRef string, at time.Time) (*Entry, error) {
	switch l {
	case monetization.VendorPro, monetization.VerifiedBadge, monetization.RushRequest, monetization.Sponsored:
	default:
		return nil, fmt.Errorf("ledger: %s is not a service payment", l)
	}
	cash, rev := CashAccount(provider), RevenueAccount(l)
	e := entry(strings.ToLower(string(l))+":"+paymentRef, strings.ToLower(string(l))+"_payment", lineP(l),
		"Payment for "+string(l), at, "payments", cash, rev)
	e.PaymentRef = paymentRef
	e.Postings = []Posting{dr(cash, amountKobo), cr(rev, amountKobo)}
	return e, nil
}

// ProviderSettlement: the provider pays out to Majo's bank, minus its fee.
func ProviderSettlement(provider, settlementRef string, grossKobo, feeKobo int64, at time.Time) *Entry {
	cash, bank, fees := CashAccount(provider), BankAccount(), FeesAccount()
	e := entry("settlement:"+slug(provider)+":"+settlementRef, "provider_settlement", nil,
		"Settlement from "+provider, at, "payments", cash, bank, fees)
	e.PaymentRef = settlementRef
	e.Postings = []Posting{dr(bank, grossKobo-feeKobo), cr(cash, grossKobo)}
	if feeKobo > 0 {
		e.Postings = append(e.Postings, dr(fees, feeKobo))
	}
	return e
}

// SplitFee: with split payments the vendor's share settles straight to the
// vendor; only Majo's fee ever reaches Majo, so only the fee is booked.
func SplitFee(dealID uuid.UUID, feeKobo int64, provider, paymentRef string, at time.Time) *Entry {
	cash, rev := CashAccount(provider), RevenueAccount(monetization.SplitPayments)
	e := entry("split_fee:"+paymentRef, "split_fee", lineP(monetization.SplitPayments), "Majo fee on a split payment", at, "payments", cash, rev)
	e.DealID, e.PaymentRef = &dealID, paymentRef
	e.Postings = []Posting{dr(cash, feeKobo), cr(rev, feeKobo)}
	return e
}

// EscrowDeposit (Stage C): the buyer's deposit is held by the partner; Majo
// owes it to the buyer until release or refund.
func EscrowDeposit(buyerID, dealID uuid.UUID, amountKobo int64, partner, holdRef string, at time.Time) *Entry {
	held, owed := EscrowAccount(partner), BuyerDepositAccount(buyerID)
	e := entry("escrow_deposit:"+holdRef, "escrow_deposit", lineP(monetization.Escrow), "Buyer deposit held by "+partner, at, "payments", held, owed)
	e.BuyerID, e.DealID, e.PaymentRef = &buyerID, &dealID, holdRef
	e.Postings = []Posting{dr(held, amountKobo), cr(owed, amountKobo)}
	return e
}

// EscrowRelease (Stage C): both sides confirmed; the deposit is now owed to
// the vendor, minus Majo's commission, which Majo earns.
func EscrowRelease(buyerID, vendorID, dealID uuid.UUID, amountKobo, commissionKobo int64, holdRef string, at time.Time) (*Entry, error) {
	if commissionKobo < 0 || commissionKobo >= amountKobo {
		return nil, fmt.Errorf("ledger: commission %d out of range for deposit %d", commissionKobo, amountKobo)
	}
	owedBuyer, owedVendor, rev := BuyerDepositAccount(buyerID), VendorPayableAccount(vendorID), RevenueAccount(monetization.Escrow)
	e := entry("escrow_release:"+holdRef, "escrow_release", lineP(monetization.Escrow), "Deposit released to vendor", at, "deal", owedBuyer, owedVendor, rev)
	e.BuyerID, e.VendorID, e.DealID, e.PaymentRef = &buyerID, &vendorID, &dealID, holdRef
	e.Postings = []Posting{dr(owedBuyer, amountKobo), cr(owedVendor, amountKobo-commissionKobo)}
	if commissionKobo > 0 {
		e.Postings = append(e.Postings, cr(rev, commissionKobo))
	}
	return e, nil
}

// EscrowPayout (Stage C): the partner pays the vendor and Majo's commission
// out of the held deposit.
func EscrowPayout(vendorID uuid.UUID, vendorKobo, commissionKobo int64, partner, payoutRef string, at time.Time) *Entry {
	held, owedVendor, cash := EscrowAccount(partner), VendorPayableAccount(vendorID), CashAccount(partner)
	e := entry("escrow_payout:"+payoutRef, "escrow_payout", lineP(monetization.Escrow), "Partner paid out the deposit", at, "payments", held, owedVendor, cash)
	e.VendorID, e.PaymentRef = &vendorID, payoutRef
	e.Postings = []Posting{dr(owedVendor, vendorKobo), cr(held, vendorKobo+commissionKobo)}
	if commissionKobo > 0 {
		e.Postings = append(e.Postings, dr(cash, commissionKobo))
	}
	return e
}

// EscrowRefund (Stage C): the deposit goes back to the buyer.
func EscrowRefund(buyerID, dealID uuid.UUID, amountKobo int64, partner, refundRef string, at time.Time) *Entry {
	held, owed := EscrowAccount(partner), BuyerDepositAccount(buyerID)
	e := entry("escrow_refund:"+refundRef, "escrow_refund", lineP(monetization.Escrow), "Deposit returned to buyer", at, "payments", held, owed)
	e.BuyerID, e.DealID, e.PaymentRef = &buyerID, &dealID, refundRef
	e.Postings = []Posting{dr(owed, amountKobo), cr(held, amountKobo)}
	return e
}
