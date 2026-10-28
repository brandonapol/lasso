SHELL := bash
.SHELLFLAGS = -e -o pipefail -c
.DEFAULT_GOAL := help

GO := $(or $(GO),$(shell command -v go))

.PHONY: help build check fmt test lint install

help: ## Show targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-10s %s\n", $$1, $$2}'

build: ## Build ./build/lasso
	$(GO) build -o build/lasso ./cmd/lasso

install: ## Install lasso into GOBIN
	$(GO) install ./cmd/lasso

fmt: ## Format the code
	gofmt -w .

check: ## gofmt, vet, lint and tests (the gate for every PR)
	@unformatted="$$(gofmt -l .)"; if [ -n "$$unformatted" ]; then echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi
	$(GO) vet ./...
	golangci-lint run ./...
	$(GO) test ./...

test: ## Run the tests
	$(GO) test ./...
