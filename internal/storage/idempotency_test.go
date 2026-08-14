package storage

import (
	"context"
	"encoding/json"
	"testing"
)

func TestGetIdem_MissingKeyReturnsNotOK(t *testing.T) {
	t.Parallel()
	_, ok, err := testStore.GetIdem(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("GetIdem() error = %v", err)
	}
	if ok {
		t.Error("GetIdem() ok = true, want false for a missing key")
	}
}

func TestPutIdemIfAbsent_GetIdem_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	jobID := seedJobForEvents(t)
	key := "test-key-" + jobID.String()

	stored, err := testStore.PutIdemIfAbsent(ctx, key, jobID, json.RawMessage(`{"ok":true}`))
	if err != nil {
		t.Fatalf("PutIdemIfAbsent() error = %v", err)
	}
	assertJSONEqual(t, stored, `{"ok":true}`)

	got, ok, err := testStore.GetIdem(ctx, key)
	if err != nil {
		t.Fatalf("GetIdem() error = %v", err)
	}
	if !ok {
		t.Fatal("GetIdem() ok = false, want true after a successful put")
	}
	assertJSONEqual(t, got, `{"ok":true}`)
}

// assertJSONEqual compares decoded values, not raw bytes: a jsonb
// column round-trips through Postgres's own canonical text form
// (spaced, keys possibly reordered), which legitimately differs byte-
// for-byte from what was written without being a different value.
func assertJSONEqual(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var gotVal, wantVal any
	if err := json.Unmarshal(got, &gotVal); err != nil {
		t.Fatalf("unmarshal got %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &wantVal); err != nil {
		t.Fatalf("unmarshal want %s: %v", want, err)
	}
	gotJSON, _ := json.Marshal(gotVal)
	wantJSON, _ := json.Marshal(wantVal)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("value = %s, want %s", got, want)
	}
}

// TestPutIdemIfAbsent_SecondCallReturnsFirstResult is PRD §14.2(d)'s
// "a concurrent duplicate loses the race harmlessly": a second put
// under the same key must not overwrite the first — the first result
// is what every replay gets back.
func TestPutIdemIfAbsent_SecondCallReturnsFirstResult(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	jobID := seedJobForEvents(t)
	key := "test-key-" + jobID.String()

	first, err := testStore.PutIdemIfAbsent(ctx, key, jobID, json.RawMessage(`{"attempt":1}`))
	if err != nil {
		t.Fatalf("first PutIdemIfAbsent() error = %v", err)
	}

	second, err := testStore.PutIdemIfAbsent(ctx, key, jobID, json.RawMessage(`{"attempt":2}`))
	if err != nil {
		t.Fatalf("second PutIdemIfAbsent() error = %v", err)
	}

	if string(second) != string(first) {
		t.Errorf("second PutIdemIfAbsent() = %s, want the first call's result %s", second, first)
	}
}
