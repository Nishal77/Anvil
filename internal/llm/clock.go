package llm

import "time"

// clock abstracts time.Now so breaker and budget tests never sleep
// (CLAUDE.md T4) — a fakeClock in tests advances instantly.
type clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
