.PHONY: build build-cli build-server test test-integration release clean dev-server dev-cli

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -ldflags "-X github.com/charliek/shed/internal/version.Version=$(VERSION) -X github.com/charliek/shed/internal/version.GitCommit=$(GIT_COMMIT) -X github.com/charliek/shed/internal/version.BuildDate=$(BUILD_DATE)"

# Build both binaries
build: build-cli build-server

# Build CLI only
build-cli:
	go build $(LDFLAGS) -o bin/shed ./cmd/shed

# Build server only
build-server:
	go build $(LDFLAGS) -o bin/shed-server ./cmd/shed-server

# Run all unit tests
test:
	go test -v ./...

# Run integration tests (requires Docker)
test-integration:
	go test -v -tags=integration ./integration/...

# Cross-compile for release
release:
	GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o dist/shed-darwin-amd64 ./cmd/shed
	GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o dist/shed-darwin-arm64 ./cmd/shed
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o dist/shed-linux-amd64 ./cmd/shed
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o dist/shed-linux-arm64 ./cmd/shed
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o dist/shed-server-linux-amd64 ./cmd/shed-server
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o dist/shed-server-linux-arm64 ./cmd/shed-server

# Clean build artifacts
clean:
	rm -rf bin/ dist/

# Run server in development mode
dev-server:
	go run ./cmd/shed-server serve --config ./configs/server.dev.yaml

# Run CLI in development mode (pass ARGS to specify command)
# Example: make dev-cli ARGS="list"
dev-cli:
	go run ./cmd/shed $(ARGS)

# Format code
fmt:
	go fmt ./...

# Run linter
lint:
	golangci-lint run

# Tidy dependencies
tidy:
	go mod tidy
