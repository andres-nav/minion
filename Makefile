# gru/ is the Go CLI on the laptop; minion/ is the Rust runner on the host,
# shipped with gru as static Linux builds that gru uploads.
.DEFAULT_GOAL := help

VERSION   := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -ldflags "-X github.com/andres-nav/minion/gru/internal/cli.version=$(VERSION)"
PREFIX    ?= $(HOME)/.local
SKILL_DIR ?= $(HOME)/.claude/skills
# minion's builds, as <goarch>:<Rust arch>; minion's version is gru's.
MINION    := amd64:x86_64 arm64:aarch64
# gru is pure Go: without cgo its builds are static and cross-compile anywhere.
export CGO_ENABLED := 0

.PHONY: help build install dist test e2e clean

help: ## show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*##' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS=":.*##"}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

build: ## build gru, its Linux agents and minion for Linux into gru/bin/
	@cd gru && go build $(LDFLAGS) -o bin/gru . && for p in linux/amd64 linux/arm64; do \
	  GOOS=$${p%/*} GOARCH=$${p#*/} go build $(LDFLAGS) -o bin/gru-$${p%/*}-$${p#*/} . || exit 1; done
	@for m in $(MINION); do \
	  t=$${m#*:}-unknown-linux-musl out=gru/bin/minion-linux-$${m%:*}; \
	  (cd minion && MINION_VERSION=$(VERSION) cargo build --release --target $$t) || exit 1; \
	  cp minion/target/$$t/release/minion $$out.tmp && mv -f $$out.tmp $$out && echo "built → $$out"; \
	done

install: build ## install gru to $(PREFIX)/bin, its agents and minion to $(PREFIX)/libexec/gru, the skill
	@mkdir -p $(PREFIX)/bin $(PREFIX)/libexec/gru
	@# install atomically (temp + mv): overwriting a binary in place invalidates
	@# its code signature on macOS and the kernel SIGKILLs it at launch.
	@cp gru/bin/gru $(PREFIX)/bin/.gru.tmp && mv -f $(PREFIX)/bin/.gru.tmp $(PREFIX)/bin/gru
	@for a in gru/bin/gru-linux-* gru/bin/minion-linux-*; do \
	  cp $$a $(PREFIX)/libexec/gru/.tmp && mv -f $(PREFIX)/libexec/gru/.tmp $(PREFIX)/libexec/gru/$${a##*/}; \
	done
	@# install the AI skill only if the Claude skills dir already exists (don't
	@# create it on machines without Claude).
	@if [ -d "$(SKILL_DIR)" ]; then \
	  mkdir -p "$(SKILL_DIR)/gru" && cp docs/ai/SKILL.md "$(SKILL_DIR)/gru/SKILL.md" && \
	  echo "skill → $(SKILL_DIR)/gru"; \
	else echo "skipped skill ($(SKILL_DIR) not present)"; fi
	@echo "installed → $(PREFIX)/bin/gru (agents and minion in $(PREFIX)/libexec/gru)"

dist: build ## release archives dist/gru-<os>-<arch>.tar.gz: gru, its Linux agents and minion
	@cd gru && for p in darwin/amd64 darwin/arm64; do \
	  GOOS=$${p%/*} GOARCH=$${p#*/} go build $(LDFLAGS) -o bin/gru-$${p%/*}-$${p#*/} . || exit 1; done
	@rm -rf dist && for p in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do \
	  mkdir -p dist/$$p && cp gru/bin/gru-$$p dist/$$p/gru && cp gru/bin/gru-linux-* gru/bin/minion-linux-* dist/$$p/ && \
	  (cd dist/$$p && tar -czf ../gru-$$p.tar.gz *) && rm -r dist/$$p && echo "packed → dist/gru-$$p.tar.gz"; \
	done

test: ## test and lint both
	@cd gru && go test ./... && go vet ./...
	@cd minion && cargo test && cargo clippy --all-targets -- -D warnings

e2e: build ## end-to-end test of gru + minion here, with no ssh or root
	@test/e2e.sh

clean: ## remove build artifacts
	@rm -rf gru/bin dist
	@cd minion && cargo clean
