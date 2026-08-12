package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Tier is a task's difficulty band (PRD §20.5).
type Tier string

const (
	TierTrivial  Tier = "trivial"
	TierSimple   Tier = "simple"
	TierModerate Tier = "moderate"
	TierHard     Tier = "hard"
)

// Task is one benchmark task loaded from dir/<name>/ — a prompt.txt
// and a check.json. Sourced from disk so adding a task is a new
// directory, never a code change.
type Task struct {
	Name   string
	Tier   Tier
	Prompt string
	// Check is the command + args run inside the sandbox after the
	// model's files are applied; exit 0 means the task passed.
	Check []string
}

type checkFile struct {
	Tier  Tier     `json:"tier"`
	Check []string `json:"check"`
}

// LoadTasks reads every task directory under dir, sorted by name for
// a stable, reproducible run order.
func LoadTasks(dir string) ([]Task, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("bench: load tasks from %s: %w", dir, err)
	}

	var tasks []Task
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		task, err := loadTask(dir, entry.Name())
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func loadTask(dir, name string) (Task, error) {
	taskDir := filepath.Join(dir, name)

	promptBytes, err := os.ReadFile(filepath.Join(taskDir, "prompt.txt"))
	if err != nil {
		return Task{}, fmt.Errorf("bench: load task %s: read prompt.txt: %w", name, err)
	}

	checkBytes, err := os.ReadFile(filepath.Join(taskDir, "check.json"))
	if err != nil {
		return Task{}, fmt.Errorf("bench: load task %s: read check.json: %w", name, err)
	}
	var cf checkFile
	if err := json.Unmarshal(checkBytes, &cf); err != nil {
		return Task{}, fmt.Errorf("bench: load task %s: parse check.json: %w", name, err)
	}

	return Task{Name: name, Tier: cf.Tier, Prompt: string(promptBytes), Check: cf.Check}, nil
}
