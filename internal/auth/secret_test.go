package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestPutSecret_ResolveSecret_RoundTrip(t *testing.T) {
	t.Parallel()
	svc := newTestService(t, newFakeStore())
	userID := uuid.New()

	if err := svc.PutSecret(context.Background(), userID, "GITHUB_TOKEN", "ghp_supersecret"); err != nil {
		t.Fatalf("PutSecret() error = %v", err)
	}

	got, err := svc.ResolveSecret(context.Background(), userID, "GITHUB_TOKEN")
	if err != nil {
		t.Fatalf("ResolveSecret() error = %v", err)
	}
	if got != "ghp_supersecret" {
		t.Errorf("ResolveSecret() = %q, want %q", got, "ghp_supersecret")
	}
}

// TestPutSecret_StoresCiphertextNotPlaintext proves the plaintext never
// reaches storage — PRD §16.5's whole premise is that a database dump
// alone yields nothing usable without the separate encryption key.
func TestPutSecret_StoresCiphertextNotPlaintext(t *testing.T) {
	t.Parallel()
	fs := newFakeStore()
	svc := newTestService(t, fs)
	userID := uuid.New()

	if err := svc.PutSecret(context.Background(), userID, "GITHUB_TOKEN", "ghp_supersecret"); err != nil {
		t.Fatalf("PutSecret() error = %v", err)
	}

	stored := fs.secrets[secretKey(userID, "GITHUB_TOKEN")]
	if string(stored.Ciphertext) == "ghp_supersecret" {
		t.Fatal("stored ciphertext equals the plaintext — secret was not encrypted")
	}
}

func TestResolveSecret_MissingReturnsErrSecretNotFound(t *testing.T) {
	t.Parallel()
	svc := newTestService(t, newFakeStore())

	_, err := svc.ResolveSecret(context.Background(), uuid.New(), "NO_SUCH_SECRET")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("ResolveSecret() error = %v, want ErrSecretNotFound", err)
	}
}

func TestListSecretNames_PassesThroughStore(t *testing.T) {
	t.Parallel()
	svc := newTestService(t, newFakeStore())
	userID := uuid.New()

	if err := svc.PutSecret(context.Background(), userID, "GITHUB_TOKEN", "v"); err != nil {
		t.Fatalf("PutSecret() error = %v", err)
	}

	names, err := svc.ListSecretNames(context.Background(), userID)
	if err != nil {
		t.Fatalf("ListSecretNames() error = %v", err)
	}
	if len(names) != 1 || names[0] != "GITHUB_TOKEN" {
		t.Errorf("ListSecretNames() = %v, want [GITHUB_TOKEN]", names)
	}
}

func TestDeleteSecret_RemovesIt(t *testing.T) {
	t.Parallel()
	svc := newTestService(t, newFakeStore())
	userID := uuid.New()

	if err := svc.PutSecret(context.Background(), userID, "GITHUB_TOKEN", "v"); err != nil {
		t.Fatalf("PutSecret() error = %v", err)
	}
	if err := svc.DeleteSecret(context.Background(), userID, "GITHUB_TOKEN"); err != nil {
		t.Fatalf("DeleteSecret() error = %v", err)
	}

	_, err := svc.ResolveSecret(context.Background(), userID, "GITHUB_TOKEN")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("ResolveSecret() after delete error = %v, want ErrSecretNotFound", err)
	}
}

func TestEncryptSecret_DecryptSecret_RoundTrip(t *testing.T) {
	t.Parallel()
	ciphertext, nonce, err := encryptSecret(testEncryptionKey, []byte("plaintext"))
	if err != nil {
		t.Fatalf("encryptSecret() error = %v", err)
	}
	if string(ciphertext) == "plaintext" {
		t.Fatal("ciphertext equals plaintext — not encrypted")
	}

	got, err := decryptSecret(testEncryptionKey, ciphertext, nonce)
	if err != nil {
		t.Fatalf("decryptSecret() error = %v", err)
	}
	if string(got) != "plaintext" {
		t.Errorf("decryptSecret() = %q, want %q", got, "plaintext")
	}
}

// TestDecryptSecret_WrongKeyFails proves GCM's authentication catches a
// mismatched key rather than silently returning garbage — a plain CBC
// mode would decrypt anything into wrong-but-plausible-looking bytes.
func TestDecryptSecret_WrongKeyFails(t *testing.T) {
	t.Parallel()
	ciphertext, nonce, err := encryptSecret(testEncryptionKey, []byte("plaintext"))
	if err != nil {
		t.Fatalf("encryptSecret() error = %v", err)
	}

	wrongKey := []byte("00000000000000000000000000000000"[:secretEncryptionKeyBytes])
	if _, err := decryptSecret(wrongKey, ciphertext, nonce); err == nil {
		t.Fatal("decryptSecret() with the wrong key succeeded, want an error")
	}
}
