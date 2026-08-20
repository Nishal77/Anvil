package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/storage"
)

// store is the subset of storage the auth service needs (CODE-STANDARDS
// §3.1 — declared at the consumer, not exported by storage).
type store interface {
	CreateUser(ctx context.Context, email, passwordHash string) (storage.User, error)
	GetUserByEmail(ctx context.Context, email string) (storage.User, error)
	CreateRefreshToken(ctx context.Context, userID uuid.UUID, tokenHash []byte, expiresAt time.Time) (storage.RefreshToken, error)
	GetRefreshTokenByHash(ctx context.Context, tokenHash []byte) (storage.RefreshToken, error)
	RevokeRefreshToken(ctx context.Context, id uuid.UUID) error
	UpsertSecret(ctx context.Context, userID uuid.UUID, name string, ciphertext, nonce []byte) error
	ListSecretNames(ctx context.Context, userID uuid.UUID) ([]string, error)
	GetSecret(ctx context.Context, userID uuid.UUID, name string) (storage.Secret, error)
	DeleteSecret(ctx context.Context, userID uuid.UUID, name string) error
	SetGitHubIdentity(ctx context.Context, userID uuid.UUID, githubID int64, githubLogin string) error
}

// Config configures a Service.
type Config struct {
	Store           store
	Logger          *slog.Logger
	JWTSigningKey   []byte
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	// EncryptionKey seals user secrets (PRD §16.5) — AES-256-GCM, so it
	// must be exactly 32 bytes. Never persisted: it lives only in
	// ANVIL_SECRET_ENCRYPTION_KEY, so a database dump alone can never
	// yield a usable secret.
	EncryptionKey []byte
	// GitHub OAuth app credentials (FR-001). All three optional
	// together: an unset GitHubClientID means GitHub linking is simply
	// unavailable rather than failing startup, matching S3's
	// unset-means-skipped pattern — but if any one of the three is set,
	// all three are required, since a partially configured OAuth app
	// can't redeem a code even once.
	GitHubClientID     string
	GitHubClientSecret string
	GitHubRedirectURL  string
	// GitHubWebURL is the frontend base URL CompleteGitHubOAuth sends
	// the browser back to after linking. Empty means no frontend is
	// configured — the callback returns a JSON confirmation instead of
	// redirecting.
	GitHubWebURL string
	// GitHubOAuthBaseURL and GitHubAPIBaseURL default to github.com and
	// api.github.com; overridable so tests never make a real network
	// call (CLAUDE.md T3's analog for a non-LLM external API).
	GitHubOAuthBaseURL string
	GitHubAPIBaseURL   string
	// HTTPClient calls github.com. Nil defaults to http.DefaultClient.
	HTTPClient *http.Client
}

func (c Config) validate() error {
	if c.Store == nil {
		return errors.New("auth: config: Store is required")
	}
	if c.Logger == nil {
		return errors.New("auth: config: Logger is required")
	}
	if len(c.JWTSigningKey) == 0 {
		return errors.New("auth: config: JWTSigningKey is required")
	}
	if c.AccessTokenTTL <= 0 {
		return errors.New("auth: config: AccessTokenTTL must be positive")
	}
	if c.RefreshTokenTTL <= 0 {
		return errors.New("auth: config: RefreshTokenTTL must be positive")
	}
	if len(c.EncryptionKey) != secretEncryptionKeyBytes {
		return fmt.Errorf("auth: config: EncryptionKey must be %d bytes, got %d", secretEncryptionKeyBytes, len(c.EncryptionKey))
	}
	githubFieldsSet := boolCount(c.GitHubClientID != "", c.GitHubClientSecret != "", c.GitHubRedirectURL != "")
	if githubFieldsSet != 0 && githubFieldsSet != 3 {
		return errors.New("auth: config: GitHubClientID, GitHubClientSecret, and GitHubRedirectURL must be set together or not at all")
	}
	return nil
}

func boolCount(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// TokenPair is an issued access token and refresh token.
type TokenPair struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// Service implements registration, login, token refresh, logout, and
// user secret storage.
type Service struct {
	store           store
	log             *slog.Logger
	signingKey      []byte
	accessTokenTTL  time.Duration
	refreshTokenTTL time.Duration
	encryptionKey   []byte

	githubClientID     string
	githubClientSecret string
	githubRedirectURL  string
	githubWebURL       string
	githubOAuthBaseURL string
	githubAPIBaseURL   string
	httpClient         *http.Client

	// dummyPasswordHash is verified against on a login attempt for an
	// unknown email, so Login takes the same time whether or not the
	// account exists. Computed once at construction (not package-init,
	// which can't return an error, and not lazily with panic — CLAUDE.md
	// §5.2 forbids panic outside cmd/*/main.go).
	dummyPasswordHash string
}

// New constructs a Service from cfg, or returns an error if cfg is
// invalid.
func New(cfg Config) (*Service, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	dummyHash, err := hashPassword("anvil-dummy-password-for-constant-time-login")
	if err != nil {
		return nil, fmt.Errorf("auth: precompute dummy password hash: %w", err)
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	githubOAuthBaseURL := cfg.GitHubOAuthBaseURL
	if githubOAuthBaseURL == "" {
		githubOAuthBaseURL = defaultGitHubOAuthBaseURL
	}
	githubAPIBaseURL := cfg.GitHubAPIBaseURL
	if githubAPIBaseURL == "" {
		githubAPIBaseURL = defaultGitHubAPIBaseURL
	}

	return &Service{
		store:              cfg.Store,
		log:                cfg.Logger,
		signingKey:         cfg.JWTSigningKey,
		accessTokenTTL:     cfg.AccessTokenTTL,
		refreshTokenTTL:    cfg.RefreshTokenTTL,
		encryptionKey:      cfg.EncryptionKey,
		githubClientID:     cfg.GitHubClientID,
		githubClientSecret: cfg.GitHubClientSecret,
		githubRedirectURL:  cfg.GitHubRedirectURL,
		githubWebURL:       cfg.GitHubWebURL,
		githubOAuthBaseURL: githubOAuthBaseURL,
		githubAPIBaseURL:   githubAPIBaseURL,
		httpClient:         httpClient,
		dummyPasswordHash:  dummyHash,
	}, nil
}

// Register creates a new user and returns an issued token pair.
func (s *Service) Register(ctx context.Context, email, password string) (TokenPair, error) {
	hash, err := hashPassword(password)
	if err != nil {
		return TokenPair{}, fmt.Errorf("auth: register: %w", err)
	}

	// Email is deliberately omitted from wrapped errors below (and from
	// every log line) — it's PII (CODE-STANDARDS L5), and user_id is
	// already logged once issuePair succeeds.
	user, err := s.store.CreateUser(ctx, email, hash)
	if err != nil {
		return TokenPair{}, fmt.Errorf("auth: register: %w", err)
	}

	pair, err := s.issuePair(ctx, user.ID)
	if err != nil {
		return TokenPair{}, err
	}
	s.log.InfoContext(ctx, "user registered", slog.String("user_id", user.ID.String()))
	return pair, nil
}

// Login verifies credentials and returns an issued token pair. Returns
// ErrInvalidCredentials on any mismatch — never distinguishes "no such
// user" from "wrong password" (SEC-030 — no user enumeration), including in
// response timing: verifyPassword always runs, against dummyPasswordHash
// when the account doesn't exist.
func (s *Service) Login(ctx context.Context, email, password string) (TokenPair, error) {
	user, err := s.store.GetUserByEmail(ctx, email)
	found := true
	if err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			return TokenPair{}, fmt.Errorf("auth: login: %w", err)
		}
		found = false
		user.PasswordHash = s.dummyPasswordHash
	}

	ok, err := verifyPassword(user.PasswordHash, password)
	if err != nil {
		return TokenPair{}, fmt.Errorf("auth: login: %w", err)
	}
	if !found || !ok {
		return TokenPair{}, ErrInvalidCredentials
	}

	pair, err := s.issuePair(ctx, user.ID)
	if err != nil {
		return TokenPair{}, err
	}
	s.log.InfoContext(ctx, "user logged in", slog.String("user_id", user.ID.String()))
	return pair, nil
}

// Refresh rotates a refresh token: the presented token is revoked and a
// new pair is issued. Returns ErrRefreshTokenExpired or
// ErrRefreshTokenRevoked as appropriate.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (TokenPair, error) {
	rt, err := s.store.GetRefreshTokenByHash(ctx, hashRefreshToken(refreshToken))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return TokenPair{}, ErrRefreshTokenRevoked
		}
		return TokenPair{}, fmt.Errorf("auth: refresh: %w", err)
	}
	if rt.RevokedAt != nil {
		return TokenPair{}, ErrRefreshTokenRevoked
	}
	if time.Now().After(rt.ExpiresAt) {
		return TokenPair{}, ErrRefreshTokenExpired
	}

	if err := s.store.RevokeRefreshToken(ctx, rt.ID); err != nil {
		return TokenPair{}, fmt.Errorf("auth: refresh: revoke used token: %w", err)
	}

	return s.issuePair(ctx, rt.UserID)
}

// Logout revokes refreshToken, but only if it belongs to callerID — the
// user ID the API layer's auth middleware verified from the request's
// Bearer token. A token that doesn't exist, or exists but belongs to a
// different user, is treated identically to "already logged out": no
// error, no revoke, no signal to the caller about which case it was
// (Phase 1 adversarial review, MAJOR: this used to revoke whatever token
// was presented in the body regardless of who was authenticated).
func (s *Service) Logout(ctx context.Context, callerID uuid.UUID, refreshToken string) error {
	rt, err := s.store.GetRefreshTokenByHash(ctx, hashRefreshToken(refreshToken))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("auth: logout: %w", err)
	}
	if rt.UserID != callerID {
		return nil
	}
	if err := s.store.RevokeRefreshToken(ctx, rt.ID); err != nil {
		return fmt.Errorf("auth: logout: %w", err)
	}
	s.log.InfoContext(ctx, "user logged out", slog.String("user_id", callerID.String()))
	return nil
}

// VerifyAccessToken parses and validates an access token, returning the
// subject user ID. Used by the API layer's auth middleware to protect
// routes that require a Bearer token (e.g. POST /auth/logout, PRD §11.2).
func (s *Service) VerifyAccessToken(token string) (uuid.UUID, error) {
	return verifyAccessToken(s.signingKey, token)
}

func (s *Service) issuePair(ctx context.Context, userID uuid.UUID) (TokenPair, error) {
	accessToken, expiresAt, err := issueAccessToken(s.signingKey, userID, s.accessTokenTTL)
	if err != nil {
		return TokenPair{}, fmt.Errorf("auth: issue pair for user %s: %w", userID, err)
	}

	refreshToken, hash, err := newRefreshToken()
	if err != nil {
		return TokenPair{}, fmt.Errorf("auth: issue pair for user %s: %w", userID, err)
	}
	if _, err := s.store.CreateRefreshToken(ctx, userID, hash, time.Now().Add(s.refreshTokenTTL)); err != nil {
		return TokenPair{}, fmt.Errorf("auth: issue pair for user %s: %w", userID, err)
	}

	return TokenPair{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
	}, nil
}
