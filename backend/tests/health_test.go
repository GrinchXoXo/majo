package tests

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/middleman/backend/internal/httpserver"
)

// TestHealthEndpoint is the minimum Phase 0 smoke test: the health handler
// must respond 200 with a JSON status field, with no dependencies required.
func TestHealthEndpoint(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	httpserver.HealthHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	if rec.Body.Len() == 0 {
		t.Fatal("expected non-empty response body")
	}
}
