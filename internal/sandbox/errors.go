package sandbox

import "errors"

// ErrSandboxNotFound indicates the Runner has no record of the given
// sandbox ID — already destroyed, or never created.
var ErrSandboxNotFound = errors.New("sandbox not found")

// ErrCommandTimeout indicates a command ran longer than its allowed
// timeout and was killed.
var ErrCommandTimeout = errors.New("command timed out")
