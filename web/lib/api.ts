// Talks to the control plane's HTTP API. There's no login screen yet
// (Phase 1 has no UI for that) — get a token with curl against
// /auth/register or /auth/login and paste it in on the home page; it's
// kept in localStorage from there.

export const API_BASE =
  process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080";

const TOKEN_KEY = "anvil_access_token";

export function getToken(): string {
  if (typeof window === "undefined") return "";
  return window.localStorage.getItem(TOKEN_KEY) ?? "";
}

export function setToken(token: string): void {
  window.localStorage.setItem(TOKEN_KEY, token);
}

// Job mirrors internal/api/jobs.go's jobResponse — every field the
// backend actually serializes, kept in sync by hand since there's no
// generated client yet (api/openapi.yaml exists but nothing consumes
// it on this side).
export interface Job {
  id: string;
  status: string;
  prompt: string;
  events_url?: string;
  failure_reason?: string;
  plan_summary?: string;
  preview_url?: string;
  has_artifact: boolean;
  token_budget: number;
  tokens_used: number;
  cost_usd_micros: number;
  // trace_id is absent for a job created with tracing disabled
  // (ANVIL_OTEL_COLLECTOR_ENDPOINT unset — see internal/telemetry's own
  // doc comment). Used to deep-link to this job's Grafana trace (EG-3,
  // PRD §17.1) — see traceExploreURL below.
  trace_id?: string;
}

// grafanaBaseURL is where this deployment's Grafana lives — unset
// disables the trace-ID deep link entirely rather than rendering a
// dead link, the same "optional, not a hard failure" pattern
// internal/config.Config uses for every other optional integration
// (S3, preview deploy, ...). Read at build/runtime via Next.js's
// standard NEXT_PUBLIC_ prefix, since this value is used in
// client-rendered components.
const grafanaBaseURL = process.env.NEXT_PUBLIC_GRAFANA_URL;

// traceExploreURL returns a link straight into Grafana's Tempo Explore
// view for traceID, or undefined if either traceID is empty (tracing
// was disabled when the job was created) or this deployment has no
// Grafana configured. The query param shape matches Grafana's Explore
// URL schema for a single-query, single-panel trace lookup against the
// datasource named "Tempo" (ops/grafana/provisioning/datasources/
// datasources.yml's fixed name for it).
export function traceExploreURL(traceID: string | undefined): string | undefined {
  if (!traceID || !grafanaBaseURL) {
    return undefined;
  }
  const query = {
    datasource: "Tempo",
    queries: [{ query: traceID, queryType: "traceql" }],
  };
  const params = new URLSearchParams({
    schemaVersion: "1",
    panes: JSON.stringify({ trace: query }),
    orgId: "1",
  });
  return `${grafanaBaseURL}/explore?${params.toString()}`;
}

class APIError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}

async function request<T>(
  path: string,
  init?: RequestInit,
): Promise<T> {
  const res = await fetch(`${API_BASE}${path}`, {
    ...init,
    headers: {
      ...(init?.body ? { "Content-Type": "application/json" } : {}),
      Authorization: `Bearer ${getToken()}`,
      ...init?.headers,
    },
  });
  if (!res.ok) {
    throw new APIError(res.status, `${path} failed: ${res.status} ${await res.text()}`);
  }
  if (res.status === 204) return undefined as T;
  return res.json();
}

export async function createJob(
  prompt: string,
  options?: { autoApprove?: boolean; createRepo?: boolean; deploy?: boolean },
): Promise<Job> {
  return request<Job>("/v1/jobs", {
    method: "POST",
    body: JSON.stringify({
      prompt,
      options: {
        auto_approve: options?.autoApprove ?? false,
        create_repo: options?.createRepo ?? false,
        deploy: options?.deploy ?? false,
      },
    }),
  });
}

export async function listJobs(): Promise<Job[]> {
  return request<Job[]>("/v1/jobs");
}

export async function getJob(id: string): Promise<Job> {
  return request<Job>(`/v1/jobs/${id}`);
}

export async function approveJob(id: string): Promise<void> {
  await request<void>(`/v1/jobs/${id}/approve`, { method: "POST" });
}

export async function retryJob(id: string): Promise<void> {
  await request<void>(`/v1/jobs/${id}/retry`, { method: "POST" });
}

export async function cancelJob(id: string): Promise<void> {
  await request<void>(`/v1/jobs/${id}/cancel`, { method: "POST" });
}

// artifactURL is a plain link, not a fetch call: GET .../artifact
// itself 302s to a presigned object-storage URL (PRD §11.2), so the
// browser follows it directly — no JS needed beyond setting href.
// Authorization travels as ?access_token= because a plain <a> can't
// set a header, the same accommodation the SSE log view needs.
export function artifactURL(id: string): string {
  return `${API_BASE}/v1/jobs/${id}/artifact?access_token=${encodeURIComponent(getToken())}`;
}

// usdFromMicros converts CostUSDMicros (USD millionths) to a display
// string, e.g. 61700 -> "$0.0617".
export function usdFromMicros(micros: number): string {
  return `$${(micros / 1_000_000).toFixed(4)}`;
}
