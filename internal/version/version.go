package version

// Version, Commit, and BuildDate are set at build time via the Makefile's
// LDFLAGS (-X .../internal/version.Version=... etc). They must be
// package-level vars, not fields on a struct returned by a constructor —
// -ldflags -X can only target a package-level string variable by its
// import path and name, so this is the one place CLAUDE.md §5.2's ban on
// package-level mutable state does not apply: these are written exactly
// once, at link time, before main() ever runs, and are never mutated by
// running code.
var (
	// Version is the git tag or "dev" for an untagged build.
	Version = "dev"
	// Commit is the short git commit hash, or "unknown".
	Commit = "unknown"
	// BuildDate is the RFC 3339 build timestamp, or "unknown".
	BuildDate = "unknown"
)

// String returns a single-line "version (commit, built date)" summary,
// suitable for a --version flag.
func String() string {
	return Version + " (" + Commit + ", built " + BuildDate + ")"
}
