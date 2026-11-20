# Shed

Shed manages persistent, VM-based development environments across multiple
servers. Spin up isolated coding sessions (with Claude Code / OpenCode
pre-installed), disconnect, and reconnect later to continue — backed by
Firecracker microVMs on Linux or Apple Virtualization VMs on macOS Apple Silicon.

**📖 Full documentation: <https://charliek.github.io/shed/>**

## Features

- **Simple CLI** — create and manage dev environments with minimal commands.
- **Session persistence** — VMs (and your tmux sessions) keep running after you disconnect.
- **Multi-server** — manage sheds across home servers and cloud VPS instances.
- **IDE integration** — native Cursor / VS Code Remote-SSH support.
- **AI-ready** — pre-configured for Claude Code and OpenCode workflows.
- **VM backends** — Firecracker microVMs (Linux) or Apple VZ (macOS Apple Silicon).
- **Hardened sheds** — `--mounts none` keeps host directories out of a shed's mounts, and on
  Firecracker `--network isolated` lets it reach the internet and nothing else, enforced by
  nftables on the host. Use both: on Firecracker an `open` shed with no mounts can still reach
  another running shed's 9P export on the bridge.
- **Orchestrator API** — mTLS service clients that create, run commands in
  (`POST /api/sheds/{name}/exec`) and delete only their own sheds, within a policy.

## Quick start

Follow the packaged quickstart for your platform:

- **[macOS Quickstart](https://charliek.github.io/shed/getting-started/macos-quickstart/)** — Homebrew + the shed-desktop approval app.
- **[Linux Quickstart](https://charliek.github.io/shed/getting-started/linux-quickstart/)** — the `shed-server` deb running Firecracker.

Building from source or customizing images? See
[Developer Setup](https://charliek.github.io/shed/getting-started/quick-start/).

The CLI also ships a coding-agent skill — `npx skills add charliek/shed` (or, for
Claude Code, `/plugin marketplace add charliek/shed` then `/plugin install shed@shed`).

## Architecture

Two binaries and a desktop app:

- **`shed`** — the CLI on your developer machine.
- **`shed-server`** — the daemon on each host (HTTP API + SSH server) that runs VMs via the VZ (macOS) or Firecracker (Linux) backend.
- **shed desktop app** — the credential broker on your machine (macOS and Linux), with the approval prompts: the [shed-desktop](https://charliek.github.io/shed/desktop/) app, in this monorepo under `desktop/` with its shared Rust client core under `crates/`. It ships separately from these binaries.

```text
Developer Machine                Remote Server / Local Mac
┌─────────────┐   HTTP/SSH    ┌──────────────────────────────┐
│  shed CLI   │ ────────────▶ │  shed-server                 │
└─────────────┘               │   ├── HTTP API (lifecycle)   │
                              │   └── SSH server (shell/IDE) │
                              │        VZ │ Firecracker VMs  │
                              └──────────────────────────────┘
```

Both backends boot from layered OCI images (`base` / `extensions` / `full`)
pulled registry-direct from `ghcr.io`. See
[Images](https://charliek.github.io/shed/reference/images/).

## Documentation

- [Getting Started](https://charliek.github.io/shed/getting-started/quick-start/) — quickstarts + developer setup
- [CLI Reference](https://charliek.github.io/shed/reference/cli/) — every command
- [Configuration](https://charliek.github.io/shed/reference/configuration/) — client + server config
- [Security Configuration](https://charliek.github.io/shed/guides/security-configuration/) — choosing a server posture and hardening sheds
- [Security](https://charliek.github.io/shed/reference/security/) — the model: postures, service clients, mount selection, network profiles
- [Orchestrated Sheds](https://charliek.github.io/shed/guides/orchestrated-sheds/) — driving sheds from CI or an agent runner
- [Images](https://charliek.github.io/shed/reference/images/) · [Storage Model](https://charliek.github.io/shed/reference/storage-model/)
- [Extensions](https://charliek.github.io/shed/reference/extensions/) — credential brokering
- [Provisioning](https://charliek.github.io/shed/reference/provisioning/) — `.shed/` install/startup hooks + tutorials
- [Desktop app](https://charliek.github.io/shed/desktop/) — the Tauri desktop app for macOS (menu bar) and Linux (`desktop/`, on the shared Rust core in `crates/`)

## Requirements

- **Client**: macOS or Linux (the `shed` CLI).
- **Server (VZ)**: macOS 13+ on Apple Silicon (arm64); macOS 14+ for shed-desktop.
- **Server (Firecracker)**: Linux with KVM.
- **Network**: Tailscale (or any private network) connecting the machines.

## Security model

Shed is local-development-first: out of the box `bind_address` defaults to
loopback (`127.0.0.1`), so the server is reachable only on the machine it runs
on. Facing the network is opt-in and takes **mtls mode** (pinned TLS + a
client certificate enrolled over SSH + an SSH key allowlist); it works
locally too. Open mode (plain HTTP, no credential) is loopback only. Workloads
run as a non-root `shed` user (UID 1000) with passwordless sudo. In mtls mode
a server can have [named users](https://charliek.github.io/shed/guides/multi-user-server/),
each reaching only their own sheds; that separates people who trust the host
and its admins, and shed is **not** built for multi-tenant use by parties that
do not. In mtls mode an orchestrator can be a
[service client](https://charliek.github.io/shed/reference/security/#service-clients) with a
policy, which narrows what it can reach through the API; that is a narrow identity, not tenant
isolation (Firecracker runs without its jailer, for one). Each shed can also be hardened on its
own, with fewer mounts and, on Firecracker, the `isolated` network.

## Development

See [Development Setup](docs/development/setup.md) for building from source and
contributing.

## License

MIT
