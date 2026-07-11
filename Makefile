# dirstat — read-only terminal disk-usage exploration.
BINARY   := dirstat
PKG      := github.com/phillipod/go-dirstat
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w -X $(PKG)/internal/version.Version=$(VERSION) -X $(PKG)/internal/version.Commit=$(COMMIT) -X $(PKG)/internal/version.BuildDate=$(DATE)
GOFLAGS  := -trimpath
GOFILES  := $(shell find cmd internal -type f -name '*.go')

.PHONY: all build run test test-race vet fmt lint tidy clean install help

all: build

build: ## Build the dirstat binary into ./bin/
	mkdir -p bin
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

run: build ## Build then run against the current directory
	./bin/$(BINARY) .

test: ## Run unit tests
	go test ./...

test-race: ## Run unit tests with the race detector
	go test -race ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format source
	gofmt -s -w $(GOFILES)

lint: vet ## Lint (requires golangci-lint on PATH if available)
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed; skipped"; \
	fi

tidy: ## Tidy module graph
	go mod tidy

clean: ## Remove build artifacts
	rm -rf bin dist coverage.txt coverage.html

install: build ## Install into $$GOBIN
	@dest="$${GOBIN:-$$HOME/.local/bin}"; \
		mkdir -p "$$dest"; \
		cp bin/$(BINARY) "$$dest/"

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.DEFAULT_GOAL := all
