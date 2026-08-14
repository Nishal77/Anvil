package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/google/uuid"
)

// IdemStore persists idempotency results, backed by the existing
// idempotency_keys table (migration 006). Declared at the consumer;
// the storage-backed implementation is wired at Executor construction.
type IdemStore interface {
	GetIdem(ctx context.Context, key string) (result json.RawMessage, ok bool, err error)
	// PutIdemIfAbsent inserts result under key iff absent, and always
	// returns the row's actual stored value — the caller's own result
	// on a fresh insert, or the winner's on a lost race against a
	// concurrent duplicate call (PRD §14.2(d): "a concurrent duplicate
	// loses the race harmlessly").
	PutIdemIfAbsent(ctx context.Context, key string, jobID uuid.UUID, result json.RawMessage) (json.RawMessage, error)
}

// callIdempotent wraps one side-effecting tool call: a cache hit skips
// fn entirely; a miss runs fn and stores the result. fn's error is
// deliberately never cached — a transient failure (network blip) must
// be retryable on the next attempt; only a genuine success is
// idempotent-safe to replay (PRD §14.2(d)).
func callIdempotent(
	ctx context.Context, store IdemStore,
	jobID, stepID uuid.UUID, tool string, args json.RawMessage,
	fn func() (json.RawMessage, error),
) (json.RawMessage, error) {
	key, err := idemKey(jobID, stepID, tool, args)
	if err != nil {
		return nil, fmt.Errorf("agent: call idempotent: %w", err)
	}

	if cached, ok, err := store.GetIdem(ctx, key); err != nil {
		return nil, fmt.Errorf("agent: call idempotent: get: %w", err)
	} else if ok {
		idemHitsTotal.WithLabelValues(tool).Inc()
		return cached, nil
	}
	idemMissesTotal.WithLabelValues(tool).Inc()

	res, err := fn()
	if err != nil {
		return nil, err // deliberately not cached: transient errors must retry
	}

	stored, err := store.PutIdemIfAbsent(ctx, key, jobID, res)
	if err != nil {
		return nil, fmt.Errorf("agent: call idempotent: put: %w", err)
	}
	return stored, nil
}

// idemKey computes sha256(job_id || step_id || tool || canonical(args))
// as lowercase hex. canonical recursively sorts JSON object keys
// before hashing: encoding/json's map iteration order is randomized
// per-process, so hashing a naively re-marshaled map would produce a
// different key for logically identical args on every run.
func idemKey(jobID, stepID uuid.UUID, tool string, args json.RawMessage) (string, error) {
	canonical, err := canonicalJSON(args)
	if err != nil {
		return "", fmt.Errorf("canonicalize args: %w", err)
	}

	h := sha256.New()
	h.Write([]byte(jobID.String()))
	h.Write([]byte(stepID.String()))
	h.Write([]byte(tool))
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// canonicalJSON re-serializes data with every object's keys sorted,
// recursively. Used only for idempotency-key hashing — never for the
// value stored or returned to the model.
func canonicalJSON(data json.RawMessage) ([]byte, error) {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	out, err := json.Marshal(canonicalize(v))
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	return out, nil
}

// canonicalize walks a decoded JSON value, turning every map into a
// sortedMap so json.Marshal emits its keys in sorted order.
func canonicalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pairs := make([]sortedField, len(keys))
		for i, k := range keys {
			pairs[i] = sortedField{Key: k, Value: canonicalize(t[k])}
		}
		return sortedMap(pairs)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = canonicalize(e)
		}
		return out
	default:
		return v
	}
}

type sortedField struct {
	Key   string
	Value any
}

// sortedMap marshals as a JSON object with its fields in the order
// they appear in the slice — which canonicalize has already sorted by
// key, giving a byte-stable encoding regardless of the source map's
// (randomized) iteration order.
type sortedMap []sortedField

func (m sortedMap) MarshalJSON() ([]byte, error) {
	buf := make([]byte, 0, 64)
	buf = append(buf, '{')
	for i, f := range m {
		if i > 0 {
			buf = append(buf, ',')
		}
		key, err := json.Marshal(f.Key)
		if err != nil {
			return nil, fmt.Errorf("marshal key %q: %w", f.Key, err)
		}
		val, err := json.Marshal(f.Value)
		if err != nil {
			return nil, fmt.Errorf("marshal value for key %q: %w", f.Key, err)
		}
		buf = append(buf, key...)
		buf = append(buf, ':')
		buf = append(buf, val...)
	}
	buf = append(buf, '}')
	return buf, nil
}
