// Package sandbox is the client used to talk to the Sandbox Runner over
// HTTP. It never touches Docker directly — that's the Runner's job alone
// (internal/sandbox/runner). A linter rule blocks this package from
// importing a Docker client at all, so that boundary can't be crossed
// by accident.
//
// Entry points:
//
//	New     - construct a Client from Config
//	Create  — create a sandbox for a job
//	Exec    — run one command, streaming output incrementally
//	Destroy — tear down a sandbox
package sandbox
