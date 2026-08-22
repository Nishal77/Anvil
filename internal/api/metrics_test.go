package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// TestRecordMetrics_LabelsRequestByRouteMethodAndStatus proves the RED
// metric this middleware feeds is queryable the way PRD §17.2 documents
// it — by the matched mux pattern, not the raw path, and by the status
// the handler actually wrote.
func TestRecordMetrics_LabelsRequestByRouteMethodAndStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	handler := recordMetrics(mux)
	labels := prometheus.Labels{"route": "GET /v1/jobs/{id}", "method": http.MethodGet, "status": "404"}

	// A delta, not an absolute value: httpRequestsTotal is a
	// package-level counter every test in this package that exercises
	// the full server chain shares, so an earlier test's requests may
	// already have incremented this exact label combination.
	before := counterValue(t, httpRequestsTotal, labels)

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs/abc-123", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("response status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if got := counterValue(t, httpRequestsTotal, labels) - before; got != 1 {
		t.Errorf("anvil_http_requests_total{route=\"GET /v1/jobs/{id}\",method=\"GET\",status=\"404\"} increased by %v, want 1", got)
	}
}

// TestRecordMetrics_UnmatchedRouteDoesNotExplodeCardinality proves a
// request no mux entry matches (an arbitrary 404) is labeled with a
// fixed "unmatched" route rather than the raw, attacker-controlled URL
// path — an unbounded label value on a counter is a cardinality attack
// surface (PRD §17.2's whole point in keying every metric off the
// matched pattern rather than the literal path).
func TestRecordMetrics_UnmatchedRouteDoesNotExplodeCardinality(t *testing.T) {
	mux := http.NewServeMux() // deliberately empty: nothing can match
	handler := recordMetrics(mux)
	labels := prometheus.Labels{"route": "unmatched", "method": http.MethodGet, "status": "404"}
	before := counterValue(t, httpRequestsTotal, labels)

	req := httptest.NewRequest(http.MethodGet, "/this/path/matches/nothing", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := counterValue(t, httpRequestsTotal, labels) - before; got != 1 {
		t.Errorf("anvil_http_requests_total{route=\"unmatched\",...} increased by %v, want 1", got)
	}
}

// counterValue reads back one label combination's current count from a
// CounterVec — promauto collectors are package-level, so tests read
// them through the standard client_golang testutil-free path (one
// Write call into a proto message) rather than needing a fresh registry
// per test.
func counterValue(t *testing.T, vec *prometheus.CounterVec, labels prometheus.Labels) float64 {
	t.Helper()
	var m dto.Metric
	if err := vec.With(labels).Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}
