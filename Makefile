.PHONY: up down logs migrate test test-unit test-integration test-concurrency build run clean

DATABASE_URL ?= postgres://wallet:wallet@localhost:5432/wallet_transfer?sslmode=disable

up:
	docker compose up -d
	@echo "Waiting for Postgres..."
	@until docker compose exec -T postgres pg_isready -U wallet -d wallet_transfer >/dev/null 2>&1; do sleep 1; done
	@echo "Postgres is ready."

down:
	docker compose down

logs:
	docker compose logs -f postgres

# Re-apply migrations (drops + recreates schema)
migrate:
	docker compose exec -T postgres psql -U wallet -d wallet_transfer -c "DROP TABLE IF EXISTS ledger_entries, transfers, wallets CASCADE; DROP TYPE IF EXISTS transfer_status, entry_direction;"
	docker compose exec -T postgres psql -U wallet -d wallet_transfer < migrations/001_init.sql

# All tests
test: test-unit test-integration test-concurrency test-regression

# Pure domain unit tests — no DB
test-unit:
	go test -v -race ./internal/domain/... ./internal/service/...

# Integration tests — needs running Postgres
test-integration:
	TEST_DATABASE_URL="$(DATABASE_URL)" go test -v -race ./tests/... -run TestIntegration

# Concurrency tests — needs running Postgres, runs the highest-leverage tests
test-concurrency:
	TEST_DATABASE_URL="$(DATABASE_URL)" go test -v -race -count=1 ./tests/... -run TestConcurrency
	
# Regression test 
test-regression:
	TEST_DATABASE_URL="$(DATABASE_URL)" go test -v -race -count=1 ./tests/... -run TestRegression

build:
	go build -o bin/server ./cmd/server

run: build
	DATABASE_URL="$(DATABASE_URL)" ./bin/server

clean:
	rm -rf bin/
