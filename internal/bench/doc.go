// Package bench drives the PRD §20.5 task benchmark suite: for each
// task, one placeholder-prompt completion via llm.Router asks the
// model to write files with a single write_file tool, applies those
// files to a fresh sandbox, and runs the task's pass/fail check.
// Runner is the package's entry point; WriteResultsMarkdown renders
// the results table cmd/anvilctl's bench subcommand appends to
// benchmarks/results.md.
package bench
