package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// RED metrics for every HTTP request this process serves (PRD §17.2).
// route is the matched mux pattern ("POST /v1/jobs/{id}"), not the raw
// URL path — the raw path has one label value per job ID, which would
// make this metric's cardinality grow without bound as jobs accumulate.
var (
	httpRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "anvil_http_requests_total",
		Help: "Total HTTP requests, by route, method, and response status.",
	}, []string{"route", "method", "status"})

	httpRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "anvil_http_request_duration_seconds",
		Help:    "HTTP request duration in seconds, by route.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route"})
)

// recordMetrics wraps next with the RED counters above. It must sit
// directly around mux, not further out in the middleware chain — the
// matched route is only available on the request via r.Pattern() once
// ServeMux has actually dispatched it, which happens inside next.
func recordMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()

		next.ServeHTTP(rec, r)

		route := r.Pattern
		if route == "" {
			route = "unmatched" // no mux entry matched (a 404 for an unknown path)
		}
		httpRequestsTotal.WithLabelValues(route, r.Method, strconv.Itoa(rec.status)).Inc()
		httpRequestDuration.WithLabelValues(route).Observe(time.Since(start).Seconds())
	})
}

// statusRecorder captures the status code a handler writes, since
// http.ResponseWriter itself doesn't expose it after the fact.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Flush passes through to the wrapped ResponseWriter's Flusher when it
// has one — GET /v1/jobs/{id}/events (SSE) depends on every layer
// between it and the client forwarding Flush, or events queue instead
// of reaching the browser as they're published.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
