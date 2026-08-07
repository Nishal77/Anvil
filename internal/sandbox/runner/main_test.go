package runner

import (
	"fmt"
	"os"
	"testing"

	"go.uber.org/goleak"
)

// TestMain checks that no goroutine started by this package's tests
// (poll loops, kill timers, reaper loops) is still running when the
// tests finish. We use goleak.Find instead of goleak.VerifyTestMain
// because VerifyTestMain calls os.Exit directly, which would skip any
// pending cleanup (real containers, running Servers) still waiting to run.
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
