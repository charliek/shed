.PHONY: build build-cli build-server build-agent build-firstboot build-tools test test-integration test-integration-local install-local-server restore-brew-server release clean dev-server dev-cli check check-kernel-pin coverage lint-all docs docs-serve firecracker-rootfs download-firecracker vz-rootfs vz-rootfs-base vz-rootfs-all

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
# Live integration tests: drive a running shed-server via the `shed` CLI.
# Pytest + subprocess, managed with uv. Requires uv on PATH (install via
# `brew install uv` or https://docs.astral.sh/uv/getting-started/installation/).
# Tests parameterized over [vz, fc]; each skips cleanly when its target
# backend is unreachable from this host. See tests/integration/README.md.
test-integration:
	@command -v uv >/dev/null 2>&1 || { \
	  echo "uv is required for integration tests."; \
	  echo "Install: brew install uv  (or https://docs.astral.sh/uv/getting-started/installation/)"; \
	  exit 1; \
	}
	cd tests/integration && uv sync && uv run pytest -v

# Local dev-binary swap + integration suite + restore (Mac VZ only).
#
# Closes the integration-suite-server-coverage gap documented in
# docs/discovery/integration-suite-server-coverage.md: by default
# `make test-integration` runs against whichever shed-server binary is
# currently installed (typically the brew release), so server-side PRs
# can pass the suite without exercising their own code. These targets
# swap in the just-built dev binary, run the suite against it, and
# restore the brew binary regardless of suite outcome.
#
# Discovery surfaced during PR-B1 (#153) and formalised in
# ~/.claude/plans/patient-bridging-heron.md §2. The corresponding FC
# remote (mini3 via SSH) workflow lives in PR 3's
# `test-integration-local-fc` target.

# Brew install paths. Resolved dynamically because the Cellar version
# changes per release; we don't want to hardcode a stale path.
# All three variables collapse to empty when brew shed isn't installed;
# the install-local-server recipe checks that before doing anything
# destructive.
BREW_SHED_PREFIX := $(shell brew --prefix shed 2>/dev/null)
BREW_VERSION := $(shell test -n "$(BREW_SHED_PREFIX)" && basename "$$(readlink "$(BREW_SHED_PREFIX)" 2>/dev/null)" 2>/dev/null)
BREW_SHED_BIN := $(BREW_SHED_PREFIX)/bin/shed-server
BACKUP_PATH := /tmp/shed-server-v$(BREW_VERSION).bak

# shed-build-tools image ref to inject when the dev binary runs.
# Dev binaries embed Version="vX.Y.Z-N-gHASH-dirty", which
# BuildToolsRefForTag returns "" for by design (dev-build isolation —
# see docs/discovery/integration-suite-server-coverage.md §7). Without
# the env var the dev binary falls back to in-guest mkfs.ext4 on first
# boot (~4 s on VZ), which the split timing gate (PR #157) skips
# cleanly but it's still not the path we want when validating server
# changes against release-shaped behavior. Override with
# `RELEASE_BUILD_TOOLS_REF=ghcr.io/charliek/shed-build-tools:vX.Y.Z`
# to pin to a non-latest release.
RELEASE_BUILD_TOOLS_REF ?= $(shell git tag --list 'v*' --sort=-version:refname | head -1 | sed 's|^|ghcr.io/charliek/shed-build-tools:|')

# Swap the just-built bin/shed-server into the brew Cellar, codesign
# ad-hoc (launchd SIGKILLs unsigned binaries), set SHED_BUILD_TOOLS_REF
# in the user's launchd domain, and restart the brew service.
# Refuses to clobber an existing backup unless FORCE=1, so a developer
# who runs this twice doesn't lose the original brew binary.
install-local-server: build
	@case "$$(uname -s)" in \
	  Darwin) ;; \
	  *) echo "ERROR: install-local-server targets the brew-installed shed-server on macOS;"; \
	     echo "       run this on a Mac. For the FC remote (mini3) workflow see"; \
	     echo "       'make install-remote-server' (planned for PR 3)."; exit 1 ;; \
	esac
	@if [ -z "$(BREW_VERSION)" ]; then \
	  echo "ERROR: brew shed is not installed (or 'brew --prefix shed' failed)."; \
	  echo "       Install with 'brew install charliek/shed/shed' and retry."; exit 1; \
	fi
	@if [ -f "$(BACKUP_PATH)" ] && [ "$(FORCE)" != "1" ]; then \
	  echo "ERROR: backup already exists at $(BACKUP_PATH)."; \
	  echo "       Run 'make restore-brew-server' first, or pass FORCE=1 to overwrite"; \
	  echo "       (FORCE=1 is destructive — the existing backup is lost)."; exit 1; \
	fi
	@cp -f "$(BREW_SHED_BIN)" "$(BACKUP_PATH)"
	@brew services stop shed
	@chmod +w "$(BREW_SHED_BIN)"
	@cp -f bin/shed-server "$(BREW_SHED_BIN)"
	@# --force lets us re-sign cleanly on subsequent runs without
	@# tripping codesign's "is already signed" error. macOS launchd
	@# SIGKILLs unsigned binaries so this step is non-optional.
	@codesign --force -s - "$(BREW_SHED_BIN)"
	@chmod -w "$(BREW_SHED_BIN)"
	@launchctl setenv SHED_BUILD_TOOLS_REF "$(RELEASE_BUILD_TOOLS_REF)"
	@brew services start shed
	@echo ""
	@echo "Dev shed-server installed in brew Cellar at $(BREW_SHED_BIN)."
	@echo "Build-tools ref: $(RELEASE_BUILD_TOOLS_REF)"
	@echo "Backup at $(BACKUP_PATH)."
	@echo "Run 'make restore-brew-server' to revert (or it runs automatically"
	@echo "at the end of 'make test-integration-local')."

# Reverse of install-local-server: restore the brew binary from the
# backup, clear the launchctl env var, restart the brew service, and
# remove the backup file. Idempotent — re-running after a successful
# restore is a no-op with a clear message.
restore-brew-server:
	@case "$$(uname -s)" in \
	  Darwin) ;; \
	  *) echo "ERROR: restore-brew-server is macOS-only; see install-local-server."; exit 1 ;; \
	esac
	@# Single shell block so the no-op branch can short-circuit cleanly
	@# (a separate `@if ... exit 0` line only exits its sub-shell, not
	@# the recipe — make would continue running the restore steps even
	@# though there's no backup to restore from).
	@if [ ! -f "$(BACKUP_PATH)" ]; then \
	  echo "No backup at $(BACKUP_PATH); nothing to restore. (Idempotent: this is OK.)"; \
	else \
	  set -e; \
	  brew services stop shed; \
	  chmod +w "$(BREW_SHED_BIN)"; \
	  cp -f "$(BACKUP_PATH)" "$(BREW_SHED_BIN)"; \
	  codesign --force -s - "$(BREW_SHED_BIN)"; \
	  chmod -w "$(BREW_SHED_BIN)"; \
	  launchctl unsetenv SHED_BUILD_TOOLS_REF; \
	  brew services start shed; \
	  rm -f "$(BACKUP_PATH)"; \
	  echo "Brew shed-server restored from backup; backup removed."; \
	fi

# Chain: install dev binary locally, run the integration suite against
# it, restore the brew binary REGARDLESS of suite outcome. The
# restore step runs in a sub-make of restore-brew-server so a suite
# failure doesn't strand the host on the dev binary.
test-integration-local: install-local-server
	@$(MAKE) test-integration; STATUS=$$?; $(MAKE) restore-brew-server; exit $$STATUS

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

# Verify the LINUX_IMAGE_VERSION pin stays in lockstep across the two
# Dockerfiles that install the Ubuntu kernel package. Pre-v0.5.8 the
# initramfs and vz/base each installed `linux-image-virtual` without
# pinning, and a Docker BuildKit cache split between stages produced
# initramfs .ko files targeting a different kernel than the rootfs
# vmlinuz — booting the resulting VZ image panicked in the
# shed-initramfs with SHED-INIT-03. The two Dockerfiles must declare
# the same ARG value; this target fails fast if they drift.
#
# firecracker/Dockerfile is intentionally skipped: the FC rootfs uses
# the custom KERNEL_TAG-built kernel and does not install
# linux-image-virtual at all. FC's initramfs is the SAME initramfs
# initramfs/Dockerfile produces, so pinning that one file covers the
# FC initramfs path as well.
check-kernel-pin:
	@vz=$$(awk '/^ARG LINUX_IMAGE_VERSION=/ { print; exit }' vz/Dockerfile) ; \
	 ir=$$(awk '/^ARG LINUX_IMAGE_VERSION=/ { print; exit }' initramfs/Dockerfile) ; \
	 if [ -z "$$vz" ] || [ -z "$$ir" ]; then \
	   echo "ERROR: LINUX_IMAGE_VERSION ARG missing:" ; \
	   echo "  vz/Dockerfile:        $$vz" ; \
	   echo "  initramfs/Dockerfile: $$ir" ; \
	   exit 1 ; \
	 fi ; \
	 if [ "$$vz" != "$$ir" ]; then \
	   echo "ERROR: LINUX_IMAGE_VERSION ARG drifted across Dockerfiles:" ; \
	   echo "  vz/Dockerfile:        $$vz" ; \
	   echo "  initramfs/Dockerfile: $$ir" ; \
	   echo "Bump in lockstep (see docs/reference/images.md)." ; \
	   exit 1 ; \
	 fi ; \
	 echo "kernel pin OK: $$vz"

# Run all checks (lint + test + kernel pin)
check: check-kernel-pin lint test

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

# Build Firecracker rootfs image. Requires bin/shed for the
# `shed image install` step that lands the blob into images_dir.
firecracker-rootfs: build-cli build-agent build-firstboot
	./scripts/build-firecracker-rootfs.sh

# Download Firecracker binary and kernel
download-firecracker:
	./scripts/download-firecracker.sh

# VZ rootfs targets

# Build default VZ rootfs image. Requires bin/shed for the
# `shed image install` step that lands the blob into images_dir.
vz-rootfs: build-cli build-agent build-firstboot
	./scripts/build-vz-rootfs.sh

# Build base VZ rootfs image (minimal)
vz-rootfs-base: build-cli build-agent build-firstboot
	./scripts/build-vz-rootfs.sh --variant base

# Build all VZ rootfs image variants
vz-rootfs-all: build-cli build-agent build-firstboot
	./scripts/build-vz-rootfs.sh --all

# Build the shed-build-tools image (mkfs.erofs + friends) locally, tagged
# `shed-build-tools:dev`. Consumers in development mode point at this tag
# via --build-tools-version=dev. See build-tools/README.md for details.
build-tools:
	docker build -t shed-build-tools:dev build-tools/
