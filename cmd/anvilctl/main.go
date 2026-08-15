// Command anvilctl is the developer CLI. Like every cmd/*/main.go, it
// contains only flag/env parsing, dependency construction, and
// wiring — no business logic (CLAUDE.md F8).
package main

import (
	"context"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: anvilctl <command> [flags]")
		os.Exit(1)
	}

	var err error
	switch os.Args[1] {
	case "bench":
		err = runBench(context.Background(), os.Args[2:])
	case "run":
		err = runRun(context.Background(), os.Args[2:])
	case "cancel":
		err = runCancel(context.Background(), os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "anvilctl:", err)
		os.Exit(1)
	}
}
