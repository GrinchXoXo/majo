package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRewriteTone(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tone/rewrite" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var b struct{ Text string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		got = b.Text
		if strings.Contains(b.Text, "nokey") {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"detail":"tone rewriting unavailable"}`))
			return
		}
		_, _ = w.Write([]byte(`{"text":"How far [[F1]]","model":"m1"}`))
	}))
	defer srv.Close()
	p := NewHTTPProvider(srv.URL)
	text, model, err := p.RewriteTone(context.Background(), "Hello [[F1]]")
	if err != nil || text != "How far [[F1]]" || model != "m1" || got != "Hello [[F1]]" {
		t.Fatalf("%q %q %v (sent %q)", text, model, err, got)
	}
	if _, _, err := p.RewriteTone(context.Background(), "nokey"); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("503 is an error: %v", err)
	}
}
