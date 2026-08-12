package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
)

// cors lets the web frontend, served from a different origin in dev
// (localhost:3000) and in production, call this API from a browser at
// all. Browsers refuse cross-origin fetch/EventSource calls without
// these headers, and preflight a POST that carries an Authorization
// header with an OPTIONS request this API otherwise has no route for —
// without this middleware, every browser-originated POST /v1/jobs fails
// before it even reaches a handler, with nothing more specific than
// "Failed to fetch" in the browser console. Reflecting the origin instead
// of a fixed one keeps this working across dev and prod without a config
// value to keep in sync; safe here since auth is a bearer token in a
// header, never a cookie, so there's no cross-site request forgery
// surface this could open up.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

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
					log.ErrorContext(r.Context(), "panic recovered",
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
			token, ok := bearerToken(r)
			if !ok {
				writeInvalidCredentials(w, r)
				return
			}
			authenticate(w, r, next, v, token)
		})
	}
}

// requireAuthSSE is requireAuth's counterpart for the one route a native
// browser EventSource has to hit: EventSource can't set an Authorization
// header at all, so this also accepts the token as ?access_token=. Every
// other route stays header-only.
func requireAuthSSE(v verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				token = r.URL.Query().Get("access_token")
			}
			if token == "" {
				writeInvalidCredentials(w, r)
				return
			}
			authenticate(w, r, next, v, token)
		})
	}
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return "", false
	}
	return header[len(prefix):], true
}

func authenticate(w http.ResponseWriter, r *http.Request, next http.Handler, v verifier, token string) {
	userID, err := v.VerifyAccessToken(token)
	if err != nil {
		writeInvalidCredentials(w, r)
		return
	}
	ctx := context.WithValue(r.Context(), authenticatedUserKey{}, userID)
	next.ServeHTTP(w, r.WithContext(ctx))
}
