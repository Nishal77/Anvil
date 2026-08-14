package agent

import "fmt"

// defaultMaxObservationBytes is FR-024's default MAX_OBSERVATION_BYTES.
const defaultMaxObservationBytes = 8 * 1024

// truncateObservation caps s at maxBytes, keeping both the head and
// the tail with an explicit elision marker between them — never just
// the head. A compiler or test runner's actual error is conventionally
// the LAST thing printed to stderr; a head-only truncation would show
// the model everything except the one line it needs to fix.
func truncateObservation(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}

	half := maxBytes / 2
	elided := len(s) - maxBytes
	marker := fmt.Sprintf("\n[... %d bytes elided ...]\n", elided)
	// The marker itself consumes budget too, or a maxBytes-sized input
	// could still slightly exceed maxBytes once the marker is spliced in.
	half -= len(marker) / 2
	if half < 0 {
		half = 0
	}

	head := s[:half]
	tail := s[len(s)-half:]
	return head + marker + tail
}
