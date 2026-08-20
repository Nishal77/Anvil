"use client";

import { useEffect, useRef, useState } from "react";
import { useParams } from "next/navigation";
import Link from "next/link";
import {
  API_BASE,
  getToken,
  getJob,
  approveJob,
  retryJob,
  cancelJob,
  artifactURL,
  usdFromMicros,
  type Job,
} from "@/lib/api";

// Every event_type the server actually emits — internal/agent/turn.go,
// executor.go, api/jobs.go, and events/subscriber.go's stream_gap.
// EventSource only delivers named events to a listener registered for
// that exact name, there's no wildcard, so this list has to stay in
// sync with the backend by hand (no shared schema yet).
const EVENT_TYPES = [
  "job_created",
  "job_cancelled",
  "job_deploying",
  "job_succeeded",
  "job_failed",
  "step_started",
  "step_finished",
  "stream_gap",
];

// Statuses with no dedicated terminal SSE event today (a plain,
// non-deploy job's RUNNING -> SUCCEEDED/FAILED transition is written
// by internal/queue's dispatcher directly, which has no publisher —
// see CLAUDE.md's package dependency graph) — polling is what makes
// this page correct for those regardless.
const POLL_INTERVAL_MS = 3000;

export default function JobPage() {
  const { id } = useParams<{ id: string }>();
  const [job, setJob] = useState<Job | null>(null);
  const [lines, setLines] = useState<string[]>([]);
  const [connected, setConnected] = useState(false);
  const [actionError, setActionError] = useState("");
  const [busy, setBusy] = useState(false);
  const logRef = useRef<HTMLPreElement>(null);

  useEffect(() => {
    let cancelled = false;
    async function load() {
      try {
        const data = await getJob(id);
        if (!cancelled) setJob(data);
      } catch {
        // Polling failure is transient (network blip, token expiry) —
        // the SSE log and the next poll tick are still live; nothing
        // here needs to interrupt the user.
      }
    }
    load();
    const interval = setInterval(load, POLL_INTERVAL_MS);
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
  }, [id]);

  useEffect(() => {
    const url = `${API_BASE}/v1/jobs/${id}/events?access_token=${encodeURIComponent(getToken())}`;
    const source = new EventSource(url);

    source.onopen = () => setConnected(true);
    source.onerror = () => setConnected(false);

    const append = (type: string) => (ev: MessageEvent) => {
      setLines((prev) => [...prev, `${type}: ${ev.data}`]);
    };
    for (const type of EVENT_TYPES) {
      source.addEventListener(type, append(type));
    }

    return () => source.close();
  }, [id]);

  useEffect(() => {
    logRef.current?.scrollTo(0, logRef.current.scrollHeight);
  }, [lines]);

  async function runAction(action: () => Promise<void>) {
    setActionError("");
    setBusy(true);
    try {
      await action();
      setJob(await getJob(id));
    } catch (err) {
      setActionError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <main>
      <p>
        <Link href="/jobs">&larr; all jobs</Link>
      </p>
      <h1>Job {id}</h1>
      <p>{connected ? "connected" : "disconnected — reconnecting"}</p>

      {job && <JobSummary job={job} />}

      {actionError && <pre style={{ color: "red" }}>{actionError}</pre>}

      {job?.status === "AWAITING_APPROVAL" && (
        <button disabled={busy} onClick={() => runAction(() => approveJob(id))}>
          Approve plan
        </button>
      )}
      {job?.status === "FAILED" && (
        <button disabled={busy} onClick={() => runAction(() => retryJob(id))}>
          Retry from failed step
        </button>
      )}
      {job && !["SUCCEEDED", "FAILED", "CANCELLED"].includes(job.status) && (
        <button disabled={busy} onClick={() => runAction(() => cancelJob(id))} style={{ marginLeft: "0.5rem" }}>
          Cancel
        </button>
      )}
      {job?.has_artifact && (
        <a href={artifactURL(id)} style={{ marginLeft: "0.5rem" }}>
          Download artifact
        </a>
      )}
      {job?.preview_url && (
        <a href={job.preview_url} target="_blank" rel="noreferrer" style={{ marginLeft: "0.5rem" }}>
          Open preview
        </a>
      )}

      <pre
        ref={logRef}
        style={{
          background: "#111",
          color: "#0f0",
          padding: "1rem",
          height: "60vh",
          overflowY: "auto",
          whiteSpace: "pre-wrap",
          marginTop: "1rem",
        }}
      >
        {lines.join("\n")}
      </pre>
    </main>
  );
}

function JobSummary({ job }: { job: Job }) {
  return (
    <dl>
      <dt>Status</dt>
      <dd>{job.status}</dd>

      {job.plan_summary && (
        <>
          <dt>Plan</dt>
          <dd>{job.plan_summary}</dd>
        </>
      )}

      {job.failure_reason && (
        <>
          <dt>Failure reason</dt>
          <dd style={{ color: "#c00" }}>{job.failure_reason}</dd>
        </>
      )}

      <dt>Cost</dt>
      <dd>
        {usdFromMicros(job.cost_usd_micros)} &middot; {job.tokens_used.toLocaleString()} /{" "}
        {job.token_budget.toLocaleString()} tokens
      </dd>
    </dl>
  );
}
