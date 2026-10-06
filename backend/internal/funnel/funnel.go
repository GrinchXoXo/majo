// Package funnel defines the core marketplace funnel stages Majo measures
// (Phase 3.1 §23A). It is deliberately not an analytics platform: a fixed
// list of stages counted from the system-of-record tables, so the evidence
// for building discovery, personalization, recommendations, proactive
// messaging or Spotlight exists before those decisions are made.
// See docs/analytics/funnel.md.
package funnel

import (
	"context"
	"time"
)

// Stage is one counted funnel step over a time window.
type Stage struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Count  int64  `json:"count"`
	Source string `json:"source"`         // the table/column the count comes from
	Note   string `json:"note,omitempty"` // caveat, e.g. read receipts are optional
}

// Report is the funnel over [Since, Until).
type Report struct {
	Since  time.Time `json:"since"`
	Until  time.Time `json:"until"`
	Stages []Stage   `json:"stages"`
}

// Reader computes the funnel. The Postgres implementation counts from
// entity tables (authoritative) rather than events (best-effort).
type Reader interface {
	Funnel(ctx context.Context, since, until time.Time) ([]Stage, error)
}
