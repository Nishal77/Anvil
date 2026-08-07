// Package api is the HTTP + SSE edge. It terminates HTTP, authenticates,
// authorizes, and validates, and contains zero business logic (PRD §9.1).
// Depends on auth, storage, telemetry (CLAUDE.md PK5).
package api
