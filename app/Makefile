VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -X 'main.version=$(VERSION)'

.PHONY: build build-all dev test clean

build:
	wails build -ldflags "$(LDFLAGS)" -tags no_duckdb_arrow
	mkdir -p dist
	# macOS preserves extended attributes (incl. ad-hoc code-sign
	# resources) via ditto, not cp -r. With cp -r the signature is
	# stripped and the launched binary aborts at startup with
	# "SIGKILL (Code Signature Invalid)" until manually re-signed.
	rm -rf dist/shell-agent-v2.app
	ditto build/bin/shell-agent-v2.app dist/shell-agent-v2.app

build-all: build

dev:
	wails dev -tags no_duckdb_arrow

test:
	go test ./...

clean:
	rm -rf build/bin dist
