package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/queue"
)

const defaultMaxSteps = 12 // ANVIL_MAX_STEPS default, PRD §12.1 (3-12 steps)

// PlannedStep is one unit of the plan the model proposes. Steps must be
// independently re-runnable: on crash recovery a partially-executed
// step resets to PENDING and executes again from a clean state
// (PRD §14.2(e)) — the planner's system prompt states this constraint
// explicitly so the model doesn't propose steps with hidden ordering
// dependencies beyond their own idx.
type PlannedStep struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Acceptance  string `json:"acceptance"`
	Optional    bool   `json:"optional"`
}

// Plan is the planner's output. Risks are surfaced to the user on the
// approval screen — a planner that volunteers its own constraints reads
// as a system that understands its environment.
type Plan struct {
	Summary string        `json:"summary"`
	Steps   []PlannedStep `json:"steps"`
	Risks   []string      `json:"risks"`
}

// EnvDescription tells the planner what the sandbox actually contains,
// so it can flag constraints as Risks rather than discovering them at
// step 4. Static config for v1 (see the Week 7 spec's open question 3).
type EnvDescription struct {
	Image     string
	Languages []string // "go1.23", "node22", "python3.12"
	Network   string   // "allowlist: npm, pypi, proxy.golang.org, github"
	Services  []string // empty in v1 — no Postgres in the sandbox
}

// PlannerConfig configures a Planner.
type PlannerConfig struct {
	Router   *llm.Router
	Pool     *pgxpool.Pool
	MaxSteps int // ANVIL_MAX_STEPS, default 12
	Logger   *slog.Logger
}

func (c *PlannerConfig) setDefaults() {
	if c.MaxSteps <= 0 {
		c.MaxSteps = defaultMaxSteps
	}
}

func (c PlannerConfig) validate() error {
	for name, v := range map[string]bool{
		"Router": c.Router == nil, "Pool": c.Pool == nil, "Logger": c.Logger == nil,
	} {
		if v {
			return fmt.Errorf("agent: planner config: %s is required", name)
		}
	}
	return nil
}

// Planner is the Planner role of PRD §12.1: one native tool-calling
// request per job that decomposes its prompt into a Plan, persisted
// atomically by queue.SavePlan.
type Planner struct {
	router   *llm.Router
	pool     *pgxpool.Pool
	maxSteps int
	log      *slog.Logger
}

// NewPlanner constructs a Planner from cfg, or returns an error if cfg
// is invalid.
func NewPlanner(cfg PlannerConfig) (*Planner, error) {
	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Planner{router: cfg.Router, pool: cfg.Pool, maxSteps: cfg.MaxSteps, log: cfg.Logger}, nil
}

// RunPlan matches queue.Dispatcher.Config.RunStep's signature: it is
// wired in for jobs claimed in PLANNING. It decomposes job.Prompt into
// a Plan and persists it via queue.SavePlan, which itself performs the
// job's exit transition out of PLANNING — RunPlan returning nil does
// NOT mean the job succeeded in the terminal sense; it means planning
// is complete and the job has already moved to AWAITING_APPROVAL or
// QUEUED (see dispatcher.runJob's post-run status check).
func (p *Planner) RunPlan(ctx context.Context, job *queue.Job, env EnvDescription) error {
	plan, err := p.Plan(ctx, job, env)
	if err != nil {
		return err
	}
	steps := make([]queue.PlannedStep, len(plan.Steps))
	for i, s := range plan.Steps {
		steps[i] = queue.PlannedStep{Title: s.Title, Description: s.Description, Acceptance: s.Acceptance, Optional: s.Optional}
	}
	if err := queue.SavePlan(ctx, p.pool, job.ID, plan.Summary, plan.Risks, steps, job.AutoApprove); err != nil {
		return fmt.Errorf("agent: run plan for job %s: %w", job.ID, err)
	}
	return nil
}

// Plan decomposes job.Prompt into steps via the model's native
// tool-calling — structured output only, JSON is never parsed out of
// prose. MaxSteps is enforced here, in code: a model that returns more
// steps than MaxSteps has its plan rejected deterministically, never
// silently truncated (a silently dropped step is a plan the user never
// approved).
func (p *Planner) Plan(ctx context.Context, job *queue.Job, env EnvDescription) (*Plan, error) {
	req := llm.Request{
		TaskClass: llm.TaskPlanning,
		System:    plannerSystemPrompt(p.maxSteps, env),
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: job.Prompt}},
		Tools:     []llm.Tool{submitPlanTool},
	}

	resp, err := p.router.Complete(ctx, job.ID, req)
	if err != nil {
		return nil, fmt.Errorf("agent: plan job %s: llm complete: %w", job.ID, err)
	}

	call, ok := firstToolCall(resp)
	if !ok || call.Name != submitPlanToolName {
		return nil, fmt.Errorf("%w: job %s: model did not call %s", ErrPlannerDidNotCallTool, job.ID, submitPlanToolName)
	}

	var plan Plan
	if err := json.Unmarshal(call.Input, &plan); err != nil {
		return nil, fmt.Errorf("agent: plan job %s: decode %s args: %w", job.ID, submitPlanToolName, err)
	}
	if len(plan.Steps) == 0 {
		return nil, fmt.Errorf("%w: job %s: plan has zero steps", ErrPlanInvalid, job.ID)
	}
	if len(plan.Steps) > p.maxSteps {
		return nil, fmt.Errorf("%w: job %s: plan has %d steps, MaxSteps is %d", ErrPlanInvalid, job.ID, len(plan.Steps), p.maxSteps)
	}

	plan.Risks = append(plan.Risks, envRisks(env)...)
	return &plan, nil
}

// envRisks flags constraints EnvDescription implies but the model may
// not have volunteered on its own — surfaced to the user on the
// approval screen alongside whatever risks the model itself named.
func envRisks(env EnvDescription) []string {
	if len(env.Services) == 0 {
		return []string{"the sandbox has no attached services (e.g. no database) — steps that assume one will fail"}
	}
	return nil
}

const submitPlanToolName = "submit_plan"

// submitPlanTool is the sole tool offered to the planning request. The
// model has exactly one legal action: propose a plan through this
// schema — there is no other tool to fall back to, which is what makes
// "native tool calling only" enforceable rather than aspirational.
var submitPlanTool = llm.Tool{
	Name:        submitPlanToolName,
	Description: "Submit the decomposed plan for this job: a short summary, an ordered list of steps, and any risks worth flagging to the user before they approve.",
	InputSchema: json.RawMessage(`{
		"type": "object",
		"properties": {
			"summary": {"type": "string"},
			"steps": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {
						"title": {"type": "string"},
						"description": {"type": "string"},
						"acceptance": {"type": "string"},
						"optional": {"type": "boolean"}
					},
					"required": ["title", "description", "acceptance"]
				}
			},
			"risks": {"type": "array", "items": {"type": "string"}}
		},
		"required": ["summary", "steps"]
	}`),
}

func plannerSystemPrompt(maxSteps int, env EnvDescription) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the planning stage of an autonomous coding agent. Decompose the user's request into 3 to %d independently re-runnable steps. ", maxSteps)
	b.WriteString("Each step must state an Acceptance criterion the executor can check mechanically (e.g. \"go test ./... exits 0\"), not a subjective judgment. ")
	b.WriteString("A step whose failure should not fail the whole job must be marked optional. ")
	b.WriteString("Because a crashed worker resumes a step from scratch, no step may depend on in-memory state from a previous step's failed attempt. ")
	fmt.Fprintf(&b, "Sandbox: %s. Languages available: %s. Network: %s. ", env.Image, strings.Join(env.Languages, ", "), env.Network)
	b.WriteString("Call submit_plan exactly once with the full plan.")
	return b.String()
}
