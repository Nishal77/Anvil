import { test, expect, type APIRequestContext } from "@playwright/test";

const API_BASE = process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080";

// There's no register/login UI yet (Phase 1 has none) — get a token the
// same way a curl-based smoke test would, then drive everything else
// through the real page.
async function registerUser(request: APIRequestContext): Promise<string> {
  const email = `e2e-${Date.now()}@example.com`;
  const res = await request.post(`${API_BASE}/auth/register`, {
    data: { email, password: "password123456" },
  });
  if (!res.ok()) {
    throw new Error(`register failed: ${res.status()} ${await res.text()}`);
  }
  const body = await res.json();
  return body.access_token as string;
}

test("golden path: submit a prompt, watch it run, job succeeds", async ({
  page,
  request,
}) => {
  const token = await registerUser(request);

  await page.goto("/");
  await page.getByLabel(/access token/i).fill(token);
  await page.getByLabel(/prompt/i).fill("build me a thing");
  await page.getByRole("button", { name: /submit/i }).click();

  await page.waitForURL(/\/jobs\/.+/);
  const jobID = page.url().split("/jobs/")[1];

  // The hardcoded plan's last step is "List app directory" (step_idx 2).
  // Waiting for its step_finished line in the live log is the UI-level
  // signal that the whole job actually finished, not just that the page
  // rendered.
  // Exact byte formatting of the payload isn't part of the API contract:
  // a live-broadcast event is Go's compact json.Marshal output, while one
  // replayed from the jsonb column comes back in Postgres's own canonical
  // form (spaced, keys reordered by length). Match on content, not bytes.
  await expect(page.locator("pre")).toContainText(/step_finished:.*"status":\s*"SUCCEEDED".*"step_idx":\s*2/, {
    timeout: 20_000,
  });

  // Cross-check against the API directly, since the log text alone
  // doesn't prove the job row itself reached a terminal state. There's a
  // brief real gap between the last step's event and the job row's own
  // transition to SUCCEEDED (the worker still has to tear down the
  // sandbox first), so poll rather than checking once.
  await expect
    .poll(
      async () => {
        const res = await request.get(`${API_BASE}/v1/jobs/${jobID}`, {
          headers: { Authorization: `Bearer ${token}` },
        });
        return (await res.json()).status;
      },
      { timeout: 10_000 },
    )
    .toBe("SUCCEEDED");
});
