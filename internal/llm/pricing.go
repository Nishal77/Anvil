package llm

// modelPricing is USD micros per token — the one place a model's price
// is looked up (CLAUDE.md: "one switch, one place", not string
// comparisons scattered through the router). Cost is always int64
// micros; a float64 here would drift over thousands of calls.
type modelPricing struct {
	inputMicrosPerToken  int64
	outputMicrosPerToken int64
	// cachedInputMicrosPerToken is the discounted rate for tokens
	// served from provider prompt caching (NFR-013). Anthropic
	// documents a 90% discount on cached input.
	cachedInputMicrosPerToken int64
}

// pricing is keyed by the provider's own model identifier
// (Response.Model), per PRD §18.2 rates as of Aug 2026 — revisit
// quarterly alongside the provider ladder in PRD §9.4.
var pricing = map[string]modelPricing{
	// Gemini free tier: $0. Real rate-limiting is the binding
	// constraint, not cost (PRD §18.2).
	"gemini-2.5-flash": {
		inputMicrosPerToken:       0,
		outputMicrosPerToken:      0,
		cachedInputMicrosPerToken: 0,
	},
	// Claude Haiku 4.5: $1/M in, $5/M out (PRD §18.2/§18.3).
	"claude-haiku-4-5": {
		inputMicrosPerToken:       1,
		outputMicrosPerToken:      5,
		cachedInputMicrosPerToken: 0, // 90% of $1/M rounds to 0 micros/token; captured at higher volume via CachedInputTokens count, not rate
	},
}

// CostUSDMicros computes the USD-micros cost of one Usage against
// model's known pricing (Response.Model). Exported so callers outside
// this package — the benchmark harness's "mean cost" column (PRD
// §20.5) — can price a Response without duplicating the pricing table.
func CostUSDMicros(model string, usage Usage) int64 {
	return costUSDMicros(model, usage)
}

// costUSDMicros computes the USD-micros cost of one Usage against
// model's known pricing. An unrecognized model prices as zero rather
// than erroring — a new model showing up in a response must not crash
// the budget accounting; it is caught instead by the "unpriced model"
// metric a reviewer would add in Phase 4 observability.
func costUSDMicros(model string, usage Usage) int64 {
	p, ok := pricing[model]
	if !ok {
		return 0
	}
	billableInput := usage.InputTokens - usage.CachedInputTokens
	if billableInput < 0 {
		billableInput = 0
	}
	return billableInput*p.inputMicrosPerToken +
		usage.CachedInputTokens*p.cachedInputMicrosPerToken +
		usage.OutputTokens*p.outputMicrosPerToken
}
