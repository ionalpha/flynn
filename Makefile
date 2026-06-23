# Convenience forwarder. The dev/ scripts are the single source of truth for
# build/test/lint/fmt/vuln/check (CI runs the same scripts), so `make test` and
# `./dev/test` are equivalent. For arguments, call the scripts directly:
#   ./dev/test ./state/...
.DEFAULT_GOAL := build
GO ?= go
BIN := flynn

.PHONY: build test lint fmt vuln check pr run tidy clean help

build: ## Compile all packages (CI build)
	@./dev/build

test: ## Race + coverage test suite (CI test)
	@./dev/test

lint: ## go mod tidy check + golangci-lint (CI lint)
	@./dev/lint

fmt: ## Auto-format (gofumpt + goimports)
	@./dev/fmt

vuln: ## govulncheck dependency scan (CI vuln)
	@./dev/vuln

check: ## Everything CI gates on, in one command
	@./dev/check

pr: ## Open a PR against main using the template
	@./dev/pr

run: ## Build and run the flynn binary
	$(GO) run ./cmd/flynn

tidy: ## go mod tidy
	$(GO) mod tidy

clean: ## Remove build artifacts
	rm -f $(BIN) $(BIN).exe coverage.out
	rm -rf dist

help: ## List targets
	@grep -E '^[a-z]+:.*## ' $(MAKEFILE_LIST) | sed -E 's/:.*## /\t/' | awk -F'\t' '{printf "  make %-7s %s\n", $$1, $$2}'
