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
| `base` | OS layer only. systemd, SSH, Docker, the shed-agent. | Tiny images. You'll wire up coding agents and credentials yourself. |
| `extensions` | `base` + [shed-extensions](https://charliek.github.io/shed-extensions/) credential brokering. No coding agents pinned. | **Custom images.** Credentials work out of the box; you choose the runtime/agent set. |
| `full` | `extensions` + Node.js LTS, Python 3.13, Claude Code, OpenCode, Codex CLI. | Default for `shed create`. |

`extensions` is the right starting point because:

- Credential brokering is preconfigured — SSH agent forwarding, AWS STS,
  and Docker registry auth work without further setup.
- No coding agents are pinned, so you choose versions yourself without
  fighting `full`'s defaults.
- You get layer sharing with `extensions` and `full` for free: anyone
  who's already pulled either variant only fetches the top layer of
  your derived image.

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
ARG SHED_VERSION=v0.5.0
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
WORKDIR /workspace
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
the shed-extensions Docker credential proxy when an `extensions`-based
shed is running).

Verify the layer stack:

```bash
shed image history my-image
```

You should see three layers (or four if the Python install was split):
your top layer, then the `extensions` and `base` layers from the
parent.

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
  base_rootfs: my-image          # or the full ghcr.io ref
  images:
    base: ghcr.io/charliek/shed-vz-base:v0.5.0
    extensions: ghcr.io/charliek/shed-vz-extensions:v0.5.0
    full: ghcr.io/charliek/shed-vz-full:v0.5.0
    my-image: ghcr.io/myorg/my-shed-image:v1
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
[Storage Model → Tag updates](../reference/storage-model.md#tag-updates-dont-propagate-to-existing-sheds)).
To roll a shed onto the new image, `shed delete devbox` and recreate.

## See also

- [Image Variants](../reference/images.md) — the full variant lineup,
  annotations, and the boot stack.
- [Storage Model](../reference/storage-model.md) — how the OCI store
  is laid out on disk.
- [CLI Reference](../reference/cli.md#image-management) — every
  `shed image` subcommand.
