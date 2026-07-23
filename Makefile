.DEFAULT_GOAL := help

.PHONY: help build build-cli build-server run test test-race test-cover vet fmt check clean

BIN_DIR := bin

help: ## List available workflows
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "%-14s %s\n", $$1, $$2}'

build: build-cli build-server ## Build CLI and web server

build-cli: ## Build the market-making CLI
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/mmg ./cmd/mmg

build-server: ## Build the local web server
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/market-maker-server ./cmd/server

run: ## Run the local web server at http://127.0.0.1:8080
	go run ./cmd/server

test: ## Run all tests
	go test ./...

test-race: ## Run all tests with the race detector
	go test -race ./...

test-cover: ## Run all tests with coverage summaries
	go test -cover ./...

vet: ## Run Go static analysis
	go vet ./...

fmt: ## Format Go source files
	gofmt -w $$(git ls-files '*.go')

check: fmt vet test-race ## Format, vet, and run race tests

clean: ## Remove local build artifacts
	rm -rf $(BIN_DIR)
