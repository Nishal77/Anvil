package llm

import "testing"

func TestCostUSDMicros_KnownModel(t *testing.T) {
	// Haiku 4.5: $1/M in, $5/M out -> 1 micro/token in, 5 micros/token out (PRD §18.2).
	got := costUSDMicros("claude-haiku-4-5", Usage{InputTokens: 1000, OutputTokens: 100})
	want := int64(1000*1 + 100*5)
	if got != want {
		t.Fatalf("costUSDMicros() = %d, want %d", got, want)
	}
}

func TestCostUSDMicros_CachedTokensDiscounted(t *testing.T) {
	full := costUSDMicros("claude-haiku-4-5", Usage{InputTokens: 1000, OutputTokens: 0})
	halfCached := costUSDMicros("claude-haiku-4-5", Usage{InputTokens: 1000, CachedInputTokens: 500, OutputTokens: 0})
	if halfCached >= full {
		t.Fatalf("cached-input cost %d should be less than uncached cost %d", halfCached, full)
	}
}

func TestCostUSDMicros_UnrecognizedModelPricesZero(t *testing.T) {
	got := costUSDMicros("some-future-model", Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if got != 0 {
		t.Fatalf("costUSDMicros() for an unpriced model = %d, want 0 (fail open, not crash)", got)
	}
}

func TestCostUSDMicros_GeminiFreeTierIsZero(t *testing.T) {
	got := costUSDMicros("gemini-2.5-flash", Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if got != 0 {
		t.Fatalf("costUSDMicros() for Gemini free tier = %d, want 0", got)
	}
}
