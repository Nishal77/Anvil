package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// oauthStateTTL bounds how long a GitHub OAuth "state" value is valid
// for — the user is expected to complete the GitHub consent screen and
// be redirected back well within this window.
const oauthStateTTL = 10 * time.Minute

// stateLen is the fixed wire size of an encoded state value: a 16-byte
// user ID, an 8-byte expiry, an 8-byte nonce, and a 16-byte truncated
// HMAC-SHA256 tag.
const stateLen = 16 + 8 + 8 + 16

// encodeOAuthState returns an opaque, tamper-evident value binding
// userID to this OAuth attempt. It is deliberately not a JWT sharing
// accessClaims' shape: an access token and an OAuth state serve
// different purposes, and keeping their wire formats distinct means a
// token of one kind can never be mistaken for the other, even if a
// caller forgets to check which verify function it passed through.
func encodeOAuthState(signingKey []byte, userID uuid.UUID) (string, error) {
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("auth: generate oauth state nonce: %w", err)
	}

	var buf [stateLen]byte
	idBytes, err := userID.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("auth: marshal user id: %w", err)
	}
	copy(buf[0:16], idBytes)
	binary.BigEndian.PutUint64(buf[16:24], uint64(time.Now().Add(oauthStateTTL).Unix()))
	copy(buf[24:32], nonce)

	tag := hmacTag(signingKey, buf[:32])
	copy(buf[32:48], tag)

	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// decodeOAuthState reverses encodeOAuthState, returning the bound user
// ID iff the HMAC tag verifies and the value has not expired.
func decodeOAuthState(signingKey []byte, state string) (uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(state)
	if err != nil || len(raw) != stateLen {
		return uuid.Nil, ErrInvalidOAuthState
	}

	wantTag := hmacTag(signingKey, raw[:32])
	if subtle.ConstantTimeCompare(wantTag, raw[32:48]) != 1 {
		return uuid.Nil, ErrInvalidOAuthState
	}

	expiresAt := int64(binary.BigEndian.Uint64(raw[16:24]))
	if time.Now().Unix() > expiresAt {
		return uuid.Nil, ErrOAuthStateExpired
	}

	var userID uuid.UUID
	if err := userID.UnmarshalBinary(raw[0:16]); err != nil {
		return uuid.Nil, ErrInvalidOAuthState
	}
	return userID, nil
}

func hmacTag(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)[:16]
}
