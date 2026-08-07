# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Repository scaffolding: directory layout, `internal/version`, `internal/config`,
  Docker Compose stack, licensing and community files.
- `internal/auth`: registration, login, JWT access tokens (HS256), rotating
  hashed refresh tokens, Argon2id password hashing.
- `internal/api`: `/auth/register`, `/auth/login`, `/auth/refresh`,
  `/auth/logout`, `/healthz`, `/readyz`.
- `internal/storage`: Postgres access layer for `users` and `refresh_tokens`.
