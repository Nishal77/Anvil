# Anvil

Autonomous cloud dev agent: describe a task in plain English, and Anvil plans it, writes the code, runs the tests, repairs its own failures, and streams every step back to your browser in real time — all inside an isolated, crash-resilient sandbox.

[![Go](https://img.shields.io/badge/Go-1.23%2B-00ADD8)](go.mod) [![License: Apache 2.0](https://img.shields.io/badge/License-Apache%202.0-blue)](LICENSE)

> **Week 7 of 12.** Phase 1 (platform skeleton) complete. Phase 2 (planning, repair loop, context budget) built this week; Gate 2 not yet passed — see [Project status](#project-status). Not yet usable end to end from a browser.

---

## What it does

You submit a prompt. A planner decomposes it into an approved, ordered plan; an executor works through each step inside a hardened container, calling tools, running commands, and repairing its own failures before giving up. Every tool call, policy decision, and log line is persisted before it happens, so a crash anywhere — control plane, worker, or browser tab — loses nothing.

The AI is the least interesting part of this project. The engineering underneath — durable execution without a coordinator, untrusted-code isolation, and real-time log streaming with zero-loss reconnect — is what it's actually about.

## Project status

| Phase | Weeks | Status | Gate |
|---|---|---|---|
| 0 — Ignition | pre | Complete | Gate 0 |
| 1 — Skeleton | 1–4 | Complete | Gate 1 |
| 2 — Intelligence | 5–7 | Built, gate not yet passed | [Gate 2](specs/phase-2-intelligence/gate-2.md) |
| 3 — Product | 8–9 | Planned | Gate 3 |
| 4 — Proof | 10–11 | Planned | Gate 4 |
| 5 — Presentation | 12 | Planned | Gate 5 |

Week 7 of 12.

Gate 2 is currently **not** passed. What's built and tested: planner with native tool-calling and a code-enforced step limit, approval gating enforced by the state machine (not the UI), cooperative cancellation checked between every turn, a wedged-worker force-cancel path via lease expiry, a repair loop proven against a forced failure, and a 7-tier token-budgeted context builder. What's missing to close the gate: a CLI to actually run and cancel a job end to end, artifact storage (nothing uploads or downloads yet), and a `/metrics` endpoint to observe the context-budget counter from outside a test.

## What works today

| Capability | What it means | Verify |
|---|---|---|
| Durable job queue | N workers claim jobs via `SELECT ... FOR UPDATE SKIP LOCKED`, no external coordinator; a dead worker's lease expires and its job is reclaimed | `go test ./internal/queue/...` |
| Crash-safe step execution | A step already `SUCCEEDED` in Postgres is never re-run after a restart | `go test -run ResumesFromLastSucceededStep ./internal/agent/...` |
| Sandbox isolation | Commands run with dropped capabilities, a read-only rootfs, seccomp, and hard resource limits | `go test -tags=security ./test/security/...` |
| Plan → approval → execution | A job cannot execute while `AWAITING_APPROVAL`, enforced by the transition function itself | `go test -run TestINV4_CannotExecuteWhileAwaitingApproval ./internal/queue/...` |
| Cooperative cancellation | Checked between every turn, not just between steps; a sandbox is force-destroyed within seconds | `go test -run TestUS04_CancelCheckedBetweenTurnsNotOnlySteps ./internal/agent/...` |
| Wedged-worker cancellation | A worker that never acknowledges a cancel still reaches `CANCELLED` once its lease expires | `go test -run TestUS04_WedgedWorkerReachesCancelledViaLeaseExpiry ./internal/queue/...` |
| Repair loop | A forced compile failure is diagnosed and repaired, up to 3 attempts, before the step gives up | `go test -run TestFR022_RepairLoopRecoversFromForcedFailure ./internal/agent/...` |
| Token-budgeted context | The executor's request is assembled in priority tiers and measurably dropped, never silently truncated | `go test -run TestContextBuilder_EmitsContextPressureOnDrop ./internal/agent/...` |
| Live log streaming | Container stdout streams as it's produced; a closed and reopened SSE connection replays every missed event exactly once | `go test ./internal/events/...` |
| Task benchmark | 5 trivial-tier tasks, single-shot completion harness — see [Limitations](#limitations) | `make bench` |

## Architecture

```text
                    ┌───────────────────────────┐
                    │  Web UI (Next.js 15 / TS) │
                    └────────────┬──────────────┘
                          HTTPS  │  SSE (text/event-stream)
                                 ▼
        ┌────────────────────────────────────────────────────┐
        │        CONTROL PLANE  (single Go binary)            │
        │                                                      │
        │  ┌──────────┐  ┌───────────┐  ┌────────────────┐   │
        │  │   api/   │  │  queue/   │  │    agent/       │   │
        │  │ HTTP+SSE │  │ dispatcher│  │ planner+executor│   │
        │  │  authz   │  │  leases   │  │  tool dispatch  │   │
        │  └──────────┘  └───────────┘  └────────────────┘   │
        │  ┌──────────┐  ┌───────────┐                        │
        │  │  llm/    │  │  events/  │                        │
        │  │ router + │  │  fan-out  │                        │
        │  │ budget   │  │  hub      │                        │
        │  └──────────┘  └───────────┘                        │
        └───┬────────────────┬───────────────────┬────────────┘
            │                │                   │ HTTP (Runner API)
            ▼                ▼                   ▼
   ┌────────────────┐ ┌─────────────┐  ┌────────────────────────┐
   │   PostgreSQL   │ │    Redis    │  │  SANDBOX RUNNER (Go)   │
   │  · jobs/steps  │ │ · pub/sub   │  │  sole Docker-socket    │
   │  · events      │ │ · idem keys │  │  owner, isolated       │
   │  · agent_turns │ └─────────────┘  └───────────┬────────────┘
   └────────────────┘                              ▼
                                       ┌────────────────────────┐
                                       │ Per-job container       │
                                       │ cap-drop=ALL             │
                                       │ read-only rootfs         │
                                       │ seccomp + no-new-privs   │
                                       │ cpu=1 mem=1G pids=256    │
                                       └──────────────────────────┘
```

**Durable execution.** A job is claimed with `SELECT ... FOR UPDATE SKIP LOCKED`, so N workers coordinate through Postgres alone — no lock service, no leader election. The claim grants a time-bounded lease extended by heartbeat; a dead worker's lease expires and the job is reclaimed with its attempt counter already incremented, so a poison-pill job can't retry forever. → [`internal/queue`](internal/queue)

**Untrusted code isolation.** Every step runs inside a container with dropped capabilities, a read-only root filesystem, seccomp, and hard CPU/memory/PID limits, built and torn down per job by a Runner process that is the *only* thing in the system allowed to touch the Docker socket — the control plane never does. Every tool call is validated against a JSON Schema and a policy engine before it reaches the sandbox, and the decision is persisted before dispatch, not after. → [`internal/sandbox`](internal/sandbox), [`internal/agent/policy.go`](internal/agent/policy.go)

**Real-time streaming with zero loss.** Every event is persisted to Postgres *before* it's published to Redis for the SSE fan-out — Redis is a fast path, never the source of truth, and the system stays correct with Redis down. A reconnecting client passes `Last-Event-ID` and replays exactly the events it missed from Postgres, once each. → [`internal/events`](internal/events)

## Quickstart

Requires Go 1.23+, Docker, and at least one of an Anthropic or OpenAI API key.

```bash
cp .env.example .env        # fill in ANVIL_ANTHROPIC_API_KEY or ANVIL_OPENAI_API_KEY
make dev                    # starts Postgres, Redis, MinIO via docker-compose
make migrate-up
make build
./bin/runner &               # sandbox runner — owns the Docker socket
./bin/anvil                  # control plane — api + queue dispatcher
```

There is no `anvilctl run` yet, so a job currently has to be submitted directly against the API:

```bash
curl -s -X POST localhost:8080/v1/jobs \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"prompt":"build a Go hello-world with a passing test","options":{"auto_approve":true}}'
```

## Testing

```bash
make test              # unit tests
make test-race         # unit tests with -race (CI gate)
make test-integration  # testcontainers: real Postgres + Redis
make security-test     # sandbox escape suite, PRD §20.4
make lint              # golangci-lint
make check-invariants  # I-1, I-3, I-4 grep guards
make coverage          # fails below 70% on core packages
make bench             # LLM task benchmark, appends to benchmarks/results.md
make ci                # everything CI runs
```

`make chaos` and `make load` are defined in the Makefile but their target directories (`test/chaos`, `test/load`) don't exist yet — Phase 4 work.

## Documentation

| Doc | What's in it |
|---|---|
| [`docs/PRD.md`](docs/PRD.md) | Normative spec: schema, API, state machines, security model |
| [`docs/BUILD-PLAN.md`](docs/BUILD-PLAN.md) | The 12-week phased plan and gate criteria |
| [`docs/adr/`](docs/adr/) | Architecture decision records (3 so far) |
| [`specs/`](specs) | Per-week task specs and per-gate verification checklists |
| [`benchmarks/results.md`](benchmarks/results.md) | Task benchmark history, with dates |

## Limitations

Anvil's sandbox uses hardened OCI containers, not hardware virtualization. Against a determined attacker with a kernel 0-day, a container is not a sufficient isolation boundary. Anvil is deployed as a single-tenant demonstration system with a documented threat model; production multi-tenant operation would require Firecracker or Kata microVMs (each workload under its own kernel via KVM). This is a tracked upgrade, not an oversight.

Beyond that:

- **No artifact storage yet.** A job's output isn't uploaded or downloadable. "Failure preserves the artifact" is a design goal, not a built feature.
- **No CLI.** `anvilctl` only has a `bench` subcommand — submitting, approving, and cancelling a job requires calling the HTTP API directly.
- **No `/metrics` endpoint.** Prometheus counters exist in-process (including the context-budget pressure counter) but nothing serves them yet.
- **The benchmark number doesn't reflect the current agent.** `make bench`'s 100% (5/5, trivial tier, 2026-08-14) runs a single-completion harness that predates the planner/executor/repair pipeline — it doesn't exercise planning, repair, or the context budget. Treat it as a placeholder, not a claim about the system this week built.
- **Concurrency ceiling: 3 simultaneous sandboxes** on the target single Hetzner CX22 deployment (4 GB RAM ÷ 1 GB/container, minus overhead) — this is a deliberate capacity target for a demonstration system, not a scaling limit of the design.
- **No frontend integration tested end to end.** The web UI exists as a skeleton; a real user has not yet driven a job through it.

## Roadmap

- **Phase 3 — Product**: GitHub integration, artifact upload and deploy
- **Phase 4 — Proof**: observability, chaos and load testing, security hardening
- **Phase 5 — Presentation**: demo, final documentation, public launch

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## License

[Apache 2.0](LICENSE)
