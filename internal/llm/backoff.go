package llm

import (
	"math"
	"math/rand/v2"
	"time"
)

// llmBackoffCap bounds retry delay within a single Router.Complete
// call — much shorter than queue's job-level backoff cap, since this
// is an in-request retry loop, not a requeue.
const llmBackoffCap = 10 * time.Second

// fullJitterDelay computes FR-035's retry delay:
// rand.Float64() * min(2^attempt seconds, cap). Full jitter (not
// additive jitter) decorrelates retries across concurrent jobs backing
// off at once — the same formula as queue.NextRunAfter (internal/queue
// /backoff.go), duplicated rather than imported because llm may not
// depend on queue (CLAUDE.md PK5).
func fullJitterDelay(attempt int) time.Duration {
	ceiling := time.Duration(math.Pow(2, float64(attempt))) * time.Second
	if ceiling > llmBackoffCap {
		ceiling = llmBackoffCap
	}
	return time.Duration(rand.Float64() * float64(ceiling))
}
