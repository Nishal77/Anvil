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

	// ErrToolPrivileged means a PRIVILEGED tool (git_push,
	// github_open_pr) was called without create_repo set. Rule 5.
	ErrToolPrivileged = errors.New("agent: privileged tool requires create_repo")

	// ErrStepTurnLimitExceeded means MAX_TURNS_PER_STEP was reached
	// without the model calling step_done. The step fails; this is
	// not the repair loop (Week 7) — it is the hard ceiling FR-021
	// exists to guarantee regardless of repair behavior.
	ErrStepTurnLimitExceeded = errors.New("agent: step turn limit exceeded")

	// ErrPlannerDidNotCallTool means the planning request returned
	// without a submit_plan tool call — the only legal outcome of a
	// planning turn. Structured output is required, never text parsed
	// out of prose (FR-020).
	ErrPlannerDidNotCallTool = errors.New("agent: planner did not call submit_plan")

	// ErrPlanInvalid means the model's plan failed a code-enforced
	// check: zero steps, or more steps than MaxSteps. Never trust the
	// model to have respected a limit stated only in the prompt.
	ErrPlanInvalid = errors.New("agent: plan invalid")

	// ErrRepairCapExceeded means a step exhausted its repair budget
	// (FR-022) — the step ends (FAILED, or SKIPPED if Optional), the
	// job does not hang waiting for a repair that will never land.
	ErrRepairCapExceeded = errors.New("agent: repair cap exceeded")

	// ErrCancelled means the job's cancellation was observed between
	// turns (PRD §13.3 step 2) — a normal, cooperative stop, not a
	// crash.
	ErrCancelled = errors.New("agent: job cancelled")
)
