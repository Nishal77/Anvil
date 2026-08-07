# Anvil — developer entry points.
#
# `make ci` is the contract: if it is green, the commit is acceptable. Run it
# before EVERY commit, not before every PR. A broken commit on main is a
# broken bisect six weeks from now.

SHELL           := /usr/bin/env bash
.SHELLFLAGS     := -eu -o pipefail -c
.DEFAULT_GOAL   := help
MAKEFLAGS       += --no-print-directory

GO              ?= go
MODULE          := github.com/anvil-dev/anvil
BIN_DIR         := bin
COVER_PROFILE   := coverage.out
COVER_MIN       := 70
CORE_PKGS       := ./internal/queue/... ./internal/agent/... ./internal/events/... ./internal/llm/...

VERSION         := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT          := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE      := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS         := -s -w \
                   -X $(MODULE)/internal/version.Version=$(VERSION) \
                   -X $(MODULE)/internal/version.Commit=$(COMMIT) \
                   -X $(MODULE)/internal/version.BuildDate=$(BUILD_DATE)

##@ General

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nAnvil\n\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
	@echo

##@ Development

.PHONY: dev
dev: ## Boot the local stack (postgres, redis, minio) and run migrations
	docker compose up -d postgres redis minio
	@./scripts/wait-for-healthy.sh postgres redis minio
	$(MAKE) migrate-up
	@echo "stack ready — postgres:5432 redis:6379 minio:9000"

.PHONY: dev-down
dev-down: ## Tear down the local stack
	docker compose down -v

.PHONY: run
run: ## Run the control plane against the local stack
	$(GO) run ./cmd/anvil

.PHONY: generate
generate: ## Regenerate sqlc queries, mocks, and the OpenAPI client
	$(GO) generate ./...
	@git diff --exit-code --quiet || \
		{ echo "generated files are stale — commit the regenerated output"; exit 1; }

##@ Build

.PHONY: build
build: ## Build all binaries into ./bin
	@mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/ ./cmd/...
	@ls -1 $(BIN_DIR)

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) $(COVER_PROFILE) coverage.html

##@ Quality  (all of these gate CI)

.PHONY: fmt
fmt: ## Format with gofumpt and goimports
	gofumpt -l -w .
	goimports -local $(MODULE) -w .

.PHONY: fmt-check
fmt-check: ## Verify formatting without modifying files
	@out=$$(gofumpt -l . | grep -v '^vendor/' || true); \
	if [ -n "$$out" ]; then echo "unformatted files:"; echo "$$out"; echo "run: make fmt"; exit 1; fi

.PHONY: vet
vet: ## go vet
	$(GO) vet ./...

.PHONY: lint
lint: ## golangci-lint
	golangci-lint run --timeout 5m

.PHONY: check-invariants
check-invariants: ## Structural guards for CLAUDE.md §3 invariants
	@./scripts/check-invariants.sh

.PHONY: vuln
vuln: ## Scan dependencies for known vulnerabilities
	govulncheck ./...

.PHONY: tidy-check
tidy-check: ## Fail if go.mod/go.sum are not tidy
	@cp go.mod go.mod.bak && cp go.sum go.sum.bak
	@$(GO) mod tidy
	@if ! diff -q go.mod go.mod.bak >/dev/null || ! diff -q go.sum go.sum.bak >/dev/null; then \
		mv go.mod.bak go.mod; mv go.sum.bak go.sum; \
		echo "go.mod/go.sum not tidy — run: go mod tidy"; exit 1; \
	fi
	@rm -f go.mod.bak go.sum.bak

##@ Testing

.PHONY: test
test: ## Unit tests
	$(GO) test -short ./...

.PHONY: test-race
test-race: ## Unit tests with the race detector (CI gate)
	$(GO) test -short -race -count=1 ./...

.PHONY: test-integration
test-integration: ## Integration tests against real Postgres and Redis (testcontainers)
	$(GO) test -race -count=1 -tags=integration -timeout 10m ./test/integration/...

.PHONY: coverage
coverage: ## Coverage report; fails below the threshold on core packages
	$(GO) test -short -coverprofile=$(COVER_PROFILE) -covermode=atomic $(CORE_PKGS)
	@$(GO) tool cover -func=$(COVER_PROFILE) | tail -1
	@pct=$$($(GO) tool cover -func=$(COVER_PROFILE) | awk '/^total:/ {gsub(/%/,"",$$3); print int($$3)}'); \
	if [ "$$pct" -lt "$(COVER_MIN)" ]; then \
		echo "coverage $$pct% is below the $(COVER_MIN)% minimum for core packages"; exit 1; \
	fi
	@$(GO) tool cover -html=$(COVER_PROFILE) -o coverage.html
	@echo "report: coverage.html"

.PHONY: chaos
chaos: ## Crash-recovery matrix, PRD §14.3  (Phase 4)
	$(GO) test -race -count=1 -tags=chaos -timeout 20m -v ./test/chaos/...

.PHONY: security-test
security-test: ## Sandbox escape suite, PRD §20.4  (Phase 4)
	$(GO) test -count=1 -tags=security -timeout 15m -v ./test/security/...

.PHONY: load
load: ## k6 load test against the SLOs in PRD §19  (Phase 4)
	k6 run test/load/main.js

.PHONY: bench
bench: ## LLM task benchmark suite, PRD §20.5  (Phase 2+)
	$(GO) run ./cmd/anvilctl bench --tasks benchmarks/tasks --out benchmarks/results.md

.PHONY: bench-queue
bench-queue: ## Queue throughput benchmark — produces the number cited in ADR-003
	$(GO) test -run=XXX -bench=BenchmarkClaim -benchtime=10s -count=5 ./internal/queue/...

##@ Database

.PHONY: migrate-up
migrate-up: ## Apply all pending migrations
	migrate -path migrations -database "$$DATABASE_URL" up

.PHONY: migrate-down
migrate-down: ## Roll back one migration
	migrate -path migrations -database "$$DATABASE_URL" down 1

.PHONY: migrate-new
migrate-new: ## Create a migration: make migrate-new NAME=add_foo
	@test -n "$(NAME)" || { echo "usage: make migrate-new NAME=add_foo"; exit 1; }
	migrate create -ext sql -dir migrations -seq $(NAME)

##@ CI

.PHONY: ci
ci: fmt-check vet lint check-invariants tidy-check test-race coverage vuln ## Everything CI runs — GREEN BEFORE EVERY COMMIT
	@echo
	@echo "  ✓ ci green — safe to commit"
	@echo

.PHONY: ci-full
ci-full: ci test-integration ## CI plus integration tests (slower; run before opening a PR)

.PHONY: tools
tools: ## Install the required developer tooling
	$(GO) install mvdan.cc/gofumpt@latest
	$(GO) install golang.org/x/tools/cmd/goimports@latest
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	$(GO) install golang.org/x/vuln/cmd/govulncheck@latest
	$(GO) install github.com/golang-migrate/migrate/v4/cmd/migrate@latest
	$(GO) install github.com/sqlc-dev/sqlc/cmd/sqlc@latest