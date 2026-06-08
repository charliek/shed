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

## Quick start

Follow the packaged quickstart for your platform:

- **[macOS Quickstart](https://charliek.github.io/shed/getting-started/macos-quickstart/)** — Homebrew + the shed-desktop approval app.
- **[Linux Quickstart](https://charliek.github.io/shed/getting-started/linux-quickstart/)** — the `shed-server` deb running Firecracker.

Building from source or customizing images? See
[Developer Setup](https://charliek.github.io/shed/getting-started/quick-start/).

The CLI also ships a coding-agent skill — `npx skills add charliek/shed` (or, for
Claude Code, `/plugin marketplace add charliek/shed` then `/plugin install shed@shed`).

## Architecture

Three binaries:

- **`shed`** — the CLI on your developer machine.
- **`shed-server`** — the daemon on each host (HTTP API + SSH server) that runs VMs via the VZ (macOS) or Firecracker (Linux) backend.
- **`shed-host-agent`** — optional host-side credential broker, from [shed-extensions](https://github.com/charliek/shed-extensions); on macOS it pairs with the [shed-desktop](https://github.com/charliek/shed-desktop) approval app.

```
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
- [Images](https://charliek.github.io/shed/reference/images/) · [Storage Model](https://charliek.github.io/shed/reference/storage-model/)
- [Extensions](https://charliek.github.io/shed/reference/extensions/) — credential brokering
- [Provisioning](https://charliek.github.io/shed/reference/provisioning/) — `.shed/` install/startup hooks + tutorials
- Related projects: [shed-extensions](https://charliek.github.io/shed-extensions/) · [shed-desktop](https://charliek.github.io/shed-desktop/)

## Requirements

- **Client**: macOS or Linux (the `shed` CLI).
- **Server (VZ)**: macOS 13+ on Apple Silicon (arm64); macOS 14+ for shed-desktop.
- **Server (Firecracker)**: Linux with KVM.
- **Network**: Tailscale (or any private network) connecting the machines.

## Security model

Shed targets single-user setups where every machine is on a private network
(e.g. Tailscale), the developer controls all of them, and network access implies
trust. Workloads run as a non-root `shed` user (UID 1000) with passwordless sudo.
It is **not** built for multi-tenant use, public-internet exposure, or untrusted
networks.

## Development

See [Development Setup](docs/development/setup.md) for building from source and
contributing.

## License

MIT
