package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/anvil-dev/anvil/internal/llm"
)

// PolicyClass is a tool's risk tier — rule 5 of the policy engine
// (PRD §16.3) denies a PRIVILEGED call unless the job opted into
// create_repo.
type PolicyClass int

const (
	PolicySafe PolicyClass = iota
	PolicyGuarded
	PolicyPrivileged
)

// Handler executes one validated tool call inside sandboxID, on behalf
// of userID (the job owner — needed by tools that resolve one of that
// user's stored secrets, e.g. git_push's GitHub token, SEC-020), and
// returns the observation text fed back to the model. A non-nil error
// means the harness itself failed (sandbox gone, exec timeout) — a
// tool-level failure (e.g. exec exit 1) belongs IN the observation
// string, not here, so the model sees it as a normal turn result it
// can act on.
type Handler func(ctx context.Context, sandboxID string, userID uuid.UUID, args json.RawMessage) (string, error)

// Tool is one entry in the Registry: schema, risk class, and
// implementation together, so ProviderTools generates the
// provider-facing schema FROM this — never a hand-duplicated second
// copy that drifts.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage // JSON Schema (2020-12), compiled once at Registry construction
	PolicyClass PolicyClass
	Handler     Handler
}

// Registry holds exactly the tool set an agent may call — fixed at
// construction, never mutated at runtime. This is §16.4's capability
// scoping: a perfect prompt injection still only reaches these tools.
type Registry struct {
	tools   map[string]Tool
	schemas map[string]*jsonschema.Schema
}

// NewRegistry compiles every tool's InputSchema and returns an error
// if any is invalid JSON Schema or a name is duplicated — caught at
// startup, not on the first call.
func NewRegistry(tools ...Tool) (*Registry, error) {
	r := &Registry{
		tools:   make(map[string]Tool, len(tools)),
		schemas: make(map[string]*jsonschema.Schema, len(tools)),
	}
	compiler := jsonschema.NewCompiler()

	for _, t := range tools {
		if _, exists := r.tools[t.Name]; exists {
			return nil, fmt.Errorf("agent: new registry: duplicate tool name %q", t.Name)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(t.InputSchema))
		if err != nil {
			return nil, fmt.Errorf("agent: new registry: tool %q: parse schema: %w", t.Name, err)
		}
		url := "mem://agent/tool/" + t.Name
		if err := compiler.AddResource(url, doc); err != nil {
			return nil, fmt.Errorf("agent: new registry: tool %q: add schema resource: %w", t.Name, err)
		}
		schema, err := compiler.Compile(url)
		if err != nil {
			return nil, fmt.Errorf("agent: new registry: tool %q: compile schema: %w", t.Name, err)
		}
		r.tools[t.Name] = t
		r.schemas[t.Name] = schema
	}
	return r, nil
}

// Lookup returns the tool named name, or ok=false — rule 1.
func (r *Registry) Lookup(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// ValidateArgs runs args through name's compiled JSON Schema — rule 2.
// Returns ErrSchemaInvalid wrapping the validator's own message when
// invalid; that message is what becomes the model's correctable
// observation, never a Go error surfaced to the job.
func (r *Registry) ValidateArgs(name string, args json.RawMessage) error {
	schema, ok := r.schemas[name]
	if !ok {
		return fmt.Errorf("agent: validate args: %w: %s", ErrToolNotRegistered, name)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(args))
	if err != nil {
		return fmt.Errorf("agent: validate args: %w: %s: args are not valid JSON: %s", ErrSchemaInvalid, name, err.Error())
	}
	if err := schema.Validate(instance); err != nil {
		return fmt.Errorf("agent: validate args: %w: %s: %s", ErrSchemaInvalid, name, err.Error())
	}
	return nil
}

// ProviderTools generates the provider-facing tool definitions from
// the registry — the single source of truth llm.Request.Tools is
// built from, so the Go registry and the LLM's tool schema can never
// drift apart.
func (r *Registry) ProviderTools() []llm.Tool {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic order: stable test fixtures and provider-side prompt caching both want it

	tools := make([]llm.Tool, 0, len(names))
	for _, name := range names {
		t := r.tools[name]
		tools = append(tools, llm.Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return tools
}
