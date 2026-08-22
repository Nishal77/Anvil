package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/anvil-dev/anvil/internal/telemetry"
)

const (
	defaultMaxRetries       = 3
	defaultBreakerThreshold = 5
	defaultBreakerWindow    = 60 * time.Second
	defaultBreakerCooldown  = 120 * time.Second
)

// Config configures a Router. Providers is the ordered ladder per task
// class — index 0 is primary, the rest are tried in order on a
// retryable error (FR-032).
type Config struct {
	Providers map[TaskClass][]Provider
	Budget    BudgetStore
	Cap       *GlobalCap
	Logger    *slog.Logger

	MaxRetries       int           // per-provider retry attempts before failing over; default 3 (FR-035)
	BreakerThreshold int           // failures within BreakerWindow that open a provider's breaker; default 5
	BreakerWindow    time.Duration // default 60s
	BreakerCooldown  time.Duration // default 120s
}

func (c *Config) setDefaults() {
	if c.MaxRetries <= 0 {
		c.MaxRetries = defaultMaxRetries
	}
	if c.BreakerThreshold <= 0 {
		c.BreakerThreshold = defaultBreakerThreshold
	}
	if c.BreakerWindow <= 0 {
		c.BreakerWindow = defaultBreakerWindow
	}
	if c.BreakerCooldown <= 0 {
		c.BreakerCooldown = defaultBreakerCooldown
	}
}

func (c Config) validate() error {
	if len(c.Providers) == 0 {
		return errors.New("llm: config: Providers is required")
	}
	if c.Budget == nil {
		return errors.New("llm: config: Budget is required")
	}
	if c.Logger == nil {
		return errors.New("llm: config: Logger is required")
	}
	return nil
}

// Router selects a Provider by TaskClass, fails over across that
// class's provider ladder on retryable errors, and enforces one
// circuit breaker per provider — never a global breaker, so one
// provider's outage cannot block calls to another.
type Router struct {
	providers  map[TaskClass][]Provider
	breakers   map[string]*breaker // keyed by Provider.Name()
	budget     BudgetStore
	cap        *GlobalCap
	log        *slog.Logger
	maxRetries int

	// sleep is the retry-delay wait, overridable in tests (same
	// package) so retry/failover tests don't burn real wall-clock time
	// on jittered backoff — CLAUDE.md T4 also applies to the delays a
	// unit test drives through, not only literal time.Sleep calls in
	// the test itself.
	sleep func(context.Context, time.Duration) error
}

// NewRouter constructs a Router from cfg, or returns an error if cfg
// is invalid.
func NewRouter(cfg Config) (*Router, error) {
	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	breakers := make(map[string]*breaker)
	for _, ladder := range cfg.Providers {
		for _, p := range ladder {
			if _, exists := breakers[p.Name()]; !exists {
				breakers[p.Name()] = newBreaker(cfg.BreakerThreshold, cfg.BreakerWindow, cfg.BreakerCooldown, realClock{})
			}
		}
	}

	return &Router{
		providers:  cfg.Providers,
		breakers:   breakers,
		budget:     cfg.Budget,
		cap:        cfg.Cap,
		log:        cfg.Logger,
		maxRetries: cfg.MaxRetries,
		sleep:      sleepCtx,
	}, nil
}

// Complete routes req to jobID's task-class ladder: a pre-call global
// cap check and job budget check (estimate-based, FR-033/FR-034),
// then per-provider circuit breaker plus jittered retry/failover
// across the ladder, then the post-call budget write using the
// provider's actual reported Usage.
//
// Retry only happens for ErrRateLimited and ErrProviderUnavailable —
// an LLM completion is safe to retry, a call that already streamed a
// partial, billed response is not (BUILD-PLAN W5 non-negotiable).
// ErrRateLimited fails over to the next provider immediately without
// retrying the same one and without counting against its breaker.
func (r *Router) Complete(ctx context.Context, jobID uuid.UUID, req Request) (Response, error) {
	spanName := "llm.complete"
	if req.TaskClass == TaskPlanning {
		spanName = "llm.plan" // PRD §17.1's tree names the planner's call distinctly from a step's
	}
	ctx, span := telemetry.Tracer("llm").Start(ctx, spanName, trace.WithAttributes(
		telemetry.AttrJobID.String(jobID.String()),
	))
	defer span.End()

	resp, err := r.complete(ctx, jobID, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return resp, err
	}
	span.SetAttributes(
		telemetry.AttrModel.String(resp.Model),
		telemetry.AttrTokensIn.Int64(resp.Usage.InputTokens),
		telemetry.AttrTokensOut.Int64(resp.Usage.OutputTokens),
		telemetry.AttrCostUSDMicros.Int64(costUSDMicros(resp.Model, resp.Usage)),
	)
	return resp, nil
}

// complete is Router.Complete's actual body, split out so the span
// wrapper above stays a thin, uniform shape regardless of how many
// branches complete itself grows — CLAUDE.md's complexity limit is on
// complete, not on span bookkeeping around it.
func (r *Router) complete(ctx context.Context, jobID uuid.UUID, req Request) (Response, error) {
	ladder, ok := r.providers[req.TaskClass]
	if !ok || len(ladder) == 0 {
		return Response{}, fmt.Errorf("llm: complete job %s: %w: no providers configured for task class %s", jobID, ErrAllProvidersExhausted, req.TaskClass)
	}

	if err := r.cap.Check(ctx); err != nil {
		return Response{}, fmt.Errorf("llm: complete job %s: %w", jobID, err)
	}

	jobBudget, err := r.budget.GetJobBudget(ctx, jobID)
	if err != nil {
		return Response{}, fmt.Errorf("llm: complete job %s: get job budget: %w", jobID, err)
	}
	if jobBudget.TokensUsed+estimateTokens(req) > jobBudget.TokenBudget {
		return Response{}, fmt.Errorf("llm: complete job %s: %w", jobID, ErrJobBudgetExceeded)
	}

	resp, err := r.completeAcrossLadder(ctx, ladder, req)
	if err != nil {
		return Response{}, fmt.Errorf("llm: complete job %s: %w", jobID, err)
	}

	cost := costUSDMicros(resp.Model, resp.Usage)
	recordUsage(resp.Model, resp.Usage, cost)
	totalTokens := resp.Usage.InputTokens + resp.Usage.OutputTokens
	if err := r.budget.AddJobUsage(ctx, jobID, totalTokens, cost); err != nil {
		return Response{}, fmt.Errorf("llm: complete job %s: record usage: %w", jobID, err)
	}

	return resp, nil
}

// completeAcrossLadder tries each provider in order via tryProvider,
// moving to the next on a failover outcome and stopping immediately
// on a fatal one.
func (r *Router) completeAcrossLadder(ctx context.Context, ladder []Provider, req Request) (Response, error) {
	for _, provider := range ladder {
		resp, outcome, err := r.tryProvider(ctx, provider, req)
		switch outcome {
		case outcomeSuccess:
			return resp, nil
		case outcomeFatal:
			return Response{}, err
		case outcomeFailover:
			continue
		}
	}
	return Response{}, ErrAllProvidersExhausted
}

// providerOutcome is tryProvider's verdict: whether completeAcrossLadder
// should return the response, stop entirely, or move to the next
// provider in the ladder.
type providerOutcome int

const (
	outcomeSuccess providerOutcome = iota
	outcomeFailover
	outcomeFatal
)

// tryProvider drives one provider's own retry loop (up to MaxRetries
// attempts on ErrProviderUnavailable), returning outcomeFailover the
// moment the breaker is open, a retry budget is exhausted, or
// ErrRateLimited is seen — never outcomeFatal for those, since a
// failover is not a stop condition for the ladder.
func (r *Router) tryProvider(ctx context.Context, provider Provider, req Request) (Response, providerOutcome, error) {
	b := r.breakers[provider.Name()]

	for attempt := 0; attempt <= r.maxRetries; attempt++ {
		if !b.allow() {
			llmCircuitState.WithLabelValues(provider.Name()).Set(circuitStateValue(true))
			return Response{}, outcomeFailover, nil // breaker open: skip straight to the next provider
		}

		start := time.Now()
		resp, err := provider.Complete(ctx, req)
		latency := time.Since(start).Seconds()

		if err == nil {
			b.recordSuccess()
			llmCircuitState.WithLabelValues(provider.Name()).Set(circuitStateValue(false))
			llmRequestsTotal.WithLabelValues(provider.Name(), resp.Model, "success").Inc()
			llmLatency.WithLabelValues(resp.Model).Observe(latency)
			return resp, outcomeSuccess, nil
		}
		llmRequestsTotal.WithLabelValues(provider.Name(), "", "error").Inc()

		switch {
		case errors.Is(err, ErrRateLimited):
			// Expected operation on a free tier, not a failure: fail
			// over without recording against this provider's breaker
			// and without retrying it.
			r.log.Info("provider rate limited, failing over", "component", "llm", "provider", provider.Name())
			return Response{}, outcomeFailover, nil
		case errors.Is(err, ErrProviderUnavailable):
			b.recordFailure()
			llmCircuitState.WithLabelValues(provider.Name()).Set(circuitStateValue(b.isOpen()))
			if attempt == r.maxRetries {
				return Response{}, outcomeFailover, nil
			}
			if sleepErr := r.sleep(ctx, fullJitterDelay(attempt)); sleepErr != nil {
				return Response{}, outcomeFatal, sleepErr
			}
		default:
			// Fatal or unclassified: not retryable, not failed-over.
			// Retrying a non-idempotent request against every
			// provider in the ladder just multiplies a bug by the
			// ladder's length.
			return Response{}, outcomeFatal, fmt.Errorf("provider %s: %w", provider.Name(), err)
		}
	}
	return Response{}, outcomeFailover, nil
}

// sleepCtx waits for d or ctx cancellation, whichever comes first —
// never a bare time.Sleep (CLAUDE.md §5.2), so a cancelled job doesn't
// block on a retry delay.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("llm: retry delay: %w", ctx.Err())
	}
}
