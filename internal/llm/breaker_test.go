package llm

import (
	"testing"
	"time"
)

type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func TestBreaker_ClosedAllowsCalls(t *testing.T) {
	clk := &fakeClock{now: time.Now()}
	b := newBreaker(5, 60*time.Second, 120*time.Second, clk)

	if !b.allow() {
		t.Fatal("closed breaker must allow calls")
	}
}

func TestBreaker_OpensAfterThresholdFailuresWithinWindow(t *testing.T) {
	clk := &fakeClock{now: time.Now()}
	b := newBreaker(5, 60*time.Second, 120*time.Second, clk)

	for i := 0; i < 5; i++ {
		if !b.allow() {
			t.Fatalf("call %d: breaker should still be closed", i)
		}
		b.recordFailure()
	}

	if b.allow() {
		t.Fatal("breaker should be open after 5 failures in the window")
	}
}

func TestBreaker_FailuresOutsideWindowDoNotAccumulate(t *testing.T) {
	clk := &fakeClock{now: time.Now()}
	b := newBreaker(5, 60*time.Second, 120*time.Second, clk)

	for i := 0; i < 4; i++ {
		b.allow()
		b.recordFailure()
	}
	clk.advance(61 * time.Second) // outside the 60s window: old failures expire
	b.allow()
	b.recordFailure() // only 1 failure now within the window

	if !b.allow() {
		t.Fatal("breaker should still be closed: prior failures aged out of the window")
	}
}

func TestBreaker_OpenRejectsUntilCooldownElapses(t *testing.T) {
	clk := &fakeClock{now: time.Now()}
	b := newBreaker(1, 60*time.Second, 120*time.Second, clk)

	b.allow()
	b.recordFailure() // 1 failure trips a threshold-1 breaker

	if b.allow() {
		t.Fatal("breaker should reject immediately after opening")
	}

	clk.advance(119 * time.Second)
	if b.allow() {
		t.Fatal("breaker should still reject: cooldown has not elapsed")
	}

	clk.advance(2 * time.Second) // now 121s elapsed, past the 120s cooldown
	if !b.allow() {
		t.Fatal("breaker should allow one half-open trial after cooldown")
	}
}

func TestBreaker_HalfOpenAllowsOnlyOneTrialCall(t *testing.T) {
	clk := &fakeClock{now: time.Now()}
	b := newBreaker(1, 60*time.Second, 120*time.Second, clk)
	b.allow()
	b.recordFailure()
	clk.advance(121 * time.Second)

	if !b.allow() {
		t.Fatal("first call after cooldown should be the half-open trial")
	}
	if b.allow() {
		t.Fatal("a second concurrent call must not also be granted a trial")
	}
}

func TestBreaker_SuccessfulHalfOpenTrialCloses(t *testing.T) {
	clk := &fakeClock{now: time.Now()}
	b := newBreaker(1, 60*time.Second, 120*time.Second, clk)
	b.allow()
	b.recordFailure()
	clk.advance(121 * time.Second)
	b.allow() // consumes the trial
	b.recordSuccess()

	if !b.allow() {
		t.Fatal("breaker should be fully closed after a successful trial")
	}
}

func TestBreaker_FailedHalfOpenTrialReopens(t *testing.T) {
	clk := &fakeClock{now: time.Now()}
	b := newBreaker(1, 60*time.Second, 120*time.Second, clk)
	b.allow()
	b.recordFailure()
	clk.advance(121 * time.Second)
	b.allow() // consumes the trial
	b.recordFailure()

	if b.allow() {
		t.Fatal("a failed trial should reopen the breaker, not close it")
	}
}
