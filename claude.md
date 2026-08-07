# CLAUDE.md

Operating manual for AI agents working in this repository. Read this fully before your first edit in any session.

**Project:** Anvil — an autonomous cloud development agent
**Language:** Go 1.23+ (backend), TypeScript (frontend)
**License:** Apache-2.0
**Status:** Public open-source project. Every commit is permanent and readable by strangers.

---

## 0. The prime directive

> **This repository must read as though a professional infrastructure team maintains it.**

Every decision — a file name, an error message, a test name, a commit — is judged against that standard. When a choice is ambiguous, pick the one a reviewer at a serious engineering organization would not comment on.

Three consequences that override normal defaults:

1. **Correctness beats completion.** A half-finished feature with tests and a clear boundary is better than a finished one without.
2. **Boring beats clever.** Clever code is a liability in a repo strangers read. If a construct needs a comment to explain *what* it does (not *why*), rewrite it.
3. **Explicit beats implicit.** No magic, no reflection where a switch works, no framework indirection where a function call works.

---

## 1. Documents and precedence

| File | Role | Read when |
|---|---|---|
| `CLAUDE.md` (this) | How to work here | Every session, first |
| `docs/BUILD-PLAN.md` | What to build now, in what order, when you may move on | Every session, second |
| `docs/PRD.md` | Normative spec: schema, API, state machines, security | Before touching those areas |
| `docs/CODE-STANDARDS.md` | Deep Go quality reference | Before writing non-trivial Go |
| `docs/adr/` | Why decisions were made | Before proposing an alternative |

**Precedence when they conflict:** `PRD.md` §10/§11/§13 (normative spec) > `BUILD-PLAN.md` > `CLAUDE.md` > `CODE-STANDARDS.md`.

If you find a genuine contradiction, **stop and report it**. Do not resolve it yourself. A silently-resolved spec conflict is how a codebase drifts from its documentation.

---

## 2. Phase gating — the hardest rule in this file

The build runs in phases (`docs/BUILD-PLAN.md`). Phases are **sequential and gated**.

### 2.1 Absolute rules

```
RULE P1. You work on exactly one week of one phase at a time.
RULE P2. You do NOT implement anything from a later week, even if it is
         "only five minutes" or "while I'm in here."
RULE P3. You do NOT proceed past a gate until every gate checkbox is
         demonstrably true.
RULE P4. If asked to do work from a later phase, say so and ask for
         explicit confirmation before proceeding.
RULE P5. Anything in PRD §4.3 (Non-goals) is a DEFECT if implemented.
         Kafka, Kubernetes, multi-agent roles, Playwright, gRPC service
         split, multi-cloud deploy. Not "nice to have later" — a defect now.
```

### 2.2 Session start protocol

At the beginning of every session, before writing code, output exactly this and wait:

```
PHASE:    P<n> — <name>
WEEK:     W<n>
TASKS:    <the task IDs from the BUILD-PLAN week table, e.g. 2.3, 2.4>
GATE:     <the gate this week feeds, and whether it is the gate week>
FILES:    <every file you will create or modify>
OUT OF SCOPE THIS WEEK: <the tempting adjacent things you will not touch>
```

Do not begin until confirmed. This 30-second checkpoint is the primary defense against the failure mode that kills this project: drifting forward with half-finished layers.

### 2.3 Session end protocol

```
COMPLETED:   <task IDs>
TESTS:       <test names added, with requirement IDs>
NOT DONE:    <anything from the week's table left undone, and why>
GATE STATUS: <each gate checkbox: PASS / FAIL / NOT YET TESTABLE>
NEXT:        <the single next task>
```

### 2.4 Phase-1 special rule

**Phase 1 (weeks 1–4) contains no LLM code whatsoever.** No provider clients, no prompts, no agent loop, no tool definitions. Plans are hardcoded. This is deliberate (`BUILD-PLAN.md` §0.2): nondeterminism masks infrastructure bugs.

If you write an LLM call during Phase 1, you have broken the plan.

---

## 3. Non-negotiable invariants

These hold at every commit. A PR violating any of them is rejected regardless of what else it does.

| # | Invariant | Enforced by |
|---|---|---|
| **I-1** | All `jobs.status` changes go through the single guarded transition function in `internal/queue`. No `UPDATE jobs SET status` exists anywhere else. | `make check-invariants` (grep) |
| **I-2** | Every side effect outside the sandbox is wrapped in `callIdempotent`. | Code review + test |
| **I-3** | No secret ever enters a prompt, a container env var, a log line, or a file on disk. | `make check-invariants` + review |
| **I-4** | The control plane never touches the Docker socket. Only `cmd/runner` does. | Import lint |
| **I-5** | Every goroutine has a documented owner, a shutdown path, and a bounded lifetime. | `goleak` in tests |
| **I-6** | No unbounded channel, slice accumulation, or map growth on a request path. | Review |
| **I-7** | Events are persisted **before** they are published to Redis. | Test |
| **I-8** | Redis is never a source of truth. The system stays correct with Redis down. | Chaos test 7 |
| **I-9** | `context.Context` is the first parameter of every function that does I/O, and is honored. | `contextcheck` lint |
| **I-10** | Every error crossing a package boundary is wrapped with `%w` and context. | `wrapcheck` lint |

---

## 4. Repository structure

### 4.1 Layout

```
anvil/
├── CLAUDE.md
├── README.md
├── LICENSE                     Apache-2.0
├── SECURITY.md
├── CONTRIBUTING.md
├── CODE_OF_CONDUCT.md
├── CHANGELOG.md                Keep a Changelog format
├── Makefile
├── .golangci.yml
├── docker-compose.yml
├── go.mod
├── .github/
│   ├── CODEOWNERS
│   ├── dependabot.yml
│   ├── ISSUE_TEMPLATE/
│   ├── pull_request_template.md
│   └── workflows/              ci.yml, deploy.yml, benchmark.yml, codeql.yml
├── cmd/                        one directory per binary; main.go only
│   ├── anvil/main.go           control plane
│   ├── runner/main.go          sandbox runner (sole Docker socket owner)
│   └── anvilctl/main.go        dev CLI
├── internal/                   all application code; nothing importable externally
│   ├── api/
│   ├── auth/
│   ├── queue/
│   ├── agent/
│   ├── llm/
│   ├── sandbox/
│   ├── events/
│   ├── deploy/
│   ├── storage/
│   ├── telemetry/
│   └── config/
├── pkg/                        ONLY if something is genuinely reusable. Default: empty.
├── migrations/                 NNN_description.up.sql / .down.sql
├── api/openapi.yaml
├── images/workspace/Dockerfile
├── ops/                        grafana/ prometheus/ caddy/ systemd/
├── test/                       integration/ chaos/ security/ load/
├── benchmarks/                 tasks/ results.md
├── docs/
│   ├── PRD.md  BUILD-PLAN.md  CODE-STANDARDS.md  architecture.md  runbook.md
│   ├── adr/                    NNNN-kebab-title.md
│   └── diagrams/
└── web/                        Next.js frontend
```

### 4.2 Package rules

```
RULE PK1. Package names: short, lowercase, singular, no underscores.
          GOOD: queue, agent, llm, sandbox, events
          BAD:  queues, jobQueue, job_queue, queueService

RULE PK2. These package names are BANNED outright:
          util, utils, helper, helpers, common, shared, misc,
          base, core, lib, types, models, interfaces
          A package is named for what it DOES, not what it contains.

RULE PK3. No stuttering. Package `queue` exports `Dispatcher`, not
          `QueueDispatcher`. Callers write queue.Dispatcher.

RULE PK4. Every package has doc.go with a package comment explaining its
          responsibility in 2–5 sentences and naming its public entry points.

RULE PK5. Dependency direction is one-way. Enforced in CI:
            api  → queue, agent, storage, events, auth
            queue → storage, telemetry
            agent → llm, sandbox, storage, events
            llm   → telemetry
            sandbox → telemetry            (NEVER storage, NEVER api)
            events  → storage, telemetry
            storage → telemetry
            telemetry → (nothing internal)
          No cycles. No package imports api. No package imports agent
          except api.
```

### 4.3 File naming rules

```
RULE F1.  Files are lowercase, no underscores, no dashes, no camelCase.
          Only `_test.go` and `_linux.go`-style build suffixes use underscores.
          GOOD: dispatcher.go  lease.go  claim.go
          BAD:  jobDispatcher.go  job_dispatcher.go  job-dispatcher.go

RULE F2.  A file is named for the primary type or concept it defines.
          dispatcher.go defines Dispatcher. lease.go defines Lease and its
          operations.

RULE F3.  These file names are BANNED: utils.go, helpers.go, common.go,
          misc.go, types.go, models.go, structs.go, interfaces.go, main2.go
          If you cannot name a file after what is in it, the contents do
          not belong together.

RULE F4.  Every package with more than one error uses errors.go for its
          sentinel errors and error types.

RULE F5.  Every package emitting metrics uses metrics.go for its collectors.

RULE F6.  Test file mirrors source file: lease.go → lease_test.go.
          Cross-cutting package tests go in <package>_test.go.

RULE F7.  Hard limit 500 lines per file. At 400, plan the split.
          Generated files and _test.go files are exempt from the 500 limit
          but not from being organized.

RULE F8.  cmd/*/main.go contains ONLY: flag/env parsing, dependency
          construction, wiring, signal handling, graceful shutdown.
          Zero business logic. Target under 150 lines.
```

**Canonical package example:**

```
internal/queue/
├── doc.go            package documentation
├── dispatcher.go     Dispatcher — the primary type, Run/Stop
├── claim.go          SKIP LOCKED claim query and Claim()
├── lease.go          Lease, Acquire, Heartbeat, Release
├── sweeper.go        expired-lease reclaim loop
├── transition.go     THE guarded state transition function (I-1)
├── backoff.go        jittered exponential backoff
├── errors.go         ErrNoWork, ErrLeaseLost, ErrIllegalTransition
├── metrics.go        prometheus collectors
├── dispatcher_test.go
├── claim_test.go
├── lease_test.go
├── transition_test.go
└── testdata/
```

### 4.4 Naming conventions (identifiers)

```
RULE N1.  Interfaces are named for behavior, not implementation.
          Single-method interfaces end in -er: Claimer, Publisher, Deployer.
          Never IFoo, never FooInterface, never AbstractFoo.

RULE N2.  Constructors: New<Type> returning the CONCRETE type, never an
          interface. NewDispatcher() *Dispatcher.

RULE N3.  Sentinel errors: ErrSomething, package-level, var not const.
          Error types: SomethingError struct.

RULE N4.  Booleans read as assertions: isReady, hasLease, canRetry.
          Never flag, check, status, ok (except the comma-ok idiom).

RULE N5.  Acronyms keep consistent case: jobID, httpClient, apiURL,
          userID, sseWriter. Never jobId, HttpClient, ApiUrl.

RULE N6.  Context is always named ctx. Errors are always named err.
          Receivers are 1–2 letters, consistent across the whole type.

RULE N7.  No abbreviation that isn't universal. GOOD: ctx, err, id, cfg,
          req, resp, db, tx. BAD: mgr, svc, hdlr, prc, cnt, tmp, val, obj.

RULE N8.  Test names: Test<ReqID>_<Behavior> where a requirement applies.
          TestFR011_LeaseHeartbeatExtendsTTL
          TestSEC001_ContainerCannotReadEtcShadow
          Otherwise Test<Type>_<Method>_<Condition>.
```

---

## 5. Code quality — the enforced minimum

Full detail in `docs/CODE-STANDARDS.md`. This is the summary you must hold in working memory.

### 5.1 Hard limits (CI-enforced)

| Limit | Value |
|---|---|
| File length | 500 lines |
| Function length | 50 lines |
| Function parameters | 5 (use a struct beyond that) |
| Cyclomatic complexity | 10 |
| Cognitive complexity | 15 |
| Nesting depth | 3 |
| Package public API surface | If a type isn't used outside the package, unexport it |
| Test coverage, core packages | 70% (`queue`, `agent`, `events`, `llm`) |

### 5.2 Forbidden — these fail CI

```
✗ fmt.Print / fmt.Println / log.Print       → log/slog only
✗ panic() outside cmd/*/main.go startup     → return an error
✗ init() functions                          → explicit construction in main
✗ package-level mutable state (var x = ...) → dependency injection
✗ context.TODO() in committed code          → thread a real context
✗ interface{} or any in a public signature  → generics or a concrete type
✗ time.Sleep in production code             → timers, tickers, ctx.Done()
✗ time.Sleep in tests                       → synchronization or a fake clock
✗ ignored errors without _ = and a reason   → handle or explain
✗ commented-out code                        → git remembers; delete it
✗ TODO / FIXME without an issue link        → TODO(#123): reason
✗ //nolint without a justification comment  → //nolint:gosec // reason: ...
✗ naked returns in functions over 5 lines   → name what you return
✗ else after a return/continue/break        → early return, flat code
✗ magic numbers/strings in logic            → named constants
✗ struct literals without field names       → Job{ID: x} not Job{x}
✗ log-and-return the same error             → wrap and return, log once at top
```

### 5.3 Required

```
✓ Every exported identifier has a doc comment starting with its name.
✓ Every error crossing a package boundary is wrapped: fmt.Errorf("claiming job: %w", err)
✓ Error strings: lowercase, no punctuation, no "failed to" prefix.
    GOOD: fmt.Errorf("acquire lease for job %s: %w", jobID, err)
    BAD:  fmt.Errorf("Failed to acquire lease!")
✓ Every goroutine's launch site has a comment naming its lifetime and shutdown path.
✓ Every exported function that does I/O takes ctx as its first parameter.
✓ Interfaces are declared where they are CONSUMED, not where implemented.
✓ Accept interfaces, return concrete types.
✓ Table-driven tests with named cases; t.Parallel() where safe.
✓ defer for every acquired resource, immediately after acquisition.
✓ Every SQL query is either in a named constant or a sqlc-generated method.
    No string concatenation into SQL, ever. Parameterized only.
```

### 5.4 The comment rule

Comments explain **why**, never **what**. If a comment restates the code, delete the comment or rewrite the code.

```go
// BAD — restates the code
// increment the attempt counter
j.Attempt++

// GOOD — explains a decision a reader would question
// Increment before the work, not after: a worker that crashes mid-job must
// still burn an attempt, otherwise a poison-pill job retries forever (FR-013).
j.Attempt++
```

The second kind of comment is what separates a codebase reviewers trust from one they don't. Write more of them, especially at every point where you chose a non-obvious option.

---

## 6. Testing discipline

```
RULE T1.  Every exported function has a test. No exceptions for "trivial".
RULE T2.  Every bug fix begins with a failing test that reproduces it.
RULE T3.  No test calls a real LLM API. FakeProvider or go-vcr fixtures only.
RULE T4.  No test uses time.Sleep. Use channels, sync primitives, or an
          injected clock. A sleeping test is a flaky test.
RULE T5.  Integration tests use testcontainers with real Postgres and Redis.
          Never mock the database.
RULE T6.  Tests run with -race in CI. A data race is a build failure.
RULE T7.  goleak.VerifyTestMain in every package that starts goroutines.
RULE T8.  Test names carry the requirement ID where one applies (RULE N8).
RULE T9.  Table-driven with named cases. One assertion concept per case.
RULE T10. Concurrency claims need concurrency tests. "Two workers race for
          one job" is a real test that must exist (FR-010).
```

**The test that matters most in this repo:** crash recovery. Every row of `PRD.md` §14.3 is a test. If it isn't tested, the durable-execution claim is unbacked, and that claim is the project's centerpiece.

---

## 7. Git and PR discipline

### 7.1 Commits

Conventional Commits + requirement ID:

```
<type>(<scope>): <imperative summary> (<REQ-ID>)

<body: why, not what — wrap at 72>

<footer: Refs #issue / BREAKING CHANGE:>
```

Types: `feat` `fix` `refactor` `test` `docs` `perf` `build` `ci` `chore` `revert`
Scopes: package names — `queue` `agent` `llm` `sandbox` `events` `api` `auth` `deploy` `storage` `telemetry`

```
GOOD: feat(queue): reclaim jobs with expired leases (FR-012)
GOOD: fix(events): emit stream_gap instead of dropping silently (FR-054)
GOOD: test(queue): two workers cannot claim the same job (FR-010)
BAD:  update stuff
BAD:  WIP
BAD:  fixed bug
```

```
RULE G1. Commits are atomic. One logical change. If the summary needs
         "and", split it.
RULE G2. Every commit compiles and passes tests. No broken intermediate
         commits on main.
RULE G3. Never commit: secrets, .env, *.key, credentials, large binaries,
         commented-out code, personal paths, IDE config.
RULE G4. Branches: <type>/<short-kebab-description>, e.g. feat/lease-heartbeat
RULE G5. Never force-push to main. Never rewrite published history.
```

### 7.2 Pull requests

A PR that does not satisfy all of these is not ready:

- [ ] Title follows the commit convention
- [ ] Description states **why**, not just what
- [ ] Requirement IDs referenced
- [ ] Tests added, including a failing-first test for any bug fix
- [ ] `make ci` green locally
- [ ] Docs updated in the same PR if behavior changed
- [ ] **PRD updated in the same PR if the spec changed** (a stale PRD is worse than none)
- [ ] No unrelated changes
- [ ] Under ~400 lines of diff, or explains why not
- [ ] New dependency, if any, justified in one line

---

## 8. Open-source obligations

This repo is public. Strangers will read it, and some will judge you on it.

```
RULE O1.  Never commit a secret. If you do: rotate it immediately, then
          purge history. Rotation first — history purge does not un-leak.
RULE O2.  Never commit real user data, real emails, real tokens, real
          hostnames. Use example.com, RFC 5737 IPs, obviously-fake IDs.
RULE O3.  Every dependency addition is justified in the PR. Prefer the
          standard library. Check license compatibility with Apache-2.0.
RULE O4.  README examples must actually work. Test them.
RULE O5.  Error messages are user-facing documentation. Write them for a
          stranger with no context.
RULE O6.  CHANGELOG.md updated for every user-visible change
          (Keep a Changelog format, semver).
RULE O7.  Security issues go to SECURITY.md's private channel, never a
          public issue.
RULE O8.  Do not overclaim in the README. "Hardened container isolation,
          single-tenant" — never "secure multi-tenant sandbox". The
          Limitations section is mandatory (PRD §16.2).
```

---

## 9. Security rules (always in force)

```
RULE S1.  Treat everything inside the sandbox as hostile. LLM-generated
          code from a user prompt is attacker-influenced code.
RULE S2.  Never mount the Docker socket into a workspace container. Ever.
RULE S3.  Secrets: envelope-encrypted at rest, never in a prompt, never in
          container env, never on disk. Injected per-command via named pipe.
RULE S4.  Every tool call passes the policy engine BEFORE dispatch and is
          persisted BEFORE execution.
RULE S5.  All paths from tool arguments go through filepath.Clean and
          symlink resolution, then a prefix check against /workspace.
          A path check without symlink resolution is not a path check.
RULE S6.  SQL is parameterized. Always. No exceptions, no "it's internal".
RULE S7.  Log no PII, no prompt bodies (hash them), no tokens, no secrets.
RULE S8.  Default deny: network egress, tool availability, file paths, auth.
RULE S9.  Rate-limit every unauthenticated endpoint.
RULE S10. Run `govulncheck` in CI. A known-vulnerable dependency blocks merge.
```

---

## 10. When you are uncertain

**Stop and ask.** Specifically, always ask rather than guess when:

- The PRD does not specify a schema, endpoint, state, or error case you need
- Two documents contradict each other
- A requirement seems wrong or impossible
- A task appears to belong to a different phase
- You would need to add a dependency not already in `go.mod`
- You would need to change something in PRD §10, §11, or §13
- A security rule seems to block legitimate work

**Never do these instead of asking:**

- Invent a column, table, endpoint, state, or error code
- Implement a simplified version and mark it TODO
- Work around a security rule
- Skip a test because the thing is "hard to test" — hard to test means poorly designed
- Silently expand scope past the current week

A five-second question is cheaper than a spec drift you discover in week 10.

---

## 11. Session checklist

**Before writing code**
- [ ] Read `CLAUDE.md`, `BUILD-PLAN.md` current week, relevant `PRD.md` sections
- [ ] Output the §2.2 session-start block and get confirmation
- [ ] Confirm no task belongs to a later phase

**While writing**
- [ ] File and identifier names follow §4.3 and §4.4
- [ ] No item from the §5.2 forbidden list
- [ ] Test written alongside, not after
- [ ] Errors wrapped with context
- [ ] Every goroutine's lifetime documented
- [ ] Every non-obvious decision has a *why* comment

**Before committing**
- [ ] `make ci` green (`fmt vet lint test-race check-invariants`)
- [ ] Coverage not reduced on core packages
- [ ] Commit message follows §7.1 with a requirement ID
- [ ] No secrets, no debug prints, no commented-out code
- [ ] Docs and PRD updated if behavior or spec changed

**At session end**
- [ ] Output the §2.3 session-end block
- [ ] Update `PROGRESS.md`
- [ ] Draft or update the ADR if a decision was made this session

---

## 12. Make targets

```bash
make dev              # boot the local stack (postgres, redis, minio)
make build            # build all binaries into ./bin
make test             # unit tests
make test-race        # unit tests with -race            (CI gate)
make test-integration # testcontainers integration tests (CI gate)
make lint             # golangci-lint run                (CI gate)
make check-invariants # grep guards for I-1, I-3, I-4    (CI gate)
make coverage         # coverage report, fails under threshold
make ci               # everything CI runs — GREEN BEFORE EVERY COMMIT
make chaos            # PRD §14.3 crash-recovery matrix  (Phase 4)
make security-test    # PRD §20.4 sandbox escape suite   (Phase 4)
make load             # k6 load test                     (Phase 4)
make bench            # LLM task benchmark suite         (Phase 2+)
make migrate-up/down  # database migrations
make generate         # sqlc, mocks, openapi client
```

**`make ci` must be green before every commit.** Not before every PR — before every commit.

---

## 13. Anti-patterns specific to this project

Failure modes this codebase is unusually prone to. Watch for them actively.

| Anti-pattern | Why it happens here | Do this instead |
|---|---|---|
| Goroutine leak on job cancel | Cancellation paths are easy to write and easy to get wrong | `goleak` in every test; every goroutine selects on `ctx.Done()` |
| Unbounded buffer for log lines | It is the obvious first implementation | Bounded 1024 + `stream_gap` (FR-054) |
| Docker socket in the control plane | It is one line and it works | Runner only (I-4, ADR-011) |
| Retry without idempotency | Retry is easy; safe retry is the actual work | `callIdempotent` on every external effect (I-2) |
| Secret in a prompt for convenience | The model "needs" the token | Opaque handle; resolve inside the sandbox (I-3) |
| `time.Sleep` to fix a flaky test | It appears to work | Synchronize properly; a sleeping test is a broken test |
| Silent event drop under load | Easiest overflow handling | `stream_gap` marker — never lie about completeness |
| Redis as source of truth | It is fast and convenient | Postgres is truth; Redis is a cache (I-8, ADR-010) |
| Swallowing an LLM error | The model output was unusable | Return it as a structured observation the agent can reason about |
| Building Phase 3 features early | They are the fun ones | RULE P2. They are defects (PRD §4.3) |
| Skipping the ADR "Revisit when" field | It feels speculative | It is the field that reads as senior. Always fill it. |

---

## 14. Quality bar — the review question

Before you consider anything done, answer honestly:

> **If a staff engineer at a company you want to work for opened this file cold, what would they comment on?**

Then fix that thing before submitting.

The bar is not "it works." The bar is **"I would not have to explain this."**

---

*This file is normative. Changes require a PR and a note in `CHANGELOG.md`.*