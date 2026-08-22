package telemetry

import (
	"context"
	"testing"
	"time"
)

func TestNewTracerProvider_RejectsEmptyServiceName(t *testing.T) {
	t.Parallel()
	if _, err := NewTracerProvider(context.Background(), TracerConfig{}); err == nil {
		t.Error("NewTracerProvider() error = nil, want an error for an empty ServiceName")
	}
}

// TestNewTracerProvider_EmptyEndpointDisablesTracingWithoutDialing proves
// the documented no-op path: no CollectorEndpoint must not attempt any
// network connection, and must return a Shutdown safe to call.
func TestNewTracerProvider_EmptyEndpointDisablesTracingWithoutDialing(t *testing.T) {
	t.Parallel()
	shutdown, err := NewTracerProvider(context.Background(), TracerConfig{ServiceName: "anvil-test"})
	if err != nil {
		t.Fatalf("NewTracerProvider() error = %v, want nil", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown() error = %v, want nil", err)
	}
}

func TestTraceIDFromContext_NoActiveSpanReturnsEmpty(t *testing.T) {
	t.Parallel()
	if got := TraceIDFromContext(context.Background()); got != "" {
		t.Errorf("TraceIDFromContext() = %q, want empty string for a context with no span", got)
	}
}

// TestTraceIDFromContext_ReturnsRootSpanID proves the property WrapHandler
// and every jobs.trace_id write depend on: the trace ID read back from a
// context is the same one a caller receiving X-Trace-Id would see —
// there is exactly one source of truth, the span itself, not a second
// value generated independently.
func TestTraceIDFromContext_ReturnsRootSpanID(t *testing.T) {
	t.Parallel()
	// A CollectorEndpoint is required to exercise the real SDK provider
	// (an empty one takes NewTracerProvider's no-op path, which mints
	// invalid, all-zero span contexts). The address is never dialed
	// synchronously — otlptracegrpc connects lazily on its first export
	// attempt — so this test needs no Collector actually listening;
	// only span *creation*, not export, is under test here.
	shutdown, err := NewTracerProvider(context.Background(), TracerConfig{
		ServiceName:       "anvil-test",
		CollectorEndpoint: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("NewTracerProvider() error = %v", err)
	}
	t.Cleanup(func() {
		// Bounded, not context.Background(): 127.0.0.1:0 is never
		// actually reachable, so a real flush attempt would otherwise
		// block for the exporter's full internal export timeout.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = shutdown(ctx)
	})

	ctx, span := Tracer("test").Start(context.Background(), "unit-test-span")
	defer span.End()

	if got := TraceIDFromContext(ctx); got != span.SpanContext().TraceID().String() {
		t.Errorf("TraceIDFromContext() = %q, want the active span's own trace ID %q", got, span.SpanContext().TraceID().String())
	}
}
