package auth

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

var testSigningKey = []byte("01234567890123456789012345678901")

func TestIssueAccessToken_VerifyAccessToken_RoundTrips(t *testing.T) {
	t.Parallel()
	userID := uuid.New()

	token, expiresAt, err := issueAccessToken(testSigningKey, userID, 15*time.Minute)
	if err != nil {
		t.Fatalf("issueAccessToken() error: %v", err)
	}
	if expiresAt.Before(time.Now()) {
		t.Fatal("expiresAt is in the past")
	}

	gotUserID, err := verifyAccessToken(testSigningKey, token)
	if err != nil {
		t.Fatalf("verifyAccessToken() error: %v", err)
	}
	if gotUserID != userID {
		t.Errorf("verifyAccessToken() = %v, want %v", gotUserID, userID)
	}
}

func TestVerifyAccessToken_ExpiredTokenFails(t *testing.T) {
	t.Parallel()
	userID := uuid.New()
	token, _, err := issueAccessToken(testSigningKey, userID, -1*time.Minute)
	if err != nil {
		t.Fatalf("issueAccessToken() error: %v", err)
	}

	_, err = verifyAccessToken(testSigningKey, token)
	if err == nil {
		t.Fatal("verifyAccessToken() succeeded for an expired token")
	}
}

func TestVerifyAccessToken_WrongKeyFails(t *testing.T) {
	t.Parallel()
	userID := uuid.New()
	token, _, err := issueAccessToken(testSigningKey, userID, 15*time.Minute)
	if err != nil {
		t.Fatalf("issueAccessToken() error: %v", err)
	}

	_, err = verifyAccessToken([]byte("different-signing-key-32-bytes!"), token)
	if err == nil {
		t.Fatal("verifyAccessToken() succeeded with the wrong signing key")
	}
}
