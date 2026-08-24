# HookRelay — common tasks. `make help` lists them.
.DEFAULT_GOAL := help
SHELL := /usr/bin/env bash

API_URL      ?= http://localhost:8080
RECEIVER_URL ?= http://localhost:9090
EVENTS       ?= 10000
CONCURRENCY  ?= 50
# Compressed backoff so the dead-letter path finishes in seconds, not hours.
TEST_RETRY_SCHEDULE ?= 1s,1s,2s,2s,3s,3s,4s

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: up
up: ## Start the whole stack (postgres, redis, api, worker, receiver, frontend)
	docker compose up --build -d
	@echo "dashboard http://localhost:3000  api http://localhost:8080  receiver http://localhost:9090"

.PHONY: up-test
up-test: ## Start the stack with a compressed retry schedule, for verify/chaos
	RETRY_SCHEDULE=$(TEST_RETRY_SCHEDULE) BREAKER_COOLDOWN=5s DELIVERY_MAX_AGE=5m \
	  docker compose up --build -d

.PHONY: down
down: ## Stop the stack, keeping volumes
	docker compose down

.PHONY: clean
clean: ## Stop the stack and delete its data volumes
	docker compose down -v

.PHONY: logs
logs: ## Tail every service's logs
	docker compose logs -f

.PHONY: test
test: ## Run the Go unit tests
	cd backend && go test ./... -v

.PHONY: test-race
test-race: ## Run the unit tests under the race detector
	cd backend && go test -race ./...

.PHONY: check
check: ## build + vet + gofmt + unit tests + frontend typecheck
	cd backend  && go build ./... && go vet ./... && go test ./...
	cd receiver && go build ./... && go vet ./...
	cd loadtest && go build ./... && go vet ./...
	@cd backend && out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "not gofmt'd:"; echo "$$out"; exit 1; fi
	cd frontend && npm run typecheck

.PHONY: verify
verify: ## End-to-end verification against a running stack (needs up-test)
	API_URL=$(API_URL) RECEIVER_URL=$(RECEIVER_URL) scripts/verify.sh

.PHONY: chaos
chaos: ## Kill the worker mid-delivery and prove zero loss
	API_URL=$(API_URL) RECEIVER_URL=$(RECEIVER_URL) scripts/chaos.sh 1000

.PHONY: loadtest
loadtest: ## Ingest EVENTS events and assert zero lost deliveries
	cd loadtest && go run ./cmd/loadtest \
	  -api $(API_URL) -receiver $(RECEIVER_URL) \
	  -events $(EVENTS) -concurrency $(CONCURRENCY)

.PHONY: fmt
fmt: ## Format the Go code
	cd backend  && gofmt -w .
	cd receiver && gofmt -w .
	cd loadtest && gofmt -w .

.PHONY: build
build: ## Build every binary into backend/bin
	mkdir -p backend/bin
	cd backend  && go build -o bin/api    ./cmd/api
	cd backend  && go build -o bin/worker ./cmd/worker
	cd receiver && go build -o ../backend/bin/receiver .
