package agent

import (
	"context"
	"encoding/json"
	"testing"
)

func trivialTool(name string) Tool {
	return Tool{
		Name:        name,
		Description: "test tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}`),
		PolicyClass: PolicySafe,
		Handler:     func(context.Context, string, json.RawMessage) (string, error) { return "ok", nil },
	}
}

func TestNewRegistry_DuplicateNameRejected(t *testing.T) {
	_, err := NewRegistry(trivialTool("dup"), trivialTool("dup"))
	if err == nil {
		t.Fatal("NewRegistry() error = nil, want an error for a duplicate tool name")
	}
}

func TestNewRegistry_InvalidSchemaRejected(t *testing.T) {
	bad := trivialTool("bad")
	bad.InputSchema = json.RawMessage(`{not valid json`)
	if _, err := NewRegistry(bad); err == nil {
		t.Fatal("NewRegistry() error = nil, want an error for invalid JSON Schema")
	}
}

func TestRegistry_Lookup(t *testing.T) {
	r, err := NewRegistry(trivialTool("a"), trivialTool("b"))
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	if _, ok := r.Lookup("a"); !ok {
		t.Error("Lookup(a) ok = false, want true")
	}
	if _, ok := r.Lookup("missing"); ok {
		t.Error("Lookup(missing) ok = true, want false")
	}
}

func TestRegistry_ValidateArgs(t *testing.T) {
	r, err := NewRegistry(trivialTool("a"))
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	if err := r.ValidateArgs("a", json.RawMessage(`{"x":"hello"}`)); err != nil {
		t.Errorf("ValidateArgs() valid args error = %v, want nil", err)
	}
	if err := r.ValidateArgs("a", json.RawMessage(`{}`)); err == nil {
		t.Error("ValidateArgs() missing required field error = nil, want an error")
	}
	if err := r.ValidateArgs("a", json.RawMessage(`not json`)); err == nil {
		t.Error("ValidateArgs() malformed JSON error = nil, want an error")
	}
	if err := r.ValidateArgs("missing-tool", json.RawMessage(`{}`)); err == nil {
		t.Error("ValidateArgs() unregistered tool error = nil, want an error")
	}
}

func TestRegistry_ProviderTools_GeneratedFromRegistryAndSorted(t *testing.T) {
	r, err := NewRegistry(trivialTool("zebra"), trivialTool("apple"))
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	tools := r.ProviderTools()
	if len(tools) != 2 {
		t.Fatalf("ProviderTools() returned %d tools, want 2", len(tools))
	}
	if tools[0].Name != "apple" || tools[1].Name != "zebra" {
		t.Errorf("ProviderTools() order = [%s, %s], want deterministic sorted order [apple, zebra]", tools[0].Name, tools[1].Name)
	}
}

func TestNewFSTools_RegistersAllFour(t *testing.T) {
	sb := newFakeSandbox()
	tools := NewFSTools(sb)
	if len(tools) != 4 {
		t.Fatalf("NewFSTools() returned %d tools, want 4", len(tools))
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"fs_read", "fs_write", "fs_list", "fs_search"} {
		if !names[want] {
			t.Errorf("NewFSTools() missing %q", want)
		}
	}
}
