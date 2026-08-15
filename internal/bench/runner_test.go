package bench

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anvil-dev/anvil/internal/agent"
	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/sandbox"
	"github.com/anvil-dev/anvil/internal/storage"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fakeSandbox is a minimal in-memory sandboxManager: exec commands are
// scripted by the test, and ExportWorkspace hands back whatever tar
// bytes the test configured — never touches Docker, so this package's
// tests run in milliseconds (the real sandbox is proven in
// internal/sandbox's own tests).
type fakeSandbox struct {
	mu        sync.Mutex
	created   int
	destroyed int
	execFunc  func(command string) (stdout string, exitCode int)
	workspace []byte
}

func newFakeSandbox() *fakeSandbox { return &fakeSandbox{} }

func (f *fakeSandbox) Create(context.Context, uuid.UUID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	return "fake-sandbox", nil
}

func (f *fakeSandbox) Exec(_ context.Context, _ string, command string, _ time.Duration, onChunk func(sandbox.ExecChunk)) error {
	stdout, exitCode := "", 0
	if f.execFunc != nil {
		stdout, exitCode = f.execFunc(command)
	}
	if stdout != "" {
		onChunk(sandbox.ExecChunk{Stream: "stdout", Data: []byte(stdout)})
	}
	onChunk(sandbox.ExecChunk{Final: true, ExitCode: exitCode})
	return nil
}

func (f *fakeSandbox) Destroy(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyed++
	return nil
}

func (f *fakeSandbox) ExportWorkspace(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.workspace)), nil
}

// fakeArtifactStore serves back whatever tar.gz bytes were uploaded
// via the executor's real upload path — Runner.runCheck downloads
// through this same interface in production.
type fakeArtifactStore struct {
	mu      sync.Mutex
	byJobID map[uuid.UUID][]byte
}

func newFakeArtifactStore() *fakeArtifactStore {
	return &fakeArtifactStore{byJobID: map[uuid.UUID][]byte{}}
}

func (f *fakeArtifactStore) Upload(_ context.Context, jobID uuid.UUID, r io.Reader, _ int64) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err //nolint:wrapcheck // reason: test fake
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byJobID[jobID] = data
	return jobID.String(), nil
}

func (f *fakeArtifactStore) Download(_ context.Context, jobID uuid.UUID) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.byJobID[jobID]
	if !ok {
		return nil, errors.New("not found")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func seedBenchUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(),
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		uuid.NewString()+"@example.com",
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func submitPlanCall(t *testing.T, steps int) llm.ToolCall {
	t.Helper()
	plan := struct {
		Summary string `json:"summary"`
		Steps   []struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Acceptance  string `json:"acceptance"`
		} `json:"steps"`
	}{Summary: "a plan"}
	for range steps {
		plan.Steps = append(plan.Steps, struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Acceptance  string `json:"acceptance"`
		}{Title: "step", Description: "do it", Acceptance: "it works"})
	}
	args, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	return llm.ToolCall{ID: "1", Name: "submit_plan", Input: args}
}

func stepDoneCall(t *testing.T, success bool) llm.ToolCall {
	t.Helper()
	args, err := json.Marshal(map[string]any{"summary": "done", "success": success})
	if err != nil {
		t.Fatalf("marshal step_done args: %v", err)
	}
	return llm.ToolCall{ID: "1", Name: "step_done", Input: args}
}

// testRunner wires a Runner against real Postgres, a fake sandbox, a
// fake artifact store, and an agent.Planner/agent.Executor pair driven
// by planProvider/execProvider — the real Planner+Executor pipeline
// this benchmark harness exists to measure, not a stand-in for it.
func testRunner(t *testing.T, sb *fakeSandbox, artifacts *fakeArtifactStore, planProvider, execProvider llm.Provider) *Runner {
	t.Helper()
	pool := requireIntegrationPool(t)

	registry, err := agent.NewRegistry(append(
		agent.NewFSTools(sb),
		agent.NewExecTool(sb),
		agent.NewStepDoneTool(),
	)...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	budget := llm.NewInMemoryBudgetStore(150_000)
	router, err := llm.NewRouter(llm.Config{
		Providers: map[llm.TaskClass][]llm.Provider{llm.TaskPlanning: {planProvider}, llm.TaskExecution: {execProvider}},
		Budget:    budget,
		Logger:    testLogger(),
	})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}

	policy, err := agent.NewPolicyEngine(agent.PolicyEngineConfig{Registry: registry, Sandbox: sb, Budget: budget})
	if err != nil {
		t.Fatalf("NewPolicyEngine: %v", err)
	}

	planner, err := agent.NewPlanner(agent.PlannerConfig{Router: router, Pool: pool, MaxSteps: 12, Logger: testLogger()})
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}

	executor, err := agent.New(agent.Config{
		Registry: registry, Policy: policy, Router: router, Sandbox: sb,
		Publisher: noopPublisher{}, IdemStore: noopIdemStore{}, Turns: pgTurnStore{pool},
		Pool: pool, Logger: testLogger(), Artifacts: artifacts,
	})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	userID := seedBenchUser(t, pool)
	r, err := NewRunner(Config{
		Pool: pool, Turns: pgTurnStore{pool}, Artifacts: artifacts,
		Planner: planner, Executor: executor, UserID: userID, Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

type noopPublisher struct{}

func (noopPublisher) Publish(context.Context, uuid.UUID, storage.EventType, json.RawMessage) error {
	return nil
}

// noopIdemStore skips idempotency caching entirely — fine for these
// tests, which never re-run the same step twice.
type noopIdemStore struct{}

func (noopIdemStore) GetIdem(context.Context, string) (json.RawMessage, bool, error) {
	return nil, false, nil
}

func (noopIdemStore) PutIdemIfAbsent(_ context.Context, _ string, _ uuid.UUID, result json.RawMessage) (json.RawMessage, error) {
	return result, nil
}

// pgTurnStore is storage.Store's AgentTurn methods, reimplemented
// minimally against the raw pool — avoids constructing a full
// storage.Store (which wants its own DSN-based connection) just to
// reuse three methods against a pool this test already has open.
type pgTurnStore struct{ pool *pgxpool.Pool }

func (s pgTurnStore) InsertAgentTurn(ctx context.Context, t storage.AgentTurn) (uuid.UUID, error) {
	const q = `
		INSERT INTO agent_turns (job_id, step_id, turn_idx, role, model, provider, prompt_sha256, tool_name, tool_args, policy_decision, policy_reason, tokens_in, tokens_out, cost_usd_micros, latency_ms)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		RETURNING id`
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, q, t.JobID, t.StepID, t.TurnIdx, t.Role, t.Model, t.Provider, t.PromptSHA256, t.ToolName, t.ToolArgs, t.PolicyDecision, t.PolicyReason, t.TokensIn, t.TokensOut, t.CostUSDMicros, t.LatencyMS).Scan(&id)
	return id, err //nolint:wrapcheck // reason: test fake
}

func (s pgTurnStore) UpdateAgentTurnResult(ctx context.Context, id uuid.UUID, observation string, tokensIn, tokensOut int, costUSDMicros int64, latencyMS int, execErr string) error {
	const q = `UPDATE agent_turns SET observation=$2, tokens_in=$3, tokens_out=$4, cost_usd_micros=$5, latency_ms=$6, error=NULLIF($7,'') WHERE id=$1`
	_, err := s.pool.Exec(ctx, q, id, observation, tokensIn, tokensOut, costUSDMicros, latencyMS, execErr)
	return err //nolint:wrapcheck // reason: test fake
}

func (s pgTurnStore) ListAgentTurns(ctx context.Context, jobID uuid.UUID) ([]storage.AgentTurn, error) {
	const q = `SELECT id, job_id, step_id, turn_idx, role, model, provider, prompt_sha256, COALESCE(tool_name,''), tool_args, policy_decision, COALESCE(policy_reason,''), COALESCE(observation,''), tokens_in, tokens_out, cost_usd_micros, COALESCE(latency_ms,0), COALESCE(error,''), created_at FROM agent_turns WHERE job_id=$1 ORDER BY turn_idx`
	rows, err := s.pool.Query(ctx, q, jobID)
	if err != nil {
		return nil, err //nolint:wrapcheck // reason: test fake
	}
	defer rows.Close()
	var out []storage.AgentTurn
	for rows.Next() {
		var t storage.AgentTurn
		if err := rows.Scan(&t.ID, &t.JobID, &t.StepID, &t.TurnIdx, &t.Role, &t.Model, &t.Provider, &t.PromptSHA256, &t.ToolName, &t.ToolArgs, &t.PolicyDecision, &t.PolicyReason, &t.Observation, &t.TokensIn, &t.TokensOut, &t.CostUSDMicros, &t.LatencyMS, &t.Error, &t.CreatedAt); err != nil {
			return nil, err //nolint:wrapcheck // reason: test fake
		}
		out = append(out, t)
	}
	return out, rows.Err() //nolint:wrapcheck // reason: test fake
}

func TestRunner_Run_PassingTaskWritesArtifactAndPasses(t *testing.T) {
	planProvider := llm.NewFakeProvider("fake-plan").ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{submitPlanCall(t, 1)}})
	execProvider := llm.NewFakeProvider("fake-exec").ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{stepDoneCall(t, true)}})

	sb := newFakeSandbox()
	sb.workspace = buildTestTarGz(t, map[string]string{"main.go": "package main"})
	artifacts := newFakeArtifactStore()
	sb.execFunc = func(command string) (string, int) {
		if strings.Contains(command, "go") {
			return "", 0 // the check command
		}
		return "", 0
	}

	r := testRunner(t, sb, artifacts, planProvider, execProvider)
	results, err := r.Run(context.Background(), []Task{{Name: "hello", Tier: TierTrivial, Prompt: "write hello world", Check: []string{"true"}}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Run() returned %d results, want 1", len(results))
	}
	got := results[0]
	if got.HarnessError != "" {
		t.Fatalf("HarnessError = %q, want empty", got.HarnessError)
	}
	if !got.Passed {
		t.Fatal("Passed = false, want true")
	}
	if sb.created != 1 || sb.destroyed != 1 {
		t.Fatalf("created=%d destroyed=%d, want 1 and 1", sb.created, sb.destroyed)
	}
}

func TestRunner_Run_FailingCheckIsNotAHarnessError(t *testing.T) {
	planProvider := llm.NewFakeProvider("fake-plan").ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{submitPlanCall(t, 1)}})
	execProvider := llm.NewFakeProvider("fake-exec").ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{stepDoneCall(t, true)}})

	sb := newFakeSandbox()
	sb.workspace = buildTestTarGz(t, map[string]string{"main.go": "this does not compile"})
	artifacts := newFakeArtifactStore()

	r := testRunner(t, sb, artifacts, planProvider, execProvider)
	results, err := r.Run(context.Background(), []Task{{Name: "broken", Tier: TierTrivial, Prompt: "x", Check: []string{"false"}}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if results[0].HarnessError != "" {
		t.Fatalf("HarnessError = %q, want empty — a failing check is a task failure, not a harness fault", results[0].HarnessError)
	}
	if results[0].Passed {
		t.Fatal("Passed = true, want false")
	}
}

func TestRunner_Run_PlanningFailureIsHarnessErrorAndDoesNotAbortOtherTasks(t *testing.T) {
	failingPlan := llm.NewFakeProvider("fake-plan").ScriptError(errors.New("provider unavailable"))
	goodPlan := llm.NewFakeProvider("fake-plan-2").ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{submitPlanCall(t, 1)}})
	execProvider := llm.NewFakeProvider("fake-exec").ScriptResponse(llm.Response{Model: "fake-model", ToolCalls: []llm.ToolCall{stepDoneCall(t, true)}})

	sb := newFakeSandbox()
	sb.workspace = buildTestTarGz(t, map[string]string{"main.go": "package main"})
	artifacts := newFakeArtifactStore()

	// Two Runners sharing one pool/user: the first only ever sees the
	// failing planner; the second's planner succeeds. Both jobs still
	// run to completion independently — one harness fault must not
	// abort the other task, same claim the old single-Runner test made,
	// now proven across two real job lifecycles instead of one Router's
	// script.
	r1 := testRunner(t, sb, artifacts, failingPlan, execProvider)
	results1, err := r1.Run(context.Background(), []Task{{Name: "first-fails-llm", Tier: TierTrivial, Prompt: "x", Check: []string{"true"}}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if results1[0].HarnessError == "" {
		t.Error("first task: HarnessError = \"\", want the planning error recorded")
	}

	r2 := testRunner(t, sb, artifacts, goodPlan, execProvider)
	results2, err := r2.Run(context.Background(), []Task{{Name: "second-succeeds", Tier: TierTrivial, Prompt: "x", Check: []string{"true"}}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if results2[0].HarnessError != "" || !results2[0].Passed {
		t.Errorf("second task: %+v, want a clean pass", results2[0])
	}
}

// buildTestTarGz builds a gzipped tar archive in memory, matching what
// sandbox.Client.ExportWorkspace would hand the executor after a real
// `tar czf - -C /workspace .` — the shape Runner.runCheck expects to
// extract.
func buildTestTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Size: int64(len(content)), Mode: 0o644}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("write tar content: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return buf.Bytes()
}
