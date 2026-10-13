package channel

// Outbound delivery failure taxonomy (Phase 4C hardening).
//
// One classifier for every outbound send (vendor requests, offer deliveries,
// workflow messages) and for asynchronous failures reported by delivery-status
// webhooks. It replaces the earlier three-way string match in the workflow
// package; there is no second taxonomy.
//
// Codes follow Meta's published WhatsApp Cloud API error-code reference
// (developers.facebook.com/documentation/business-messaging/whatsapp/support/
// error-codes, read Oct 2026). Meta states errors may arrive synchronously
// (Graph API response), asynchronously (webhooks) or both, so the same
// classification is applied to both paths. Unrecognised codes are UNKNOWN and
// are never retried blindly.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// FailureClass is the class of an outbound delivery failure.
type FailureClass string

const (
	FailTransient        FailureClass = "TRANSIENT"            // network/transport failure, 5xx, Meta 131000
	FailRateLimited      FailureClass = "RATE_LIMITED"         // throttling: 4, 80007, 130429, 131056, HTTP 429
	FailUnavailable      FailureClass = "PROVIDER_UNAVAILABLE" // 131016, 133004, 131057 (maintenance), HTTP 503
	FailInvalidRecipient FailureClass = "INVALID_RECIPIENT"    // 131026, 131021, 130403
	FailInvalidRequest   FailureClass = "INVALID_REQUEST"      // malformed message / parameters (our defect)
	FailPolicy           FailureClass = "POLICY_RESTRICTED"    // messaging window, template or messaging-policy restriction
	FailAuthConfig       FailureClass = "AUTH_OR_CONFIG"       // token, permission, registration, billing, missing template
	FailUnknown          FailureClass = "UNKNOWN"              // cannot be classified safely
)

// ProviderError is a rejected send with the provider's structured detail.
// Error() keeps the historical text ("whatsapp api returned status N: body")
// so stored send_error values and logs are unchanged.
type ProviderError struct {
	Provider   string // "whatsapp"
	HTTPStatus int
	Code       int // provider error code (0 = not present)
	Subcode    int
	Title      string
	Message    string
	Details    string
	Body       string // bounded raw body
}

func (e *ProviderError) Error() string {
	p := e.Provider
	if p == "" {
		p = "provider"
	}
	return fmt.Sprintf("%s api returned status %d: %s", p, e.HTTPStatus, e.Body)
}

// ParseGraphError builds a ProviderError from a Graph API error response
// body ({"error":{"code":..,"error_subcode":..,"message":..,"error_data":{"details":..}}}).
func ParseGraphError(provider string, status int, body []byte) *ProviderError {
	pe := &ProviderError{Provider: provider, HTTPStatus: status, Body: strings.TrimSpace(string(body))}
	var raw struct {
		Error struct {
			Code      int    `json:"code"`
			Subcode   int    `json:"error_subcode"`
			Message   string `json:"message"`
			Title     string `json:"error_user_title"`
			ErrorData struct {
				Details string `json:"details"`
			} `json:"error_data"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &raw) == nil {
		pe.Code, pe.Subcode, pe.Message, pe.Title, pe.Details = raw.Error.Code, raw.Error.Subcode, raw.Error.Message, raw.Error.Title, raw.Error.ErrorData.Details
	}
	return pe
}

// Failure is the classified outcome of a failed send and what the runtime
// may do about it. It is a pure function of the provider's answer.
type Failure struct {
	Class FailureClass `json:"class"`
	Code  int          `json:"code,omitempty"` // provider code, when known
	// Retry: an automatic retry of the same message is permitted (always
	// bounded by AUTO_RETRY_MAX_ATTEMPTS).
	Retry bool `json:"retry"`
	// FirstDelay before the first retry (later ones back off).
	FirstDelay time.Duration `json:"first_delay"`
	// TemplateRequired: the message may only go out as an approved template
	// (Meta 131047: the 24h customer-service window is closed).
	TemplateRequired bool `json:"template_required"`
	// WaitForRecipient: a compliant later route exists — send when the
	// recipient next messages (that re-opens the service window).
	WaitForRecipient bool `json:"wait_for_recipient"`
	// AccountLevel: the restriction affects the whole sending account (token,
	// billing, registration, integrity enforcement), not this recipient.
	AccountLevel bool `json:"account_level"`
	// Operator: no compliant autonomous action exists; a person must act.
	Operator bool `json:"operator"`
}

var (
	rateLimitCodes   = codes(4, 80007, 130429, 131056)
	unavailableCodes = codes(131016, 133004, 131057)
	transientCodes   = codes(131000)
	recipientCodes   = codes(131026, 131021, 130403)
	requestCodes     = codes(100, 131008, 131009, 131051, 131053, 131052, 135000, 132000, 132005, 132012, 132018)
	windowCodes      = codes(131047)
	// Policy restrictions that concern the recipient or the template's
	// marketing use: not retryable as-is, no account-wide impact.
	recipientPolicyCodes = codes(131049, 131050, 130472, 131063)
	// Account-wide integrity / quality enforcement.
	accountPolicyCodes = codes(368, 130497, 131031, 131048, 131064, 132007, 132015, 132016)
	authConfigCodes    = codes(0, 3, 10, 190, 131005, 131037, 131042, 131045, 133010, 132001, 134011)
)

func codes(cs ...int) map[int]bool {
	m := make(map[int]bool, len(cs))
	for _, c := range cs {
		m[c] = true
	}
	return m
}

var (
	statusPattern = regexp.MustCompile(`status (\d{3})`)
	codePattern   = regexp.MustCompile(`"code"\s*:\s*(\d+)`)
)

// Classify classifies a send error. A *ProviderError is classified from its
// structured code; any other error falls back to the error text (older
// wrappers) and finally to "transport failure" for network errors.
func Classify(err error) Failure {
	if err == nil {
		return Failure{}
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		return ClassifyCode(pe.Code, pe.HTTPStatus)
	}
	if errors.Is(err, ErrInvalidMessage) {
		return policyFor(FailInvalidRequest, 0)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return policyFor(FailTransient, 0)
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return policyFor(FailTransient, 0)
	}
	msg := strings.ToLower(err.Error())
	status := 0
	if m := statusPattern.FindStringSubmatch(msg); m != nil {
		status, _ = strconv.Atoi(m[1])
	}
	code := -1
	if m := codePattern.FindStringSubmatch(msg); m != nil {
		code, _ = strconv.Atoi(m[1])
	} else if strings.Contains(msg, "re-engagement") || strings.Contains(msg, "(#470)") {
		code = 131047
	}
	if status == 0 && code < 0 {
		// No provider answer at all: the request never completed (connection
		// refused, reset, timeout).
		return policyFor(FailTransient, 0)
	}
	if code < 0 {
		code = 0
		if status != 0 {
			return classifyStatusOnly(status)
		}
	}
	return ClassifyCode(code, status)
}

// ClassifyCode classifies a provider error code (webhook statuses carry a
// code but no HTTP status: pass httpStatus 0).
func ClassifyCode(code, httpStatus int) Failure {
	switch {
	case code > 0 && rateLimitCodes[code]:
		return policyFor(FailRateLimited, code)
	case code > 0 && unavailableCodes[code]:
		return policyFor(FailUnavailable, code)
	case code > 0 && transientCodes[code]:
		return policyFor(FailTransient, code)
	case code > 0 && recipientCodes[code]:
		return policyFor(FailInvalidRecipient, code)
	case code > 0 && windowCodes[code]:
		f := policyFor(FailPolicy, code)
		f.TemplateRequired, f.WaitForRecipient = true, true
		return f
	case code > 0 && recipientPolicyCodes[code]:
		return policyFor(FailPolicy, code)
	case code > 0 && accountPolicyCodes[code]:
		f := policyFor(FailPolicy, code)
		f.AccountLevel, f.Operator = true, true
		return f
	case code > 0 && requestCodes[code]:
		return policyFor(FailInvalidRequest, code)
	case authConfigCodes[code] && (code > 0 || httpStatus == 401 || httpStatus == 403):
		return policyFor(FailAuthConfig, code)
	case code >= 200 && code <= 299:
		return policyFor(FailAuthConfig, code)
	}
	// An unrecognised code: a server-side status (5xx / 429) still says
	// "temporary"; an unrecognised 4xx (or a webhook code) is UNKNOWN.
	if httpStatus != 0 && (code == 0 || httpStatus >= 500 || httpStatus == 429) {
		f := classifyStatusOnly(httpStatus)
		f.Code = code
		return f
	}
	return policyFor(FailUnknown, code)
}

func classifyStatusOnly(status int) Failure {
	switch {
	case status == 429:
		return policyFor(FailRateLimited, 0)
	case status == 503:
		return policyFor(FailUnavailable, 0)
	case status >= 500:
		return policyFor(FailTransient, 0)
	case status == 401 || status == 403:
		return policyFor(FailAuthConfig, 0)
	}
	return policyFor(FailUnknown, 0)
}

// policyFor is the per-class handling table (documented in
// docs/architecture/whatsapp.md → Delivery failures).
func policyFor(c FailureClass, code int) Failure {
	f := Failure{Class: c, Code: code}
	switch c {
	case FailTransient:
		f.Retry, f.FirstDelay = true, 5*time.Second
	case FailRateLimited:
		f.Retry, f.FirstDelay = true, 60*time.Second
		if code == 131056 { // pair rate limit: same recipient, wait longer
			f.FirstDelay = 5 * time.Minute
		}
	case FailUnavailable:
		f.Retry, f.FirstDelay = true, 60*time.Second
	case FailInvalidRecipient:
		f.Operator = true // the stored number is wrong or unreachable: data must be fixed
	case FailInvalidRequest:
		f.Operator = true // Majo built an invalid message: a defect to investigate
	case FailPolicy:
		// Not retryable as-is. Window-type restrictions set WaitForRecipient.
	case FailAuthConfig:
		f.AccountLevel, f.Operator = true, true
	case FailUnknown:
		f.Operator = true
	}
	return f
}

// Retryable reports whether the class permits an automatic retry.
func (f Failure) Retryable() bool { return f.Retry }
