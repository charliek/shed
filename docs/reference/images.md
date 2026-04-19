# Image Variants

Shed provides multiple rootfs image variants for the VZ and Firecracker backends. Each variant includes the core shed infrastructure (systemd, SSH, Docker CE, shed-agent) but differs in the development tools installed.

## Available Variants

| Variant | Description | Coding Agents | Language Runtimes |
|---------|-------------|---------------|-------------------|
| `base` | Minimal. Core tools only. | None | None |
| `devtools` | Foundation layer with version manager and runtimes. | Claude Code | Node.js (LTS), Python 3.13 |
| `default` | Full experience. All tools and agents. | Claude Code, OpenCode, Codex CLI, Cursor CLI (VZ only) | Node.js (LTS), Python 3.13 |
| `experimental` | Default + [shed-extensions](https://charliek.github.io/shed-extensions/) credential brokering. | Claude Code, OpenCode, Codex CLI, Cursor CLI (VZ only) | Node.js (LTS), Python 3.13 |

All variants include: systemd, SSH, Docker CE, git, gh, curl, wget, vim, neovim, tmux, htop, jq, ripgrep, tree, build-essential, and the shed-agent.

`default` inherits from `devtools`, which inherits from `base`. `experimental` inherits from `default`. All variants share the same kernel and core system.

### Experimental Variant

The `experimental` variant adds [shed-extensions](https://charliek.github.io/shed-extensions/) credential brokering on top of `default`. It includes:

- **`shed-ssh-agent`** — SSH agent proxy that forwards key operations to your Mac (private keys never enter the VM)
- **`shed-aws-proxy`** — AWS credential proxy that vends short-lived STS tokens via the host
- **`docker-credential-shed`** — Docker credential helper that delegates registry authentication to the host via the message bus. Guest Docker is pre-configured with `{"credsStore": "shed"}` so `docker pull` from private registries works without storing credentials in the VM.
- **`shed-ext`** — CLI for checking extension connectivity and health
- Pre-configured `SSH_AUTH_SOCK` and `AWS_CONTAINER_CREDENTIALS_FULL_URI` environment variables

**When to use:** You want SSH agent forwarding and/or AWS credential proxying without long-lived credentials entering the VM.

**Prerequisite:** The `shed-host-agent` binary must be running on your host machine. See the [shed-extensions quick start](https://charliek.github.io/shed-extensions/getting-started/quick-start/) for setup.

```bash
shed create mydev --image experimental
```

For local development on shed-extensions itself, use the `--shed-ext-version` flag when building images:

```bash
./scripts/build-vz-rootfs.sh --variant experimental --shed-ext-version dev
```

## Published Images

Pre-built `base` images are published to `ghcr.io/charliek/` on each release:

| Image | Platform | Tag Format |
|-------|----------|-----------|
| `ghcr.io/charliek/shed-vz-base` | linux/arm64 (Apple Silicon) | `:{version}` |
| `ghcr.io/charliek/shed-fc-base` | linux/amd64 (x86_64) | `:{version}` |

The `experimental` variant is also published:

| Image | Platform | Tag Format |
|-------|----------|-----------|
| `ghcr.io/charliek/shed-vz-experimental` | linux/arm64 (Apple Silicon) | `:{version}` |
| `ghcr.io/charliek/shed-fc-experimental` | linux/amd64 (x86_64) | `:{version}` |

Additional variants (`default`) can be built locally from source.

Both VZ and Firecracker images include the kernel needed to boot the VM. For VZ, the kernel and initrd are extracted from the Ubuntu `linux-image-generic` package. For Firecracker, a custom kernel is compiled with Docker, 9P, and BPF support built in. No separate kernel build or download is needed when using published images.

These images serve two purposes:

1. **Direct use**: Reference them in server config as Docker refs — shed auto-pulls and converts to ext4 on first use.
2. **Base for custom images**: Use `FROM ghcr.io/charliek/shed-vz-base:{version}` in your own Dockerfile.

Replace `{version}` with the version matching your `shed` binary — run `shed version` to check.

To pre-cache images before the first `shed create`:

```bash
sudo shed-server pull-images
```

This pulls all configured image variants and converts them to ext4, so the first shed creation is fast.

## Server Configuration

### Using published images (recommended)

Point your config at Docker image references. Shed pulls and converts to ext4 automatically on first `shed create`:

=== "VZ (macOS)"

    ```yaml
    vz:
      base_rootfs: ghcr.io/charliek/shed-vz-base:{version}
      images:
        base: ghcr.io/charliek/shed-vz-base:{version}
        experimental: ghcr.io/charliek/shed-vz-experimental:{version}
      images_dir: ~/Library/Application Support/shed/vz/
    ```

=== "Firecracker (Linux)"

    ```yaml
    firecracker:
      base_rootfs: ghcr.io/charliek/shed-fc-base:{version}
      images:
        base: ghcr.io/charliek/shed-fc-base:{version}
        experimental: ghcr.io/charliek/shed-fc-experimental:{version}
      images_dir: /var/lib/shed/firecracker/images
    ```

### Using local images

If you build images locally, point to ext4 file paths:

=== "VZ (macOS)"

    ```yaml
    vz:
      base_rootfs: ~/Library/Application Support/shed/vz/default-rootfs.ext4
      images:
        base: ~/Library/Application Support/shed/vz/base-rootfs.ext4
        default: ~/Library/Application Support/shed/vz/default-rootfs.ext4
        experimental: ~/Library/Application Support/shed/vz/experimental-rootfs.ext4
    ```

=== "Firecracker (Linux)"

    ```yaml
    firecracker:
      base_rootfs: /var/lib/shed/firecracker/images/default-rootfs.ext4
      images:
        base: /var/lib/shed/firecracker/images/base-rootfs.ext4
        default: /var/lib/shed/firecracker/images/default-rootfs.ext4
        experimental: /var/lib/shed/firecracker/images/experimental-rootfs.ext4
    ```

You can mix Docker refs and local paths in the same config.

The `base_rootfs` field is used when no `--image` flag is specified. The `images` map enables per-shed variant selection via `--image`. The `images_dir` directory is scanned for auto-discovered images matching `{name}-rootfs.ext4`.

### `base_rootfs` vs `images:` — how they interact

The two fields play different roles:

- `images:` is a map of named variants. Each entry is cacheable, pre-pullable via `shed-server pull-images`, visible in `shed image list`, and selectable with `shed create --image <name>`.
- `base_rootfs` is a single top-level fallback used when `shed create` is invoked without `--image`. It is stored on disk as `_base-rootfs.ext4` (underscore prefix).

When `base_rootfs` equals one of the `images:` refs (a common pattern where `base_rootfs` and `images.experimental` both point at the same Docker ref), `shed-server pull-images` stores `_base-rootfs.ext4` as a **hardlink** to the matching variant file — zero extra disk cost. Because the two names share the same inode, running `shed image delete experimental` will not reclaim disk until the other name (`_base`) is also removed; `shed image prune` after a config-ref bump handles this cleanly.

## Using Variants

Create a shed with a specific variant:

```bash
shed create myproject --image experimental
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
FROM ghcr.io/charliek/shed-vz-base:{version}

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

Use the corresponding base image for your backend: `shed-vz-base` for VZ (linux/arm64) or `shed-fc-base` for Firecracker (linux/amd64).

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

Add a new stage to `vz/Dockerfile` or `firecracker/Dockerfile` that inherits from the base stage, then build with the build script:

```bash
# VZ
./scripts/build-vz-rootfs.sh --variant rust

# Firecracker
./scripts/build-firecracker-rootfs.sh --variant rust
```

## Organization Images

Organizations can distribute custom shed images to their teams.

### Option A: Dockerfile repo

Maintain a repo with a Dockerfile that extends a published base:

```
shed-acmeco/
  Dockerfile.shed     # FROM ghcr.io/charliek/shed-vz-base:{version}
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

Converted ext4 images are cached in `images_dir`. A `.source` sidecar file tracks which Docker ref produced each image, and a `.lock` sidecar coordinates concurrent pulls.

When the Docker ref in your config changes (for example after a version bump), shed compares each cached `.ext4`'s `.source` against the current config ref. On mismatch, the cache is considered stale — `shed-server pull-images` re-pulls it, and `shed image prune` treats it as a candidate for deletion.

## On-Disk Layout

=== "Firecracker (Linux)"

    Default `images_dir`: `/var/lib/shed/firecracker/images/`

    | File | Description |
    |------|-------------|
    | `{name}-rootfs.ext4` | Cached variant rootfs (20GB sparse, 2–5GB actual). One per entry in `firecracker.images:`. |
    | `{name}-rootfs.ext4.source` | Docker ref this cache was built from. Used by `shed image prune` to detect stale caches. |
    | `{name}-rootfs.ext4.lock` | Empty flock file. Preserved across deletes to avoid a lock-inode race — safe to ignore. |
    | `{name}-rootfs.ext4.tmp` | Transient file that may briefly exist while a cached variant is being hardlinked into `_base` (or any other cache slot). Swept on the next invocation of the hydration path if left behind by a crash — no manual cleanup needed. |
    | `_base-rootfs.ext4` (+ `.source`, `.lock`) | Backing cache for `firecracker.base_rootfs` (the fallback used when `shed create` is invoked without `--image`). Stored as a hardlink to a matching variant when refs align, otherwise a full copy. |
    | `vmlinux` | Extracted kernel (~40MB). |

    Per running shed, the server creates a full copy of the base image at `/var/lib/shed/firecracker/instances/{shed-name}/rootfs.ext4` (another 20GB sparse, 2–5GB actual) plus `metadata.json`. Deleting the shed removes this whole directory.

    Control sockets live in `/var/run/shed/firecracker/` (tiny files).

=== "VZ (macOS)"

    Default `images_dir`: `~/Library/Application Support/shed/vz/`

    | File | Description |
    |------|-------------|
    | `{name}-rootfs.ext4` | Cached variant rootfs (20GB sparse, 2–5GB actual). One per entry in `vz.images:`. |
    | `{name}-rootfs.ext4.source` | Docker ref this cache was built from. Used by `shed image prune` to detect stale caches. |
    | `{name}-rootfs.ext4.lock` | Empty flock file. Preserved across deletes — safe to ignore. |
    | `{name}-rootfs.ext4.tmp` | Transient file that may briefly exist while a cached variant is being hardlinked into `_base` (or any other cache slot). Swept on the next invocation of the hydration path if left behind by a crash — no manual cleanup needed. |
    | `_base-rootfs.ext4` (+ `.source`, `.lock`) | Backing cache for `vz.base_rootfs`. Stored as a hardlink to a matching variant when refs align, otherwise a full copy. |
    | `vmlinux` | Extracted kernel. |
    | `initrd.img` | Extracted initial RAM disk (VZ requires this; Firecracker boots directly from `vmlinux`). |

    Per running shed, the server creates a full copy at `~/Library/Application Support/shed/vz/instances/{shed-name}/rootfs.ext4` plus `metadata.json`. Deleting the shed removes this directory.

    Vsock sockets live in `~/.shed/vz/sockets/` (tiny files).

## Cleaning Up Images

Cached images can be 2–5 GB each, and every running shed adds another 2–5 GB for its instance rootfs. Use these commands to reclaim disk space:

```bash
# Delete a specific cached image (refused if it is currently referenced
# by the config images: map, or if it is _base while base_rootfs is a
# Docker ref, or if an existing shed still depends on it)
shed image delete myimage

# Preview which images would be pruned
shed image prune --dry-run

# Remove all unused or stale cached images
shed image prune
```

When `_base` shares an inode with a variant (because `base_rootfs` and an `images:` entry point at the same Docker ref), `shed image delete _base` is refused — `_base` is still backing the configured `base_rootfs`. Freeing that shared storage requires bumping the `base_rootfs` ref in config (or removing the `base_rootfs` field entirely) and then running `shed image prune`. The [cookbook](#cookbook-upgrading-image-versions-and-reclaiming-disk) below walks through the version-bump case.

`shed image prune` preserves an image only when it matches the current config. Specifically, a cached `.ext4` is preserved when:

- It is referenced in the `images:` map **and** its `.source` sidecar matches the configured Docker ref (or the config entry is a local path, in which case it is always preserved).
- Or it is `_base` **and** its `.source` sidecar matches the configured `base_rootfs`.
- Or it is referenced by an existing shed's metadata.

Anything else — stale caches after a config ref bump, leftover variants from a renamed image, and the underscore-prefixed `_base` when it no longer matches — is a candidate for deletion. Prune removes the `.ext4` and `.source` files; `.lock` files are intentionally left behind (removing them creates a race where concurrent processes can hold locks on different inodes).

Deleting a cached image does not affect running sheds — each shed uses its own copy of the rootfs. You'll need to re-pull or rebuild the image before creating new sheds from it.

### Cookbook: upgrading image versions and reclaiming disk

The common end-to-end flow when bumping image refs (for example, moving config from `:v0.3.3` to `:v0.3.4`):

1. Delete any running sheds you no longer need — this frees their per-instance rootfs.
   ```bash
   shed delete <name>
   ```
2. Edit the server config and bump the image refs. Update both `base_rootfs` and any entries in `images:` that reference the same Docker ref.
3. Restart the server:

    === "Linux (deb)"

        ```bash
        sudo systemctl restart shed-server
        ```

    === "macOS (Homebrew)"

        ```bash
        brew services restart shed
        ```

4. Pull the new images. If `base_rootfs` shares a ref with a variant, `_base-rootfs.ext4` is hardlinked to the variant (no extra disk, no extra pull).
   ```bash
   sudo shed-server pull-images
   ```
5. Reclaim stale caches. The `--dry-run` preview shows which stale files would go; `--force` actually deletes them.
   ```bash
   shed image prune --dry-run
   shed image prune --force
   ```
6. Verify.
   ```bash
   shed image list
   du -sh <images_dir>      # cached variants + _base + kernel
   du -sh <instances_dir>   # running shed rootfs copies
   ```

## Disk Space

Each variant produces a 20GB sparse ext4 image. Actual disk usage is much smaller (typically 2–5GB depending on the variant). Each running shed adds another 2–5GB for its own rootfs copy.

Use `du -sh` to check actual usage:

```bash
# VZ
du -sh ~/Library/Application\ Support/shed/vz/*-rootfs.ext4
du -sh ~/Library/Application\ Support/shed/vz/instances/*

# Firecracker
du -sh /var/lib/shed/firecracker/images/*-rootfs.ext4
du -sh /var/lib/shed/firecracker/instances/*
```

## Requirements

Image conversion requires Docker with privileged container support. The ext4 creation step uses a privileged Docker container for loop mounting.

For VZ images, the kernel and initrd are automatically extracted alongside the rootfs during conversion. Both `shed image build --from` and the auto-pull on `shed create` handle this — no manual kernel extraction is needed.

## Building from Source

=== "VZ (macOS)"

    ```bash
    # Build the default variant
    ./scripts/build-vz-rootfs.sh

    # Build a specific variant
    ./scripts/build-vz-rootfs.sh --variant base
    ./scripts/build-vz-rootfs.sh --variant experimental

    # Build all variants
    ./scripts/build-vz-rootfs.sh --all
    ```

    Output files are placed in `~/Library/Application Support/shed/vz/`:

    | File | Description |
    |------|-------------|
    | `default-rootfs.ext4` | Default variant rootfs (20GB sparse) |
    | `base-rootfs.ext4` | Base variant rootfs |
    | `experimental-rootfs.ext4` | Experimental variant rootfs (default + shed-extensions) |
    | `vmlinux` | Decompressed Linux kernel (shared) |
    | `initrd.img` | Initial RAM disk (shared) |

=== "Firecracker (Linux)"

    ```bash
    # Build the default variant
    ./scripts/build-firecracker-rootfs.sh

    # Build a specific variant
    ./scripts/build-firecracker-rootfs.sh --variant base
    ./scripts/build-firecracker-rootfs.sh --variant experimental

    # Build all variants
    ./scripts/build-firecracker-rootfs.sh --all
    ```

    Output files are placed in `/var/lib/shed/firecracker/images/`:

    | File | Description |
    |------|-------------|
    | `default-rootfs.ext4` | Default variant rootfs (20GB sparse) |
    | `base-rootfs.ext4` | Base variant rootfs |
    | `experimental-rootfs.ext4` | Experimental variant rootfs (default + shed-extensions) |

    To build a custom kernel (for local development or advanced use):

    ```bash
    ./scripts/build-firecracker-kernel.sh
    ```

    Set `kernel_path` in your config to use the custom kernel instead of the one extracted from published images.
