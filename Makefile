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

install: build cross ## install the host binary to $(PREFIX)/bin, agents to $(PREFIX)/libexec/minion
	@mkdir -p $(PREFIX)/bin $(PREFIX)/libexec/minion
	@# install atomically (temp + mv): overwriting a binary in place invalidates
	@# its code signature on macOS and the kernel SIGKILLs it at launch.
	@cp bin/minion $(PREFIX)/bin/.minion.tmp && mv -f $(PREFIX)/bin/.minion.tmp $(PREFIX)/bin/minion
	@for a in bin/minion-linux-*; do \
	  cp $$a $(PREFIX)/libexec/minion/.tmp && mv -f $(PREFIX)/libexec/minion/.tmp $(PREFIX)/libexec/minion/$$(basename $$a); \
	done
	@echo "installed → $(PREFIX)/bin/minion (agents in $(PREFIX)/libexec/minion)"

test: ## run tests and vet
	@go test ./...
	@go vet ./...

clean: ## remove build artifacts
	@rm -rf bin
