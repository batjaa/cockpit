BINARY  := cockpit
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: build install test fmt vet clean help

build: ## Build ./cockpit with the version stamped in
	go build $(LDFLAGS) -o $(BINARY) .

install: ## Install cockpit into $GOBIN (go env GOBIN / GOPATH/bin)
	go install $(LDFLAGS) .

test: ## Run the test suite
	go test ./...

fmt: ## Format all Go source
	gofmt -w .

vet: ## Run go vet
	go vet ./...

clean: ## Remove build artifacts
	rm -f $(BINARY)
	rm -rf dist/

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'
