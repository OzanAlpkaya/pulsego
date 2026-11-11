# PulseGo. Developer tools are pinned as go.mod `tool` directives, so every
# target below works from a bare checkout with only Go and Docker installed.

COMPOSE := docker compose -f deployments/compose.yaml
GO      := go

# Local runs read .env if it exists; Compose supplies its own environment.
# A variable already set in the environment (`VAR=value make ...`) wins over
# its .env line, as do make arguments (`make ... VAR=value`), so a one-off
# value on the command line is never silently replaced by .env. Environment
# values are saved before the include and restored, unexpanded, after it.
ifneq (,$(wildcard .env))
DOTENV_KEYS := $(shell sed -n 's/^[[:space:]]*\([A-Za-z_][A-Za-z0-9_]*\)[[:space:]]*=.*/\1/p' .env)
DOTENV_FROM_ENV := $(foreach v,$(DOTENV_KEYS),$(if $(filter environment,$(origin $(v))),$(v)))
$(foreach v,$(DOTENV_FROM_ENV),$(eval DOTENV_SAVED_$(v) := $$(value $(v))))
include .env
$(foreach v,$(DOTENV_FROM_ENV),$(eval $(v) := $$(DOTENV_SAVED_$(v))))
export
endif

.DEFAULT_GOAL := help
.PHONY: help bootstrap up down logs migrate-up migrate-down migrate-status \
        sqlc run-api run-worker test test-integration lint fmt vet vuln build \
        docker-build clean demo-seed demo

help: ## List available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

bootstrap: ## Download modules and verify the pinned tools resolve
	$(GO) mod download
	$(GO) tool sqlc version
	$(GO) tool golangci-lint version
	@test -f .env || (cp .env.example .env && echo "created .env from .env.example")

up: ## Start postgres, run migrations, then start api and worker
	$(COMPOSE) up --build -d
	@echo "api:      http://localhost:8080/healthz"
	@echo "internal: http://localhost:9090/healthz"

down: ## Stop the stack and remove its volumes
	$(COMPOSE) down -v

logs: ## Follow the api and worker logs
	$(COMPOSE) logs -f api worker

migrate-up: ## Apply all pending migrations
	$(GO) run ./cmd/migrate up

migrate-down: ## Roll back the most recent migration
	$(GO) run ./cmd/migrate down

migrate-status: ## Show which migrations have been applied
	$(GO) run ./cmd/migrate status

sqlc: ## Regenerate the type-safe query code from db/queries
	$(GO) tool sqlc generate

run-api: ## Run the API locally
	$(GO) run ./cmd/api

# The worker's internal listener moves to :9091 so it does not collide with a
# local api on :9090.
run-worker: ## Run the worker locally (internal listener on :9091)
	INTERNAL_HTTP_ADDR=:9091 $(GO) run ./cmd/worker

test: ## Run unit tests with the race detector
	$(GO) test -race ./...

# Uses the PostgreSQL server in PULSEGO_TEST_DATABASE_URL when it is set
# (creating and dropping its own pulsego_test_* databases); otherwise starts
# PostgreSQL through Docker/Testcontainers.
test-integration: ## Run integration tests (PULSEGO_TEST_DATABASE_URL, else Docker)
	$(GO) test -race -tags integration -timeout 15m ./...

lint: ## Run golangci-lint
	$(GO) tool golangci-lint run

fmt: ## Format all Go source
	gofmt -w .

vet: ## Run go vet
	$(GO) vet ./...

vuln: ## Scan dependencies for known vulnerabilities
	$(GO) tool govulncheck ./...

build: ## Build both service binaries into bin/
	$(GO) build -o bin/api ./cmd/api
	$(GO) build -o bin/worker ./cmd/worker
	$(GO) build -o bin/migrate ./cmd/migrate

docker-build: ## Build all three container images
	docker build --build-arg BINARY=api     -t pulsego-api:local     -f deployments/Dockerfile .
	docker build --build-arg BINARY=worker  -t pulsego-worker:local  -f deployments/Dockerfile .
	docker build --build-arg BINARY=migrate -t pulsego-migrate:local -f deployments/Dockerfile .

# Seeds demo monitors into a running api (API_KEY required; API_URL defaults to
# http://localhost:8080; TARGET_BASE points at demo/cmd/targets, else public
# endpoints are used). Re-runs skip monitors that already exist.
demo-seed: ## Seed demo monitors into a running api (API_KEY, API_URL, TARGET_BASE)
	scripts/seed-demo.sh

# Local development only: starts its own api and worker against DATABASE_URL
# and rewrites docs/screenshots/*.png and docs/demo.gif. The demo targets are
# on 127.0.0.1, so the caller opts out of SSRF protection explicitly; this
# target never does it for you, and scripts/demo.sh refuses to start without
# both variables:
#   ENVIRONMENT=development ALLOW_PRIVATE_TARGETS=true make demo
demo: ## Regenerate screenshots and demo GIF; caller passes ENVIRONMENT=development ALLOW_PRIVATE_TARGETS=true (local only)
	scripts/demo.sh

clean: ## Remove build artefacts
	rm -rf bin coverage.out
