package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/middleman/backend/internal/monetization"
)

type fakeLedger struct{}

func (fakeLedger) Summary(context.Context) (int, map[string]int64, error) {
	return 0, map[string]int64{}, nil
}

func TestMonetizationView(t *testing.T) {
	h := NewMonetizationHandlers(monetization.Default(), fakeLedger{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	h.Get(rec, httptest.NewRequest(http.MethodGet, "/api/monetization", nil))
	var body struct {
		Stage    string `json:"stage"`
		Provider string `json:"provider"`
		Lines    []struct {
			Line   string `json:"line"`
			On     bool   `json:"on"`
			EnvVar string `json:"env_var"`
		} `json:"lines"`
		Ledger struct {
			Entries int `json:"entries"`
		} `json:"ledger"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if body.Stage != "A" || body.Provider != "none" || len(body.Lines) != 7 || body.Lines[0].EnvVar != "FEATURE_LEAD_CREDITS" {
		t.Fatalf("%+v", body)
	}
	for _, l := range body.Lines {
		if l.On {
			t.Fatalf("%s on", l.Line)
		}
	}
}
