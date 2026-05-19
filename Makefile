# Makefile — one-liner entry points for the dev/test workflow.
#
# Defaults assume the local test database from `make db-up` (port 5433,
# postgres/postgres/payroll_db). Override DATABASE_URL on the command line
# to point at a different target.

SHELL := /bin/bash

DATABASE_URL ?= postgres://postgres:postgres@localhost:5433/payroll_db?sslmode=disable
MIGRATIONS_DIR := internal/db/migrations
MIGRATE_IMAGE := migrate/migrate

# Pretty banner for the help target.
.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# ──────────────────────────── Database ─────────────────────────────

.PHONY: db-up
db-up: ## Start the test Postgres container (port 5433).
	@docker start payroll_db_test 2>/dev/null || docker run -d --name payroll_db_test \
		-e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=postgres \
		-e POSTGRES_DB=payroll_db -p 5433:5432 \
		-v "$(PWD)/config/postgres-init.sql:/docker-entrypoint-initdb.d/init.sql:ro" \
		postgres:16-alpine
	@until docker exec payroll_db_test pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done
	@echo "postgres ready on :5433"

.PHONY: db-down
db-down: ## Stop the test Postgres container.
	@docker stop payroll_db_test 2>/dev/null || true

.PHONY: db-nuke
db-nuke: ## Destroy the test Postgres container + volume (full reset).
	@docker rm -f payroll_db_test 2>/dev/null || true
	@docker volume rm payroll_db_test 2>/dev/null || true

.PHONY: migrate-up
migrate-up: ## Apply all pending migrations to DATABASE_URL.
	@docker run --rm --network host \
		-v "$(PWD)/$(MIGRATIONS_DIR):/migrations" \
		$(MIGRATE_IMAGE) \
		-path=/migrations -database "$(DATABASE_URL)" up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent migration.
	@docker run --rm --network host \
		-v "$(PWD)/$(MIGRATIONS_DIR):/migrations" \
		$(MIGRATE_IMAGE) \
		-path=/migrations -database "$(DATABASE_URL)" down 1

# ──────────────────────────── Tests ─────────────────────────────

.PHONY: test
test: ## Run unit tests only.
	@go test -count=1 ./...

.PHONY: test-integration
test-integration: ## Run unit + integration tests against DATABASE_URL.
	@DATABASE_URL='host=localhost user=postgres password=postgres dbname=payroll_db port=5433 sslmode=disable' \
	 MIGRATIONS_PATH='file://internal/db/migrations' \
	 go test -tags=integration -count=1 ./...

.PHONY: test-race
test-race: ## Run unit + integration tests with the race detector.
	@DATABASE_URL='host=localhost user=postgres password=postgres dbname=payroll_db port=5433 sslmode=disable' \
	 MIGRATIONS_PATH='file://internal/db/migrations' \
	 go test -tags=integration -race -count=1 ./...

.PHONY: cover
cover: ## Print aggregate coverage percentage.
	@DATABASE_URL='host=localhost user=postgres password=postgres dbname=payroll_db port=5433 sslmode=disable' \
	 MIGRATIONS_PATH='file://internal/db/migrations' \
	 go test -tags=integration -count=1 -coverprofile=/tmp/cov.out ./... >/dev/null
	@go tool cover -func=/tmp/cov.out | tail -1

# ──────────────────────────── Build / Lint ─────────────────────────────

.PHONY: build
build: ## Build the api binary.
	@go build -o /tmp/go-payroll-engine ./cmd/api && echo "binary at /tmp/go-payroll-engine"

.PHONY: vet
vet: ## go vet ./...
	@go vet ./...

.PHONY: lint
lint: ## golangci-lint run (requires golangci-lint installed locally).
	@golangci-lint run

# ──────────────────────────── Docker Compose stack ─────────────────────────────

.PHONY: compose-up
compose-up: ## Bring up the full docker-compose stack (api, worker, db, redis, observability).
	@docker-compose up -d --build
	@echo "Waiting for db to become healthy..."
	@until [ "$$(docker inspect -f '{{.State.Health.Status}}' payroll_db 2>/dev/null)" = "healthy" ]; do sleep 1; done
	@echo "Waiting for API on :28080..."
	@until curl -fs http://localhost:28080/healthz >/dev/null 2>&1; do sleep 1; done
	@echo "API up. Try: curl http://localhost:28080/readyz"

.PHONY: compose-down
compose-down: ## Stop the compose stack (keeps volumes — data persists).
	@docker-compose down

.PHONY: compose-fresh
compose-fresh: ## Tear down compose AND drop volumes, then bring it back up. Use when you change .env or init SQL.
	@docker-compose down -v
	@docker rm -f payroll_db payroll_redis payroll_api payroll_worker \
		payroll_prometheus payroll_grafana \
		payroll_postgres_exporter payroll_redis_exporter 2>/dev/null || true
	@$(MAKE) compose-up

.PHONY: compose-logs
compose-logs: ## Follow api + worker logs.
	@docker-compose logs -f api worker

.PHONY: compose-creds
compose-creds: ## Print the credentials this compose stack is using (from .env).
	@test -f .env || { echo "no .env file — run: cp .env.example .env, then edit"; exit 1; }
	@echo "── compose credentials (from .env) ───────────────────────────"
	@awk -F= '\
	    $$1 == "POSTGRES_USER"          {print "postgres superuser : " $$2} \
	    $$1 == "POSTGRES_PASSWORD"      {print "  password         : " $$2} \
	    $$1 == "POSTGRES_APP_PASSWORD"  {print "payroll_app role   : payroll_app / " $$2} \
	    $$1 == "POSTGRES_DB"            {print "database           : " $$2} \
	    $$1 == "REDIS_PASSWORD"         {print "redis password     : " $$2} \
	    $$1 == "GRAFANA_PASSWORD"       {print "grafana admin pw   : " $$2} \
	' .env
	@echo "──────────────────────────────────────────────────────────────"
	@echo "psql:  PGPASSWORD=<pw> docker exec -it payroll_db psql -U payroll_app -d payroll_db"

# ──────────────────────────── Full bootstrap ─────────────────────────────

.PHONY: bootstrap
bootstrap: db-up migrate-up ## Start the standalone test DB and apply migrations from scratch.
	@echo "ready: make test-integration"
