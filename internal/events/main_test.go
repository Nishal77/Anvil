package events

import (
	"fmt"
	"os"
	"testing"

	"go.uber.org/goleak"
)

// TestMain checks that no goroutine started by this package's tests
// (Hub's per-job dispatch loops, the Redis adapter's subscription
// goroutines) is still running once the tests finish.
func TestMain(m *testing.M) {
	os.Exit(runTestMain(m))
}

func runTestMain(m *testing.M) int {
	code := m.Run()
	if code == 0 {
		if err := goleak.Find(); err != nil {
			fmt.Fprintln(os.Stderr, "goroutine leak:", err)
			return 1
		}
	}
	return code
}
