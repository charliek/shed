.PHONY: build build-cli build-server build-egress-proxy build-agent build-firstboot build-host-agent build-machine-rc build-tools build-fc-remote-server test test-integration test-integration-dev test-integration-dev-fc dev-server-up dev-server-down dev-server-status dev-server-logs dev-server-restart dev-server-up-fc dev-server-down-fc dev-server-status-fc dev-server-logs-fc dev-server-restart-fc release clean dev-server dev-cli check check-kernel-pin coverage lint-all docs docs-serve firecracker-rootfs download-firecracker vz-rootfs vz-rootfs-base vz-rootfs-all

GOARCH ?= $(shell go env GOARCH)

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -ldflags "-X github.com/charliek/shed/internal/version.Version=$(VERSION) -X github.com/charliek/shed/internal/version.GitCommit=$(GIT_COMMIT) -X github.com/charliek/shed/internal/version.BuildDate=$(BUILD_DATE)"

# Build all binaries
build: build-cli build-server build-egress-proxy build-agent build-firstboot build-host-agent build-machine-rc

# Build CLI only
build-cli:
	go build $(LDFLAGS) -o bin/shed ./cmd/shed

# Build server only
build-server:
	go build $(LDFLAGS) -o bin/shed-server ./cmd/shed-server

# Build the egress-control proxy (host-side child of shed-server)
build-egress-proxy:
	go build $(LDFLAGS) -o bin/shed-egress-proxy ./cmd/shed-egress-proxy

# Build shed-agent only (for Firecracker VMs)
build-agent:
	GOOS=linux GOARCH=$(GOARCH) go build $(LDFLAGS) -o bin/shed-agent ./cmd/shed-agent

# Build shed-firstboot only (in-VM oneshot for identity regen)
build-firstboot:
	GOOS=linux GOARCH=$(GOARCH) go build $(LDFLAGS) -o bin/shed-firstboot ./cmd/shed-firstboot

# Build shed-host-agent only (host-side agent; darwin CGO/Touch ID via build tags)
build-host-agent:
	go build $(LDFLAGS) -o bin/shed-host-agent ./cmd/shed-host-agent

# Build shed-machine-rc only (host-side machine rc helper)
build-machine-rc:
	go build $(LDFLAGS) -o bin/shed-machine-rc ./cmd/shed-machine-rc

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
	  if ! kill -0 $$PID 2>/dev/null; then \
	    echo "Dev server PID $$PID is stale (process already gone); cleaning up PID file."; \
	    rm -f "$(DEV_PID_PATH)"; \
	  elif ! ps -p $$PID -o comm= 2>/dev/null | grep -q "shed-server"; then \
	    echo "Dev server PID $$PID points at an unrelated process"; \
	    echo "  (current comm: $$(ps -p $$PID -o comm= 2>/dev/null))."; \
	    echo "  Refusing to send signals; cleaning up the stale PID file only."; \
	    echo "  If you have a stranded shed-server somewhere, find it with"; \
	    echo "  'pgrep -fl shed-server' and stop it by hand."; \
	    rm -f "$(DEV_PID_PATH)"; \
	  else \
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
	    rm -f "$(DEV_PID_PATH)"; \
	  fi; \
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
	@# Set BOTH the prod env-var pair (so the existing `shed_server`
	@# fixture in test_smoke / test_lifecycle / test_exec_shell retargets
	@# at the dev server) AND the dev env-var pair (so the
	@# `shed_server_dev` fixture activates for any test that explicitly
	@# uses it). Today's tests all use `shed_server`; future
	@# dev-specific meta-tests can use `shed_server_dev` without needing
	@# a different Make target.
	SHED_VZ_SERVER=$(SHED_VZ_DEV_SERVER) \
	  SHED_VZ_LOG_PATH=$(DEV_LOG_PATH) \
	  SHED_VZ_DEV_SERVER=$(SHED_VZ_DEV_SERVER) \
	  SHED_VZ_DEV_LOG_PATH=$(DEV_LOG_PATH) \
	  $(MAKE) test-integration

# Parallel dev shed-server lifecycle (FC remote).
#
# Runs a SECOND shed-server (the dev binary, cross-compiled for the
# remote's GOARCH) on $(FC_REMOTE_HOST) ALONGSIDE the deb-installed
# one. The deb server keeps running undisturbed on 8080/2222; the
# dev server runs on 18080/12222.
#
# Same shape as the Mac local dev-server-* targets, but the lifecycle
# happens over SSH (with sudo, because the FC backend needs
# CAP_NET_ADMIN for TAP/bridge operations). No systemd unit — the
# remote dev server runs via `sudo nohup` with a PID file in /tmp,
# matching the intentionally-ephemeral lifecycle of the Mac dev
# server (crashes visible, no survives-reboot).
#
# Setup (one-time per dev workstation):
#   1. Add a ~/.shed/config.yaml entry for $(SHED_FC_DEV_SERVER)
#      (snippet in CLAUDE.md / tests/integration/README.md), OR run
#      `shed server add $(FC_REMOTE_HOST) --port 18080 --name $(SHED_FC_DEV_SERVER)`
#      after the first `make dev-server-up-fc`.
#
# Assumed infrastructure (same as today's deb install):
#   - Passwordless SSH from this host to $(FC_REMOTE_HOST).
#   - sudo NOPASSWD on the remote for the SSH user. The recipes drive
#     systemctl, install, cp, rm, nohup, tail, cat, stat, kill, ps.
#   - The remote has FC + KVM available (which the deb shed-server
#     already requires).
#
# FC isolation notes:
#   - Different ports (18080/12222 vs deb's 8080/2222).
#   - Separate state-dirs under /var/lib/shed-dev/firecracker/.
#   - Offset vsock_base_cid=600 (deb default is 100) to avoid CID
#     collision. See configs/server.dev-parallel.linux-fc.yaml.
#   - SHARED bridge/CIDR/tap_prefix with the deb server. Kernel-level
#     TAP existence check in `internal/firecracker/network.go:
#     FindAvailableTAPIndex` coordinates across the two servers.

FC_REMOTE_HOST ?= $(or $(SHED_FC_HOST),mini3)

# Shed CLI entry name for the deb FC server (today's default).
# Mirrors `SHED_FC_SERVER` in the test suite's conftest.
SHED_FC_SERVER ?= $(FC_REMOTE_HOST)

# Shed CLI entry name for the parallel dev FC server. Mirrors
# `SHED_FC_DEV_SERVER` in the conftest. By convention `<host>-dev`.
SHED_FC_DEV_SERVER ?= $(FC_REMOTE_HOST)-dev

# Remote paths for the parallel dev FC server's binary, config, log,
# PID file. Lives in /tmp because nothing here is meant to survive a
# reboot (no systemd unit). Override via env vars if your remote is
# unusual.
FC_DEV_BIN_PATH  ?= /tmp/shed-server-dev
FC_DEV_CONFIG    ?= /tmp/shed-server-dev.yaml
FC_DEV_LOG_PATH  ?= /tmp/shed-server-dev.log
FC_DEV_PID_PATH  ?= /tmp/shed-server-dev.pid
# The egress proxy must sit next to the running shed-server binary so it
# resolves as its sibling. The dev server runs from /tmp, so install here.
FC_DEV_PROXY_PATH ?= /tmp/shed-egress-proxy

# Cross-compile shed-server for the remote host's GOARCH. Detects arch
# at recipe time via `ssh <host> uname -m`; refuses to silently default
# (a mismatch here produces a "cannot execute binary" failure later
# that's painful to debug). Today mini3 is x86_64 → amd64; future
# arm64 Linux boxes work without code changes. Always builds to a
# fixed output path.
build-fc-remote-server:
	@ARCH=$$(ssh -o BatchMode=yes -o ConnectTimeout=5 $(FC_REMOTE_HOST) "uname -m" 2>/dev/null); \
	 case "$$ARCH" in \
	   x86_64)  GOARCH=amd64 ;; \
	   aarch64) GOARCH=arm64 ;; \
	   "")  echo "ERROR: could not detect arch on $(FC_REMOTE_HOST); is SSH reachable?"; exit 1 ;; \
	   *)   echo "ERROR: unsupported remote arch on $(FC_REMOTE_HOST): $$ARCH"; exit 1 ;; \
	 esac; \
	 echo "Cross-compiling shed-server + shed-egress-proxy for linux/$$GOARCH (remote $(FC_REMOTE_HOST) is $$ARCH)..."; \
	 GOOS=linux GOARCH=$$GOARCH go build $(LDFLAGS) -o bin/shed-server-fc-remote ./cmd/shed-server && \
	 GOOS=linux GOARCH=$$GOARCH go build $(LDFLAGS) -o bin/shed-egress-proxy-fc-remote ./cmd/shed-egress-proxy

# Launch the parallel dev shed-server on the remote via sudo nohup.
# Refuses to start if a dev server is already running there (PID file
# check, with comm verification). Polls `shed -s $(SHED_FC_DEV_SERVER)
# list` for readiness (15 s).
#
# WARNING: this starts a SECOND shed-server on the shared FC host.
# Any other developer using $(FC_REMOTE_HOST) won't see the deb
# server change, but if they're also running their own dev server
# on the same ports, this will conflict. Coordinate.
dev-server-up-fc: build-fc-remote-server
	@if [ -z "$(RELEASE_BUILD_TOOLS_REF)" ]; then \
	  echo "ERROR: RELEASE_BUILD_TOOLS_REF is empty; can't infer from git tag."; \
	  echo "       Either tag this repo or override on the command line."; exit 1; \
	fi
	@# Refuse to start if a dev server is already running on the remote.
	@# Same shape as the Mac local dev-server-up's PID check.
	@# Use `ps -p PID` instead of `kill -0` for the liveness check —
	@# the dev server runs as root via sudo nohup, and `kill -0` from
	@# the SSH user can't signal a root-owned process even though it's
	@# alive. `ps -p PID` works cross-user.
	@if ssh -o BatchMode=yes -o ConnectTimeout=5 $(FC_REMOTE_HOST) \
	     "test -f $(FC_DEV_PID_PATH) && ps -p \$$(sudo -n cat $(FC_DEV_PID_PATH) 2>/dev/null) -o comm= 2>/dev/null | grep -q shed-server" 2>/dev/null; then \
	  echo "ERROR: dev server already running on $(FC_REMOTE_HOST) (pid in $(FC_DEV_PID_PATH))."; \
	  echo "       Use 'make dev-server-restart-fc' to pick up a rebuild,"; \
	  echo "       or 'make dev-server-down-fc' to stop it first."; \
	  exit 1; \
	fi
	@# scp binary + config to the remote. PID-unique tmp suffix so two
	@# concurrent install runs on the same host don't clobber each
	@# other's uploaded files (belt-and-suspenders — the dev
	@# workstation is single-user).
	@UPLOAD_BIN=/tmp/shed-server-dev.upload.$$$$; \
	 UPLOAD_CFG=/tmp/shed-server-dev.upload.$$$$.yaml; \
	 UPLOAD_PROXY=/tmp/shed-egress-proxy-dev.upload.$$$$; \
	 scp bin/shed-server-fc-remote $(FC_REMOTE_HOST):$$UPLOAD_BIN && \
	 scp bin/shed-egress-proxy-fc-remote $(FC_REMOTE_HOST):$$UPLOAD_PROXY && \
	 scp configs/server.dev-parallel.linux-fc.yaml $(FC_REMOTE_HOST):$$UPLOAD_CFG && \
	 ssh -o BatchMode=yes $(FC_REMOTE_HOST) "set -e; \
	   sudo install -m 755 $$UPLOAD_BIN $(FC_DEV_BIN_PATH); \
	   sudo install -m 755 $$UPLOAD_PROXY $(FC_DEV_PROXY_PATH); \
	   sudo install -m 644 $$UPLOAD_CFG $(FC_DEV_CONFIG); \
	   rm -f $$UPLOAD_BIN $$UPLOAD_PROXY $$UPLOAD_CFG; \
	   sudo install -m 644 /dev/null $(FC_DEV_LOG_PATH); \
	   sudo bash -c 'SHED_BUILD_TOOLS_REF=\"$(RELEASE_BUILD_TOOLS_REF)\" \
	     nohup $(FC_DEV_BIN_PATH) serve --config $(FC_DEV_CONFIG) \
	     >> $(FC_DEV_LOG_PATH) 2>&1 < /dev/null & \
	     echo \$$! > $(FC_DEV_PID_PATH)'"
	@# Readiness probe — same shape as the Mac local dev-server-up's.
	@for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do \
	  if shed -s $(SHED_FC_DEV_SERVER) list >/dev/null 2>&1; then \
	    echo "Remote FC dev shed-server ($(SHED_FC_DEV_SERVER) on $(FC_REMOTE_HOST)) ready after $${i}s."; \
	    break; \
	  fi; \
	  if [ "$$i" = "15" ]; then \
	    echo "WARNING: FC dev shed-server ($(SHED_FC_DEV_SERVER) on $(FC_REMOTE_HOST)) not reachable after 15 s."; \
	    echo "  - Tail the remote log:  make dev-server-logs-fc"; \
	    echo "  - CLI entry:            verify ~/.shed/config.yaml has '$(SHED_FC_DEV_SERVER)' on $(FC_REMOTE_HOST):18080"; \
	    echo "  - Deb server:           should be unaffected; 'shed -s $(SHED_FC_SERVER) list' still works"; \
	  fi; \
	  sleep 1; \
	done
	@echo ""
	@echo "Dev FC shed-server up on $(FC_REMOTE_HOST):"
	@echo "  Binary:  $(FC_DEV_BIN_PATH)"
	@echo "  Config:  $(FC_DEV_CONFIG)"
	@echo "  Log:     $(FC_DEV_LOG_PATH)"
	@echo "  PID:     $(FC_DEV_PID_PATH)"
	@echo "  CLI:     shed -s $(SHED_FC_DEV_SERVER) list"
	@echo "Build-tools ref (via SHED_BUILD_TOOLS_REF env): $(RELEASE_BUILD_TOOLS_REF)"
	@echo "Run 'make test-integration-dev-fc' to run the suite against it."

dev-server-down-fc:
	@# Same PID-safety shape as Mac dev-server-down: verify the PID
	@# points at a shed-server process before sending signals, so a
	@# stale PID file pointing at an unrelated remote process doesn't
	@# get killed. Liveness is checked via `ps -p PID -o comm=` rather
	@# than `kill -0` because the dev server runs as root (sudo nohup)
	@# and the SSH user's `kill -0` can't signal a root-owned process
	@# (would report 'dead' for a live root-owned process).
	@ssh -o BatchMode=yes -o ConnectTimeout=5 $(FC_REMOTE_HOST) "set -e; \
	  if [ ! -f $(FC_DEV_PID_PATH) ]; then \
	    echo 'Dev FC server not running (no PID file at $(FC_REMOTE_HOST):$(FC_DEV_PID_PATH)).'; \
	    echo '  (Idempotent: this is OK.)'; \
	    exit 0; \
	  fi; \
	  PID=\$$(sudo -n cat $(FC_DEV_PID_PATH) 2>/dev/null || cat $(FC_DEV_PID_PATH)); \
	  COMM=\$$(ps -p \$$PID -o comm= 2>/dev/null || true); \
	  if [ -z \"\$$COMM\" ]; then \
	    echo \"Dev FC server PID \$$PID is stale (process already gone); cleaning up PID file.\"; \
	    sudo rm -f $(FC_DEV_PID_PATH) $(FC_DEV_LOG_PATH); \
	  elif ! echo \"\$$COMM\" | grep -q 'shed-server'; then \
	    echo \"Dev FC server PID \$$PID points at an unrelated process (current comm: \$$COMM).\"; \
	    echo '  Refusing to send signals; cleaning up the stale PID file only.'; \
	    sudo rm -f $(FC_DEV_PID_PATH); \
	  else \
	    sudo -n kill -TERM \$$PID 2>/dev/null; \
	    for i in 1 2 3 4 5; do \
	      if [ -z \"\$$(ps -p \$$PID -o comm= 2>/dev/null)\" ]; then break; fi; \
	      sleep 1; \
	    done; \
	    if [ -n \"\$$(ps -p \$$PID -o comm= 2>/dev/null)\" ]; then \
	      echo \"Dev FC server didn't stop gracefully after 5 s; SIGKILL\"; \
	      sudo -n kill -KILL \$$PID 2>/dev/null; \
	    fi; \
	    echo \"Dev FC server (pid \$$PID) stopped on $(FC_REMOTE_HOST).\"; \
	    sudo rm -f $(FC_DEV_PID_PATH) $(FC_DEV_BIN_PATH) $(FC_DEV_PROXY_PATH) $(FC_DEV_CONFIG) $(FC_DEV_LOG_PATH); \
	  fi"

dev-server-status-fc:
	@# `ps -p PID -o comm=` instead of `kill -0`: the dev server runs
	@# as root and the SSH user's `kill -0` can't signal it.
	@ssh -o BatchMode=yes -o ConnectTimeout=5 $(FC_REMOTE_HOST) "\
	  if [ -f $(FC_DEV_PID_PATH) ]; then \
	    PID=\$$(sudo -n cat $(FC_DEV_PID_PATH) 2>/dev/null || cat $(FC_DEV_PID_PATH)); \
	    COMM=\$$(ps -p \$$PID -o comm= 2>/dev/null || true); \
	    if [ -n \"\$$COMM\" ] && echo \"\$$COMM\" | grep -q 'shed-server'; then \
	      echo \"Dev FC server: running on $(FC_REMOTE_HOST) (pid \$$PID)\"; \
	    else \
	      echo 'Dev FC server: NOT running on $(FC_REMOTE_HOST)'; \
	      echo '  (Stale PID file at $(FC_DEV_PID_PATH); make dev-server-down-fc will clean up.)'; \
	    fi; \
	  else \
	    echo 'Dev FC server: NOT running on $(FC_REMOTE_HOST)'; \
	  fi"
	@if shed -s $(SHED_FC_DEV_SERVER) list >/dev/null 2>&1; then \
	  echo "Reachable:     YES (shed -s $(SHED_FC_DEV_SERVER) list succeeds)"; \
	else \
	  echo "Reachable:     NO (shed -s $(SHED_FC_DEV_SERVER) list failed)"; \
	fi
	@echo "Log:           $(FC_REMOTE_HOST):$(FC_DEV_LOG_PATH)"
	@echo "Config:        $(FC_REMOTE_HOST):$(FC_DEV_CONFIG)"

dev-server-logs-fc:
	@ssh -t $(FC_REMOTE_HOST) "sudo -n tail -F $(FC_DEV_LOG_PATH)"

dev-server-restart-fc: dev-server-down-fc dev-server-up-fc

# Run the integration suite against the parallel dev FC shed-server.
# Auto-ups the dev server if it isn't already running. Doesn't
# auto-stop after — the dev server is intentionally long-lived for
# iterative work. Use `make dev-server-down-fc` to stop.
#
# Sets BOTH the prod-fixture vars (SHED_FC_HOST + SHED_FC_SERVER, so
# the existing fc_server fixture retargets at the dev server) AND the
# dev-fixture vars (SHED_FC_DEV_SERVER + SHED_FC_DEV_LOG_PATH, so the
# fc_server_dev fixture activates for future dev-specific tests).
# VZ tests still target the brew server unless you also set the
# Mac dev env vars; the typical dev workflow runs test-integration-dev
# OR test-integration-dev-fc depending on which backend you're
# changing.
test-integration-dev-fc:
	@# Auto-up if not running on the remote (`ps -p PID` check; the
	@# dev server runs as root and the SSH user's `kill -0` can't
	@# signal a root-owned process).
	@if ! ssh -o BatchMode=yes -o ConnectTimeout=5 $(FC_REMOTE_HOST) \
	     "test -f $(FC_DEV_PID_PATH) && ps -p \$$(sudo -n cat $(FC_DEV_PID_PATH) 2>/dev/null) -o comm= 2>/dev/null | grep -q shed-server" 2>/dev/null; then \
	  echo "Dev FC server not running on $(FC_REMOTE_HOST); starting via dev-server-up-fc..."; \
	  $(MAKE) dev-server-up-fc; \
	fi
	SHED_FC_HOST=$(FC_REMOTE_HOST) \
	  SHED_FC_SERVER=$(SHED_FC_DEV_SERVER) \
	  SHED_FC_LOG_PATH=$(FC_DEV_LOG_PATH) \
	  SHED_FC_DEV_SERVER=$(SHED_FC_DEV_SERVER) \
	  SHED_FC_DEV_LOG_PATH=$(FC_DEV_LOG_PATH) \
	  $(MAKE) test-integration

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
