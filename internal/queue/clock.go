package queue

import "time"

// Clock abstracts time so tests never sleep (CLAUDE.md T4). Production
// code gets realClock{}; tests inject their own.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// realClock is the production Clock, backed by the time package.
type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
