package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// problem is an RFC 7807 problem+json document (PRD §11.1). This is the
// one place errors cross the API boundary, per CODE-STANDARDS E8.
type problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
	TraceID  string `json:"trace_id,omitempty"`
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, typ, title, detail string) {
	p := problem{
		Type:     typ,
		Title:    title,
		Status:   status,
		Detail:   detail,
		Instance: r.URL.Path,
		TraceID:  traceIDFromContext(r.Context()),
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

func writeInvalidCredentials(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, http.StatusUnauthorized,
		"https://anvil.dev/errors/invalid-credentials",
		"Invalid credentials",
		"the email or password is incorrect")
}

// writeInternalError logs err once, at this outermost layer (CODE-STANDARDS
// E2), then returns an opaque 500 — the caller's err is never leaked to the
// client.
func (s *Server) writeInternalError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("internal error",
		slog.String("trace_id", traceIDFromContext(r.Context())),
		slog.String("path", r.URL.Path),
		slog.Any("err", err))
	writeProblem(w, r, http.StatusInternalServerError,
		"https://anvil.dev/errors/internal",
		"Internal server error",
		"")
}
