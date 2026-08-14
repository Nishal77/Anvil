package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/anvil-dev/anvil/internal/llm"
)

// PolicyDecision is Policy.Evaluate's verdict.
type PolicyDecision int

const (
	Allow PolicyDecision = iota
	Deny
	RequireApproval // reserved for a later Phase 2 milestone; unused this week
)

// String is the literal value persisted to agent_turns.policy_decision.
func (d PolicyDecision) String() string {
	switch d {
	case Allow:
		return "ALLOW"
	case Deny:
		return "DENY"
	case RequireApproval:
		return "REQUIRE_APPROVAL"
	default:
		return "UNKNOWN"
	}
}

// ToolCall is one model-issued invocation to evaluate and, if
// allowed, dispatch. SandboxID is required for the fs_* path-escape
// check (rule 3), which must resolve symlinks inside the sandbox's
// own filesystem, not the control plane's.
type ToolCall struct {
	Name      string
	Args      json.RawMessage
	SandboxID string
}

// Policy is declared at the consumer (CODE-STANDARDS §3.1) — Executor
// is the only caller.
type Policy interface {
	// Evaluate returns the decision and a human-and-model-readable
	// reason. The reason is always non-empty on Deny — it becomes the
	// model's correctable observation — and may be empty on Allow.
	Evaluate(ctx context.Context, jobID uuid.UUID, call ToolCall) (PolicyDecision, string)
}

// PolicyEngineConfig wires PolicyEngine's dependencies.
type PolicyEngineConfig struct {
	Registry *Registry
	Sandbox  sandboxClient
	Budget   llm.BudgetStore
	// CreateRepo mirrors job.options.create_repo (PRD §16.3 rule 5).
	// No PRIVILEGED tool is registered this week (git_push and
	// github_open_pr land in Week 8), so this is unreachable in
	// practice today — kept so the rule set is complete now rather
	// than patched in later.
	CreateRepo bool
}

func (c PolicyEngineConfig) validate() error {
	if c.Registry == nil {
		return fmt.Errorf("agent: policy engine config: Registry is required")
	}
	if c.Sandbox == nil {
		return fmt.Errorf("agent: policy engine config: Sandbox is required")
	}
	if c.Budget == nil {
		return fmt.Errorf("agent: policy engine config: Budget is required")
	}
	return nil
}

// PolicyEngine implements Policy as the 7 ordered, first-match-wins
// rules of PRD §16.3.
type PolicyEngine struct {
	cfg PolicyEngineConfig
}

// NewPolicyEngine constructs a PolicyEngine from cfg, or returns an
// error if cfg is invalid.
func NewPolicyEngine(cfg PolicyEngineConfig) (*PolicyEngine, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &PolicyEngine{cfg: cfg}, nil
}

// Evaluate runs call through all 7 rules in order, first match wins.
func (p *PolicyEngine) Evaluate(ctx context.Context, jobID uuid.UUID, call ToolCall) (PolicyDecision, string) {
	tool, registered := p.cfg.Registry.Lookup(call.Name) // rule 1
	if !registered {
		return Deny, fmt.Sprintf("tool %q is not registered", call.Name)
	}

	if err := p.cfg.Registry.ValidateArgs(call.Name, call.Args); err != nil { // rule 2
		return Deny, err.Error()
	}

	if strings.HasPrefix(call.Name, "fs_") { // rule 3
		if reason := p.evaluateFSPathEscape(ctx, call); reason != "" {
			return Deny, reason
		}
	}

	if call.Name == "exec" { // rule 4
		if reason := p.evaluateExecBlocklist(call); reason != "" {
			return Deny, reason
		}
	}

	if reason := p.evaluatePrivileged(tool); reason != "" { // rule 5
		return Deny, reason
	}

	if reason := p.evaluateBudget(ctx, jobID); reason != "" { // rule 6
		return Deny, reason
	}

	return Allow, "" // rule 7
}

func (p *PolicyEngine) evaluatePrivileged(tool Tool) string {
	if tool.PolicyClass == PolicyPrivileged && !p.cfg.CreateRepo {
		return fmt.Sprintf("tool %q is privileged and this job did not set create_repo", tool.Name)
	}
	return ""
}

func (p *PolicyEngine) evaluateBudget(ctx context.Context, jobID uuid.UUID) string {
	budget, err := p.cfg.Budget.GetJobBudget(ctx, jobID)
	if err != nil {
		return fmt.Sprintf("check job budget: %s", err.Error())
	}
	if budget.TokensUsed >= budget.TokenBudget {
		return "job token budget exceeded"
	}
	return ""
}

// evaluateFSPathEscape extracts the "path" argument common to every
// fs_* tool and resolves it inside the sandbox. Returns a non-empty
// deny reason on escape or resolution failure, empty on a clean
// resolution.
func (p *PolicyEngine) evaluateFSPathEscape(ctx context.Context, call ToolCall) string {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(call.Args, &in); err != nil || in.Path == "" {
		return "" // no path argument to check (e.g. fs_search with no path: defaults to workspace root, not an escape risk)
	}
	if _, err := resolveWorkspacePath(ctx, p.cfg.Sandbox, call.SandboxID, in.Path); err != nil {
		return err.Error()
	}
	return ""
}

// evaluateExecBlocklist checks an exec call's command against the
// blocklist and the credential-shaped-string heuristic.
func (p *PolicyEngine) evaluateExecBlocklist(call ToolCall) string {
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(call.Args, &in); err != nil {
		return "" // schema validation (rule 2) already caught a malformed exec call before this rule runs
	}
	if blocked, name := isBlockedCommand(in.Command); blocked {
		return fmt.Sprintf("command blocked: %s", name)
	}
	if flagged, _ := containsCredentialShapedString(in.Command); flagged {
		return "command contains a credential-shaped string"
	}
	return ""
}
