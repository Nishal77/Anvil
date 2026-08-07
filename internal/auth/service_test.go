package auth

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/storage"
)

// fakeStore is an in-memory userStore for unit tests. Not a testcontainers
// integration test — CODE-STANDARDS §3.1's whole point is that this fake is
// two methods' worth of real work, not twenty.
type fakeStore struct {
	usersByEmail map[string]storage.User
	tokens       map[string]storage.RefreshToken // keyed by string(hash)
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		usersByEmail: make(map[string]storage.User),
		tokens:       make(map[string]storage.RefreshToken),
	}
}

func (f *fakeStore) CreateUser(_ context.Context, email, passwordHash string) (storage.User, error) {
	if _, exists := f.usersByEmail[email]; exists {
		return storage.User{}, storage.ErrDuplicateEmail
	}
	u := storage.User{ID: uuid.New(), Email: email, PasswordHash: passwordHash, CreatedAt: time.Now()}
	f.usersByEmail[email] = u
	return u, nil
}

func (f *fakeStore) GetUserByEmail(_ context.Context, email string) (storage.User, error) {
	u, ok := f.usersByEmail[email]
	if !ok {
		return storage.User{}, storage.ErrNotFound
	}
	return u, nil
}

func (f *fakeStore) CreateRefreshToken(_ context.Context, userID uuid.UUID, tokenHash []byte, expiresAt time.Time) (storage.RefreshToken, error) {
	rt := storage.RefreshToken{ID: uuid.New(), UserID: userID, TokenHash: tokenHash, ExpiresAt: expiresAt}
	f.tokens[string(tokenHash)] = rt
	return rt, nil
}

func (f *fakeStore) GetRefreshTokenByHash(_ context.Context, tokenHash []byte) (storage.RefreshToken, error) {
	rt, ok := f.tokens[string(tokenHash)]
	if !ok {
		return storage.RefreshToken{}, storage.ErrNotFound
	}
	return rt, nil
}

func (f *fakeStore) RevokeRefreshToken(_ context.Context, id uuid.UUID) error {
	for k, rt := range f.tokens {
		if rt.ID == id {
			now := time.Now()
			rt.RevokedAt = &now
			f.tokens[k] = rt
		}
	}
	return nil
}

func newTestService(t *testing.T, store userStore) *Service {
	t.Helper()
	svc, err := New(Config{
		Store:           store,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		JWTSigningKey:   testSigningKey,
		AccessTokenTTL:  15 * time.Minute,
		RefreshTokenTTL: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	return svc
}

func TestFR001_RegisterIssuesTokenPair(t *testing.T) {
	t.Parallel()
	svc := newTestService(t, newFakeStore())

	pair, err := svc.Register(context.Background(), "alice@example.com", "hunter22")
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Fatal("Register() returned an empty token")
	}
}

func TestFR001_LoginWithValidCredentialsSucceeds(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	svc := newTestService(t, store)
	ctx := context.Background()

	if _, err := svc.Register(ctx, "bob@example.com", "correcthorse"); err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	pair, err := svc.Login(ctx, "bob@example.com", "correcthorse")
	if err != nil {
		t.Fatalf("Login() error: %v", err)
	}
	if pair.AccessToken == "" {
		t.Fatal("Login() returned an empty access token")
	}
}

func TestSEC030_LoginWithWrongPasswordReturnsGenericError(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	svc := newTestService(t, store)
	ctx := context.Background()

	if _, err := svc.Register(ctx, "carol@example.com", "rightpassword"); err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	_, wrongPasswordErr := svc.Login(ctx, "carol@example.com", "wrongpassword")
	_, unknownEmailErr := svc.Login(ctx, "nobody@example.com", "irrelevant")

	if wrongPasswordErr != ErrInvalidCredentials {
		t.Errorf("wrong password error = %v, want ErrInvalidCredentials", wrongPasswordErr)
	}
	if unknownEmailErr != ErrInvalidCredentials {
		t.Errorf("unknown email error = %v, want ErrInvalidCredentials", unknownEmailErr)
	}
}

// TestSEC030_LoginTimingIsConstantRegardlessOfAccountExistence guards
// against the timing side-channel found in the Phase 1 adversarial review:
// an unknown-email login used to return before ever hashing, making it
// measurably faster than a wrong-password login against a real account —
// enumerable despite the identical error value. Both paths now run
// Argon2id, so their durations should be comparable, not orders of
// magnitude apart.
func TestSEC030_LoginTimingIsConstantRegardlessOfAccountExistence(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	svc := newTestService(t, store)
	ctx := context.Background()

	if _, err := svc.Register(ctx, "gina@example.com", "rightpassword"); err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	const samples = 5
	var knownTotal, unknownTotal time.Duration
	for range samples {
		start := time.Now()
		_, _ = svc.Login(ctx, "gina@example.com", "wrongpassword")
		knownTotal += time.Since(start)

		start = time.Now()
		_, _ = svc.Login(ctx, "nobody@example.com", "irrelevant")
		unknownTotal += time.Since(start)
	}

	ratio := float64(unknownTotal) / float64(knownTotal)
	if ratio < 0.3 {
		t.Errorf("unknown-account login took %v, known-account took %v (ratio %.2f) — "+
			"unknown-account path is returning early instead of hashing, which leaks account existence via timing",
			unknownTotal/samples, knownTotal/samples, ratio)
	}
}

func TestAuthService_Logout_OnlyRevokesCallersOwnToken(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	svc := newTestService(t, store)
	ctx := context.Background()

	victim, err := svc.Register(ctx, "victim@example.com", "password123")
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	attacker, err := svc.Register(ctx, "attacker@example.com", "password123")
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	attackerUserID, err := svc.VerifyAccessToken(attacker.AccessToken)
	if err != nil {
		t.Fatalf("VerifyAccessToken() error: %v", err)
	}

	// Attacker is authenticated as themselves but presents the victim's
	// refresh token in the body.
	if err := svc.Logout(ctx, attackerUserID, victim.RefreshToken); err != nil {
		t.Fatalf("Logout() error: %v", err)
	}

	// The victim's token must still work — the attacker's request should
	// have been a silent no-op, not a cross-account revoke.
	if _, err := svc.Refresh(ctx, victim.RefreshToken); err != nil {
		t.Errorf("victim's refresh token was revoked by a different authenticated user: %v", err)
	}
}

func TestFR001_RefreshRotatesToken(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	svc := newTestService(t, store)
	ctx := context.Background()

	original, err := svc.Register(ctx, "dave@example.com", "password123")
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	rotated, err := svc.Refresh(ctx, original.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh() error: %v", err)
	}
	if rotated.RefreshToken == original.RefreshToken {
		t.Error("Refresh() returned the same refresh token — no rotation occurred")
	}
}

func TestFR001_RefreshWithRevokedTokenFails(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	svc := newTestService(t, store)
	ctx := context.Background()

	original, err := svc.Register(ctx, "erin@example.com", "password123")
	if err != nil {
		t.Fatalf("Register() error: %v", err)
	}
	if _, err := svc.Refresh(ctx, original.RefreshToken); err != nil {
		t.Fatalf("first Refresh() error: %v", err)
	}

	_, err = svc.Refresh(ctx, original.RefreshToken)
	if err != ErrRefreshTokenRevoked {
		t.Errorf("second Refresh() with the same token = %v, want ErrRefreshTokenRevoked", err)
	}
}

func TestFR001_RefreshWithExpiredTokenFails(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	svc := newTestService(t, store)
	ctx := context.Background()

	// The service never mints an already-expired token, so exercise the
	// expiry path by inserting one directly into the fake store — the
	// raw token and its hash are generated together since the hash is
	// one-way.
	userID := uuid.New()
	token, hash, err := newRefreshToken()
	if err != nil {
		t.Fatalf("newRefreshToken() error: %v", err)
	}
	if _, err := store.CreateRefreshToken(ctx, userID, hash, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("CreateRefreshToken() error: %v", err)
	}

	_, err = svc.Refresh(ctx, token)
	if err != ErrRefreshTokenExpired {
		t.Errorf("Refresh() with expired token = %v, want ErrRefreshTokenExpired", err)
	}
}

func TestAuthService_Register_DuplicateEmailFails(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	svc := newTestService(t, store)
	ctx := context.Background()

	if _, err := svc.Register(ctx, "frank@example.com", "password123"); err != nil {
		t.Fatalf("first Register() error: %v", err)
	}

	_, err := svc.Register(ctx, "frank@example.com", "different")
	if err == nil {
		t.Fatal("second Register() with the same email succeeded, want error")
	}
}
