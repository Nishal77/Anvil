import { defineConfig } from "@playwright/test";

// This suite drives the real product against a real backend — it needs
// Postgres, Redis, the Runner, and the anvil control plane already
// running (see web/e2e/README.md). Playwright only starts the frontend
// dev server for you; it can't stand up the rest of that stack.
export default defineConfig({
  testDir: "./e2e",
  timeout: 30_000,
  fullyParallel: false,
  retries: 0,
  reporter: "list",
  use: {
    baseURL: "http://localhost:3000",
  },
  webServer: {
    command: "npm run dev",
    url: "http://localhost:3000",
    reuseExistingServer: true,
    timeout: 30_000,
  },
});
