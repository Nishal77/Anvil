// Package telemetry configures structured logging and distributed tracing,
// used by every other package in the control plane (PRD §17.1, §17.5).
// It has no internal dependencies (CLAUDE.md PK5). Entry points: NewLogger for
// slog, NewTracerProvider to construct and globally register a process's
// tracer, Tracer to look one up by component name, WrapHandler to give an
// HTTP handler a root span, and TraceIDFromContext to read back the trace
// ID a span carries — the same value PRD §17.1 requires stored on
// jobs.trace_id and returned as X-Trace-Id.
package telemetry
