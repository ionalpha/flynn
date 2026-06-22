.DEFAULT_GOAL := build
GO ?= go
BIN := flynn

.PHONY: build
build:
	$(GO) build -o $(BIN) ./cmd/flynn

.PHONY: run
run:
	$(GO) run ./cmd/flynn

.PHONY: test
test:
	$(GO) test -race -covermode=atomic -coverprofile=coverage.out ./...

.PHONY: fmt
fmt:
	$(GO) run mvdan.cc/gofumpt@latest -w .
	$(GO) run golang.org/x/tools/cmd/goimports@latest -w -local github.com/ionalpha/flynn .

.PHONY: lint
lint:
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run

.PHONY: vuln
vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: ci
ci: tidy build
	$(GO) vet ./...
	$(MAKE) lint
	$(MAKE) test
	$(MAKE) vuln

.PHONY: clean
clean:
	rm -f $(BIN) $(BIN).exe coverage.out
	rm -rf dist
