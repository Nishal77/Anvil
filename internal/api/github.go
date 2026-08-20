package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/anvil-dev/anvil/internal/auth"
)

// handleBeginGitHubOAuth — GET /auth/github?access_token=<jwt>. Not
// behind requireAuth: a top-level browser navigation to GitHub's
// consent screen can't carry an Authorization header, so the access
// token travels as a query parameter instead (the same accommodation
// requireAuthQueryToken already makes for EventSource, middleware.go).
func (s *Server) handleBeginGitHubOAuth(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("access_token")
	if token == "" {
		writeInvalidCredentials(w, r)
		return
	}
	userID, err := s.auth.VerifyAccessToken(token)
	if err != nil {
		writeInvalidCredentials(w, r)
		return
	}

	redirectURL, err := s.auth.BeginGitHubOAuth(userID)
	if err != nil {
		if errors.Is(err, auth.ErrGitHubNotConfigured) {
			writeProblem(w, r, http.StatusServiceUnavailable, "https://anvil.dev/errors/github-not-configured", "GitHub integration not configured", "")
			return
		}
		s.writeInternalError(w, r, err)
		return
	}
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

type githubLinkedResponse struct {
	Status string `json:"status"`
}

// handleGitHubCallback — GET /auth/github/callback?code=...&state=....
// The state parameter (not an Authorization header) is what proves
// which Anvil user initiated this — see handleBeginGitHubOAuth.
func (s *Server) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		writeProblem(w, r, http.StatusBadRequest, "https://anvil.dev/errors/invalid-request", "Invalid request", "code and state are required")
		return
	}

	redirectURL, err := s.auth.CompleteGitHubOAuth(r.Context(), code, state)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrInvalidOAuthState), errors.Is(err, auth.ErrOAuthStateExpired):
			writeProblem(w, r, http.StatusBadRequest, "https://anvil.dev/errors/invalid-oauth-state", "Invalid or expired OAuth state", "")
		case errors.Is(err, auth.ErrGitHubAccountAlreadyLinked):
			writeProblem(w, r, http.StatusConflict, "https://anvil.dev/errors/github-account-linked", "GitHub account already linked to a different user", "")
		case errors.Is(err, auth.ErrGitHubNotConfigured):
			writeProblem(w, r, http.StatusServiceUnavailable, "https://anvil.dev/errors/github-not-configured", "GitHub integration not configured", "")
		default:
			s.writeInternalError(w, r, err)
		}
		return
	}

	if redirectURL != "" {
		http.Redirect(w, r, redirectURL, http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(githubLinkedResponse{Status: "linked"})
}
