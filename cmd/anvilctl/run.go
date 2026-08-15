package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"time"
)

const jobPollInterval = 2 * time.Second

type jobResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// terminalStatuses are the job_status values RunStep never transitions
// out of (PRD §13.1) — the poll loop below stops on any of them.
var terminalStatuses = map[string]bool{
	"SUCCEEDED": true, "FAILED": true, "CANCELLED": true,
}

// runRun implements `anvilctl run` — G2-1's fully-autonomous-run
// command (gate-2.md), and G2-2 without --auto-approve. Submits the
// job, then polls until it reaches a terminal status, printing every
// status change.
func runRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	addr := fs.String("addr", "http://localhost:8080", "control plane address")
	prompt := fs.String("prompt", "", "job prompt (required)")
	autoApprove := fs.Bool("auto-approve", false, "skip the approval gate")
	email := fs.String("email", "", "account email (or ANVIL_EMAIL)")
	password := fs.String("password", "", "account password (or ANVIL_PASSWORD)")
	wait := fs.Bool("wait", true, "poll until the job reaches a terminal status")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if *prompt == "" {
		return fmt.Errorf("anvilctl run: --prompt is required")
	}

	client, err := newAPIClient(ctx, *addr, *email, *password)
	if err != nil {
		return err
	}

	var job jobResponse
	status, err := client.do(ctx, http.MethodPost, "/v1/jobs", map[string]any{
		"prompt":  *prompt,
		"options": map[string]any{"auto_approve": *autoApprove},
	}, &job)
	if err != nil {
		return fmt.Errorf("create job: %w", err)
	}
	if status != http.StatusAccepted {
		return fmt.Errorf("create job: unexpected status %d", status)
	}
	fmt.Printf("job %s created, status %s\n", job.ID, job.Status)

	if !*wait {
		return nil
	}
	return pollUntilTerminal(ctx, client, job.ID)
}

// pollUntilTerminal polls GET /v1/jobs/{id} until status is terminal,
// printing each change it observes — the visible progress `anvilctl
// run` gives an operator without needing to open a second terminal for
// the SSE stream.
func pollUntilTerminal(ctx context.Context, client *apiClient, jobID string) error {
	last := ""
	ticker := time.NewTicker(jobPollInterval)
	defer ticker.Stop()

	for {
		var job jobResponse
		if _, err := client.do(ctx, http.MethodGet, "/v1/jobs/"+jobID, nil, &job); err != nil {
			return fmt.Errorf("poll job %s: %w", jobID, err)
		}
		if job.Status != last {
			fmt.Printf("job %s: %s\n", jobID, job.Status)
			last = job.Status
		}
		if terminalStatuses[job.Status] {
			fmt.Printf("artifact: %s/v1/jobs/%s/artifact (Authorization: Bearer <token>)\n", client.addr, jobID)
			if job.Status != "SUCCEEDED" {
				return fmt.Errorf("job %s ended in %s", jobID, job.Status)
			}
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("poll job %s: %w", jobID, ctx.Err())
		case <-ticker.C:
		}
	}
}
