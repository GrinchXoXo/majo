package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// MediaError is a failed media download. Permanent() tells the caller
// (the durable job worker) whether retrying can help. It never contains
// the access token or request headers.
type MediaError struct {
	Stage      string // "metadata" | "download"
	StatusCode int    // 0 for transport errors
	Reason     string
	permanent  bool
}

func (e *MediaError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("whatsapp media %s: status %d: %s", e.Stage, e.StatusCode, e.Reason)
	}
	return fmt.Sprintf("whatsapp media %s: %s", e.Stage, e.Reason)
}

func (e *MediaError) Permanent() bool { return e.permanent }

// DownloadMedia fetches inbound media by Meta media ID using the Graph
// media endpoint's two steps:
//
//  1. GET {api}/{media-id} (Bearer token) -> {url, mime_type, file_size, ...}.
//     The returned URL is short-lived (minutes), so it is never stored;
//     every attempt resolves a fresh one.
//  2. GET {url} (Bearer token) -> the bytes.
//
// At most maxBytes are read; a declared or actual size above it is a
// permanent error. Classification:
//   - retryable: transport errors/timeouts, 429, 5xx, 401/403 (credential
//     or configuration problems an operator can fix within the retry
//     window), a 404/410 on the short-lived download URL (re-resolved on
//     the next attempt), and truncated or empty bodies;
//   - permanent: unknown/expired media ID (400/404/410 on step 1), other
//     4xx, malformed metadata, oversized media.
//
// The returned MIME type is the provider's claim only; callers must
// validate the bytes themselves.
func (p *CloudAPIProvider) DownloadMedia(ctx context.Context, mediaID string, maxBytes int64) ([]byte, string, error) {
	if strings.TrimSpace(mediaID) == "" || strings.ContainsAny(mediaID, "/?#") {
		return nil, "", &MediaError{Stage: "metadata", Reason: "invalid media id", permanent: true}
	}

	// Step 1: resolve metadata and a fresh download URL.
	metaURL := fmt.Sprintf("%s/%s", strings.TrimRight(p.apiURL, "/"), url.PathEscape(mediaID))
	resp, err := p.authorizedGet(ctx, metaURL)
	if err != nil {
		return nil, "", &MediaError{Stage: "metadata", Reason: transportReason(err)}
	}
	var meta struct {
		URL      string `json:"url"`
		MimeType string `json:"mime_type"`
		FileSize int64  `json:"file_size"`
	}
	func() {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			err = json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&meta)
			return
		}
		err = statusError("metadata", resp, map[int]bool{http.StatusBadRequest: true, http.StatusNotFound: true, http.StatusGone: true})
	}()
	if err != nil {
		if me, ok := err.(*MediaError); ok {
			return nil, "", me
		}
		return nil, "", &MediaError{Stage: "metadata", Reason: "malformed metadata response", permanent: true}
	}
	if meta.URL == "" {
		return nil, "", &MediaError{Stage: "metadata", Reason: "metadata response has no download url", permanent: true}
	}
	if u, perr := url.Parse(meta.URL); perr != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, "", &MediaError{Stage: "metadata", Reason: "metadata response has an invalid download url", permanent: true}
	}
	if maxBytes > 0 && meta.FileSize > maxBytes {
		return nil, "", &MediaError{Stage: "metadata", Reason: fmt.Sprintf("declared size %d exceeds limit %d", meta.FileSize, maxBytes), permanent: true}
	}

	// Step 2: download the bytes, bounded.
	resp, err = p.authorizedGet(ctx, meta.URL)
	if err != nil {
		return nil, "", &MediaError{Stage: "download", Reason: transportReason(err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 404/410 here usually means the short-lived URL lapsed: retryable.
		return nil, "", statusError("download", resp, nil)
	}
	if maxBytes > 0 && resp.ContentLength > maxBytes {
		return nil, "", &MediaError{Stage: "download", Reason: fmt.Sprintf("content length %d exceeds limit %d", resp.ContentLength, maxBytes), permanent: true}
	}
	limit := maxBytes
	if limit <= 0 {
		limit = 64 << 20
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", &MediaError{Stage: "download", Reason: "reading body: " + transportReason(err)}
	}
	if int64(len(data)) > limit {
		return nil, "", &MediaError{Stage: "download", Reason: fmt.Sprintf("body exceeds limit %d", limit), permanent: true}
	}
	if len(data) == 0 {
		return nil, "", &MediaError{Stage: "download", Reason: "empty body"}
	}
	if meta.FileSize > 0 && int64(len(data)) != meta.FileSize {
		return nil, "", &MediaError{Stage: "download", Reason: fmt.Sprintf("truncated body: got %d of %d bytes", len(data), meta.FileSize)}
	}

	mime := meta.MimeType
	if mime == "" {
		mime = resp.Header.Get("Content-Type")
	}
	return data, mime, nil
}

func (p *CloudAPIProvider) authorizedGet(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.accessToken)
	return p.mediaClient.Do(req)
}

// statusError classifies a non-200 response. permanentCodes lists the
// statuses that are permanent at this stage; any other 4xx except
// 401/403/408/429 is permanent too, and 5xx is retryable.
func statusError(stage string, resp *http.Response, permanentCodes map[int]bool) *MediaError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	code := resp.StatusCode
	retryable := code >= 500 || code == http.StatusTooManyRequests || code == http.StatusRequestTimeout ||
		code == http.StatusUnauthorized || code == http.StatusForbidden
	if stage == "download" && (code == http.StatusNotFound || code == http.StatusGone) {
		retryable = true
	}
	if permanentCodes[code] {
		retryable = false
	}
	return &MediaError{Stage: stage, StatusCode: code, Reason: strings.TrimSpace(string(body)), permanent: !retryable}
}

// transportReason strips URLs from transport errors (the download URL is a
// short-lived signed link and does not belong in logs).
func transportReason(err error) string {
	if ue, ok := err.(*url.Error); ok {
		return ue.Op + ": " + ue.Err.Error()
	}
	return err.Error()
}
