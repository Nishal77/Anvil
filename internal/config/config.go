package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"
)

const (
	defaultHTTPAddr          = ":8080"
	defaultDatabaseMaxConns  = 10
	defaultAccessTokenTTL    = 15 * time.Minute
	defaultRefreshTokenTTL   = 7 * 24 * time.Hour
	defaultRunnerAddr        = "http://127.0.0.1:9090"
	minJWTSigningKeyBytes    = 32
	secretEncryptionKeyBytes = 32
	defaultGeminiModel       = "gemini-2.5-flash"
	defaultOpenAIModel       = "gpt-4o-mini"
	defaultMonthlyUSDCap     = 10
	usdMicrosPerUSD          = 1_000_000
	defaultMaxSteps          = 12
	defaultCaddyAdminAddr    = "http://127.0.0.1:2019"
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
	// SecretEncryptionKey seals user secrets (PRD §16.5) — AES-256-GCM,
	// exactly 32 bytes.
	SecretEncryptionKey []byte
	// GitHub OAuth app (FR-001). All optional together: unset means
	// GitHub linking is unavailable rather than a startup failure — see
	// auth.Config's own validation for the "all or nothing" rule across
	// these three.
	GitHubClientID     string
	GitHubClientSecret string
	GitHubRedirectURL  string
	GitHubWebURL       string
	// GeminiAPIKey is optional — Gemini is not required when Anthropic
	// and/or OpenAI keys are set (see validate).
	GeminiAPIKey    string
	GeminiModel     string
	AnthropicAPIKey string
	OpenAIAPIKey    string
	OpenAIModel     string
	// MonthlyUSDCapMicros is ANVIL_MONTHLY_USD_CAP (whole USD) converted
	// to micros — FR-034's global spend ceiling.
	MonthlyUSDCapMicros int64
	// MaxSteps is ANVIL_MAX_STEPS — the Planner's code-enforced ceiling
	// on plan size (PRD §12.1), never merely requested in the prompt.
	MaxSteps int
	// Artifact storage (PRD §8.2). Endpoint is host:port, no scheme —
	// all optional: an unset Endpoint means artifact upload/download is
	// skipped entirely rather than failing startup, so a deployment
	// without object storage configured still runs everything else.
	S3Endpoint  string
	S3Bucket    string
	S3AccessKey string
	S3SecretKey string
	S3UseSSL    bool
	// Preview deployments (PRD §9.7, FR-060..FR-063). PreviewDomain
	// unset means deploy is unavailable rather than a startup failure,
	// the same pattern as S3Endpoint above — a job that requests
	// options.deploy on a deployment with no preview domain configured
	// fails that job at deploy time (agent.Executor's deployPreview),
	// not the whole control plane at startup.
	PreviewDomain  string
	CaddyAdminAddr string
	// OTelCollectorEndpoint is the OTel Collector's OTLP/gRPC address
	// (PRD §17.1). Empty disables tracing entirely — see
	// telemetry.NewTracerProvider's own doc comment for why that is a
	// safe default rather than a startup failure.
	OTelCollectorEndpoint string
}

// LogValue redacts JWTSigningKey and every API key so a careless
// `slog.Any("config", cfg)` can't leak them (CLAUDE.md I-3).
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("http_addr", c.HTTPAddr),
		slog.String("database_url", c.DatabaseURL),
		slog.Int("database_max_conns", int(c.DatabaseMaxConns)),
		slog.String("redis_addr", c.RedisAddr),
		slog.String("runner_addr", c.RunnerAddr),
		slog.String("jwt_signing_key", "[redacted]"),
		slog.String("secret_encryption_key", "[redacted]"),
		slog.Duration("access_token_ttl", c.AccessTokenTTL),
		slog.Duration("refresh_token_ttl", c.RefreshTokenTTL),
		slog.Bool("gemini_configured", c.GeminiAPIKey != ""),
		slog.String("gemini_model", c.GeminiModel),
		slog.Bool("anthropic_configured", c.AnthropicAPIKey != ""),
		slog.Bool("openai_configured", c.OpenAIAPIKey != ""),
		slog.String("openai_model", c.OpenAIModel),
		slog.Int64("monthly_usd_cap_micros", c.MonthlyUSDCapMicros),
		slog.Int("max_steps", c.MaxSteps),
		slog.Bool("s3_configured", c.S3Endpoint != ""),
		slog.String("s3_bucket", c.S3Bucket),
		slog.Bool("preview_deploy_configured", c.PreviewDomain != ""),
		slog.String("preview_domain", c.PreviewDomain),
		slog.Bool("tracing_enabled", c.OTelCollectorEndpoint != ""),
	)
}

// Load reads Config from the environment and validates it, applying
// defaults for optional fields.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:              envOr("ANVIL_HTTP_ADDR", defaultHTTPAddr),
		DatabaseURL:           os.Getenv("DATABASE_URL"),
		DatabaseMaxConns:      defaultDatabaseMaxConns,
		RedisAddr:             envOr("REDIS_URL", ""),
		RunnerAddr:            envOr("ANVIL_RUNNER_URL", defaultRunnerAddr),
		JWTSigningKey:         []byte(os.Getenv("ANVIL_JWT_SECRET")),
		AccessTokenTTL:        defaultAccessTokenTTL,
		RefreshTokenTTL:       defaultRefreshTokenTTL,
		SecretEncryptionKey:   []byte(os.Getenv("ANVIL_SECRET_ENCRYPTION_KEY")),
		GitHubClientID:        os.Getenv("ANVIL_GITHUB_CLIENT_ID"),
		GitHubClientSecret:    os.Getenv("ANVIL_GITHUB_CLIENT_SECRET"),
		GitHubRedirectURL:     os.Getenv("ANVIL_GITHUB_REDIRECT_URL"),
		GitHubWebURL:          os.Getenv("ANVIL_WEB_URL"),
		GeminiAPIKey:          os.Getenv("ANVIL_GEMINI_API_KEY"),
		GeminiModel:           envOr("ANVIL_GEMINI_MODEL", defaultGeminiModel),
		AnthropicAPIKey:       os.Getenv("ANVIL_ANTHROPIC_API_KEY"),
		OpenAIAPIKey:          os.Getenv("ANVIL_OPENAI_API_KEY"),
		OpenAIModel:           envOr("ANVIL_OPENAI_MODEL", defaultOpenAIModel),
		MonthlyUSDCapMicros:   defaultMonthlyUSDCap * usdMicrosPerUSD,
		MaxSteps:              defaultMaxSteps,
		S3Endpoint:            os.Getenv("ANVIL_S3_ENDPOINT"),
		S3Bucket:              envOr("ANVIL_S3_BUCKET", "anvil-artifacts"),
		S3AccessKey:           os.Getenv("ANVIL_S3_ACCESS_KEY"),
		S3SecretKey:           os.Getenv("ANVIL_S3_SECRET_KEY"),
		S3UseSSL:              os.Getenv("ANVIL_S3_USE_SSL") == "true",
		PreviewDomain:         os.Getenv("ANVIL_PREVIEW_DOMAIN"),
		CaddyAdminAddr:        envOr("ANVIL_CADDY_ADMIN_ADDR", defaultCaddyAdminAddr),
		OTelCollectorEndpoint: os.Getenv("ANVIL_OTEL_COLLECTOR_ENDPOINT"),
	}

	if v := os.Getenv("ANVIL_MAX_STEPS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("config: parse ANVIL_MAX_STEPS: %w", err)
		}
		cfg.MaxSteps = n
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
	if len(c.SecretEncryptionKey) != secretEncryptionKeyBytes {
		return fmt.Errorf("config: ANVIL_SECRET_ENCRYPTION_KEY must be exactly %d bytes, got %d", secretEncryptionKeyBytes, len(c.SecretEncryptionKey))
	}
	if c.DatabaseMaxConns <= 0 {
		return fmt.Errorf("config: DATABASE_MAX_CONNS must be positive, got %d", c.DatabaseMaxConns)
	}
	if c.GeminiAPIKey == "" && c.AnthropicAPIKey == "" && c.OpenAIAPIKey == "" {
		return fmt.Errorf("config: set at least one of ANVIL_GEMINI_API_KEY, ANVIL_ANTHROPIC_API_KEY, ANVIL_OPENAI_API_KEY")
	}
	return nil
}

// BenchConfig holds cmd/anvilctl's "bench" subcommand configuration
// (PRD §20.5). Deliberately separate from Config: the benchmark
// harness drives the real Planner+Executor pipeline (Postgres, object
// storage for artifact verification) but has no Redis, HTTP, or JWT
// dependency, so it must not share Config.validate's requirement that
// those be set.
type BenchConfig struct {
	DatabaseURL      string
	DatabaseMaxConns int32
	RunnerAddr       string
	GeminiAPIKey     string
	GeminiModel      string
	AnthropicAPIKey  string
	OpenAIAPIKey     string
	OpenAIModel      string
	MaxSteps         int
	// Object storage: required, not optional, unlike Config's — a
	// benchmark run that can't download the artifact can't verify a
	// task's check command, which makes the whole run meaningless.
	S3Endpoint  string
	S3Bucket    string
	S3AccessKey string
	S3SecretKey string
	S3UseSSL    bool
}

// LogValue redacts every API key (CLAUDE.md I-3).
func (c BenchConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("database_url", c.DatabaseURL),
		slog.String("runner_addr", c.RunnerAddr),
		slog.Bool("gemini_configured", c.GeminiAPIKey != ""),
		slog.String("gemini_model", c.GeminiModel),
		slog.Bool("anthropic_configured", c.AnthropicAPIKey != ""),
		slog.Bool("openai_configured", c.OpenAIAPIKey != ""),
		slog.String("openai_model", c.OpenAIModel),
		slog.Bool("s3_configured", c.S3Endpoint != ""),
		slog.String("s3_bucket", c.S3Bucket),
	)
}

// LoadBench reads BenchConfig from the environment. At least one
// provider key is required — the benchmark harness has nothing to
// call otherwise.
func LoadBench() (BenchConfig, error) {
	cfg := BenchConfig{
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		DatabaseMaxConns: defaultDatabaseMaxConns,
		RunnerAddr:       envOr("ANVIL_RUNNER_URL", defaultRunnerAddr),
		GeminiAPIKey:     os.Getenv("ANVIL_GEMINI_API_KEY"),
		GeminiModel:      envOr("ANVIL_GEMINI_MODEL", defaultGeminiModel),
		AnthropicAPIKey:  os.Getenv("ANVIL_ANTHROPIC_API_KEY"),
		OpenAIAPIKey:     os.Getenv("ANVIL_OPENAI_API_KEY"),
		OpenAIModel:      envOr("ANVIL_OPENAI_MODEL", defaultOpenAIModel),
		MaxSteps:         defaultMaxSteps,
		S3Endpoint:       os.Getenv("ANVIL_S3_ENDPOINT"),
		S3Bucket:         envOr("ANVIL_S3_BUCKET", "anvil-artifacts"),
		S3AccessKey:      os.Getenv("ANVIL_S3_ACCESS_KEY"),
		S3SecretKey:      os.Getenv("ANVIL_S3_SECRET_KEY"),
		S3UseSSL:         os.Getenv("ANVIL_S3_USE_SSL") == "true",
	}
	if cfg.GeminiAPIKey == "" && cfg.AnthropicAPIKey == "" && cfg.OpenAIAPIKey == "" {
		return BenchConfig{}, fmt.Errorf("config: set at least one of ANVIL_GEMINI_API_KEY, ANVIL_ANTHROPIC_API_KEY, ANVIL_OPENAI_API_KEY")
	}
	if cfg.DatabaseURL == "" {
		return BenchConfig{}, fmt.Errorf("config: DATABASE_URL is required")
	}
	if cfg.S3Endpoint == "" {
		return BenchConfig{}, fmt.Errorf("config: ANVIL_S3_ENDPOINT is required — the benchmark harness verifies a task by downloading its artifact")
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
