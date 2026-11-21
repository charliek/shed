.PHONY: build build-cli build-server build-egress-proxy build-agent build-firstboot directory-ref build-tools build-fc-remote-server test test-integration test-integration-units test-integration-directory check-directory-itest-host test-broker-wire test-integration-dev test-integration-dev-fc check-dev-settings check-no-dev-auth-mode check-dev-users check-dev-config check-dev-config-fc check-remote-dev-config dev-itest-users check-remote-dev-settings dev-server-up dev-server-down dev-server-status dev-server-logs dev-server-restart dev-server-up-fc dev-server-down-fc dev-server-status-fc dev-server-logs-fc dev-server-restart-fc dev-server-up-remote dev-server-down-remote dev-server-status-remote dev-server-logs-remote dev-server-restart-remote test-integration-remote test-netns-remote release clean dev-server dev-cli check check-kernel-pin check-roost-pin check-tool-pins check-craze-pin check-gce-pins gce-image test-gce-image craze-binaries coverage lint-all docs docs-serve firecracker-rootfs download-firecracker vz-rootfs vz-rootfs-base vz-rootfs-all

GOARCH ?= $(shell go env GOARCH)

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
LDFLAGS := -ldflags "-X github.com/charliek/shed/internal/version.Version=$(VERSION) -X github.com/charliek/shed/internal/version.GitCommit=$(GIT_COMMIT) -X github.com/charliek/shed/internal/version.BuildDate=$(BUILD_DATE)"

# Build all binaries
build: build-cli build-server build-egress-proxy build-agent build-firstboot

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

# The directory contract's reference server, for tests only: plain HTTP on
# loopback, all state in memory (internal/directory/refserver). Built into
# bin/test/, never by `make build`, and part of no release (the goreleaser
# configs list their mains explicitly).
directory-ref:
	go build -o bin/test/shed-directory-ref ./internal/directory/refserver/cmd/shed-directory-ref

# Desktop app targets live in desktop/Makefile; `make desktop-<target>` forwards
# (e.g. `make desktop-build`, `make desktop-test`). `desktop-` is a reserved
# target prefix. The root build/test/check targets stay Go-only.
desktop-%:
	$(MAKE) -C desktop $*

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
# PYTEST_ARGS passes extra arguments to pytest, e.g. a subset of modules:
#   make test-integration-dev-fc PYTEST_ARGS="test_smoke.py test_mounts.py"
PYTEST_ARGS ?=

test-integration:
	@command -v uv >/dev/null 2>&1 || { \
	  echo "uv is required for integration tests."; \
	  echo "Install: brew install uv  (or https://docs.astral.sh/uv/getting-started/installation/)"; \
	  exit 1; \
	}
	cd tests/integration && uv sync && uv run pytest -v $(PYTEST_ARGS)

# Hermetic broker wire harness (the THIRD pytest suite — never merged with
# tests/integration or desktop/tools/shedtest). It runs the desktop app's
# embedded credential broker in a test process (crates/shed-broker-harness,
# test-only, in no release) and asserts its wire-visible output under a defined
# canonicalization against goldens recorded from the last agreeing Go↔Rust run,
# then runs the language-neutral golden-fixture runners in Rust. Needs Rust
# (cargo on PATH) + uv. See tests/broker-wire/README.md.
test-broker-wire:
	@command -v uv >/dev/null 2>&1 || { \
	  echo "uv is required for the broker wire harness."; \
	  echo "Install: brew install uv  (or https://docs.astral.sh/uv/getting-started/installation/)"; \
	  exit 1; \
	}
	cd tests/broker-wire && uv sync && uv run pytest -v
	# The golden-fixture runners all live in shed-broker: its tests/golden.rs
	# (effective_policy, gate_namespaces) and the in-crate runners
	# (load_discovered_servers, ssh_payload_shapes, the config/aws/docker/egress
	# vectors).
	cd crates && PATH="$$HOME/.cargo/bin:$$PATH" cargo test -p shed-broker golden

# The machine-transport differential (plan 012 AC2). The FOURTH pytest suite,
# and — like the other three — never merged with them. SSH has no argv API, so a
# remote command is one string the far side re-parses; `shed-core`/the Tauri app
# compose it in Rust and shed-mobile composes it in Dart, and two implementations
# of one wire contract drift silently. This suite owns the shared contract
# (tests/machine-transport/scenarios.json + goldens/) and runs the LIVE leg:
# every wire line through a throwaway sshd on 127.0.0.1, asserting the remote
# process received exactly the intended argv. It also covers the forwarded-hub
# family, which asserts DELIVERED FRAMES rather than tunnel liveness (a
# liveness-only check passes with a completely dead feed — that was a real bug).
#
# The other two legs live where their implementations do: Rust in
# `cargo test -p shed-core --test machine_transport_contract`, Dart in
# shed-mobile. Needs uv and an sshd (skips cleanly without one).
# See tests/machine-transport/README.md.
# Server-free unit tests of the integration suite's own helpers and baked-image
# test bodies (tests/integration/test_smoke_units.py, test_fixture_units.py).
# Runs in CI (integration-units).
test-integration-units:
	@command -v uv >/dev/null 2>&1 || { \
	  echo "uv is required for the integration unit tests."; \
	  echo "Install: brew install uv  (or https://docs.astral.sh/uv/getting-started/installation/)"; \
	  exit 1; \
	}
	cd tests/integration && uv sync && uv run pytest test_smoke_units.py test_fixture_units.py -v

# The integration suite's one self-managed module: a server whose users come
# from an account service (auth.directory), end to end, on this host
# (tests/integration/test_directory.py). It builds bin/ and the directory
# contract's reference server, and each case starts its own reference server
# and private shed-server from bin/ on loopback ports of its own (the remote
# backend's test drivers: no root, no cloud VM), then stops them and removes
# their files. It never touches an installed or dev server: every server
# variable of the rest of the suite is set empty. Linux only (the local link
# runs a shed-agent on this host). Not in CI. About five minutes, most of it
# two cases waiting out a two-minute trust time. Narrow it with
# PYTEST_ARGS, e.g.
#   make test-integration-directory PYTEST_ARGS="-k rates"
test-integration-directory: check-directory-itest-host build directory-ref
	cd tests/integration && uv sync && \
	  PATH="$(CURDIR)/bin:$$PATH" \
	  SHED_DIRECTORY_ITEST=1 \
	  SHED_BIN_DIR="$(CURDIR)/bin" \
	  SHED_VZ_SERVER= \
	  SHED_VZ_DEV_SERVER= \
	  SHED_VZ_LOG_PATH= \
	  SHED_VZ_DEV_LOG_PATH= \
	  SHED_FC_HOST= \
	  SHED_FC_SERVER= \
	  SHED_FC_DEV_SERVER= \
	  SHED_FC_LOG_PATH= \
	  SHED_FC_DEV_LOG_PATH= \
	  SHED_REMOTE_SERVER= \
	  SHED_ITEST_SOURCE= \
	  uv run pytest -v test_directory.py $(PYTEST_ARGS)

check-directory-itest-host:
	@if [ "$$(uname -s)" != "Linux" ]; then \
	  echo "test-integration-directory runs on Linux only: its server uses the remote backend's local link,"; \
	  echo "which runs a shed-agent on this host (this host is $$(uname -s))."; \
	  exit 1; \
	fi
	@command -v uv >/dev/null 2>&1 || { \
	  echo "uv is required for integration tests."; \
	  echo "Install: brew install uv  (or https://docs.astral.sh/uv/getting-started/installation/)"; \
	  exit 1; \
	}

test-machine-transport:
	@command -v uv >/dev/null 2>&1 || { \
	  echo "uv is required for the machine-transport harness."; \
	  echo "Install: brew install uv  (or https://docs.astral.sh/uv/getting-started/installation/)"; \
	  exit 1; \
	}
	cd tests/machine-transport && uv sync && uv run pytest -v

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
# Setup (one-time per developer), after the first `make dev-server-up`:
#   shed server add localhost --ssh-port 12222 --name $(SHED_VZ_DEV_SERVER)
#   (first contact is over SSH and enrols an mtls client certificate; see
#   CLAUDE.md or tests/integration/README.md). The dev server keeps its
#   identity in its own state_dir (~/.shed/dev/state), not the brew server's
#   ~/.shed; dev-server-up says when a start made a new one and how to re-add
#   the entry.
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

# OPTIONAL, per-machine: relocate the dev server's blob-heavy state (images,
# instances, snapshots, uppers) off the boot volume. Unset by default, which
# is the committed config's `~/Library/Application Support/shed-dev/vz` — so
# every other developer's behavior is unchanged and nothing about this is
# machine-specific in the repo.
#
# Set it in your shell profile when this machine's boot volume can't host a
# multi-GB dev image store, e.g.:
#   export SHED_DEV_STATE_DIR=/Volumes/miniext/shed-dev/vz
#
# socket_dir is deliberately NOT relocated (unix socket path length limits) —
# see scripts/render-dev-config.sh — and neither is the dev server's identity
# directory (state_dir: ~/.shed/dev/state in $(DEV_CONFIG): host key, TLS
# certificate, client CA, upgrade marker, pins). The volume must be mounted;
# dev-server-up refuses to start otherwise rather than silently building a
# second, empty image store on the mount point.
SHED_DEV_STATE_DIR ?=

# OPTIONAL: start a dev server with the integration suite's itest user list
# (auth.users: itest-admin, itest-alice, itest-bob, and the config's own
# auth.ssh key sources moved under the admin user `dev`), so the users cases
# (tests/integration/test_users.py) can run against it. Empty (the default):
# no user list, one implicit user. `itest`: dev-server-up, -up-fc and
# -up-remote (and their restarts) render the config with
# `scripts/render-dev-config.sh --users $(SHED_ITEST_USERS_DIR)`, and
# test-integration-dev-fc and -remote tell the suite so
# (SHED_ITEST_USERS=<backend>). The suite's [vz] users cases restart the Mac
# dev server with the list themselves (and restore the committed base), so
# test-integration-dev needs neither. The keys are generated once, outside
# the repo (`make dev-itest-users`). Needs uv.
SHED_DEV_USERS ?=
SHED_ITEST_USERS_DIR ?= $(HOME)/.shed/dev/itest-users

# These reach the recipes through the environment and are read there as
# quoted shell variables ("$$SHED_ITEST_USERS_DIR"), never pasted into recipe
# text, so a directory with a space, a quote or a `$` in it is one argument
# and is never run as code.
export SHED_DEV_STATE_DIR SHED_DEV_USERS SHED_ITEST_USERS_DIR

# The config renderer the dev targets run, and the shed-server binary the Mac
# and remote dev targets launch. Variables only so the fixture unit tests
# (tests/integration/test_fixture_units.py) can put stubs in their place.
DEV_RENDER_SCRIPT ?= ./scripts/render-dev-config.sh
DEV_SERVER_BIN ?= bin/shed-server

# The rendered config, written whenever ANY override above is active. With no
# overrides the targets run the committed $(DEV_CONFIG) directly — byte
# identical to having no generation step at all.
DEV_GENERATED_CONFIG := $(HOME)/.shed/dev/server.generated.yaml
DEV_RENDER := $(if $(or $(SHED_DEV_STATE_DIR),$(filter itest,$(SHED_DEV_USERS))),yes,)

# Every dev target that renders a config checks what it is about to start
# with. A failed render must never fall through to a launch: shed-server
# given `--config ""` (or an empty file) loads its normal config search
# paths, so a dev target could start a server on the developer's own config
# and state. This leaves the shell variable RUN_CONFIG set and exits the
# shell unless it names a non-empty file. Each config snippet below removes
# its output before rendering, so a renderer that fails, or "succeeds"
# without writing, cannot leave an earlier file in place to pass this check.
define dev-check-run-config
if [ -z "$$RUN_CONFIG" ] || [ ! -f "$$RUN_CONFIG" ] || [ ! -s "$$RUN_CONFIG" ]; then \
  echo "ERROR: no usable dev server config ('$$RUN_CONFIG' is unset, missing or empty); not starting shed-server."; \
  exit 1; \
fi
endef

# The Mac dev server's config: the committed $(DEV_CONFIG) with no override,
# else $(DEV_GENERATED_CONFIG) rendered from it with --state-dir and/or
# --users. Sets RUN_CONFIG; exits the shell on any failure.
define dev-mac-config
if [ -n "$(DEV_RENDER)" ]; then \
  RUN_CONFIG="$(DEV_GENERATED_CONFIG)"; \
  set --; \
  if [ -n "$$SHED_DEV_STATE_DIR" ]; then set -- "$$@" --state-dir "$$SHED_DEV_STATE_DIR"; fi; \
  if [ "$$SHED_DEV_USERS" = itest ]; then set -- "$$@" --users "$$SHED_ITEST_USERS_DIR"; fi; \
  rm -f "$$RUN_CONFIG"; \
  if ! $(DEV_RENDER_SCRIPT) "$(DEV_CONFIG)" "$$RUN_CONFIG" "$$@"; then \
    rm -f "$$RUN_CONFIG"; \
    echo "ERROR: rendering the dev server config failed (above); not starting shed-server."; \
    exit 1; \
  fi; \
else \
  RUN_CONFIG="$(DEV_CONFIG)"; \
fi; \
$(dev-check-run-config)
endef

# Refuse to start when SHED_DEV_STATE_DIR points somewhere unwritable — an
# external volume that isn't mounted, typically. Without this the server
# happily creates an empty store on the bare mount point, then reports "image
# not found" for every create, which reads like a product bug rather than
# "plug the drive in". No-op when the variable is unset.
define check-dev-state-dir
@if [ -n "$(SHED_DEV_STATE_DIR)" ]; then \
  parent="$$(dirname "$(SHED_DEV_STATE_DIR)")"; \
  while [ ! -d "$$parent" ] && [ "$$parent" != "/" ]; do parent="$$(dirname "$$parent")"; done; \
  case "$(SHED_DEV_STATE_DIR)" in \
    /Volumes/*) vol="/$$(echo "$(SHED_DEV_STATE_DIR)" | cut -d/ -f2-3)"; \
      if ! mount | grep -q " on $$vol "; then \
        echo "ERROR: SHED_DEV_STATE_DIR=$(SHED_DEV_STATE_DIR) but $$vol is not mounted."; \
        echo "       Mount the volume, or unset SHED_DEV_STATE_DIR to use the"; \
        echo "       default under ~/Library/Application Support/shed-dev/vz."; \
        exit 1; \
      fi ;; \
  esac; \
  if ! mkdir -p "$(SHED_DEV_STATE_DIR)" 2>/dev/null || [ ! -w "$(SHED_DEV_STATE_DIR)" ]; then \
    echo "ERROR: SHED_DEV_STATE_DIR=$(SHED_DEV_STATE_DIR) is not writable."; \
    exit 1; \
  fi; \
fi
endef

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

# SHED_DEV_AUTH_MODE was removed: both parallel dev servers run their
# committed configs, which are mtls, and shed-server no longer loads a token
# config at all. A value left in the environment fails here with one line,
# before anything is stopped or started, rather than being silently ignored.
check-no-dev-auth-mode:
	@if [ -n "$(SHED_DEV_AUTH_MODE)" ]; then \
	  echo "ERROR: SHED_DEV_AUTH_MODE was removed; the dev servers run mtls only. Unset it (got '$(SHED_DEV_AUTH_MODE)')."; \
	  exit 1; \
	fi

# SHED_DEV_USERS takes '' or `itest`; anything else stops here, before
# anything is stopped or started.
check-dev-users:
	@case "$$SHED_DEV_USERS" in \
	  ""|itest) ;; \
	  *) echo "ERROR: SHED_DEV_USERS is '$$SHED_DEV_USERS'; want empty (no user list) or itest."; exit 1 ;; \
	esac

# Generate the itest users' keys (idempotent; an existing key is never
# replaced, since a running server may have been started with it).
dev-itest-users:
	@command -v uv >/dev/null 2>&1 || { echo "uv is required (https://docs.astral.sh/uv/)."; exit 1; }
	cd tests/integration && uv run --quiet python -m fixtures.identity --dir "$$SHED_ITEST_USERS_DIR" keys

# The Mac dev server's settings guard: the removed auth knob, SHED_DEV_USERS
# and SHED_DEV_STATE_DIR. A prerequisite of dev-server-up and of
# dev-server-restart, so a bad value fails before anything is stopped or
# started. (dev-server-restart runs its stop and start from its recipe,
# which make starts only once this has succeeded, whatever -j is.)
check-dev-settings: check-no-dev-auth-mode check-dev-users
	$(call check-dev-state-dir)

# Render (when an override is active) and check the Mac dev server's config.
# A prerequisite of dev-server-restart, so a render that fails stops the
# restart before the running server is stopped. dev-server-up renders again
# in its launch shell and checks again there.
check-dev-config: check-dev-settings
	@$(dev-mac-config); echo "Dev server config: $$RUN_CONFIG"

dev-server-up: check-dev-settings build
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
	@# Select the config: with NO overrides (the default) the committed
	@# $(DEV_CONFIG) runs untouched — no generation step at all. An override
	@# (SHED_DEV_STATE_DIR, SHED_DEV_USERS=itest) renders a throwaway variant
	@# first; the committed base is never edited. A failed render, or a config
	@# that is missing or empty, exits here before the launch (dev-mac-config).
	@# Combined into the SAME shell invocation as the nohup launch below (Make
	@# starts a fresh subshell per recipe line; RUN_CONFIG must survive into
	@# the launch line).
	@#
	@# Inline env so we don't pollute the caller's launchctl domain
	@# (and so the brew server's launchctl env is left alone). Note
	@# the env var name: shed-server reads SHED_BUILD_TOOLS_REF; the
	@# Makefile-level variable that holds its value is named
	@# RELEASE_BUILD_TOOLS_REF (because it points at the *release*
	@# tooling image).
	@$(dev-mac-config); \
	SHED_BUILD_TOOLS_REF="$(RELEASE_BUILD_TOOLS_REF)" \
	  nohup $(DEV_SERVER_BIN) serve --config "$$RUN_CONFIG" \
	    > "$(DEV_LOG_PATH)" 2>&1 & \
	  echo $$! > "$(DEV_PID_PATH)"
	@# Readiness probe: scripts/dev-server-probe.sh. Not a CLI probe, which
	@# needs an enrolled client entry that under auth.mode: mtls does not
	@# exist until `shed server add` has run. Not the readiness log line
	@# alone either: serve.go logs it before it reads its listeners' errors,
	@# so a server that cannot bind its port logs "ready" and then exits. The
	@# script also wants the process alive, every logged listener accepting a
	@# TCP connection, and no "Server error:" a second later. A server that
	@# died fails this target rather than being reported up. The polling is
	@# scripts/dev-server-wait.sh, shared by all three dev-server-up targets:
	@# 0 ready, 1 still not ready after the tries, anything else failed (a
	@# probe that refuses its arguments is a failure, never "not yet").
	@PID=$$(cat "$(DEV_PID_PATH)"); \
	STATE=$$(bash ./scripts/dev-server-wait.sh 15 -- bash ./scripts/dev-server-probe.sh "$(DEV_LOG_PATH)" "$$PID"); RC=$$?; \
	case "$$RC" in \
	  0) echo "Dev VZ shed-server ($(SHED_VZ_DEV_SERVER)) ready: $$STATE." ;; \
	  1) echo "WARNING: Dev VZ shed-server ($(SHED_VZ_DEV_SERVER)) not ready after 15 s ($$STATE)."; \
	     echo "  - Tail the log:  make dev-server-logs   (or 'tail $(DEV_LOG_PATH)')"; \
	     echo "  - Brew server:   should be unaffected; 'shed -s $(SHED_VZ_SERVER) list' still works" ;; \
	  *) echo "ERROR: Dev VZ shed-server ($(SHED_VZ_DEV_SERVER)) did not start: $$STATE"; \
	     echo "  Last lines of $(DEV_LOG_PATH):"; \
	     tail -n 20 "$(DEV_LOG_PATH)"; \
	     exit 1 ;; \
	esac
	@echo ""
	@echo "Dev shed-server up: pid $$(cat $(DEV_PID_PATH))"
	@if [ -n "$(DEV_RENDER)" ]; then \
	  echo "  Config:  $(DEV_GENERATED_CONFIG) (generated from $(DEV_CONFIG))"; \
	else \
	  echo "  Config:  $(DEV_CONFIG)"; \
	fi
	@if [ -n "$$SHED_DEV_STATE_DIR" ]; then \
	  echo "  State:   $$SHED_DEV_STATE_DIR (SHED_DEV_STATE_DIR)"; \
	fi
	@if [ "$$SHED_DEV_USERS" = itest ]; then \
	  echo "  Users:   the itest user list (keys in $$SHED_ITEST_USERS_DIR); you are user dev"; \
	fi
	@echo "  Log:     $(DEV_LOG_PATH)"
	@echo "  CLI:     shed -s $(SHED_VZ_DEV_SERVER) list"
	@# Its identity directory (the config's own state_dir, never the brew
	@# server's ~/.shed), as the server logged it. A start that made a new SSH
	@# host key there (its first start in that directory) also made a new TLS
	@# certificate and client CA, so a CLI entry added before no longer
	@# matches: say how to re-add that one entry. Nothing here edits
	@# ~/.shed/config.yaml or ~/.shed/known_hosts.
	@STATE=$$(sed -n 's/.*State dir: //p' "$(DEV_LOG_PATH)" 2>/dev/null | head -n 1); \
	if [ -n "$$STATE" ]; then echo "  Identity: $$STATE (host key, TLS certificate, client CA, pins)"; fi; \
	if grep -qF 'Generated new host key:' "$(DEV_LOG_PATH)" 2>/dev/null; then \
	  echo ""; \
	  echo "NOTE: this start made a new SSH host key in $${STATE:-the dev server state directory}"; \
	  echo "  (on a first start there, a new TLS certificate and client CA too). A"; \
	  echo "  $(SHED_VZ_DEV_SERVER) entry added before this no longer matches it. Remove only that"; \
	  echo "  entry and its host key line, then add it again:"; \
	  echo "    shed server remove $(SHED_VZ_DEV_SERVER)"; \
	  echo "    ssh-keygen -R '[localhost]:12222' -f ~/.shed/known_hosts"; \
	  echo "    shed server add localhost --ssh-port 12222 --name $(SHED_VZ_DEV_SERVER)"; \
	  echo "  Leave every other entry, and default_server, alone. With no entry yet, only the add."; \
	  echo ""; \
	fi
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
	  STATE=$$(sed -n 's/.*State dir: //p' "$(DEV_LOG_PATH)" 2>/dev/null | head -n 1); \
	  if [ -n "$$STATE" ]; then echo "Identity:   $$STATE"; fi; \
	  if [ -n "$(SHED_DEV_STATE_DIR)" ]; then \
	    echo "State dir:  $(SHED_DEV_STATE_DIR) (SHED_DEV_STATE_DIR now in effect)"; \
	  fi; \
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

# Stop, then start, from the recipe rather than as sibling prerequisites:
# make runs this recipe only after check-dev-settings and check-dev-config
# have succeeded, so a rejected setting or a config that cannot be rendered
# never stops the running server, and the stop finishes before the start
# begins, whatever -j is.
dev-server-restart: check-dev-config
	@$(MAKE) --no-print-directory dev-server-down
	@$(MAKE) --no-print-directory dev-server-up

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
test-integration-dev: check-no-dev-auth-mode build
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
	@#
	@# PATH puts THIS tree's freshly built bin/ first, so the suite drives the
	@# branch's `shed` rather than the brew-installed release. Without it the
	@# fixtures resolve `shed` via shutil.which() to /opt/homebrew/bin/shed and
	@# silently test the OLD client against the dev server — wrong for any
	@# client-side change, and fatal under auth.mode: mtls, where a pre-mtls
	@# client cannot complete the TLS handshake at all and every VZ test skips
	@# with a misleading "dev server is not reachable".
	PATH="$(CURDIR)/bin:$$PATH" \
	  SHED_VZ_SERVER=$(SHED_VZ_DEV_SERVER) \
	  SHED_VZ_LOG_PATH=$(DEV_LOG_PATH) \
	  SHED_VZ_DEV_SERVER=$(SHED_VZ_DEV_SERVER) \
	  SHED_VZ_DEV_LOG_PATH=$(DEV_LOG_PATH) \
	  SHED_IMAGE_HAS_ROOST=vz \
	  SHED_IMAGE_HAS_CRAZE=vz \
	  SHED_IMAGE_HAS_HOME_DIRS=vz \
	  SHED_ITEST_USERS_DIR="$$SHED_ITEST_USERS_DIR" \
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
# Setup (one-time per dev workstation), after the first
# `make dev-server-up-fc`:
#   shed server add $(FC_REMOTE_HOST) --ssh-port 12222 --name $(SHED_FC_DEV_SERVER)
#   (first contact is over SSH and enrols an mtls client certificate. An
#   entry left from when this server ran open is plain HTTP and does not
#   heal itself: `shed server remove $(SHED_FC_DEV_SERVER)` first.)
#
# Assumed infrastructure (same as today's deb install):
#   - Passwordless SSH from this host to $(FC_REMOTE_HOST).
#   - sudo NOPASSWD on the remote for the SSH user. The recipes drive
#     systemctl, install, cp, rm, nohup, tail, cat, stat, kill, ps.
#   - The remote has FC + KVM available (which the deb shed-server
#     already requires).
#
# FC isolation notes:
#   - Different ports (mtls HTTPS 18443 + SSH 12222 vs deb's 8080/2222).
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

# The committed config the FC dev targets upload. It is mtls. It is uploaded
# untouched unless SHED_DEV_USERS=itest, which renders it with the itest
# user list first (FC_DEV_USERS_CONFIG).
FC_DEV_LOCAL_CONFIG := configs/server.dev-parallel.linux-fc.yaml
FC_DEV_USERS_CONFIG := $(HOME)/.shed/dev/server.dev-fc.users.yaml

# The local file the FC dev targets upload: the committed config, or, with
# SHED_DEV_USERS=itest, $(FC_DEV_USERS_CONFIG) rendered from it. Sets
# RUN_CONFIG; exits the shell when the render fails or the file is missing
# or empty (dev-check-run-config).
define dev-fc-config
if [ "$$SHED_DEV_USERS" = itest ]; then \
  RUN_CONFIG="$(FC_DEV_USERS_CONFIG)"; \
  rm -f "$$RUN_CONFIG"; \
  if ! $(DEV_RENDER_SCRIPT) "$(FC_DEV_LOCAL_CONFIG)" "$$RUN_CONFIG" --users "$$SHED_ITEST_USERS_DIR"; then \
    rm -f "$$RUN_CONFIG"; \
    echo "ERROR: rendering the FC dev server config failed (above); not starting shed-server."; \
    exit 1; \
  fi; \
else \
  RUN_CONFIG="$(FC_DEV_LOCAL_CONFIG)"; \
fi; \
$(dev-check-run-config)
endef

# Render and check the FC dev config. A prerequisite of dev-server-restart-fc,
# so a render that fails stops the restart before the running server is
# stopped; dev-server-up-fc renders and checks again before its upload.
check-dev-config-fc: check-no-dev-auth-mode check-dev-users
	@$(dev-fc-config); echo "FC dev server config: $$RUN_CONFIG"

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
# check, with comm verification). Polls scripts/dev-server-probe.sh on
# the remote for readiness (15 s); fails if the server dies starting up.
#
# WARNING: this starts a SECOND shed-server on the shared FC host.
# Any other developer using $(FC_REMOTE_HOST) won't see the deb
# server change, but if they're also running their own dev server
# on the same ports, this will conflict. Coordinate.
dev-server-up-fc: check-no-dev-auth-mode check-dev-users build-fc-remote-server
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
	@# Upload the committed (mtls) config: untouched, or with the itest user
	@# list when SHED_DEV_USERS=itest. A failed render, or a config that is
	@# missing or empty, exits here before anything is uploaded or started.
	@$(dev-fc-config); \
	echo "Uploading $$RUN_CONFIG..."; \
	MOUNT_SRCS=$$(grep -E '^[[:space:]]+source:[[:space:]]+/var/lib/shed-dev/mounts/' "$$RUN_CONFIG" | awk '{print $$2}' | tr '\n' ' '); \
	UPLOAD_BIN=/tmp/shed-server-dev.upload.$$$$; \
	 UPLOAD_CFG=/tmp/shed-server-dev.upload.$$$$.yaml; \
	 UPLOAD_PROXY=/tmp/shed-egress-proxy-dev.upload.$$$$; \
	 scp bin/shed-server-fc-remote $(FC_REMOTE_HOST):$$UPLOAD_BIN && \
	 scp bin/shed-egress-proxy-fc-remote $(FC_REMOTE_HOST):$$UPLOAD_PROXY && \
	 scp "$$RUN_CONFIG" $(FC_REMOTE_HOST):$$UPLOAD_CFG && \
	 ssh -o BatchMode=yes $(FC_REMOTE_HOST) "set -e; \
	   sudo install -m 755 $$UPLOAD_BIN $(FC_DEV_BIN_PATH); \
	   sudo install -m 755 $$UPLOAD_PROXY $(FC_DEV_PROXY_PATH); \
	   sudo install -m 644 $$UPLOAD_CFG $(FC_DEV_CONFIG); \
	   rm -f $$UPLOAD_BIN $$UPLOAD_PROXY $$UPLOAD_CFG; \
	   if [ -n \"$$MOUNT_SRCS\" ]; then sudo install -d -m 0755 $$MOUNT_SRCS; fi; \
	   sudo install -m 644 /dev/null $(FC_DEV_LOG_PATH); \
	   sudo bash -c 'SHED_BUILD_TOOLS_REF=\"$(RELEASE_BUILD_TOOLS_REF)\" \
	     nohup $(FC_DEV_BIN_PATH) serve --config $(FC_DEV_CONFIG) \
	     >> $(FC_DEV_LOG_PATH) 2>&1 < /dev/null & \
	     echo \$$! > $(FC_DEV_PID_PATH)'"
	@# Readiness probe — the Mac dev-server-up's scripts/dev-server-probe.sh,
	@# piped over ssh and run on the remote against its log (emptied just
	@# before the launch above, so nothing in it is from an earlier run) and
	@# the launched pid. Not a CLI probe, which needs an enrolled mtls entry
	@# that does not exist until `shed server add` has run; not the readiness
	@# line alone, which serve.go logs before it reads its listeners' errors
	@# (a server that cannot bind logs "ready", then exits). A server that
	@# died fails this target. Polled by scripts/dev-server-wait.sh, as the
	@# Mac target is: an ssh failure (exit 255) is "not yet", the probe's own
	@# 1 is "not yet", and any other status (the probe refusing its arguments,
	@# say) is a failure.
	@STATE=$$(bash ./scripts/dev-server-wait.sh --stdin scripts/dev-server-probe.sh --retry 255 15 -- \
	    ssh -o BatchMode=yes -o ConnectTimeout=5 $(FC_REMOTE_HOST) \
	    "bash -s -- $(FC_DEV_LOG_PATH) \"\$$(sudo -n cat $(FC_DEV_PID_PATH) 2>/dev/null || cat $(FC_DEV_PID_PATH) 2>/dev/null)\""); RC=$$?; \
	case "$$RC" in \
	  0) echo "Remote FC dev shed-server ($(SHED_FC_DEV_SERVER) on $(FC_REMOTE_HOST)) ready: $$STATE." ;; \
	  1) echo "WARNING: FC dev shed-server ($(SHED_FC_DEV_SERVER) on $(FC_REMOTE_HOST)) not ready after 15 s ($$STATE)."; \
	     echo "  - Tail the remote log:  make dev-server-logs-fc"; \
	     echo "  - Deb server:           should be unaffected; 'shed -s $(SHED_FC_SERVER) list' still works" ;; \
	  *) echo "ERROR: FC dev shed-server ($(SHED_FC_DEV_SERVER) on $(FC_REMOTE_HOST)) did not start: $$STATE"; \
	     echo "  - Read the remote log:  ssh $(FC_REMOTE_HOST) sudo -n tail -n 40 $(FC_DEV_LOG_PATH)"; \
	     exit 1 ;; \
	esac
	@echo ""
	@echo "Dev FC shed-server up on $(FC_REMOTE_HOST):"
	@echo "  Auth mode: mtls"
	@echo "  Binary:  $(FC_DEV_BIN_PATH)"
	@echo "  Config:  $(FC_DEV_CONFIG) (remote path; uploaded from $(FC_DEV_LOCAL_CONFIG))"
	@echo "  Log:     $(FC_DEV_LOG_PATH)"
	@echo "  PID:     $(FC_DEV_PID_PATH)"
	@echo "  CLI:     shed -s $(SHED_FC_DEV_SERVER) list"
	@echo "           (register once: shed server add $(FC_REMOTE_HOST) --ssh-port 12222 --name $(SHED_FC_DEV_SERVER))"
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
	    echo '  Its nftables tables (if any) are left in place: they still guard any isolated dev shed whose VM is running.'; \
	    echo '  Once no dev shed is running, delete them with:'; \
	    echo '    ssh $(FC_REMOTE_HOST) sudo $(FC_DEV_BIN_PATH) network teardown --config $(FC_DEV_CONFIG)'; \
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
	    if sudo -n $(FC_DEV_BIN_PATH) network teardown --config $(FC_DEV_CONFIG); then \
	      sudo rm -f $(FC_DEV_PID_PATH) $(FC_DEV_BIN_PATH) $(FC_DEV_PROXY_PATH) $(FC_DEV_CONFIG) $(FC_DEV_LOG_PATH); \
	    else \
	      echo 'ERROR: network teardown failed; the dev tables may still be installed.'; \
	      echo '  Kept $(FC_DEV_BIN_PATH) and $(FC_DEV_CONFIG) for a retry:'; \
	      echo '    ssh $(FC_REMOTE_HOST) sudo $(FC_DEV_BIN_PATH) network teardown --config $(FC_DEV_CONFIG)'; \
	      sudo rm -f $(FC_DEV_PID_PATH); \
	      exit 1; \
	    fi; \
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
	@echo "Auth mode:     mtls (the committed config; the FC dev server has no other mode)"

dev-server-logs-fc:
	@ssh -t $(FC_REMOTE_HOST) "sudo -n tail -F $(FC_DEV_LOG_PATH)"

# Stop, then start, from the recipe rather than as sibling prerequisites:
# make runs this recipe only after the settings guards and check-dev-config-fc
# have succeeded, so a rejected setting or a config that cannot be rendered
# never stops the running server, and the stop finishes before the start
# begins, whatever -j is.
dev-server-restart-fc: check-no-dev-auth-mode check-dev-users check-dev-config-fc
	@$(MAKE) --no-print-directory dev-server-down-fc
	@$(MAKE) --no-print-directory dev-server-up-fc

# Parallel dev shed-server lifecycle (remote backend).
#
# Runs the dev build with `default_backend: remote` on THIS machine, beside
# any installed shed-server (which keeps 8080/2222 and is never touched).
# Each shed is a cloud VM. Same intentionally-ephemeral shape as the other
# dev servers: nohup + PID file, no unit, no auto-restart.
#
#   make dev-server-up-remote        # render the config, build, start
#   shed server add localhost --port 18090 --ssh-port 12290 --name remote-dev   # once
#   shed -s remote-dev create t1
#   make build && make dev-server-restart-remote   # pick up a rebuild
#   make dev-server-down-remote      # sheds (cloud VMs) are NOT deleted
#
# Cloud settings are private and never in this repo. The targets source them
# from $(SHED_REMOTE_ENV_FILE) (VAR=value lines): SHED_REMOTE_ENV (the
# environment name), SHED_GCE_PROJECT, SHED_GCE_ZONE, SHED_GCE_NETWORK,
# SHED_GCE_SUBNET, SHED_GCE_CREDENTIALS_FILE, SHED_REMOTE_SSH_KEY_FILE and
# SHED_REMOTE_SOURCE_RANGES (the firewall rule's source ranges). Two more
# choose the firewall mode: SHED_REMOTE_FIREWALL_MODE (static, the default,
# or auto) and, for auto, SHED_REMOTE_ECHO_URLS (two or more https echo
# services on different hosts, comma-separated).
# The config is rendered from configs/server.dev-remote.yaml.tmpl on every
# start; edit the template, not the rendered file.
SHED_REMOTE_ENV_FILE    ?= $(HOME)/.config/shed/remote-dev/env
SHED_REMOTE_DEV_SERVER  ?= remote-dev
# The GitHub account whose SSH keys may enrol a CLI against the dev server.
# No account is named here: the default is the owner of this checkout's
# origin remote. Set it in the environment, or in the env file, to override.
SHED_REMOTE_GITHUB_USER ?= $(shell git remote get-url origin 2>/dev/null | sed -nE 's|^.*github\.com[:/]([^/]+)/[^/]+$$|\1|p')
REMOTE_DEV_DIR          := $(HOME)/.shed/dev-remote
REMOTE_DEV_TEMPLATE     := $(CURDIR)/configs/server.dev-remote.yaml.tmpl
REMOTE_DEV_CONFIG       := $(REMOTE_DEV_DIR)/server.yaml
REMOTE_DEV_LOG_PATH     := $(REMOTE_DEV_DIR)/server.log
REMOTE_DEV_PID_PATH     := $(REMOTE_DEV_DIR)/server.pid
REMOTE_DEV_AGENT        := $(REMOTE_DEV_DIR)/agent/shed-agent-linux-amd64

# Source the env file and validate the settings the config is rendered from,
# leaving GITHUB_USER, FW_MODE and FW_SETTING set for the caller's shell. One
# shell snippet, used by check-remote-dev-settings (so a restart refuses bad
# settings before it stops anything) and by dev-server-up-remote (which renders
# from the same values).
define remote-dev-settings
if [ ! -f "$(SHED_REMOTE_ENV_FILE)" ]; then \
  echo "ERROR: no env file at $(SHED_REMOTE_ENV_FILE)."; \
  echo "       It holds the cloud settings (see the comment above dev-server-up-remote);"; \
  echo "       point SHED_REMOTE_ENV_FILE at yours."; \
  exit 1; \
fi; \
set -a; . "$(SHED_REMOTE_ENV_FILE)"; set +a; \
if [ -z "$$SHED_REMOTE_ENV" ]; then \
  echo "ERROR: $(SHED_REMOTE_ENV_FILE) does not set SHED_REMOTE_ENV (the environment name)."; \
  exit 1; \
fi; \
GITHUB_USER="$${SHED_REMOTE_GITHUB_USER:-$(SHED_REMOTE_GITHUB_USER)}"; \
if [ -z "$$GITHUB_USER" ]; then \
  echo "ERROR: set SHED_REMOTE_GITHUB_USER (the GitHub account whose SSH keys may enrol a CLI)."; \
  exit 1; \
fi; \
FW_MODE="$${SHED_REMOTE_FIREWALL_MODE:-static}"; \
case "$$FW_MODE" in \
  static) FW_SETTING="source_ranges_env: SHED_REMOTE_SOURCE_RANGES" ;; \
  auto) \
    case "$$SHED_REMOTE_ECHO_URLS" in \
      ""|*[!A-Za-z0-9:/._,-]*) \
        echo "ERROR: SHED_REMOTE_FIREWALL_MODE=auto needs SHED_REMOTE_ECHO_URLS in $(SHED_REMOTE_ENV_FILE):"; \
        echo "       two or more https echo services on different hosts, comma-separated, no spaces."; \
        exit 1 ;; \
    esac; \
    FW_SETTING="echo_urls: [$$(printf '%s' "$$SHED_REMOTE_ECHO_URLS" | sed 's/,/, /g')]" ;; \
  *) \
    echo "ERROR: SHED_REMOTE_FIREWALL_MODE is '$$FW_MODE'; want static or auto."; \
    exit 1 ;; \
esac
endef

# Render the remote dev server's config from the template, plus the itest
# user list with SHED_DEV_USERS=itest, into $(REMOTE_DEV_CONFIG). Runs after
# remote-dev-settings in the same shell (it reads what that sets). Sets
# RUN_CONFIG; exits the shell when a step fails or the file is missing or
# empty (dev-check-run-config), leaving no partial config behind.
define remote-dev-config
RUN_CONFIG="$(REMOTE_DEV_CONFIG)"; \
mkdir -p "$(REMOTE_DEV_DIR)"; \
rm -f "$$RUN_CONFIG" "$$RUN_CONFIG.users"; \
if ! sed -e "s|@DEV_REMOTE_DIR@|$(REMOTE_DEV_DIR)|g" \
    -e "s|@SHED_REMOTE_ENV@|$$SHED_REMOTE_ENV|g" \
    -e "s|@GITHUB_USER@|$$GITHUB_USER|g" \
    -e "s|@FIREWALL_MODE@|$$FW_MODE|g" \
    -e "s|@FIREWALL_SETTING@|$$FW_SETTING|g" \
    "$(REMOTE_DEV_TEMPLATE)" > "$$RUN_CONFIG"; then \
  rm -f "$$RUN_CONFIG"; \
  echo "ERROR: rendering $(REMOTE_DEV_TEMPLATE) failed; not starting shed-server."; \
  exit 1; \
fi; \
if [ "$$SHED_DEV_USERS" = itest ]; then \
  if ! $(DEV_RENDER_SCRIPT) "$$RUN_CONFIG" "$$RUN_CONFIG.users" --users "$$SHED_ITEST_USERS_DIR" \
      || ! mv "$$RUN_CONFIG.users" "$$RUN_CONFIG"; then \
    rm -f "$$RUN_CONFIG" "$$RUN_CONFIG.users"; \
    echo "ERROR: rendering the itest user list into the remote dev config failed (above); not starting shed-server."; \
    exit 1; \
  fi; \
fi; \
$(dev-check-run-config)
endef

# The remote dev server's settings guard. A prerequisite of
# dev-server-up-remote and of dev-server-restart-remote, so bad settings fail
# before anything is stopped or started.
check-remote-dev-settings: check-dev-users
	@$(remote-dev-settings)

# Render and check the remote dev config. A prerequisite of
# dev-server-restart-remote, so a render that fails stops the restart before
# the running server is stopped; dev-server-up-remote renders and checks
# again in its launch shell.
check-remote-dev-config: check-remote-dev-settings
	@$(remote-dev-settings); \
	umask 077; \
	$(remote-dev-config); \
	echo "Remote dev server config: $$RUN_CONFIG"

dev-server-up-remote: check-remote-dev-settings build-server build-cli
	@if [ -f "$(REMOTE_DEV_PID_PATH)" ] && kill -0 "$$(cat $(REMOTE_DEV_PID_PATH))" 2>/dev/null; then \
	  echo "ERROR: remote dev server already running (pid $$(cat $(REMOTE_DEV_PID_PATH)))."; \
	  echo "       Use 'make dev-server-restart-remote' to pick up a rebuild,"; \
	  echo "       or 'make dev-server-down-remote' to stop it first."; \
	  exit 1; \
	fi
	@mkdir -p "$(REMOTE_DEV_DIR)/state" "$(REMOTE_DEV_DIR)/remote" "$(REMOTE_DEV_DIR)/agent"
	@chmod 700 "$(REMOTE_DEV_DIR)"
	@# The agent build the backend offers a guest whose own is incompatible.
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build $(LDFLAGS) -o "$(REMOTE_DEV_AGENT)" ./cmd/shed-agent
	@# Render, then launch, in ONE shell: the sourced env file must reach both
	@# the render (the environment name) and the server process (everything
	@# else). The log is appended to, so a restart keeps the earlier run.
	@# START_LINE is stripped of whitespace: macOS wc pads the count with
	@# spaces, which the probe refuses (exit 64) as a malformed first-line.
	@$(remote-dev-settings); \
	umask 077; \
	$(remote-dev-config); \
	echo "=== dev-server-up-remote $$(date -u +%Y-%m-%dT%H:%M:%SZ) ===" >> "$(REMOTE_DEV_LOG_PATH)"; \
	START_LINE=$$(wc -l < "$(REMOTE_DEV_LOG_PATH)" | tr -d '[:space:]'); \
	nohup $(DEV_SERVER_BIN) serve --config "$$RUN_CONFIG" \
	  >> "$(REMOTE_DEV_LOG_PATH)" 2>&1 & \
	echo $$! > "$(REMOTE_DEV_PID_PATH)"; \
	PID=$$(cat "$(REMOTE_DEV_PID_PATH)"); \
	STATE=$$(bash ./scripts/dev-server-wait.sh 150 -- \
	  bash ./scripts/dev-server-probe.sh "$(REMOTE_DEV_LOG_PATH)" "$$PID" 1 "$$START_LINE"); RC=$$?; \
	case "$$RC" in \
	  0) echo "Remote dev shed-server ready: $$STATE." ;; \
	  1) echo "WARNING: remote dev shed-server not ready after 150 s ($$STATE)."; \
	     echo "  - Tail the log:  make dev-server-logs-remote" ;; \
	  *) echo "ERROR: remote dev shed-server did not start: $$STATE"; \
	     tail -n 20 "$(REMOTE_DEV_LOG_PATH)"; \
	     if ! kill -0 "$$PID" 2>/dev/null; then rm -f "$(REMOTE_DEV_PID_PATH)"; fi; \
	     exit 1 ;; \
	esac
	@echo ""
	@echo "Remote dev shed-server up: pid $$(cat $(REMOTE_DEV_PID_PATH))"
	@echo "  Config:  $(REMOTE_DEV_CONFIG) (rendered from $(REMOTE_DEV_TEMPLATE))"
	@echo "  Log:     $(REMOTE_DEV_LOG_PATH)"
	@echo "  Ports:   HTTPS 18490 (mtls), SSH 12290"
	@echo "  CLI:     shed -s $(SHED_REMOTE_DEV_SERVER) list"

dev-server-down-remote:
	@if [ ! -f "$(REMOTE_DEV_PID_PATH)" ]; then \
	  echo "Remote dev server not running (no PID file at $(REMOTE_DEV_PID_PATH))."; \
	  echo "  (Idempotent: this is OK.)"; \
	else \
	  PID=$$(cat "$(REMOTE_DEV_PID_PATH)"); \
	  if ! kill -0 $$PID 2>/dev/null; then \
	    echo "Remote dev server PID $$PID is stale (process already gone); cleaning up PID file."; \
	    rm -f "$(REMOTE_DEV_PID_PATH)"; \
	  elif ! ps -p $$PID -o comm= 2>/dev/null | grep -q "shed-server"; then \
	    echo "Remote dev server PID $$PID points at an unrelated process"; \
	    echo "  (current comm: $$(ps -p $$PID -o comm= 2>/dev/null))."; \
	    echo "  Refusing to send signals; cleaning up the stale PID file only."; \
	    rm -f "$(REMOTE_DEV_PID_PATH)"; \
	  else \
	    kill -TERM $$PID 2>/dev/null; \
	    for i in 1 2 3 4 5 6 7 8 9 10; do \
	      if ! kill -0 $$PID 2>/dev/null; then break; fi; \
	      sleep 1; \
	    done; \
	    if kill -0 $$PID 2>/dev/null; then \
	      echo "Remote dev server didn't stop gracefully after 10s; SIGKILL"; \
	      kill -KILL $$PID 2>/dev/null; \
	    fi; \
	    echo "Remote dev server (pid $$PID) stopped. Its sheds are cloud VMs and keep running."; \
	    rm -f "$(REMOTE_DEV_PID_PATH)"; \
	  fi; \
	fi

dev-server-status-remote:
	@if [ -f "$(REMOTE_DEV_PID_PATH)" ] && kill -0 "$$(cat $(REMOTE_DEV_PID_PATH))" 2>/dev/null; then \
	  echo "Remote dev server: running (pid $$(cat $(REMOTE_DEV_PID_PATH)))"; \
	  if bin/shed -s $(SHED_REMOTE_DEV_SERVER) list >/dev/null 2>&1; then \
	    echo "Reachable:  YES (shed -s $(SHED_REMOTE_DEV_SERVER) list succeeds)"; \
	  else \
	    echo "Reachable:  NO (shed -s $(SHED_REMOTE_DEV_SERVER) list failed)"; \
	    echo "            (check ~/.shed/config.yaml has '$(SHED_REMOTE_DEV_SERVER)')"; \
	  fi; \
	  echo "Log:        $(REMOTE_DEV_LOG_PATH)"; \
	  echo "Config:     $(REMOTE_DEV_CONFIG)"; \
	else \
	  echo "Remote dev server: NOT running"; \
	  if [ -f "$(REMOTE_DEV_PID_PATH)" ]; then \
	    echo "  (Stale PID file at $(REMOTE_DEV_PID_PATH); 'make dev-server-down-remote' will clean up.)"; \
	  fi; \
	fi

dev-server-logs-remote:
	@if [ ! -f "$(REMOTE_DEV_LOG_PATH)" ]; then \
	  echo "No log at $(REMOTE_DEV_LOG_PATH). Run 'make dev-server-up-remote' first."; \
	  exit 1; \
	fi
	@tail -F "$(REMOTE_DEV_LOG_PATH)"

# Stop, then start, from the recipe rather than as sibling prerequisites: make
# runs this recipe only after the settings guard, the config render and both
# builds have succeeded, so bad settings, a config that cannot be rendered or
# a broken build never stop the running server, and the stop finishes before
# the start begins, whatever -j is.
dev-server-restart-remote: check-remote-dev-settings check-remote-dev-config build-server build-cli
	@$(MAKE) --no-print-directory dev-server-down-remote
	@$(MAKE) --no-print-directory dev-server-up-remote

# Run the integration suite's [remote] cases against the remote dev server.
# MANUAL ONLY: every case that creates a shed creates a billed cloud VM (a full
# run is about 25 VMs, each up for a minute or two, at most
# remote.limits.max_running at once). It does not start the dev server for
# you; run `make dev-server-up-remote` first.
#
# Every vz/fc server variable is set EMPTY so no [vz] or [fc] case runs (the
# defaults can name production servers). Cases the remote backend cannot run
# (mounts, image store, snapshots, reset, egress, vz/fc timing) skip with a
# reason. Narrow a run with PYTEST_ARGS, e.g.
#   make test-integration-remote PYTEST_ARGS="test_remote_refusals.py"
test-integration-remote: build-cli
	@if [ ! -f "$(REMOTE_DEV_PID_PATH)" ] || ! kill -0 "$$(cat $(REMOTE_DEV_PID_PATH))" 2>/dev/null; then \
	  echo "ERROR: the remote dev server is not running; 'make dev-server-up-remote' first."; \
	  exit 1; \
	fi
	PATH="$(CURDIR)/bin:$$PATH" \
	  SHED_VZ_SERVER= \
	  SHED_VZ_DEV_SERVER= \
	  SHED_FC_SERVER= \
	  SHED_FC_DEV_SERVER= \
	  SHED_REMOTE_SERVER=$(SHED_REMOTE_DEV_SERVER) \
	  SHED_REMOTE_LOG_PATH=$(REMOTE_DEV_LOG_PATH) \
	  SHED_ITEST_USERS=$(if $(filter itest,$(SHED_DEV_USERS)),remote,) \
	  SHED_ITEST_USERS_DIR="$$SHED_ITEST_USERS_DIR" \
	  $(MAKE) test-integration

# Root-only proof of the isolated network profile's nftables rules, run on
# $(FC_REMOTE_HOST) entirely inside network namespaces the test creates and
# deletes (a bridge and veth pairs stand in for TAPs; nft only ever runs via
# `ip netns exec`). Cross-compiles the netpolicy test binary with the netns
# tag for the remote's arch, runs it under sudo, and deletes the copy.
NETNS_TEST_BIN := bin/netpolicy-netns.test
test-netns-remote:
	@ARCH=$$(ssh -o BatchMode=yes -o ConnectTimeout=5 $(FC_REMOTE_HOST) "uname -m" 2>/dev/null); \
	 case "$$ARCH" in \
	   x86_64)  GOARCH=amd64 ;; \
	   aarch64) GOARCH=arm64 ;; \
	   *) echo "ERROR: unsupported or unreachable remote arch on $(FC_REMOTE_HOST): '$$ARCH'"; exit 1 ;; \
	 esac; \
	 echo "Cross-compiling the netpolicy netns test for linux/$$GOARCH..."; \
	 GOOS=linux GOARCH=$$GOARCH CGO_ENABLED=0 go test -c -tags netns -o $(NETNS_TEST_BIN) ./internal/netpolicy || exit 1; \
	 RDIR=$$(ssh -o BatchMode=yes $(FC_REMOTE_HOST) "mktemp -d /tmp/netpolicy-netns.XXXXXXXX") || exit 1; \
	 case "$$RDIR" in /tmp/netpolicy-netns.*) ;; *) echo "ERROR: unexpected remote temp dir '$$RDIR'"; exit 1 ;; esac; \
	 RC=0; \
	 if scp -q $(NETNS_TEST_BIN) $(FC_REMOTE_HOST):$$RDIR/netpolicy-netns.test; then \
	   OUT=$$(ssh -o BatchMode=yes $(FC_REMOTE_HOST) "sudo -n $$RDIR/netpolicy-netns.test -test.v -test.count=1 -test.run '^TestNetns'" 2>&1); RC=$$?; \
	   printf '%s\n' "$$OUT"; \
	   if [ $$RC -eq 0 ] && printf '%s\n' "$$OUT" | grep -q -- '--- SKIP'; then \
	     echo "ERROR: a netns test skipped (root, nft and ip are required on $(FC_REMOTE_HOST)); the rules were not proven"; RC=1; \
	   elif [ $$RC -eq 0 ] && ! printf '%s\n' "$$OUT" | grep -q -- '--- PASS: TestNetns'; then \
	     echo "ERROR: no netns test ran"; RC=1; \
	   fi; \
	 else \
	   RC=1; \
	 fi; \
	 ssh -o BatchMode=yes $(FC_REMOTE_HOST) "rm -rf $$RDIR"; \
	 exit $$RC

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
test-integration-dev-fc: check-no-dev-auth-mode
	@# Auto-up if not running on the remote (`ps -p PID` check; the
	@# dev server runs as root and the SSH user's `kill -0` can't
	@# signal a root-owned process).
	@if ! ssh -o BatchMode=yes -o ConnectTimeout=5 $(FC_REMOTE_HOST) \
	     "test -f $(FC_DEV_PID_PATH) && ps -p \$$(sudo -n cat $(FC_DEV_PID_PATH) 2>/dev/null) -o comm= 2>/dev/null | grep -q shed-server" 2>/dev/null; then \
	  echo "Dev FC server not running on $(FC_REMOTE_HOST); starting via dev-server-up-fc..."; \
	  $(MAKE) dev-server-up-fc; \
	fi
	@# Same PATH reasoning as test-integration-dev: drive the branch's `shed`,
	@# not the brew-installed release.
	@# SHED_VZ_SERVER and SHED_VZ_DEV_SERVER are set EMPTY so every [vz] case
	@# skips (conftest treats an empty name as "no server"): this target is
	@# FC-only, and the VZ default (`my-server`) can name a production server
	@# (on a Linux workstation it is often mini3's deb server), which the
	@# suite would otherwise create and delete sheds on.
	PATH="$(CURDIR)/bin:$$PATH" \
	  SHED_VZ_SERVER= \
	  SHED_VZ_DEV_SERVER= \
	  SHED_FC_HOST=$(FC_REMOTE_HOST) \
	  SHED_FC_SERVER=$(SHED_FC_DEV_SERVER) \
	  SHED_FC_LOG_PATH=$(FC_DEV_LOG_PATH) \
	  SHED_FC_DEV_SERVER=$(SHED_FC_DEV_SERVER) \
	  SHED_FC_DEV_LOG_PATH=$(FC_DEV_LOG_PATH) \
	  SHED_IMAGE_HAS_ROOST=fc \
	  SHED_IMAGE_HAS_CRAZE=fc \
	  SHED_IMAGE_HAS_HOME_DIRS=fc \
	  SHED_ITEST_USERS=$(if $(filter itest,$(SHED_DEV_USERS)),fc,) \
	  SHED_ITEST_USERS_DIR="$$SHED_ITEST_USERS_DIR" \
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

# Local goreleaser snapshot of a goreleaser release component — the pre-CI
# proof that .goreleaser.<component>.yaml still builds its targets and renders
# its deb / brew formula. `make snapshot-server` (the only one).
# Mirrors what ci.yml's release-snapshot job runs.
#
# goreleaser comes from .mise.toml pinned to CI's exact version (a local
# snapshot that predicts CI has to run CI's goreleaser). Setup is
# `mise install`.
#
# `check` alone (config validation, no build) is the cheap subset:
#   mise exec -- goreleaser check -f .goreleaser.server.yaml
#
# A pattern rule, so it needs no .PHONY entry (pattern rules never match a
# real file here — there is no `snapshot-*` on disk).
snapshot-%:
	mise exec -- goreleaser release --snapshot --clean -f .goreleaser.$*.yaml

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

# check-roost-pin: the roost-session version + protocol baked into both
# rootfs Dockerfiles (ARG ROOST_SESSION_VERSION / ROOST_SESSION_PROTOCOL)
# must match the Rust source of truth: RELEASE_PIN's version and
# LATEST_KNOWN_RELEASE's protocol in crates/shed-core/src/roost/bootstrap/source.rs.
# It also holds both Dockerfiles' ROOST_SESSION_SHA256_{AMD64,ARM64} ARGs in
# lockstep with each other — it cannot verify the digests offline (that is
# what each image build's sha256sum -c does), but it stops the two files
# drifting apart, which matters most for the pair no build ever exercises:
# vz only ever builds arm64 (on the Mac) and fc only ever builds amd64 (on
# mini3), so vz's AMD64 value and fc's ARM64 value are never proven by a
# build. Modeled on check-kernel-pin above — same fail-fast-on-drift shape.
# Portable to macOS BSD sed/grep: awk/sed -E only, no grep -P (023 found that
# breaks there).
check-roost-pin: check-gce-pins
	@vz_v=$$(awk -F= '/^ARG ROOST_SESSION_VERSION=/ { print $$2; exit }' vz/Dockerfile) ; \
	 fc_v=$$(awk -F= '/^ARG ROOST_SESSION_VERSION=/ { print $$2; exit }' firecracker/Dockerfile) ; \
	 vz_p=$$(awk -F= '/^ARG ROOST_SESSION_PROTOCOL=/ { print $$2; exit }' vz/Dockerfile) ; \
	 fc_p=$$(awk -F= '/^ARG ROOST_SESSION_PROTOCOL=/ { print $$2; exit }' firecracker/Dockerfile) ; \
	 vz_amd64=$$(awk -F= '/^ARG ROOST_SESSION_SHA256_AMD64=/ { print $$2; exit }' vz/Dockerfile) ; \
	 fc_amd64=$$(awk -F= '/^ARG ROOST_SESSION_SHA256_AMD64=/ { print $$2; exit }' firecracker/Dockerfile) ; \
	 vz_arm64=$$(awk -F= '/^ARG ROOST_SESSION_SHA256_ARM64=/ { print $$2; exit }' vz/Dockerfile) ; \
	 fc_arm64=$$(awk -F= '/^ARG ROOST_SESSION_SHA256_ARM64=/ { print $$2; exit }' firecracker/Dockerfile) ; \
	 pin_v=$$(sed -n 's/^pub const RELEASE_PIN: .*RoostRelease { version: "\([0-9.][0-9.]*\)".*/\1/p' crates/shed-core/src/roost/bootstrap/source.rs | head -1) ; \
	 known_p=$$(sed -n 's/^pub const LATEST_KNOWN_RELEASE: .*= ("[0-9.]*", \([0-9][0-9]*\)).*/\1/p' crates/shed-core/src/roost/bootstrap/source.rs | head -1) ; \
	 if [ -z "$$vz_v" ] || [ -z "$$fc_v" ] || [ -z "$$vz_p" ] || [ -z "$$fc_p" ] || [ -z "$$pin_v" ] || [ -z "$$known_p" ] || \
	    [ -z "$$vz_amd64" ] || [ -z "$$fc_amd64" ] || [ -z "$$vz_arm64" ] || [ -z "$$fc_arm64" ]; then \
	   echo "ERROR: roost-session pin values missing:" ; \
	   echo "  vz/Dockerfile ARG ROOST_SESSION_VERSION:           $$vz_v" ; \
	   echo "  firecracker/Dockerfile ARG ROOST_SESSION_VERSION:  $$fc_v" ; \
	   echo "  vz/Dockerfile ARG ROOST_SESSION_PROTOCOL:          $$vz_p" ; \
	   echo "  firecracker/Dockerfile ARG ROOST_SESSION_PROTOCOL: $$fc_p" ; \
	   echo "  vz/Dockerfile ARG ROOST_SESSION_SHA256_AMD64:      $$vz_amd64" ; \
	   echo "  firecracker/Dockerfile ARG ROOST_SESSION_SHA256_AMD64: $$fc_amd64" ; \
	   echo "  vz/Dockerfile ARG ROOST_SESSION_SHA256_ARM64:      $$vz_arm64" ; \
	   echo "  firecracker/Dockerfile ARG ROOST_SESSION_SHA256_ARM64: $$fc_arm64" ; \
	   echo "  RELEASE_PIN version:                               $$pin_v" ; \
	   echo "  LATEST_KNOWN_RELEASE protocol:                     $$known_p" ; \
	   exit 1 ; \
	 fi ; \
	 if [ "$$vz_v" != "$$fc_v" ] || [ "$$vz_v" != "$$pin_v" ] || [ "$$vz_p" != "$$fc_p" ] || [ "$$vz_p" != "$$known_p" ]; then \
	   echo "ERROR: roost-session pin drifted:" ; \
	   echo "  vz/Dockerfile:          version=$$vz_v protocol=$$vz_p" ; \
	   echo "  firecracker/Dockerfile: version=$$fc_v protocol=$$fc_p" ; \
	   echo "  crates/shed-core RELEASE_PIN version:            $$pin_v" ; \
	   echo "  crates/shed-core LATEST_KNOWN_RELEASE protocol:  $$known_p" ; \
	   echo "Bump in lockstep (see docs/reference/images.md)." ; \
	   exit 1 ; \
	 fi ; \
	 if [ "$$vz_amd64" != "$$fc_amd64" ] || [ "$$vz_arm64" != "$$fc_arm64" ]; then \
	   echo "ERROR: roost-session digests drifted between Dockerfiles:" ; \
	   if [ "$$vz_amd64" != "$$fc_amd64" ]; then \
	     echo "  ROOST_SESSION_SHA256_AMD64: vz=$$vz_amd64 fc=$$fc_amd64" ; \
	   fi ; \
	   if [ "$$vz_arm64" != "$$fc_arm64" ]; then \
	     echo "  ROOST_SESSION_SHA256_ARM64: vz=$$vz_arm64 fc=$$fc_arm64" ; \
	   fi ; \
	   echo "Both Dockerfiles must carry the identical digest pair (see docs/reference/images.md)." ; \
	   exit 1 ; \
	 fi ; \
	 echo "roost-session pin OK: $$vz_v / protocol $$vz_p"

# check-gce-pins: the GCE image bake (scripts/build-gce-image.sh, gce/) takes
# its roost-session and craze pins from firecracker/Dockerfile through
# scripts/read-guest-pins.sh, so check-roost-pin and check-craze-pin above
# (which hold that Dockerfile in lockstep with vz/, crates/shed-core and
# craze-pin.env) guard the cloud image too. This target, a prerequisite of
# both, fails if the reader cannot produce all seven values or if gce/ holds a
# literal pin of its own (a version or any 64-hex digest).
check-gce-pins:
	@pins="$$(scripts/read-guest-pins.sh firecracker/Dockerfile)" || { \
	   echo "ERROR: scripts/read-guest-pins.sh could not read the guest pins from firecracker/Dockerfile" ; exit 1 ; } ; \
	 bad=0 ; \
	 for v in $$(printf '%s\n' "$$pins" | grep -E '^(ROOST_SESSION_VERSION|CRAZE_VERSION)=' | cut -d= -f2) ; do \
	   if grep -rnwF -- "$$v" gce/ ; then \
	     echo "ERROR: gce/ holds the literal pin version $$v; it must come from scripts/read-guest-pins.sh" ; bad=1 ; \
	   fi ; \
	 done ; \
	 if grep -rnE '[0-9a-f]{64}' gce/ ; then \
	   echo "ERROR: gce/ holds a literal sha256; digests must come from scripts/read-guest-pins.sh" ; bad=1 ; \
	 fi ; \
	 [ "$$bad" -eq 0 ] || exit 1 ; \
	 echo "gce pins OK: read from firecracker/Dockerfile, none literal in gce/"

# gce-image: bake a shed remote VM image on GCE (scripts/build-gce-image.sh).
# Project, zone, network, subnet and tag come from the environment
# (SHED_GCE_PROJECT, SHED_GCE_ZONE, SHED_GCE_NETWORK, SHED_GCE_SUBNET,
# SHED_GCE_BAKE_TAG or SHED_GCE_TAG) or GCE_IMAGE_ARGS, e.g.
#   make gce-image GCE_IMAGE_ARGS="--env-file ~/my-gce.env --label purpose=test"
#   make gce-image GCE_IMAGE_ARGS=--print     # show the gcloud steps, run nothing
GCE_IMAGE_ARGS ?=
gce-image:
	scripts/build-gce-image.sh $(GCE_IMAGE_ARGS)

# test-gce-image: offline checks of the image tooling (syntax, shellcheck when
# installed, the --print golden, gce/shed-identity.sh against a fake metadata
# server). Needs python3, curl and ssh-keygen.
test-gce-image:
	scripts/test-gce-image.sh

# CRAZE_SRC: the dedicated cache clone craze-binaries builds from (plan 025
# Amendment A4). NEVER ~/projects/craze and NEVER a `git worktree add` into
# it — that still writes into the owner's working repository, and another
# session may be working in it. A plain clone/fetch here touches nothing but
# this cache dir.
CRAZE_SRC ?= $(HOME)/.cache/shed/craze-src

# craze-binaries: builds craze, craze-fake-host and craze-fake-agent at the
# pinned CRAZE_TEST_SHA (craze-pin.env), plus craze at the real v0.0.1 tag as
# craze-0.0.1 (the too-old test cell, plan 025 Amendment A1 — stamped with
# craze's own 0.0.1 source default, never test-<sha12>), into OUT (default
# ~/.cache/shed/craze-<sha12>/). Asserts the pin is a MERGED craze main
# commit (O4: never a branch head) before building. `set -e`: a failed
# fetch/checkout/build fails the target rather than being masked by a later
# successful step. CRAZE_SRC is forced clean at each checkout
# (`--force --detach` + `git clean -fdx`) — safe because it is shed's own
# cache clone (never `~/projects/craze` or a worktree of it, Amendment A4),
# so there is no owner work to lose; this is what makes the target
# idempotent and immune to a half-finished prior run leaving stray files.
# OUT is resolved to an ABSOLUTE path up front: `go build -o` runs inside a
# `cd "$(CRAZE_SRC)"` subshell, so a relative OUT would otherwise be written
# relative to CRAZE_SRC instead of the caller's cwd (`OUT=bin` must mean
# `./bin` next to the shed repo, not `$(CRAZE_SRC)/bin`). Prints the
# resolved absolute SHED_CRAZE_BIN_DIR= line a caller exports.
craze-binaries:
	@set -eu; \
	 sha="$$(scripts/release/read-craze-pin.sh --field CRAZE_TEST_SHA craze-pin.env)"; \
	 sha12=$$(printf '%s' "$$sha" | cut -c1-12); \
	 out="$${OUT:-$(HOME)/.cache/shed/craze-$$sha12}"; \
	 case "$$out" in \
	   /*) ;; \
	   *) out="$$(pwd)/$$out" ;; \
	 esac; \
	 craze_src_abs="$(CRAZE_SRC)"; \
	 case "$$craze_src_abs" in \
	   /*) ;; \
	   *) craze_src_abs="$$(pwd)/$$craze_src_abs" ;; \
	 esac; \
	 case "$$out" in \
	   "$$craze_src_abs" | "$$craze_src_abs"/*) \
	     echo "ERROR: OUT ($$out) resolves inside CRAZE_SRC ($$craze_src_abs) — every checkout force-cleans CRAZE_SRC (git clean -fdx below), which would delete anything just built there even though this target would still report success. Pick an OUT outside CRAZE_SRC." >&2; \
	     exit 1 ;; \
	 esac; \
	 mkdir -p "$$out"; \
	 if [ -d "$(CRAZE_SRC)/.git" ]; then \
	   echo "craze-binaries: fetching charliek/craze into $(CRAZE_SRC)"; \
	   git -C "$(CRAZE_SRC)" fetch --quiet origin; \
	 else \
	   echo "craze-binaries: cloning charliek/craze into $(CRAZE_SRC)"; \
	   git clone --quiet https://github.com/charliek/craze "$(CRAZE_SRC)"; \
	 fi; \
	 if ! git -C "$(CRAZE_SRC)" merge-base --is-ancestor "$$sha" origin/main; then \
	   echo "ERROR: CRAZE_TEST_SHA $$sha is not an ancestor of charliek/craze origin/main (O4: a merged main commit, never a branch head)" >&2; exit 1; \
	 fi; \
	 git -C "$(CRAZE_SRC)" checkout --quiet --force --detach "$$sha"; \
	 git -C "$(CRAZE_SRC)" clean --quiet -fdx; \
	 ( cd "$(CRAZE_SRC)" && CGO_ENABLED=0 go build -trimpath \
	     -ldflags "-X github.com/charliek/craze/internal/version.Version=test-$$sha12" \
	     -o "$$out/" ./cmd/craze ./cmd/craze-fake-host ./cmd/craze-fake-agent ); \
	 git -C "$(CRAZE_SRC)" fetch --quiet --tags origin v0.0.1; \
	 git -C "$(CRAZE_SRC)" checkout --quiet --force --detach v0.0.1; \
	 git -C "$(CRAZE_SRC)" clean --quiet -fdx; \
	 ( cd "$(CRAZE_SRC)" && CGO_ENABLED=0 go build -trimpath -o "$$out/craze-0.0.1" ./cmd/craze ); \
	 git -C "$(CRAZE_SRC)" checkout --quiet --force --detach "$$sha"; \
	 git -C "$(CRAZE_SRC)" clean --quiet -fdx; \
	 echo "SHED_CRAZE_BIN_DIR=$$out"

# check-craze-pin: craze-pin.env (plan 025) must be well formed, and when
# CRAZE_RELEASE is set, the baked Dockerfiles' CRAZE_VERSION ARGs must agree
# with it and with each other's digest pair (vz==fc) — the same fail-fast-
# on-drift shape as check-roost-pin above. Offline, macOS-portable
# awk/sed -E/grep -E only.
#
# CRAZE_RELEASE is empty until the bake lands (plan 025 §3.9 / C13): until
# then NEITHER Dockerfile may DECLARE an ARG CRAZE_VERSION at all — checked
# by a DECLARATION regex (`^[[:space:]]*ARG[[:space:]]+CRAZE_VERSION([=[:space:]]|$)`,
# comment lines excluded first), not by whether a default value happened to
# be extracted, so `ARG CRAZE_VERSION` or `ARG CRAZE_VERSION=` (no default)
# is still caught as a half-landed bake even though it carries no value to
# compare.
#
# The vendored-fixture clause (plan 025 C7): crates/shed-craze/fixtures/wire.PIN
# — the craze sha the vendored WIRE fixtures were copied from — must equal
# CRAZE_TEST_SHA, so moving the test pin without re-vendoring the fixtures (or
# the other way round) fails here, offline, before CI's drift diff ever runs.
check-craze-pin: check-gce-pins
	@sha="$$(scripts/release/read-craze-pin.sh --field CRAZE_TEST_SHA craze-pin.env)" || exit 1; \
	 rel="$$(scripts/release/read-craze-pin.sh --field CRAZE_RELEASE craze-pin.env)" || exit 1; \
	 pin_file=crates/shed-craze/fixtures/wire.PIN; \
	 if [ ! -f "$$pin_file" ]; then \
	   echo "ERROR: $$pin_file is missing — it names the craze sha the vendored wire fixtures were copied from" ; \
	   exit 1 ; \
	 fi ; \
	 wire_pin=$$(tr -d '[:space:]' < "$$pin_file") ; \
	 if [ "$$wire_pin" != "$$sha" ]; then \
	   echo "ERROR: $$pin_file ($$wire_pin) != craze-pin.env CRAZE_TEST_SHA ($$sha)" ; \
	   echo "Re-vendor craze's internal/fakehost/testdata/wire (README.md included) at CRAZE_TEST_SHA into crates/shed-craze/fixtures/wire and write the sha to $$pin_file." ; \
	   exit 1 ; \
	 fi ; \
	 vz_decl=no; fc_decl=no; \
	 grep -Ev '^[[:space:]]*#' vz/Dockerfile | grep -Eq '^[[:space:]]*ARG[[:space:]]+CRAZE_VERSION([=[:space:]]|$$)' && vz_decl=yes; \
	 grep -Ev '^[[:space:]]*#' firecracker/Dockerfile | grep -Eq '^[[:space:]]*ARG[[:space:]]+CRAZE_VERSION([=[:space:]]|$$)' && fc_decl=yes; \
	 vz_v=$$(sed -nE 's/^[[:space:]]*ARG[[:space:]]+CRAZE_VERSION=(.*)$$/\1/p' vz/Dockerfile | head -1) ; \
	 fc_v=$$(sed -nE 's/^[[:space:]]*ARG[[:space:]]+CRAZE_VERSION=(.*)$$/\1/p' firecracker/Dockerfile | head -1) ; \
	 if [ -z "$$rel" ]; then \
	   if [ "$$vz_decl" = "yes" ] || [ "$$fc_decl" = "yes" ]; then \
	     echo "ERROR: craze-pin.env CRAZE_RELEASE is empty but a Dockerfile already declares ARG CRAZE_VERSION:" ; \
	     echo "  vz/Dockerfile declares it:          $$vz_decl (value='$$vz_v')" ; \
	     echo "  firecracker/Dockerfile declares it: $$fc_decl (value='$$fc_v')" ; \
	     echo "Bake landed without CRAZE_RELEASE being set (see plan 025 S3.9)." ; \
	     exit 1 ; \
	   fi ; \
	   echo "craze pin OK: no release baked yet (CRAZE_TEST_SHA=$$sha, wire.PIN matches)" ; \
	 else \
	   vz_amd64=$$(awk -F= '/^ARG CRAZE_SHA256_AMD64=/ { print $$2; exit }' vz/Dockerfile) ; \
	   fc_amd64=$$(awk -F= '/^ARG CRAZE_SHA256_AMD64=/ { print $$2; exit }' firecracker/Dockerfile) ; \
	   vz_arm64=$$(awk -F= '/^ARG CRAZE_SHA256_ARM64=/ { print $$2; exit }' vz/Dockerfile) ; \
	   fc_arm64=$$(awk -F= '/^ARG CRAZE_SHA256_ARM64=/ { print $$2; exit }' firecracker/Dockerfile) ; \
	   if [ "$$vz_decl" != "yes" ] || [ "$$fc_decl" != "yes" ] || [ -z "$$vz_v" ] || [ -z "$$fc_v" ] || [ -z "$$vz_amd64" ] || [ -z "$$fc_amd64" ] || [ -z "$$vz_arm64" ] || [ -z "$$fc_arm64" ]; then \
	     echo "ERROR: craze-pin.env CRAZE_RELEASE=$$rel but a Dockerfile is missing a CRAZE ARG or its value:" ; \
	     echo "  vz/Dockerfile declares ARG CRAZE_VERSION:          $$vz_decl (value='$$vz_v')" ; \
	     echo "  firecracker/Dockerfile declares ARG CRAZE_VERSION: $$fc_decl (value='$$fc_v')" ; \
	     echo "  vz/Dockerfile ARG CRAZE_SHA256_AMD64:          $$vz_amd64" ; \
	     echo "  firecracker/Dockerfile ARG CRAZE_SHA256_AMD64: $$fc_amd64" ; \
	     echo "  vz/Dockerfile ARG CRAZE_SHA256_ARM64:          $$vz_arm64" ; \
	     echo "  firecracker/Dockerfile ARG CRAZE_SHA256_ARM64: $$fc_arm64" ; \
	     exit 1 ; \
	   fi ; \
	   if [ "$$vz_v" != "$$rel" ] || [ "$$fc_v" != "$$rel" ]; then \
	     echo "ERROR: craze-pin.env CRAZE_RELEASE=$$rel but a Dockerfile's CRAZE_VERSION disagrees:" ; \
	     echo "  vz/Dockerfile:          CRAZE_VERSION=$$vz_v" ; \
	     echo "  firecracker/Dockerfile: CRAZE_VERSION=$$fc_v" ; \
	     exit 1 ; \
	   fi ; \
	   if [ "$$vz_amd64" != "$$fc_amd64" ] || [ "$$vz_arm64" != "$$fc_arm64" ]; then \
	     echo "ERROR: craze digests drifted between Dockerfiles:" ; \
	     if [ "$$vz_amd64" != "$$fc_amd64" ]; then echo "  CRAZE_SHA256_AMD64: vz=$$vz_amd64 fc=$$fc_amd64" ; fi ; \
	     if [ "$$vz_arm64" != "$$fc_arm64" ]; then echo "  CRAZE_SHA256_ARM64: vz=$$vz_arm64 fc=$$fc_arm64" ; fi ; \
	     exit 1 ; \
	   fi ; \
	   echo "craze pin OK: release $$rel (CRAZE_TEST_SHA=$$sha, wire.PIN matches)" ; \
	 fi

# Run all checks (lint + test + kernel pin)
# check-tool-pins: golangci-lint and goreleaser are pinned in .mise.toml, ci.yml
# and publish-images.yaml; the release job runs its own copies on a tag, so a
# pin that moved in CI alone stops a release after the tag exists.
check-tool-pins:
	@scripts/release/check-tool-pins.sh

check: check-kernel-pin check-roost-pin check-craze-pin check-tool-pins lint test

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
	uv run --locked zensical build --strict

# Serve documentation locally
docs-serve:
	uv sync --group docs
	uv run --locked zensical serve

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
