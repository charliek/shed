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

The `base_rootfs` field is used when no `--image` flag is specified. The `images` map enables per-shed variant selection via `--image`. The `images_dir` directory is scanned for auto-discovered tags under `tags/<name>.json`.

### `base_rootfs` vs `images:` — how they interact

The two fields play different roles:

- `images:` is a map of named variants. Each entry is cacheable, pre-pullable via `shed-server pull-images`, visible in `shed image ls`, and selectable with `shed create --image <name>`.
- `base_rootfs` is a single top-level fallback used when `shed create` is invoked without `--image`. Under the content-addressed layout it is just another tag — `_base` — pointing at a digest under `blobs/sha256/<digest>/`, not a flat `_base-rootfs.ext4` file. The underscore-prefixed name keeps it distinct from user-visible variants in `shed image ls`.

When `base_rootfs` equals one of the `images:` refs (a common pattern where `base_rootfs` and `images.experimental` both point at the same Docker ref), `shed-server pull-images` points the `_base` and `experimental` tags at the same content-addressed blob — zero extra disk cost. Because both tags share a digest, removing one (`shed image rm experimental`) leaves the blob in place; only `shed image prune` reclaims it once nothing pins the digest.

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
shed image ls
```

(`shed image list` is kept as a deprecated alias for one release.)

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

Converted ext4 images are cached in `images_dir` as **content-addressed
blobs** with **tag indirection**. Each ext4 image is identified by the
sha256 digest of its bytes; tags are pointers from human-readable names
to digests. See [Storage Model](storage-model.md) for the full layout
and rationale.

When the Docker ref in your config changes (for example after a
version bump), shed compares the manifest's recorded `source_ref`
against the configured ref. On mismatch, the cache is considered stale
— `shed-server pull-images` re-pulls it. `shed image prune` removes
any blob with no protective references (no shed and no snapshot).

## On-Disk Layout

=== "Firecracker (Linux)"

    Default `images_dir`: `/var/lib/shed/firecracker/images/`

    | Path | Description |
    |------|-------------|
    | `blobs/sha256/<digest>/rootfs.ext4` | Read-only blob content (mode 0444). |
    | `blobs/sha256/<digest>/manifest.json` | Image metadata: digest, source_ref, sizes, timestamps. |
    | `blobs/sha256/<digest>/kernel` | Extracted kernel for this image. |
    | `blobs/sha256/.<digest>.lock` | Empty flock file used to serialize install/prune. |
    | `tags/<tag>.json` | Tag → digest pointer (`{"digest": "sha256:...", "updated_at": "..."}`). |

    Per shed, the server creates:

    - `/var/lib/shed/firecracker/uppers/{shed-name}/upper.ext4` — the per-shed writable overlay (sparse, default 5 GB). `mkfs.ext4` runs on first boot inside the guest.
    - `/var/lib/shed/firecracker/instances/{shed-name}/metadata.json` — v2 schema; records `lower_digest` (the shared blob's content address, which protects it from prune) and `upper_path`.

    The lower image is NOT copied per shed — it's shared via the blob store. `shed delete <name>` removes both the upper directory and the instance directory, but never touches the shared lower.

    Control sockets live in `/var/run/shed/firecracker/` (tiny files).

=== "VZ (macOS)"

    Default `images_dir`: `~/Library/Application Support/shed/vz/`

    | Path | Description |
    |------|-------------|
    | `blobs/sha256/<digest>/rootfs.ext4` | Read-only blob content. |
    | `blobs/sha256/<digest>/manifest.json` | Image metadata. |
    | `blobs/sha256/<digest>/kernel` | Extracted kernel. |
    | `blobs/sha256/<digest>/initrd` | Extracted initrd (VZ requires this). |
    | `tags/<tag>.json` | Tag → digest pointer. |

    Per shed, the server creates:

    - `~/Library/Application Support/shed/vz/uppers/{shed-name}/upper.ext4` — the per-shed writable overlay (sparse, default 5 GB).
    - `~/Library/Application Support/shed/vz/instances/{shed-name}/metadata.json` — records `lower_digest` and `upper_path`.

    The lower image is NOT copied per shed — it's shared via the blob store. `shed delete <name>` removes both the upper and instance directories.

    Vsock sockets live in `~/.shed/vz/sockets/` (tiny files).

## Cleaning Up Images

Shed follows the Docker model: `shed image rm` removes a tag,
`shed image prune` garbage-collects unreferenced blobs.

```bash
# Remove a tag. The underlying blob is NOT deleted by this command;
# any other tag pointing at the same digest still resolves, and any
# shed/snapshot pinning the digest is unaffected.
shed image rm myimage

# Preview which blobs would be reclaimed (those with no protective
# shed or snapshot reference)
shed image prune --dry-run

# Reclaim them
shed image prune
```

`shed image rm` is refused when the tag is in the `images:` config map
(or `_base` while `base_rootfs` is a Docker ref) — those are
system-managed tags. To bump them, edit the server config and let
`shed-server pull-images` advance the tag to the new digest; the
previous digest becomes dangling and `shed image prune` reclaims it.

`shed image prune` protects a digest only when it has at least one
**shed** or **snapshot** reference recorded in its on-disk metadata.
Tags do NOT protect a digest. After
`shed image rm experimental` you typically also want
`shed image prune` to actually free the blob.

Deleting a cached image does not affect running sheds — every shed
pins its lower digest in `metadata.json`, and that digest is what
boots the VM (via the shared `blobs/sha256/<digest>/rootfs.ext4`).
Pruning a digest that's still pinned is refused; only after the last
shed and snapshot referencing it are gone does `shed image prune`
reclaim the blob. To create *new* sheds from a deleted image, re-pull
or rebuild it first.

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

4. Pull the new images. Each pull lands a new blob under
   `blobs/sha256/<digest>/` and advances the tag (`tags/<tag>.json`) to
   the new digest. When `base_rootfs` shares a Docker ref with a variant,
   the same blob backs both `_base` and the variant tag — no duplicate
   blob, no second pull.
   ```bash
   sudo shed-server pull-images
   ```
5. Reclaim stale blobs. After the new pull advances each tag to a new
   digest, the previous digests are dangling — protected only if a
   shed/snapshot still pins them.
   ```bash
   shed image prune --dry-run
   shed image prune --force
   ```
6. Verify.
   ```bash
   shed image ls
   du -sh <images_dir>      # blobs + tags + kernel
   du -sh <instances_dir>   # per-shed metadata + (VZ) console.log
   du -sh <uppers_dir>      # per-shed writable upper layers
   ```

## Disk Space

Each variant is a 20 GB sparse ext4 image; actual physical usage is
typically 2–5 GB depending on what the variant installed. Per-shed cost is
the writable upper layer alone (sparse, default 5 GB, configurable via
`shed create --upper-size`) — not a copy of the rootfs. The lower image
(the cached blob) is shared read-only across every shed pinning the same
digest, both on disk and in the host page cache. See
[Storage Model](storage-model.md) for the overlay-in-guest design.

To measure usage, use `shed system df`:

```bash
shed system df
shed system df -v
```

See [Disk Management](disk-management.md) for how to measure and reclaim
space with `shed system df` / `shed system prune`, plus the APFS
extent-sharing caveat that affects physical-byte reporting.

## Requirements

Image conversion requires Docker with privileged container support. The ext4 creation step uses a privileged Docker container for loop mounting.

For VZ images, the kernel and initrd are extracted from the source image
and written into the blob directory alongside the rootfs
(`{images_dir}/blobs/sha256/<digest>/{kernel,initrd}`). Phase B's
in-guest initramfs prefers these embedded files over the legacy
`kernel_path`/`initrd_path` config values. Both `shed image build --from`
and the auto-pull on `shed create` handle this — no manual extraction is
needed.

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
