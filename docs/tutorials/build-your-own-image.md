# Build Your Own Image

This tutorial walks through deriving a custom shed image from the
published `extensions` variant, pushing it to a private registry, and
booting a shed from it.

We'll build an image with [uv](https://docs.astral.sh/uv/) (Python
package manager), Python 3.13, and [OpenCode](https://opencode.ai)
preinstalled.

## Why start from `extensions`?

The three published variants are:

| Variant | Contents | Recommended use |
|---|---|---|
| `base` | OS layer only. systemd, SSH, git, build tools, the shed-agent. No Docker. | Tiny images. You'll wire up coding agents and credentials yourself. |
| `extensions` | `base` + [credential brokering](../reference/extensions.md). No Docker daemon, no coding agents pinned. | **Custom images.** Credentials work out of the box; you choose the runtime/agent set. |
| `full` | `extensions` + Docker CE (with `docker compose`), mise, bun, and coding agents (Claude Code, OpenCode, Codex CLI). | Default for `shed create`. |

`extensions` is the right starting point because:

- Credential brokering is preconfigured — SSH agent forwarding, AWS STS,
  and Docker registry auth work without further setup.
- No coding agents are pinned, so you choose versions yourself without
  fighting `full`'s defaults.
- Building on a published variant means the shed-overlay boot setup,
  kernel/initrd, and credential brokering are already in place — you only
  add your own top layers.

## Prerequisites

- A host with Docker installed (for `docker buildx`).
- `shed` and `shed-server` already set up. See
  [VZ Setup](../getting-started/vz-setup.md) or
  [Firecracker Setup](../getting-started/fc-setup.md).
- A private registry you can push to. We'll use
  `ghcr.io/myorg/my-shed-image:v1` in examples — substitute your own.

## 1. Write the Dockerfile

Save the following as `Dockerfile.shed` in an empty directory:

```dockerfile
# Pick the variant matching your backend platform.
# VZ (macOS arm64): shed-vz-extensions
# Firecracker (Linux amd64): shed-fc-extensions
ARG SHED_VERSION=v0.5.1
FROM ghcr.io/charliek/shed-vz-extensions:${SHED_VERSION}

USER root

# Install uv (system-wide, statically linked).
RUN curl -fsSL https://astral.sh/uv/install.sh \
  | env UV_INSTALL_DIR=/usr/local/bin sh

# Install Python 3.13 via uv (system-wide).
RUN /usr/local/bin/uv python install 3.13 --default --preview

# Install OpenCode as the shed user (writes to /home/shed/.local).
USER shed
RUN curl -fsSL https://opencode.ai/install | bash
ENV PATH="/home/shed/.local/bin:${PATH}"

# Tag the derived layer so it shows up in `shed image inspect`.
LABEL io.shed.variant=myorg-python-opencode

# Mandatory shed boot setup — keep these.
USER root
WORKDIR /home/shed
ENTRYPOINT ["/sbin/init"]
```

Notes on the constraints:

- **`FROM ghcr.io/charliek/shed-vz-extensions`** (or `shed-fc-extensions`
  for Firecracker) — the layer below yours must be a shed-managed
  variant. Other base images will not boot.
- **Do not remove the `shed` user (UID 1000)** — much of the shed-agent
  and credential brokering assumes it exists.
- **Do not disable `shed-agent.service`** — it's how the host talks to
  the guest.
- **`ENTRYPOINT ["/sbin/init"]`** is mandatory. Shed boots via systemd.

## 2. Build the image

Use `docker buildx` with the right platform for your backend
(`linux/arm64` for VZ, `linux/amd64` for Firecracker):

```bash
# VZ (macOS arm64)
docker buildx build \
  --platform linux/arm64 \
  -t ghcr.io/myorg/my-shed-image:v1 \
  -f Dockerfile.shed \
  --load \
  .

# Firecracker (Linux amd64)
docker buildx build \
  --platform linux/amd64 \
  -t ghcr.io/myorg/my-shed-image:v1 \
  -f Dockerfile.shed \
  --load \
  .
```

`--load` makes the image available to the local Docker daemon so the
next step can push it.

## 2a. Alternative: building without Docker

`shed image build` drives `docker buildx` under the hood. If you don't
want a Docker daemon on your build host, any tool that emits an OCI
image-layout tar will work — shed ingests it via
`shed image build --from-oci-archive`. The downstream pipeline
(layer ingestion into the blob store, initramfs annotation, source-ref
annotation) is pure Go and runs identically regardless of which tool
produced the archive.

**Build with podman** (rootless on Linux, no daemon):

```bash
# Linux/amd64 → Firecracker; macOS/arm64 → VZ
podman build \
  --platform linux/arm64 \
  --output type=oci,dest=my-shed-image.tar \
  -f Dockerfile.shed \
  .
```

**Build with buildah** (rootless on Linux, no daemon, no Podman):

```bash
buildah bud \
  --platform linux/arm64 \
  --output type=oci-archive,dest=my-shed-image.tar \
  -f Dockerfile.shed \
  .
```

**Anything else that emits an OCI archive** works too — nix-build
piped through skopeo, a custom Go binary using
`github.com/google/go-containerregistry`, even hand-assembled tarballs
that conform to OCI image-layout-v1.

Then ingest into shed's local store:

```bash
# Build the shed-overlay initramfs (image-content-independent — one per
# machine suffices, the same artifact works for every derived image).
./scripts/build-initramfs.sh --backend vz --platform linux/arm64 --output /tmp/shed-initrd.img

# Ingest the OCI archive. shed handles the initramfs blob injection,
# manifest annotations, source-ref, kernel extraction, and tag
# advancement — same as the docker path, just without docker.
shed image build \
    --from-oci-archive my-shed-image.tar \
    -n my-shed-image \
    --initramfs /tmp/shed-initrd.img \
    --source-ref ghcr.io/myorg/my-shed-image:v1
```

`--from-oci-archive` is mutually exclusive with `--file`, `--target`,
`--platform`, and `--force` (those are Dockerfile-mode options;
ingesting a pre-built archive has nothing to drive). `--initramfs`,
`--name`, `--source-ref`, and `--output-dir` apply to both modes.

From here the rest of the flow (push, pull, boot) is identical to the
Docker path below.

**What still needs Docker.** Today shed-server itself doesn't need
Docker for anything in the boot/image lifecycle: `pull`, `push`,
`save`, `load`, `inspect`, `history`, `ls`, `prune`, the
materialize step on `shed create`, snapshots, and all VM ops are
Docker-free. The remaining Docker dep is only `shed image build`
when invoked without `--from-oci-archive`. With this flag, a
cloud-VPS or rootless-Linux host can run shed-server end-to-end
without installing Docker CE.

## 3. Push to a private registry

You have two options.

**Option A — `docker push`** (uses your existing Docker credentials):

```bash
docker login ghcr.io
docker push ghcr.io/myorg/my-shed-image:v1
```

**Option B — `shed image push`** (registry-direct, byte-perfect):

```bash
# Build the shed-overlay initramfs (shared across variants — once
# per machine is enough, the artifact is image-content-independent).
./scripts/build-initramfs.sh --backend vz --platform linux/arm64 --output /tmp/shed-initrd.img

# Import the just-built rootfs into shed's local OCI store.
shed image build \
    -f Dockerfile.shed \
    -n my-shed-image \
    --initramfs /tmp/shed-initrd.img \
    .

# Then push from there. --local skips the HTTP API hop to a running
# shed-server, which is handy from a build host without one running.
shed image push --local my-shed-image ghcr.io/myorg/my-shed-image:v1
```

`shed image push` is byte-perfect: the manifest digest at the
destination equals the local manifest digest, so any host that later
pulls `ghcr.io/myorg/my-shed-image:v1` resolves to the same digest you
just pushed. The shed-overlay initramfs is uploaded as a sibling blob
referenced by an `io.shed.initrd.digest` annotation, so the receiving
host doesn't need to rebuild it locally.

## 4. Pull on the shed server

On any machine running `shed-server`:

```bash
shed image pull ghcr.io/myorg/my-shed-image:v1 -t my-image
```

`shed image pull` is registry-direct — no Docker daemon required on
the shed server. Authentication uses
`~/.docker/config.json` and any installed credential helpers (including
the Docker credential proxy extension when an `extensions`-based
shed is running).

The pull is **boot-only** by default: the host boots from the prebuilt
erofs blob, so the layer tarballs aren't downloaded (roughly half the
bytes — see [boot-only pulls](../reference/cli.md#boot-only-by-default)).
That's the right default for a host that just *boots* your image. The
layers are only needed to byte-perfect **re-push a pulled image**; if you
ever need that on this host, re-pull with `--with-layers`.

Verify the manifest:

```bash
shed image inspect my-image
```

You should see roughly 9–11 layers in the manifest: your `RUN curl … uv …`,
`RUN curl … opencode …`, the `LABEL` and any other top-of-Dockerfile
instructions you added, then the 7-or-so layers from the parent
`extensions` variant. `shed image ls` shows the image under `LAYERS` as
`boot-only` (the layer *blobs* weren't downloaded) and its `SIZE` is the
on-disk footprint (erofs + kernel + initrd), not the full layer set.

## 5. Boot a shed from it

```bash
shed create devbox --image my-image
shed console devbox
```

Inside the shed:

```bash
python --version       # 3.13.x
uv --version
opencode --version
```

## 6. (Optional) Make it your default

To make `shed create` (without `--image`) use your derived image by
default, edit your server config:

```yaml
vz:
  default_image: my-image        # a local label (-t), or the full ghcr.io ref
  image_aliases:
    base: ghcr.io/charliek/shed-vz-base:v0.5.1
    extensions: ghcr.io/charliek/shed-vz-extensions:v0.5.1
    full: ghcr.io/charliek/shed-vz-full:v0.5.1
    my-image: ghcr.io/myorg/my-shed-image:v1
  pull_policy: missing
```

Restart the server. Subsequent `shed create <name>` calls without
`--image` will boot from your custom layer set.

## Iterating

To rebuild after editing the Dockerfile:

```bash
docker buildx build --platform linux/arm64 -t ghcr.io/myorg/my-shed-image:v2 -f Dockerfile.shed --load .
docker push ghcr.io/myorg/my-shed-image:v2
shed image pull ghcr.io/myorg/my-shed-image:v2 -t my-image
```

Existing sheds keep booting from the digest they were created against —
tag updates don't propagate to running sheds (see
[Storage Model → Re-pulling a ref doesn't propagate](../reference/storage-model.md#re-pulling-a-ref-doesnt-propagate-to-existing-sheds)).
To roll a shed onto the new image, `shed delete devbox` and recreate.

## See also

- [Images](../reference/images.md) — the full variant lineup,
  annotations, and the boot stack.
- [Storage Model](../reference/storage-model.md) — how the OCI store
  is laid out on disk.
- [CLI Reference](../reference/cli.md#image-management) — every
  `shed image` subcommand.
