package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// accessClaims is the JWT payload for an access token. Signed HS256 with a
// single symmetric key (see specs/phase-1-skeleton/week-01-foundations.md
// Open Questions — no key rotation in v1).
type accessClaims struct {
	jwt.RegisteredClaims
}

// issueAccessToken returns a signed JWT for userID, valid for ttl.
func issueAccessToken(signingKey []byte, userID uuid.UUID, ttl time.Duration) (string, time.Time, error) {
	now := time.Now()
	expiresAt := now.Add(ttl)

	claims := accessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(signingKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: sign access token: %w", err)
	}
	return signed, expiresAt, nil
}

// verifyAccessToken parses and validates tokenString, returning the
// subject user ID.
func verifyAccessToken(signingKey []byte, tokenString string) (uuid.UUID, error) {
	token, err := jwt.ParseWithClaims(tokenString, &accessClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("auth: unexpected signing method %v", t.Header["alg"])
		}
		return signingKey, nil
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("auth: parse access token: %w", err)
	}

	claims, ok := token.Claims.(*accessClaims)
	if !ok || !token.Valid {
		return uuid.Nil, errors.New("auth: invalid access token")
	}

	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return uuid.Nil, fmt.Errorf("auth: parse subject as uuid: %w", err)
	}
	return userID, nil
}
