SHELL := /bin/bash
-include .env
export

.PHONY: build test sqlc migrate-up migrate-down migrate-status bootstrap run worker vet

build:
	go build ./...

vet:
	go vet ./...

sqlc:
	cd db && sqlc generate

migrate-up:
	go run ./cmd/migration up

migrate-down:
	go run ./cmd/migration down

migrate-status:
	go run ./cmd/migration status

# make bootstrap CODE=ACME NAME="Acme Retail" EMAIL=owner@acme.id PASSWORD=secret123
bootstrap:
	go run ./cmd/migration bootstrap -code "$(CODE)" -name "$(NAME)" -email "$(EMAIL)" -password "$(PASSWORD)"

run:
	go run ./cmd/api

worker:
	go run ./cmd/worker

# integration tests need a Postgres with ltree/citext; see tests/integration
test:
	TEST_DATABASE_URL=$${TEST_DATABASE_URL:-postgres://ritel35k:ritel35k@localhost:5432/ritel35k_test?sslmode=disable} go test ./... -count=1
