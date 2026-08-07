package telemetry

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestNewLogger_IncludesComponentField(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := NewLogger(&buf, "api")

	logger.Info("hello", slog.String("trace_id", "abc123"))

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("unmarshal log line: %v", err)
	}
	if record["component"] != "api" {
		t.Errorf("component = %v, want api", record["component"])
	}
	if record["trace_id"] != "abc123" {
		t.Errorf("trace_id = %v, want abc123", record["trace_id"])
	}
}
