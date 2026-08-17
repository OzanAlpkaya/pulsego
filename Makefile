# PulseGo. Developer tools are pinned as go.mod `tool` directives, so every
# target below works from a bare checkout with only Go and Docker installed.

COMPOSE := docker compose -f deployments/compose.yaml
GO      := go

# Local runs read .env if it exists; Compose supplies its own environment.
ifneq (,$(wildcard .env))
include .env
export
endif

.DEFAULT_GOAL := help
.PHONY: help bootstrap up down logs migrate-up migrate-down migrate-status \
        sqlc run-api run-worker test test-integration lint fmt vet vuln build \
        docker-build clean

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

run-worker: ## Run the worker locally
	$(GO) run ./cmd/worker

test: ## Run unit tests with the race detector
	$(GO) test -race ./...

test-integration: ## Run integration tests (requires a Docker daemon)
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

clean: ## Remove build artefacts
	rm -rf bin coverage.out
