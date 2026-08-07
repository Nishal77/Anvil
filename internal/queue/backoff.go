package queue

import (
	"math"
	"math/rand/v2"
	"time"
)

const backoffCap = 5 * time.Minute

// NextRunAfter computes FR-014's full-jitter backoff:
// rand.Float64() * min(2^attempt seconds, cap) — not
// min(2^attempt, cap) + rand. Full jitter decorrelates retries across
// many jobs backing off at once; the additive form does not, since every
// job still waits at least min(2^attempt, cap).
func NextRunAfter(clk Clock, attempt int) time.Time {
	ceiling := time.Duration(math.Pow(2, float64(attempt))) * time.Second
	if ceiling > backoffCap {
		ceiling = backoffCap
	}
	jittered := time.Duration(rand.Float64() * float64(ceiling))
	return clk.Now().Add(jittered)
}
