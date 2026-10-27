SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

COMPOSE := docker compose
SERVICES := catalog booking notification

# Local ports used by `make run-*` (each service needs its own).
CATALOG_HTTP := :3002
CATALOG_ADMIN := :9092
CATALOG_GRPC := :50051
BOOKING_HTTP := :3001
BOOKING_ADMIN := :9091
NOTIFICATION_ADMIN := :9093

TEST_DATABASE_URL ?= postgres://chore:chore@localhost:5435/chore_test?sslmode=disable

.PHONY: help
help: ## Show available targets
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

## ---------------------------------------------------------------- development
.PHONY: up
up: ## Build images and start the full stack (services + observability)
	$(COMPOSE) up --build -d

.PHONY: up-infra
up-infra: ## Start only Postgres and RabbitMQ (for local `go run`)
	$(COMPOSE) up -d postgres rabbitmq

.PHONY: down
down: ## Stop the stack and remove containers
	$(COMPOSE) down

.PHONY: logs
logs: ## Tail logs from every service
	$(COMPOSE) logs -f --tail=100

.PHONY: ps
ps: ## Show stack status
	$(COMPOSE) ps

.PHONY: run-catalog
run-catalog: ## Run the catalog service locally
	HTTP_ADDR=$(CATALOG_HTTP) ADMIN_ADDR=$(CATALOG_ADMIN) GRPC_ADDR=$(CATALOG_GRPC) go run ./cmd/catalog

.PHONY: run-booking
run-booking: ## Run the booking service locally
	HTTP_ADDR=$(BOOKING_HTTP) ADMIN_ADDR=$(BOOKING_ADMIN) go run ./cmd/booking

.PHONY: run-notification
run-notification: ## Run the notification consumer locally
	ADMIN_ADDR=$(NOTIFICATION_ADMIN) go run ./cmd/notification

## --------------------------------------------------------------------- build
.PHONY: build
build: ## Compile every binary into ./bin
	@mkdir -p bin
	@for service in $(SERVICES); do \
		echo "building $$service"; \
		CGO_ENABLED=0 go build -trimpath -o bin/$$service ./cmd/$$service; \
	done

.PHONY: proto
proto: ## Regenerate Go code from proto definitions
	buf lint
	buf generate

.PHONY: tools
tools: ## Install code generation tools
	go install github.com/bufbuild/buf/cmd/buf@latest
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

## --------------------------------------------------------------------- tests
.PHONY: test
test: ## Run unit tests with the race detector
	go test -race -count=1 ./...

.PHONY: test-integration
test-integration: ## Run tests against a real Postgres (needs `make up-infra`)
	@docker compose exec -T postgres psql -U chore -d chore -c 'CREATE DATABASE chore_test' >/dev/null 2>&1 || true
	TEST_DATABASE_URL=$(TEST_DATABASE_URL) go test -race -count=1 -tags=integration ./...

.PHONY: cover
cover: ## Produce a coverage profile and print the total
	go test -race -count=1 -covermode=atomic -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: e2e
e2e: ## Run the HTTP e2e flow against a running stack
	./scripts/e2e.sh

## -------------------------------------------------------------------- quality
.PHONY: fmt
fmt: ## Format Go code
	gofmt -w $$(find . -name '*.go' -not -path './internal/gen/*')

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint (installed separately)
	golangci-lint run ./...

.PHONY: tidy
tidy: ## Tidy go.mod/go.sum
	go mod tidy

.PHONY: clean
clean: ## Remove build and coverage artifacts
	rm -rf bin coverage.out coverage.html
