package auth

import (
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestEncodeOAuthState_DecodeOAuthState_RoundTrip(t *testing.T) {
	t.Parallel()
	userID := uuid.New()

	state, err := encodeOAuthState(testSigningKey, userID)
	if err != nil {
		t.Fatalf("encodeOAuthState() error: %v", err)
	}

	got, err := decodeOAuthState(testSigningKey, state)
	if err != nil {
		t.Fatalf("decodeOAuthState() error: %v", err)
	}
	if got != userID {
		t.Errorf("decodeOAuthState() = %s, want %s", got, userID)
	}
}

func TestDecodeOAuthState_TamperedTagFails(t *testing.T) {
	t.Parallel()
	state, err := encodeOAuthState(testSigningKey, uuid.New())
	if err != nil {
		t.Fatalf("encodeOAuthState() error: %v", err)
	}

	tampered := state[:len(state)-1] + flipLastChar(state)
	if _, err := decodeOAuthState(testSigningKey, tampered); err == nil {
		t.Fatal("decodeOAuthState() with a tampered value succeeded, want an error")
	}
}

func flipLastChar(s string) string {
	if s[len(s)-1] == 'a' {
		return "b"
	}
	return "a"
}

func TestDecodeOAuthState_GarbageFails(t *testing.T) {
	t.Parallel()
	if _, err := decodeOAuthState(testSigningKey, "not-a-valid-state-value"); err == nil {
		t.Fatal("decodeOAuthState() with garbage input succeeded, want an error")
	}
}

func TestDecodeOAuthState_WrongKeyFails(t *testing.T) {
	t.Parallel()
	state, err := encodeOAuthState(testSigningKey, uuid.New())
	if err != nil {
		t.Fatalf("encodeOAuthState() error: %v", err)
	}

	wrongKey := []byte("00000000000000000000000000000000"[:secretEncryptionKeyBytes])
	if _, err := decodeOAuthState(wrongKey, state); err == nil {
		t.Fatal("decodeOAuthState() with the wrong signing key succeeded, want an error")
	}
}

// TestDecodeOAuthState_ExpiredFails proves a state past its TTL is
// rejected even with a valid tag — encodeOAuthState always sets a
// future expiry, so this constructs an already-expired one directly to
// exercise the check without a real 10-minute sleep (CLAUDE.md T4).
func TestDecodeOAuthState_ExpiredFails(t *testing.T) {
	t.Parallel()
	userID := uuid.New()

	var buf [stateLen]byte
	idBytes, err := userID.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal user id: %v", err)
	}
	copy(buf[0:16], idBytes)
	expired := time.Now().Add(-time.Minute).Unix()
	for i := range 8 {
		buf[16+i] = byte(expired >> (56 - 8*i))
	}
	tag := hmacTag(testSigningKey, buf[:32])
	copy(buf[32:48], tag)

	_, err = decodeOAuthState(testSigningKey, base64.RawURLEncoding.EncodeToString(buf[:]))
	if !errors.Is(err, ErrOAuthStateExpired) {
		t.Errorf("decodeOAuthState() on an expired state = %v, want ErrOAuthStateExpired", err)
	}
}
