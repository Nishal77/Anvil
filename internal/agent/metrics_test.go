package agent

import (
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/prometheus/client_golang/prometheus"
)

// testCounterValue reads c's current value directly via the
// Prometheus Metric.Write protocol — used to assert a metric actually
// incremented, not just that the code compiled around it. Avoids
// pulling in prometheus/client_golang/prometheus/testutil (and its
// transitive kylelemons/godebug dependency) for one value read.
func testCounterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter value: %v", err)
	}
	return m.GetCounter().GetValue()
}
