# VZ Image Variants

Shed provides multiple rootfs image variants for the VZ backend. Each variant includes the core shed infrastructure (systemd, SSH, Docker, shed-agent) but differs in the development tools installed.

## Available Variants

| Variant | Description | Coding Agents | Language Runtimes |
|---------|-------------|---------------|-------------------|
| `base` | Minimal. Core tools only. | None | None |
| `devtools` | Foundation layer with version manager and runtimes. | Claude Code | Node.js (LTS), Python 3.13 |
| `default` | Full experience. All tools and agents. | Claude Code, OpenCode, Cursor CLI, Codex CLI | Node.js (LTS), Python 3.13 |
| `typescript` | TypeScript focused. Node.js + Claude Code. | Claude Code | Node.js (LTS), Python 3.13 |

All variants include: systemd, SSH, Docker CE, git, gh, curl, wget, vim, neovim, tmux, htop, jq, ripgrep, tree, build-essential, and the shed-agent.

Both `default` and `typescript` inherit from `devtools`, which inherits from `base`. All variants share the same kernel and core system.

## Published Images

Pre-built images are published to `ghcr.io/charliek/` on each release:

| Image | Tag Format |
|-------|-----------|
| `ghcr.io/charliek/shed-vz-base` | `:{version}`, `:latest` |
| `ghcr.io/charliek/shed-vz-devtools` | `:{version}`, `:latest` |
| `ghcr.io/charliek/shed-vz-default` | `:{version}`, `:latest` |
| `ghcr.io/charliek/shed-vz-typescript` | `:{version}`, `:latest` |

These images serve two purposes:

1. **Direct use**: Reference them in server config as Docker refs — shed auto-pulls and converts to ext4 on first use.
2. **Base for custom images**: Use `FROM ghcr.io/charliek/shed-vz-base:latest` in your own Dockerfile.

## Server Configuration

### Using published images (recommended)

Point your config at Docker image references. Shed pulls and converts to ext4 automatically on first `shed create`:

```yaml
vz:
  base_rootfs: ghcr.io/charliek/shed-vz-default:v1.0.0
  images:
    base: ghcr.io/charliek/shed-vz-base:v1.0.0
    default: ghcr.io/charliek/shed-vz-default:v1.0.0
    typescript: ghcr.io/charliek/shed-vz-typescript:v1.0.0
  images_dir: ~/Library/Application Support/shed/vz/
```

### Using local images

If you build images locally, point to ext4 file paths:

```yaml
vz:
  base_rootfs: ~/Library/Application Support/shed/vz/default-rootfs.ext4
  images:
    base: ~/Library/Application Support/shed/vz/base-rootfs.ext4
    default: ~/Library/Application Support/shed/vz/default-rootfs.ext4
    typescript: ~/Library/Application Support/shed/vz/typescript-rootfs.ext4
```

You can mix Docker refs and local paths in the same config.

The `base_rootfs` field is used when no `--image` flag is specified. The `images` map enables per-shed variant selection via `--image`. The `images_dir` directory is scanned for auto-discovered images matching `{name}-rootfs.ext4`.

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

List available images:

```bash
shed image list
```

## Creating Custom Images

### From a Dockerfile

Create a Dockerfile that extends a published shed base image:

```dockerfile
FROM ghcr.io/charliek/shed-vz-base:latest

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

Build and convert to ext4:

```bash
shed image build -f Dockerfile.shed -n rust
```

The image is immediately available:

```bash
shed create myproject --image rust
```

### From a Docker registry

Convert an existing Docker image directly:

```bash
shed image build --from registry.company.com/shed-custom:latest -n custom
```

### From source (contributing to this repo)

Add a new stage to `vz/Dockerfile` that inherits from `shed-vz-base`, then build with the build script:

```bash
./scripts/build-vz-rootfs.sh --variant rust
```

## Organization Images

Organizations can distribute custom shed images to their teams.

### Option A: Dockerfile repo

Maintain a repo with a Dockerfile that extends a published base:

```
shed-acmeco/
  Dockerfile.shed     # FROM ghcr.io/charliek/shed-vz-base:v1.0.0
```

Developers clone the repo and run:

```bash
shed image build -f Dockerfile.shed -n acmeco
```

### Option B: Company Docker registry

1. Build and push the custom image in CI:
   ```bash
   docker buildx build --platform linux/arm64 -t registry.company.com/shed-acmeco:latest --push .
   ```

2. Developers convert it locally:
   ```bash
   shed image build --from registry.company.com/shed-acmeco:latest -n acmeco
   ```

3. Or configure it in server config for auto-pull:
   ```yaml
   vz:
     images:
       acmeco: registry.company.com/shed-acmeco:latest
   ```

## Image Caching

Converted ext4 images are cached in `images_dir` (default: `~/Library/Application Support/shed/vz/`). A `.source` sidecar file tracks which Docker ref produced each image.

When the Docker ref in your config changes (e.g., updating from `v1.0.0` to `v1.1.0`), shed detects the mismatch and re-converts automatically on the next `shed create`.

## Requirements

Image conversion requires Docker with privileged container support. The ext4 creation step uses a privileged Docker container for loop mounting.

## Disk Space

Each variant produces a 20GB sparse ext4 image. Actual disk usage is much smaller (typically 2-5GB depending on the variant). Use `du -sh` to check actual usage:

```bash
du -sh ~/Library/Application\ Support/shed/vz/*-rootfs.ext4
```

## Building from Source

Build the default variant locally:

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

Output files are placed in `~/Library/Application Support/shed/vz/`:

| File | Description |
|------|-------------|
| `default-rootfs.ext4` | Default variant rootfs (20GB sparse) |
| `base-rootfs.ext4` | Base variant rootfs |
| `typescript-rootfs.ext4` | TypeScript variant rootfs |
| `vmlinux` | Decompressed Linux kernel (shared) |
| `initrd.img` | Initial RAM disk (shared) |
