package llm

import "errors"

var (
	// ErrRateLimited means the provider returned 429. This is NOT a
	// circuit-breaker failure: free-tier rate limiting is expected
	// operation, not an outage. Router fails over to the next provider
	// on the ladder without incrementing the breaker's failure count.
	ErrRateLimited = errors.New("llm: rate limited")

	// ErrProviderUnavailable covers 5xx, timeouts, and connection
	// errors. These DO count against the per-provider circuit breaker.
	ErrProviderUnavailable = errors.New("llm: provider unavailable")

	// ErrProviderFatal covers errors that will not succeed on retry
	// (4xx other than 429 — bad request, auth failure). Not retried,
	// not sent to a fallback provider, and does not count against the
	// breaker: retrying a malformed request against every provider in
	// the ladder just multiplies a bug by the ladder's length.
	ErrProviderFatal = errors.New("llm: provider rejected request")

	// ErrAllProvidersExhausted means every provider in the task
	// class's ladder failed or is breaker-open for this call.
	ErrAllProvidersExhausted = errors.New("llm: all providers exhausted")

	// ErrJobBudgetExceeded means the job's token_budget would be, or
	// was, exceeded (FR-033). Callers surface this as job status
	// FAILED(BUDGET_EXCEEDED) — never a silent context truncation.
	ErrJobBudgetExceeded = errors.New("llm: job token budget exceeded")

	// ErrGlobalCapExceeded means ANVIL_MONTHLY_USD_CAP has been
	// reached (FR-034). Callers surface this as HTTP 503 on POST /jobs.
	ErrGlobalCapExceeded = errors.New("llm: monthly usd cap exceeded")
)
