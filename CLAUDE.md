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

# Run the suite (auto-installs Python deps into a managed venv on first run)
make test-integration
```

The suite is **parameterized over `["vz", "fc"]`** and skips cleanly when a backend is unreachable from this host. On a Mac with the brew-installed `shed-server`, VZ tests run against `shed -s my-server`; FC tests target the entry named by `$SHED_FC_HOST` (default `mini3`) over SSH.

**FC live tests require the remote `shed-server` to emit `PhaseTimer` log lines** (added in v0.5.4 via PR #118). Two tests (`test_phase_timer_emitted[fc]`, `test_plain_create_timing[fc]`) skip cleanly with a clear message if the remote is older — the other three FC tests work against any shed-server version. Once the remote upgrades to v0.5.4+, the suite picks up the FC tests automatically with no config change.

Environment overrides (full list in `tests/integration/README.md`):

| Variable | Default | Effect |
|---|---|---|
| `SHED_VZ_SERVER` | `my-server` | `~/.shed/config.yaml` entry for the local VZ server |
| `SHED_VZ_LOG_PATH` | `/opt/homebrew/var/log/shed-server.log` | brew log path (override for Intel-Mac homebrew prefix or custom installs) |
| `SHED_FC_HOST` | `mini3` | SSH host for FC live tests |
| `SHED_FC_SERVER` | same as host | `~/.shed/config.yaml` entry name when it differs from the SSH host |

See `docs/development/testing.md` (Development → Testing on the docs site) for the full operator guide — adding a test, the per-backend timing ceilings, the fixture conventions, the mini3 upgrade procedure for FC e2e, etc.

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

## `shed exec` semantics

`shed exec` runs `argv[0]` with `argv[1:]` directly inside the shed — like `docker exec` / `kubectl exec`. There is **no implicit shell wrapping**, so pipes, redirects, semicolons, `$VAR`, command substitution, etc. only fire when the user explicitly invokes a shell. The CLI single-quotes each argv element before handing to `ssh` so the SSH server's `shlex.Split` recovers the original argv intact.

Implications when writing code or docs:

- `shed exec <name> -- mytool` is direct-exec; tools must already be on the agent's `PATH` (`/etc/environment.d/` defaults).
- Anything needing shell features goes through `bash -c '…'`: `shed exec <name> -- bash -c 'a | b > c'`.
- Login-shell init (`/etc/profile.d`, `~/.profile`) is opt-in via `bash -lc '…'`.
- Provisioning hooks are a separate path (`internal/vmutil/provisioning.go`) — they still run as `bash --login -c` and source profile scripts.
- The legacy idiom `shed exec name "cmd | with | pipes"` no longer works; rewrite as `shed exec name -- bash -c 'cmd | with | pipes'`.

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
