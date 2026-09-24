.DEFAULT_GOAL := test

.PHONY: fmt-check vet test test-race test-integration openapi build compose-config compose-load-config compose-up compose-down smoke

fmt-check:
	@test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './.git/*'))"

vet:
	go vet ./...

test:
	go test ./...

test-race:
	go test -race ./...

test-integration:
	@test -n "$(TEST_DATABASE_URL)" || { echo "TEST_DATABASE_URL must be set for integration tests" >&2; exit 1; }
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -count=1 ./internal/adapter/postgres ./internal/app

openapi:
	go test ./internal/adapter/httpapi -run OpenAPI

build:
	mkdir -p bin
	go build -o bin/fx-quotes ./cmd/fx-quotes

compose-config:
	docker compose config

compose-load-config:
	docker compose -p fx-quotes-load -f compose.load.yaml config

compose-up:
	docker compose up --build -d

compose-down:
	docker compose down

smoke:
	./scripts/smoke.sh
