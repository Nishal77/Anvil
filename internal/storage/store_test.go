package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// testStore is shared across every test in this package. It is set up once
// in TestMain against a real, containerized Postgres (CLAUDE.md T5: never
// mock the database) — spinning up a fresh container per test would make
// this suite too slow to run on every commit. Each test uses a unique
// email/token per call so tests can run without truncating shared tables
// between them.
var (
	testStore *Store
	testDSN   string
)

func TestMain(m *testing.M) {
	os.Exit(runTestMain(m))
}

func runTestMain(m *testing.M) int {
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("anvil_test"),
		postgres.WithUsername("anvil"),
		postgres.WithPassword("anvil"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "storage tests: start postgres container:", err)
		return 1
	}
	defer func() { _ = container.Terminate(ctx) }()

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "storage tests: get connection string:", err)
		return 1
	}

	store, err := New(ctx, connStr, 5)
	if err != nil {
		fmt.Fprintln(os.Stderr, "storage tests: connect:", err)
		return 1
	}
	defer store.Close()

	// go test sets the working directory to this package's directory, so
	// this path is relative to internal/storage/.
	migration, err := os.ReadFile("../../migrations/001_users.up.sql")
	if err != nil {
		fmt.Fprintln(os.Stderr, "storage tests: read migration:", err)
		return 1
	}
	if _, err := store.pool.Exec(ctx, string(migration)); err != nil {
		fmt.Fprintln(os.Stderr, "storage tests: apply migration:", err)
		return 1
	}

	testStore = store
	testDSN = connStr
	return m.Run()
}

func uniqueEmail(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s@example.com", uuid.NewString())
}

func TestStore_Ping_Succeeds(t *testing.T) {
	t.Parallel()
	if err := testStore.Ping(context.Background()); err != nil {
		t.Fatalf("Ping() error: %v", err)
	}
}

func TestStore_CreateUser_GetUserByEmail_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	email := uniqueEmail(t)

	created, err := testStore.CreateUser(ctx, email, "argon2id$fake-hash")
	if err != nil {
		t.Fatalf("CreateUser() error: %v", err)
	}
	if created.ID == uuid.Nil {
		t.Error("CreateUser() returned a zero-value ID")
	}

	got, err := testStore.GetUserByEmail(ctx, email)
	if err != nil {
		t.Fatalf("GetUserByEmail() error: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("GetUserByEmail() ID = %v, want %v", got.ID, created.ID)
	}
	if got.PasswordHash != "argon2id$fake-hash" {
		t.Errorf("GetUserByEmail() PasswordHash = %q, want the hash CreateUser stored", got.PasswordHash)
	}
}

func TestStore_CreateUser_DuplicateEmailFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	email := uniqueEmail(t)

	if _, err := testStore.CreateUser(ctx, email, "hash-one"); err != nil {
		t.Fatalf("first CreateUser() error: %v", err)
	}

	_, err := testStore.CreateUser(ctx, email, "hash-two")
	if !errors.Is(err, ErrDuplicateEmail) {
		t.Errorf("second CreateUser() with the same email = %v, want ErrDuplicateEmail", err)
	}
}

func TestStore_GetUserByEmail_NotFoundReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	_, err := testStore.GetUserByEmail(context.Background(), uniqueEmail(t))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("GetUserByEmail() for a nonexistent email = %v, want ErrNotFound", err)
	}
}

func TestStore_CreateRefreshToken_GetRefreshTokenByHash_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	user, err := testStore.CreateUser(ctx, uniqueEmail(t), "hash")
	if err != nil {
		t.Fatalf("CreateUser() error: %v", err)
	}

	hash := []byte(uuid.NewString())
	expiresAt := time.Now().Add(time.Hour).Truncate(time.Microsecond)

	created, err := testStore.CreateRefreshToken(ctx, user.ID, hash, expiresAt)
	if err != nil {
		t.Fatalf("CreateRefreshToken() error: %v", err)
	}
	if created.RevokedAt != nil {
		t.Error("CreateRefreshToken() returned a token already marked revoked")
	}

	got, err := testStore.GetRefreshTokenByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetRefreshTokenByHash() error: %v", err)
	}
	if got.ID != created.ID || got.UserID != user.ID {
		t.Errorf("GetRefreshTokenByHash() = %+v, want ID=%v UserID=%v", got, created.ID, user.ID)
	}
}

func TestStore_GetRefreshTokenByHash_NotFoundReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	_, err := testStore.GetRefreshTokenByHash(context.Background(), []byte(uuid.NewString()))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("GetRefreshTokenByHash() for an unknown hash = %v, want ErrNotFound", err)
	}
}

func TestStore_RevokeRefreshToken_MarksRevokedAndIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	user, err := testStore.CreateUser(ctx, uniqueEmail(t), "hash")
	if err != nil {
		t.Fatalf("CreateUser() error: %v", err)
	}
	hash := []byte(uuid.NewString())
	rt, err := testStore.CreateRefreshToken(ctx, user.ID, hash, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateRefreshToken() error: %v", err)
	}

	if err := testStore.RevokeRefreshToken(ctx, rt.ID); err != nil {
		t.Fatalf("first RevokeRefreshToken() error: %v", err)
	}
	got, err := testStore.GetRefreshTokenByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetRefreshTokenByHash() error: %v", err)
	}
	if got.RevokedAt == nil {
		t.Fatal("RevokeRefreshToken() did not set revoked_at")
	}
	firstRevokedAt := *got.RevokedAt

	// Idempotent: revoking an already-revoked token is a no-op, not an
	// error, and does not move revoked_at forward (the query is
	// WHERE revoked_at IS NULL).
	if err := testStore.RevokeRefreshToken(ctx, rt.ID); err != nil {
		t.Fatalf("second RevokeRefreshToken() error: %v", err)
	}
	got, err = testStore.GetRefreshTokenByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetRefreshTokenByHash() error: %v", err)
	}
	if !got.RevokedAt.Equal(firstRevokedAt) {
		t.Errorf("second RevokeRefreshToken() changed revoked_at: %v -> %v", firstRevokedAt, *got.RevokedAt)
	}
}

func TestStore_Close_ReleasesPoolConnections(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	store, err := New(ctx, testDSN, 5)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if err := store.Ping(ctx); err != nil {
		t.Fatalf("Ping() before Close() error: %v", err)
	}

	store.Close()

	if err := store.Ping(ctx); err == nil {
		t.Error("Ping() after Close() succeeded, want an error — the pool did not actually close")
	}
}

func TestStore_New_HonoursDatabaseMaxConns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const wantMaxConns = 3

	store, err := New(ctx, testDSN, wantMaxConns)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	defer store.Close()

	if got := store.pool.Stat().MaxConns(); got != wantMaxConns {
		t.Errorf("pool MaxConns = %d, want %d (DATABASE_MAX_CONNS not honoured)", got, wantMaxConns)
	}
}
