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
	t.Setenv("ANVIL_ANTHROPIC_API_KEY", "test-key-not-real")
}

func TestConfig_Load_MissingLLMProviderKeyFails(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("ANVIL_ANTHROPIC_API_KEY", "")
	t.Setenv("ANVIL_GEMINI_API_KEY", "")
	t.Setenv("ANVIL_OPENAI_API_KEY", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() succeeded, want error when none of the LLM provider keys is set")
	}
}

func TestConfig_Load_OpenAIKeyAloneSatisfiesRequirement(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("ANVIL_ANTHROPIC_API_KEY", "")
	t.Setenv("ANVIL_GEMINI_API_KEY", "")
	t.Setenv("ANVIL_OPENAI_API_KEY", "test-key-not-real")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v, want success with only ANVIL_OPENAI_API_KEY set", err)
	}
	if cfg.OpenAIModel != defaultOpenAIModel {
		t.Errorf("OpenAIModel = %q, want %q", cfg.OpenAIModel, defaultOpenAIModel)
	}
}

func TestConfig_Load_MonthlyUSDCapDefaultsAndParses(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.MonthlyUSDCapMicros != defaultMonthlyUSDCap*usdMicrosPerUSD {
		t.Errorf("MonthlyUSDCapMicros = %d, want %d", cfg.MonthlyUSDCapMicros, defaultMonthlyUSDCap*usdMicrosPerUSD)
	}

	t.Setenv("ANVIL_MONTHLY_USD_CAP", "25")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.MonthlyUSDCapMicros != 25*usdMicrosPerUSD {
		t.Errorf("MonthlyUSDCapMicros = %d, want %d", cfg.MonthlyUSDCapMicros, 25*usdMicrosPerUSD)
	}
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

func TestConfig_Load_InvalidDatabaseMaxConnsFails(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DATABASE_MAX_CONNS", "not-a-number")

	if _, err := Load(); err == nil {
		t.Fatal("Load() succeeded, want error for unparseable DATABASE_MAX_CONNS")
	}
}

func TestConfig_Load_InvalidMonthlyUSDCapFails(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("ANVIL_MONTHLY_USD_CAP", "not-a-number")

	if _, err := Load(); err == nil {
		t.Fatal("Load() succeeded, want error for unparseable ANVIL_MONTHLY_USD_CAP")
	}
}

func TestConfig_Load_MissingRedisURLFails(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("REDIS_URL", "")

	if _, err := Load(); err == nil {
		t.Fatal("Load() succeeded, want error for missing REDIS_URL")
	}
}

func setRequiredBenchEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ANVIL_ANTHROPIC_API_KEY", "test-key-not-real")
}

func TestLoadBench_AppliesDefaults(t *testing.T) {
	setRequiredBenchEnv(t)

	cfg, err := LoadBench()
	if err != nil {
		t.Fatalf("LoadBench() error: %v", err)
	}
	if cfg.RunnerAddr != defaultRunnerAddr {
		t.Errorf("RunnerAddr = %q, want %q", cfg.RunnerAddr, defaultRunnerAddr)
	}
	if cfg.GeminiModel != defaultGeminiModel {
		t.Errorf("GeminiModel = %q, want %q", cfg.GeminiModel, defaultGeminiModel)
	}
}

func TestLoadBench_MissingAllKeysFails(t *testing.T) {
	t.Setenv("ANVIL_GEMINI_API_KEY", "")
	t.Setenv("ANVIL_ANTHROPIC_API_KEY", "")
	t.Setenv("ANVIL_OPENAI_API_KEY", "")

	if _, err := LoadBench(); err == nil {
		t.Fatal("LoadBench() succeeded, want error when no provider key is set")
	}
}

func TestLoadBench_OpenAIKeyAloneSatisfiesRequirement(t *testing.T) {
	t.Setenv("ANVIL_GEMINI_API_KEY", "")
	t.Setenv("ANVIL_ANTHROPIC_API_KEY", "")
	t.Setenv("ANVIL_OPENAI_API_KEY", "test-key-not-real")

	cfg, err := LoadBench()
	if err != nil {
		t.Fatalf("LoadBench() error: %v, want success with only ANVIL_OPENAI_API_KEY set", err)
	}
	if cfg.OpenAIModel != defaultOpenAIModel {
		t.Errorf("OpenAIModel = %q, want %q", cfg.OpenAIModel, defaultOpenAIModel)
	}
}

func TestBenchConfig_LogValue_RedactsKeys(t *testing.T) {
	setRequiredBenchEnv(t)
	t.Setenv("ANVIL_GEMINI_API_KEY", "gemini-secret-value")
	t.Setenv("ANVIL_OPENAI_API_KEY", "openai-secret-value")
	cfg, err := LoadBench()
	if err != nil {
		t.Fatalf("LoadBench() error: %v", err)
	}

	rendered := cfg.LogValue().String()
	if strings.Contains(rendered, cfg.AnthropicAPIKey) || strings.Contains(rendered, "gemini-secret-value") || strings.Contains(rendered, "openai-secret-value") {
		t.Fatal("LogValue() leaked a raw API key")
	}
}
