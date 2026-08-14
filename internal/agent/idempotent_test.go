package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
)

type fakeIdemStore struct {
	mu   sync.Mutex
	rows map[string]json.RawMessage
}

func newFakeIdemStore() *fakeIdemStore {
	return &fakeIdemStore{rows: map[string]json.RawMessage{}}
}

func (f *fakeIdemStore) GetIdem(_ context.Context, key string) (json.RawMessage, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.rows[key]
	return v, ok, nil
}

func (f *fakeIdemStore) PutIdemIfAbsent(_ context.Context, key string, _ uuid.UUID, result json.RawMessage) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing, ok := f.rows[key]; ok {
		return existing, nil
	}
	f.rows[key] = result
	return result, nil
}

func TestCallIdempotent_MissRunsFnAndCaches(t *testing.T) {
	store := newFakeIdemStore()
	calls := 0
	fn := func() (json.RawMessage, error) {
		calls++
		return json.RawMessage(`"result"`), nil
	}

	jobID, stepID := uuid.New(), uuid.New()
	args := json.RawMessage(`{"a":1}`)

	if _, err := callIdempotent(context.Background(), store, jobID, stepID, "exec", args, fn); err != nil {
		t.Fatalf("callIdempotent() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("fn called %d times, want 1", calls)
	}
}

func TestCallIdempotent_HitSkipsFn(t *testing.T) {
	store := newFakeIdemStore()
	calls := 0
	fn := func() (json.RawMessage, error) {
		calls++
		return json.RawMessage(`"result"`), nil
	}

	jobID, stepID := uuid.New(), uuid.New()
	args := json.RawMessage(`{"a":1}`)

	if _, err := callIdempotent(context.Background(), store, jobID, stepID, "exec", args, fn); err != nil {
		t.Fatalf("first call error = %v", err)
	}
	result, err := callIdempotent(context.Background(), store, jobID, stepID, "exec", args, fn)
	if err != nil {
		t.Fatalf("second call error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("fn called %d times across two identical calls, want 1 (second must hit cache)", calls)
	}
	if string(result) != `"result"` {
		t.Errorf("result = %s, want the cached value", result)
	}
}

func TestCallIdempotent_ErrorNeverCached(t *testing.T) {
	store := newFakeIdemStore()
	calls := 0
	fn := func() (json.RawMessage, error) {
		calls++
		return nil, errors.New("transient failure")
	}

	jobID, stepID := uuid.New(), uuid.New()
	args := json.RawMessage(`{"a":1}`)

	for i := 0; i < 2; i++ {
		if _, err := callIdempotent(context.Background(), store, jobID, stepID, "exec", args, fn); err == nil {
			t.Fatalf("call %d: error = nil, want the transient failure", i)
		}
	}
	if calls != 2 {
		t.Fatalf("fn called %d times, want 2 — an error must never be cached, so a retry runs fn again", calls)
	}
}

func TestCallIdempotent_DifferentArgsDifferentKeys(t *testing.T) {
	store := newFakeIdemStore()
	calls := 0
	fn := func() (json.RawMessage, error) {
		calls++
		return json.RawMessage(`"result"`), nil
	}

	jobID, stepID := uuid.New(), uuid.New()
	if _, err := callIdempotent(context.Background(), store, jobID, stepID, "exec", json.RawMessage(`{"a":1}`), fn); err != nil {
		t.Fatalf("call 1 error = %v", err)
	}
	if _, err := callIdempotent(context.Background(), store, jobID, stepID, "exec", json.RawMessage(`{"a":2}`), fn); err != nil {
		t.Fatalf("call 2 error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("fn called %d times, want 2 — different args must not collide on the same key", calls)
	}
}

// TestIdemKey_StableAcrossMapIterationOrder is the specific bug the
// canonical-JSON encoder exists to prevent: encoding/json's map
// iteration order is randomized per process, so hashing a naively
// re-marshaled map would produce a different key for logically
// identical args from one call to the next.
func TestIdemKey_StableAcrossMapIterationOrder(t *testing.T) {
	jobID, stepID := uuid.New(), uuid.New()
	a := json.RawMessage(`{"zebra":1,"apple":2,"mango":3}`)
	b := json.RawMessage(`{"apple":2,"mango":3,"zebra":1}`) // same fields, different source order

	keyA, err := idemKey(jobID, stepID, "exec", a)
	if err != nil {
		t.Fatalf("idemKey(a) error = %v", err)
	}
	keyB, err := idemKey(jobID, stepID, "exec", b)
	if err != nil {
		t.Fatalf("idemKey(b) error = %v", err)
	}
	if keyA != keyB {
		t.Errorf("idemKey differs for the same logical args in different field order: %s vs %s", keyA, keyB)
	}
}

func TestIdemKey_DifferentJobOrStepDiffers(t *testing.T) {
	args := json.RawMessage(`{"a":1}`)
	k1, _ := idemKey(uuid.New(), uuid.New(), "exec", args)
	k2, _ := idemKey(uuid.New(), uuid.New(), "exec", args)
	if k1 == k2 {
		t.Error("idemKey collided for two different (job, step) pairs")
	}
}

func TestCanonicalJSON_SortsNestedObjects(t *testing.T) {
	in := json.RawMessage(`{"b":{"z":1,"a":2},"a":[3,{"y":1,"x":2}]}`)
	out, err := canonicalJSON(in)
	if err != nil {
		t.Fatalf("canonicalJSON() error = %v", err)
	}
	want := `{"a":[3,{"x":2,"y":1}],"b":{"a":2,"z":1}}`
	if string(out) != want {
		t.Errorf("canonicalJSON() = %s, want %s", out, want)
	}
}
