# Images

Shed images are OCI-compliant container images. The on-disk store is an
[OCI image-layout-v1](https://github.com/opencontainers/image-spec/blob/main/image-layout.md)
directory, layers are shared across tags, and pulls/pushes go directly to any
OCI registry — no Docker daemon required for transport.

Each image carries:

- one or more read-only gzipped tar layers (Dockerfile-stage shape preserved),
- a prebuilt rootfs erofs blob (`io.shed.rootfs.erofs.digest` annotation; this
  is what the VM mounts read-only at `/dev/vdb`),
- a kernel blob (`io.shed.kernel.digest`),
- an initrd blob (`io.shed.initrd.digest`),
- standard annotations (source ref, variant name, shed schema version).

The rootfs erofs is minted at image-publish time by `mkfs.erofs` running
inside the [shed-build-tools](build-tools.md) container — a known-good
`erofs-utils` version is pinned there so every consumer sees byte-identical
filesystem layout regardless of where the image was published or where
it'll be mounted. Hosts do not invoke `mkfs.erofs` themselves; they
download the blob and mount it directly. At boot the in-guest initramfs
assembles a single-lower overlayfs with the per-shed writable upper on
top.

## Prerequisites

No host-side erofs tooling is required. The shed-server package does
not depend on `erofs-utils`; the OCI manifests ship the prebuilt erofs
as a content-addressed blob. (Hosts that *publish* images — i.e., run
`shed image build` — need Docker so the build-tools container can run,
but not `erofs-utils` directly on the host.)

If you're upgrading from v0.5.1 or earlier, see the
[v0.5.1 → v0.5.2 upgrade guide](../upgrades/v0.5.1-to-v0.5.2.md) for the
breaking-change details and the required `shed image rm` / `pull-images`
steps.

## Available images

| Variant | Description |
|---------|-------------|
| `base` | Minimal OS layer: systemd, SSH, git, gh, build-essential, jq, ripgrep, neovim, tmux, and the shed-agent. No Docker, no coding agents, no language runtimes. |
| `extensions` | `base` + [credential brokering](extensions.md) (SSH agent forwarding, AWS STS proxy, Docker credential helper) and the `shed-ext-rc` Remote Control helper. No Docker daemon, no coding agents pinned — start here for custom images. |
| `full` | `extensions` + Docker CE (with `docker compose`), [mise](https://mise.jdx.dev/), [bun](https://bun.sh/), and coding agents (Claude Code, OpenCode, Codex CLI). The batteries-included default. |

Each variant inherits from the one above it as a discrete OCI layer, so
`shed image pull shed-vz-full` reuses the `base` and `extensions` layers
already cached locally.

!!! note "Autonomous Remote Control sessions need `full`"
    `shed attach --plan` / Remote Control autonomous sessions drive Claude Code
    via `shed-ext-rc`. `shed-ext-rc` ships in `extensions` and `full`, but Claude
    Code is only in `full` — so the autonomous-plan workflow requires the `full`
    variant (and Claude logged in inside the shed).

!!! note "Docker is only in `full`"
    The Docker daemon ships **only** in the `full` variant. `base` and
    `extensions` install the `docker-credential-shed` helper config but not
    the daemon. Provisioning hooks that use `docker` / `docker compose`
    (Testcontainers, service stacks) therefore require the `full` image.
    `full` does **not** ship a system Node.js or Python — install language
    runtimes per-project (e.g. `uv` provisions Python, `bun` is built in).
    See [Provisioning](provisioning.md) for the patterns.

### `extensions` variant

The `extensions` variant adds [credential brokering](extensions.md) on top of `base`. It includes:

- **`shed-ssh-agent`** — SSH agent proxy that forwards key operations to your Mac (private keys never enter the VM).
- **`shed-aws-proxy`** — AWS credential proxy that vends short-lived STS tokens via the host.
- **`docker-credential-shed`** — Docker credential helper that delegates registry authentication to the host. Guest Docker is pre-configured with `{"credsStore": "shed"}`.
- **`shed-ext`** — CLI for checking extension connectivity and health.
- Pre-configured `SSH_AUTH_SOCK` and `AWS_CONTAINER_CREDENTIALS_FULL_URI` environment variables.

**When to use it:** as a base for organization-specific images, or when you
want credential brokering without the full coding-agent set.

**Prerequisite:** the `shed-host-agent` binary must be running on your host.
See the [macOS quickstart](../getting-started/macos-quickstart.md) for host-agent
setup and the [Extensions configuration reference](../extensions/configuration.md).

```bash
shed create mydev --image extensions
```

## Published Images

Pre-built images for each variant are published per release to `ghcr.io`:

| Image | Platform | Tag Format |
|-------|----------|------------|
| `ghcr.io/charliek/shed-vz-base` | linux/arm64 | `:vX.Y.Z` |
| `ghcr.io/charliek/shed-vz-extensions` | linux/arm64 | `:vX.Y.Z` |
| `ghcr.io/charliek/shed-vz-full` | linux/arm64 | `:vX.Y.Z` |
| `ghcr.io/charliek/shed-fc-base` | linux/amd64 | `:vX.Y.Z` |
| `ghcr.io/charliek/shed-fc-extensions` | linux/amd64 | `:vX.Y.Z` |
| `ghcr.io/charliek/shed-fc-full` | linux/amd64 | `:vX.Y.Z` |

Each release publishes a `:vX.Y.Z` tag matching the `shed` version (e.g.
`:v0.6.5`) — run `shed version` to check. In **server config**, leave
`default_image`/`image_aliases` unset to track the version automatically, or use
the `${shed.version}` token — see
[Configuration](configuration.md#image-references-and-shedversion). (That token
is shed-config only; a Dockerfile `FROM` must pin a concrete tag.)

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
  blobs/sha256/<hex>                           # OCI blobs (manifests, configs, layer tar.gz, kernel, initrd, rootfs erofs)
  tags/<name>.json                             # tag → manifest digest pointers
  uppers/<shed>/upper.ext4                     # per-shed writable overlay upper
  instances/<shed>/metadata.json               # per-shed bookkeeping (pins manifest digest)
  snapshots/<snap>/snapshot.json               # per-snapshot bookkeeping
```

**Everything is a content-addressed blob.** Layer tarballs, kernel,
initrd, and the prebuilt rootfs erofs all live in `blobs/sha256/` keyed
by their content sha256. The rootfs erofs is what the VM mounts at boot
as `/dev/vdb`. The layer tarballs are only needed to byte-perfectly
re-push a *pulled* image (`shed image push`) — booting never reads them —
so by default `shed image pull` and `shed create` pull **boot-only**,
skipping the layer tarballs entirely (see [Pulling, building,
pushing](#pulling-building-pushing) below and the
[boot-only section](cli.md#boot-only-by-default) in the CLI reference).

The legacy `cache/sha256/<manifest-digest>.erofs` directory (used
through v0.5.1 for locally-materialized erofs files) is gone. Older
installs may still have one — it's safe to `rm -rf` after upgrading;
see the [v0.5.1 → v0.5.2 upgrade guide](../upgrades/v0.5.1-to-v0.5.2.md).

**Layer sharing happens at the blob layer.** When layers *are* present
(a `--with-layers` pull, or a locally-built image), two tags that share a
base layer share the underlying blob, deduplicated by digest. The
flattened erofs is per-manifest, so each variant has its own erofs file
(no cross-variant sharing for the boot artifact).

**Disk overhead.** Each manifest's flattened erofs file is roughly
0.5–0.7× the equivalent uncompressed ext4 (lz4 compression). A boot-only
image costs essentially just its erofs (+ the small shared kernel/initrd);
a full image adds its layer tar.gz blobs (deduplicated across manifests)
on top. `shed image ls` reports the real on-disk size and flags boot-only
images under a `LAYERS` column. See
[Storage Model → Disk overhead](storage-model.md#disk-overhead) and
[Lazy rootfs streaming](../discovery/lazy-rootfs-streaming.md)
for design notes on shrinking this further.

**Layer cap.** `MaxLayers = 16`. Manifests with more than 16 layers are
rejected at pull/load time. Shed's own variants ship 5–10 layers (`base`
sits near the low end, `full` near the high end). With the flatten model
the cap is a soft hint — only the blob deduplication math cares about
layer count at boot time, not overlayfs assembly.

### Inspecting layers with `shed image history`

`shed image history <tag>` walks the manifest top-down (latest layer first)
and prints one row per layer. The output describes the manifest's layer
chain — useful for understanding what blobs the image references and how
much each Dockerfile instruction contributes to the size — but does NOT
correspond to a guest-side overlay stack. The guest sees a single
flattened lower regardless of the number of layers.

```bash
shed image history shed-vz-full
```

```text
LAYER  DIGEST                                                                   SIZE      CREATED         CREATED BY
9      sha256:6214c050b2d46d711a9878da53f2ae1f1c2cc2644d1d30f9116d346c59d06ab2   493.4 MB  2 hours ago     RUN runuser -l shed -c '… mise use -g node@lts; uv python install 3.13; …'
8      sha256:4f4fb700ef54461cfa02571ae0db9a0dc1e0cdb5577484a6d75e68dc38e8acc1     32 B    2 hours ago     ENV CLAUDE_CONFIG_DIR=/home/shed/.claude
7      sha256:5c61939d1edf11daa570fcfe8ea24b56a60a89403f5ce91c4354cd400cad2591   6.92 MB   2 hours ago     RUN --mount=type=bind,target=/ctx … (install staged extension binaries)
…
2      sha256:a3e89a578b079f684c28e09084737b3ff22914ab234c60ae0064c6f4d218be54   1.18 GB   2 hours ago     RUN apt-get install systemd docker-ce …
1      sha256:818154cda96df8bbb276b4f4339124da55756620a1037af15570bc95312850fa     28 MB   2 hours ago     ubuntu:24.04 base
```

The `CREATED BY` column is the corresponding history entry from the OCI
config — most often the `Dockerfile` line that produced the layer. The
big shared layers — `ubuntu:24.04` (ordinal 1) and the APT install
(ordinal 2) — appear with the same digest in `base`, `extensions`, and
`full`, so the underlying tar.gz blobs cost once across all three
variants.

### Known build quirks

Two artifacts of the Docker/BuildKit build pipeline are worth knowing when
reading `shed image history` or reasoning about blob sharing:

- **Layer non-determinism across "identical" builds.** BuildKit's gzip/tar
  emission is not byte-stable, so a layer intended to be identical across
  variants (or across two builds of the same variant) can differ by a few
  bytes and get a different digest — defeating cross-variant blob sharing
  and causing a post-build `pull` of the published tag to re-download
  content you already have locally. Pinning `SOURCE_DATE_EPOCH` and using
  `BUILDKIT_INLINE_CACHE` mitigate but do not fully eliminate this.
- **32-byte empty layers.** Stages that only set `ENV` / `LABEL` /
  `WORKDIR` produce a 32-byte gzipped-empty-tar layer (e.g. digest
  `sha256:4f4fb700…`, visible in the history output above). They are
  cosmetic but pollute `shed image history` and count against the
  `MaxLayers = 16` budget.

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

It unpacks each blob into the local store and advances the tag(s).
The rootfs erofs blob ships in the OCI archive alongside the layers —
no on-host `mkfs.erofs` step is needed. Layers already present
locally are skipped.

### Push to a registry

`shed image push` uploads a tag or digest to any OCI registry:

```bash
shed image push shed-vz-full ghcr.io/myorg/shed-vz-full:v0.5.1
shed image push sha256:9a1c... ghcr.io/myorg/shed-vz-full@sha256:9a1c...
```

The upload is byte-perfect: the manifest digest at the destination equals
the manifest digest in the local store. That means a tag pushed from one
host and pulled by another resolves to the same digest, and `shed image
inspect` shows identical manifest annotations on both sides.

Registry authentication uses your Docker credential helper (e.g. the
Docker credential proxy extension when the `extensions` variant is
running). For host-side `shed image push` invocations, the regular
`~/.docker/config.json` credential resolution applies.

The upload streams the layer blobs straight from disk, so they must all
be present. An image pulled **boot-only** has none — push fails a layer
preflight (HTTP 409, `image layers not present locally`); re-pull it with
`shed image pull <ref> --with-layers` first. Images you built locally
already have their layers, so build → push needs no extra step.

## Pulling, building, pushing

How the three flows move bytes, and what each needs on disk.

### Pull (registry → store)

`shed image pull` (and the implicit pull during `shed create`) is
**registry-direct** — no Docker daemon. It fetches the manifest, config,
kernel, initrd, and the prebuilt rootfs erofs, downloading blobs
**concurrently** (bounded; Docker-style, ~half the wall-clock of serial
on a high-latency link).

By default the pull is **boot-only**: it skips the layer tarballs (the
host boots from the erofs and never reads them), saving roughly half the
bytes and disk of a typical image. `--with-layers` pulls the full image,
and on an already-boot-only image it *hydrates* — re-runs the pull and
fetches only the missing layer blobs (present blobs are content-addressed
no-ops). On an interactive terminal the pull renders Docker-style live
per-blob progress bars; piped/`--json`/older-server output falls back to
plain status lines. Pre-v0.5.2 images (no erofs annotation) can't be
pulled boot-only and require `--with-layers`. See
[boot-only pulls](cli.md#boot-only-by-default).

### Build (Dockerfile → store)

`shed image build` is **self-contained** and does not consume any
previously-pulled layers. The `FROM <shed-vz-*|shed-fc-*>` base is
resolved by `docker buildx` (or podman/buildah, or a pre-built OCI
archive via `--from-oci-archive`) into the build tool's own cache; shed
then ingests the produced OCI image — installing its layer blobs and
minting the rootfs erofs from *those just-built* layers (via the
`shed-build-tools` container). So a built image is always **full** (it
has its layers), and building/pushing your own image is unaffected by
boot-only pulls. See [Creating Custom Images](#creating-custom-images)
and the [Build Your Own Image](../tutorials/build-your-own-image.md)
tutorial.

### Push (store → registry)

`shed image push` is byte-perfect and streams the layer blobs from disk,
so the image must be **full** — a boot-only pull must be hydrated with
`--with-layers` first (see above). The only host that re-pushes a
*pulled* image is a mirror/relay; build hosts push images they just
built (which already have their layers).

### On disk

Everything is a content-addressed blob under `{images_dir}/blobs/sha256/`.
Boot mounts the erofs directly; a boot-only image stores the erofs +
kernel + initrd and omits the layer tarballs. See the
[Storage Model](storage-model.md) for the full layout, reachability, and
prune behavior (which tolerates the absent layers of a boot-only image).

## Manifest annotations

Every shed-built manifest carries these annotations (visible via
`shed image inspect`):

| Annotation | Description |
|---|---|
| `io.shed.variant` | One of `base`, `extensions`, `full`, or a custom string for derivations. |
| `io.shed.source-ref` | The Docker / OCI reference the image was pulled or built from. |
| `io.shed.kernel.digest` | Digest of the kernel blob embedded in the image. |
| `io.shed.initrd.digest` | Digest of the initrd (shed-built initramfs) blob. |
| `io.shed.rootfs.erofs.digest` | Digest of the prebuilt read-only rootfs erofs blob the VM mounts at `/dev/vdb`. v0.5.2+. Required — images missing this annotation are rejected at boot. |
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

Shed pulls published OCI references registry-direct on first `shed create`
(no Docker daemon needed for pull). The rootfs images are published in
lockstep with each release — `ghcr.io/charliek/shed-<backend>-{base,
extensions,full}:vX.Y.Z`.

A released `shed-server` resolves them from **its own version**, so the
common config names no image at all and never carries a tag to bump:

=== "VZ (macOS)"

    ```yaml
    vz:
      pull_policy: missing
      images_dir: ~/Library/Application Support/shed/vz/
      # default_image / image_aliases omitted -> resolved from the server version
    ```

=== "Firecracker (Linux)"

    ```yaml
    firecracker:
      pull_policy: missing
      images_dir: /var/lib/shed/firecracker/images
      # default_image / image_aliases omitted -> resolved from the server version
    ```

Upgrading the server (brew/deb) is all it takes to move to the new images;
`pull_policy: missing` means the new tag is a cache miss and pulls on the
next `shed create`. `shed server <name> /info` and the server's startup log
report the resolved `default_image`.

### Resolving images from the server version

When `default_image` (or `image_aliases`) is unset, a release build fills
it with the lockstep ref for its version. To keep an explicit ref — a
specific release, a private mirror — while still tracking the server
version, use the **`${shed.version}`** token. It expands to the running
server's tag (e.g. `v0.6.2`) when the config loads:

```yaml
vz:
  default_image: myregistry.example.com/shed-vz-full:${shed.version}
  image_aliases:
    base: myregistry.example.com/shed-vz-base:${shed.version}
```

The token and version-synthesis only apply to **release builds**. A
dev/dirty/ahead-of-tag `shed-server` has no matching published image, so it
does not synthesize a default, and a `${shed.version}` token is rejected at
load with an actionable error — pin an explicit ref on such builds (see
[Using local images](#using-local-images)).

### Using local images

If you build images locally, point `default_image` (and any aliases) at a
local OCI ref, a ref in a local registry, or a cosmetic tag you applied with
`shed image build/pull -t <label>`:

```yaml
vz:
  default_image: ghcr.io/charliek/shed-vz-full:dev
  image_aliases:
    base: my-base-label
```

You can mix registry refs and local labels in the same config.

`default_image` is the image used when no `--image` flag is passed to
`shed create`. `image_aliases` is an optional convenience map for
`shed create --image <alias>`. `images_dir` is the content-addressed OCI
store root.

### Identity, `default_image`, and `image_aliases`

- An image's identity is its **Docker ref** (recorded as the
  `io.shed.source-ref` manifest annotation and indexed by the ref it was
  pulled by). `shed image ls` lists images by ref.
- `default_image` is the ref `shed create` resolves when given no `--image`.
- `image_aliases` are short names that resolve to a ref — `shed create
  --image full` is shorthand for the aliased ref. Listings always show the
  resolved ref, never the alias.
- `pull_policy` controls cache-vs-pull (see [Pull policy](#pull-policy)
  below). A version bump to `default_image` is a cache miss and re-pulls on
  the next `shed create`.

When `default_image` and an alias point at the same ref, they converge on one
content-addressed manifest — a single pull, shared layers.

### Pull policy

`pull_policy` (per backend) governs how `shed create` reconciles the
configured ref against the local store:

| Value | Behavior |
|-------|----------|
| `missing` (default) | Use the cached ref; pull only if absent. Fast and offline-tolerant. |
| `always` | Always contact the registry and pull, even if cached. |
| `never` | Use the cached ref; error if absent. Never contacts the registry. |

Policy is ignored for local-path images and for the explicit
`shed image pull` command. Avoid mutable tags (`:latest`) with `missing` —
pin versioned tags so a cache hit is always the right image.

## Using image aliases

```bash
shed create myproject --image full        # batteries-included
shed create myproject --image extensions  # credential brokering, no agents
shed create tools --image base            # minimal
shed create app --image ghcr.io/you/custom:v1   # any ref directly
```

Default (no `--image` flag) uses `default_image`:

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
FROM ghcr.io/charliek/shed-vz-extensions:vX.Y.Z

USER shed
ENV PATH="/home/shed/.local/bin:${PATH}"

RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y
ENV PATH="/home/shed/.cargo/bin:${PATH}"

RUN curl -fsSL https://claude.ai/install.sh | bash

USER root
WORKDIR /home/shed
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

`shed image build`'s default Dockerfile path shells out to
`docker buildx`. To build without a Docker daemon, produce an OCI
image-layout tar with any tool (podman, buildah, nix-build, etc.) and
pass it to `shed image build --from-oci-archive <path>` — the
downstream ingestion (layer install, manifest annotations, initramfs
injection, tag advancement) is pure Go. See
[Build your own image § 2a](../tutorials/build-your-own-image.md#2a-alternative-building-without-docker)
for the workflow.

`shed image pull` is already docker-free — it uses
[`go-containerregistry`](https://github.com/google/go-containerregistry)
directly.

### Kernel version pinning

`initramfs/Dockerfile` and `vz/Dockerfile` both install an Ubuntu
kernel package — the initramfs to stage `erofs.ko` + `libcrc32c.ko`,
the VZ rootfs to ship the `vmlinuz` shed extracts at image-publish
time. The two must target the **same** kernel version: VZ kernels
don't have erofs built-in, so the initramfs's staged `.ko` files
must match the booted `vmlinuz` ABI exactly or the in-guest
overlay mount fails with `SHED-INIT-03`.

Pre-v0.5.8 both stages installed the `linux-image-virtual`
metapackage without pinning. GitHub Actions builds were safe (each
runner has a fresh BuildKit cache, so both stages see the same apt
snapshot), but iterative local builds via `./scripts/build-vz-rootfs.sh`
could pull two different apt snapshots out of BuildKit's cache and
produce an unbootable image with kernel-version skew between the
initramfs and the rootfs.

v0.5.8+ pins via `ARG LINUX_IMAGE_VERSION` in both Dockerfiles:

| File | What the ARG controls |
|---|---|
| `initramfs/Dockerfile` | Package the initramfs stages `erofs.ko` + `libcrc32c.ko` from. |
| `vz/Dockerfile` | Package the VZ rootfs installs as the in-guest kernel. |

`make check-kernel-pin` (wired into `make check`) fails the build
if the two values drift apart. Bump both in lockstep when picking
up a new Ubuntu kernel:

```bash
# Both lines must declare the same package version.
grep '^ARG LINUX_IMAGE_VERSION=' initramfs/Dockerfile vz/Dockerfile
```

Firecracker's rootfs uses the custom `KERNEL_TAG`-built kernel
(`firecracker/Dockerfile` `kernel-builder` stage) and does **not**
install `linux-image-virtual` — so it's not covered by this pin.
FC's initramfs IS the same artifact `initramfs/Dockerfile` builds,
so the FC initramfs path is covered by `LINUX_IMAGE_VERSION`
automatically; FC's custom kernel has erofs built-in, so the .ko
load is non-load-bearing on FC anyway.

After bumping the pin, validate locally:

```bash
docker buildx prune --all --force
./scripts/build-vz-rootfs.sh --variant base
# Then boot a shed against the rebuilt blob and verify uname -r.
```

See `docs/upgrades/v0.5.7-to-v0.5.8.md` for the original SHED-INIT-03
incident report.

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

`shed image prune` walks every tag, shed, and snapshot, collects the
manifest digests they pin (`tags/<name>.json`,
`instances/<name>/metadata.json` → `lower_digest`,
`snapshots/<name>/snapshot.json` → `lower_digest`), expands each
manifest to the layers it references, and deletes any blob or cached
ext4 not in that reachable set. Tags ARE protective as of v0.5.8 —
pre-v0.5.8 they were informational and prune deleted blobs they
pointed at (see [v0.5.7 → v0.5.8](../upgrades/v0.5.7-to-v0.5.8.md)).

Deleting a tag for an image that's still pinned by a running or stopped
shed is safe — the shed boots from the digest pinned in its metadata,
not the tag.

### Cookbook: upgrading image versions

For the full step-by-step (with Linux/.deb + macOS/brew variants and
local-image-build cache cleanup), see the
[v0.5.7 → v0.5.8 upgrade guide](../upgrades/v0.5.7-to-v0.5.8.md#image-cache-cleanup-playbook).
Same shape applies to every shed version bump:

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
5. Drop any stale tags you added by hand (the configured tags
   advance in place during step 4 and are not stale).
   ```bash
   shed image rm <stale-experimental-tag>
   ```
6. Reclaim unreferenced blobs.
   ```bash
   shed image prune --dry-run
   shed image prune --force
   ```
7. Verify.
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

Host-backed `--local-dir`/`--add-dir` mounts (bound under
`/home/shed/<basename>`) are unaffected. Data that lived only inside the
deleted upper layers — including a cloned `--repo` and anything written
under `/home/shed` — is lost, by design: the upper layer is per-shed
scratch space, not durable storage.

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
