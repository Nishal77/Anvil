package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// newTestGitHubService wires a Service against a fake GitHub server
// (both the OAuth token endpoint and the user API), so these tests
// never make a real network call (CLAUDE.md T3's analog for a non-LLM
// external API).
func newTestGitHubService(t *testing.T, githubServerURL string) *Service {
	t.Helper()
	svc, err := New(Config{
		Store:              newFakeStore(),
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		JWTSigningKey:      testSigningKey,
		AccessTokenTTL:     15 * time.Minute,
		RefreshTokenTTL:    7 * 24 * time.Hour,
		EncryptionKey:      testEncryptionKey,
		GitHubClientID:     "test-client-id",
		GitHubClientSecret: "test-client-secret",
		GitHubRedirectURL:  "https://anvil.example.com/auth/github/callback",
		GitHubWebURL:       "https://app.anvil.example.com/settings",
		GitHubOAuthBaseURL: githubServerURL,
		GitHubAPIBaseURL:   githubServerURL,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return svc
}

func TestBeginGitHubOAuth_NotConfiguredReturnsError(t *testing.T) {
	t.Parallel()
	svc := newTestService(t, newFakeStore()) // no GitHub fields set

	_, err := svc.BeginGitHubOAuth(uuid.New())
	if !errors.Is(err, ErrGitHubNotConfigured) {
		t.Errorf("BeginGitHubOAuth() error = %v, want ErrGitHubNotConfigured", err)
	}
}

func TestBeginGitHubOAuth_ReturnsAuthorizeURLWithState(t *testing.T) {
	t.Parallel()
	svc := newTestGitHubService(t, "https://github.example.com")
	userID := uuid.New()

	redirectURL, err := svc.BeginGitHubOAuth(userID)
	if err != nil {
		t.Fatalf("BeginGitHubOAuth() error: %v", err)
	}

	u, err := url.Parse(redirectURL)
	if err != nil {
		t.Fatalf("parse redirect URL: %v", err)
	}
	if u.Path != "/login/oauth/authorize" {
		t.Errorf("path = %q, want /login/oauth/authorize", u.Path)
	}
	q := u.Query()
	if q.Get("client_id") != "test-client-id" {
		t.Errorf("client_id = %q, want test-client-id", q.Get("client_id"))
	}
	if q.Get("state") == "" {
		t.Error("state is empty")
	}

	// The state must actually resolve back to userID — that's the
	// entire mechanism CompleteGitHubOAuth relies on to know which
	// Anvil user is linking their GitHub account.
	gotUserID, err := decodeOAuthState(testSigningKey, q.Get("state"))
	if err != nil {
		t.Fatalf("decodeOAuthState() error: %v", err)
	}
	if gotUserID != userID {
		t.Errorf("state decodes to user %s, want %s", gotUserID, userID)
	}
}

// fakeGitHubServer serves both the token exchange and user endpoints a
// real GitHub OAuth app would hit.
func fakeGitHubServer(t *testing.T, wantCode string, tokenValue string, userID int64, userLogin string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			body, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(body))
			if wantCode != "" && form.Get("code") != wantCode {
				_ = json.NewEncoder(w).Encode(githubTokenResponse{Error: "bad_verification_code", ErrorDesc: "unexpected code"})
				return
			}
			_ = json.NewEncoder(w).Encode(githubTokenResponse{AccessToken: tokenValue})
		case "/user":
			if r.Header.Get("Authorization") != "Bearer "+tokenValue {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(githubUserResponse{ID: userID, Login: userLogin})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestCompleteGitHubOAuth_StoresTokenAndLinksIdentity(t *testing.T) {
	t.Parallel()
	server := fakeGitHubServer(t, "valid-code", "gho_faketoken", 4242, "octocat")
	defer server.Close()

	svc := newTestGitHubService(t, server.URL)
	userID := uuid.New()
	state, err := encodeOAuthState(testSigningKey, userID)
	if err != nil {
		t.Fatalf("encodeOAuthState() error: %v", err)
	}

	redirectURL, err := svc.CompleteGitHubOAuth(context.Background(), "valid-code", state)
	if err != nil {
		t.Fatalf("CompleteGitHubOAuth() error: %v", err)
	}
	if redirectURL != "https://app.anvil.example.com/settings" {
		t.Errorf("redirectURL = %q, want the configured GitHubWebURL", redirectURL)
	}

	stored, err := svc.ResolveSecret(context.Background(), userID, githubSecretName)
	if err != nil {
		t.Fatalf("ResolveSecret() error: %v", err)
	}
	if stored != "gho_faketoken" {
		t.Errorf("stored secret = %q, want the exchanged token", stored)
	}
}

func TestCompleteGitHubOAuth_InvalidStateFails(t *testing.T) {
	t.Parallel()
	server := fakeGitHubServer(t, "valid-code", "gho_faketoken", 4242, "octocat")
	defer server.Close()

	svc := newTestGitHubService(t, server.URL)

	_, err := svc.CompleteGitHubOAuth(context.Background(), "valid-code", "not-a-real-state")
	if !errors.Is(err, ErrInvalidOAuthState) {
		t.Errorf("CompleteGitHubOAuth() error = %v, want ErrInvalidOAuthState", err)
	}
}

// TestCompleteGitHubOAuth_ForgedStateFails proves a state value can't
// be tampered with — flipping the user ID it carries must invalidate
// the HMAC tag, not silently link the wrong account.
func TestCompleteGitHubOAuth_ForgedStateFails(t *testing.T) {
	t.Parallel()
	server := fakeGitHubServer(t, "valid-code", "gho_faketoken", 4242, "octocat")
	defer server.Close()

	svc := newTestGitHubService(t, server.URL)
	state, err := encodeOAuthState(testSigningKey, uuid.New())
	if err != nil {
		t.Fatalf("encodeOAuthState() error: %v", err)
	}
	forged := state[:len(state)-2] + "xx"

	_, err = svc.CompleteGitHubOAuth(context.Background(), "valid-code", forged)
	if err == nil {
		t.Fatal("CompleteGitHubOAuth() with a forged state succeeded, want an error")
	}
}

func TestCompleteGitHubOAuth_GitHubRejectsCodeFails(t *testing.T) {
	t.Parallel()
	server := fakeGitHubServer(t, "valid-code", "gho_faketoken", 4242, "octocat")
	defer server.Close()

	svc := newTestGitHubService(t, server.URL)
	state, err := encodeOAuthState(testSigningKey, uuid.New())
	if err != nil {
		t.Fatalf("encodeOAuthState() error: %v", err)
	}

	_, err = svc.CompleteGitHubOAuth(context.Background(), "wrong-code", state)
	if err == nil {
		t.Fatal("CompleteGitHubOAuth() with a code GitHub rejects succeeded, want an error")
	}
}

func TestCompleteGitHubOAuth_DuplicateGitHubAccountFails(t *testing.T) {
	t.Parallel()
	server := fakeGitHubServer(t, "", "gho_faketoken", 4242, "octocat")
	defer server.Close()

	fs := newFakeStore()
	svc, err := New(Config{
		Store:              fs,
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		JWTSigningKey:      testSigningKey,
		AccessTokenTTL:     15 * time.Minute,
		RefreshTokenTTL:    7 * 24 * time.Hour,
		EncryptionKey:      testEncryptionKey,
		GitHubClientID:     "test-client-id",
		GitHubClientSecret: "test-client-secret",
		GitHubRedirectURL:  "https://anvil.example.com/auth/github/callback",
		GitHubOAuthBaseURL: server.URL,
		GitHubAPIBaseURL:   server.URL,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	firstUser := uuid.New()
	firstState, _ := encodeOAuthState(testSigningKey, firstUser)
	if _, err := svc.CompleteGitHubOAuth(context.Background(), "code", firstState); err != nil {
		t.Fatalf("first CompleteGitHubOAuth() error: %v", err)
	}

	secondUser := uuid.New()
	secondState, _ := encodeOAuthState(testSigningKey, secondUser)
	_, err = svc.CompleteGitHubOAuth(context.Background(), "code", secondState)
	if !errors.Is(err, ErrGitHubAccountAlreadyLinked) {
		t.Errorf("second CompleteGitHubOAuth() for the same GitHub account = %v, want ErrGitHubAccountAlreadyLinked", err)
	}
}

func TestConfig_PartialGitHubFieldsFails(t *testing.T) {
	t.Parallel()
	_, err := New(Config{
		Store:           newFakeStore(),
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		JWTSigningKey:   testSigningKey,
		AccessTokenTTL:  15 * time.Minute,
		RefreshTokenTTL: 7 * 24 * time.Hour,
		EncryptionKey:   testEncryptionKey,
		GitHubClientID:  "only-this-one-set",
	})
	if err == nil {
		t.Fatal("New() with only GitHubClientID set succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "GitHub") {
		t.Errorf("error = %v, want it to mention the GitHub fields", err)
	}
}
