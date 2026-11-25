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

build: ## build gru, its Linux agents, minion for Linux and the skill into gru/bin/
	@cd gru && go build $(LDFLAGS) -o bin/gru . && for p in linux/amd64 linux/arm64; do \
	  GOOS=$${p%/*} GOARCH=$${p#*/} go build $(LDFLAGS) -o bin/gru-$${p%/*}-$${p#*/} . || exit 1; done
	@cp docs/ai/SKILL.md gru/bin/SKILL.md
	@for m in $(MINION); do \
	  t=$${m#*:}-unknown-linux-musl out=gru/bin/minion-linux-$${m%:*}; \
	  (cd minion && MINION_VERSION=$(VERSION) cargo build --release --target $$t) || exit 1; \
	  cp minion/target/$$t/release/minion $$out.tmp && mv -f $$out.tmp $$out && echo "built → $$out"; \
	done

install: build ## install gru to $(PREFIX)/bin, its agents and minion to $(PREFIX)/libexec/gru, the skill
	@gru/bin/gru install --prefix $(PREFIX) --skill-dir $(SKILL_DIR)

dist: build ## release archives dist/gru-<os>-<arch>.tar.gz: gru, its Linux agents, minion and the skill
	@cd gru && for p in darwin/amd64 darwin/arm64; do \
	  GOOS=$${p%/*} GOARCH=$${p#*/} go build $(LDFLAGS) -o bin/gru-$${p%/*}-$${p#*/} . || exit 1; done
	@rm -rf dist && for p in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do \
	  mkdir -p dist/$$p && cp gru/bin/gru-$$p dist/$$p/gru && cp gru/bin/gru-linux-* gru/bin/minion-linux-* gru/bin/SKILL.md dist/$$p/ && \
	  (cd dist/$$p && tar -czf ../gru-$$p.tar.gz *) && rm -r dist/$$p && echo "packed → dist/gru-$$p.tar.gz"; \
	done

test: ## test and lint both
	@cd gru && go test ./... && go vet ./...
	@cd minion && cargo test && cargo clippy --all-targets -- -D warnings

e2e: dist ## end-to-end test of gru + minion here, with no ssh or root
	@test/e2e.sh

clean: ## remove build artifacts
	@rm -rf gru/bin dist
	@cd minion && cargo clean
