APP     := shell-agent-v2
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -X 'main.version=$(VERSION)'

# macOS Developer ID signing / notarization (see nlink-jp/.github
# CONVENTIONS.md §Code Signing → Wails / GUI apps). Defaults match
# any Developer ID Application cert in the keychain and the
# org-standard notary profile. Builds without these fall back to
# whatever signature `wails build` emitted (typically ad-hoc) with
# a one-line warning from the scripts.
CODESIGN_IDENTITY ?= Developer ID Application
NOTARY_PROFILE    ?= nlink-jp-notary
ENTITLEMENTS      ?= ../scripts/entitlements.plist

# Scripts live at the project root; app/ Makefile reaches up.
CODESIGN_SCRIPT := ../scripts/codesign-darwin-app.sh
NOTARIZE_SCRIPT := ../scripts/notarize-darwin-app.sh

.PHONY: build build-all package dev test clean

build:
	wails build -platform darwin/arm64 -ldflags "$(LDFLAGS)" -tags no_duckdb_arrow
	@$(CODESIGN_SCRIPT) build/bin/$(APP).app "$(CODESIGN_IDENTITY)" "$(ENTITLEMENTS)"
	mkdir -p dist
	# macOS preserves extended attributes (incl. code-sign
	# resources) via ditto, not cp -r. With cp -r the signature is
	# stripped and the launched binary aborts at startup with
	# "SIGKILL (Code Signature Invalid)" until manually re-signed.
	rm -rf dist/$(APP).app
	ditto build/bin/$(APP).app dist/$(APP).app

build-all: build

## package: build, notarize the .app (staples the ticket), then zip
## the now-stapled bundle for upload to GitHub Releases.
package: build
	@$(NOTARIZE_SCRIPT) dist/$(APP).app "$(NOTARY_PROFILE)"
	cd dist && /usr/bin/ditto -c -k --keepParent $(APP).app $(APP)-$(VERSION)-darwin-arm64.zip
	@ls -la dist/$(APP)-$(VERSION)-darwin-arm64.zip

dev:
	wails dev -tags no_duckdb_arrow

test:
	go test ./...

clean:
	rm -rf build/bin dist

# Homebrew tap generation (see ../scripts/release-brew.mk). After `make package`,
# `make brew` generates this cask from the built darwin-arm64 zip into the local
# nlink-jp/homebrew-tap checkout. Makefile lives in app/, scripts at repo root.
BREW_KIND      := cask
BREW_DESC      := Local-first chat and agent tool with interactive data analysis
BREW_NAME      := $(APP)
BREW_APP       := $(APP).app
BREW_BUNDLE_ID := com.wails.shell-agent-v2
GEN_BREW       := ../scripts/gen-brew.sh
include ../scripts/release-brew.mk
