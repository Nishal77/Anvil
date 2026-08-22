package telemetry

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// TracerConfig configures NewTracerProvider.
type TracerConfig struct {
	// ServiceName identifies this process in Tempo/Grafana — e.g.
	// "anvil-api" or "anvil-runner", one per cmd/ binary (PRD §17.1's
	// span tree labels each span with the component that emitted it).
	ServiceName string
	// CollectorEndpoint is the OTel Collector's OTLP/gRPC address, e.g.
	// "otel-collector:4317". Empty disables tracing: NewTracerProvider
	// registers otel's built-in no-op provider instead of a real one, so
	// a dev running without the observability compose profile never
	// blocks on a Collector that was never started, and every span.End()
	// call site stays exactly the same either way.
	CollectorEndpoint string
}

// Shutdown flushes any spans still buffered and closes the exporter's
// connection. Every caller of NewTracerProvider must defer it — an
// unflushed batch at process exit is a trace with a gap at its root.
type Shutdown func(ctx context.Context) error

// noopShutdown satisfies every NewTracerProvider caller's defer when
// tracing is disabled, or once shutdown has already run once.
func noopShutdown(context.Context) error { return nil }

// NewTracerProvider constructs the tracer provider for this process and
// registers it (and the W3C trace-context + baggage propagators every
// HTTP hop needs) as OpenTelemetry's global default, so every
// Tracer(component) call anywhere in the process — regardless of which
// package constructed the provider — resolves to it (PRD §17.1).
func NewTracerProvider(ctx context.Context, cfg TracerConfig) (Shutdown, error) {
	if cfg.ServiceName == "" {
		return nil, errors.New("telemetry: tracer config: ServiceName is required")
	}

	// Registered unconditionally, even with tracing disabled: an
	// incoming traceparent header must still round-trip through a
	// no-op span so a request that crosses into a traced process
	// downstream doesn't start a disconnected trace.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	if cfg.CollectorEndpoint == "" {
		return noopShutdown, nil
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.CollectorEndpoint),
		// The Collector sits on the same private network as every other
		// component in PRD §18.1's single-VPS topology — nothing here
		// crosses a public boundary, so there's no TLS termination to
		// configure for it.
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry: construct OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(cfg.ServiceName)))
	if err != nil {
		return nil, fmt.Errorf("telemetry: construct resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(provider)

	return func(shutdownCtx context.Context) error {
		if err := provider.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("telemetry: shut down tracer provider: %w", err)
		}
		return nil
	}, nil
}

// Tracer returns the named tracer every span component starts from.
// component matches PRD §17.1's span-tree labels — "api", "queue",
// "agent", "llm", "sandbox", "runner", "storage", "deploy" — so a span's
// instrumentation-scope name in Tempo tells a reader which package
// emitted it without needing an anvil.* attribute for that alone.
func Tracer(component string) trace.Tracer {
	return otel.Tracer("github.com/anvil-dev/anvil/" + component)
}

// TraceIDFromContext returns the hex-encoded OpenTelemetry trace ID of
// the span active in ctx, or "" if ctx carries no valid span context —
// tracing disabled, or called outside any span. This is the exact value
// PRD §17.1 requires stored on jobs.trace_id and returned as X-Trace-Id:
// the trace ID is not a separately generated value, it *is* the root
// span's ID, which is what makes "click a job, open its Grafana trace"
// (EG-3) a single lookup rather than a mapping this process would have
// to maintain.
func TraceIDFromContext(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

// ContextWithTraceID returns ctx carrying a synthetic remote parent span
// in traceID's trace, so the next span started from it belongs to that
// trace rather than starting a new one. This is how PRD §17.1's "one
// trace per job" holds across queue.Dispatcher's async boundary: a job's
// steps run later, on a worker goroutine with no live descendant of the
// HTTP request's own context — the only thing that survives that gap is
// jobs.trace_id itself. The parent span this synthesizes doesn't
// correspond to a real span Tempo ever received; only its trace ID
// matters, to group everything under one trace so a job's whole history
// — from the original request through every later step — is one lookup
// away, which is the actual property EG-3 needs.
//
// If traceID isn't a valid 32-hex-char trace ID (empty, because tracing
// was disabled when the job was created, is the common case), ctx is
// returned unchanged and the next span simply starts a new trace of its
// own instead of erroring.
func ContextWithTraceID(ctx context.Context, traceID string) context.Context {
	tid, err := trace.TraceIDFromHex(traceID)
	if err != nil {
		return ctx
	}
	var sid trace.SpanID
	if _, err := rand.Read(sid[:]); err != nil {
		return ctx
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     sid,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	return trace.ContextWithRemoteSpanContext(ctx, sc)
}
