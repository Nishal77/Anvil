// Package llm routes completion requests across LLM providers by task
// class, enforces per-job and global-monthly budgets, and fails over
// on rate limits and transient errors behind a per-provider circuit
// breaker. Router is the package's entry point; Provider is the
// interface every backend (Gemini, Anthropic, FakeProvider) implements.
package llm
