package slogotel

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func newTestLogger(t *testing.T) (*slog.Logger, *bytes.Buffer, *ContextHandler) {
	t.Helper()
	var buf bytes.Buffer
	handler, err := NewContextHandler(slog.NewJSONHandler(&buf, nil))
	if err != nil {
		t.Fatalf("NewContextHandler: %v", err)
	}
	return slog.New(handler), &buf, handler
}

func lastLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	var entry map[string]any
	if err := json.Unmarshal(lines[len(lines)-1], &entry); err != nil {
		t.Fatalf("parse log line %q: %v", lines[len(lines)-1], err)
	}
	return entry
}

func sampledSpanContext() (context.Context, trace.SpanContext) {
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		SpanID:     trace.SpanID{0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x01, 0x02},
		TraceFlags: trace.FlagsSampled,
	})
	return trace.ContextWithSpanContext(context.Background(), spanContext), spanContext
}

func TestNewContextHandlerRejectsNilInner(t *testing.T) {
	if _, err := NewContextHandler(nil); err == nil {
		t.Fatal("nil inner handler: want error")
	}
}

func TestHandleWithoutSpanAddsNoTraceFields(t *testing.T) {
	logger, buf, _ := newTestLogger(t)
	logger.InfoContext(context.Background(), "plain")
	entry := lastLine(t, buf)
	for _, key := range []string{"trace_id", "span_id", "trace_sampled"} {
		if _, present := entry[key]; present {
			t.Errorf("field %q must be absent without a valid span context", key)
		}
	}
}

func TestHandleWithValidSpanAddsTraceFields(t *testing.T) {
	logger, buf, _ := newTestLogger(t)
	ctx, spanContext := sampledSpanContext()

	logger.InfoContext(ctx, "traced")
	entry := lastLine(t, buf)
	if entry["trace_id"] != spanContext.TraceID().String() {
		t.Errorf("trace_id = %v, want %s", entry["trace_id"], spanContext.TraceID())
	}
	if entry["span_id"] != spanContext.SpanID().String() {
		t.Errorf("span_id = %v, want %s", entry["span_id"], spanContext.SpanID())
	}
	if entry["trace_sampled"] != true {
		t.Errorf("trace_sampled = %v, want true", entry["trace_sampled"])
	}
}

func TestWithAttrsAndWithGroupPreserveWrapper(t *testing.T) {
	logger, buf, handler := newTestLogger(t)
	_ = logger

	derived := handler.WithAttrs([]slog.Attr{slog.String("component", "test")})
	if _, ok := derived.(*ContextHandler); !ok {
		t.Fatalf("WithAttrs returned %T, want *ContextHandler", derived)
	}
	grouped := handler.WithGroup("grp")
	if _, ok := grouped.(*ContextHandler); !ok {
		t.Fatalf("WithGroup returned %T, want *ContextHandler", grouped)
	}

	ctx, spanContext := sampledSpanContext()
	slog.New(derived).InfoContext(ctx, "derived")
	entry := lastLine(t, buf)
	if entry["component"] != "test" {
		t.Errorf("component attr lost: %v", entry)
	}
	if entry["trace_id"] != spanContext.TraceID().String() {
		t.Errorf("derived logger lost trace correlation: %v", entry)
	}

	slog.New(grouped).InfoContext(ctx, "grouped", "inner", "v")
	entry = lastLine(t, buf)
	group, ok := entry["grp"].(map[string]any)
	if !ok || group["inner"] != "v" {
		t.Fatalf("grouped attrs misplaced: %v", entry)
	}
	// Per slog semantics record attrs follow the open group; the correlation
	// fields must still be present there, never dropped.
	if group["trace_id"] != spanContext.TraceID().String() {
		t.Errorf("grouped logger lost trace correlation: %v", entry)
	}
}

func TestEnabledDelegates(t *testing.T) {
	var buf bytes.Buffer
	handler, err := NewContextHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err != nil {
		t.Fatalf("NewContextHandler: %v", err)
	}
	if handler.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Enabled(Info) = true, want delegation to Warn-level inner handler")
	}
	if !handler.Enabled(context.Background(), slog.LevelError) {
		t.Error("Enabled(Error) = false, want true")
	}
}
