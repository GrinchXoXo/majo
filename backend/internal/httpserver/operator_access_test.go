package httpserver

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

type fakeOperatorStore struct {
	byHash map[[32]byte]Principal
	err    error
}

func (f *fakeOperatorStore) FindActiveByTokenHash(_ context.Context, h [32]byte) (Principal, bool, error) {
	if f.err != nil {
		return Principal{}, false, f.err
	}
	p, ok := f.byHash[h]
	return p, ok, nil
}

func TestNamedOperatorRoles(t *testing.T) {
	const shared = "shared-break-glass-token"
	adaID, bobID := uuid.New(), uuid.New()
	store := &fakeOperatorStore{byHash: map[[32]byte]Principal{
		sha256.Sum256([]byte("ada-token")): {ID: &adaID, Name: "Ada", Role: RoleOperator},
		sha256.Sum256([]byte("bob-token")): {ID: &bobID, Name: "Bob", Role: RoleViewer},
	}}
	a := NewOperatorAuth(shared)
	a.SetStore(store, 2, nil)

	var seen Principal
	handler := func(w http.ResponseWriter, r *http.Request) { seen = PrincipalFrom(r.Context()); w.WriteHeader(204) }
	read := a.RequireViewer(handler)
	write := a.Require(handler)

	cases := []struct {
		name, token string
		write       bool
		want        int
		who         string
	}{
		{"no token read", "", false, 401, ""},
		{"no token write", "", true, 401, ""},
		{"unknown token", "nope", false, 401, ""},
		{"viewer reads", "bob-token", false, 204, "Bob"},
		{"viewer cannot write", "bob-token", true, 403, ""},
		{"operator reads", "ada-token", false, 204, "Ada"},
		{"operator writes", "ada-token", true, 204, "Ada"},
		{"shared token writes as operator", shared, true, 204, "operator"},
	}
	for _, c := range cases {
		seen = Principal{}
		req := httptest.NewRequest("GET", "/api/x", nil)
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		rec := httptest.NewRecorder()
		if c.write {
			write(rec, req)
		} else {
			read(rec, req)
		}
		if rec.Code != c.want {
			t.Errorf("%s: got %d want %d", c.name, rec.Code, c.want)
		}
		if c.want == 204 && seen.Name != c.who {
			t.Errorf("%s: principal %q want %q", c.name, seen.Name, c.who)
		}
	}
	// The named operator's ID reaches handlers (recorded as actor_id).
	req := httptest.NewRequest("POST", "/api/x", nil)
	req.Header.Set("Authorization", "Bearer ada-token")
	write(httptest.NewRecorder(), req)
	if seen.ID == nil || *seen.ID != adaID {
		t.Fatalf("operator ID not propagated: %+v", seen)
	}
}

func TestOperatorStoreErrorFailsClosed(t *testing.T) {
	a := NewOperatorAuth("")
	a.SetStore(&fakeOperatorStore{err: errors.New("db down")}, 1, nil)
	if a.Open() {
		t.Fatal("active named operators must close the development open mode")
	}
	req := httptest.NewRequest("GET", "/api/requests", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	a.RequireViewer(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })(rec, req)
	if rec.Code != 401 {
		t.Fatalf("store error must deny, got %d", rec.Code)
	}
}

func TestOpenDevelopmentModeWithoutOperators(t *testing.T) {
	a := NewOperatorAuth("")
	a.SetStore(&fakeOperatorStore{}, 0, nil)
	if !a.Open() {
		t.Fatal("no token and no named operators keeps development open")
	}
}

func TestCORSAllowList(t *testing.T) {
	h := CORS([]string{"http://localhost:3000/", " https://ops.majo.example "})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	cases := []struct {
		origin, method string
		preflight      bool
		wantCode       int
		wantACAO       string
	}{
		{"http://localhost:3000", "GET", false, 200, "http://localhost:3000"},
		{"https://ops.majo.example", "GET", false, 200, "https://ops.majo.example"},
		{"https://evil.example", "GET", false, 200, ""},
		{"", "GET", false, 200, ""},
		{"http://localhost:3000", "OPTIONS", true, 204, "http://localhost:3000"},
		{"https://evil.example", "OPTIONS", true, 403, ""},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, "/api/requests", nil)
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		if c.preflight {
			req.Header.Set("Access-Control-Request-Method", "POST")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.wantCode || rec.Header().Get("Access-Control-Allow-Origin") != c.wantACAO {
			t.Errorf("%s %s: got %d %q want %d %q", c.method, c.origin, rec.Code,
				rec.Header().Get("Access-Control-Allow-Origin"), c.wantCode, c.wantACAO)
		}
	}
}

func TestPageParams(t *testing.T) {
	cases := []struct {
		q             string
		limit, offset int
		paged, bad    bool
	}{
		{"", 50, 0, false, false},
		{"limit=10&offset=20", 10, 20, true, false},
		{"limit=0", 0, 0, false, true},
		{"limit=201", 0, 0, false, true},
		{"limit=x", 0, 0, false, true},
		{"offset=-1", 0, 0, false, true},
	}
	for _, c := range cases {
		l, o, p, err := pageParams(httptest.NewRequest("GET", "/api/events?"+c.q, nil))
		if (err != nil) != c.bad || (!c.bad && (l != c.limit || o != c.offset || p != c.paged)) {
			t.Errorf("%q: got %d %d %v %v", c.q, l, o, p, err)
		}
	}
}
