package config

import (
	"strings"
	"testing"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://localhost/anvil")
	t.Setenv("REDIS_URL", "redis://localhost:6379")
	t.Setenv("ANVIL_JWT_SECRET", "01234567890123456789012345678901")
}

func TestConfig_Load_MissingDatabaseURLFails(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DATABASE_URL", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() succeeded, want error for missing DATABASE_URL")
	}
}

func TestConfig_Load_ShortJWTSecretFails(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("ANVIL_JWT_SECRET", "too-short")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() succeeded, want error for short ANVIL_JWT_SECRET")
	}
}

func TestConfig_Load_AppliesDefaults(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.HTTPAddr != defaultHTTPAddr {
		t.Errorf("HTTPAddr = %q, want %q", cfg.HTTPAddr, defaultHTTPAddr)
	}
	if cfg.AccessTokenTTL != defaultAccessTokenTTL {
		t.Errorf("AccessTokenTTL = %v, want %v", cfg.AccessTokenTTL, defaultAccessTokenTTL)
	}
	if cfg.DatabaseMaxConns != defaultDatabaseMaxConns {
		t.Errorf("DatabaseMaxConns = %d, want %d", cfg.DatabaseMaxConns, defaultDatabaseMaxConns)
	}
}

func TestConfig_LogValue_RedactsJWTSigningKey(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	rendered := cfg.LogValue().String()
	if strings.Contains(rendered, string(cfg.JWTSigningKey)) {
		t.Fatal("LogValue() leaked the raw JWT signing key")
	}
}
