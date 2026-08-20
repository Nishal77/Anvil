package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/auth"
)

func TestHandleBeginGitHubOAuth_MissingTokenIsUnauthorized(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeAuth{}, &fakePinger{})

	req := httptest.NewRequest(http.MethodGet, "/auth/github", nil)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestHandleBeginGitHubOAuth_RedirectsToAuthorizeURL(t *testing.T) {
	t.Parallel()
	userID := uuid.New()
	a := &fakeAuth{
		verifyFn: func(_ string) (uuid.UUID, error) { return userID, nil },
		beginGitHubOAuthFn: func(_ uuid.UUID) (string, error) {
			return "https://github.com/login/oauth/authorize?state=abc", nil
		},
	}
	srv := newTestServer(t, a, &fakePinger{})

	req := httptest.NewRequest(http.MethodGet, "/auth/github?access_token=valid", nil)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusFound)
	}
	if loc := w.Header().Get("Location"); loc != "https://github.com/login/oauth/authorize?state=abc" {
		t.Errorf("Location = %q, want the authorize URL", loc)
	}
}

func TestHandleBeginGitHubOAuth_NotConfiguredReturns503(t *testing.T) {
	t.Parallel()
	userID := uuid.New()
	a := &fakeAuth{
		verifyFn:           func(_ string) (uuid.UUID, error) { return userID, nil },
		beginGitHubOAuthFn: func(_ uuid.UUID) (string, error) { return "", auth.ErrGitHubNotConfigured },
	}
	srv := newTestServer(t, a, &fakePinger{})

	req := httptest.NewRequest(http.MethodGet, "/auth/github?access_token=valid", nil)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestHandleGitHubCallback_MissingParamsIsBadRequest(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeAuth{}, &fakePinger{})

	req := httptest.NewRequest(http.MethodGet, "/auth/github/callback", nil)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleGitHubCallback_RedirectsToWebURLOnSuccess(t *testing.T) {
	t.Parallel()
	a := &fakeAuth{
		completeGitHubOAuthFn: func(_ context.Context, code, state string) (string, error) {
			if code != "the-code" || state != "the-state" {
				t.Errorf("CompleteGitHubOAuth called with (%q, %q)", code, state)
			}
			return "https://app.anvil.example.com/settings", nil
		},
	}
	srv := newTestServer(t, a, &fakePinger{})

	req := httptest.NewRequest(http.MethodGet, "/auth/github/callback?code=the-code&state=the-state", nil)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusFound)
	}
	if loc := w.Header().Get("Location"); loc != "https://app.anvil.example.com/settings" {
		t.Errorf("Location = %q, want the configured web URL", loc)
	}
}

func TestHandleGitHubCallback_ReturnsJSONWhenNoWebURLConfigured(t *testing.T) {
	t.Parallel()
	a := &fakeAuth{
		completeGitHubOAuthFn: func(_ context.Context, _, _ string) (string, error) { return "", nil },
	}
	srv := newTestServer(t, a, &fakePinger{})

	req := httptest.NewRequest(http.MethodGet, "/auth/github/callback?code=c&state=s", nil)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if !strings.Contains(w.Body.String(), "linked") {
		t.Errorf("body = %s, want a JSON confirmation", w.Body)
	}
}

func TestHandleGitHubCallback_InvalidStateReturns400(t *testing.T) {
	t.Parallel()
	a := &fakeAuth{
		completeGitHubOAuthFn: func(_ context.Context, _, _ string) (string, error) { return "", auth.ErrInvalidOAuthState },
	}
	srv := newTestServer(t, a, &fakePinger{})

	req := httptest.NewRequest(http.MethodGet, "/auth/github/callback?code=c&state=s", nil)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleGitHubCallback_AlreadyLinkedReturns409(t *testing.T) {
	t.Parallel()
	a := &fakeAuth{
		completeGitHubOAuthFn: func(_ context.Context, _, _ string) (string, error) {
			return "", auth.ErrGitHubAccountAlreadyLinked
		},
	}
	srv := newTestServer(t, a, &fakePinger{})

	req := httptest.NewRequest(http.MethodGet, "/auth/github/callback?code=c&state=s", nil)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusConflict)
	}
}
