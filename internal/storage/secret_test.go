package storage

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func seedUserForSecrets(t *testing.T) uuid.UUID {
	t.Helper()
	user, err := testStore.CreateUser(context.Background(), uniqueEmail(t), "hash")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return user.ID
}

func TestUpsertSecret_GetSecret_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	userID := seedUserForSecrets(t)

	if err := testStore.UpsertSecret(ctx, userID, "GITHUB_TOKEN", []byte("ciphertext-bytes"), []byte("nonce-bytes")); err != nil {
		t.Fatalf("UpsertSecret() error = %v", err)
	}

	got, err := testStore.GetSecret(ctx, userID, "GITHUB_TOKEN")
	if err != nil {
		t.Fatalf("GetSecret() error = %v", err)
	}
	if string(got.Ciphertext) != "ciphertext-bytes" || string(got.Nonce) != "nonce-bytes" {
		t.Errorf("GetSecret() = %+v, want ciphertext/nonce round-tripped", got)
	}
}

func TestUpsertSecret_SameNameOverwrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	userID := seedUserForSecrets(t)

	if err := testStore.UpsertSecret(ctx, userID, "GITHUB_TOKEN", []byte("old"), []byte("n1")); err != nil {
		t.Fatalf("first UpsertSecret() error = %v", err)
	}
	if err := testStore.UpsertSecret(ctx, userID, "GITHUB_TOKEN", []byte("new"), []byte("n2")); err != nil {
		t.Fatalf("second UpsertSecret() error = %v", err)
	}

	got, err := testStore.GetSecret(ctx, userID, "GITHUB_TOKEN")
	if err != nil {
		t.Fatalf("GetSecret() error = %v", err)
	}
	if string(got.Ciphertext) != "new" {
		t.Errorf("Ciphertext = %q, want %q — a re-submitted secret must replace the old value", got.Ciphertext, "new")
	}
}

func TestGetSecret_MissingReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	userID := seedUserForSecrets(t)

	_, err := testStore.GetSecret(context.Background(), userID, "NO_SUCH_SECRET")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSecret() error = %v, want ErrNotFound", err)
	}
}

func TestListSecretNames_ReturnsOnlyThatUsersNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	userA := seedUserForSecrets(t)
	userB := seedUserForSecrets(t)

	if err := testStore.UpsertSecret(ctx, userA, "GITHUB_TOKEN", []byte("a"), []byte("n")); err != nil {
		t.Fatalf("UpsertSecret() error = %v", err)
	}
	if err := testStore.UpsertSecret(ctx, userB, "OTHER_TOKEN", []byte("b"), []byte("n")); err != nil {
		t.Fatalf("UpsertSecret() error = %v", err)
	}

	names, err := testStore.ListSecretNames(ctx, userA)
	if err != nil {
		t.Fatalf("ListSecretNames() error = %v", err)
	}
	if len(names) != 1 || names[0] != "GITHUB_TOKEN" {
		t.Errorf("ListSecretNames(userA) = %v, want [GITHUB_TOKEN] — must not leak userB's secret names", names)
	}
}

func TestDeleteSecret_RemovesIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	userID := seedUserForSecrets(t)

	if err := testStore.UpsertSecret(ctx, userID, "GITHUB_TOKEN", []byte("a"), []byte("n")); err != nil {
		t.Fatalf("UpsertSecret() error = %v", err)
	}
	if err := testStore.DeleteSecret(ctx, userID, "GITHUB_TOKEN"); err != nil {
		t.Fatalf("DeleteSecret() error = %v", err)
	}

	_, err := testStore.GetSecret(ctx, userID, "GITHUB_TOKEN")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSecret() after delete error = %v, want ErrNotFound", err)
	}
}

func TestDeleteSecret_MissingIsNotAnError(t *testing.T) {
	t.Parallel()
	userID := seedUserForSecrets(t)

	if err := testStore.DeleteSecret(context.Background(), userID, "NO_SUCH_SECRET"); err != nil {
		t.Errorf("DeleteSecret() error = %v, want nil for a name that was never stored", err)
	}
}
