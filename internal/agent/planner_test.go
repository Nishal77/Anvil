package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/queue"
)

func testPlanner(t *testing.T, provider llm.Provider, maxSteps int) *Planner {
	t.Helper()
	router, err := llm.NewRouter(llm.Config{
		Providers: map[llm.TaskClass][]llm.Provider{llm.TaskPlanning: {provider}},
		Budget:    llm.NewInMemoryBudgetStore(150_000),
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("llm.NewRouter() error = %v", err)
	}
	p, err := NewPlanner(PlannerConfig{Router: router, Pool: requireIntegrationPool(t), MaxSteps: maxSteps, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("NewPlanner() error = %v", err)
	}
	return p
}

func submitPlanCall(t *testing.T, steps int) llm.ToolCall {
	t.Helper()
	plan := Plan{Summary: "a plan"}
	for i := range steps {
		plan.Steps = append(plan.Steps, PlannedStep{Title: "step", Description: "do it", Acceptance: "it works"})
		_ = i
	}
	args, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return llm.ToolCall{ID: "1", Name: submitPlanToolName, Input: args}
}

func TestPlannerConfig_SetDefaults(t *testing.T) {
	cfg := PlannerConfig{}
	cfg.setDefaults()
	if cfg.MaxSteps != defaultMaxSteps {
		t.Errorf("MaxSteps = %d, want %d", cfg.MaxSteps, defaultMaxSteps)
	}
}

func TestPlannerConfig_Validate_MissingFields(t *testing.T) {
	if err := (PlannerConfig{}).validate(); err == nil {
		t.Fatal("validate() error = nil, want an error for a completely empty PlannerConfig")
	}
}

func TestNewPlanner_RejectsInvalidConfig(t *testing.T) {
	if _, err := NewPlanner(PlannerConfig{}); err == nil {
		t.Fatal("NewPlanner() error = nil, want an error for a completely empty PlannerConfig")
	}
}

// TestFR020_PlannerUsesNativeToolCalling proves a response with no
// tool call — text only, exactly what "JSON parsed out of prose" would
// require — is rejected, never scraped for a plan.
func TestFR020_PlannerUsesNativeToolCalling(t *testing.T) {
	provider := llm.NewFakeProvider("fake").ScriptResponse(llm.Response{Model: "fake-model", Text: `{"summary":"sneaky","steps":[]}`})
	p := testPlanner(t, provider, 12)

	_, err := p.Plan(context.Background(), &queue.Job{ID: uuid.New(), Prompt: "build a thing"}, EnvDescription{})
	if !errors.Is(err, ErrPlannerDidNotCallTool) {
		t.Fatalf("Plan() error = %v, want ErrPlannerDidNotCallTool", err)
	}
}

// TestFR020_MaxStepsEnforcedInCode proves a plan exceeding MaxSteps is
// rejected deterministically — the limit is enforced in code, not
// merely requested in the prompt.
func TestFR020_MaxStepsEnforcedInCode(t *testing.T) {
	provider := llm.NewFakeProvider("fake").ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{submitPlanCall(t, 30)}})
	p := testPlanner(t, provider, 12)

	_, err := p.Plan(context.Background(), &queue.Job{ID: uuid.New(), Prompt: "build a thing"}, EnvDescription{})
	if !errors.Is(err, ErrPlanInvalid) {
		t.Fatalf("Plan() error = %v, want ErrPlanInvalid for a 30-step plan against MaxSteps=12", err)
	}
}

// TestFR020_RisksSurfacedFromEnvDescription proves an environment with
// no attached services produces a risk entry, even when the model
// itself named none.
func TestFR020_RisksSurfacedFromEnvDescription(t *testing.T) {
	provider := llm.NewFakeProvider("fake").ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{submitPlanCall(t, 3)}})
	p := testPlanner(t, provider, 12)

	plan, err := p.Plan(context.Background(), &queue.Job{ID: uuid.New(), Prompt: "build a thing"}, EnvDescription{Services: nil})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if len(plan.Risks) == 0 {
		t.Error("Risks is empty, want a no-services risk surfaced from EnvDescription")
	}
}

// TestPlanner_RunPlan_PersistsAndTransitionsJob proves RunPlan's
// SavePlan call actually lands: the job moves out of PLANNING and its
// steps exist, end to end through the real Planner (not just Plan()'s
// in-memory return value).
func TestPlanner_RunPlan_PersistsAndTransitionsJob(t *testing.T) {
	pool := requireIntegrationPool(t)
	job := seedClaimedPlanningJob(t, pool)

	provider := llm.NewFakeProvider("fake").ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{submitPlanCall(t, 2)}})
	p := testPlanner(t, provider, 12)

	if err := p.RunPlan(context.Background(), job, EnvDescription{Services: []string{"postgres"}}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}

	got, err := queue.GetJob(context.Background(), pool, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != queue.StatusAwaitingApproval {
		t.Errorf("Status = %s, want AWAITING_APPROVAL", got.Status)
	}

	steps, err := queue.ListSteps(context.Background(), pool, job.ID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("len(steps) = %d, want 2", len(steps))
	}
}

// seedClaimedPlanningJob seeds a real job in PLANNING with a live
// lease, the state RunPlan/SavePlan expect to transition out of.
func seedClaimedPlanningJob(t *testing.T, pool *pgxpool.Pool) *queue.Job {
	t.Helper()
	userID := seedTestUser(t, pool)
	job, err := queue.CreateJob(context.Background(), pool, userID, "build a thing", false)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	_, err = pool.Exec(context.Background(),
		`UPDATE jobs SET status = 'PLANNING', lease_owner = 'test', lease_expires_at = now() + interval '1 minute' WHERE id = $1`,
		job.ID)
	if err != nil {
		t.Fatalf("seedClaimedPlanningJob: %v", err)
	}
	got, err := queue.GetJob(context.Background(), pool, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	return got
}

// TestFR020_ValidPlanIsAccepted is the baseline: a well-formed plan
// within MaxSteps passes through unchanged.
func TestFR020_ValidPlanIsAccepted(t *testing.T) {
	provider := llm.NewFakeProvider("fake").ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{submitPlanCall(t, 3)}})
	p := testPlanner(t, provider, 12)

	plan, err := p.Plan(context.Background(), &queue.Job{ID: uuid.New(), Prompt: "build a thing"}, EnvDescription{Services: []string{"postgres"}})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if len(plan.Steps) != 3 {
		t.Errorf("len(Steps) = %d, want 3", len(plan.Steps))
	}
}
