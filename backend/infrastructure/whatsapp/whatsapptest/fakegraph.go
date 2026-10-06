// Package whatsapptest provides a deterministic stand-in for the Meta Graph
// media endpoint, for tests only. No live Meta account is ever used.
//
//	GET /{media-id}           -> {"url": ".../download/{media-id}?sig=..", "mime_type", "sha256", "file_size", "id"}
//	GET /download/{media-id}  -> the media bytes
//
// Both require "Authorization: Bearer <token>", like the real API. Each
// media ID can be scripted to fail its next N metadata or download calls
// with given HTTP statuses (404, 401, 403, 429, 500, 502, 503 ...), to
// return a malformed metadata body, to declare a different size, or to
// truncate its body.
package whatsapptest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

type entry struct {
	data         []byte
	mime         string
	metaFailures []int
	dlFailures   []int
	rawMetadata  string
	declaredSize int64
	truncateTo   int
}

// FakeGraph is an httptest server emulating the Graph media endpoints.
type FakeGraph struct {
	Server *httptest.Server
	Token  string

	mu            sync.Mutex
	media         map[string]*entry
	metadataCalls map[string]int
	downloadCalls map[string]int
	authHeaders   []string
}

// New starts a fake Graph server. Close it with Close().
func New(token string) *FakeGraph {
	f := &FakeGraph{Token: token, media: map[string]*entry{}, metadataCalls: map[string]int{}, downloadCalls: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *FakeGraph) Close() { f.Server.Close() }

// URL is the API base to pass to whatsapp.NewCloudAPIProvider.
func (f *FakeGraph) URL() string { return f.Server.URL }

// AddMedia registers bytes for a media ID. mime is what the metadata claims.
func (f *FakeGraph) AddMedia(id string, data []byte, mime string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.media[id] = &entry{data: data, mime: mime}
}

// FailMetadata makes the next len(statuses) metadata calls for id fail.
func (f *FakeGraph) FailMetadata(id string, statuses ...int) {
	f.with(id, func(e *entry) { e.metaFailures = statuses })
}

// FailDownload makes the next len(statuses) byte downloads for id fail.
func (f *FakeGraph) FailDownload(id string, statuses ...int) {
	f.with(id, func(e *entry) { e.dlFailures = statuses })
}

// MalformedMetadata makes metadata for id return this raw body with 200.
func (f *FakeGraph) MalformedMetadata(id, raw string) {
	f.with(id, func(e *entry) { e.rawMetadata = raw })
}

// DeclareSize overrides the file_size the metadata reports.
func (f *FakeGraph) DeclareSize(id string, n int64) {
	f.with(id, func(e *entry) { e.declaredSize = n })
}

// TruncateOnce makes the next download return only n bytes.
func (f *FakeGraph) TruncateOnce(id string, n int) { f.with(id, func(e *entry) { e.truncateTo = n }) }

func (f *FakeGraph) with(id string, fn func(*entry)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.media[id]
	if e == nil {
		e = &entry{}
		f.media[id] = e
	}
	fn(e)
}

// Calls returns how many metadata and download requests id received.
func (f *FakeGraph) Calls(id string) (metadata, download int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.metadataCalls[id], f.downloadCalls[id]
}

// AuthHeaders returns every Authorization header received.
func (f *FakeGraph) AuthHeaders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.authHeaders...)
}

func (f *FakeGraph) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authHeaders = append(f.authHeaders, r.Header.Get("Authorization"))
	if r.Method != http.MethodGet {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.Token {
		writeErr(w, http.StatusUnauthorized, 190, "Invalid OAuth access token")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	if id, ok := strings.CutPrefix(path, "download/"); ok {
		f.downloadCalls[id]++
		e := f.media[id]
		if e == nil {
			writeErr(w, http.StatusNotFound, 100, "not found")
			return
		}
		if len(e.dlFailures) > 0 {
			st := e.dlFailures[0]
			e.dlFailures = e.dlFailures[1:]
			writeErr(w, st, 0, "scripted download failure")
			return
		}
		body := e.data
		if e.truncateTo > 0 {
			body = body[:e.truncateTo]
			e.truncateTo = 0
		}
		w.Header().Set("Content-Type", e.mime)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}

	id := path
	f.metadataCalls[id]++
	e := f.media[id]
	if e == nil || e.data == nil && e.rawMetadata == "" {
		writeErr(w, http.StatusNotFound, 100, fmt.Sprintf("Unsupported get request. Object with ID '%s' does not exist", id))
		return
	}
	if len(e.metaFailures) > 0 {
		st := e.metaFailures[0]
		e.metaFailures = e.metaFailures[1:]
		writeErr(w, st, 0, "scripted metadata failure")
		return
	}
	if e.rawMetadata != "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(e.rawMetadata))
		return
	}
	size := int64(len(e.data))
	if e.declaredSize != 0 {
		size = e.declaredSize
	}
	sum := sha256.Sum256(e.data)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"messaging_product": "whatsapp",
		"url":               fmt.Sprintf("%s/download/%s?sig=short-lived", f.Server.URL, id),
		"mime_type":         e.mime,
		"sha256":            hex.EncodeToString(sum[:]),
		"file_size":         size,
		"id":                id,
	})
}

func writeErr(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "code": code}})
}
