// Package executor runs a job's plan inside a sandbox and reports
// progress as events.
//
// Right now that plan is a fixed list of three shell commands, not
// anything an LLM produced — there's no planner yet. Executor.RunStep
// matches the signature queue.Dispatcher expects for its Config.RunStep
// field, so it plugs straight into the queue's worker loop with no
// adapter needed.
package executor
