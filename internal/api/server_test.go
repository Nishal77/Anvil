package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/auth"
)

// fakeAuth is a minimal authService fake — CODE-STANDARDS §3.1: the fake is
// as many methods as the interface, not a mock framework.
type fakeAuth struct {
	registerFn            func(ctx context.Context, email, password string) (auth.TokenPair, error)
	loginFn               func(ctx context.Context, email, password string) (auth.TokenPair, error)
	refreshFn             func(ctx context.Context, token string) (auth.TokenPair, error)
	logoutFn              func(ctx context.Context, callerID uuid.UUID, token string) error
	verifyFn              func(token string) (uuid.UUID, error)
	putSecretFn           func(ctx context.Context, userID uuid.UUID, name, plaintext string) error
	listSecretNamesFn     func(ctx context.Context, userID uuid.UUID) ([]string, error)
	deleteSecretFn        func(ctx context.Context, userID uuid.UUID, name string) error
	beginGitHubOAuthFn    func(userID uuid.UUID) (string, error)
	completeGitHubOAuthFn func(ctx context.Context, code, state string) (string, error)
}

func (f *fakeAuth) Register(ctx context.Context, email, password string) (auth.TokenPair, error) {
	return f.registerFn(ctx, email, password)
}

func (f *fakeAuth) Login(ctx context.Context, email, password string) (auth.TokenPair, error) {
	return f.loginFn(ctx, email, password)
}

func (f *fakeAuth) Refresh(ctx context.Context, token string) (auth.TokenPair, error) {
	return f.refreshFn(ctx, token)
}

func (f *fakeAuth) Logout(ctx context.Context, callerID uuid.UUID, token string) error {
	return f.logoutFn(ctx, callerID, token)
}

func (f *fakeAuth) VerifyAccessToken(token string) (uuid.UUID, error) {
	return f.verifyFn(token)
}

func (f *fakeAuth) PutSecret(ctx context.Context, userID uuid.UUID, name, plaintext string) error {
	return f.putSecretFn(ctx, userID, name, plaintext)
}

func (f *fakeAuth) ListSecretNames(ctx context.Context, userID uuid.UUID) ([]string, error) {
	return f.listSecretNamesFn(ctx, userID)
}

func (f *fakeAuth) DeleteSecret(ctx context.Context, userID uuid.UUID, name string) error {
	return f.deleteSecretFn(ctx, userID, name)
}

func (f *fakeAuth) BeginGitHubOAuth(userID uuid.UUID) (string, error) {
	return f.beginGitHubOAuthFn(userID)
}

func (f *fakeAuth) CompleteGitHubOAuth(ctx context.Context, code, state string) (string, error) {
	return f.completeGitHubOAuthFn(ctx, code, state)
}

type fakePinger struct {
	err error
}

func (f *fakePinger) Ping(_ context.Context) error {
	return f.err
}

func validTokenPair() auth.TokenPair {
	return auth.TokenPair{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)}
}

func newTestServer(t *testing.T, a *fakeAuth, p *fakePinger) *Server {
	t.Helper()
	srv, err := New(Config{
		Addr:       ":0",
		Auth:       a,
		Store:      p,
		Pool:       testPool,
		Hub:        &fakeHub{},
		EventStore: &fakeEventStore{},
		Publisher:  &fakePublisher{},
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return srv
}

func TestFR004_ResponseIncludesTraceIDHeader(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeAuth{}, &fakePinger{})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Header().Get("X-Trace-Id") == "" {
		t.Error("response missing X-Trace-Id header")
	}
}

func TestFR004_PanicInHandlerRecoversWith500(t *testing.T) {
	t.Parallel()
	a := &fakeAuth{
		loginFn: func(_ context.Context, _, _ string) (auth.TokenPair, error) {
			panic("boom")
		},
	}
	srv := newTestServer(t, a, &fakePinger{})

	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"email":"a@b.com","password":"x"}`))
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestFR006_ReadyzFailsWhenDatabaseUnreachable(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeAuth{}, &fakePinger{err: errors.New("connection refused")})

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Error("readyz returned 200 with an unreachable database")
	}
}

func TestFR006_HealthzAlwaysReturns200(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeAuth{}, &fakePinger{err: errors.New("db is down")})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("healthz status = %d, want 200 even with dependencies down", rec.Code)
	}
}

func TestServer_UnexpectedError_IsLoggedNotSwallowed(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	a := &fakeAuth{
		loginFn: func(_ context.Context, _, _ string) (auth.TokenPair, error) {
			return auth.TokenPair{}, errors.New("database is on fire")
		},
	}
	srv, err := New(Config{
		Addr:       ":0",
		Auth:       a,
		Store:      &fakePinger{},
		Pool:       testPool,
		Hub:        &fakeHub{},
		EventStore: &fakeEventStore{},
		Publisher:  &fakePublisher{},
		Logger:     slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"email":"a@b.com","password":"x"}`))
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(logs.String(), "database is on fire") {
		t.Error("unexpected error was returned as an opaque 500 but never logged")
	}
}

func TestServer_Register_ReturnsTokenPair(t *testing.T) {
	t.Parallel()
	a := &fakeAuth{
		registerFn: func(_ context.Context, _, _ string) (auth.TokenPair, error) {
			return validTokenPair(), nil
		},
	}
	srv := newTestServer(t, a, &fakePinger{})

	req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(`{"email":"a@b.com","password":"validpassword"}`))
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rec.Code)
	}
}

func TestServer_Register_RejectsShortPassword(t *testing.T) {
	t.Parallel()
	a := &fakeAuth{
		registerFn: func(_ context.Context, _, _ string) (auth.TokenPair, error) {
			t.Fatal("Register() called with an invalid password — validation should have rejected it first")
			return auth.TokenPair{}, nil
		},
	}
	srv := newTestServer(t, a, &fakePinger{})

	req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(`{"email":"a@b.com","password":"short"}`))
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestServer_Register_RejectsMalformedEmail(t *testing.T) {
	t.Parallel()
	a := &fakeAuth{
		registerFn: func(_ context.Context, _, _ string) (auth.TokenPair, error) {
			t.Fatal("Register() called with an invalid email — validation should have rejected it first")
			return auth.TokenPair{}, nil
		},
	}
	srv := newTestServer(t, a, &fakePinger{})

	req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(`{"email":"not-an-email","password":"validpassword"}`))
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestServer_Register_RejectsOversizedBody(t *testing.T) {
	t.Parallel()
	a := &fakeAuth{
		registerFn: func(_ context.Context, _, _ string) (auth.TokenPair, error) {
			t.Fatal("Register() called with an oversized body — decodeJSON should have rejected it first")
			return auth.TokenPair{}, nil
		},
	}
	srv := newTestServer(t, a, &fakePinger{})

	huge := `{"email":"a@b.com","password":"` + strings.Repeat("x", maxRequestBodyBytes*2) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/auth/register", strings.NewReader(huge))
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a body exceeding maxRequestBodyBytes", rec.Code)
	}
}

func TestServer_Logout_RequiresBearerToken(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeAuth{}, &fakePinger{})

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", strings.NewReader(`{"refresh_token":"x"}`))
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status without Authorization header = %d, want 401", rec.Code)
	}
}

func TestServer_Logout_WithValidBearerTokenSucceeds(t *testing.T) {
	t.Parallel()
	a := &fakeAuth{
		verifyFn: func(_ string) (uuid.UUID, error) { return uuid.New(), nil },
		logoutFn: func(_ context.Context, _ uuid.UUID, _ string) error { return nil },
	}
	srv := newTestServer(t, a, &fakePinger{})

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", strings.NewReader(`{"refresh_token":"x"}`))
	req.Header.Set("Authorization", "Bearer valid-token")
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
}

func TestServer_Logout_PassesVerifiedCallerIDNotBody(t *testing.T) {
	t.Parallel()
	verifiedUserID := uuid.New()
	var gotCallerID uuid.UUID

	a := &fakeAuth{
		verifyFn: func(_ string) (uuid.UUID, error) { return verifiedUserID, nil },
		logoutFn: func(_ context.Context, callerID uuid.UUID, _ string) error {
			gotCallerID = callerID
			return nil
		},
	}
	srv := newTestServer(t, a, &fakePinger{})

	req := httptest.NewRequest(http.MethodPost, "/auth/logout", strings.NewReader(`{"refresh_token":"someone-elses-token"}`))
	req.Header.Set("Authorization", "Bearer valid-token")
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if gotCallerID != verifiedUserID {
		t.Errorf("Logout received caller ID %v, want the Bearer-verified ID %v — logout must be scoped to the authenticated caller, not trust the request body", gotCallerID, verifiedUserID)
	}
}

func TestServer_CORS_PreflightSucceedsForBrowserOrigin(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeAuth{}, &fakePinger{})

	req := httptest.NewRequest(http.MethodOptions, "/v1/jobs", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the request's Origin echoed back", got)
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Errorf("Access-Control-Allow-Headers = %q, want it to allow Authorization", rec.Header().Get("Access-Control-Allow-Headers"))
	}
}

func TestServer_CORS_RealResponseCarriesAllowOrigin(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, &fakeAuth{}, &fakePinger{})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the request's Origin echoed back on a normal response too", got)
	}
}
