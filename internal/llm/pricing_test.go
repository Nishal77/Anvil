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

// TestCostUSDMicros_SubMicroPerTokenRateDoesNotZeroOut is the bug a
// per-token integer rate would hit: gpt-4o-mini's $0.15/M input is
// 0.15 micros/token, which truncates to exactly 0 if priced per
// token. Pricing per million and dividing once at the end (not once
// per token) is what keeps this nonzero.
func TestCostUSDMicros_SubMicroPerTokenRateDoesNotZeroOut(t *testing.T) {
	got := costUSDMicros("gpt-4o-mini", Usage{InputTokens: 100_000, OutputTokens: 0})
	if got == 0 {
		t.Fatal("costUSDMicros() = 0 for 100,000 input tokens at $0.15/M — sub-micro-per-token rate got truncated away")
	}
	want := int64(100_000) * 150_000 / microsPerUnit // = 15,000 micros = $0.015
	if got != want {
		t.Fatalf("costUSDMicros() = %d, want %d", got, want)
	}
}

// TestCostUSDMicros_DatedSnapshotModelPricesSameAsAlias is a
// regression test for a bug confirmed against the real Anthropic API:
// Response.Model comes back as the resolved dated snapshot
// ("claude-haiku-4-5-20251001"), never the bare alias
// ("claude-haiku-4-5") requested — an exact-match-only pricing lookup
// silently priced every real call at $0, so FR-034's global USD cap
// would never trip no matter how much was actually spent.
func TestCostUSDMicros_DatedSnapshotModelPricesSameAsAlias(t *testing.T) {
	usage := Usage{InputTokens: 1000, OutputTokens: 100}
	alias := costUSDMicros("claude-haiku-4-5", usage)
	snapshot := costUSDMicros("claude-haiku-4-5-20251001", usage)
	if snapshot == 0 {
		t.Fatal("costUSDMicros() for a dated snapshot model = 0, want the same price as its bare alias")
	}
	if snapshot != alias {
		t.Fatalf("costUSDMicros(dated snapshot) = %d, want %d (same as the bare alias)", snapshot, alias)
	}
}

func TestStripDateSnapshotSuffix(t *testing.T) {
	tests := []struct {
		model    string
		wantBase string
		wantOK   bool
	}{
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5", true},
		// OpenAI's dash-separated "YYYY-MM-DD" isn't a single 8-digit
		// trailing component ("06" is 2 digits), so it's correctly left
		// alone — not a confirmed-live format this fix targets.
		{"gpt-4o-2024-08-06", "", false},
		{"claude-haiku-4-5", "", false}, // no trailing digit run
		{"gpt-4o-mini", "", false},
		{"model-2025080", "", false}, // 7 digits, not 8
		{"model--20251001", "model-", true},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			base, ok := stripDateSnapshotSuffix(tt.model)
			if ok != tt.wantOK || base != tt.wantBase {
				t.Errorf("stripDateSnapshotSuffix(%q) = (%q, %t), want (%q, %t)", tt.model, base, ok, tt.wantBase, tt.wantOK)
			}
		})
	}
}
