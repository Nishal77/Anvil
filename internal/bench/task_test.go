package bench

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestTask(t *testing.T, root, name, prompt, checkJSON string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.txt"), []byte(prompt), 0o644); err != nil {
		t.Fatalf("WriteFile(prompt.txt) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "check.json"), []byte(checkJSON), 0o644); err != nil {
		t.Fatalf("WriteFile(check.json) error = %v", err)
	}
}

func TestLoadTasks_ReadsEveryTaskDirectory(t *testing.T) {
	root := t.TempDir()
	writeTestTask(t, root, "hello-world", "Create a Go hello-world with a test",
		`{"tier":"trivial","check":["go","test","./..."]}`)
	writeTestTask(t, root, "rest-api", "Build a REST API",
		`{"tier":"simple","check":["npm","test"]}`)

	tasks, err := LoadTasks(root)
	if err != nil {
		t.Fatalf("LoadTasks() error = %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("LoadTasks() returned %d tasks, want 2", len(tasks))
	}

	byName := map[string]Task{}
	for _, task := range tasks {
		byName[task.Name] = task
	}

	got, ok := byName["hello-world"]
	if !ok {
		t.Fatal("LoadTasks() missing task \"hello-world\"")
	}
	if got.Tier != TierTrivial {
		t.Errorf("Tier = %q, want trivial", got.Tier)
	}
	if got.Prompt != "Create a Go hello-world with a test" {
		t.Errorf("Prompt = %q", got.Prompt)
	}
	if len(got.Check) != 3 || got.Check[0] != "go" {
		t.Errorf("Check = %v, want [go test ./...]", got.Check)
	}
}

func TestLoadTasks_MissingDirReturnsError(t *testing.T) {
	if _, err := LoadTasks(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("LoadTasks() on a missing directory error = nil, want an error")
	}
}

func TestLoadTasks_IgnoresNonDirectoryEntries(t *testing.T) {
	root := t.TempDir()
	writeTestTask(t, root, "hello-world", "prompt", `{"tier":"trivial","check":["true"]}`)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("not a task"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	tasks, err := LoadTasks(root)
	if err != nil {
		t.Fatalf("LoadTasks() error = %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("LoadTasks() returned %d tasks, want 1 (README.md must be skipped)", len(tasks))
	}
}
