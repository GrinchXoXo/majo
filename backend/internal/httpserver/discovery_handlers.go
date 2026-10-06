package httpserver

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/discovery"
	"github.com/middleman/backend/internal/distribution"
)

// Phase 4B: Discovery & Distribution operator endpoints. EVERY route is
// operator-only (OperatorAuth); none is a consumer endpoint.
//
//	GET  /api/discovery/settings                feature gate, caps, window, arms, ranking version
//	GET  /api/discovery/candidates              Discovery.Candidates for a hypothetical viewer (diagnostics)
//	GET  /api/discovery/preferences             consent records
//	POST /api/discovery/preferences             explicit staff/test consent record (INTERNAL, allowlisted only)
//	POST /api/discovery/consent/invite          send the opt-in prompt to an allowlisted staff number
//	POST /api/discovery/preview                 dry run (nothing persisted or sent)
//	POST /api/discovery/batches                 create a batch (PLANNED; idempotent per client_key)
//	GET  /api/discovery/batches                 list
//	GET  /api/discovery/batches/{id}            batch + deliveries
//	POST /api/discovery/batches/{id}/send       operator-triggered send (INTERNAL only)
//	GET  /api/discovery/deliveries/{id}         delivery + permitted-now decision + attribution
//	GET  /api/discovery/funnel                  discovery funnel (?batch_id=)
type DiscoveryHandlers struct {
	dist   *distribution.Service
	disc   *discovery.Service
	logger *slog.Logger
}

func NewDiscoveryHandlers(dist *distribution.Service, disc *discovery.Service, logger *slog.Logger) *DiscoveryHandlers {
	return &DiscoveryHandlers{dist: dist, disc: disc, logger: logger}
}

func (h *DiscoveryHandlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, distribution.ErrDiscoveryOff):
		writeErrorCode(w, http.StatusForbidden, "DISCOVERY_OFF", "discovery distribution is OFF (DISCOVERY_MODE)")
	case errors.Is(err, distribution.ErrNotPermitted):
		writeErrorCode(w, http.StatusForbidden, "NOT_INTERNAL_RECIPIENT", "only allowlisted staff test numbers may receive discovery in INTERNAL mode")
	case errors.Is(err, distribution.ErrOutsideWindow):
		writeErrorCode(w, http.StatusConflict, "OUTSIDE_DELIVERY_WINDOW", err.Error())
	case errors.Is(err, distribution.ErrBatchNotFound), errors.Is(err, distribution.ErrDeliveryNotFound):
		writeErrorCode(w, http.StatusNotFound, "NOT_FOUND", err.Error())
	case errors.Is(err, distribution.ErrUnknownBuyer):
		writeErrorCode(w, http.StatusNotFound, "UNKNOWN_BUYER", err.Error())
	case errors.Is(err, distribution.ErrInvalidInput):
		writeErrorCode(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
	default:
		h.logger.ErrorContext(r.Context(), "discovery request failed", "path", r.URL.Path, "error", err)
		writeErrorCode(w, http.StatusInternalServerError, "INTERNAL", "discovery request failed")
	}
}

// Settings handles GET /api/discovery/settings.
func (h *DiscoveryHandlers) Settings(w http.ResponseWriter, r *http.Request) {
	c := h.dist.Config()
	pc := h.disc.Engine().Policy().Config()
	writeJSON(w, http.StatusOK, map[string]any{
		"mode": c.Mode, "send_allowed": c.CanSend(), "internal_recipients": len(c.InternalRecipients),
		"arms": c.Arms, "max_deliveries_per_buyer": c.MaxDeliveriesPerBuyer, "cap_period": c.CapPeriod.String(),
		"exposure_window": c.ExposureWindow.String(), "listings_per_delivery": c.ListingsPerDelivery,
		"delivery_window": c.Window.Raw, "delivery_window_open": c.Window.Allows(time.Now()),
		"interaction_ttl": c.InteractionTTL.String(), "template_configured": c.Template.Name != "",
		"consent_wording": c.ConsentWording, "enquiry_confirmation": c.EnquiryConfirmation,
		"ranking_version": c.Ranking.Version(pc), "weights": c.Ranking.Weights, "exclude_stale": c.Ranking.ExcludeStale,
		"policy": map[string]any{
			"max_lifecycle_days": int(pc.MaxLifecycle / (24 * time.Hour)), "freshness_window": pc.FreshnessWindow.String(),
			"geo_days": map[string]int{"local": int(pc.GeoLocalAfter / (24 * time.Hour)), "nearby": int(pc.GeoNearbyAfter / (24 * time.Hour)),
				"state": int(pc.GeoStateAfter / (24 * time.Hour)), "region": int(pc.GeoRegionAfter / (24 * time.Hour))},
		},
	})
}

type scopeBody struct {
	Category string `json:"category"`
	Query    string `json:"query"`
	PriceMin *int64 `json:"price_min"`
	PriceMax *int64 `json:"price_max"`
	Limit    int    `json:"limit"`
}

func (s scopeBody) scope() discovery.Scope {
	return discovery.Scope{Category: strings.TrimSpace(s.Category), Query: strings.TrimSpace(s.Query), PriceMin: s.PriceMin,
		PriceMax: s.PriceMax, Limit: s.Limit, Purpose: "batch"}
}

// Candidates handles GET /api/discovery/candidates?location=&category=&query=&interests=&budget_max=&limit=
// — the Discovery contract for a hypothetical viewer, with reasons and
// the canonical eligibility/reach of every listing (diagnostics only).
func (h *DiscoveryHandlers) Candidates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	viewer := discovery.ViewerContext{Location: q.Get("location")}
	if v := q.Get("interests"); v != "" {
		viewer.Interests = strings.Split(v, ",")
	}
	if v := q.Get("budget_max"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeErrorCode(w, http.StatusBadRequest, "BAD_REQUEST", "budget_max must be an integer")
			return
		}
		viewer.BudgetMax = &n
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	scope := discovery.Scope{Category: q.Get("category"), Query: q.Get("query"), Limit: limit, Purpose: "browse"}
	res, err := h.disc.Candidates(r.Context(), viewer, scope, h.dist.Config().Ranking)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// Preferences handles GET /api/discovery/preferences.
func (h *DiscoveryHandlers) Preferences(w http.ResponseWriter, r *http.Request) {
	ps, err := h.dist.Preferences(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if ps == nil {
		ps = []*distribution.Preference{}
	}
	writeJSON(w, http.StatusOK, ps)
}

type staffPreferenceBody struct {
	Recipient string   `json:"recipient"`
	Status    *string  `json:"status"`
	Location  *string  `json:"location"`
	Interests []string `json:"interests"`
	Note      string   `json:"note"`
}

// SetPreference handles POST /api/discovery/preferences (staff records).
func (h *DiscoveryHandlers) SetPreference(w http.ResponseWriter, r *http.Request) {
	var b staffPreferenceBody
	if err := decodeStrict(r, &b); err != nil || strings.TrimSpace(b.Recipient) == "" {
		writeErrorCode(w, http.StatusBadRequest, "BAD_REQUEST", "recipient is required")
		return
	}
	sp := distribution.StaffPreference{Recipient: strings.TrimSpace(b.Recipient), Location: b.Location, Interests: b.Interests, Note: b.Note}
	if b.Status != nil {
		st := distribution.ConsentStatus(*b.Status)
		sp.Status = &st
	}
	p, err := h.dist.SetStaffPreference(r.Context(), sp)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// Invite handles POST /api/discovery/consent/invite {"recipient": "..."}.
func (h *DiscoveryHandlers) Invite(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Recipient string `json:"recipient"`
	}
	if err := decodeStrict(r, &b); err != nil || strings.TrimSpace(b.Recipient) == "" {
		writeErrorCode(w, http.StatusBadRequest, "BAD_REQUEST", "recipient is required")
		return
	}
	if err := h.dist.InviteConsent(r.Context(), strings.TrimSpace(b.Recipient)); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"invited": true})
}

type batchBody struct {
	ClientKey  string    `json:"client_key"`
	Scope      scopeBody `json:"scope"`
	Recipients []string  `json:"recipients"`
	Note       string    `json:"note"`
}

// Preview handles POST /api/discovery/preview (dry run).
func (h *DiscoveryHandlers) Preview(w http.ResponseWriter, r *http.Request) {
	var b batchBody
	if err := decodeStrict(r, &b); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	rep, err := h.dist.Preview(r.Context(), distribution.PlanRequest{Scope: b.Scope.scope(), Recipients: b.Recipients})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// CreateBatch handles POST /api/discovery/batches.
func (h *DiscoveryHandlers) CreateBatch(w http.ResponseWriter, r *http.Request) {
	var b batchBody
	if err := decodeStrict(r, &b); err != nil {
		writeErrorCode(w, http.StatusBadRequest, "BAD_REQUEST", "invalid request body")
		return
	}
	batch, created, err := h.dist.CreateBatch(r.Context(), distribution.PlanRequest{
		ClientKey: strings.TrimSpace(b.ClientKey), Scope: b.Scope.scope(), Recipients: b.Recipients, Note: b.Note, CreatedBy: "operator",
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"batch": batch, "created": created})
}

// Batches handles GET /api/discovery/batches.
func (h *DiscoveryHandlers) Batches(w http.ResponseWriter, r *http.Request) {
	bs, err := h.dist.Batches(r.Context(), 50)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if bs == nil {
		bs = []*distribution.Batch{}
	}
	writeJSON(w, http.StatusOK, bs)
}

func pathUUID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErrorCode(w, http.StatusBadRequest, "BAD_REQUEST", "invalid id")
		return uuid.Nil, false
	}
	return id, true
}

// Batch handles GET /api/discovery/batches/{id}.
func (h *DiscoveryHandlers) Batch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	b, ds, err := h.dist.Batch(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if ds == nil {
		ds = []*distribution.Delivery{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch": b, "deliveries": ds})
}

// Send handles POST /api/discovery/batches/{id}/send.
func (h *DiscoveryHandlers) Send(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	rep, err := h.dist.SendBatch(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// Delivery handles GET /api/discovery/deliveries/{id}.
func (h *DiscoveryHandlers) Delivery(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	d, err := h.dist.Delivery(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// Funnel handles GET /api/discovery/funnel?batch_id=.
func (h *DiscoveryHandlers) Funnel(w http.ResponseWriter, r *http.Request) {
	var batchID *uuid.UUID
	if v := r.URL.Query().Get("batch_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			writeErrorCode(w, http.StatusBadRequest, "BAD_REQUEST", "invalid batch_id")
			return
		}
		batchID = &id
	}
	f, err := h.dist.Funnel(r.Context(), batchID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, f)
}
