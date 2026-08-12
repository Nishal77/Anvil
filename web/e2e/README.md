# E2E tests

Golden-path test against the real product — real backend, real sandbox,
real Postgres, no mocks. Bring the whole stack up first:

```bash
# from the repo root
docker compose up -d                      # postgres, redis, minio
export DATABASE_URL="postgres://anvil:anvil@localhost:5432/anvil?sslmode=disable"
migrate -path migrations -database "$DATABASE_URL" up

export ANVIL_SANDBOX_IMAGE=anvil/workspace:test
go run ./cmd/runner &                     # sandbox Runner

export REDIS_URL=localhost:6379
export ANVIL_JWT_SECRET=$(openssl rand -hex 32)
export ANVIL_RUNNER_URL=http://127.0.0.1:9090
go run ./cmd/anvil &                      # control plane
```

Then, from `web/`:

```bash
npx playwright install --with-deps chromium   # once
npm run test:e2e
```

Playwright only starts the Next.js dev server for you (`playwright.config.ts`'s
`webServer`) — it can't start the Go processes or Docker containers above.
