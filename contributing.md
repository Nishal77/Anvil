# Contributing to Anvil

Thanks for your interest. This document tells you how to get a change merged, and — just as usefully — what kinds of changes will not be merged.

Anvil is an autonomous cloud development agent. It runs untrusted, LLM-generated code in sandboxes, so the bar for changes touching execution, isolation, or state is higher than in a typical project. That is not gatekeeping; it is the nature of the problem.

---

## Before you start

| If you want to... | Do this first |
|---|---|
| Fix a typo or improve docs | Open a PR directly |
| Fix a bug | Open an issue with a reproduction, then a PR |
| Add a feature | **Open an issue and get agreement before writing code** |
| Change the architecture | Open a discussion; expect to write an ADR |
| Report a security issue | **Do not open an issue.** See [SECURITY.md](SECURITY.md) |

Unsolicited large PRs are the most common way contributor time gets wasted. A ten-minute issue conversation prevents a ten-hour rewrite.

---

## What will not be merged

Stated up front to save your time:

- **Anything in [PRD.md §4.3 Non-goals](docs/PRD.md).** Kubernetes support, Kafka, multi-agent role orchestration, browser automation, additional deploy targets. These are deliberately deferred with written reasoning in `docs/adr/`. If you disagree with an ADR, argue with the ADR in a discussion — do not route around it with a PR.
- **A new dependency** that replaces something the standard library does adequately.
- **A web framework, an ORM, a DI container, or an assertion library.** See [CODE-STANDARDS.md §9](docs/CODE-STANDARDS.md).
- **A change that relaxes a sandbox restriction** without a threat-model analysis.
- **Code without tests**, unless it is documentation or configuration.
- **A refactor with no behavior change and no stated benefit.** Churn has a cost.

---

## Development setup

**Prerequisites:** Go 1.23+, Docker, Docker Compose, Make.

```bash
git clone https://github.com/anvil-dev/anvil.git
cd anvil

make tools     # install gofumpt, golangci-lint, govulncheck, migrate, sqlc
make dev       # boot postgres, redis, minio; run migrations
make ci        # verify a clean checkout is green before you change anything
```

If `make ci` is red on a fresh clone, that is a bug — please open an issue.

---

## The workflow

1. **Open or claim an issue.** Comment that you are working on it.
2. **Branch:** `<type>/<short-kebab-description>` — e.g. `feat/lease-heartbeat`, `fix/sse-gap-marker`.
3. **Write the test first** for bug fixes. Commit the failing test separately if it makes the review clearer.
4. **Implement.** Read [CODE-STANDARDS.md](docs/CODE-STANDARDS.md) before writing non-trivial Go.
5. **`make ci` must be green before every commit** — not before every PR.
6. **Open the PR.** Fill in the template honestly, including the parts that aren't flattering.

---

## Commit messages

[Conventional Commits](https://www.conventionalcommits.org/), plus the requirement ID where one applies:

```
<type>(<scope>): <imperative summary> (<REQ-ID>)

Why this change is needed. Not what the diff does — the diff shows that.
Wrap at 72 characters.

Refs #123
```

**Types:** `feat` `fix` `refactor` `test` `docs` `perf` `build` `ci` `chore` `revert`
**Scopes:** package names — `queue` `agent` `llm` `sandbox` `events` `api` `auth` `deploy` `storage` `telemetry`

```
✓ feat(queue): reclaim jobs with expired leases (FR-012)
✓ fix(events): emit stream_gap instead of dropping silently (FR-054)
✓ test(sandbox): fork bomb is contained by pids-limit (SEC-004)
✗ update stuff
✗ WIP
✗ fixed the bug
```

Commits are atomic: one logical change each. If the summary needs the word "and", split it.

---

## Pull requests

### Checklist

- [ ] Title follows the commit convention
- [ ] Description explains **why**, and links the issue
- [ ] Requirement IDs referenced where applicable
- [ ] Tests added; bug fixes have a failing-first test
- [ ] `make ci` green locally
- [ ] `make test-integration` green if you touched storage, queue, or events
- [ ] Docs updated in the same PR if behavior changed
- [ ] **`docs/PRD.md` updated in the same PR if the spec changed**
- [ ] `CHANGELOG.md` updated for user-visible changes
- [ ] No unrelated changes
- [ ] Under ~400 lines of diff, or the description explains why not

### Review expectations

- Every PR needs one approving review.
- Changes to `internal/sandbox/`, `internal/queue/`, or anything security-relevant need review from a `CODEOWNERS` entry.
- Reviewers comment on design, correctness, and clarity. Style is the linter's job — if you find yourself arguing about formatting, fix the linter config instead.
- Reviews are about the code. Nobody's competence is on trial, and neither is yours.

### If your PR goes quiet

Ping it after a week. Maintainer bandwidth is finite and a nudge is welcome, not rude.

---

## Code standards

The full reference is [docs/CODE-STANDARDS.md](docs/CODE-STANDARDS.md). The short version:

**Hard limits (CI-enforced):** 500 lines per file · 50 lines per function · 5 parameters · cyclomatic complexity 10 · nesting depth 3 · 70% coverage on core packages.

**Always:**
- Errors wrapped with `%w` and context when crossing a package boundary
- `ctx context.Context` first, and honored
- Interfaces declared where they are consumed, and small
- Every goroutine has a documented owner and shutdown path
- Table-driven tests with named cases
- `doc.go` in every package

**Never:**
- `fmt.Println`, `panic` in library code, `init()`, package-level mutable state
- `context.TODO()`, `interface{}` in a public signature
- `time.Sleep` — in production code *or* tests
- Commented-out code, or a `TODO` without an issue link
- String concatenation into SQL

---

## Testing

```bash
make test              # unit
make test-race         # unit with -race (CI gate)
make test-integration  # real Postgres + Redis via testcontainers
make chaos             # crash-recovery matrix (PRD §14.3)
make security-test     # sandbox escape suite (PRD §20.4)
```

Rules that matter more here than in most projects:

- **No test hits a real LLM API.** Use `FakeProvider` or a `go-vcr` fixture.
- **No test sleeps.** A test that needs `time.Sleep` to pass has a synchronization bug; growing the sleep hides it rather than fixing it.
- **Concurrency claims need concurrency tests.** If you change claim, lease, or fan-out logic, prove the property under contention.
- **Security changes need a test in `test/security/`.** A sandbox restriction with no test is a comment.

---

## Architecture Decision Records

Open an ADR when you change a technology choice, a service boundary, a security posture, or a data model in a way that would surprise someone reading the code later.

Copy `docs/adr/template.md`. Every ADR has five sections, and the fifth is not optional:

1. **Context** — the forces at play
2. **Decision** — what we are doing, in the active voice
3. **Consequences** — what gets better, and what gets worse
4. **Alternatives considered** — what you rejected and why
5. **Revisit when** — **a specific, observable trigger.** "When we have more users" is not a trigger. "When sustained queue depth exceeds 1,000 or throughput exceeds 500 jobs/sec" is.

That last field is what makes an ADR a decision rather than an opinion.

---

## Security

Do not open a public issue for a security vulnerability. See [SECURITY.md](SECURITY.md) for the private disclosure process.

Contributions touching sandbox isolation, the policy engine, secret handling, or authentication get extra scrutiny and will be asked for a threat-model paragraph. Please write it in the PR description rather than waiting to be asked.

---

## Licence

By contributing, you agree that your contributions are licensed under [Apache License 2.0](LICENSE). No CLA is required.

---

## Code of conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md). Be decent. Assume good faith. Disagree about the code, not about the person.