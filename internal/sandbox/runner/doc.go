// Package runner is the sandbox Runner: the only code in this repository
// allowed to talk to Docker directly (a linter rule enforces this). It
// owns the container lifecycle — create, run a command, destroy — streams
// command output back as it happens, and kills commands that run past
// their timeout. It's reached over HTTP through the client in
// internal/sandbox.
//
// Every container hardening setting lives in one place, container.go's
// dockerCreateOpts, so a test can check each one individually.
package runner
