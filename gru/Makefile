.DEFAULT_GOAL := help

VERSION   := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -ldflags "-X github.com/andres-nav/minion/gru/internal/cli.version=$(VERSION)"
PLATFORMS ?= linux/amd64 linux/arm64

# CGO is disabled: gru is pure Go, so this yields portable, statically-linked
# binaries (ideal for the agent uploaded to arbitrary Linux hosts) and lets the
# linux agents cross-compile on macOS without a Linux C toolchain.
export CGO_ENABLED := 0

.PHONY: help build cross test clean

help: ## show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*##' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS=":.*##"}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

build: ## compile the gru binary into bin/
	@go build $(LDFLAGS) -o bin/gru .
	@echo "built → bin/gru ($(VERSION))"

cross: ## cross-compile agent binaries for remote platforms (bin/gru-<os>-<arch>)
	@for p in $(PLATFORMS); do \
	  goos=$${p%/*}; goarch=$${p#*/}; out=bin/gru-$$goos-$$goarch; \
	  GOOS=$$goos GOARCH=$$goarch go build $(LDFLAGS) -o $$out . || exit 1; \
	  echo "built → $$out"; \
	done

test: ## run tests and vet
	@go test ./...
	@go vet ./...

clean: ## remove build artifacts
	@rm -rf bin
