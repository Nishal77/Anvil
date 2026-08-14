package llm

import "strings"

// modelPricing is USD micros per MILLION tokens — the one place a
// model's price is looked up (CLAUDE.md: "one switch, one place", not
// string comparisons scattered through the router). Priced per
// million, not per token: several real models (e.g. $0.15/M input)
// are sub-1-micro per token, which would truncate to exactly 0 and
// silently zero out that half of the cost under a per-token integer
// rate. Cost is always int64 micros; a float64 here would drift over
// thousands of calls.
type modelPricing struct {
	inputMicrosPerMillion  int64
	outputMicrosPerMillion int64
	// cachedInputMicrosPerMillion is the discounted rate for tokens
	// served from provider prompt caching (NFR-013). Anthropic
	// documents a 90% discount on cached input.
	cachedInputMicrosPerMillion int64
}

const microsPerUnit = 1_000_000

// pricing is keyed by the provider's own model identifier
// (Response.Model), per PRD §18.2 rates as of Aug 2026 — revisit
// quarterly alongside the provider ladder in PRD §9.4.
var pricing = map[string]modelPricing{
	// Gemini free tier: $0. Real rate-limiting is the binding
	// constraint, not cost (PRD §18.2).
	"gemini-2.5-flash": {
		inputMicrosPerMillion:       0,
		outputMicrosPerMillion:      0,
		cachedInputMicrosPerMillion: 0,
	},
	// Claude Haiku 4.5: $1/M in, $5/M out (PRD §18.2/§18.3).
	"claude-haiku-4-5": {
		inputMicrosPerMillion:       1_000_000,
		outputMicrosPerMillion:      5_000_000,
		cachedInputMicrosPerMillion: 100_000, // 90% discount on cached input
	},
	// GPT-4o mini: $0.15/M in, $0.60/M out — OpenAI's long-published
	// mini-tier rate, high confidence.
	"gpt-4o-mini": {
		inputMicrosPerMillion:       150_000,
		outputMicrosPerMillion:      600_000,
		cachedInputMicrosPerMillion: 75_000, // OpenAI's published 50% cached-input discount
	},
	// GPT-5 mini: estimated at $0.25/M in, $2/M out, matching OpenAI's
	// established mini-tier pricing shape. Lower confidence than
	// gpt-4o-mini above — verify against platform.openai.com/pricing
	// before relying on this for a real budget decision.
	"gpt-5-mini": {
		inputMicrosPerMillion:       250_000,
		outputMicrosPerMillion:      2_000_000,
		cachedInputMicrosPerMillion: 125_000,
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
	p, ok := lookupPricing(model)
	if !ok {
		return 0
	}
	billableInput := usage.InputTokens - usage.CachedInputTokens
	if billableInput < 0 {
		billableInput = 0
	}
	return billableInput*p.inputMicrosPerMillion/microsPerUnit +
		usage.CachedInputTokens*p.cachedInputMicrosPerMillion/microsPerUnit +
		usage.OutputTokens*p.outputMicrosPerMillion/microsPerUnit
}

// lookupPricing tries an exact match first, then strips a trailing
// dated-snapshot suffix ("-20251001") and retries. Confirmed live:
// Anthropic's API reports Response.Model as the resolved snapshot
// ("claude-haiku-4-5-20251001"), not the alias requested
// ("claude-haiku-4-5") — an exact-match-only lookup silently priced
// every real call at $0, which would have made the global USD cap
// (FR-034) never trip no matter how much was actually spent.
func lookupPricing(model string) (modelPricing, bool) {
	if p, ok := pricing[model]; ok {
		return p, true
	}
	if base, ok := stripDateSnapshotSuffix(model); ok {
		if p, ok := pricing[base]; ok {
			return p, true
		}
	}
	return modelPricing{}, false
}

// stripDateSnapshotSuffix removes a trailing "-YYYYMMDD" (8 digits)
// component, the shape every major provider uses for a pinned model
// snapshot.
func stripDateSnapshotSuffix(model string) (string, bool) {
	i := strings.LastIndexByte(model, '-')
	if i < 0 || len(model)-i-1 != 8 {
		return "", false
	}
	suffix := model[i+1:]
	for _, c := range suffix {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	return model[:i], true
}
