.PHONY: build build-cli build-server build-agent build-firstboot build-tools build-fc-remote-server test test-integration test-integration-local test-integration-local-fc install-local-server restore-brew-server install-remote-server restore-remote-server release clean dev-server dev-cli check check-kernel-pin coverage lint-all docs docs-serve firecracker-rootfs download-firecracker vz-rootfs vz-rootfs-base vz-rootfs-all

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
	@# launchd start is async — the suite's session-scoped probe runs
	@# the moment make returns and can race the server before it binds
	@# 8080. Poll until \`shed list\` succeeds (the same probe the
	@# integration suite uses) or 15 s elapses. Without this the suite
	@# silently skips all VZ tests with "shed-server not reachable."
	@for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do \
	  if shed -s my-server list >/dev/null 2>&1; then \
	    echo "VZ shed-server ready after $${i}s."; break; \
	  fi; \
	  if [ "$$i" = "15" ]; then \
	    echo "WARNING: VZ shed-server not reachable after 15 s; integration suite may skip VZ tests."; \
	  fi; \
	  sleep 1; \
	done
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
	@# Always clear the launchctl env var, even in the no-backup branch:
	@# install-local-server sets the env var ALONGSIDE creating the
	@# backup, so a stranded env (manual setenv, partial-failure run, or
	@# a backup that someone rm'd by hand) is the case where "restore"
	@# is most likely to be invoked. Skipping the unset there would
	@# leave the dev binary's behavior wired into the brew binary's
	@# next start.
	@launchctl unsetenv SHED_BUILD_TOOLS_REF
	@# Single shell block so the no-op branch can short-circuit cleanly
	@# (a separate `@if ... exit 0` line only exits its sub-shell, not
	@# the recipe — make would continue running the restore steps even
	@# though there's no backup to restore from).
	@if [ ! -f "$(BACKUP_PATH)" ]; then \
	  echo "No backup at $(BACKUP_PATH); nothing to restore (env var cleared anyway). (Idempotent: this is OK.)"; \
	else \
	  set -e; \
	  brew services stop shed; \
	  chmod +w "$(BREW_SHED_BIN)"; \
	  cp -f "$(BACKUP_PATH)" "$(BREW_SHED_BIN)"; \
	  codesign --force -s - "$(BREW_SHED_BIN)"; \
	  chmod -w "$(BREW_SHED_BIN)"; \
	  brew services start shed; \
	  rm -f "$(BACKUP_PATH)"; \
	  echo "Brew shed-server restored from backup; backup removed."; \
	fi

# Chain: install dev binary locally, run the integration suite against
# it, restore the brew binary REGARDLESS of suite outcome. Captures
# BOTH the suite and the restore exit codes — a restore failure is at
# least as serious as a suite failure (it strands the host), so the
# chain target reports non-zero if either step fails.
test-integration-local: install-local-server
	@$(MAKE) test-integration; SUITE=$$?; \
	 $(MAKE) restore-brew-server; RESTORE=$$?; \
	 if [ $$SUITE -ne 0 ] && [ $$RESTORE -ne 0 ]; then \
	   echo "test-integration-local: suite FAILED (exit $$SUITE) AND restore FAILED (exit $$RESTORE); inspect host state"; \
	   exit $$SUITE; \
	 elif [ $$SUITE -ne 0 ]; then \
	   echo "test-integration-local: suite FAILED (exit $$SUITE); restore succeeded"; \
	   exit $$SUITE; \
	 elif [ $$RESTORE -ne 0 ]; then \
	   echo "test-integration-local: suite passed BUT restore FAILED (exit $$RESTORE); inspect host state"; \
	   exit $$RESTORE; \
	 fi

# Remote dev-binary swap + integration suite + restore for the FC
# backend on `$SHED_FC_HOST` (default `mini3`) over SSH. The FC sibling
# of test-integration-local: closes the same gap for FC-side PRs.
# Together they make the workflow promise — every server-side PR (VZ
# or FC) can validate against its own branch in one command — true.
#
# Assumes (and the install recipe sanity-checks):
#  - Passwordless SSH from this host to $(FC_REMOTE_HOST).
#  - Passwordless sudo on the remote for the SSH user (the integration
#    suite's journalctl read already requires this, so this is the
#    same bar).
#  - shed-server on the remote installed at $(FC_REMOTE_BIN_PATH) and
#    run by systemd as shed-server.service. Default path is
#    /usr/local/bin/shed-server, matching the deb's install location
#    (verified on mini3 v0.5.8: ExecStart=/usr/local/bin/shed-server).
#    Override with FC_REMOTE_BIN_PATH=... if the deploy location moves.

FC_REMOTE_HOST ?= $(or $(SHED_FC_HOST),mini3)
FC_REMOTE_BIN_PATH ?= /usr/local/bin/shed-server
FC_REMOTE_BACKUP := /tmp/shed-server-deb.bak
FC_REMOTE_ENVOVERRIDE := /etc/systemd/system/shed-server.service.d/dev-override.conf

# Cross-compile shed-server for the remote host's GOARCH. Detects arch
# at recipe time via `ssh <host> uname -m`; refuses to silently default
# (a mismatch here produces a "cannot execute binary" failure later
# that's painful to debug). Today mini3 is x86_64 → amd64; future
# arm64 Linux boxes work without code changes. Always builds to a
# fixed output path so install-remote-server doesn't need to re-detect.
build-fc-remote-server:
	@ARCH=$$(ssh -o BatchMode=yes -o ConnectTimeout=5 $(FC_REMOTE_HOST) "uname -m" 2>/dev/null); \
	 case "$$ARCH" in \
	   x86_64)  GOARCH=amd64 ;; \
	   aarch64) GOARCH=arm64 ;; \
	   "")  echo "ERROR: could not detect arch on $(FC_REMOTE_HOST); is SSH reachable?"; exit 1 ;; \
	   *)   echo "ERROR: unsupported remote arch on $(FC_REMOTE_HOST): $$ARCH"; exit 1 ;; \
	 esac; \
	 echo "Cross-compiling shed-server for linux/$$GOARCH (remote $(FC_REMOTE_HOST) is $$ARCH)..."; \
	 GOOS=linux GOARCH=$$GOARCH go build $(LDFLAGS) -o bin/shed-server-fc-remote ./cmd/shed-server

# Swap the just-built dev binary into the remote at $(FC_REMOTE_BIN_PATH),
# back up the deb-installed binary on the REMOTE (so a developer
# rebooting their workstation mid-test can still recover the host via
# `make restore-remote-server`), drop a systemd Environment= override
# for SHED_BUILD_TOOLS_REF, daemon-reload, restart shed-server.
# Refuses to clobber an existing backup unless FORCE=1, matching the
# local install-local-server safety pattern.
#
# WARNING: this swaps the binary on the SHARED dev/test host. Any
# active sessions against $(FC_REMOTE_HOST) will see the service
# restart. Don't run while another developer is mid-create.
install-remote-server: build-fc-remote-server
	@if [ -z "$(RELEASE_BUILD_TOOLS_REF)" ]; then \
	  echo "ERROR: RELEASE_BUILD_TOOLS_REF is empty; can't infer from git tag."; \
	  echo "       Either tag this repo (git tag --list 'v*' returned nothing)"; \
	  echo "       or override: RELEASE_BUILD_TOOLS_REF=ghcr.io/charliek/shed-build-tools:vX.Y.Z"; exit 1; \
	fi
	@ssh -o BatchMode=yes -o ConnectTimeout=5 $(FC_REMOTE_HOST) \
	  "test ! -f $(FC_REMOTE_BACKUP) || ( [ '$(FORCE)' = '1' ] || \
	   ( echo 'ERROR: backup already exists at $(FC_REMOTE_HOST):$(FC_REMOTE_BACKUP); run make restore-remote-server first, or pass FORCE=1' && exit 1 ) )"
	@scp bin/shed-server-fc-remote $(FC_REMOTE_HOST):/tmp/shed-server-dev
	@ssh -o BatchMode=yes $(FC_REMOTE_HOST) "set -e; \
	  sudo cp $(FC_REMOTE_BIN_PATH) $(FC_REMOTE_BACKUP); \
	  sudo systemctl stop shed-server; \
	  sudo install -m 755 /tmp/shed-server-dev $(FC_REMOTE_BIN_PATH); \
	  sudo mkdir -p $$(dirname $(FC_REMOTE_ENVOVERRIDE)); \
	  printf '[Service]\nEnvironment=SHED_BUILD_TOOLS_REF=$(RELEASE_BUILD_TOOLS_REF)\n' | sudo tee $(FC_REMOTE_ENVOVERRIDE) > /dev/null; \
	  sudo systemctl daemon-reload; \
	  sudo systemctl start shed-server; \
	  rm -f /tmp/shed-server-dev"
	@# systemctl start returns before the service has bound 8080. Poll
	@# the local `shed -s $(FC_REMOTE_HOST) list` (matches the suite's
	@# probe) so the chain target's suite invocation doesn't race the
	@# startup and skip all FC tests.
	@for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do \
	  if shed -s $(FC_REMOTE_HOST) list >/dev/null 2>&1; then \
	    echo "Remote FC shed-server on $(FC_REMOTE_HOST) ready after $${i}s."; break; \
	  fi; \
	  if [ "$$i" = "15" ]; then \
	    echo "WARNING: $(FC_REMOTE_HOST) shed-server not reachable after 15 s; integration suite may skip FC tests."; \
	  fi; \
	  sleep 1; \
	done
	@echo ""
	@echo "Dev shed-server installed on $(FC_REMOTE_HOST) at $(FC_REMOTE_BIN_PATH)."
	@echo "Build-tools ref (via $(FC_REMOTE_ENVOVERRIDE)): $(RELEASE_BUILD_TOOLS_REF)"
	@echo "Backup at $(FC_REMOTE_HOST):$(FC_REMOTE_BACKUP)."
	@echo "Run 'make restore-remote-server' to revert (or it runs automatically"
	@echo "at the end of 'make test-integration-local-fc')."

# Reverse of install-remote-server. Idempotent: a no-op when no backup
# exists. The systemd drop-in is removed in BOTH branches so a
# stranded override doesn't survive a manual restore.
restore-remote-server:
	@# Always remove the env override + daemon-reload, even if the
	@# binary backup is missing — same reasoning as restore-brew-server's
	@# unconditional launchctl unsetenv: the override is the companion
	@# to the binary swap, and restoring the binary without removing
	@# the override leaves dev-binary behavior wired into shed-server.
	@ssh -o BatchMode=yes -o ConnectTimeout=5 $(FC_REMOTE_HOST) "set -e; \
	  if [ ! -f $(FC_REMOTE_BACKUP) ]; then \
	    echo 'No backup at $(FC_REMOTE_HOST):$(FC_REMOTE_BACKUP); nothing to restore.'; \
	    if [ -f $(FC_REMOTE_ENVOVERRIDE) ]; then \
	      echo 'Removing stranded env override $(FC_REMOTE_ENVOVERRIDE)...'; \
	      sudo rm -f $(FC_REMOTE_ENVOVERRIDE); \
	      sudo systemctl daemon-reload; \
	      sudo systemctl restart shed-server; \
	    fi; \
	  else \
	    sudo systemctl stop shed-server; \
	    sudo install -m 755 $(FC_REMOTE_BACKUP) $(FC_REMOTE_BIN_PATH); \
	    sudo rm -f $(FC_REMOTE_ENVOVERRIDE) $(FC_REMOTE_BACKUP); \
	    sudo systemctl daemon-reload; \
	    sudo systemctl start shed-server; \
	    echo 'Remote shed-server restored from backup; backup + env override removed.'; \
	  fi"

# Chain: install dev binary on the remote, run integration suite
# against it (via SHED_FC_HOST), restore the remote binary REGARDLESS
# of suite outcome. Same exit-code propagation pattern as
# test-integration-local: surfaces a restore failure separately
# rather than silently swallowing it.
test-integration-local-fc: install-remote-server
	@SHED_FC_HOST=$(FC_REMOTE_HOST) $(MAKE) test-integration; SUITE=$$?; \
	 $(MAKE) restore-remote-server; RESTORE=$$?; \
	 if [ $$SUITE -ne 0 ] && [ $$RESTORE -ne 0 ]; then \
	   echo "test-integration-local-fc: suite FAILED (exit $$SUITE) AND restore FAILED (exit $$RESTORE); inspect $(FC_REMOTE_HOST) state"; \
	   exit $$SUITE; \
	 elif [ $$SUITE -ne 0 ]; then \
	   echo "test-integration-local-fc: suite FAILED (exit $$SUITE); restore succeeded"; \
	   exit $$SUITE; \
	 elif [ $$RESTORE -ne 0 ]; then \
	   echo "test-integration-local-fc: suite passed BUT restore FAILED (exit $$RESTORE); inspect $(FC_REMOTE_HOST) state"; \
	   exit $$RESTORE; \
	 fi

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
