// Package slogotel correlates slog records with the active OpenTelemetry
// span context. It performs JSON log/trace correlation only; it is not an
// OTel log exporter.
package slogotel

import (
	"context"
	"errors"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// ContextHandler decorates another slog.Handler, stamping records with
// trace_id, span_id and trace_sampled whenever the request context carries a
// valid span context. Request-path code must log with the Context variants
// (InfoContext, ErrorContext, ...) for the correlation to appear.
//
// The correlation attrs are added to the record at Handle time, so standard
// slog semantics apply: inside a WithGroup scope they appear within that
// group (never dropped). Loggers used on request paths here don't open
// groups, keeping the fields top-level for log indexers.
type ContextHandler struct {
	inner slog.Handler
}

var _ slog.Handler = (*ContextHandler)(nil)

// NewContextHandler wraps inner.
func NewContextHandler(inner slog.Handler) (*ContextHandler, error) {
	if inner == nil {
		return nil, errors.New("slogotel: NewContextHandler requires an inner handler")
	}
	return &ContextHandler{inner: inner}, nil
}

// Enabled implements slog.Handler.
func (h *ContextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle implements slog.Handler. The record is cloned before mutation, as
// required by the slog.Handler contract for retained records.
func (h *ContextHandler) Handle(ctx context.Context, record slog.Record) error {
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return h.inner.Handle(ctx, record)
	}
	record = record.Clone()
	record.AddAttrs(
		slog.String("trace_id", spanContext.TraceID().String()),
		slog.String("span_id", spanContext.SpanID().String()),
		slog.Bool("trace_sampled", spanContext.IsSampled()),
	)
	return h.inner.Handle(ctx, record)
}

// WithAttrs implements slog.Handler; the wrapper is preserved so derived
// loggers keep emitting trace fields.
func (h *ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ContextHandler{inner: h.inner.WithAttrs(attrs)}
}

// WithGroup implements slog.Handler; the wrapper is preserved so derived
// loggers keep emitting trace fields.
func (h *ContextHandler) WithGroup(name string) slog.Handler {
	return &ContextHandler{inner: h.inner.WithGroup(name)}
}
