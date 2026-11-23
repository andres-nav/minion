.DEFAULT_GOAL := help
.DELETE_ON_ERROR:

BIN       := bin/gru
VERSION   := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PREFIX    ?= $(HOME)/.local
SKILL_DIR ?= $(HOME)/.claude/skills
PLATFORMS := linux/amd64 linux/arm64

.PHONY: help build cross install test vet clean

help: ## show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*##' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS=":.*##"}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

# CGO is disabled: gru is pure Go, so this yields portable, statically-linked
# binaries (ideal for the agent uploaded to arbitrary Linux hosts) and lets the
# linux agents cross-compile on macOS without a Linux C toolchain.
build: ## compile the gru binary into bin/
	@CGO_ENABLED=0 go build -ldflags "-X github.com/andres-nav/minion/gru/internal/cli.version=$(VERSION)" -o $(BIN) .
	@echo "built → $(BIN) ($(VERSION))"

cross: ## cross-compile agent binaries for remote platforms (bin/gru-<os>-<arch>)
	@for p in $(PLATFORMS); do \
	  goos=$${p%/*}; goarch=$${p#*/}; out=bin/gru-$$goos-$$goarch; \
	  CGO_ENABLED=0 GOOS=$$goos GOARCH=$$goarch go build -ldflags "-X github.com/andres-nav/minion/gru/internal/cli.version=$(VERSION)" -o $$out . || exit 1; \
	  echo "built → $$out"; \
	done

install: build cross ## install the host binary to $(PREFIX)/bin, agents to $(PREFIX)/libexec/gru
	@mkdir -p $(PREFIX)/bin $(PREFIX)/libexec/gru
	@# install atomically (temp + mv): overwriting a binary in place invalidates
	@# its code signature on macOS and the kernel SIGKILLs it at launch.
	@cp bin/gru $(PREFIX)/bin/.gru.tmp && mv -f $(PREFIX)/bin/.gru.tmp $(PREFIX)/bin/gru
	@for a in bin/gru-linux-*; do \
	  cp $$a $(PREFIX)/libexec/gru/.tmp && mv -f $(PREFIX)/libexec/gru/.tmp $(PREFIX)/libexec/gru/$$(basename $$a); \
	done
	@# install the AI skill only if the Claude skills dir already exists (don't
	@# create it on machines without Claude).
	@if [ -d "$(SKILL_DIR)" ]; then \
	  mkdir -p "$(SKILL_DIR)/gru" && cp ../docs/ai/SKILL.md "$(SKILL_DIR)/gru/SKILL.md" && \
	  echo "skill → $(SKILL_DIR)/gru"; \
	else echo "skipped skill ($(SKILL_DIR) not present)"; fi
	@echo "installed → $(PREFIX)/bin/gru (agents in $(PREFIX)/libexec/gru)"

test: ## run tests and vet
	@go test ./...
	@go vet ./...

clean: ## remove build artifacts
	@rm -rf bin
