package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

const refreshTokenBytes = 32

// newRefreshToken returns a random opaque refresh token and its SHA-256
// hash. Only the hash is ever persisted (PRD §10 — refresh_tokens.token_hash
// "SHA-256, never the raw token").
func newRefreshToken() (token string, hash []byte, err error) {
	raw := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("auth: generate refresh token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	return token, sum[:], nil
}

// hashRefreshToken returns the SHA-256 hash of a presented refresh token, so
// it can be looked up against the stored hash.
func hashRefreshToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
