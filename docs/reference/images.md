# Image Variants

Shed images are OCI-compliant container images. The on-disk store is an
[OCI image-layout-v1](https://github.com/opencontainers/image-spec/blob/main/image-layout.md)
directory, layers are shared across tags, and pulls/pushes go directly to any
OCI registry — no Docker daemon required for transport.

Each image carries one or more read-only ext4 layers and a few annotations
(kernel digest, initrd digest, source ref, variant name, schema version). At
boot the in-guest initramfs stacks those layers as overlayfs lowers with a
per-shed writable upper on top.

## Available Variants

| Variant | Description |
|---------|-------------|
| `base` | Minimal OS layer: systemd, SSH, Docker CE, git, gh, build-essential, the shed-agent. No coding agents, no language runtimes. |
| `extensions` | `base` + [shed-extensions](https://charliek.github.io/shed-extensions/) credential brokering (SSH agent forwarding, AWS STS proxy, Docker credential helper). No coding agents pinned — start here for custom images. |
| `full` | `extensions` + Node.js LTS, Python 3.13, Claude Code, OpenCode, Codex CLI, and (VZ only) Cursor CLI. The batteries-included default. |

Each variant inherits from the one above it as a discrete OCI layer, so
`shed image pull shed-vz-full` reuses the `base` and `extensions` layers
already cached locally.

### `extensions` variant

The `extensions` variant adds [shed-extensions](https://charliek.github.io/shed-extensions/) credential brokering on top of `base`. It includes:

- **`shed-ssh-agent`** — SSH agent proxy that forwards key operations to your Mac (private keys never enter the VM).
- **`shed-aws-proxy`** — AWS credential proxy that vends short-lived STS tokens via the host.
- **`docker-credential-shed`** — Docker credential helper that delegates registry authentication to the host. Guest Docker is pre-configured with `{"credsStore": "shed"}`.
- **`shed-ext`** — CLI for checking extension connectivity and health.
- Pre-configured `SSH_AUTH_SOCK` and `AWS_CONTAINER_CREDENTIALS_FULL_URI` environment variables.

**When to use it:** as a base for organization-specific images, or when you
want credential brokering without the full coding-agent set.

**Prerequisite:** the `shed-host-agent` binary must be running on your host.
See the [shed-extensions quick start](https://charliek.github.io/shed-extensions/getting-started/quick-start/).

```bash
shed create mydev --image extensions
```

## Published Images

Pre-built images for each variant are published per release to `ghcr.io`:

| Image | Platform | Tag Format |
|-------|----------|------------|
| `ghcr.io/charliek/shed-vz-base` | linux/arm64 | `:v{version}` |
| `ghcr.io/charliek/shed-vz-extensions` | linux/arm64 | `:v{version}` |
| `ghcr.io/charliek/shed-vz-full` | linux/arm64 | `:v{version}` |
| `ghcr.io/charliek/shed-fc-base` | linux/amd64 | `:v{version}` |
| `ghcr.io/charliek/shed-fc-extensions` | linux/amd64 | `:v{version}` |
| `ghcr.io/charliek/shed-fc-full` | linux/amd64 | `:v{version}` |

Replace `{version}` with the version matching your `shed` binary — run
`shed version` to check.

Both VZ and Firecracker images embed the kernel needed to boot the VM. For
VZ, the kernel and initrd are extracted from the Ubuntu
`linux-image-generic` package. For Firecracker, a custom kernel is compiled
with Docker, 9P, and BPF support built in.

To pre-cache images:

```bash
sudo shed-server pull-images
```

## Layer Model

The on-disk store under `{images_dir}/` is OCI image-layout-v1:

```text
{images_dir}/
  oci-layout                                   # {"imageLayoutVersion":"1.0.0"}
  index.json                                   # OCI image index
  blobs/sha256/<hex>                           # OCI blobs (manifests, configs, layer tar.gz)
  cache/sha256/<hex>.ext4                      # derived per-layer ext4, materialized lazily
  tags/<name>.json                             # tag → manifest digest pointers
  uppers/<shed>/upper.ext4                     # per-shed writable overlay upper
  instances/<shed>/metadata.json               # per-shed bookkeeping (pins manifest digest)
  snapshots/<snap>/snapshot.json               # per-snapshot bookkeeping
```

Each layer is a tar.gz blob in `blobs/sha256/`. The first time a layer is
needed for boot, shed materializes it into `cache/sha256/<hex>.ext4`. Both
forms stay on disk: the tar.gz so `shed image push` can byte-perfectly
round-trip the manifest to a registry, the ext4 so boot is fast.

**Layer sharing.** Two tags that share a base layer share the underlying
blob and the derived ext4 cache — pulling `shed-vz-full:v0.5.0` after
`shed-vz-extensions:v0.5.0` only fetches the top layer.

**Disk overhead.** Keeping both forms costs roughly 1.4× per layer (tar.gz
≈ 0.4× the ext4 size for typical content). The trade-off buys byte-perfect
registry round-trips and fast boot. See
[Layer storage optimization](../discovery/layer-storage-optimization.md)
for design notes on shrinking this in the future.

**Layer cap.** `MaxLayers = 16`. Manifests with more than 16 layers are
rejected at pull/load time. Shed's own variants ship 2–4 layers; the cap
exists to keep overlayfs and boot-time mount overhead bounded for custom
images.

### Inspecting layers with `shed image history`

`shed image history <tag>` walks the manifest top-down (latest layer first)
and prints one row per layer:

```bash
shed image history shed-vz-full
```

```text
LAYER  DIGEST                                                                   SIZE      CREATED         CREATED BY
3      sha256:9a1c4f3b8e2d7a05c1e0a3f81b9c6d2e74a8b5c3f9e2d7a1c4f3b8e2d7a05c1   612.4 MB  2 hours ago     LABEL io.shed.variant=full
2      sha256:7c2e5d0f4a8b9c1e3f6a8d2b5c9e1f4a7b0d3c6f9e2d5b8a1c4f7e0d3b6a9c   148.2 MB  2 hours ago     LABEL io.shed.variant=extensions
1      sha256:3f6a8d2b5c9e1f4a7b0d3c6f9e2d5b8a1c4f7e0d3b6a9c2e5d0f4a8b9c1e3f6   1.84 GB   2 hours ago     LABEL io.shed.variant=base
```

The `CREATED BY` column is the corresponding history entry from the OCI
config — most often the `Dockerfile` line that produced the layer. Layer
ordinals match the order the in-guest initramfs stacks them: ordinal 1 is
the bottom-most overlay lower, ordinal N is just below the writable upper.

### Air-gap transport with `shed image save` / `shed image load`

`shed image save` writes a tag (and every layer it references) to a
single OCI archive file, suitable for transferring across an air-gap or
keeping as a backup:

```bash
shed image save shed-vz-full -o shed-vz-full.tar
```

The archive is a standard OCI image layout — third-party tools work on it
unmodified. For example, `crane manifest --from-archive` can pull the
manifest out without needing a registry:

```bash
crane manifest --from-archive shed-vz-full.tar
```

`shed image load` is the inverse:

```bash
shed image load -i shed-vz-full.tar
```

It unpacks each blob into the local store, advances the tag(s), and
materializes the ext4 cache lazily on first boot. Layers already present
locally are skipped.

### Push to a registry

`shed image push` uploads a tag or digest to any OCI registry:

```bash
shed image push shed-vz-full ghcr.io/myorg/shed-vz-full:v0.5.0
shed image push sha256:9a1c... ghcr.io/myorg/shed-vz-full@sha256:9a1c...
```

The upload is byte-perfect: the manifest digest at the destination equals
the manifest digest in the local store. That means a tag pushed from one
host and pulled by another resolves to the same digest, and `shed image
inspect` shows identical manifest annotations on both sides.

Registry authentication uses your Docker credential helper (e.g. the
shed-extensions Docker credential proxy when the `extensions` variant is
running). For host-side `shed image push` invocations, the regular
`~/.docker/config.json` credential resolution applies.

## Manifest annotations

Every shed-built manifest carries these annotations (visible via
`shed image inspect`):

| Annotation | Description |
|---|---|
| `io.shed.variant` | One of `base`, `extensions`, `full`, or a custom string for derivations. |
| `io.shed.source-ref` | The Docker / OCI reference the image was pulled or built from. |
| `io.shed.kernel.digest` | Digest of the kernel blob embedded in the image. |
| `io.shed.initrd.digest` | Digest of the initrd (shed-built initramfs) blob. |
| `io.shed.schema-version` | Metadata schema version (currently `v3`). |
| `io.shed.rootfs.logical-size` | Sum of layer logical sizes — what the merged overlay sees from inside the guest. |

Annotations are preserved across `pull` / `save` / `load` / `push` and
match byte-for-byte across hosts.

## Boot stack

When `shed start` runs, the in-guest initramfs:

1. Mounts each layer's ext4 read-only at `/lower/<n>`.
2. Mounts `/dev/vdb` (the per-shed `upper.ext4`) read-write at `/upper`.
3. Mounts an overlayfs at `/sysroot` with lowers `lower/1:lower/2:…:lower/N`
   (top layer last) and upper `/upper`.
4. `switch_root` into `/sysroot`.

Panic messages from the initramfs are numbered for triage:

| Code | Meaning |
|---|---|
| `SHED-INIT-02` | Kernel cmdline missing a required `shed.lower.N=` directive. |
| `SHED-INIT-03` | A layer device referenced by cmdline is absent. |
| `SHED-INIT-04` | Layer ext4 superblock check failed (wrong magic at offset 1080). |
| `SHED-INIT-05` | Upper ext4 superblock check failed — typically the upper is uninitialized; recoverable with `shed reset <name>`. |
| `SHED-INIT-06` | `overlayfs` mount returned `-EINVAL`; usually a kernel-without-overlay regression. |
| `SHED-INIT-07` | Could not pivot into `/sysroot`. |
| `SHED-INIT-08` | More than `MaxLayers` (16) layers declared. |
| `SHED-INIT-09` | Schema-version mismatch — the running initramfs expects metadata v3, the image declares something else. |

`shed reset <name>` recovers from upper-corruption panics
(`SHED-INIT-05`).

## Server Configuration

### Using published images (recommended)

Point your config at OCI references. Shed pulls them registry-direct on
first `shed create` (no Docker daemon needed for pull):

=== "VZ (macOS)"

    ```yaml
    vz:
      base_rootfs: ghcr.io/charliek/shed-vz-full:v{version}
      images:
        base: ghcr.io/charliek/shed-vz-base:v{version}
        extensions: ghcr.io/charliek/shed-vz-extensions:v{version}
        full: ghcr.io/charliek/shed-vz-full:v{version}
      images_dir: ~/Library/Application Support/shed/vz/
    ```

=== "Firecracker (Linux)"

    ```yaml
    firecracker:
      base_rootfs: ghcr.io/charliek/shed-fc-full:v{version}
      images:
        base: ghcr.io/charliek/shed-fc-base:v{version}
        extensions: ghcr.io/charliek/shed-fc-extensions:v{version}
        full: ghcr.io/charliek/shed-fc-full:v{version}
      images_dir: /var/lib/shed/firecracker/images
    ```

### Using local images

If you build images locally, point to OCI references in a local
registry or to tags already present in the blob store:

```yaml
vz:
  base_rootfs: full
  images:
    base: base
    extensions: extensions
    full: full
```

You can mix registry refs and local tags in the same config.

The `base_rootfs` field is the default when no `--image` flag is passed
to `shed create`. The `images` map enables per-shed variant selection.
`images_dir` is the OCI store root.

### `base_rootfs` vs `images:`

- `images:` is a map of named variants. Each entry is selectable via
  `shed create --image <name>`, pre-pullable via
  `shed-server pull-images`, and visible in `shed image ls`.
- `base_rootfs` is the fallback for `shed create` invocations without
  `--image`. It's stored as the underscore-prefixed tag `_base` so it
  doesn't collide with user-facing variant names.

When `base_rootfs` matches an `images:` entry, the underlying manifest
digest is shared — `_base` and the variant tag point at the same OCI
manifest, and pulls deduplicate to a single layer set.

## Using Variants

```bash
shed create myproject --image full        # batteries-included
shed create myproject --image extensions  # credential brokering, no agents
shed create tools --image base            # minimal
```

Default (no `--image` flag) uses `base_rootfs`:

```bash
shed create myproject
```

List local images:

```bash
shed image ls
```

## Creating Custom Images

The easiest path is a Dockerfile that extends a published variant. Most
custom images start from `extensions` — you get credential brokering for
free, no coding agents are pre-pinned, and your layer sits on top of a
stable shed-managed base.

```dockerfile
FROM ghcr.io/charliek/shed-vz-extensions:v{version}

USER shed
ENV PATH="/home/shed/.local/bin:${PATH}"

RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y
ENV PATH="/home/shed/.cargo/bin:${PATH}"

RUN curl -fsSL https://claude.ai/install.sh | bash

USER root
WORKDIR /workspace
ENTRYPOINT ["/sbin/init"]
```

Constraints when extending a shed image:

- Keep `ENTRYPOINT ["/sbin/init"]` — shed boots via systemd.
- Don't remove or reassign the `shed` user (UID 1000); much of the boot
  setup assumes it.
- Don't disable the `shed-agent.service` unit.

Build for the right backend platform (`linux/arm64` for VZ,
`linux/amd64` for Firecracker):

```bash
shed image build -f Dockerfile.shed -n rust .
shed create myproject --image rust
```

For an end-to-end worked example including push to a private registry,
see [Build your own image](../tutorials/build-your-own-image.md).

### Builder backend

`shed image build` currently shells out to `docker buildx`. The
`--builder` flag accepts `docker` (the only working option today);
`podman` and `buildah` are placeholders for a future docker-free build
path (see [Docker-free builds](../discovery/docker-free-builds.md)).
`shed image pull` is already docker-free — it uses
[`go-containerregistry`](https://github.com/google/go-containerregistry)
directly.

## Image Caching

Pull / save / load / build all land manifests and blobs in the OCI
store. Tag pointers (`tags/<name>.json`) are the human-readable handles.

When the OCI ref in your config changes (for example after a version
bump), shed compares the manifest's recorded `io.shed.source-ref`
annotation against the configured ref. On mismatch, the cache is
considered stale and `shed-server pull-images` re-pulls.

## Cleaning Up Images

Shed follows the Docker model: `shed image rm` removes a tag,
`shed image prune` garbage-collects unreferenced blobs.

```bash
shed image rm myimage           # removes the tag; blobs persist
shed image prune --dry-run      # preview reclaim
shed image prune                # reclaim
```

`shed image prune` walks every shed and snapshot, collects the manifest
digests they pin (`instances/<name>/metadata.json` → `lower_digest`),
expands each manifest to the layers it references, and deletes any blob
or cached ext4 not in that reachable set. Tags do NOT protect blobs.

Deleting a tag for an image that's still pinned by a running or stopped
shed is safe — the shed boots from the digest pinned in its metadata,
not the tag.

### Cookbook: upgrading image versions

1. Delete any sheds you no longer need.
   ```bash
   shed delete <name>
   ```
2. Bump the refs in `server.yaml`.
3. Restart the server.

    === "Linux (deb)"

        ```bash
        sudo systemctl restart shed-server
        ```

    === "macOS (Homebrew)"

        ```bash
        brew services restart shed
        ```

4. Pull the new images.
   ```bash
   sudo shed-server pull-images
   ```
5. Reclaim stale blobs.
   ```bash
   shed image prune --dry-run
   shed image prune --force
   ```
6. Verify.
   ```bash
   shed image ls
   shed system df
   ```

## Upgrading across schema versions

Metadata schema went `v2` → `v3` with the OCI store rollout. The
initramfs refuses to mount lower images stamped with anything other than
`v3`. Pre-v3 sheds and snapshots are not migrated automatically.

To upgrade from a v2 install:

1. Stop the server.
2. Delete `images_dir`, `instances/`, `snapshots/`, and `uppers/`.
3. Install the new release.
4. Restart the server and re-pull images.

Workspace data under `--local-dir` mounts is unaffected. Workspace data
that lived only inside the deleted upper layers is lost, by design — the
upper layer is per-shed scratch space, not durable storage.

## Disk Space

Per-shed cost is the writable upper layer alone (sparse, default 5 GB,
configurable via `shed create --upper-size`) — not a copy of the rootfs.
All read-only layers are shared across every shed pinning the same
manifest digest, both on disk and in the host page cache.

To measure usage:

```bash
shed system df
shed system df -v
```

See [Disk Management](disk-management.md) and
[Storage Model](storage-model.md) for layout details and reclamation.
