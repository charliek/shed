# VZ Image Variants

Shed provides multiple rootfs image variants for the VZ backend. Each variant includes the core shed infrastructure (systemd, SSH, Docker, shed-agent) but differs in the development tools installed.

## Available Variants

| Variant | Description | Coding Agents | Language Runtimes |
|---------|-------------|---------------|-------------------|
| `base` | Minimal. Core tools only. | None | None |
| `default` | Full experience. All tools and agents. | Claude Code, OpenCode, Cursor CLI, Codex CLI | Node.js (LTS), Python 3.13 |
| `typescript` | TypeScript focused. Node.js + Claude Code. | Claude Code | Node.js (LTS), Python 3.13 |

All variants include: systemd, SSH, Docker CE, git, gh, curl, wget, vim, neovim, tmux, htop, jq, ripgrep, tree, build-essential, and the shed-agent.

Both `default` and `typescript` inherit from `base`, so they share the same kernel and core system.

## Building Images

Build the default variant (same as today's image):

```bash
./scripts/build-vz-rootfs.sh
```

Build a specific variant:

```bash
./scripts/build-vz-rootfs.sh --variant base
./scripts/build-vz-rootfs.sh --variant typescript
```

Build all variants:

```bash
./scripts/build-vz-rootfs.sh --all
```

Force kernel/initrd re-extraction (normally skipped if files already exist):

```bash
./scripts/build-vz-rootfs.sh --variant base --force-kernel
```

Custom output directory:

```bash
OUTPUT_DIR=/path/to/output ./scripts/build-vz-rootfs.sh --variant base
```

Output files are placed in `~/Library/Application Support/shed/vz/`:

| File | Description |
|------|-------------|
| `default-rootfs.ext4` | Default variant rootfs (20GB sparse) |
| `base-rootfs.ext4` | Base variant rootfs |
| `typescript-rootfs.ext4` | TypeScript variant rootfs |
| `vmlinux` | Decompressed Linux kernel (shared) |
| `initrd.img` | Initial RAM disk (shared) |

## Server Configuration

Configure available variants in your server config:

```yaml
vz:
  base_rootfs: ~/Library/Application Support/shed/vz/default-rootfs.ext4
  images:
    base: ~/Library/Application Support/shed/vz/base-rootfs.ext4
    default: ~/Library/Application Support/shed/vz/default-rootfs.ext4
    typescript: ~/Library/Application Support/shed/vz/typescript-rootfs.ext4
```

The `base_rootfs` field is used when no `--image` flag is specified. The `images` map enables per-shed variant selection via `--image`.

## Using Variants

Create a shed with a specific variant:

```bash
shed create myproject --image typescript
shed create tools --image base
```

Create a shed with the default variant (no flag needed):

```bash
shed create myproject
```

## Creating Custom Variants

Add a new stage to `vz/Dockerfile` that inherits from `shed-vz-base`:

```dockerfile
# =============================================================================
# Stage: shed-vz-rust
# Rust development environment.
# =============================================================================
FROM shed-vz-base AS shed-vz-rust

USER shed

ENV PATH="/home/shed/.local/bin:${PATH}"

# Rust via rustup
RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y
ENV PATH="/home/shed/.cargo/bin:${PATH}"

# Claude Code
RUN curl -fsSL https://claude.ai/install.sh | bash

USER root

WORKDIR /workspace

ENTRYPOINT ["/sbin/init"]
```

Then build it:

```bash
./scripts/build-vz-rootfs.sh --variant rust
```

And add it to your server config:

```yaml
vz:
  images:
    rust: ~/Library/Application Support/shed/vz/rust-rootfs.ext4
```

## Disk Space

Each variant produces a 20GB sparse ext4 image. Actual disk usage is much smaller (typically 2-5GB depending on the variant). Use `du -sh` to check actual usage:

```bash
du -sh ~/Library/Application\ Support/shed/vz/*-rootfs.ext4
```
