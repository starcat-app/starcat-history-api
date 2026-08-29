.PHONY: test test-go test-builder build run

test: test-go test-builder

test-go:
	go test ./...
	go vet ./...

test-builder:
	cd builder && uv sync --extra test --python 3.12 && uv run pytest -q

build:
	mkdir -p bin
	go build -o bin/starcat-history-api ./cmd/server

run:
	go run ./cmd/server
