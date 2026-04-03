# CLAUDE.md

Project context for AI assistants working on this codebase.

## Project Overview

Shed is a CLI tool for managing persistent, containerized development environments across multiple servers. It supports Docker containers, Firecracker microVMs (Linux), and Apple VZ virtual machines (macOS).

## Build & Test

```bash
make build          # Build all binaries (shed, shed-server, shed-agent) into bin/
make test           # Run all unit tests
make lint           # Run golangci-lint
make fmt            # Format code with gofmt
make check          # Run test + lint + fmt
make test-integration  # Integration tests (requires Docker)
make coverage       # Tests with coverage report
```

Tools are managed via [mise](https://mise.jdx.dev/) — run `mise install` to set up Go and golangci-lint.

## Project Structure

- `cmd/shed/` — CLI binary (command handlers split across `shed.go`, `console.go`, `attach.go`, `sessions.go`, `tunnels.go`, `sync.go`, `ssh_config.go`, etc.)
- `cmd/shed-server/` — Server daemon binary
- `cmd/shed-agent/` — In-VM agent binary (Firecracker/VZ)
- `internal/api/` — HTTP API handlers (Chi router)
- `internal/config/` — Configuration types and validation
- `internal/docker/` — Docker backend
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
- **Build tags**: `linux` for Firecracker code, `darwin` for VZ code, `integration` for Docker tests, `e2e` for Firecracker VM tests
- **Config types**: All in `internal/config/types.go`
- **Docker labels**: All `shed.*` prefixed — see `config.Label*` constants
- **Container naming**: `shed-{name}` prefix (see `config.ContainerName()`)
- **Volume naming**: `shed-{name}-workspace` (see `config.VolumeName()`)
- **Workspace path**: `/workspace` inside containers/VMs (see `config.WorkspacePath`)
- **Container user**: `shed` (UID 1000) with passwordless sudo

## Backends

| Backend | Platform | Isolation | Workspace |
|---------|----------|-----------|-----------|
| Docker | Linux | Container | Named volume or bind mount (`--local-dir`) |
| Firecracker | Linux (KVM) | microVM | Rootfs ext4 image |
| VZ | macOS (Apple Silicon) | VM via vfkit | Rootfs ext4 or VirtioFS (`--local-dir`) |

## VM Images

VZ and Firecracker use multi-stage Dockerfiles (`vz/Dockerfile`, `firecracker/Dockerfile`) to build rootfs ext4 images. Variants: `base`, `default`, `experimental`. The `experimental` variant layers [shed-extensions](https://github.com/charliek/shed-extensions) guest binaries via `COPY --from=` a published multi-arch Docker image, pinned by `ARG SHED_EXT_VERSION`.

- Build locally: `./scripts/build-vz-rootfs.sh --variant experimental`
- Local dev with shed-extensions changes: `--shed-ext-version dev` (after building the shed-extensions Docker image locally)
- See `docs/reference/images.md` for full variant docs and build instructions

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
