.DEFAULT_GOAL := help

.PHONY: help build build-cli build-server run test test-js test-race test-cover fuzz fuzz-fixed fuzz-eventlog vet fmt fmt-check check clean

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

test-js: ## Run dependency-free browser runtime tests
	node --test web/static/index.test.js

test-race: ## Run all tests with the race detector
	go test -race ./...

test-cover: ## Run all tests with coverage summaries
	go test -cover ./...

fuzz: fuzz-fixed fuzz-eventlog ## Run bounded fuzz targets

fuzz-fixed: ## Fuzz fixed-point parsing and serialization for 5 seconds
	go test -fuzz=FuzzDecimalRoundTrip -fuzztime=5s ./internal/fixed

fuzz-eventlog: ## Fuzz strict JSON decoding for 5 seconds
	go test -fuzz=FuzzDecodeStrictJSON -fuzztime=5s ./internal/eventlog

vet: ## Run Go static analysis
	go vet ./...

fmt: ## Format Go source files
	gofmt -w $$(git ls-files '*.go')

fmt-check: ## Verify Go source files are formatted without modifying them
	@gofmt -d $$(git ls-files '*.go') | diff -u /dev/null -

check: fmt-check vet test-js test-race build ## Verify formatting, vet, runtime tests, race tests, and builds

clean: ## Remove local build artifacts
	rm -rf $(BIN_DIR)
