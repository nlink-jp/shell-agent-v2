VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -X 'main.version=$(VERSION)'

.PHONY: build build-all dev test clean

build:
	wails build -ldflags "$(LDFLAGS)" -tags no_duckdb_arrow
	mkdir -p dist
	cp -r build/bin/shell-agent-v2.app dist/

build-all: build

dev:
	wails dev

test:
	go test ./...

clean:
	rm -rf build/bin dist
