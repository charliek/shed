# shed-desktop — common dev tasks. Run `make` (or `make help`) to list them.
#
# The shed desktop app: a Tauri client (macOS + Linux) over the shared Rust
# core in ../crates, with an in-process JSON IPC socket that `shedctl` and the
# pytest harness drive, so changes can be verified (and screenshotted) without a
# human clicking.

.DEFAULT_GOAL := help

# The desktop package lives at <monorepo>/desktop; the shared Rust core is the
# sibling <monorepo>/crates workspace. REPO_ROOT is the monorepo root — the
# Docker legs mount it so the Tauri path-deps (../../../crates) resolve inside
# the recreated /work layout exactly as they do in the repo.
REPO_ROOT := $(abspath $(CURDIR)/..)

# Paths excluded from every Docker-leg source tarball: build outputs + the
# mac-only staged Sparkle framework/dist (a dev Mac's staging must never enter
# the containers).
TAR_EXCLUDES := --exclude=desktop/tauri/src-tauri/target \
  --exclude=desktop/tauri/src-tauri/Sparkle.framework \
  --exclude=desktop/tauri/src-tauri/.sparkle-dist \
  --exclude=desktop/tauri/ui/node_modules \
  --exclude=crates/target

.PHONY: help
help:  ## List available targets
	@echo "shed-desktop dev tasks:"
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
	  | sort \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

# ---- everyday aliases ---------------------------------------------------
# The short names point at the Tauri client + the Rust core, so the obvious
# commands do the obvious thing. Each one is a plain alias for the target named
# in its help text.

.PHONY: build test lint fmt e2e run
build: tauri-build  ## Alias: tauri-build
test: core-test tauri-test  ## Alias: core-test + tauri-test (cargo unit tests)
lint: core-lint tauri-lint  ## Alias: core-lint + tauri-lint (clippy, deny warnings)
fmt: core-fmt  ## cargo fmt the Rust core + the Tauri crate
	cd tauri/src-tauri && cargo fmt
e2e: e2e-tauri  ## Alias: e2e-tauri (the hermetic pytest harness)
run: tauri-run  ## Alias: tauri-run

# ---- rust core --------------------------------------------------------

.PHONY: core-test core-lint core-fmt core-linux
core-test:  ## cargo test the Rust core workspace (+ the non-default `broker` feature)
	cd ../crates && cargo test
	# `broker` (the embedded host-agent bridge) is non-default, so a bare
	# `cargo test` skips `broker_bridge.rs` entirely. `rc` was the other
	# non-default feature until plan 022 (S6, charliek/shed#328) deleted it.
	cd ../crates && cargo test -p shed-app --features broker

core-lint:  ## cargo clippy the Rust core (deny warnings; + the `broker` feature)
	cd ../crates && cargo clippy --workspace --all-targets -- -D warnings
	# The default clippy above never compiles the `broker` module.
	cd ../crates && cargo clippy -p shed-app --features broker --all-targets -- -D warnings

core-fmt:  ## cargo fmt the Rust core
	cd ../crates && cargo fmt --all

# ---- Linux .deb packaging (built from the Tauri client) --------------

DEB_VERSION ?= 0.0.1-dev

.PHONY: deb deb-validate
deb: tauri-ui-build  ## Build the shed-desktop Tauri .deb in Docker (ubuntu:24.04 + WebKitGTK + nfpm) → out/ (DEB_VERSION=x)
	# Reuses the render image (WebKitGTK build deps) + installs nfpm. The frontend
	# bundle (tauri/ui/dist) is built on the host first (tauri-ui-build); the source
	# is copied into a writable /work (Tauri's build.rs writes gen/ next to Cargo.toml,
	# so a read-only mount fails + would leave root-owned files in the repo). CARGO_TARGET_DIR
	# gives build-deb.sh its /target/{tauri,core} split; the .deb is copied back to ./out.
	docker build -t shed-tauri-linux:latest - < Dockerfile.tauri-linux
	mkdir -p "$(CURDIR)/out"
	docker run --rm \
	  -v "$(REPO_ROOT):/repo:ro" \
	  -v "$(CURDIR)/out:/out" \
	  -v shed-tauri-linux-cargo:/usr/local/cargo/registry \
	  -v shed-tauri-linux-cargo-git:/usr/local/cargo/git \
	  -v shed-tauri-linux-target:/target \
	  -e CARGO_TARGET_DIR=/target \
	  -w /repo shed-tauri-linux:latest \
	  bash -lc 'set -e; \
	    echo "deb [trusted=yes] https://repo.goreleaser.com/apt/ /" > /etc/apt/sources.list.d/goreleaser.list && \
	    apt-get update -qq && apt-get install -y -qq nfpm >/dev/null && \
	    mkdir -p /work && \
	    tar -C /repo $(TAR_EXCLUDES) \
	      -cf - crates desktop/tauri desktop/linux desktop/packaging desktop/Resources desktop/pyproject.toml desktop/uv.lock | tar -C /work -xf - && \
	    cd /work/desktop && ./linux/scripts/build-deb.sh $(DEB_VERSION) && \
	    cp /work/desktop/out/*.deb /out/'

deb-validate: deb  ## Build + install-validate the .deb in a clean ubuntu:24.04 container
	./linux/scripts/validate-deb.sh $$(ls -t out/shed-desktop_*.deb | head -1)

# ---- tauri (Phase A: a real Linux client toward Mac parity) -----------

UNAME_S := $(shell uname -s)

.PHONY: tauri-build tauri-run tauri-lint tauri-test tauri-ui-build tauri-build-linux tauri-test-linux
.PHONY: sparkle-framework tauri-bundle-mac tauri-dmg-mac

sparkle-framework:  ## Stage Sparkle.framework + bin tools for the Tauri mac app (pinned; idempotent)
	./scripts/fetch-sparkle.sh

# On Darwin every Tauri mac build needs Sparkle.framework staged first:
# tauri.macos.conf.json's bundle.macOS.frameworks references it (the bundler
# copies it at bundle time), and the sparkle-updater crate's build.rs (C2)
# links it and panics when it is absent. Linux never compiles that crate
# (mac-only target dep), and the Docker legs --exclude the staged framework
# so it never enters the containers.
ifeq ($(UNAME_S),Darwin)
tauri-build: sparkle-framework
tauri-run: sparkle-framework
tauri-lint: sparkle-framework
tauri-test: sparkle-framework
endif

tauri-bundle-mac: sparkle-framework  ## Build + sign the Tauri mac app -> build/ShedDesktop.app
	./scripts/bundle-tauri-mac.sh

tauri-dmg-mac: tauri-bundle-mac  ## Tauri mac bundle + drag-install DMG -> build/ShedDesktop-<ver>.dmg
	./scripts/make-dmg.sh

tauri-ui-build:  ## Type-check + unit-test + build the Vite/React frontend bundle (tauri/ui/dist)
	# `npm run build` is tsc --noEmit, then `npm run test` (node's own runner over
	# the frontend's pure helpers — no framework, nothing new in the lock), then
	# vite. The test leg rides here rather than in a target of its own so every
	# path that produces the bundle (this, the Docker legs, `make deb`, CI's
	# direct `npm run build`) runs it.
	cd tauri/ui && npm run build

tauri-build: tauri-ui-build  ## Build the Tauri client: the frontend bundle + the standalone Rust workspace
	cd tauri/src-tauri && cargo build

tauri-run: tauri-ui-build  ## Build the frontend bundle + launch the Tauri client (loads the embedded dist)
	cd tauri/src-tauri && cargo run

tauri-lint: tauri-ui-build  ## clippy the Tauri client (its own standalone workspace; kept out of core-lint)
	# Needs the frontend bundle: compiling the crate runs tauri's generate_context!,
	# which fails closed if tauri.conf.json's frontendDist (../ui/dist) is absent.
	cd tauri/src-tauri && cargo clippy --all-targets -- -D warnings

tauri-test: tauri-ui-build  ## cargo test the Tauri client locally (its standalone workspace; the cross-platform dbus_notify race tests etc. — Linux-only approval-seam tests run under tauri-test-linux)
	cd tauri/src-tauri && cargo test

# The render gate: build the Tauri Rust app + run the hermetic e2e on
# ubuntu:24.04 / WebKitGTK 2.44 under Xvfb, so the oklch linen theme + the
# drivability probes + the scrot screenshot are verified on the real shipped
# WebView (a static CSS denylist would be miscalibrated — 2.44 supports oklch,
# color-mix, :has(), @container — so this render smoke IS the CSS gate). The
# frontend bundle is built on the host first (platform-independent); the source
# (tauri + the core/ path-dep crates it links: shed-core + shed-app + Resources/
# with the bundled terminal openers that build.rs validates) is copied into a
# writable /work because Tauri's build.rs writes gen/ next to Cargo.toml (a
# read-only mount fails there); the Rust builds to a /target volume
# so it never clobbers the mac target dir. Both cargo caches are volumes — the
# registry AND `git`, because shed-core takes `roost-ipc` as a git dependency
# (plan 013) and an uncached /usr/local/cargo/git re-clones roost on every run.
# --cap-add SYS_ADMIN + seccomp=unconfined let WebKitGTK's web-process bubblewrap
# sandbox create the user namespaces Docker's default seccomp blocks (else the
# content process dies and JS never runs).
#
# The craze cells (plan 025 §3.5/§3.6.7): `docker run` forwards only explicit
# `-e` variables, so SHED_CRAZE_REQUIRE and SHED_CRAZE_BIN_DIR are passed through
# by name (unset on the host = unset in the container). The binaries ride the
# tarred `desktop/tools` — build them into the gitignored
# `desktop/tools/shedtest/.craze-bin` with `make craze-binaries
# OUT=desktop/tools/shedtest/.craze-bin`, and point the container at them:
# `SHED_CRAZE_REQUIRE=1 SHED_CRAZE_BIN_DIR=/work/desktop/tools/shedtest/.craze-bin
# make -C desktop tauri-build-linux` (or `tauri-test-linux`). With REQUIRE set
# and the directory empty, the craze cells FAIL rather than skip.
tauri-build-linux: tauri-ui-build  ## Render gate: Tauri e2e on ubuntu:24.04 + WebKitGTK 2.44 under Xvfb
	docker build -t shed-tauri-linux:latest - < Dockerfile.tauri-linux
	docker run --rm \
	  --cap-add SYS_ADMIN --security-opt seccomp=unconfined --shm-size=1g \
	  -v "$(REPO_ROOT):/repo:ro" \
	  -v shed-tauri-linux-cargo:/usr/local/cargo/registry \
	  -v shed-tauri-linux-cargo-git:/usr/local/cargo/git \
	  -v shed-tauri-linux-target:/target \
	  -w /repo \
	  -e CARGO_TARGET_DIR=/target \
	  -e UV_PROJECT_ENVIRONMENT=/tmp/uv-venv \
	  -e SHED_TAURI_BIN=/target/debug/shed-desktop-tauri \
	  -e SHED_TAURI_TEST_TIMEOUT_SCALE=6 \
	  -e SHED_CRAZE_REQUIRE -e SHED_CRAZE_BIN_DIR \
	  shed-tauri-linux:latest \
	  bash -lc 'set -e; mkdir -p /work; \
	    tar -C /repo $(TAR_EXCLUDES) \
	      -cf - crates desktop/tauri desktop/tools desktop/Resources desktop/pyproject.toml desktop/uv.lock | tar -C /work -xf -; \
	    cd /work/desktop && (cd tauri/src-tauri && cargo build --locked) && \
	    xvfb-run -a --server-args="-screen 0 1400x900x24" \
	      uv run --group test pytest tools/shedtest -q -p no:cacheprovider'

# The Tauri Rust crate's unit tests on Linux (the platform where the polkit
# AuthGate + libnotify Notifier are compiled): asserts the gate is fail-closed
# (never Approved without a real polkit auth; biometrics-only → Unavailable). Uses
# the same image as the render gate; no display needed. Joins the tauri CI leg.
# It lints that cfg first (`cargo clippy -D warnings`) — `tauri-lint` only runs on
# the Mac leg, so nothing else would catch a Linux-only lint.
tauri-test-linux: tauri-ui-build  ## Tauri crate cargo test on ubuntu:24.04 (Linux-only approval-seam tests)
	# tauri-ui-build first: `cargo test` compiles the crate, whose generate_context!
	# fails closed without the frontendDist bundle (tarred into the container below).
	docker build -t shed-tauri-linux:latest - < Dockerfile.tauri-linux
	docker run --rm \
	  -v "$(REPO_ROOT):/repo:ro" \
	  -v shed-tauri-linux-cargo:/usr/local/cargo/registry \
	  -v shed-tauri-linux-cargo-git:/usr/local/cargo/git \
	  -v shed-tauri-linux-target:/target \
	  -w /repo \
	  -e CARGO_TARGET_DIR=/target \
	  -e SHED_CRAZE_REQUIRE -e SHED_CRAZE_BIN_DIR \
	  shed-tauri-linux:latest \
	  bash -lc 'set -e; mkdir -p /work; \
	    tar -C /repo $(TAR_EXCLUDES) \
	      -cf - crates desktop/tauri desktop/tools desktop/Resources desktop/pyproject.toml desktop/uv.lock | tar -C /work -xf -; \
	    cd /work/desktop/tauri/src-tauri && \
	    cargo clippy --locked --all-targets -- -D warnings && cargo test --locked'

core-linux:  ## Build+test shed-core on Linux in Docker (ubuntu:24.04; ring needs build-essential)
	docker build -t shed-core-linux:latest - < Dockerfile.linux
	docker run --rm \
	  -v "$(REPO_ROOT)/crates:/work:ro" \
	  -v shed-core-linux-target:/target \
	  -v shed-core-linux-cargo:/usr/local/cargo/registry \
	  -e CARGO_TARGET_DIR=/target \
	  -w /work shed-core-linux:latest \
	  bash -lc 'cargo test -p shed-core --all-targets --locked && \
	            cargo clippy -p shed-core --all-targets --locked -- -D warnings'

# ---- test -------------------------------------------------------------

.PHONY: e2e-tauri
e2e-tauri: tauri-build  ## Tauri e2e: the hermetic pytest harness (needs a display; Xvfb on Linux)
	uv run --group test pytest tools/shedtest -q

# ---- docs -------------------------------------------------------------

.PHONY: docs docs-serve
docs:  ## Build the mkdocs site into site-build/
	uv run --locked --group docs zensical build --strict

docs-serve:  ## Serve the docs locally with live reload
	uv run --locked --group docs zensical serve

# ---- clean ------------------------------------------------------------

.PHONY: clean
clean:  ## Remove build artifacts
	rm -rf build site-build
