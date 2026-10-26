.DEFAULT_GOAL := build

GO       ?= go
PYTHON   ?= python3
BIN_DIR  ?= bin
DIST_DIR ?= dist
GOOS     ?= linux
GOARCH   ?= $(shell $(GO) env GOARCH)

# Match desktop-client/workspace-agent: exact tag for a release, otherwise
# <last-tag>-<n>-g<sha>[-dirty], or a bare SHA when no tag is available.
# Override VERSION explicitly to build from a source export without Git history.
VERSION  ?= $(shell $(PYTHON) scripts/release.py version)
LDFLAGS  ?= -s -w -X main.version=$(VERSION)
TARGETS  := linux-amd64 linux-arm64
ARGS     ?=

.PHONY: build build-all run version version-check package verify-release test test-release test-integration vet fmt fmt-check lint check

version-check:
	@test -n "$(VERSION)" || { printf '%s\n' 'error: version resolution failed; set VERSION explicitly' >&2; exit 1; }

build: version-check
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o "$(BIN_DIR)/container-init" ./cmd/container-init

build-all:
	@set -e; for target in $(TARGETS); do \
		$(MAKE) build GOOS=linux GOARCH=$${target#linux-} BIN_DIR="$(BIN_DIR)/$$target" VERSION="$(VERSION)"; \
	done

run: version-check
	$(GO) run -ldflags "$(LDFLAGS)" ./cmd/container-init $(ARGS)

version: version-check
	@printf '%s\n' "$(VERSION)"

# Consume bin/linux-{amd64,arm64}/container-init, as downloaded from CI's
# native build matrix. The archives contain one stable root directory each.
package:
	$(PYTHON) scripts/release.py package --bin-dir "$(BIN_DIR)" --dist-dir "$(DIST_DIR)" --version "$(VERSION)"

verify-release:
	$(PYTHON) scripts/release.py verify --dist-dir "$(DIST_DIR)" --version "$(VERSION)"

test:
	$(GO) test -race ./...

test-release:
	$(PYTHON) -m unittest discover -s scripts -p 'test_*.py'

test-integration: build
	CONTAINER_INIT_INTEGRATION=1 $(GO) test -count=1 -v ./test/integration

vet:
	$(GO) vet ./...

fmt:
	gofmt -w cmd internal test

fmt-check:
	@test -z "$$(gofmt -l cmd internal test)" || { gofmt -l cmd internal test; exit 1; }

lint:
	CGO_ENABLED=0 golangci-lint run ./...

check: fmt-check vet test test-release
