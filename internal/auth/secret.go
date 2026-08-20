package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/storage"
)

// secretEncryptionKeyBytes is AES-256's fixed key length — unlike the
// JWT signing key (any length works, more is better), AES-256-GCM
// requires exactly 32 bytes or key setup fails outright.
const secretEncryptionKeyBytes = 32

// PutSecret encrypts plaintext with the server's envelope key and
// stores it under (userID, name), replacing any existing value for
// that name (PRD §16.5). plaintext never touches storage or a log
// line — only the AES-256-GCM ciphertext and its nonce do.
func (s *Service) PutSecret(ctx context.Context, userID uuid.UUID, name, plaintext string) error {
	ciphertext, nonce, err := encryptSecret(s.encryptionKey, []byte(plaintext))
	if err != nil {
		return fmt.Errorf("auth: put secret: %w", err)
	}
	if err := s.store.UpsertSecret(ctx, userID, name, ciphertext, nonce); err != nil {
		return fmt.Errorf("auth: put secret: %w", err)
	}
	return nil
}

// ListSecretNames returns the names of every secret userID has stored.
// There is no equivalent that returns values — PRD §16.5: "There is no
// read-back endpoint. Ever."
func (s *Service) ListSecretNames(ctx context.Context, userID uuid.UUID) ([]string, error) {
	names, err := s.store.ListSecretNames(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("auth: list secret names: %w", err)
	}
	return names, nil
}

// DeleteSecret removes userID's secret named name.
func (s *Service) DeleteSecret(ctx context.Context, userID uuid.UUID, name string) error {
	if err := s.store.DeleteSecret(ctx, userID, name); err != nil {
		return fmt.Errorf("auth: delete secret: %w", err)
	}
	return nil
}

// ResolveSecret decrypts and returns userID's secret named name.
// Deliberately not reachable from any API route (PRD §16.5's "no
// read-back endpoint" is absolute) — this exists for the Runner's
// credential-injection path (SEC-020), which needs a git credential
// helper to hand a real token to a single command invocation.
func (s *Service) ResolveSecret(ctx context.Context, userID uuid.UUID, name string) (string, error) {
	sec, err := s.store.GetSecret(ctx, userID, name)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "", fmt.Errorf("auth: resolve secret %q: %w", name, ErrSecretNotFound)
		}
		return "", fmt.Errorf("auth: resolve secret: %w", err)
	}

	plaintext, err := decryptSecret(s.encryptionKey, sec.Ciphertext, sec.Nonce)
	if err != nil {
		return "", fmt.Errorf("auth: resolve secret: %w", err)
	}
	return string(plaintext), nil
}

// encryptSecret returns plaintext sealed under key with AES-256-GCM,
// and the random nonce used to seal it (GCM requires a fresh nonce per
// encryption, so it must be stored alongside the ciphertext to decrypt
// later).
func encryptSecret(key, plaintext []byte) (ciphertext, nonce []byte, err error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}

	nonce = make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("generate nonce: %w", err)
	}

	return gcm.Seal(nil, nonce, plaintext, nil), nonce, nil
}

// decryptSecret reverses encryptSecret.
func decryptSecret(key, ciphertext, nonce []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("construct AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("construct GCM mode: %w", err)
	}
	return gcm, nil
}
