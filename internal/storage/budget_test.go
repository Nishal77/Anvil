package storage

import (
	"context"
	"testing"
)

func TestGetJobTokenBudget_ReturnsDefaults(t *testing.T) {
	t.Parallel()
	jobID := seedJobForEvents(t)

	budget, used, err := testStore.GetJobTokenBudget(context.Background(), jobID)
	if err != nil {
		t.Fatalf("GetJobTokenBudget() error = %v", err)
	}
	if budget != 150000 { // jobs.token_budget default, migration 002
		t.Errorf("budget = %d, want 150000", budget)
	}
	if used != 0 {
		t.Errorf("used = %d, want 0", used)
	}
}

func TestAddJobTokenUsage_Accumulates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	jobID := seedJobForEvents(t)

	if err := testStore.AddJobTokenUsage(ctx, jobID, 100, 5); err != nil {
		t.Fatalf("first AddJobTokenUsage() error = %v", err)
	}
	if err := testStore.AddJobTokenUsage(ctx, jobID, 50, 3); err != nil {
		t.Fatalf("second AddJobTokenUsage() error = %v", err)
	}

	_, used, err := testStore.GetJobTokenBudget(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJobTokenBudget() error = %v", err)
	}
	if used != 150 {
		t.Errorf("used = %d, want 150 (100 + 50)", used)
	}
}

// Deliberately not t.Parallel(): MonthSpendUSDMicros sums cost across
// every job in the current calendar month with no per-test scoping
// (it is a genuine global aggregate, matching production), so it must
// run in this package's serial phase — before any t.Parallel() sibling
// starts — or a concurrent AddJobTokenUsage elsewhere makes the
// before/after diff flaky.
func TestMonthSpendUSDMicros_SumsCurrentMonthJobs(t *testing.T) {
	ctx := context.Background()
	jobA := seedJobForEvents(t)
	jobB := seedJobForEvents(t)

	before, err := testStore.MonthSpendUSDMicros(ctx)
	if err != nil {
		t.Fatalf("MonthSpendUSDMicros() error = %v", err)
	}

	if err := testStore.AddJobTokenUsage(ctx, jobA, 0, 1000); err != nil {
		t.Fatalf("AddJobTokenUsage(jobA) error = %v", err)
	}
	if err := testStore.AddJobTokenUsage(ctx, jobB, 0, 2000); err != nil {
		t.Fatalf("AddJobTokenUsage(jobB) error = %v", err)
	}

	after, err := testStore.MonthSpendUSDMicros(ctx)
	if err != nil {
		t.Fatalf("MonthSpendUSDMicros() error = %v", err)
	}
	if after-before != 3000 {
		t.Errorf("MonthSpendUSDMicros() increased by %d, want 3000 (1000 + 2000)", after-before)
	}
}
