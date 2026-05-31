.PHONY: build build-cli build-server build-agent build-firstboot build-tools build-fc-remote-server test test-integration test-integration-dev test-integration-local-fc install-remote-server restore-remote-server dev-server-up dev-server-down dev-server-status dev-server-logs dev-server-restart release clean dev-server dev-cli check check-kernel-pin coverage lint-all docs docs-serve firecracker-rootfs download-firecracker vz-rootfs vz-rootfs-base vz-rootfs-all

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

# Parallel dev shed-server lifecycle (Mac VZ).
#
# Runs the just-built bin/shed-server ALONGSIDE the brew-installed
# shed-server on a different port (18080/12222). The brew server keeps
# running undisturbed; the dev server is for testing server-side
# changes against the developer's source tree.
#
# Workflow:
#   1. make dev-server-up                # once (or after rebuild via -restart)
#   2. make test-integration-dev         # runs the suite against the dev server
#   3. ... iterate on source ...
#   4. make build && make dev-server-restart  # pick up the new binary
#   5. make dev-server-down              # when done
#
# Setup (one-time per developer):
#   - Add a ~/.shed/config.yaml entry for $(SHED_VZ_DEV_SERVER); see
#     CLAUDE.md or tests/integration/README.md for the snippet.
#
# Why no launchd plist:
#   The dev server is intentionally ephemeral — no auto-restart on
#   crash (crashes should be visible), no survives-reboot (re-up after
#   reboot). `nohup` + PID file gives clean start/stop/restart
#   semantics without an Apple-specific lifecycle layer.

# Shed CLI entry name for the brew VZ server (the entry in
# `~/.shed/config.yaml`). Mirrors `SHED_VZ_SERVER` in the test suite.
SHED_VZ_SERVER ?= my-server

# Shed CLI entry name for the parallel dev VZ server. Mirrors
# `SHED_VZ_DEV_SERVER` in the test suite's conftest.
SHED_VZ_DEV_SERVER ?= my-server-dev

DEV_LOG_PATH := $(HOME)/.shed/dev/server.log
DEV_PID_PATH := $(HOME)/.shed/dev/server.pid
DEV_CONFIG   := $(CURDIR)/configs/server.dev-parallel.mac.yaml

# shed-build-tools image ref to inject when the dev binary runs.
# Dev binaries embed Version="vX.Y.Z-N-gHASH-dirty", which
# BuildToolsRefForTag returns "" for by design (dev-build isolation).
# Without the env var the dev binary falls back to in-guest mkfs.ext4
# on first boot (~4 s on VZ), which the split timing gate (PR #157)
# skips cleanly but isn't the path we want when validating server
# changes against release-shaped behavior. Override with
# `RELEASE_BUILD_TOOLS_REF=ghcr.io/charliek/shed-build-tools:vX.Y.Z`
# to pin to a non-latest release.
RELEASE_BUILD_TOOLS_REF ?= $(shell git tag --list 'v*' --sort=-version:refname | head -1 | sed 's|^|ghcr.io/charliek/shed-build-tools:|')

dev-server-up: build
	@case "$$(uname -s)" in \
	  Darwin) ;; \
	  *) echo "ERROR: dev-server-up targets the local Mac VZ shed-server."; \
	     echo "       For the FC remote workflow see 'make dev-server-up-fc'."; exit 1 ;; \
	esac
	@mkdir -p "$(HOME)/.shed/dev"
	@# Refuse if already running — `dev-server-restart` is the way to
	@# pick up a rebuilt binary. Plain `dev-server-up` should never
	@# silently kill an existing server.
	@if [ -f "$(DEV_PID_PATH)" ] && kill -0 "$$(cat $(DEV_PID_PATH))" 2>/dev/null; then \
	  echo "ERROR: dev server already running (pid $$(cat $(DEV_PID_PATH)))."; \
	  echo "       Use 'make dev-server-restart' to pick up a rebuild,"; \
	  echo "       or 'make dev-server-down' to stop it first."; \
	  exit 1; \
	fi
	@# Inline env so we don't pollute the caller's launchctl domain
	@# (and so the brew server's launchctl env is left alone). Note
	@# the env var name: shed-server reads SHED_BUILD_TOOLS_REF; the
	@# Makefile-level variable that holds its value is named
	@# RELEASE_BUILD_TOOLS_REF (because it points at the *release*
	@# tooling image).
	@SHED_BUILD_TOOLS_REF="$(RELEASE_BUILD_TOOLS_REF)" \
	  nohup bin/shed-server serve --config "$(DEV_CONFIG)" \
	    > "$(DEV_LOG_PATH)" 2>&1 & \
	  echo $$! > "$(DEV_PID_PATH)"
	@# Readiness probe — same shape used everywhere in this Makefile.
	@for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do \
	  if shed -s $(SHED_VZ_DEV_SERVER) list >/dev/null 2>&1; then \
	    echo "Dev VZ shed-server ($(SHED_VZ_DEV_SERVER)) ready after $${i}s."; \
	    break; \
	  fi; \
	  if [ "$$i" = "15" ]; then \
	    echo "WARNING: Dev VZ shed-server ($(SHED_VZ_DEV_SERVER)) not reachable after 15 s."; \
	    echo "  - Tail the log:  make dev-server-logs   (or 'tail $(DEV_LOG_PATH)')"; \
	    echo "  - CLI entry:     verify ~/.shed/config.yaml has '$(SHED_VZ_DEV_SERVER)' on port 18080"; \
	    echo "  - Brew server:   should be unaffected; 'shed -s $(SHED_VZ_SERVER) list' still works"; \
	  fi; \
	  sleep 1; \
	done
	@echo ""
	@echo "Dev shed-server up: pid $$(cat $(DEV_PID_PATH))"
	@echo "  Config:  $(DEV_CONFIG)"
	@echo "  Log:     $(DEV_LOG_PATH)"
	@echo "  CLI:     shed -s $(SHED_VZ_DEV_SERVER) list"
	@echo "Run 'make test-integration-dev' to run the suite against it."

dev-server-down:
	@case "$$(uname -s)" in \
	  Darwin) ;; \
	  *) echo "ERROR: dev-server-down is macOS-only (Mac VZ)."; exit 1 ;; \
	esac
	@# Single shell block so the no-PID-file branch can short-circuit
	@# cleanly (a separate `@if ... exit 0` line only exits its
	@# sub-shell, not the recipe — make would continue to the next
	@# `@cat $(DEV_PID_PATH)` line and emit a confusing "No such file
	@# or directory").
	@if [ ! -f "$(DEV_PID_PATH)" ]; then \
	  echo "Dev server not running (no PID file at $(DEV_PID_PATH))."; \
	  echo "  (Idempotent: this is OK.)"; \
	else \
	  PID=$$(cat "$(DEV_PID_PATH)"); \
	  if kill -0 $$PID 2>/dev/null; then \
	    kill -TERM $$PID 2>/dev/null; \
	    for i in 1 2 3 4 5; do \
	      if ! kill -0 $$PID 2>/dev/null; then break; fi; \
	      sleep 1; \
	    done; \
	    if kill -0 $$PID 2>/dev/null; then \
	      echo "Dev server didn't stop gracefully after 5s; SIGKILL"; \
	      kill -KILL $$PID 2>/dev/null; \
	    fi; \
	    echo "Dev server (pid $$PID) stopped."; \
	  else \
	    echo "Dev server PID $$PID is stale (process already gone); cleaning up PID file."; \
	  fi; \
	  rm -f "$(DEV_PID_PATH)"; \
	fi

dev-server-status:
	@case "$$(uname -s)" in \
	  Darwin) ;; \
	  *) echo "ERROR: dev-server-status is macOS-only (Mac VZ)."; exit 1 ;; \
	esac
	@if [ -f "$(DEV_PID_PATH)" ] && kill -0 "$$(cat $(DEV_PID_PATH))" 2>/dev/null; then \
	  PID=$$(cat "$(DEV_PID_PATH)"); \
	  echo "Dev server: running (pid $$PID)"; \
	  if shed -s $(SHED_VZ_DEV_SERVER) list >/dev/null 2>&1; then \
	    echo "Reachable:  YES (shed -s $(SHED_VZ_DEV_SERVER) list succeeds)"; \
	  else \
	    echo "Reachable:  NO (shed -s $(SHED_VZ_DEV_SERVER) list failed)"; \
	    echo "            (process is running but the CLI can't reach it —"; \
	    echo "             check ~/.shed/config.yaml has '$(SHED_VZ_DEV_SERVER)')"; \
	  fi; \
	  echo "Log:        $(DEV_LOG_PATH)"; \
	  echo "Config:     $(DEV_CONFIG)"; \
	else \
	  echo "Dev server: NOT running"; \
	  if [ -f "$(DEV_PID_PATH)" ]; then \
	    echo "  (Stale PID file at $(DEV_PID_PATH); 'make dev-server-down' will clean up.)"; \
	  fi; \
	fi

dev-server-logs:
	@if [ ! -f "$(DEV_LOG_PATH)" ]; then \
	  echo "No log at $(DEV_LOG_PATH). Run 'make dev-server-up' first."; \
	  exit 1; \
	fi
	@tail -F "$(DEV_LOG_PATH)"

dev-server-restart: dev-server-down dev-server-up

# Run the integration suite against the parallel dev shed-server.
# Auto-starts the dev server if it isn't already running. Doesn't
# auto-stop after — the dev server is intentionally long-lived for
# iterative work. Use 'make dev-server-down' to stop.
#
# The suite is env-var-retargeted: SHED_VZ_SERVER points at the dev
# entry, SHED_VZ_LOG_PATH points at the dev log file. The existing
# `shed_server` fixture (parameterized over [vz, fc]) honors these
# without any test-side changes — VZ tests run against the dev server;
# FC tests still target $(FC_REMOTE_HOST) (the deb prod on mini3) as
# in `make test-integration`. PR 3 adds the FC parallel-dev sibling
# `test-integration-dev-fc` for testing FC changes.
#
# If you rebuilt the source but the dev server is still running the
# old binary, run 'make dev-server-restart' before this target.
test-integration-dev: build
	@case "$$(uname -s)" in \
	  Darwin) ;; \
	  *) echo "ERROR: test-integration-dev targets the local Mac VZ dev server."; \
	     echo "       For the FC remote workflow see 'make test-integration-dev-fc'."; exit 1 ;; \
	esac
	@if [ ! -f "$(DEV_PID_PATH)" ] || ! kill -0 "$$(cat $(DEV_PID_PATH))" 2>/dev/null; then \
	  echo "Dev server not running; starting via dev-server-up..."; \
	  $(MAKE) dev-server-up; \
	fi
	SHED_VZ_SERVER=$(SHED_VZ_DEV_SERVER) \
	  SHED_VZ_LOG_PATH=$(DEV_LOG_PATH) \
	  $(MAKE) test-integration

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

# Shed CLI entry name for the FC server. Mirrors the integration
# suite's `SHED_FC_SERVER` env var (see tests/integration/conftest.py):
# defaults to `$(FC_REMOTE_HOST)` (the same string), but a developer
# whose `~/.shed/config.yaml` entry differs from the SSH hostname can
# override with SHED_FC_SERVER=... to align both the readiness probe
# AND the chain target's suite invocation.
SHED_FC_SERVER ?= $(FC_REMOTE_HOST)

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
	  echo "       or override: shed-build-tools image ref"; exit 1; \
	fi
	@# Use a PID-unique tmp path on the remote so two concurrent
	@# install-remote-server runs on the same host don't clobber each
	@# other's uploaded binary at /tmp/shed-server-dev. Belt-and-suspenders
	@# — the dev workstation is single-user — but the cost is one $$
	@# substitution.
	@TMP=/tmp/shed-server-dev.$$$$; \
	 scp bin/shed-server-fc-remote $(FC_REMOTE_HOST):$$TMP && \
	 ssh -o BatchMode=yes $(FC_REMOTE_HOST) "set -e; \
	   trap 'rm -f $$TMP' EXIT; \
	   if [ -f $(FC_REMOTE_BACKUP) ] && [ '$(FORCE)' != '1' ]; then \
	     echo 'ERROR: backup already exists at $(FC_REMOTE_HOST):$(FC_REMOTE_BACKUP); run make restore-remote-server first, or pass FORCE=1'; \
	     exit 1; \
	   fi; \
	   sudo cp $(FC_REMOTE_BIN_PATH) $(FC_REMOTE_BACKUP); \
	   sudo systemctl stop shed-server; \
	   sudo install -m 755 $$TMP $(FC_REMOTE_BIN_PATH); \
	   sudo mkdir -p \$$(dirname $(FC_REMOTE_ENVOVERRIDE)); \
	   printf '[Service]\nEnvironment=SHED_BUILD_TOOLS_REF=$(RELEASE_BUILD_TOOLS_REF)\n' | sudo tee $(FC_REMOTE_ENVOVERRIDE) > /dev/null; \
	   sudo systemctl daemon-reload; \
	   sudo systemctl start shed-server"
	@# systemctl start returns before the service has bound 8080. Poll
	@# the local `shed -s $(FC_REMOTE_HOST) list` (matches the suite's
	@# probe) so the chain target's suite invocation doesn't race the
	@# startup and skip all FC tests.
	@for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do \
	  if shed -s $(SHED_FC_SERVER) list >/dev/null 2>&1; then \
	    echo "Remote FC shed-server ($(SHED_FC_SERVER) on $(FC_REMOTE_HOST)) ready after $${i}s."; break; \
	  fi; \
	  if [ "$$i" = "15" ]; then \
	    echo "WARNING: FC shed-server ($(SHED_FC_SERVER) on $(FC_REMOTE_HOST)) not reachable after 15 s; integration suite may skip FC tests."; \
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
# Same shape as test-integration-local: install in body (not as a
# prerequisite) so partial-failure installs reach the restore block,
# with pre/post snapshot so the auto-restore doesn't consume a
# pre-existing backup that belongs to a prior manual install.
test-integration-local-fc:
	@HAD_REMOTE_STATE=0; \
	 if ssh -o BatchMode=yes -o ConnectTimeout=5 $(FC_REMOTE_HOST) \
	      "test -f $(FC_REMOTE_BACKUP) || test -f $(FC_REMOTE_ENVOVERRIDE)" 2>/dev/null; then \
	   HAD_REMOTE_STATE=1; \
	 fi; \
	 $(MAKE) install-remote-server; INSTALL=$$?; \
	 if [ $$INSTALL -ne 0 ]; then \
	   HAS_REMOTE_STATE=0; \
	   if ssh -o BatchMode=yes -o ConnectTimeout=5 $(FC_REMOTE_HOST) \
	        "test -f $(FC_REMOTE_BACKUP) || test -f $(FC_REMOTE_ENVOVERRIDE)" 2>/dev/null; then \
	     HAS_REMOTE_STATE=1; \
	   fi; \
	   if [ $$HAD_REMOTE_STATE -eq 0 ] && [ $$HAS_REMOTE_STATE -eq 1 ]; then \
	     echo "test-integration-local-fc: install FAILED (exit $$INSTALL); NEW remote mutation on $(FC_REMOTE_HOST); running restore"; \
	     $(MAKE) restore-remote-server; \
	   else \
	     echo "test-integration-local-fc: install FAILED (exit $$INSTALL); no new mutation on $(FC_REMOTE_HOST) (any pre-existing state belongs to a prior install); leaving deb install alone"; \
	   fi; \
	   exit $$INSTALL; \
	 fi; \
	 SHED_FC_HOST=$(FC_REMOTE_HOST) SHED_FC_SERVER=$(SHED_FC_SERVER) $(MAKE) test-integration; SUITE=$$?; \
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
