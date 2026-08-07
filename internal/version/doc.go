// Package version holds build-time metadata stamped by the Makefile's
// LDFLAGS. Its variables are never set by code — only by
// -ldflags "-X .../internal/version.Version=..." at build time.
package version
