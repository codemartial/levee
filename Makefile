GO ?= go
GOBIN := $(shell $(GO) env GOPATH)/bin
PKG := ./...
FUZZTIME ?= 30s

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: test
test: ## Run unit tests
	$(GO) test $(PKG)

.PHONY: test-race
test-race: ## Run tests with the race detector and write coverage.out
	$(GO) test -race -covermode=atomic -coverprofile=coverage.out $(PKG)

.PHONY: test-amd64
test-amd64: ## Run tests as amd64 (Rosetta on Apple silicon) to catch arch bugs
	GOARCH=amd64 $(GO) test $(PKG)

.PHONY: cover
cover: test-race ## Show per-function coverage
	$(GO) tool cover -func=coverage.out

.PHONY: bench
bench: ## Run the throughput micro-benchmark on one CPU
	$(GO) test -run '^$$' -bench BenchmarkThroughput -benchmem -cpu 1 $(PKG)

.PHONY: fuzz
fuzz: ## Fuzz each target for FUZZTIME (default 30s)
	$(GO) test -run '^$$' -fuzz '^FuzzAdmission$$' -fuzztime $(FUZZTIME) .
	$(GO) test -run '^$$' -fuzz '^FuzzInvNormCDF$$' -fuzztime $(FUZZTIME) .
	$(GO) test -run '^$$' -fuzz '^FuzzWilson$$' -fuzztime $(FUZZTIME) .

.PHONY: vet
vet: ## Vet the library and the benchmarks module
	$(GO) vet $(PKG)
	cd benchmarks && $(GO) vet ./...

.PHONY: lint
lint: ## Run golangci-lint (see CONTRIBUTING.md to install)
	$(GOBIN)/golangci-lint run $(PKG)

.PHONY: vuln
vuln: ## Run govulncheck
	$(GOBIN)/govulncheck $(PKG)

.PHONY: tidy
tidy: ## Tidy modules and format sources
	$(GO) mod tidy
	gofmt -w .

.PHONY: ci
ci: vet lint test-race vuln ## Run the full local gate (mirrors CI)
