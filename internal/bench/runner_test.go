package bench

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/llm"
	"github.com/anvil-dev/anvil/internal/sandbox"
)

// fakeSandbox is a minimal in-memory sandboxRunner: files written via
// the base64 "write" commands land in a map, and any exec whose
// command matches wantCheck's joined form is scored against
// checkExitCode. It never touches Docker, so bench's own tests run in
// milliseconds (CLAUDE.md T3/T5 spirit: no real infra for logic
// tests; the real sandbox is proven in internal/sandbox's own tests).
type fakeSandbox struct {
	mu            sync.Mutex
	created       int
	destroyed     int
	execCommands  []string
	createErr     error
	execErr       error
	checkExitCode int
	writtenFiles  map[string]bool
}

func newFakeSandbox() *fakeSandbox {
	return &fakeSandbox{writtenFiles: map[string]bool{}}
}

func (f *fakeSandbox) Create(context.Context, uuid.UUID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	if f.createErr != nil {
		return "", f.createErr
	}
	return "fake-sandbox-id", nil
}

func (f *fakeSandbox) Exec(_ context.Context, _ string, command string, _ time.Duration, onChunk func(sandbox.ExecChunk)) error {
	f.mu.Lock()
	f.execCommands = append(f.execCommands, command)
	f.mu.Unlock()

	if f.execErr != nil {
		return f.execErr
	}

	exitCode := 0
	switch {
	case strings.Contains(command, "base64 -d"):
		f.mu.Lock()
		f.writtenFiles[command] = true
		f.mu.Unlock()
	default:
		exitCode = f.checkExitCode
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

func writeFileCall(path, content string) llm.ToolCall {
	input, _ := json.Marshal(map[string]string{"path": path, "content": content})
	return llm.ToolCall{ID: "1", Name: "write_file", Input: input}
}

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestRunner_Run_PassingTaskWritesFilesAndPasses(t *testing.T) {
	provider := llm.NewFakeProvider("fake").ScriptResponse(llm.Response{
		Model:     "claude-haiku-4-5",
		ToolCalls: []llm.ToolCall{writeFileCall("main.go", "package main")},
		Usage:     llm.Usage{InputTokens: 100, OutputTokens: 20},
	})
	router, err := llm.NewRouter(llm.Config{
		Providers: map[llm.TaskClass][]llm.Provider{llm.TaskPlanning: {provider}},
		Budget:    llm.NewInMemoryBudgetStore(150_000),
		Logger:    testLogger(),
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	sb := newFakeSandbox()
	sb.checkExitCode = 0
	r := &Runner{router: router, sandbox: sb, log: testLogger()}

	results, err := r.Run(context.Background(), []Task{{Name: "hello", Tier: TierTrivial, Prompt: "write hello world", Check: []string{"go", "test", "./..."}}})
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
	if got.Usage.InputTokens != 100 {
		t.Fatalf("Usage.InputTokens = %d, want 100", got.Usage.InputTokens)
	}
	if sb.created != 1 || sb.destroyed != 1 {
		t.Fatalf("created=%d destroyed=%d, want 1 and 1", sb.created, sb.destroyed)
	}
}

func TestRunner_Run_FailingCheckIsNotAHarnessError(t *testing.T) {
	provider := llm.NewFakeProvider("fake").ScriptResponse(llm.Response{
		Model:     "claude-haiku-4-5",
		ToolCalls: []llm.ToolCall{writeFileCall("main.go", "this does not compile")},
	})
	router, _ := llm.NewRouter(llm.Config{
		Providers: map[llm.TaskClass][]llm.Provider{llm.TaskPlanning: {provider}},
		Budget:    llm.NewInMemoryBudgetStore(150_000),
		Logger:    testLogger(),
	})

	sb := newFakeSandbox()
	sb.checkExitCode = 1 // the check ran and failed
	r := &Runner{router: router, sandbox: sb, log: testLogger()}

	results, err := r.Run(context.Background(), []Task{{Name: "broken", Tier: TierTrivial, Prompt: "x", Check: []string{"go", "build"}}})
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

func TestRunner_Run_SandboxFailureIsHarnessErrorAndDoesNotAbortOtherTasks(t *testing.T) {
	failingSandbox := newFakeSandbox()
	failingSandbox.createErr = errors.New("docker daemon unreachable")

	provider := llm.NewFakeProvider("fake") // never called: sandbox creation fails first
	router, _ := llm.NewRouter(llm.Config{
		Providers: map[llm.TaskClass][]llm.Provider{llm.TaskPlanning: {provider}},
		Budget:    llm.NewInMemoryBudgetStore(150_000),
		Logger:    testLogger(),
	})
	r := &Runner{router: router, sandbox: failingSandbox, log: testLogger()}

	results, err := r.Run(context.Background(), []Task{
		{Name: "broken-sandbox", Tier: TierTrivial, Prompt: "x", Check: []string{"true"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil — a harness fault is reported per-task, not fatal to the run", err)
	}
	if results[0].HarnessError == "" {
		t.Fatal("HarnessError = \"\", want a sandbox-create failure recorded")
	}
	if results[0].Passed {
		t.Fatal("Passed = true, want false on a harness fault")
	}
}

func TestRunner_Run_MultipleTasksAllRunDespiteOneFailing(t *testing.T) {
	good := llm.NewFakeProvider("fake").
		ScriptError(errors.New("boom")).
		ScriptResponse(llm.Response{Model: "claude-haiku-4-5", ToolCalls: []llm.ToolCall{writeFileCall("a.go", "x")}})
	router, _ := llm.NewRouter(llm.Config{
		Providers: map[llm.TaskClass][]llm.Provider{llm.TaskPlanning: {good}},
		Budget:    llm.NewInMemoryBudgetStore(150_000),
		Logger:    testLogger(),
	})
	sb := newFakeSandbox()
	r := &Runner{router: router, sandbox: sb, log: testLogger()}

	results, err := r.Run(context.Background(), []Task{
		{Name: "first-fails-llm", Tier: TierTrivial, Prompt: "x", Check: []string{"true"}},
		{Name: "second-succeeds", Tier: TierTrivial, Prompt: "x", Check: []string{"true"}},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("Run() returned %d results, want 2 — one task failing must not stop the others", len(results))
	}
	if results[0].HarnessError == "" {
		t.Error("first task: HarnessError = \"\", want the LLM error recorded")
	}
	if results[1].HarnessError != "" || !results[1].Passed {
		t.Errorf("second task: %+v, want a clean pass", results[1])
	}
}
