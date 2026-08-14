#!/usr/bin/env bash
#
# check-invariants.sh — mechanical guards for the rules golangci-lint cannot express.
#
# These are the invariants in CLAUDE.md §3 and the naming rules in §4 that are
# structural rather than syntactic. Each check names the rule it enforces and
# tells the reader what to do instead, because a CI failure that only says
# "FAILED" costs more than it saves.
#
# Usage: make check-invariants   (or: ./scripts/check-invariants.sh)
# Exit:  0 = clean, 1 = one or more violations

set -euo pipefail

readonly RED=$'\033[0;31m' GREEN=$'\033[0;32m' YELLOW=$'\033[0;33m' DIM=$'\033[2m' RESET=$'\033[0m'

violations=0

fail() {
    local rule="$1" msg="$2" fix="$3"
    printf '%s✗ %s%s  %s\n' "$RED" "$rule" "$RESET" "$msg"
    printf '  %s→ %s%s\n' "$DIM" "$fix" "$RESET"
    violations=$((violations + 1))
}

pass() { printf '%s✓%s %s\n' "$GREEN" "$RESET" "$1"; }

# Source files only: exclude tests, generated code, vendor, and migrations.
go_sources() {
    git ls-files '*.go' \
        | grep -v '_test\.go$' \
        | grep -v '\.gen\.go$' \
        | grep -v '\.pb\.go$' \
        | grep -v '^vendor/' \
        | grep -v '^internal/storage/queries/'
}

all_go_files() {
    git ls-files '*.go' | grep -v '^vendor/'
}

printf '\n%s── Invariant checks ──────────────────────────────────%s\n\n' "$YELLOW" "$RESET"

# ---------------------------------------------------------------------------
# I-1 : all job status changes go through the guarded transition function
# ---------------------------------------------------------------------------
hits=$(go_sources | xargs grep -lniE 'UPDATE[[:space:]]+jobs[[:space:]]+SET[^;]*\bstatus\b' 2>/dev/null \
        | grep -v '^internal/queue/transition\.go$' \
        | grep -v '^internal/queue/claim\.go$' \
        | grep -v '^internal/queue/doc\.go$' || true)
if [[ -n "$hits" ]]; then
    fail "I-1" "job status mutated outside the guarded transition function:
      $(echo "$hits" | tr '\n' ' ')" \
      "Route the change through queue.Transition (docs/PRD.md §13.1). \
Scattered status writes are how state-machine invariants rot."
else
    pass "I-1  job status changes are centralised"
fi

# ---------------------------------------------------------------------------
# I-3 : no secret material in logs, prompts, or on disk
# ---------------------------------------------------------------------------
hits=$(go_sources | xargs grep -nE \
    'slog\.[A-Z][a-z]+\([^)]*"(password|token|secret|api_?key|credential|authorization)"' 2>/dev/null || true)
if [[ -n "$hits" ]]; then
    fail "I-3" "possible secret in a log line:
$(echo "$hits" | sed 's/^/      /')" \
      "Log a SHA-256 or an opaque handle. Secrets never enter logs (CLAUDE.md §9 S7)."
else
    pass "I-3a no secret-shaped keys in log calls"
fi

hits=$(go_sources | xargs grep -nE '(prompt|Prompt)[[:space:]]*[:,][[:space:]]*(p|prompt|req\.Prompt)\b' 2>/dev/null \
        | grep -iE 'slog|log\.' || true)
if [[ -n "$hits" ]]; then
    fail "I-3" "prompt body appears in a log line:
$(echo "$hits" | sed 's/^/      /')" \
      "Log promptSHA256 instead. Prompt bodies are user data (PRD FR-025)."
else
    pass "I-3b no prompt bodies in logs"
fi

# ---------------------------------------------------------------------------
# I-4 : only the runner may import the Docker client
# ---------------------------------------------------------------------------
hits=$(go_sources | xargs grep -l 'github.com/docker/docker' 2>/dev/null \
        | grep -vE '^(cmd/runner/|internal/sandbox/runner/)' || true)
if [[ -n "$hits" ]]; then
    fail "I-4" "Docker client imported outside the runner:
      $(echo "$hits" | tr '\n' ' ')" \
      "The control plane parses untrusted input; Docker socket access is \
root-equivalent on the host. Runner only (ADR-011)."
else
    pass "I-4  Docker access confined to the runner"
fi

# ---------------------------------------------------------------------------
# SQL injection: no string building into a query
# ---------------------------------------------------------------------------
hits=$(go_sources | xargs grep -nE \
    '(Query|Exec|QueryRow)[A-Za-z]*\([^)]*(fmt\.Sprintf|"[[:space:]]*\+|\+[[:space:]]*")' 2>/dev/null || true)
if [[ -n "$hits" ]]; then
    fail "D1" "SQL built by string concatenation:
$(echo "$hits" | sed 's/^/      /')" \
      "Parameterize. No exceptions, including for internal values \
(docs/CODE-STANDARDS.md §6 D1)."
else
    pass "D1   all SQL is parameterized"
fi

# ---------------------------------------------------------------------------
# Banned file names (CLAUDE.md RULE F3)
# ---------------------------------------------------------------------------
hits=$(all_go_files \
        | grep -iE '/(utils?|helpers?|common|shared|misc|base|types|models|structs|interfaces)\.go$' || true)
if [[ -n "$hits" ]]; then
    fail "F3" "banned file name:
      $(echo "$hits" | tr '\n' ' ')" \
      "Name a file for the type it defines. If you cannot, the contents do \
not belong together."
else
    pass "F3   no junk-drawer file names"
fi

# ---------------------------------------------------------------------------
# Banned package names (CLAUDE.md RULE PK2)
# ---------------------------------------------------------------------------
hits=$(git ls-files 'internal/**/*.go' 'pkg/**/*.go' 2>/dev/null \
        | xargs -r dirname | sort -u \
        | grep -iE '/(utils?|helpers?|common|shared|misc|base|core|lib|types|models|interfaces)$' || true)
if [[ -n "$hits" ]]; then
    fail "PK2" "banned package name:
      $(echo "$hits" | tr '\n' ' ')" \
      "A package is named for what it does, not what it contains."
else
    pass "PK2  no junk-drawer package names"
fi

# ---------------------------------------------------------------------------
# File name style: lowercase, no underscores or dashes (RULE F1)
# ---------------------------------------------------------------------------
hits=$(all_go_files | xargs -r -n1 basename \
        | grep -vE '_test\.go$' \
        | grep -vE '_(linux|darwin|windows|amd64|arm64)\.go$' \
        | grep -vE '\.(gen|pb)\.go$' \
        | grep -E '[A-Z_-]' || true)
if [[ -n "$hits" ]]; then
    fail "F1" "non-conforming file name:
      $(echo "$hits" | tr '\n' ' ')" \
      "Lowercase, no underscores, no dashes, no camelCase."
else
    pass "F1   file names are lowercase and clean"
fi

# ---------------------------------------------------------------------------
# File length (CLAUDE.md §5.1)
# ---------------------------------------------------------------------------
long=$(go_sources | while read -r f; do
    n=$(wc -l < "$f")
    (( n > 500 )) && printf '%s (%d lines)\n' "$f" "$n"
done || true)
if [[ -n "$long" ]]; then
    fail "5.1" "file over the 500-line limit:
$(echo "$long" | sed 's/^/      /')" \
      "Split it. A 500-line file has more than one responsibility."
else
    pass "5.1  no file exceeds 500 lines"
fi

# ---------------------------------------------------------------------------
# Every package has doc.go (RULE PK4)
# ---------------------------------------------------------------------------
missing=$(git ls-files 'internal/**/*.go' 2>/dev/null | xargs -r dirname | sort -u | while read -r d; do
    [[ -f "$d/doc.go" ]] || echo "$d"
done || true)
if [[ -n "$missing" ]]; then
    fail "PK4" "package without doc.go:
      $(echo "$missing" | tr '\n' ' ')" \
      "Write 2–5 sentences on the package's responsibility and entry points. \
It is the first thing a stranger reads."
else
    pass "PK4  every package documented"
fi

# ---------------------------------------------------------------------------
# TODO without an issue reference (DOC6)
# ---------------------------------------------------------------------------
hits=$(all_go_files | xargs grep -nE '(TODO|FIXME|XXX|HACK)' 2>/dev/null \
        | grep -vE 'TODO\(#[0-9]+\)' || true)
if [[ -n "$hits" ]]; then
    fail "DOC6" "TODO without an issue link:
$(echo "$hits" | sed 's/^/      /')" \
      "Use TODO(#123): reason — an untracked TODO is a wish, not a plan."
else
    pass "DOC6 all TODOs are tracked"
fi

# ---------------------------------------------------------------------------
# Commented-out code (DOC5)
# ---------------------------------------------------------------------------
# "if"/"for"/"switch" collide with ordinary English sentences ("if any
# is invalid...", "for the same row..."), so those three additionally
# require a brace or paren somewhere on the line — real commented-out
# control-flow code has one (`if err != nil {`), prose essentially
# never does. func/return/var/err:= are narrow enough to skip that
# extra filter; a period-terminated line is still excluded either way
# (prose sentences end in one, code doesn't).
hits=$( { go_sources | xargs grep -nE '^[[:space:]]*//[[:space:]]*(if|for|switch)\b' 2>/dev/null | grep -E '[{(]'; \
         go_sources | xargs grep -nE '^[[:space:]]*//[[:space:]]*(func|return|var |err :?=)\b' 2>/dev/null; } \
    | grep -vE '\.$' || true)
if [[ -n "$hits" ]]; then
    fail "DOC5" "commented-out code:
$(echo "$hits" | sed 's/^/      /')" \
      "Delete it. Git remembers."
else
    pass "DOC5 no commented-out code"
fi

# ---------------------------------------------------------------------------
# goleak in packages that start goroutines (RULE T7 / invariant I-5)
# ---------------------------------------------------------------------------
missing=$(git ls-files 'internal/**/*.go' 2>/dev/null | grep -v '_test\.go$' | while read -r f; do
    grep -q '^[[:space:]]*go func\|^[[:space:]]*go [a-z]' "$f" 2>/dev/null || continue
    d=$(dirname "$f")
    # goleak.Find() is the sanctioned pattern here, not VerifyTestMain:
    # VerifyTestMain calls os.Exit internally, which skips any pending
    # t.Cleanup teardown (real containers, running Servers) — see
    # internal/queue/main_test.go's own comment on this. Either form
    # proves the same thing: this package's tests check for leaks.
    grep -rqs 'goleak.VerifyTestMain\|goleak.Find(' "$d" || echo "$d"
done | sort -u || true)
if [[ -n "$missing" ]]; then
    fail "T7" "package starts goroutines but has no goleak check:
      $(echo "$missing" | tr '\n' ' ')" \
      "Add TestMain with goleak.VerifyTestMain(m). Goroutine leaks on the \
cancel path are this project's most likely concurrency bug."
else
    pass "T7   goroutine leak detection present"
fi

# ---------------------------------------------------------------------------
# Secrets accidentally committed
# ---------------------------------------------------------------------------
hits=$(git ls-files | grep -viE '\.(md|ya?ml)$' | xargs -r grep -nEl \
    '(sk-[A-Za-z0-9]{20,}|ghp_[A-Za-z0-9]{36}|AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----)' 2>/dev/null || true)
if [[ -n "$hits" ]]; then
    fail "O1" "possible committed credential:
      $(echo "$hits" | tr '\n' ' ')" \
      "ROTATE THE CREDENTIAL FIRST, then purge history. Rotation first — \
purging history does not un-leak a key that is already public."
else
    pass "O1   no credential patterns committed"
fi

# ---------------------------------------------------------------------------
printf '\n'
if (( violations > 0 )); then
    printf '%s%d invariant violation(s).%s Fix before committing — `make ci` gates every commit.\n\n' \
        "$RED" "$violations" "$RESET"
    exit 1
fi
printf '%sAll invariants hold.%s\n\n' "$GREEN" "$RESET"