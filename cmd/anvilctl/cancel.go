package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
)

// runCancel implements `anvilctl cancel <job_id>` — PRD §13.3 step 1.
func runCancel(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	addr := fs.String("addr", "http://localhost:8080", "control plane address")
	email := fs.String("email", "", "account email (or ANVIL_EMAIL)")
	password := fs.String("password", "", "account password (or ANVIL_PASSWORD)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("anvilctl cancel: usage: anvilctl cancel <job_id>")
	}
	jobID := fs.Arg(0)

	client, err := newAPIClient(ctx, *addr, *email, *password)
	if err != nil {
		return err
	}

	status, err := client.do(ctx, http.MethodPost, "/v1/jobs/"+jobID+"/cancel", nil, nil)
	if err != nil {
		return fmt.Errorf("cancel job %s: %w", jobID, err)
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("cancel job %s: unexpected status %d", jobID, status)
	}
	fmt.Printf("job %s: cancel requested\n", jobID)
	return nil
}
