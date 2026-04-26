.PHONY: build build-cli build-server build-agent build-firstboot test test-integration release clean dev-server dev-cli check coverage lint-all docs docs-serve firecracker-rootfs download-firecracker vz-rootfs vz-rootfs-base vz-rootfs-all

GOARCH ?= $(shell go env GOARCH)

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -ldflags "-X github.com/charliek/shed/internal/version.Version=$(VERSION) -X github.com/charliek/shed/internal/version.GitCommit=$(GIT_COMMIT) -X github.com/charliek/shed/internal/version.BuildDate=$(BUILD_DATE)"

# Build all binaries
build: build-cli build-server build-agent build-firstboot

# Build CLI only
build-cli:
	go build $(LDFLAGS) -o bin/shed ./cmd/shed

# Build server only
build-server:
	go build $(LDFLAGS) -o bin/shed-server ./cmd/shed-server

# Build shed-agent only (for Firecracker VMs)
build-agent:
	GOOS=linux GOARCH=$(GOARCH) go build $(LDFLAGS) -o bin/shed-agent ./cmd/shed-agent

# Build shed-firstboot only (in-VM oneshot for identity regen)
build-firstboot:
	GOOS=linux GOARCH=$(GOARCH) go build $(LDFLAGS) -o bin/shed-firstboot ./cmd/shed-firstboot

# Run all unit tests (including SDK submodule)
test:
	go test -v ./...
	cd sdk && go test -v ./...

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
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o dist/shed-agent-linux-amd64 ./cmd/shed-agent
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o dist/shed-agent-linux-arm64 ./cmd/shed-agent
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o dist/shed-firstboot-linux-amd64 ./cmd/shed-firstboot
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o dist/shed-firstboot-linux-arm64 ./cmd/shed-firstboot

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

# Format code (including SDK submodule)
fmt:
	go fmt ./...
	cd sdk && go fmt ./...

# Run linter (including SDK submodule)
lint:
	golangci-lint run
	cd sdk && go vet ./...

# Tidy dependencies (including SDK submodule)
tidy:
	go mod tidy
	cd sdk && go mod tidy

# Run all checks (lint + test)
check: lint test

# Run tests with coverage
coverage:
	go test -v -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

# Run all linting (Go only)
lint-all: lint

# Build documentation
docs:
	uv sync --group docs
	uv run mkdocs build

# Serve documentation locally
docs-serve:
	uv sync --group docs
	uv run mkdocs serve

# Firecracker targets

# Build Firecracker rootfs image
firecracker-rootfs: build-agent build-firstboot
	./scripts/build-firecracker-rootfs.sh

# Download Firecracker binary and kernel
download-firecracker:
	./scripts/download-firecracker.sh

# VZ rootfs targets

# Build default VZ rootfs image
vz-rootfs: build-agent build-firstboot
	./scripts/build-vz-rootfs.sh

# Build base VZ rootfs image (minimal)
vz-rootfs-base: build-agent build-firstboot
	./scripts/build-vz-rootfs.sh --variant base

# Build all VZ rootfs image variants
vz-rootfs-all: build-agent build-firstboot
	./scripts/build-vz-rootfs.sh --all
