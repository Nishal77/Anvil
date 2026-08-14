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
	defaultRunnerAddr       = "http://127.0.0.1:9090"
	minJWTSigningKeyBytes   = 32
	defaultGeminiModel      = "gemini-2.5-flash"
	defaultMonthlyUSDCap    = 10
	usdMicrosPerUSD         = 1_000_000
)

// Config holds process-wide configuration loaded from the environment.
// Field names and defaults follow PRD Appendix B.
type Config struct {
	HTTPAddr         string
	DatabaseURL      string
	DatabaseMaxConns int32
	RedisAddr        string
	RunnerAddr       string
	JWTSigningKey    []byte
	AccessTokenTTL   time.Duration
	RefreshTokenTTL  time.Duration
	GeminiAPIKey     string
	GeminiModel      string
	AnthropicAPIKey  string
	// MonthlyUSDCapMicros is ANVIL_MONTHLY_USD_CAP (whole USD) converted
	// to micros — FR-034's global spend ceiling.
	MonthlyUSDCapMicros int64
}

// LogValue redacts JWTSigningKey so a careless `slog.Any("config", cfg)`
// can't leak it (CLAUDE.md I-3).
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("http_addr", c.HTTPAddr),
		slog.String("database_url", c.DatabaseURL),
		slog.Int("database_max_conns", int(c.DatabaseMaxConns)),
		slog.String("redis_addr", c.RedisAddr),
		slog.String("runner_addr", c.RunnerAddr),
		slog.String("jwt_signing_key", "[redacted]"),
		slog.Duration("access_token_ttl", c.AccessTokenTTL),
		slog.Duration("refresh_token_ttl", c.RefreshTokenTTL),
		slog.Bool("gemini_configured", c.GeminiAPIKey != ""),
		slog.String("gemini_model", c.GeminiModel),
		slog.Bool("anthropic_configured", c.AnthropicAPIKey != ""),
		slog.Int64("monthly_usd_cap_micros", c.MonthlyUSDCapMicros),
	)
}

// Load reads Config from the environment and validates it, applying
// defaults for optional fields.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:            envOr("ANVIL_HTTP_ADDR", defaultHTTPAddr),
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		DatabaseMaxConns:    defaultDatabaseMaxConns,
		RedisAddr:           envOr("REDIS_URL", ""),
		RunnerAddr:          envOr("ANVIL_RUNNER_URL", defaultRunnerAddr),
		JWTSigningKey:       []byte(os.Getenv("ANVIL_JWT_SECRET")),
		AccessTokenTTL:      defaultAccessTokenTTL,
		RefreshTokenTTL:     defaultRefreshTokenTTL,
		GeminiAPIKey:        os.Getenv("ANVIL_GEMINI_API_KEY"),
		GeminiModel:         envOr("ANVIL_GEMINI_MODEL", defaultGeminiModel),
		AnthropicAPIKey:     os.Getenv("ANVIL_ANTHROPIC_API_KEY"),
		MonthlyUSDCapMicros: defaultMonthlyUSDCap * usdMicrosPerUSD,
	}

	if v := os.Getenv("DATABASE_MAX_CONNS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return Config{}, fmt.Errorf("config: parse DATABASE_MAX_CONNS: %w", err)
		}
		cfg.DatabaseMaxConns = int32(n)
	}

	if v := os.Getenv("ANVIL_MONTHLY_USD_CAP"); v != "" {
		usd, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return Config{}, fmt.Errorf("config: parse ANVIL_MONTHLY_USD_CAP: %w", err)
		}
		cfg.MonthlyUSDCapMicros = usd * usdMicrosPerUSD
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
	if c.GeminiAPIKey == "" && c.AnthropicAPIKey == "" {
		return fmt.Errorf("config: set ANVIL_GEMINI_API_KEY and/or ANVIL_ANTHROPIC_API_KEY")
	}
	return nil
}

// BenchConfig holds cmd/anvilctl's "bench" subcommand configuration
// (PRD §20.5). Deliberately separate from Config: the benchmark
// harness has no Postgres, Redis, or JWT dependency, so it must not
// share Config.validate's requirement that those be set.
type BenchConfig struct {
	RunnerAddr      string
	GeminiAPIKey    string
	GeminiModel     string
	AnthropicAPIKey string
}

// LogValue redacts both API keys (CLAUDE.md I-3).
func (c BenchConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("runner_addr", c.RunnerAddr),
		slog.Bool("gemini_configured", c.GeminiAPIKey != ""),
		slog.String("gemini_model", c.GeminiModel),
		slog.Bool("anthropic_configured", c.AnthropicAPIKey != ""),
	)
}

// LoadBench reads BenchConfig from the environment. At least one
// provider key is required — the benchmark harness has nothing to
// call otherwise.
func LoadBench() (BenchConfig, error) {
	cfg := BenchConfig{
		RunnerAddr:      envOr("ANVIL_RUNNER_URL", defaultRunnerAddr),
		GeminiAPIKey:    os.Getenv("ANVIL_GEMINI_API_KEY"),
		GeminiModel:     envOr("ANVIL_GEMINI_MODEL", defaultGeminiModel),
		AnthropicAPIKey: os.Getenv("ANVIL_ANTHROPIC_API_KEY"),
	}
	if cfg.GeminiAPIKey == "" && cfg.AnthropicAPIKey == "" {
		return BenchConfig{}, fmt.Errorf("config: set ANVIL_GEMINI_API_KEY and/or ANVIL_ANTHROPIC_API_KEY")
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
