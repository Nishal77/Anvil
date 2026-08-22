package telemetry

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// WrapHandler instruments next with one HTTP server span per request,
// named "<method> <path>" (PRD §17.1's tree shows this span as
// "http.POST /v1/jobs") so the trace is legible without expanding into
// its attributes first. It extracts an inbound W3C traceparent header if
// the caller sent one, or starts a new root span otherwise — the latter
// is what makes the API edge the point PRD §17.1 means by "trace ID is
// generated at the API edge": the first hop with no incoming trace
// context is definitionally where a trace begins.
//
// operation is the fallback name for a request otelhttp can't format
// from (none in practice, since every request has a method and a path,
// but WithSpanNameFormatter's signature requires one).
func WrapHandler(next http.Handler, operation string) http.Handler {
	return otelhttp.NewHandler(next, operation,
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return "http." + r.Method + " " + r.URL.Path
		}),
	)
}

// ExtractTraceContext continues an inbound W3C traceparent header into
// next's request context, without otherwise touching the request or
// response — deliberately not otelhttp.NewHandler, which wraps
// http.ResponseWriter in a way that silently drops streaming responses'
// http.Flusher support in some configurations. The Runner's
// /sandboxes/{id}/exec endpoint streams command output chunk-by-chunk as
// it's produced (internal/sandbox/runner/handlers.go's handleExec); a
// wrapped ResponseWriter that can't be flushed makes the client block
// waiting for output that already left the handler but never left the
// process. Use this, not WrapHandler, for any handler that streams.
func ExtractTraceContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
