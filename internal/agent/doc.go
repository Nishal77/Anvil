// Package agent implements the Executor role of PRD §12.1: a per-step
// turn loop that calls an llm.Router with a fixed tool registry,
// evaluates every tool call against a policy engine before dispatch,
// persists the full audit trail to agent_turns, and runs side-effecting
// calls through an idempotency cache. Executor is the package's entry
// point; its RunStep method is what cmd/anvil wires into
// queue.Dispatcher.Config.RunStep.
package agent
