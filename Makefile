# minion/ is the Go CLI (see minion/Makefile); runner/ is the Rust energy-runner.
.DEFAULT_GOAL := help

.PHONY: help build cross install test e2e clean

help: ## show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*##' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS=":.*##"}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

build: ## build minion (minion/bin/) and energy-runner (runner/target/release/)
	@$(MAKE) -C minion build
	@cd runner && cargo build --release

cross: ## cross-compile the minion agents (minion/bin/minion-linux-*)
	@$(MAKE) -C minion cross

install: ## install minion (see minion/Makefile)
	@$(MAKE) -C minion install

test: ## test and lint both
	@$(MAKE) -C minion test
	@cd runner && cargo test && cargo clippy --all-targets -- -D warnings

e2e: build ## end-to-end test of minion + energy-runner here, with no ssh or root
	@test/e2e.sh

clean: ## remove build artifacts
	@$(MAKE) -C minion clean
	@cd runner && cargo clean
