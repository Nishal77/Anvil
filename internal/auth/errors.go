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

// ErrSecretNotFound indicates the requested secret name has no stored
// value for that user.
var ErrSecretNotFound = errors.New("secret not found")

// ErrInvalidOAuthState indicates a GitHub OAuth callback's state
// parameter failed to verify — forged, corrupted, or simply not one
// this server issued.
var ErrInvalidOAuthState = errors.New("invalid oauth state")

// ErrOAuthStateExpired indicates a GitHub OAuth callback arrived after
// its state's TTL — the user took too long on GitHub's consent screen,
// or the state is being replayed.
var ErrOAuthStateExpired = errors.New("oauth state expired")

// ErrGitHubNotConfigured indicates the server has no GitHub OAuth app
// credentials configured — BeginGitHubOAuth and CompleteGitHubOAuth are
// unusable until an operator sets ANVIL_GITHUB_CLIENT_ID and friends.
var ErrGitHubNotConfigured = errors.New("github oauth is not configured")

// ErrGitHubAccountAlreadyLinked indicates the GitHub account the user
// just authorized is already linked to a different Anvil user.
var ErrGitHubAccountAlreadyLinked = errors.New("github account already linked to a different user")
