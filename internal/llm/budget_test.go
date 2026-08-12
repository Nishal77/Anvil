package llm

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"
)

type fakeSpendReader struct {
	spentUSDMicros int64
}

func (f *fakeSpendReader) MonthSpendUSDMicros(context.Context) (int64, error) {
	return f.spentUSDMicros, nil
}

func TestGlobalCap_AllowsUnderCap(t *testing.T) {
	reader := &fakeSpendReader{spentUSDMicros: 1_000_000}
	gc := NewGlobalCap(reader, 10_000_000, slog.Default())

	if err := gc.Check(context.Background()); err != nil {
		t.Fatalf("Check() = %v, want nil", err)
	}
}

func TestGlobalCap_RejectsAtOrOverCap(t *testing.T) {
	reader := &fakeSpendReader{spentUSDMicros: 10_000_000}
	gc := NewGlobalCap(reader, 10_000_000, slog.Default())

	err := gc.Check(context.Background())
	if err == nil {
		t.Fatal("Check() = nil, want ErrGlobalCapExceeded at 100% spend")
	}
}

func TestGlobalCap_ZeroCapDisablesEnforcement(t *testing.T) {
	reader := &fakeSpendReader{spentUSDMicros: 999_999_999}
	gc := NewGlobalCap(reader, 0, slog.Default())

	if err := gc.Check(context.Background()); err != nil {
		t.Fatalf("Check() = %v, want nil: a zero cap must not enforce", err)
	}
}

func TestGlobalCap_NilReceiverAllows(t *testing.T) {
	var gc *GlobalCap
	if err := gc.Check(context.Background()); err != nil {
		t.Fatalf("Check() on nil *GlobalCap = %v, want nil (Router.Cap is optional)", err)
	}
}

func TestEstimateTokens_HasHeadroomOverRawCharCount(t *testing.T) {
	req := Request{System: "0123456789abcdef"} // 16 chars -> raw len/4 estimate of 4
	got := estimateTokens(req)
	if got <= 4 {
		t.Fatalf("estimateTokens() = %d, want > 4 (headroom applied)", got)
	}
}

func TestInMemoryBudgetStore_GrantsDefaultBudgetToNewJob(t *testing.T) {
	store := NewInMemoryBudgetStore(150_000)
	jobID := uuid.New()

	b, err := store.GetJobBudget(context.Background(), jobID)
	if err != nil {
		t.Fatalf("GetJobBudget() error = %v", err)
	}
	if b.TokenBudget != 150_000 || b.TokensUsed != 0 {
		t.Fatalf("GetJobBudget() = %+v, want {150000 0}", b)
	}
}

func TestInMemoryBudgetStore_AddJobUsageAccumulates(t *testing.T) {
	store := NewInMemoryBudgetStore(150_000)
	jobID := uuid.New()
	ctx := context.Background()

	if err := store.AddJobUsage(ctx, jobID, 100, 5); err != nil {
		t.Fatalf("AddJobUsage() error = %v", err)
	}
	if err := store.AddJobUsage(ctx, jobID, 50, 5); err != nil {
		t.Fatalf("AddJobUsage() error = %v", err)
	}

	b, err := store.GetJobBudget(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJobBudget() error = %v", err)
	}
	if b.TokensUsed != 150 {
		t.Fatalf("TokensUsed = %d, want 150", b.TokensUsed)
	}
}
