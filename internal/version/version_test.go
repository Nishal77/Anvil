package version

import (
	"strings"
	"testing"
)

func TestString_IncludesVersionCommitAndBuildDate(t *testing.T) {
	t.Parallel()

	got := String()

	for _, want := range []string{Version, Commit, BuildDate} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, want it to contain %q", got, want)
		}
	}
}
