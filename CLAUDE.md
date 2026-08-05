# CLAUDE.md

Project context for AI assistants working on this codebase.

## Project Overview

Shed is a CLI tool for managing persistent, VM-based development environments across multiple servers. It supports Firecracker microVMs (Linux) and Apple VZ virtual machines (macOS).

## Monorepo layout

The Go tree (`cmd/`, `internal/`, the CLI/server/agent) is the primary product. Two imported subtrees live alongside it, each an isolated build world with its own nested `CLAUDE.md` — read that before working in it:

- **`crates/`** — the shared **Rust client core** (`shed-core`, `shed-core-ffi`, `shed-app`, `shedctl`), its own Cargo workspace + committed `Cargo.lock` + `rust-toolchain.toml`. Dependency-clean: WebKitGTK/Tauri deps must never enter it. See `crates/CLAUDE.md`.
- **`desktop/`** — the desktop app: the **Swift macOS** menu-bar app (transitional — the Tauri client is on track to eventually replace it) + the **Tauri Linux** app + packaging + the `shedtest` harness. A **second, isolated** Cargo workspace (`desktop/tauri/src-tauri`, on purpose so its WebKitGTK deps stay out of `crates/`) and its own `uv` env (`desktop/pyproject.toml`). See `desktop/CLAUDE.md`.

The Go build never traverses `crates/` or `desktop/` (`go list ./...` / `golangci-lint` stay Go-only); root `make build`/`test`/`check` are Go-only. Desktop targets are reached via the reserved `desktop-` passthrough (`make desktop-<target>`, e.g. `make desktop-build`) or `make -C desktop <target>`.

**Three pytest suites, never merged:** `tests/integration/` is the live server-facing create-cycle suite (drives a real `shed-server`); `desktop/tools/shedtest/` is the mock-server UI harness (drives the real app over its IPC socket, hermetic); `tests/host-agent-diff/` is the hermetic shed-host-agent **wire** harness — it spawns the Rust `crates/shed-host-agent` daemon binary and asserts its wire-visible output, under a defined canonicalization, against committed goldens (recorded from the last agreeing Go↔Rust run before the Go daemon was retired in plan 006), so it needs Rust + Python/uv, no Go toolchain (run it with `make test-host-agent-diff`). Different worlds — don't cross-wire them.

## Release model

One `vX.Y.Z` tag family, **manifest-selected** — a component ships iff its version manifest equals the tag:

- **server** selector = `.claude-plugin/plugin.json` (renamed from `go`; file unchanged); ships the CLI/server/agent brew + apt + rootfs images.
- **host-agent** selector = `crates/shed-host-agent/VERSION`; ships brew `shed-host-agent` + a GH linux tarball (brew-only, no apt).
- **machine-rc** selector = `cmd/shed-machine-rc/VERSION`; ships brew + apt `shed-machine-rc`.
- **desktop** selector = `desktop/VERSION` (with `crates/Cargo.toml`, the Tauri `Cargo.toml`/`tauri.conf.json`, and both Cargo locks in verified **lockstep**); ships the DMG + Sparkle appcast + `shed-desktop` debs.

`scripts/release/update-version.sh X.Y.Z --components server,host-agent,machine-rc,desktop` bumps the selected manifests (default `server`; `go` accepted as a deprecated alias). `scripts/release/recommend-components.sh X.Y.Z` recommends the component set (all four on a minor/major bump, only what changed since each component's last-shipped tag on a patch) for the human to confirm/edit before bumping. `scripts/release/release-plan.sh` maps a tag → `ship_server`/`ship_host_agent`/`ship_machine_rc`/`ship_desktop` and **exits 1 if none match** (forgotten-bump guard). Each `CHANGELOG.md` entry opens with a `**Ships:**` line naming the components — **enforced** by `release-plan.sh` against the manifest-computed set on stable tags. A **desktop-only** tag publishes **no** rootfs images; a **helper-only** tag (host-agent and/or machine-rc, no server) also publishes no images and leaves the other components' brew/apt entries pinned at their prior release. Full detail in `RELEASING.md` (and `desktop/RELEASING.md` for the recurring desktop steps).

## Build & Test

```bash
make build              # Build all binaries (shed, shed-server, shed-agent) into bin/
make test               # Run all unit tests
make lint               # Run golangci-lint
make fmt                # Format code with gofmt
make check              # Run lint + test
make coverage           # Tests with coverage report
make test-integration   # Run the pytest-based integration suite (see below)
```

Tools are managed via [mise](https://mise.jdx.dev/) — run `mise install` to set up Go and golangci-lint.

### Integration test suite (`tests/integration/`)

Live create-cycle tests that drive a running `shed-server` via the `shed` CLI. Pytest + subprocess (Fabric reserved for the few remote-orchestration tasks that need it), managed with `uv`. Complements the Go unit tests — those catch logic / file-shape regressions on every PR; the integration suite catches live boot-path / SSE / timing regressions that need a real VM.

```bash
# One-time: install uv (https://docs.astral.sh/uv/getting-started/installation/)
brew install uv

# Run the suite against whichever shed-server is currently INSTALLED
# on the host (brew on Mac, deb on Linux). Use this for CLI/client
# changes; see "Server-side changes" below for server changes.
make test-integration
```

The suite is **parameterized over `["vz", "fc"]`** and skips cleanly when a backend is unreachable from this host. On a Mac with the brew-installed `shed-server`, VZ tests run against the entry named by `$SHED_VZ_SERVER` (default `my-server`) in `~/.shed/config.yaml`; FC tests target the entry named by `$SHED_FC_HOST` (default `mini3`) over SSH. Both have env-var overrides — see the table below and `tests/integration/README.md` for the full setup checklist.

**FC live tests require the remote `shed-server` to emit `PhaseTimer` log lines** (added in v0.5.4 via PR #118). Two tests (`test_phase_timer_emitted[fc]`, `test_create_agent_p50[fc]`) skip cleanly with a clear message if the remote is older — the other tests work against any shed-server version. Once the remote upgrades to v0.5.4+, the suite picks up the FC tests automatically with no config change.

> **In-VM agent (`cmd/shed-agent`) changes have an extra trap:** the agent is baked into the rootfs **image**, not the host `shed-server`, so `make test-integration-dev` (which only restarts the dev *server*) does **not** pick up agent changes — you must rebuild the rootfs into the dev image store. The in-repo skill **`.claude/skills/testing-vm-agent-changes`** documents that loop end-to-end (codesign, build-tools/BuildKit-cache/ref-index gremlins, running the linux-only agent unit tests via Docker, and verifying the VM is running *your* agent). Read it before validating an agent change, and **update it whenever you hit a new rough edge**.

#### Server-side changes — parallel dev server

`make test-integration` runs against whatever `shed-server` binary is currently **installed** on the host (brew on Mac, deb on Linux), not the source tree you're editing. A server-side-only change (orchestrator, lifecycle internals, backend handlers with no CLI-visible signature change) can pass `make test-integration` without ever executing the new code path — the installed binary is still the OLD one.

**Any server-side change must be validated against the developer's own source tree before opening a PR.** The mechanism is a **parallel dev shed-server** that runs alongside the brew/deb one on a different port — the production server keeps running undisturbed, the dev server is what the suite targets.

```bash
# One-time setup per developer (Mac):
#   1. Add a ~/.shed/config.yaml entry for the dev server (snippet below).
#   2. make dev-server-up           # launches dev shed-server on ports 18080/12222
#
# Per development cycle:
make dev-server-up                  # refuses if already running (use dev-server-restart)
make test-integration-dev           # run suite against dev server (auto-ups if needed)
# ... edit source ...
make build && make dev-server-restart   # pick up the rebuild
make test-integration-dev           # re-run
make dev-server-down                # when done
```

The dev server's lifecycle is intentionally `nohup` + PID file under `~/.shed/dev/` — no launchd plist, no auto-restart on crash (crashes should be visible), no survives-reboot. State-dirs are isolated under `~/Library/Application Support/shed-dev/vz/` so `shed image prune` from the brew server never touches dev blobs. The dev server's `SHED_BUILD_TOOLS_REF` is set inline to the latest release tag so the dev binary uses release-shaped upper-template behavior (sub-100 ms rootfs phase).

**`~/.shed/config.yaml` entry** (copy-paste alongside the existing `my-server` entry):

```yaml
servers:
  my-server-dev:
    host: localhost
    http_port: 18080
    ssh_port: 12222
```

Or run `shed server add localhost --port 18080 --ssh-port 12222 --name my-server-dev` after the first `make dev-server-up` — first contact is SSH-first, so the dev server's SSH port (12222) must be named explicitly.

**FC remote parallel-dev** (same shape, over SSH to `$SHED_FC_HOST`):

```bash
# One-time setup: register the FC dev entry (after first dev-server-up-fc):
shed server add mini3 --port 18080 --ssh-port 12222 --name mini3-dev

# Per dev cycle:
make dev-server-up-fc                    # launches dev shed-server on mini3:18080
make test-integration-dev-fc             # runs suite against FC dev (auto-ups if needed)
make build && make dev-server-restart-fc # pick up the rebuild
make dev-server-down-fc                  # when done
```

The FC dev server runs via `sudo nohup` on the remote (no systemd unit — same intentionally-ephemeral lifecycle as the Mac dev server). Listens on `mini3:18080/12222` alongside the deb shed-server's `mini3:8080/2222`. Separate state-dirs under `/var/lib/shed-dev/firecracker/`. Offset `vsock_base_cid: 600` (deb default is 100) to avoid CID collision. SHARED bridge/CIDR/tap_prefix — kernel-level TAP coordination handles the cross-server case.

**Open a server-side PR with `make test-integration-dev: N/N pass against dev-build at commit <sha>`**, and that statement is true and meaningful — not a brew-binary alibi.

**For PRs that change `shed-build-tools` (`build-tools/`) or base images (`vz/Dockerfile`, `firecracker/Dockerfile`, shed-extensions bumps):** the dev-server workflow extends to those too — see `docs/development/testing.md` § "Validating pre-release: build-tools image changes" and § "Validating pre-release: base image changes" for the `RELEASE_BUILD_TOOLS_REF=shed-build-tools:dev` and `OUTPUT_DIR=...shed-dev/vz ./scripts/build-vz-rootfs.sh ...` overrides that wire local builds into the dev server's blob store.

#### Performance impact — vet against the released version

For changes that touch the boot path, agent dial, healthPoll, upper-allocation, mount, image-resolution, or any other hot path: **measure the impact on each platform the change affects, against the most recent release binary, before merging.** The split timing gate (`test_create_agent_p50` + `test_create_rootfs_template_present`) is the floor — it fires on regressions around 500 ms or more (see `tests/integration/fixtures/server.py:DEFAULT_AGENT_P50_MS` for the per-backend regression budget) — but a sub-threshold regression (or worse, a "no regression" that masks an actual gain that didn't materialise) won't trip it.

The workflow uses the parallel-dev pair on both sides of the comparison — no service interruption needed:

1. **Baseline against release.** `make test-integration` (it targets the brew/deb install). Record the agent_p50 and rootfs_ms from the PhaseTimer line for each backend you're changing.
2. **Compare against your branch.** `make build && make dev-server-restart && make test-integration-dev` (it targets the parallel dev server running your source). Same measurements.
3. **Repeat on every affected backend.** A change shipping for both VZ and FC needs both backends measured — Apple Silicon vfkit and Linux KVM Firecracker have different floors and the same code can be faster on one and slower on the other.
4. **Record the measurements in the PR body.** Hypothesised gains that don't show up are worth investigating before merge.

This catches the v0.5.4-class regression where the binary built correctly but a config knob (build-tools-ref, healthPoll constant, etc.) silently disabled the fast path. The dynamic timing gate complements the unit tests; the per-platform measurement is the only safety net for changes whose value-add IS the timing characteristic.

#### Environment overrides (full list in `tests/integration/README.md`)

| Variable | Default | Effect |
|---|---|---|
| `SHED_VZ_SERVER` | `my-server` | `~/.shed/config.yaml` entry for the brew/deb VZ server. |
| `SHED_VZ_DEV_SERVER` | `my-server-dev` | `~/.shed/config.yaml` entry for the parallel dev VZ server. Honored by `make dev-server-up` / `dev-server-status` / `test-integration-dev`. |
| `SHED_VZ_LOG_PATH` | `/opt/homebrew/var/log/shed-server.log` | brew log path (override for Intel-Mac homebrew prefix or custom installs). Set by `test-integration-dev` to the dev log path automatically. |
| `SHED_FC_HOST` | `mini3` | SSH host for FC live tests + the `dev-server-*-fc` Makefile targets. |
| `SHED_FC_SERVER` | same as `$SHED_FC_HOST` | `~/.shed/config.yaml` entry name for the deb FC server. |
| `SHED_FC_DEV_SERVER` | `$(SHED_FC_HOST)-dev` | `~/.shed/config.yaml` entry name for the parallel dev FC server. |
| `SHED_FC_LOG_PATH` | _unset_ (uses journald) | Remote file path for `fc_server` fixture to read logs from (set by `test-integration-dev-fc` to the dev server's log file because the dev FC server runs via `sudo nohup` and isn't under systemd). |
| `RELEASE_BUILD_TOOLS_REF` | latest `git tag` matching `v*` | shed-build-tools image ref injected into the dev binary so it uses release-shaped upper-template behavior. Pin to an older release if your source has drifted: `RELEASE_BUILD_TOOLS_REF=ghcr.io/charliek/shed-build-tools:v0.5.7`. |

See `docs/development/testing.md` for the full operator guide — adding a test, the per-backend timing ceilings, the fixture conventions, the dev-server lifecycle.

## Project Structure

- `cmd/shed/` — CLI binary (command handlers split across `shed.go`, `console.go`, `attach.go`, `sessions.go`, `tunnels.go`, `sync.go`, `ssh_config.go`, etc.)
- `cmd/shed-server/` — Server daemon binary
- `cmd/shed-agent/` — In-VM agent binary (Firecracker/VZ)
- `crates/shed-host-agent/` — Host-side credential broker, in Rust (see `crates/CLAUDE.md`). The Go `cmd/shed-host-agent` it replaced was retired in plan 006. Its Touch ID / LocalAuthentication approval gate uses `objc2` to link the framework directly — no CGO. Its release job (`.goreleaser.host-agent.yaml`, `builder: rust`) still runs on **macos-latest** to match the real release runner (cross-compiling linux via `cargo zigbuild`).
- `cmd/shed-machine-rc/` — Host-side RC-session helper (native machine sibling of `shed-ext-rc`)
- `cmd/shed-ext-ssh-agent/`, `cmd/shed-ext-aws-credentials/`, `cmd/docker-credential-shed/`, `cmd/shed-ext-rc/` — In-VM guest extension binaries (imported from shed-extensions). Cross-compiled for linux and staged into the rootfs build context by `scripts/stage-guest-binaries.sh`; not shipped as host `bin/` artifacts.
- `internal/api/` — HTTP API handlers (Chi router)
- `internal/config/` — Configuration types and validation
- `internal/ext/` — Extensions internals (imported from shed-extensions: `protocol`, `sshagent`, `awsproxy`, `dockercred`, `rc`, `clirc`, `testutil`); no crates/Rust, all Go
- `internal/firecracker/` — Firecracker backend (Linux only, `//go:build linux`)
- `internal/vz/` — VZ backend (macOS only, `//go:build darwin`)
- `internal/vmutil/` — Shared VM agent communication (no build tags)
- `internal/agentproto/` — Binary protocol for vsock communication
- `internal/sshd/` — SSH server implementation
- `internal/backend/` — Backend interface all backends implement
- `guest/extensions/etc/` — Guest overlay (systemd units, `environment.d`, `shed-extensions.d` manifests) staged into the rootfs alongside the guest binaries
- `crates/` — shared Rust client core workspace (see `crates/CLAUDE.md` and "Monorepo layout" above)
- `desktop/` — Swift/Tauri desktop app + `shedtest` harness (see `desktop/CLAUDE.md`)
- `docs/` — Zensical documentation site (desktop docs under `docs/desktop/`)

## In-repo skills

In-repo skills under `.claude/skills/` (`testing-vm-agent-changes`, `shedtest-mac`, `shedtest-linux`) are **living docs**. Whenever you use one during a debugging loop and hit a rough edge it doesn't cover, **update the skill in the same PR** — that's what keeps the next session from re-losing the hour. (These are repo-local dev skills, distinct from the plugin skills under `skills/` that `scripts/validate-plugin.sh` gates.)

## Key Conventions

- **Go version**: 1.24+ (see `go.mod`)
- **Formatting**: `gofmt` — run `make fmt` before committing
- **Linting**: `golangci-lint` — run `make lint`
- **Tests**: Table-driven tests with `t.Run()`. Place `_test.go` files alongside source.
- **Build tags**: `linux` for Firecracker code, `darwin` for VZ code, `e2e` for Firecracker VM tests (requires KVM)
- **Config types**: All in `internal/config/types.go`
- **Home-rooted workspaces**: everything lives under the shed user's home dir `/home/shed` (`config.HomePath`). `--repo` clones into `~/<reponame>`; `--local-dir` mounts a host dir at `~/<basename>` and becomes the landing dir; `--add-dir` (repeatable, requires `--local-dir`) mounts additional host dirs at `~/<basename>` each. Interactive logins land in the shed's `LandingDir` (project dir, or `~` by default). There is no `/workspace` (removed in the home-rooted-workspaces change).
- **VM user**: `shed` (UID 1000) with passwordless sudo

## `shed exec` semantics and the SSH command channel

`shed exec` ships argv literally. The CLI single-quote-wraps each argv element before SSH (`cmd/shed/console.go:shellQuoteArgs`), and the server reparses the SSH command through `bash -lc` (`internal/sshd/wrap.go:wrapCommand`). Because bash treats single-quoted text as literal data, `shed exec name -- echo '$HOME'` echoes the literal `$HOME` — argv is preserved, shell metacharacters in user-supplied data don't escape.

Raw SSH (`ssh shed-name 'cmd | pipe'`) gets the full shell, because the client sends a raw command string and the server's `bash -lc` wrap interprets it. This matches Docker, Codespaces, devcontainers, and every other hosted-shell product, and is what Zed Remote-SSH, VS Code Remote-SSH, JetBrains Gateway, raw `ssh`, and `rsync` assume.

Implications when writing code or docs:

- `shed exec <name> -- mytool` runs `mytool` direct-argv via `bash -lc "'mytool'"` server-side — `mytool` must be on the shed user's login PATH (`/etc/profile.d/*.sh` and `/etc/environment.d/` defaults are sourced by the `-l`).
- Anything needing shell features inside `shed exec` still goes through `bash -c '…'` explicitly, as before; the difference is that `bash` is now also implicit on the SSH-server side, so direct argv elements get the same login-shell PATH treatment.
- Provisioning hooks are a separate path (`internal/vmutil/provisioning.go`) — they still run as `bash --login -c` and bypass the sshd wrap entirely.
- The CLI quoter (`cmd/shed/console.go:validateAndQuoteArgs`) is the **security gate**. It single-quotes each argv element and rejects empty elements + NUL bytes. The Go-level bash round-trip test (`cmd/shed/console_test.go:TestShellQuoteBashRoundTrip`) is the unit audit; the integration suite (`tests/integration/test_exec_shell.py`) is the live wire audit.
- The legacy idiom `shed exec name "cmd | with | pipes"` still doesn't work — the CLI strips the pipe by single-quoting; rewrite as `shed exec name -- bash -c 'cmd | with | pipes'`.

This convention is documented end-user-style in `docs/reference/cli.md` under `shed exec`.

## Backends

| Backend | Platform | Isolation | Workspace |
|---------|----------|-----------|-----------|
| Firecracker | Linux (KVM) | microVM | Rootfs ext4 image |
| VZ | macOS (Apple Silicon) | VM via vfkit | Overlay rootfs; host dirs via VirtioFS (`--local-dir`/`--add-dir`) |

The server config `default_backend` supports `vz`, `firecracker`, or `detect` (auto-selects based on platform).

## VM Images

VZ and Firecracker use multi-stage Dockerfiles (`vz/Dockerfile`, `firecracker/Dockerfile`) to build rootfs OCI images. Variants: `base`, `extensions`, `full`. The `extensions` and `full` variants install the four guest extension binaries (`shed-ext-ssh-agent`, `shed-ext-aws-credentials`, `docker-credential-shed`, `shed-ext-rc`) plus the `guest/extensions/etc/` overlay (systemd units, `environment.d`, `shed-extensions.d` manifests). These are **built in-tree** from `cmd/shed-ext-*` and cross-compiled + staged into the build context by `scripts/stage-guest-binaries.sh` (the single producer shared by the local rootfs scripts and the `publish-images.yaml` CI jobs) — there is no separate `ghcr.io/charliek/shed-extensions` image and no per-image version build-arg any more.

Since v0.5.2 the read-only rootfs **erofs is built at image-publish time** by `mkfs.erofs` running inside the `ghcr.io/charliek/shed-build-tools:vX.Y.Z` container (pinned `erofs-utils` version, tagged in lockstep with shed releases). The resulting erofs ships as a content-addressed OCI blob referenced by the `io.shed.rootfs.erofs.digest` manifest annotation. Hosts pull the blob and mount it directly — no on-host `mkfs.erofs` invocation. Pre-v0.5.2 images lack the annotation and are rejected at boot with a clear "rebuild against current tooling" error (see `internal/vmimage/manager.go:resolveManifestLower`).

- Build locally: `./scripts/build-vz-rootfs.sh --variant extensions`
- Local dev with guest-extension changes: edit `cmd/shed-ext-*` or `guest/extensions/etc/` and rebuild the `extensions` (or `full`) variant — `scripts/stage-guest-binaries.sh` recompiles them in-tree automatically (no image ref to bump)
- Local dev iterating on the build-tools image: `make build-tools && ./scripts/build-{vz,firecracker}-rootfs.sh --build-tools-version dev`
- See `docs/reference/images.md`, `docs/reference/build-tools.md`, and `docs/upgrades/v0.5.1-to-v0.5.2.md` for full docs.

## Storage Model

Images are stored content-addressed under `{images_dir}/blobs/sha256/<digest>` (flat files, one per blob — manifests, configs, layer tar.gz, kernel, initrd, rootfs erofs). Since v0.6.0 an image's identity is its **Docker ref** (the `io.shed.source-ref` annotation value), resolved O(1) through a ref-index at `{images_dir}/refs/<sha256(ref)>.json` (ref → manifest digest, written as the final commit of every pull/build). `shed create` resolves the configured ref (`default_image`, or `--image <alias|ref|/abs/path|label>`) through this index per `pull_policy` (missing|always|never). Tags (`{images_dir}/tags/<tag>.json`) are now optional cosmetic labels, decoupled from resolution; the internal `_base` tag was removed. Each shed and snapshot pins the manifest digest in metadata; `shed image prune` walks reachability (sheds, snapshots, every tag, and the configured `default_image`/`image_aliases` digests are protective). See `docs/reference/storage-model.md` for the full layout and lifecycle commands (`shed image ls/inspect/tag/pull/rm/prune`).

## Documentation

Docs use [Zensical](https://zensical.org) with the shared
[stridelabs-docs-theme](https://github.com/charliek/stridelabs-docs-theme) package
(pinned by tag in `pyproject.toml`; palette/fonts/features live there, not here).
See `zensical.toml` for style guidelines (top comment block). Key rules:
- Professional, direct tone
- Tables for CLI options and config fields
- Code blocks with language hints
- Examples should be copy-pasteable
- One topic per page

```bash
make docs-serve     # Serve docs at http://127.0.0.1:7070
```
