package auth

import "errors"

// ErrInvalidCredentials indicates a login attempt failed. It is
// deliberately returned for both "no such user" and "wrong password" —
// SEC-030 forbids distinguishing the two, which would let an attacker
// enumerate registered emails.
var ErrInvalidCredentials = errors.New("invalid credentials")

// ErrRefreshTokenExpired indicates the presented refresh token is past its
// TTL.
var ErrRefreshTokenExpired = errors.New("refresh token expired")

// ErrRefreshTokenRevoked indicates the presented refresh token was already
// used or explicitly revoked. Callers must treat this as a signal the
// token may have been stolen, not merely retry.
var ErrRefreshTokenRevoked = errors.New("refresh token revoked")
