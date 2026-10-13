package channel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

func TestParseGraphErrorKeepsCodeAndText(t *testing.T) {
	body := []byte(`{"error":{"message":"(#131047) Re-engagement message","type":"OAuthException","code":131047,"error_subcode":2494010,"error_data":{"details":"Message failed to send because more than 24 hours have passed"}}}`)
	pe := ParseGraphError("whatsapp", 400, body)
	if pe.Code != 131047 || pe.Subcode != 2494010 || pe.Details == "" {
		t.Fatalf("parsed: %+v", pe)
	}
	if got := pe.Error(); got != "whatsapp api returned status 400: "+string(body) {
		t.Fatalf("error text unchanged for stored send_error values: %q", got)
	}
	wrapped := fmt.Errorf("offer delivery failed: %w", pe)
	if f := Classify(wrapped); f.Class != FailPolicy || !f.TemplateRequired || !f.WaitForRecipient || f.Retry {
		t.Fatalf("classified through wrapping: %+v", f)
	}
}

func TestClassifyEveryClass(t *testing.T) {
	pe := func(status, code int) error {
		return &ProviderError{Provider: "whatsapp", HTTPStatus: status, Code: code}
	}
	cases := []struct {
		name     string
		err      error
		class    FailureClass
		retry    bool
		operator bool
	}{
		{"connection refused", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, FailTransient, true, false},
		{"timeout", context.DeadlineExceeded, FailTransient, true, false},
		{"5xx without code", pe(502, 0), FailTransient, true, false},
		{"meta unknown error 131000 (retry per Meta)", pe(500, 131000), FailTransient, true, false},
		{"throughput 130429 on HTTP 400", pe(400, 130429), FailRateLimited, true, false},
		{"pair rate limit 131056", pe(400, 131056), FailRateLimited, true, false},
		{"app rate limit 4", pe(400, 4), FailRateLimited, true, false},
		{"WABA rate limit 80007", pe(400, 80007), FailRateLimited, true, false},
		{"HTTP 429", pe(429, 0), FailRateLimited, true, false},
		{"service unavailable 131016", pe(503, 131016), FailUnavailable, true, false},
		{"maintenance 131057", pe(400, 131057), FailUnavailable, true, false},
		{"not a WhatsApp user 131026", pe(400, 131026), FailInvalidRecipient, false, true},
		{"sender = recipient 131021", pe(400, 131021), FailInvalidRecipient, false, true},
		{"business blocked user 130403", pe(400, 130403), FailInvalidRecipient, false, true},
		{"missing parameter 131008", pe(400, 131008), FailInvalidRequest, false, true},
		{"invalid parameter 100", pe(400, 100), FailInvalidRequest, false, true},
		{"invalid message (local)", fmt.Errorf("%w: body too long", ErrInvalidMessage), FailInvalidRequest, false, true},
		{"window closed 131047", pe(400, 131047), FailPolicy, false, false},
		{"opted out of marketing 131050", pe(400, 131050), FailPolicy, false, false},
		{"ecosystem limit 131049", pe(400, 131049), FailPolicy, false, false},
		{"account restricted 368", pe(400, 368), FailPolicy, false, true},
		{"spam restriction 131048", pe(400, 131048), FailPolicy, false, true},
		{"token expired 190", pe(401, 190), FailAuthConfig, false, true},
		{"permission 10", pe(403, 10), FailAuthConfig, false, true},
		{"template missing 132001", pe(400, 132001), FailAuthConfig, false, true},
		{"payment 131042", pe(400, 131042), FailAuthConfig, false, true},
		{"401 without code", pe(401, 0), FailAuthConfig, false, true},
		{"unrecognised code", pe(400, 987654), FailUnknown, false, true},
		{"unrecognised code on a 5xx", pe(503, 1), FailUnavailable, true, false},
		{"unrecognised code on a 500", pe(500, 2), FailTransient, true, false},
		{"400 without code", pe(400, 0), FailUnknown, false, true},
	}
	for _, c := range cases {
		f := Classify(c.err)
		if f.Class != c.class || f.Retry != c.retry || f.Operator != c.operator {
			t.Errorf("%s: got %+v, want class %s retry %v operator %v", c.name, f, c.class, c.retry, c.operator)
		}
	}
}

func TestRetryDelaysAreBoundedPerClass(t *testing.T) {
	if f := ClassifyCode(131056, 0); f.FirstDelay != 5*time.Minute {
		t.Fatalf("per-recipient rate limit waits longer: %s", f.FirstDelay)
	}
	if f := ClassifyCode(130429, 0); f.FirstDelay != time.Minute {
		t.Fatalf("throughput: %s", f.FirstDelay)
	}
	if f := Classify(errors.New("calling whatsapp api: EOF")); !f.Retry || f.FirstDelay != 5*time.Second {
		t.Fatalf("transport: %+v", f)
	}
}

func TestWebhookCodesClassifyWithoutHTTPStatus(t *testing.T) {
	if f := ClassifyCode(131047, 0); f.Class != FailPolicy || !f.WaitForRecipient {
		t.Fatalf("async window failure: %+v", f)
	}
	if f := ClassifyCode(131026, 0); f.Class != FailInvalidRecipient {
		t.Fatalf("async undeliverable: %+v", f)
	}
	if f := ClassifyCode(0, 0); f.Class != FailUnknown || f.Retry {
		t.Fatalf("a failure with no code is unknown and not retried: %+v", f)
	}
}

func TestTextFallbackForLegacyErrors(t *testing.T) {
	if f := Classify(errors.New(`whatsapp api returned status 400: {"error":{"message":"(#131047) Re-engagement message","code":131047}}`)); f.Class != FailPolicy {
		t.Fatalf("legacy text: %+v", f)
	}
	if f := Classify(errors.New(`whatsapp api returned status 400: {"error":{"code":130429}}`)); f.Class != FailRateLimited || !f.Retry {
		t.Fatalf("a 400 that is throttling is retried: %+v", f)
	}
}
