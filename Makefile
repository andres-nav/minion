.DEFAULT_GOAL := help
.DELETE_ON_ERROR:

BIN       := bin/minion
VERSION   := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PREFIX    ?= $(HOME)/.local
PLATFORMS := linux/amd64 linux/arm64

.PHONY: help build cross install test vet clean

help: ## show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*##' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS=":.*##"}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

build: ## compile the minion binary into bin/
	@go build -ldflags "-X github.com/andres-nav/minion/internal/cli.version=$(VERSION)" -o $(BIN) .
	@echo "built → $(BIN) ($(VERSION))"

cross: ## cross-compile agent binaries for remote platforms (bin/minion-<os>-<arch>)
	@for p in $(PLATFORMS); do \
	  goos=$${p%/*}; goarch=$${p#*/}; out=bin/minion-$$goos-$$goarch; \
	  GOOS=$$goos GOARCH=$$goarch go build -ldflags "-X github.com/andres-nav/minion/internal/cli.version=$(VERSION)" -o $$out . ; \
	  echo "built → $$out"; \
	done

install: build cross ## build (incl. cross), copy binaries into $(PREFIX)/bin
	@mkdir -p $(PREFIX)/bin
	@cp bin/minion bin/minion-* $(PREFIX)/bin/
	@echo "installed → $(PREFIX)/bin (minion + cross agents)"

test: ## run tests and vet
	@go test ./...
	@go vet ./...

clean: ## remove build artifacts
	@rm -rf bin
