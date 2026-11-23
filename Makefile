# gru/ is the Go CLI on the laptop (see gru/Makefile); minion/ is the Rust runner on the host.
.DEFAULT_GOAL := help

.PHONY: help build cross install test e2e clean

help: ## show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*##' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS=":.*##"}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

build: ## build gru (gru/bin/) and minion (minion/target/release/)
	@$(MAKE) -C gru build
	@cd minion && cargo build --release

cross: ## cross-compile the gru agents (gru/bin/gru-linux-*)
	@$(MAKE) -C gru cross

install: ## install gru, its agents and the skill (see gru/Makefile)
	@$(MAKE) -C gru install

test: ## test and lint both
	@$(MAKE) -C gru test
	@cd minion && cargo test && cargo clippy --all-targets -- -D warnings

e2e: build ## end-to-end test of gru + minion here, with no ssh or root
	@test/e2e.sh

clean: ## remove build artifacts
	@$(MAKE) -C gru clean
	@cd minion && cargo clean
