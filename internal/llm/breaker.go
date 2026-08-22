package llm

import (
	"sync"
	"time"
)

type breakerState int

const (
	breakerClosed breakerState = iota
	breakerOpen
	breakerHalfOpen
)

// breaker is one provider's circuit breaker (FR-032): closed passes
// every call through; open fails fast without touching the provider;
// half-open allows exactly one trial call after Cooldown elapses.
// State is per instance — Router keeps one breaker per provider name,
// never a shared/global breaker, so one provider's outage cannot block
// calls to another (BUILD-PLAN W5 non-negotiable).
//
// ErrRateLimited (429) is never reported to recordFailure — see
// Router.Complete, which only calls recordFailure for
// ErrProviderUnavailable. A breaker that opened on free-tier rate
// limits would disable itself under normal operation.
type breaker struct {
	threshold int
	window    time.Duration
	cooldown  time.Duration
	clk       clock

	mu               sync.Mutex
	state            breakerState
	failures         []time.Time // failure timestamps within the last window
	openUntil        time.Time
	halfOpenInFlight bool
}

func newBreaker(threshold int, window, cooldown time.Duration, clk clock) *breaker {
	return &breaker{threshold: threshold, window: window, cooldown: cooldown, clk: clk, state: breakerClosed}
}

// allow reports whether a call may proceed. Closed and a granted
// half-open trial both return true; open (cooldown not yet elapsed)
// returns false.
func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case breakerClosed:
		return true
	case breakerHalfOpen:
		if b.halfOpenInFlight {
			return false
		}
		b.halfOpenInFlight = true
		return true
	default: // breakerOpen
		if b.clk.Now().Before(b.openUntil) {
			return false
		}
		b.state = breakerHalfOpen
		b.halfOpenInFlight = true
		return true
	}
}

// recordSuccess closes the breaker. A successful half-open trial call
// is what proves the provider recovered.
func (b *breaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = breakerClosed
	b.failures = nil
	b.halfOpenInFlight = false
}

// recordFailure counts one failure toward the threshold. Callers must
// only pass ErrProviderUnavailable here — never ErrRateLimited or
// ErrProviderFatal (see Router.Complete).
func (b *breaker) recordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.halfOpenInFlight = false
	if b.state == breakerHalfOpen {
		// The trial call failed: the provider hasn't recovered yet.
		b.open()
		return
	}

	now := b.clk.Now()
	cutoff := now.Add(-b.window)
	kept := b.failures[:0]
	for _, t := range b.failures {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	b.failures = append(kept, now)
	if len(b.failures) >= b.threshold {
		b.open()
	}
}

// isOpen reports whether the breaker is currently rejecting calls
// (PRD §17.2's anvil_llm_circuit_state: half-open, which is actively
// letting a trial call through, reads as closed, not open).
func (b *breaker) isOpen() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state == breakerOpen
}

// open must be called with mu held.
func (b *breaker) open() {
	b.state = breakerOpen
	b.openUntil = b.clk.Now().Add(b.cooldown)
	b.failures = nil
}
