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

export interface CreateJobResponse {
  id: string;
  status: string;
  prompt: string;
  events_url: string;
}

export async function createJob(prompt: string): Promise<CreateJobResponse> {
  const res = await fetch(`${API_BASE}/v1/jobs`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${getToken()}`,
    },
    body: JSON.stringify({ prompt }),
  });
  if (!res.ok) {
    throw new Error(`create job failed: ${res.status} ${await res.text()}`);
  }
  return res.json();
}
