# CLAUDE.md

Project context for AI assistants working on this codebase.

## Project Overview

Shed is a CLI tool for managing persistent, VM-based development environments across multiple servers. It supports Firecracker microVMs (Linux) and Apple VZ virtual machines (macOS).

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

#### Server-side changes — required e2e validation

`make test-integration` runs against whatever `shed-server` binary is currently **installed** on the host, not the source tree you're editing. A server-side-only change (orchestrator, lifecycle internals, backend handlers with no CLI-visible signature change) can pass `make test-integration` without ever executing the new code path — the brew/deb binary is still the OLD one. This gap masked real coverage on PRs #151-156; see `docs/discovery/integration-suite-server-coverage.md` for the full motivation.

**Any server-side change MUST be validated against the developer's own source tree before opening a PR.** Two one-command targets automate the build + binary-swap + suite + restore cycle:

```bash
# VZ on local Mac (brew shed-server). macOS only.
make test-integration-local

# FC on remote Linux (default $SHED_FC_HOST=mini3) over SSH. Linux deb shed-server.
make test-integration-local-fc
```

Both targets build the dev binary, swap it in (codesigned on Mac; systemd drop-in on Linux), set `SHED_BUILD_TOOLS_REF` to the latest release tag so the dev binary uses release-shaped behavior, run the full suite, and restore the host's installed binary **regardless of suite outcome**. The chained targets capture both the suite and the restore exit codes so a restore failure is surfaced separately from a suite failure. Backups live at `/tmp/shed-server-v<VERSION>.bak` (Mac) and `/tmp/shed-server-deb.bak` (remote); the install targets refuse to clobber an existing backup without `FORCE=1`.

**Open a server-side PR with `make test-integration-local: N/N pass against dev-build at commit <sha>`** (or its `-fc` sibling), and that statement is true and meaningful — not a brew-binary alibi.

#### Performance impact — vet against the released version

For changes that touch the boot path, agent dial, healthPoll, upper-allocation, mount, image-resolution, or any other hot path: **measure the impact on each platform the change affects, against the most recent release binary, before merging.** The split timing gate (`test_create_agent_p50` + `test_create_rootfs_template_present`) is the floor — it'll fire on regressions around 500 ms or more (see `tests/integration/fixtures/server.py:DEFAULT_AGENT_P50_MS` for the per-backend regression budget) — but a sub-threshold regression (or worse, a "no regression" that masks an actual gain that didn't materialise) won't trip it.

The workflow:

1. Run the suite against the **release** binary (the bare `make test-integration` — it picks up the brew/deb install). Record the agent_p50 + total wall-clock from the PhaseTimer line for each backend you're changing.
2. Swap in the dev binary (`make install-local-server` / `make install-remote-server`) and re-run. Same measurements.
3. Compare. The dev binary on `SHED_BUILD_TOOLS_REF=<latest-tag>` should be the apples-to-apples comparison.
4. Repeat on every backend the change affects. **A change shipping for both VZ and FC needs both backends measured** — Apple Silicon vfkit and Linux KVM Firecracker have different floors and the same code can be faster on one and slower on the other.
5. Record the measurements in the PR body. Hypothesised gains that don't show up are worth investigating before merge.

This is what kept the v0.5.4 build-tools-ref regression hidden (caught by a user noticing slow creates after `brew upgrade`, not by the suite). The dynamic timing gate complements the unit tests; the per-platform measurement is the only safety net for changes whose value-add IS the timing characteristic.

#### Environment overrides (full list in `tests/integration/README.md`)

| Variable | Default | Effect |
|---|---|---|
| `SHED_VZ_SERVER` | `my-server` | `~/.shed/config.yaml` entry for the local VZ server. Also honored by `make install-local-server` / `make test-integration-local`. |
| `SHED_VZ_LOG_PATH` | `/opt/homebrew/var/log/shed-server.log` | brew log path (override for Intel-Mac homebrew prefix or custom installs) |
| `SHED_FC_HOST` | `mini3` | SSH host for FC live tests + `make install-remote-server` / `make test-integration-local-fc`. |
| `SHED_FC_SERVER` | same as `$SHED_FC_HOST` | `~/.shed/config.yaml` entry name when it differs from the SSH host |
| `FC_REMOTE_BIN_PATH` | `/usr/local/bin/shed-server` | shed-server install path on the FC remote (override if the deb's install location moves) |
| `RELEASE_BUILD_TOOLS_REF` | latest `git tag` matching `v*` | shed-build-tools image ref injected into the dev binary so it uses release-shaped behavior. Pin to an older release if your source has drifted: `RELEASE_BUILD_TOOLS_REF=ghcr.io/charliek/shed-build-tools:v0.5.7` |

See `docs/development/testing.md` (Development → Testing on the docs site) for the full operator guide — adding a test, the per-backend timing ceilings, the fixture conventions, the FC remote upgrade procedure for first-time setup, etc.

## Project Structure

- `cmd/shed/` — CLI binary (command handlers split across `shed.go`, `console.go`, `attach.go`, `sessions.go`, `tunnels.go`, `sync.go`, `ssh_config.go`, etc.)
- `cmd/shed-server/` — Server daemon binary
- `cmd/shed-agent/` — In-VM agent binary (Firecracker/VZ)
- `internal/api/` — HTTP API handlers (Chi router)
- `internal/config/` — Configuration types and validation
- `internal/firecracker/` — Firecracker backend (Linux only, `//go:build linux`)
- `internal/vz/` — VZ backend (macOS only, `//go:build darwin`)
- `internal/vmutil/` — Shared VM agent communication (no build tags)
- `internal/agentproto/` — Binary protocol for vsock communication
- `internal/sshd/` — SSH server implementation
- `internal/backend/` — Backend interface all backends implement
- `docs/` — MkDocs documentation site

## Key Conventions

- **Go version**: 1.24+ (see `go.mod`)
- **Formatting**: `gofmt` — run `make fmt` before committing
- **Linting**: `golangci-lint` — run `make lint`
- **Tests**: Table-driven tests with `t.Run()`. Place `_test.go` files alongside source.
- **Build tags**: `linux` for Firecracker code, `darwin` for VZ code, `e2e` for Firecracker VM tests (requires KVM)
- **Config types**: All in `internal/config/types.go`
- **Workspace path**: `/workspace` inside VMs (see `config.WorkspacePath`)
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
| VZ | macOS (Apple Silicon) | VM via vfkit | Rootfs ext4 or VirtioFS (`--local-dir`) |

The server config `default_backend` supports `vz`, `firecracker`, or `detect` (auto-selects based on platform).

## VM Images

VZ and Firecracker use multi-stage Dockerfiles (`vz/Dockerfile`, `firecracker/Dockerfile`) to build rootfs OCI images. Variants: `base`, `extensions`, `full`. The `extensions` and `full` variants layer [shed-extensions](https://github.com/charliek/shed-extensions) guest binaries via `COPY --from=` a published multi-arch Docker image, pinned by `ARG SHED_EXT_VERSION`.

Since v0.5.2 the read-only rootfs **erofs is built at image-publish time** by `mkfs.erofs` running inside the `ghcr.io/charliek/shed-build-tools:vX.Y.Z` container (pinned `erofs-utils` version, tagged in lockstep with shed releases). The resulting erofs ships as a content-addressed OCI blob referenced by the `io.shed.rootfs.erofs.digest` manifest annotation. Hosts pull the blob and mount it directly — no on-host `mkfs.erofs` invocation. Pre-v0.5.2 images lack the annotation and are rejected at boot with a clear "rebuild against current tooling" error (see `internal/vmimage/manager.go:resolveManifestLower`).

- Build locally: `./scripts/build-vz-rootfs.sh --variant extensions`
- Local dev with shed-extensions changes: `--shed-ext-version dev` (after building the shed-extensions Docker image locally)
- Local dev iterating on the build-tools image: `make build-tools && ./scripts/build-{vz,firecracker}-rootfs.sh --build-tools-version dev`
- See `docs/reference/images.md`, `docs/reference/build-tools.md`, and `docs/upgrades/v0.5.1-to-v0.5.2.md` for full docs.

## Storage Model

Images are stored content-addressed under `{images_dir}/blobs/sha256/<digest>` (flat files, one per blob — manifests, configs, layer tar.gz, kernel, initrd, rootfs erofs). Tags live at `{images_dir}/tags/<tag>.json`. Each shed and snapshot pins the manifest digest in metadata so `shed image prune` can refcount-GC unreferenced blobs. Tags do NOT protect blobs from prune (Docker model). See `docs/reference/storage-model.md` for the full layout and the lifecycle commands (`shed image ls/inspect/tag/pull/rm/prune`).

## Documentation

Docs use MkDocs Material. See `mkdocs.yml` for style guidelines (top comment block). Key rules:
- Professional, direct tone
- Tables for CLI options and config fields
- Code blocks with language hints
- Examples should be copy-pasteable
- One topic per page

```bash
make docs-serve     # Serve docs at http://127.0.0.1:7070
```
