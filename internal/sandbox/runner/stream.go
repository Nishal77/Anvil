package runner

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
)

// jobControlNotice matches the "[1]+  Done ..." style line bash prints to
// stderr when a background job finishes (see the set -m comment in
// runInGroup, exec.go). It's bash's own bookkeeping, not something the
// command actually printed, so we filter it out — otherwise it would show
// up as confusing, unexplained output.
var jobControlNotice = regexp.MustCompile(`^\[\d+\][+-]?\s+(Done|Running|Stopped|Terminated|Killed)\b`)

// maxScanTokenBytes bounds how long a single line of a container's
// output can be. A compiler can print one very long line, and the
// default scanner limit (64 KiB) would fail outright on that. This needs
// to be generous enough that real output never gets silently dropped.
const maxScanTokenBytes = 1 << 20 // 1 MiB

// scanLines reads r one line at a time and calls onChunk for each line
// right as it's produced — it's never held back until the command
// finishes. It returns once r hits the end of input (the normal case,
// once the writer side closes) or hits a read error, including a line
// longer than maxScanTokenBytes (bufio.ErrTooLong) — returned rather
// than swallowed, since a caller that never learns the stream was cut
// short would report the command as having succeeded with silently
// truncated output. The caller still finds out the command actually
// finished from its exit code, not from this function returning.
func scanLines(r io.Reader, stream string, onChunk func(stream string, data []byte)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxScanTokenBytes)
	for scanner.Scan() {
		if jobControlNotice.Match(scanner.Bytes()) {
			continue
		}
		// Scanner reuses its internal buffer; the chunk must be copied
		// before handing it to a caller that may retain it past the next
		// Scan() call.
		onChunk(stream, append([]byte(nil), scanner.Bytes()...))
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("runner: scan %s: %w", stream, err)
	}
	return nil
}
