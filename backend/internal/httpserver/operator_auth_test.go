package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOperatorAuth(t *testing.T) {
	const tok = "s3cret-operator-token-xyz"
	a := NewOperatorAuth(tok)
	ran := 0
	h := a.Require(func(w http.ResponseWriter, r *http.Request) { ran++; w.WriteHeader(204) })
	cases := []struct {
		header string
		want   int
	}{
		{"", 401},
		{"Bearer", 401},
		{"Bearer ", 401},
		{"Basic " + tok, 401},
		{"Bearer wrong", 401},
		{"Bearer " + tok + "x", 401},
		{"Bearer " + tok, 204},
		{"bearer " + tok, 204},
		{"Bearer  " + tok + " ", 204},
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "/api/supply-listings/x/publish", nil)
		if c.header != "" {
			req.Header.Set("Authorization", c.header)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != c.want {
			t.Errorf("%q: got %d want %d", c.header, rec.Code, c.want)
		}
		if c.want == 401 && rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%q: 401 without WWW-Authenticate", c.header)
		}
	}
	if ran != 3 {
		t.Fatalf("handler ran %d times, want 3", ran)
	}
	if a.Open() {
		t.Fatal("a configured token must not be open")
	}
	open := NewOperatorAuth("")
	if !open.Open() || !open.Authorized(httptest.NewRequest("GET", "/", nil)) {
		t.Fatal("no token (development) keeps the existing open boundary")
	}
}
