.PHONY: test test-go test-builder build run install-local-snapshot

VERSION ?= 0.0.0-dev
VERSION_SYMBOL := github.com/starcat-app/starcat-history-api/internal/version.Version

test: test-go test-builder

test-go:
	go test ./...
	go vet ./...

test-builder:
	cd builder && uv sync --extra test --python 3.12 && uv run pytest -q

build:
	mkdir -p bin
	go build -ldflags "-X $(VERSION_SYMBOL)=$(VERSION)" -o bin/starcat-history-api ./cmd/server

run:
	go run ./cmd/server

# 把 Builder Snapshot 的 history.sqlite 安装到本地 STORE_FILE（默认 ./data/history.sqlite）
# 用法: make install-local-snapshot SNAPSHOT=/Volumes/T0/Starcat/history/snapshots/watch-history-20260825-v1
# 覆盖已有库: make install-local-snapshot SNAPSHOT=... FORCE=1
install-local-snapshot:
	@if [ -z "$(SNAPSHOT)" ]; then \
		echo "usage: make install-local-snapshot SNAPSHOT=/path/to/snapshot-dir-or-history.sqlite [FORCE=1]"; \
		exit 2; \
	fi
	@args="$(SNAPSHOT)"; \
	if [ "$(FORCE)" = "1" ]; then args="$$args --force"; fi; \
	scripts/install-local-snapshot.sh $$args
