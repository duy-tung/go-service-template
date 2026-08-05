SHELL := /bin/bash

BIN_DIR := $(CURDIR)/bin
BUF ?= $(or $(wildcard $(BIN_DIR)/buf),buf)
HELM ?= $(or $(wildcard $(BIN_DIR)/helm),helm)

# Application database (local development).
DATABASE_URL ?= postgres://order_engine:local-dev-password@127.0.0.1:5432/order_engine?sslmode=disable
# Admin DSN used by integration tests to create throwaway databases.
TEST_DATABASE_URL ?= postgres://postgres@127.0.0.1:5433/postgres?sslmode=disable

IMAGE ?= order-engine:dev
CHART := deployments/helm/order-engine
KUBE_VERSION := 1.34.0

.PHONY: all tools generate lint vet fmt-check build test test-unit docker-build \
	migrate-up migrate-down db-seed db-reset helm-lint helm-template clean

all: lint build test

## tools: install pinned developer tools into ./bin
tools:
	GOBIN=$(BIN_DIR) go install github.com/bufbuild/buf/cmd/buf@v1.72.0
	@echo "install helm v3.21.3 from https://get.helm.sh/helm-v3.21.3-$$(go env GOOS)-$$(go env GOARCH).tar.gz into $(BIN_DIR)"

## generate: lint protos and regenerate gen/ (must run before Go builds)
generate:
	$(BUF) lint
	$(BUF) generate

lint: generate vet fmt-check

vet:
	go vet ./...

fmt-check:
	@out="$$(gofmt -l cmd internal pkg)"; \
	if [ -n "$$out" ]; then echo "gofmt needed on:"; echo "$$out"; exit 1; fi

build: generate
	go build ./...

## test: full suite incl. integration tests against real PostgreSQL.
## ORDER_ENGINE_TEST_REQUIRE_DB=1 turns missing-database skips into failures.
test:
	ORDER_ENGINE_TEST_DATABASE_URL="$(TEST_DATABASE_URL)" \
	ORDER_ENGINE_TEST_REQUIRE_DB=1 \
	go test -race -count=1 ./...

## test-unit: only tests that need no database
test-unit:
	go test -race -count=1 ./pkg/... ./internal/usecase/... ./internal/transport/...

docker-build:
	docker build --tag $(IMAGE) .

## migrate-up/migrate-down: apply migrations with psql, in filename order.
## Fresh-database workflow: files are not idempotent by design; use db-reset
## to rebuild a dev database from scratch.
migrate-up:
	set -euo pipefail; \
	for f in migrations/*.up.sql; do \
		echo "applying $$f"; \
		psql "$(DATABASE_URL)" -q -v ON_ERROR_STOP=1 -f "$$f"; \
	done

migrate-down:
	set -euo pipefail; \
	for f in $$(ls -r migrations/*.down.sql); do \
		echo "applying $$f"; \
		psql "$(DATABASE_URL)" -q -v ON_ERROR_STOP=1 -f "$$f"; \
	done

db-seed:
	psql "$(DATABASE_URL)" -q -v ON_ERROR_STOP=1 -f scripts/seed.sql

db-reset: migrate-down migrate-up db-seed

helm-lint:
	$(HELM) lint --strict $(CHART)

helm-template:
	$(HELM) template order-engine $(CHART) --kube-version $(KUBE_VERSION) \
		--set routing.mode=grpc --set routing.hostname=grpc.example.test > /dev/null
	$(HELM) template order-engine $(CHART) --kube-version $(KUBE_VERSION) \
		--set routing.mode=http --set routing.hostname=connect.example.test > /dev/null
	@echo "helm template: both routing modes render"

clean:
	rm -rf $(BIN_DIR)
