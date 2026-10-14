// Package logging provides structured logging for the Majo backend.
// Phase 0 requirement: logs must include timestamp, level, service, request_id,
// event_type, entity_id and message where relevant, without standing up a full
// observability platform.
package logging

import (
	"context"
	"log/slog"
	"os"
)

type ctxKey string

const requestIDKey ctxKey = "request_id"

// New builds the application-wide structured logger. JSON output is used so
// logs remain machine-parseable once volume grows enough to need aggregation.
func New(service string) *slog.Logger {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	return slog.New(handler).With("service", service)
}

// WithRequestID attaches a request ID to a context so downstream logging
// calls can include it without threading it through every function signature.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey, requestID)
}

// RequestIDFromContext extracts the request ID previously attached to ctx,
// returning an empty string if none is present.
func RequestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

// FromContext returns a logger enriched with the request ID found in ctx, if any.
func FromContext(ctx context.Context, base *slog.Logger) *slog.Logger {
	if id := RequestIDFromContext(ctx); id != "" {
		return base.With("request_id", id)
	}
	return base
}
