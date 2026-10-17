package deal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/autonomy"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/deal/answer"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/jobs"
	"github.com/middleman/backend/internal/offer"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/transaction"
	"github.com/middleman/backend/internal/vendor"
)

// Clarification kinds (the answers come back through the buyer and vendor
// runtimes, which hand them to this service).
const (
	KindBuyerConfirm  = answer.KindBuyerConfirm
	KindBuyerRating   = answer.KindBuyerRating
	KindVendorConfirm = answer.KindVendorConfirm
)

// Job types.
const (
	JobFollowUp = "DEAL_FOLLOWUP"
	JobClose    = "DEAL_CLOSE"
)

// Exception reasons.
const (
	ReasonDisputed  = "deal_disputed"
	ReasonLowRating = "low_rating"
)

// Config [PARAM]s.
type Config struct {
	Enabled       bool          // DEAL_FOLLOWUP (default on)
	FollowUpDelay time.Duration // DEAL_FOLLOWUP_DELAY (default 6h: inside the buyer's 24h window)
	AnswerTTL     time.Duration // DEAL_ANSWER_TTL (default 72h)
	FreeDeals     int           // FREE_DEALS (default 100)
	// ShareBuyerContact (INTRO_SHARE_BUYER_CONTACT): the vendor also gets the
	// buyer's WhatsApp link. Default false: the buyer gets the vendor's link
	// and makes contact. OWNER DECISION.
	ShareBuyerContact bool
	// MaxAskAttempts bounds re-scheduling when a question cannot be asked yet
	// (another question open, automation paused).
	MaxAskAttempts int
}

// DefaultConfig is the pilot starting point.
func DefaultConfig() Config {
	return Config{Enabled: true, FollowUpDelay: 6 * time.Hour, AnswerTTL: 72 * time.Hour, FreeDeals: 100, MaxAskAttempts: 24}
}

// Summary is the dashboard view.
type Summary struct {
	Total          int            `json:"total"`
	ByStatus       map[string]int `json:"by_status"`
	Confirmed      int            `json:"confirmed"`
	FreeLimit      int            `json:"free_limit"`
	FreeUsed       int            `json:"free_used"`
	FreeRemaining  int            `json:"free_remaining"`
	Ratings        int            `json:"ratings"`
	AverageRating  *float64       `json:"average_rating"`
	LowRatings     int            `json:"low_ratings"`
	ConfirmedValue int64          `json:"confirmed_value"` // sum of confirmed deal amounts (naira)
}

// Repository persists deals.
type Repository interface {
	// Create is idempotent per transaction (created=false: it already existed).
	Create(ctx context.Context, d *Deal) (*Deal, bool, error)
	Get(ctx context.Context, id uuid.UUID) (*Deal, error)
	MarkAsked(ctx context.Context, id uuid.UUID, party string, at time.Time) error
	MarkUnreachable(ctx context.Context, id uuid.UUID, party string) error
	BumpAskAttempts(ctx context.Context, id uuid.UUID) (int, error)
	// RecordAnswer stores one side's answer and the resulting status; a deal
	// that becomes CONFIRMED gets the next deal number (free when it is
	// within freeLimit), atomically. Answers after a terminal status are
	// ignored (returned unchanged, changed=false).
	RecordAnswer(ctx context.Context, id uuid.UUID, party, answer string, at time.Time, freeLimit int) (d *Deal, prev Status, changed bool, err error)
	// Close settles the deal at the end of the answer window.
	Close(ctx context.Context, id uuid.UUID, at time.Time) (*Deal, Status, error)
	SetRating(ctx context.Context, id uuid.UUID, score int, at time.Time) (*Deal, bool, error)
	Summary(ctx context.Context, freeLimit int) (*Summary, error)
	List(ctx context.Context, status string, limit, offset int) ([]*Deal, int, error)
}

// Small read interfaces over existing repositories.
type (
	Offers interface {
		FindByID(ctx context.Context, id uuid.UUID) (*offer.Offer, error)
	}
	Requests interface {
		FindByID(ctx context.Context, id uuid.UUID) (*request.BuyerRequest, error)
	}
	Vendors interface {
		FindByID(ctx context.Context, id uuid.UUID) (*vendor.Vendor, error)
	}
	Conversations interface {
		FindByID(ctx context.Context, id uuid.UUID) (*conversation.Conversation, error)
	}
	// References gives the MM- reference both sides already know.
	References interface {
		PublicReference(ctx context.Context, vendorRequestID uuid.UUID) (string, error)
	}
	Messenger interface {
		SendText(ctx context.Context, to, body string) error
	}
	// Holds: an operator pause (one request, or all automation).
	Holds interface {
		Held(ctx context.Context, buyerRequestID uuid.UUID) bool
	}
)

// Deps groups the constructor arguments.
type Deps struct {
	Repo          Repository
	Offers        Offers
	Requests      Requests
	Vendors       Vendors
	Conversations Conversations
	References    References
	Messenger     Messenger
	Store         autonomy.Store
	Jobs          jobs.Store
	Events        event.Writer
	Logger        *slog.Logger
	Holds         Holds // optional
	// VendorBusy (optional): the vendor owes a reply on another buyer request.
	// While true Majo does not ask "did you sell?" (a bare "yes" would be
	// ambiguous between the two questions).
	VendorBusy func(ctx context.Context, vendorID uuid.UUID) bool
}

// Service runs introductions and follow-ups.
type Service struct {
	cfg Config
	d   Deps
	now func() time.Time
}

// NewService builds the service.
func NewService(cfg Config, d Deps) *Service {
	if cfg.MaxAskAttempts <= 0 {
		cfg.MaxAskAttempts = 24
	}
	return &Service{cfg: cfg, d: d, now: time.Now}
}

// SetClock replaces the clock (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// Config returns the configuration (dashboard).
func (s *Service) Config() Config { return s.cfg }

// Register the job handlers.
func (s *Service) Register(w *jobs.Worker) {
	w.Register(JobFollowUp, s.handleFollowUp, nil)
	w.Register(JobClose, s.handleClose, nil)
}

// ---- introduction (at the buyer's choice) ------------------------------------------------

// Introduction is what Majo tells the buyer once they chose an offer: who
// the seller is, how to reach them, and how to stay safe. Used in the
// selection confirmation. "" when the offer or vendor cannot be read.
func (s *Service) Introduction(ctx context.Context, offerID uuid.UUID) string {
	o, err := s.d.Offers.FindByID(ctx, offerID)
	if err != nil || o == nil {
		return ""
	}
	v, err := s.d.Vendors.FindByID(ctx, o.VendorID)
	if err != nil || v == nil {
		return ""
	}
	ref := s.reference(ctx, o.VendorRequestID)
	return BuyerIntroduction(v.BusinessName, v.WhatsAppExternalID, ref)
}

// BuyerIntroduction is the buyer-facing text (pure; reviewed in one place).
func BuyerIntroduction(vendorName, vendorPhone, ref string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Seller: %s\n", vendorName)
	if link := WALink(vendorPhone); link != "" {
		fmt.Fprintf(&b, "Chat with them on WhatsApp: %s\n", link)
	}
	if ref != "" {
		fmt.Fprintf(&b, "Mention reference %s so they know it's you.\n", ref)
	}
	b.WriteString("\n🔒 Stay safe: pay on delivery or meet in a busy public place, check the item before you pay, " +
		"and never send money before you see it. Majo doesn't hold or handle payments.")
	return b.String()
}

// VendorIntroduction is the vendor-facing text.
func VendorIntroduction(item string, amount *int64, ref, buyerPhone string, shareBuyer bool) string {
	var b strings.Builder
	b.WriteString("✅ Good news! A Majo buyer chose your offer")
	if item != "" {
		fmt.Fprintf(&b, " for %s", item)
	}
	if amount != nil && *amount > 0 {
		fmt.Fprintf(&b, " at %s", naira(*amount))
	}
	if ref != "" {
		fmt.Fprintf(&b, " (reference %s)", ref)
	}
	b.WriteString(".\n\n")
	if shareBuyer && WALink(buyerPhone) != "" {
		fmt.Fprintf(&b, "Chat with the buyer: %s\n", WALink(buyerPhone))
	} else {
		b.WriteString("They'll message you on WhatsApp. ")
	}
	b.WriteString("Please reply quickly and agree delivery or pickup with them. Payment is between you and the buyer.")
	return b.String()
}

// WALink is a click-to-chat link for a WhatsApp number ("" if not a number).
func WALink(phone string) string {
	var d strings.Builder
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			d.WriteRune(r)
		}
	}
	if d.Len() < 8 {
		return ""
	}
	return "https://wa.me/" + d.String()
}

// OnSelected implements transaction.Listener: a buyer chose an offer. The
// vendor is told; the buyer is told too when the choice did not come from
// the buyer's own WhatsApp reply (their confirmation already introduced the
// seller); the follow-up is scheduled.
func (s *Service) OnSelected(ctx context.Context, tx *transaction.Transaction, source string) {
	if !s.cfg.Enabled || tx == nil {
		return
	}
	o, err := s.d.Offers.FindByID(ctx, tx.OfferID)
	if err != nil || o == nil {
		s.d.Logger.Error("deal: loading offer", "transaction_id", tx.ID, "error", err)
		return
	}
	req, err := s.d.Requests.FindByID(ctx, tx.BuyerRequestID)
	if err != nil || req == nil {
		s.d.Logger.Error("deal: loading request", "transaction_id", tx.ID, "error", err)
		return
	}
	v, err := s.d.Vendors.FindByID(ctx, tx.VendorID)
	if err != nil || v == nil {
		s.d.Logger.Error("deal: loading vendor", "transaction_id", tx.ID, "error", err)
		return
	}
	item := itemOf(o, req)
	d := &Deal{TransactionID: tx.ID, BuyerRequestID: tx.BuyerRequestID, BuyerID: tx.BuyerID, VendorID: tx.VendorID,
		ConversationID: req.ConversationID, VendorRequestID: o.VendorRequestID, Item: item, Reference: s.reference(ctx, o.VendorRequestID),
		Amount: o.Price, Status: StatusPending, AskAt: s.now().UTC().Add(s.cfg.FollowUpDelay)}
	saved, created, err := s.d.Repo.Create(ctx, d)
	if err != nil {
		s.d.Logger.Error("deal: creating", "transaction_id", tx.ID, "error", err)
		return
	}
	if !created {
		return // already introduced and scheduled (duplicate notification)
	}
	buyerPhone := s.buyerPhone(ctx, saved.ConversationID)
	s.send(ctx, saved, v.WhatsAppExternalID, "deal_introduction_vendor",
		VendorIntroduction(item, o.Price, saved.Reference, buyerPhone, s.cfg.ShareBuyerContact))
	if source != transaction.SourceWhatsApp && buyerPhone != "" {
		s.send(ctx, saved, buyerPhone, "deal_introduction_buyer",
			"Your choice is recorded ✅ "+item+"\n\n"+BuyerIntroduction(v.BusinessName, v.WhatsAppExternalID, saved.Reference))
	}
	s.event(ctx, event.DealCreated, saved, map[string]any{"source": source, "ask_at": saved.AskAt})
	s.enqueue(ctx, JobFollowUp, saved.ID, "ask:"+Buyer+"+"+Vendor, saved.AskAt, map[string]any{"party": "BOTH"})
}

func itemOf(o *offer.Offer, req *request.BuyerRequest) string {
	parts := []string{}
	if o.Product != nil && *o.Product != "" {
		parts = append(parts, *o.Product)
	} else if req.Product != nil {
		parts = append(parts, *req.Product)
	}
	if o.Variant != nil && *o.Variant != "" {
		parts = append(parts, *o.Variant)
	} else if req.Variant != nil && *req.Variant != "" {
		parts = append(parts, *req.Variant)
	}
	if len(parts) == 0 {
		return "the item"
	}
	return strings.Join(parts, " ")
}

// ---- follow-up questions -----------------------------------------------------------------

func (s *Service) handleFollowUp(ctx context.Context, j *jobs.Job) error {
	id, err := uuid.Parse(fmt.Sprint(j.Payload["deal_id"]))
	if err != nil {
		return jobs.Permanent(err)
	}
	party := fmt.Sprint(j.Payload["party"])
	return s.FollowUp(ctx, id, party)
}

// FollowUp asks the buyer and/or the vendor ("BOTH", "BUYER", "VENDOR").
func (s *Service) FollowUp(ctx context.Context, id uuid.UUID, party string) error {
	d, err := s.d.Repo.Get(ctx, id)
	if err != nil {
		return err
	}
	if d == nil || d.Status.Terminal() || d.ClosedAt != nil {
		return nil
	}
	now := s.now().UTC()
	if s.d.Holds != nil && s.d.Holds.Held(ctx, d.BuyerRequestID) {
		return s.retryLater(ctx, d, party, now.Add(time.Hour), "held")
	}
	blocked := false
	if (party == "BOTH" || party == Buyer) && needsAsk(d.BuyerAnswer, d.BuyerAskedAt, d.BuyerUnreachable, party == Buyer) {
		if !s.askBuyer(ctx, d, now) {
			blocked = true
		}
	}
	if (party == "BOTH" || party == Vendor) && needsAsk(d.VendorAnswer, d.VendorAskedAt, d.VendorUnreach, party == Vendor) {
		if !s.askVendor(ctx, d, now) {
			blocked = true
		}
	}
	if blocked {
		// Another question is open in that conversation: try that side again
		// later (the side already asked is not asked twice).
		return s.retryLater(ctx, d, party, now.Add(30*time.Minute), "question_open")
	}
	s.enqueue(ctx, JobClose, d.ID, "close", now.Add(s.cfg.AnswerTTL), nil)
	return nil
}

// needsAsk: not answered (NOT_YET counts as unanswered only on a re-ask),
// not already asked in this round, reachable.
func needsAsk(answer *string, askedAt *time.Time, unreachable, reask bool) bool {
	if unreachable {
		return false
	}
	if answer == nil {
		return askedAt == nil || reask
	}
	return *answer == NotYet && reask
}

func (s *Service) retryLater(ctx context.Context, d *Deal, party string, at time.Time, why string) error {
	n, err := s.d.Repo.BumpAskAttempts(ctx, d.ID)
	if err != nil {
		return err
	}
	if n > s.cfg.MaxAskAttempts {
		s.d.Logger.Warn("deal: follow-up gave up", "deal_id", d.ID, "why", why, "attempts", n)
		s.enqueue(ctx, JobClose, d.ID, "close", s.now().UTC().Add(s.cfg.AnswerTTL), nil)
		return nil
	}
	s.enqueue(ctx, JobFollowUp, d.ID, fmt.Sprintf("ask:%s:retry:%d", party, n), at, map[string]any{"party": party})
	return nil
}

func (s *Service) askBuyer(ctx context.Context, d *Deal, now time.Time) bool {
	phone := s.buyerPhone(ctx, d.ConversationID)
	if phone == "" {
		_ = s.d.Repo.MarkUnreachable(ctx, d.ID, Buyer)
		return true
	}
	vendorName := s.vendorName(ctx, d.VendorID)
	q := BuyerQuestion(d.Item, vendorName)
	conv, buyer, brid := d.ConversationID, d.BuyerID, d.BuyerRequestID
	c := &autonomy.Clarification{Party: autonomy.PartyBuyer, ConversationID: &conv, BuyerID: &buyer, BuyerRequestID: &brid,
		Kind: KindBuyerConfirm, Question: q, Context: map[string]any{"deal_id": d.ID.String()}, Status: autonomy.ClarOpen,
		Attempts: 1, ExpiresAt: now.Add(s.cfg.AnswerTTL)}
	opened, blocking, err := s.d.Store.OpenClarificationIfFree(ctx, c)
	if err != nil {
		s.d.Logger.Error("deal: opening buyer question", "deal_id", d.ID, "error", err)
		return false
	}
	if opened == nil || blocking != nil {
		return false
	}
	if err := s.sendErr(ctx, d, phone, "deal_followup_buyer", q); err != nil {
		_ = s.d.Store.ResolveClarification(ctx, opened.ID, autonomy.ClarCancelled, nil, "", map[string]any{"reason": "undelivered"}, now)
		_ = s.d.Repo.MarkUnreachable(ctx, d.ID, Buyer)
		return true
	}
	_ = s.d.Repo.MarkAsked(ctx, d.ID, Buyer, now)
	return true
}

func (s *Service) askVendor(ctx context.Context, d *Deal, now time.Time) bool {
	v, err := s.d.Vendors.FindByID(ctx, d.VendorID)
	if err != nil || v == nil {
		_ = s.d.Repo.MarkUnreachable(ctx, d.ID, Vendor)
		return true
	}
	if open, err := s.d.Store.OpenVendorClarification(ctx, v.ID, now); err != nil {
		return false
	} else if open != nil {
		return false // never supersede a question the vendor is answering
	}
	if s.d.VendorBusy != nil && s.d.VendorBusy(ctx, v.ID) {
		return false
	}
	q := VendorQuestion(d.Item, d.Reference)
	vid := v.ID
	brid := d.BuyerRequestID
	c := &autonomy.Clarification{Party: autonomy.PartyVendor, VendorID: &vid, VendorRequestID: d.VendorRequestID, BuyerRequestID: &brid,
		Kind: KindVendorConfirm, Question: q, Context: map[string]any{"deal_id": d.ID.String()}, Status: autonomy.ClarOpen,
		Attempts: 1, ExpiresAt: now.Add(s.cfg.AnswerTTL)}
	opened, err := s.d.Store.OpenClarification(ctx, c)
	if err != nil {
		s.d.Logger.Error("deal: opening vendor question", "deal_id", d.ID, "error", err)
		return false
	}
	if err := s.sendErr(ctx, d, v.WhatsAppExternalID, "deal_followup_vendor", q); err != nil {
		_ = s.d.Store.ResolveClarification(ctx, opened.ID, autonomy.ClarCancelled, nil, "", map[string]any{"reason": "undelivered"}, now)
		_ = s.d.Repo.MarkUnreachable(ctx, d.ID, Vendor)
		return true
	}
	_ = s.d.Repo.MarkAsked(ctx, d.ID, Vendor, now)
	return true
}

// BuyerQuestion / VendorQuestion: the follow-up wording.
func BuyerQuestion(item, vendorName string) string {
	from := ""
	if vendorName != "" {
		from = " from " + vendorName
	}
	return fmt.Sprintf("Hi 👋 Quick one: did you buy the %s%s? Reply YES, NO or NOT YET.", item, from)
}

func VendorQuestion(item, ref string) string {
	r := ""
	if ref != "" {
		r = " (reference " + ref + ")"
	}
	return fmt.Sprintf("Hi 👋 Did you sell the %s to the Majo buyer%s? Reply YES, NO or NOT YET.", item, r)
}

// RatingQuestion asks the buyer to rate the seller.
func RatingQuestion(vendorName string) string {
	if vendorName == "" {
		vendorName = "the seller"
	}
	return fmt.Sprintf("Great, thanks for confirming 🎉 How would you rate %s? Reply with a number from 1 (bad) to 5 (great).", vendorName)
}

// ---- answers -------------------------------------------------------------------------------

// BuyerAnswered records the buyer's answer (the buyer runtime closed the
// question already) and replies.
func (s *Service) BuyerAnswered(ctx context.Context, dealID uuid.UUID, answer string) error {
	now := s.now().UTC()
	d, prev, changed, err := s.d.Repo.RecordAnswer(ctx, dealID, Buyer, answer, now, s.cfg.FreeDeals)
	if err != nil || d == nil {
		return err
	}
	phone := s.buyerPhone(ctx, d.ConversationID)
	if !changed {
		s.send(ctx, d, phone, "deal_ack", "Thanks 🙏 I've already noted this one.")
		return nil
	}
	switch answer {
	case Yes:
		vendorName := s.vendorName(ctx, d.VendorID)
		q := RatingQuestion(vendorName)
		conv, buyer, brid := d.ConversationID, d.BuyerID, d.BuyerRequestID
		c := &autonomy.Clarification{Party: autonomy.PartyBuyer, ConversationID: &conv, BuyerID: &buyer, BuyerRequestID: &brid,
			Kind: KindBuyerRating, Question: q, Context: map[string]any{"deal_id": d.ID.String()}, Status: autonomy.ClarOpen,
			Attempts: 1, ExpiresAt: now.Add(s.cfg.AnswerTTL)}
		if opened, blocking, err := s.d.Store.OpenClarificationIfFree(ctx, c); err == nil && opened != nil && blocking == nil {
			s.send(ctx, d, phone, "deal_rating_question", q)
		} else {
			s.send(ctx, d, phone, "deal_ack", "Great, thanks for confirming 🎉")
		}
	case No:
		s.send(ctx, d, phone, "deal_ack", fmt.Sprintf("Thanks for letting me know 🙏 If you still need %s, just tell me and I'll search again.", d.Item))
	case NotYet:
		s.send(ctx, d, phone, "deal_ack", "No wahala 👍 I'll check back later.")
		s.enqueue(ctx, JobFollowUp, d.ID, "reask:"+Buyer, now.Add(s.cfg.FollowUpDelay), map[string]any{"party": Buyer})
	}
	s.afterChange(ctx, d, prev)
	return nil
}

// VendorAnswered records the vendor's answer and replies.
func (s *Service) VendorAnswered(ctx context.Context, dealID uuid.UUID, answer string) error {
	now := s.now().UTC()
	d, prev, changed, err := s.d.Repo.RecordAnswer(ctx, dealID, Vendor, answer, now, s.cfg.FreeDeals)
	if err != nil || d == nil {
		return err
	}
	phone := ""
	if v, err := s.d.Vendors.FindByID(ctx, d.VendorID); err == nil && v != nil {
		phone = v.WhatsAppExternalID
	}
	if !changed {
		s.send(ctx, d, phone, "deal_ack", "Thanks 🙏 I've already noted this one.")
		return nil
	}
	switch answer {
	case Yes:
		s.send(ctx, d, phone, "deal_ack", "Thanks for confirming 🙏")
	case No:
		s.send(ctx, d, phone, "deal_ack", "Thanks for letting me know 🙏")
	case NotYet:
		s.send(ctx, d, phone, "deal_ack", "Okay 👍 I'll check back later.")
		s.enqueue(ctx, JobFollowUp, d.ID, "reask:"+Vendor, now.Add(s.cfg.FollowUpDelay), map[string]any{"party": Vendor})
	}
	s.afterChange(ctx, d, prev)
	return nil
}

// BuyerRated stores the rating; a low rating reaches the operator.
func (s *Service) BuyerRated(ctx context.Context, dealID uuid.UUID, score int) error {
	if score < 1 || score > 5 {
		return errors.New("rating must be 1-5")
	}
	d, changed, err := s.d.Repo.SetRating(ctx, dealID, score, s.now().UTC())
	if err != nil || d == nil || !changed {
		return err
	}
	phone := s.buyerPhone(ctx, d.ConversationID)
	switch {
	case score >= 4:
		s.send(ctx, d, phone, "deal_ack", "Thank you! 🙏 Glad it went well.")
	case score == 3:
		s.send(ctx, d, phone, "deal_ack", "Thanks for the feedback 🙏")
	default:
		s.send(ctx, d, phone, "deal_ack", "Sorry it didn't go well 😔 Thanks for telling me. I'll look into it.")
		s.raise(ctx, d, ReasonLowRating, map[string]any{"rating": score,
			"why": "the buyer rated this seller 1 or 2 out of 5: check what happened before routing more buyers to them"})
	}
	s.event(ctx, event.DealRated, d, map[string]any{"rating": score})
	return nil
}

// ---- closing ------------------------------------------------------------------------------

func (s *Service) handleClose(ctx context.Context, j *jobs.Job) error {
	id, err := uuid.Parse(fmt.Sprint(j.Payload["deal_id"]))
	if err != nil {
		return jobs.Permanent(err)
	}
	d, prev, err := s.d.Repo.Close(ctx, id, s.now().UTC())
	if err != nil || d == nil {
		return err
	}
	if prev != d.Status {
		s.event(ctx, event.DealClosed, d, map[string]any{"from": string(prev)})
	}
	return nil
}

func (s *Service) afterChange(ctx context.Context, d *Deal, prev Status) {
	if d.Status == prev {
		return
	}
	switch d.Status {
	case StatusConfirmed:
		s.event(ctx, event.DealConfirmed, d, map[string]any{"deal_number": d.DealNumber, "free": d.Free})
	case StatusDisputed:
		s.raise(ctx, d, ReasonDisputed, map[string]any{"buyer_answer": val(d.BuyerAnswer), "vendor_answer": val(d.VendorAnswer),
			"why": "the buyer and the seller disagree about whether the sale happened: check with both"})
		s.event(ctx, event.DealDisputed, d, nil)
	}
}

// ---- dashboard -----------------------------------------------------------------------------

// Summary is the free-deal counter and outcome counts.
func (s *Service) Summary(ctx context.Context) (*Summary, error) {
	return s.d.Repo.Summary(ctx, s.cfg.FreeDeals)
}

// List pages through deals (newest first).
func (s *Service) List(ctx context.Context, status string, limit, offset int) ([]*Deal, int, error) {
	return s.d.Repo.List(ctx, status, limit, offset)
}

// ---- helpers -------------------------------------------------------------------------------

func (s *Service) reference(ctx context.Context, vrID *uuid.UUID) string {
	if vrID == nil || s.d.References == nil {
		return ""
	}
	ref, err := s.d.References.PublicReference(ctx, *vrID)
	if err != nil {
		return ""
	}
	return ref
}

func (s *Service) buyerPhone(ctx context.Context, conversationID uuid.UUID) string {
	c, err := s.d.Conversations.FindByID(ctx, conversationID)
	if err != nil || c == nil {
		return ""
	}
	return c.ExternalID
}

func (s *Service) vendorName(ctx context.Context, id uuid.UUID) string {
	v, err := s.d.Vendors.FindByID(ctx, id)
	if err != nil || v == nil {
		return ""
	}
	return v.BusinessName
}

func (s *Service) send(ctx context.Context, d *Deal, to, kind, text string) {
	if err := s.sendErr(ctx, d, to, kind, text); err != nil {
		s.d.Logger.Warn("deal: message not sent", "deal_id", d.ID, "kind", kind, "error", err)
	}
}

func (s *Service) sendErr(ctx context.Context, d *Deal, to, kind, text string) error {
	if to == "" {
		return errors.New("no recipient")
	}
	id := d.ID
	sendCtx := conversation.WithOutboundPurpose(ctx, conversation.OutboundPurpose{Kind: kind, EntityType: "deal", EntityID: &id,
		Extra: map[string]any{"buyer_request_id": d.BuyerRequestID.String(), "transaction_id": d.TransactionID.String()}})
	return s.d.Messenger.SendText(sendCtx, to, text)
}

func (s *Service) enqueue(ctx context.Context, typ string, id uuid.UUID, key string, at time.Time, extra map[string]any) {
	if s.d.Jobs == nil {
		return
	}
	payload := map[string]any{"deal_id": id.String()}
	for k, v := range extra {
		payload[k] = v
	}
	if _, err := s.d.Jobs.Enqueue(ctx, &jobs.Job{Type: typ, IdempotencyKey: "deal:" + id.String() + ":" + key, Payload: payload,
		MaxAttempts: 5, RunAt: at}); err != nil {
		s.d.Logger.Error("deal: enqueueing", "deal_id", id, "job", typ, "error", err)
	}
}

func (s *Service) raise(ctx context.Context, d *Deal, reason string, detail map[string]any) {
	brid := d.BuyerRequestID
	detail["deal_id"] = d.ID.String()
	detail["vendor_id"] = d.VendorID.String()
	if _, err := s.d.Store.RaiseException(ctx, &autonomy.Exception{Class: autonomy.SafetyException, Reason: reason, SubjectType: "deal",
		SubjectID: d.ID, BuyerRequestID: &brid, Detail: detail}); err != nil {
		s.d.Logger.Error("deal: raising exception", "deal_id", d.ID, "reason", reason, "error", err)
	}
}

func (s *Service) event(ctx context.Context, t event.Type, d *Deal, extra map[string]any) {
	if s.d.Events == nil {
		return
	}
	id := d.ID
	payload := map[string]any{"deal_id": d.ID.String(), "transaction_id": d.TransactionID.String(), "buyer_request_id": d.BuyerRequestID.String(),
		"vendor_id": d.VendorID.String(), "status": string(d.Status), "amount": d.Amount}
	for k, v := range extra {
		payload[k] = v
	}
	if err := s.d.Events.Write(ctx, &event.Event{EventType: t, EntityType: "deal", EntityID: &id, ActorType: event.ActorSystem, Payload: payload}); err != nil {
		s.d.Logger.Error("deal: writing event", "type", t, "error", err)
	}
}

func naira(v int64) string {
	s := fmt.Sprint(v)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return "₦" + b.String()
}
