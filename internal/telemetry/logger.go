package telemetry

import (
	"io"
	"log/slog"
)

// NewLogger returns a slog.Logger that writes JSON to w, with component
// bound as a static field on every record it produces (PRD §17.5 — every
// log line carries trace_id, job_id where applicable, and component).
func NewLogger(w io.Writer, component string) *slog.Logger {
	handler := slog.NewJSONHandler(w, nil)
	return slog.New(handler).With(slog.String("component", component))
}
