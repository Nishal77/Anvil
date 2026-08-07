package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"
)

const (
	defaultHTTPAddr         = ":8080"
	defaultDatabaseMaxConns = 10
	defaultAccessTokenTTL   = 15 * time.Minute
	defaultRefreshTokenTTL  = 7 * 24 * time.Hour
	minJWTSigningKeyBytes   = 32
)

// Config holds process-wide configuration loaded from the environment.
// Field names and defaults follow PRD Appendix B.
type Config struct {
	HTTPAddr         string
	DatabaseURL      string
	DatabaseMaxConns int32
	RedisAddr        string
	JWTSigningKey    []byte
	AccessTokenTTL   time.Duration
	RefreshTokenTTL  time.Duration
}

// LogValue redacts JWTSigningKey so a careless `slog.Any("config", cfg)`
// can't leak it (CLAUDE.md I-3).
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("http_addr", c.HTTPAddr),
		slog.String("database_url", c.DatabaseURL),
		slog.Int("database_max_conns", int(c.DatabaseMaxConns)),
		slog.String("redis_addr", c.RedisAddr),
		slog.String("jwt_signing_key", "[redacted]"),
		slog.Duration("access_token_ttl", c.AccessTokenTTL),
		slog.Duration("refresh_token_ttl", c.RefreshTokenTTL),
	)
}

// Load reads Config from the environment and validates it, applying
// defaults for optional fields.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:         envOr("ANVIL_HTTP_ADDR", defaultHTTPAddr),
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		DatabaseMaxConns: defaultDatabaseMaxConns,
		RedisAddr:        envOr("REDIS_URL", ""),
		JWTSigningKey:    []byte(os.Getenv("ANVIL_JWT_SECRET")),
		AccessTokenTTL:   defaultAccessTokenTTL,
		RefreshTokenTTL:  defaultRefreshTokenTTL,
	}

	if v := os.Getenv("DATABASE_MAX_CONNS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return Config{}, fmt.Errorf("config: parse DATABASE_MAX_CONNS: %w", err)
		}
		cfg.DatabaseMaxConns = int32(n)
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("config: DATABASE_URL is required")
	}
	if c.RedisAddr == "" {
		return fmt.Errorf("config: REDIS_URL is required")
	}
	if len(c.JWTSigningKey) < minJWTSigningKeyBytes {
		return fmt.Errorf("config: ANVIL_JWT_SECRET must be at least %d bytes, got %d", minJWTSigningKeyBytes, len(c.JWTSigningKey))
	}
	if c.DatabaseMaxConns <= 0 {
		return fmt.Errorf("config: DATABASE_MAX_CONNS must be positive, got %d", c.DatabaseMaxConns)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
