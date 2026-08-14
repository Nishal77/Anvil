package agent

import "errors"

var (
	// ErrToolNotRegistered means a model-issued tool call named a tool
	// the Registry does not contain. Rule 1 of the policy engine
	// (PRD §16.3) — the model gets this back as a correctable
	// observation, never a crashed job.
	ErrToolNotRegistered = errors.New("agent: tool not registered")

	// ErrSchemaInvalid means a tool call's args failed JSON Schema
	// validation. Rule 2. Same treatment as ErrToolNotRegistered: a
	// correctable observation, not a failure.
	ErrSchemaInvalid = errors.New("agent: tool args failed schema validation")

	// ErrPathEscape means a fs_* path resolved (after symlink
	// resolution) outside the sandbox workspace. Rule 3 — the security
	// bug this week exists to close.
	ErrPathEscape = errors.New("agent: path escapes workspace")

	// ErrCommandBlocked means an exec command matched the blocklist.
	// Rule 4.
	ErrCommandBlocked = errors.New("agent: command blocked by policy")

	// ErrToolPrivileged means a PRIVILEGED tool was called without
	// create_repo set. Rule 5. Unreachable this week — no PRIVILEGED
	// tool is registered yet (git_push/github_open_pr land in Week 8)
	// — kept so PolicyEngine's rule set is complete now, not patched
	// in later.
	ErrToolPrivileged = errors.New("agent: privileged tool requires create_repo")

	// ErrStepTurnLimitExceeded means MAX_TURNS_PER_STEP was reached
	// without the model calling step_done. The step fails; this is
	// not the repair loop (Week 7) — it is the hard ceiling FR-021
	// exists to guarantee regardless of repair behavior.
	ErrStepTurnLimitExceeded = errors.New("agent: step turn limit exceeded")
)
