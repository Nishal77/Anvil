package api

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain guards against goroutine leaks from Server.Run's listener
// goroutine (CLAUDE.md I-5, CODE-STANDARDS C7).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
