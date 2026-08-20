"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { listJobs, usdFromMicros, type Job } from "@/lib/api";

const STATUS_COLORS: Record<string, string> = {
  SUCCEEDED: "#0a0",
  FAILED: "#c00",
  CANCELLED: "#888",
  RUNNING: "#06c",
  DEPLOYING: "#06c",
  PLANNING: "#a60",
  QUEUED: "#a60",
  AWAITING_APPROVAL: "#a60",
  PENDING_PLAN: "#a60",
};

export default function JobsPage() {
  const [jobs, setJobs] = useState<Job[] | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    async function load() {
      try {
        const data = await listJobs();
        if (!cancelled) setJobs(data);
      } catch (err) {
        if (!cancelled) setError(err instanceof Error ? err.message : String(err));
      }
    }
    load();
    // Plain jobs (no deploy) currently emit no terminal SSE event of
    // their own (only step_started/step_finished and, for a job that
    // requested a preview, job_deploying/job_succeeded/job_failed) —
    // polling here is what keeps this list's status column honest for
    // every job, not just deployed ones.
    const interval = setInterval(load, 3000);
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
  }, []);

  return (
    <main>
      <p>
        <Link href="/">&larr; new job</Link>
      </p>
      <h1>Jobs</h1>
      {error && <pre style={{ color: "red" }}>{error}</pre>}
      {jobs === null && !error && <p>Loading…</p>}
      {jobs !== null && jobs.length === 0 && <p>No jobs yet.</p>}
      {jobs !== null && jobs.length > 0 && (
        <table style={{ width: "100%", borderCollapse: "collapse" }}>
          <thead>
            <tr style={{ textAlign: "left", borderBottom: "1px solid #ccc" }}>
              <th>Prompt</th>
              <th>Status</th>
              <th>Cost</th>
            </tr>
          </thead>
          <tbody>
            {jobs.map((job) => (
              <tr key={job.id} style={{ borderBottom: "1px solid #eee" }}>
                <td style={{ maxWidth: "40ch", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                  <Link href={`/jobs/${job.id}`}>{job.prompt}</Link>
                </td>
                <td style={{ color: STATUS_COLORS[job.status] ?? "inherit" }}>{job.status}</td>
                <td>{usdFromMicros(job.cost_usd_micros)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </main>
  );
}
