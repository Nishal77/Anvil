package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
)

type traceIDKey struct{}

// traceIDFromContext returns the trace ID stashed by the traceID
// middleware, or "" if none is present.
func traceIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(traceIDKey{}).(string)
	return id
}

// traceID assigns a trace ID to every request, honoring an incoming
// X-Trace-Id header if present, and echoes it back on the response
// (FR-004).
func traceID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Trace-Id")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Trace-Id", id)
		ctx := context.WithValue(r.Context(), traceIDKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// recoverPanic converts a panicking handler into a 500 response instead of
// a crashed process (FR-004).
func recoverPanic(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic recovered",
						slog.Any("panic", rec),
						slog.String("trace_id", traceIDFromContext(r.Context())),
						slog.String("path", r.URL.Path))
					writeProblem(w, r, http.StatusInternalServerError,
						"https://anvil.dev/errors/internal", "Internal server error", "")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

type authenticatedUserKey struct{}

// authenticatedUserID returns the user ID the auth middleware verified for
// this request, or uuid.Nil if the request was not authenticated.
func authenticatedUserID(ctx context.Context) uuid.UUID {
	id, _ := ctx.Value(authenticatedUserKey{}).(uuid.UUID)
	return id
}

// verifier is the subset of auth the middleware needs.
type verifier interface {
	VerifyAccessToken(token string) (uuid.UUID, error)
}

// requireAuth rejects requests without a valid Bearer access token and
// stashes the verified user ID in the request context for handlers.
func requireAuth(v verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			const prefix = "Bearer "
			header := r.Header.Get("Authorization")
			if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
				writeInvalidCredentials(w, r)
				return
			}

			userID, err := v.VerifyAccessToken(header[len(prefix):])
			if err != nil {
				writeInvalidCredentials(w, r)
				return
			}

			ctx := context.WithValue(r.Context(), authenticatedUserKey{}, userID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
