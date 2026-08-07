// Package storage is the Postgres access layer. It depends only on
// telemetry (CLAUDE.md PK5) and exposes Store as its concrete type — every
// consumer declares its own narrow interface over the methods it needs
// (CODE-STANDARDS §3.1).
package storage
