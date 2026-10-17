// Package autonomy holds the shared vocabulary of Majo's autonomous runtime
// (Phase 4C correction): feature flags, the fallback taxonomy, clarification
// state, the decision log and the operator exception queue.
//
// Operating model (Architecture Lock): Majo is autonomous by default. Humans
// operate the control plane (observe, pause, override, correct, investigate)
// and handle exceptions; they are never the hidden state machine of a healthy
// transaction. Ambiguity is resolved through context and participant
// clarification before any operator escalation.
//
// Interpretation (deterministic rules + AI extraction) produces typed
// proposals; deterministic policy validates them; only application services
// mutate state. Nothing in this package mutates commerce state itself.
package autonomy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---- feature flags --------------------------------------------------------

// Flags gate each autonomous path. OFF means the path is intentionally
// disabled (the implementation still exists and is tested), never that the
// system lacks it. Every value is a [PARAM] read from the environment.
type Flags struct {
	AutoConfirmClearRequests      bool
	AutoRouteDiscoveryRequests    bool
	AutoRouteMatchedRequests      bool
	AutoResolveVendorContext      bool
	AutoClarifyRequests           bool
	AutoClarifyVendorResponses    bool
	AutoRetryDelivery             bool
	AutoPublishTrustedVendorLists bool
	// AutoRecoverStalledRequests: when every contacted vendor went quiet
	// (timeout), could not be reached, or declined, Majo contacts eligible
	// alternative vendors (bounded). OFF: the buyer is still told the status
	// (never a silent dead end), but nobody new is contacted automatically.
	AutoRecoverStalledRequests bool
}

// AllOn is the autonomous default (auto-publish stays gated by evidence).
func AllOn() Flags {
	return Flags{AutoConfirmClearRequests: true, AutoRouteDiscoveryRequests: true, AutoRouteMatchedRequests: true,
		AutoResolveVendorContext: true, AutoClarifyRequests: true, AutoClarifyVendorResponses: true, AutoRetryDelivery: true, AutoRecoverStalledRequests: true}
}

// Map reports the flags by their environment names (settings/diagnostics).
func (f Flags) Map() map[string]bool {
	return map[string]bool{
		"AUTO_CONFIRM_CLEAR_REQUESTS": f.AutoConfirmClearRequests, "AUTO_ROUTE_DISCOVERY_REQUESTS": f.AutoRouteDiscoveryRequests,
		"AUTO_ROUTE_MATCHED_REQUESTS": f.AutoRouteMatchedRequests, "AUTO_RESOLVE_VENDOR_CONTEXT": f.AutoResolveVendorContext,
		"AUTO_CLARIFY_REQUESTS": f.AutoClarifyRequests, "AUTO_CLARIFY_VENDOR_RESPONSES": f.AutoClarifyVendorResponses,
		"AUTO_RETRY_DELIVERY": f.AutoRetryDelivery, "AUTO_PUBLISH_TRUSTED_VENDOR_LISTINGS": f.AutoPublishTrustedVendorLists,
		"AUTO_RECOVER_STALLED_REQUESTS": f.AutoRecoverStalledRequests,
	}
}

// Params are the numeric policy parameters of the autonomous runtime.
type Params struct {
	// Confirmation.
	ConfirmMinConfidence float64 // extraction confidence floor for a clear request
	MaxClarifications    int     // questions per request before Majo stops asking
	ClarificationTTL     time.Duration
	// Routing.
	RouteMinScore      int // matching score floor for automatic routing
	RouteMaxVendors    int // fan-out per request
	RouteVendorMaxOpen int // skip a vendor already holding this many open requests
	// Vendor side.
	VendorRequestTTL       time.Duration // a SENT request older than this no longer counts as awaiting a reply
	VendorClarifyMaxRounds int
	// Delivery retry.
	RetryMaxAttempts int
	// Context.
	ContextWindow time.Duration // how far back conversation context is read
	// Recovery (Phase 4C hardening).
	RecoveryMaxRounds         int           // automatic re-routing rounds per request after vendors went quiet
	MaxVendorsPerRequest      int           // total vendors ever contacted for one request (all rounds)
	PendingNoticeTTL          time.Duration // a queued buyer notice older than this is cancelled, not sent late
	BroadenExcludedCategories []string      // categories Majo never offers to search outside the buyer's area
	RestrictedTerms           []string      // demand naming one of these is held for review (never auto-routed)
	// RetryFirstDelayOverride replaces every class's first retry delay when
	// set (AUTO_RETRY_FIRST_DELAY; 0 = per-class defaults: 5s transient,
	// 60s rate limit / provider unavailable, 5m per-recipient rate limit).
	RetryFirstDelayOverride time.Duration
	// LateReplyWindow: after a vendor request expired, a reply that clearly
	// names it is still taken as its reply for this long (the buyer's
	// request must still be open). AUTO_LATE_REPLY_WINDOW.
	LateReplyWindow time.Duration
	// DefaultLocation (PILOT_DEFAULT_LOCATION, pilot batch): in a one-campus
	// pilot, a request with no location and nothing known about the buyer's
	// area is taken to be for this place instead of asking "where are you?".
	// Empty (the default) keeps asking.
	DefaultLocation string
}

// DefaultParams are the starting hypotheses (documented in .env.example).
func DefaultParams() Params {
	return Params{ConfirmMinConfidence: 0.45, MaxClarifications: 3, ClarificationTTL: 24 * time.Hour,
		// RouteMinScore 45 (was 50, 01a audit): a vendor in the request's
		// category who serves the buyer's area (category 30 + service area
		// 17 = 47) is contacted even before they have posted any stock;
		// category alone (30) or category + a supply guess elsewhere is not.
		RouteMinScore: 45, RouteMaxVendors: 3, RouteVendorMaxOpen: 5,
		VendorRequestTTL: 72 * time.Hour, VendorClarifyMaxRounds: 2, RetryMaxAttempts: 5, ContextWindow: 72 * time.Hour,
		RecoveryMaxRounds: 2, MaxVendorsPerRequest: 6, PendingNoticeTTL: 72 * time.Hour, RestrictedTerms: DefaultRestrictedTerms(),
		LateReplyWindow: 72 * time.Hour}
}

// Validate fails clearly on nonsensical parameters.
func (p Params) Validate() error {
	switch {
	case p.ConfirmMinConfidence < 0 || p.ConfirmMinConfidence > 1:
		return fmt.Errorf("AUTO_CONFIRM_MIN_CONFIDENCE must be between 0 and 1")
	case p.MaxClarifications < 1 || p.MaxClarifications > 10:
		return fmt.Errorf("AUTO_CLARIFY_MAX_QUESTIONS must be between 1 and 10")
	case p.ClarificationTTL <= 0 || p.VendorRequestTTL <= 0 || p.ContextWindow <= 0:
		return fmt.Errorf("autonomy windows must be positive")
	case p.RouteMinScore < 0 || p.RouteMinScore > 100:
		return fmt.Errorf("AUTO_ROUTE_MIN_SCORE must be between 0 and 100")
	case p.RouteMaxVendors < 1 || p.RouteMaxVendors > 10:
		return fmt.Errorf("AUTO_ROUTE_MAX_VENDORS must be between 1 and 10")
	case p.RouteVendorMaxOpen < 1:
		return fmt.Errorf("AUTO_ROUTE_VENDOR_MAX_OPEN must be at least 1")
	case p.VendorClarifyMaxRounds < 1 || p.RetryMaxAttempts < 1:
		return fmt.Errorf("autonomy retry/clarification bounds must be at least 1")
	case p.RetryMaxAttempts > 20:
		return fmt.Errorf("AUTO_RETRY_MAX_ATTEMPTS must be at most 20")
	case p.RecoveryMaxRounds < 1 || p.RecoveryMaxRounds > 5:
		return fmt.Errorf("AUTO_RECOVERY_MAX_ROUNDS must be between 1 and 5")
	case p.MaxVendorsPerRequest < p.RouteMaxVendors || p.MaxVendorsPerRequest > 20:
		return fmt.Errorf("AUTO_ROUTE_MAX_VENDORS_PER_REQUEST must be between AUTO_ROUTE_MAX_VENDORS and 20")
	case p.PendingNoticeTTL <= 0 || p.LateReplyWindow <= 0:
		return fmt.Errorf("AUTO_PENDING_NOTICE_TTL and AUTO_LATE_REPLY_WINDOW must be positive")
	}
	return nil
}

// ---- fallback taxonomy (correction §16) ------------------------------------

// Fallback classifies why an autonomous step did not simply proceed. Only
// SafetyException and SystemException normally reach an operator.
type Fallback string

const (
	FallbackNone             Fallback = ""
	ClarificationRequired    Fallback = "CLARIFICATION_REQUIRED"     // Majo asks the participant
	RetryRequired            Fallback = "RETRY_REQUIRED"             // Majo recovers automatically
	PolicyRejection          Fallback = "POLICY_REJECTION"           // deterministic refusal
	TemporaryProviderFailure Fallback = "TEMPORARY_PROVIDER_FAILURE" // retry/recovery path
	GenuineAmbiguity         Fallback = "GENUINE_AMBIGUITY"          // context exhausted: ask the participant
	SafetyException          Fallback = "SAFETY_EXCEPTION"           // control-plane intervention
	SystemException          Fallback = "SYSTEM_EXCEPTION"           // investigation
)

// NeedsOperator reports whether the fallback class is an operator exception.
func (f Fallback) NeedsOperator() bool { return f == SafetyException || f == SystemException }

// ---- clarification state ----------------------------------------------------

// Party is who a clarification question is put to.
type Party string

const (
	PartyBuyer  Party = "BUYER"
	PartyVendor Party = "VENDOR"
)

// Clarification statuses.
const (
	ClarOpen       = "OPEN"
	ClarAnswered   = "ANSWERED"
	ClarSuperseded = "SUPERSEDED" // the participant moved on (new request / newer question)
	ClarExpired    = "EXPIRED"
	ClarExhausted  = "EXHAUSTED" // asked MaxClarifications times without a usable answer
	ClarCancelled  = "CANCELLED" // operator/control plane closed it
)

// Clarification kinds.
const (
	// Buyer.
	KindMissingField  = "missing_field"  // Field names the field (product, location, spec, budget)
	KindWhichItem     = "which_item"     // several context items match a reference
	KindConfirmOption = "confirm_option" // "Do you want option 2 (₦120,000)? Reply 2"
	KindBroadenScope  = "broaden_scope"  // no local match: may Majo search further?
	KindAskSeller     = "ask_seller"     // a listing without a price: may Majo ask the seller?
	// Vendor.
	KindVendorWhichRequest = "vendor_which_request" // several open requests: which one is this about?
	KindVendorDetail       = "vendor_detail"        // unclear reply: confirm availability/price
)

// Option is one choice offered in a clarification.
type Option struct {
	Key   string         `json:"key"`   // "1", "2", "yes", "no"
	Label string         `json:"label"` // what the participant saw
	Ref   string         `json:"ref,omitempty"`
	Value map[string]any `json:"value,omitempty"` // what choosing it means (ids, fields)
}

// Clarification is one question Majo asked a participant, and its outcome.
type Clarification struct {
	ID                uuid.UUID      `json:"id"`
	Party             Party          `json:"party"`
	ConversationID    *uuid.UUID     `json:"conversation_id"`
	BuyerID           *uuid.UUID     `json:"buyer_id"`
	VendorID          *uuid.UUID     `json:"vendor_id"`
	BuyerRequestID    *uuid.UUID     `json:"buyer_request_id"`
	VendorRequestID   *uuid.UUID     `json:"vendor_request_id"`
	VendorResponseID  *uuid.UUID     `json:"vendor_response_id"`
	Kind              string         `json:"kind"`
	Field             string         `json:"field,omitempty"`
	Question          string         `json:"question"`
	Options           []Option       `json:"options"`
	Context           map[string]any `json:"context"`
	Status            string         `json:"status"`
	Attempts          int            `json:"attempts"`
	QuestionMessageID *uuid.UUID     `json:"question_message_id"`
	AnswerMessageID   *uuid.UUID     `json:"answer_message_id"`
	AnswerText        string         `json:"answer_text,omitempty"`
	Resolution        map[string]any `json:"resolution"`
	AskedAt           time.Time      `json:"asked_at"`
	ExpiresAt         time.Time      `json:"expires_at"`
	ResolvedAt        *time.Time     `json:"resolved_at"`
}

// ---- decision log -----------------------------------------------------------

// Decision kinds: the typed proposals the interpreter may make.
const (
	DecInterpretRequest       = "INTERPRET_REQUEST"
	DecContinueRequest        = "CONTINUE_REQUEST" // clarification answer merged
	DecAskClarification       = "ASK_CLARIFICATION"
	DecConfirmRequest         = "CONFIRM_REQUEST"
	DecRouteRequest           = "ROUTE_REQUEST"
	DecNoMatch                = "NO_MATCH"
	DecAnswerFromContext      = "ANSWER_FROM_CONTEXT"
	DecReferenceItem          = "REFERENCE_ITEM"
	DecInterpretVendorReply   = "INTERPRET_VENDOR_RESPONSE"
	DecAssociateVendorReply   = "ASSOCIATE_VENDOR_RESPONSE"
	DecAskVendorClarification = "ASK_VENDOR_CLARIFICATION"
	DecRetryDelivery          = "RETRY_DELIVERY"
	DecAutoPublish            = "AUTO_PUBLISH_LISTING"
	DecHandleDiscovery        = "HANDLE_DISCOVERY_INTERACTION"
)

// Decision outcomes.
const (
	OutcomeApplied  = "APPLIED"  // policy accepted, service executed
	OutcomeRejected = "REJECTED" // policy refused (see Fallback/Reason)
	OutcomeDeferred = "DEFERRED" // waiting (clarification, retry)
	OutcomeFailed   = "FAILED"   // service failed
)

// Decision is one entry of the append-only decision log: what Majo decided,
// why, with what confidence and inputs — so an operator can see why a route
// (or a question, or a refusal) happened.
type Decision struct {
	ID             uuid.UUID      `json:"id"`
	WorkflowType   string         `json:"workflow_type"` // buyer_request | vendor_message | listing | conversation
	SubjectType    string         `json:"subject_type"`
	SubjectID      uuid.UUID      `json:"subject_id"`
	BuyerRequestID *uuid.UUID     `json:"buyer_request_id"`
	Kind           string         `json:"decision"`
	Outcome        string         `json:"outcome"`
	Fallback       Fallback       `json:"fallback,omitempty"`
	Reason         string         `json:"reason,omitempty"`
	Confidence     *float64       `json:"confidence"`
	PolicyVersion  string         `json:"policy_version"`
	Inputs         map[string]any `json:"inputs"`
	Result         map[string]any `json:"result"`
	Actor          string         `json:"actor"`
	CreatedAt      time.Time      `json:"created_at"`
}

// PolicyVersion identifies the autonomous policy set recorded on decisions.
const PolicyVersion = "autonomy-v1"

// ---- operator exceptions ------------------------------------------------------

// Exception statuses.
const (
	ExceptionOpen     = "OPEN"
	ExceptionResolved = "RESOLVED"
)

// Exception is an item that genuinely needs the control plane.
type Exception struct {
	ID             uuid.UUID      `json:"id"`
	Class          Fallback       `json:"class"` // SAFETY_EXCEPTION | SYSTEM_EXCEPTION
	Reason         string         `json:"reason"`
	SubjectType    string         `json:"subject_type"`
	SubjectID      uuid.UUID      `json:"subject_id"`
	BuyerRequestID *uuid.UUID     `json:"buyer_request_id"`
	Detail         map[string]any `json:"detail"`
	Status         string         `json:"status"`
	CreatedAt      time.Time      `json:"created_at"`
	ResolvedAt     *time.Time     `json:"resolved_at"`
	ResolvedBy     string         `json:"resolved_by,omitempty"`
	Resolution     string         `json:"resolution,omitempty"`
}

// Exception reasons (stable strings).
const (
	ReasonDeliveryRetryExhausted = "delivery_retry_exhausted"
	ReasonProviderPolicy         = "provider_policy_rejection" // e.g. 24h window closed, template required
	ReasonWorkflowFailure        = "workflow_failure"
	ReasonVendorUnreachable      = "vendor_unreachable"
)

// ---- persistence ------------------------------------------------------------------

// ErrClarificationNotOpen: the clarification was already resolved.
var ErrClarificationNotOpen = errors.New("clarification is not open")

// Store is the autonomy persistence boundary (PostgreSQL in production).
type Store interface {
	// OpenClarification persists a new OPEN clarification, superseding any
	// other OPEN one for the same party conversation (buyer conversation or
	// vendor), atomically.
	OpenClarification(ctx context.Context, c *Clarification) (*Clarification, error)
	// OpenClarificationIfFree opens c unless another OPEN question in the
	// same buyer conversation belongs to a different subject (another
	// request, or a question about shown items); then nothing changes and
	// that question is returned as blocking. Same subject = re-ask (the old
	// one is superseded). Atomic per conversation.
	OpenClarificationIfFree(ctx context.Context, c *Clarification) (opened, blocking *Clarification, err error)
	SetClarificationMessage(ctx context.Context, id, messageID uuid.UUID) error
	// OpenBuyerClarification / OpenVendorClarification return the current
	// OPEN, unexpired question (nil when none); expired ones are closed.
	OpenBuyerClarification(ctx context.Context, conversationID uuid.UUID, now time.Time) (*Clarification, error)
	OpenVendorClarification(ctx context.Context, vendorID uuid.UUID, now time.Time) (*Clarification, error)
	// ResolveClarification moves an OPEN clarification to status (ANSWERED,
	// SUPERSEDED, EXHAUSTED, CANCELLED). ErrClarificationNotOpen if it no
	// longer is (a concurrent answer won).
	ResolveClarification(ctx context.Context, id uuid.UUID, status string, answerMessageID *uuid.UUID, answer string, resolution map[string]any, at time.Time) error
	// BumpClarification re-asks: attempts+1 and a new question text.
	BumpClarification(ctx context.Context, id uuid.UUID, question string, expiresAt time.Time) (*Clarification, error)
	ClarificationsForRequest(ctx context.Context, buyerRequestID uuid.UUID) ([]*Clarification, error)

	RecordDecision(ctx context.Context, d *Decision) error
	DecisionsForRequest(ctx context.Context, buyerRequestID uuid.UUID) ([]*Decision, error)

	RaiseException(ctx context.Context, e *Exception) (*Exception, error)
	ListExceptions(ctx context.Context, status string, limit int) ([]*Exception, error)
	ResolveException(ctx context.Context, id uuid.UUID, by, resolution string, at time.Time) error

	// WithLock runs fn while holding a session-level advisory lock on key
	// (serialises workflow steps for one request or vendor).
	WithLock(ctx context.Context, key string, fn func(ctx context.Context) error) error
}

// ---- small shared helpers -----------------------------------------------------

// Normalize lowercases and collapses whitespace and trailing punctuation.
func Normalize(s string) string {
	t := strings.ToLower(strings.TrimSpace(s))
	t = strings.ReplaceAll(t, "’", "'")
	t = strings.TrimRight(t, ".!?🙏👍 ")
	return strings.Join(strings.Fields(t), " ")
}

// Ptr returns a pointer to v.
func Ptr[T any](v T) *T { return &v }

// ---- autonomy metrics (correction §17-18, §28) ---------------------------------

// Rate is a raw count with its denominator; Pct is nil when the denominator
// is 0. Raw counts are always reported alongside percentages so 1/4 never
// looks like 250/1000.
type Rate struct {
	N   int      `json:"n"`
	Of  int      `json:"of"`
	Pct *float64 `json:"pct"`
}

// NewRate builds a Rate.
func NewRate(n, of int) Rate {
	r := Rate{N: n, Of: of}
	if of > 0 {
		v := float64(int(float64(n)/float64(of)*1000+0.5)) / 10
		r.Pct = &v
	}
	return r
}

// Workflow classes, most severe first (a workflow has exactly one).
const (
	ClassSystemFailure        = "system_failure"
	ClassOperatorIntervention = "operator_intervention" // an operator performed a normal-path step
	ClassOperatorOverride     = "operator_override"     // an operator changed a decision Majo made
	ClassAutomaticRetry       = "automatic_retry"
	ClassClarificationAssist  = "clarification_assisted"
	ClassAutonomous           = "autonomous"
)

// WorkflowFacts are the per-request signals the classification uses.
type WorkflowFacts struct {
	OperatorConfirmed  bool
	OperatorRouted     bool
	OperatorClarified  bool
	OperatorAssociated bool
	OperatorCorrected  bool // request or vendor-response correction
	OperatorSelected   bool
	Clarified          bool // a clarification (buyer or vendor) was answered
	Recovered          bool // an automatic retry succeeded
	SystemException    bool
	HasTransaction     bool
	Progressed         bool // routed, or the buyer was told no match / asked to broaden
}

// Classify assigns one class to a workflow.
func Classify(f WorkflowFacts) string {
	switch {
	case f.SystemException:
		return ClassSystemFailure
	case f.OperatorConfirmed || f.OperatorRouted || f.OperatorClarified || f.OperatorAssociated:
		return ClassOperatorIntervention
	case f.OperatorCorrected || f.OperatorSelected:
		return ClassOperatorOverride
	case f.Recovered:
		return ClassAutomaticRetry
	case f.Clarified:
		return ClassClarificationAssist
	}
	return ClassAutonomous
}

// AutonomousClass reports whether the class counts as Majo working alone.
func AutonomousClass(c string) bool {
	return c == ClassAutonomous || c == ClassClarificationAssist || c == ClassAutomaticRetry
}

// Metrics is the autonomy report.
type Metrics struct {
	Since                     *time.Time      `json:"since"`
	Workflows                 int             `json:"total_workflows"`
	ByClass                   map[string]int  `json:"workflows_by_class"`
	AutonomousRequestHandling Rate            `json:"autonomous_request_handling"` // progressed autonomously / all requests
	AutonomousCompletion      Rate            `json:"autonomous_completion"`       // autonomous transactions / all transactions
	ClarificationResolution   Rate            `json:"clarification_resolution"`    // answered / issued (closed or open)
	ClarificationsByParty     map[string]Rate `json:"clarification_resolution_by_party"`
	OperatorIntervention      Rate            `json:"operator_intervention"` // workflows with intervention / all
	InterventionsByReason     map[string]int  `json:"interventions_by_reason"`
	ManualRouting             Rate            `json:"manual_routing"`      // vendor requests routed by an operator / all vendor requests
	ManualConfirmation        Rate            `json:"manual_confirmation"` // operator confirmations / all confirmations
	AutomaticRecovery         Rate            `json:"automatic_recovery"`  // recovered / retries scheduled
	FailuresByCategory        map[string]int  `json:"failures_by_category"`
	OpenExceptions            int             `json:"open_exceptions"`
	ExceptionsByReason        map[string]int  `json:"exceptions_by_reason"`
	Decisions                 map[string]int  `json:"decisions"` // decision:outcome -> count

	// Phase 4C hardening: metrics with one level each (workflow, question,
	// recovery, delivery) and explicit numerators/denominators. The legacy
	// fields above are kept for the dashboard; see Definitions.
	Workflow    WorkflowMetrics   `json:"workflow"`
	Questions   QuestionMetrics   `json:"questions"`
	Recovery    RecoveryMetrics   `json:"recovery"`
	Delivery    DeliveryMetrics   `json:"delivery"`
	Definitions map[string]string `json:"definitions"`
}

// WorkflowMetrics: denominator = buyer requests created in the window
// (Started). Small talk creates no request and is not counted.
type WorkflowMetrics struct {
	Started                int  `json:"started"`
	Completed              Rate `json:"completed"`               // a transaction exists / started
	CompletedAutonomously  Rate `json:"completed_autonomously"`  // completed AND autonomous class / started
	RequiringClarification Rate `json:"requiring_clarification"` // ≥1 question delivered (buyer or vendor) / started
	EscalatedToOperator    Rate `json:"escalated_to_operator"`   // ≥1 operator exception raised / started
	OperatorPerformedStep  Rate `json:"operator_performed_step"` // an operator confirmed/routed/asked/associated / started
	Held                   Rate `json:"held"`                    // ever paused by an operator / started
	EndedWithoutOffer      Rate `json:"ended_without_offer"`     // buyer told no match / no reply (no transaction) / started
	Abandoned              Rate `json:"abandoned"`               // waiting on the buyer whose last question expired / started
	Pending                Rate `json:"pending"`                 // none of the above end states yet / started
}

// QuestionMetrics: denominator = questions delivered (clarification rows
// not CANCELLED and not closed as undelivered).
type QuestionMetrics struct {
	Delivered         int  `json:"delivered"`          // rows (one question, possibly re-asked)
	MessagesSent      int  `json:"messages_sent"`      // including re-asks (sum of attempts)
	Answered          Rate `json:"answered"`           // ANSWERED / delivered
	Resolved          Rate `json:"resolved"`           // answered AND the same thing was not asked again / delivered
	ReAsked           Rate `json:"re_asked"`           // asked more than once (attempts > 1, or the same field asked again later) / delivered
	ExpiredOrGaveUp   Rate `json:"expired_or_gave_up"` // EXPIRED or EXHAUSTED / delivered
	Superseded        Rate `json:"superseded"`         // the participant moved on / delivered
	Open              int  `json:"open"`
	BuyerDelivered    int  `json:"buyer_delivered"`
	VendorDelivered   int  `json:"vendor_delivered"`
	UndeliveredClosed int  `json:"undelivered_closed"` // closed because the provider reported it undelivered (not in the denominator)
}

// RecoveryMetrics: automatic recovery, by mechanism.
type RecoveryMetrics struct {
	DeliveryRetries           Rate           `json:"delivery_retries"` // recovered / retries scheduled (transient failures only)
	DeliveryRetriesFailed     int            `json:"delivery_retries_failed"`
	VendorRequestsExpired     int            `json:"vendor_requests_expired"` // deadline passed, or never sent
	RecoveryDecisions         map[string]int `json:"recovery_decisions"`      // RECOVER_REQUEST outcome:reason -> count
	Rerouted                  int            `json:"rerouted"`
	RerouteLedToOffer         Rate           `json:"reroute_led_to_offer"` // rerouted requests with an offer since / rerouted requests
	DuplicateActionsPrevented int            `json:"duplicate_actions_prevented"`
}

// DeliveryMetrics: outbound communication health (message level).
type DeliveryMetrics struct {
	PolicyBlocked    int `json:"policy_blocked"` // sends refused by a messaging policy (window, template, opt-out)
	NoticesQueued    int `json:"notices_queued"`
	NoticesDelivered int `json:"notices_delivered"`
	NoticesPending   int `json:"notices_pending"`
	NoticesCancelled int `json:"notices_cancelled"`
	ProviderFailures int `json:"provider_failures_reported_async"`
	SystemExceptions int `json:"system_exceptions"`
	SafetyExceptions int `json:"safety_exceptions"`
}

// MetricDefinitions documents every reported number (served with the
// metrics so the dashboard and reports cannot drift from the code).
func MetricDefinitions() map[string]string {
	return map[string]string{
		"workflow.*":                      "denominator = buyer requests created since `since` (request level)",
		"workflow.completed":              "requests with a transaction (UNPAID) / started",
		"workflow.pending":                "requests not completed, not ended without offer, not abandoned / started",
		"questions.*":                     "denominator = questions delivered: clarification rows except CANCELLED and undelivered (question level)",
		"questions.resolved":              "ANSWERED and the same request+field (or vendor request+kind) was not asked again afterwards",
		"questions.messages_sent":         "message level: every question message including re-asks",
		"recovery.delivery_retries":       "recovered / retries scheduled (transient provider failures only; policy refusals are never retried and never in this denominator)",
		"recovery.reroute_led_to_offer":   "requests re-routed after silence that later got an offer / requests re-routed",
		"legacy.clarification_resolution": "question rows ANSWERED / rows not CANCELLED — questions answered, not ambiguity resolved (kept for compatibility; prefer questions.*)",
		"legacy.automatic_recovery":       "same as recovery.delivery_retries",
		"legacy.manual_routing":           "vendor-request level: VENDORS_ROUTED by an operator / all VENDORS_ROUTED",
		"legacy.manual_confirmation":      "request level: REQUEST_CONFIRMED by an operator / all REQUEST_CONFIRMED",
		"note":                            "test runs against the fake provider produce synthetic numbers; label them as such",
	}
}

// DefaultRestrictedTerms is the starting restricted-items list: demand that
// names one of these is never confirmed or routed automatically; it is held
// for operator review (SAFETY_EXCEPTION). It is a [PARAM] safety default,
// not a legal policy — the owner must confirm the list (see the hardening
// report, outstanding decisions). Matching is whole-word.
func DefaultRestrictedTerms() []string {
	return []string{"gun", "guns", "pistol", "rifle", "ak47", "ak-47", "ammunition", "ammo", "bullets", "firearm", "firearms",
		"cocaine", "heroin", "meth", "methamphetamine", "codeine", "tramadol", "rohypnol", "skunk", "weed", "marijuana", "cannabis", "colorado",
		"fake id", "fake passport", "fake certificate", "counterfeit", "fake naira", "explosive", "explosives", "dynamite"}
}
