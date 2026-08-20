package storage

import "errors"

// ErrNotFound indicates the requested row does not exist.
var ErrNotFound = errors.New("not found")

// ErrDuplicateEmail indicates a CreateUser call used an email already
// registered to another user.
var ErrDuplicateEmail = errors.New("email already registered")

// ErrDuplicateGitHubID indicates a SetGitHubIdentity call used a
// GitHub account already linked to a different user.
var ErrDuplicateGitHubID = errors.New("github account already linked")
