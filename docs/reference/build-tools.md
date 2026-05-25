# shed-build-tools

`shed-build-tools` is an OCI image carrying the binaries shed uses
when it publishes a shed-fc / shed-vz image. It exists so the on-disk
format of every published shed image is determined by a pinned tool
version, not by whatever the publishing host's distro happens to
ship.

## What's in the image

| Binary | Source | Used for |
| --- | --- | --- |
| `mkfs.erofs` | [erofs-utils](https://github.com/erofs/erofs-utils) | Building the read-only rootfs blob inside shed-fc / shed-vz images. |
| `dump.erofs` | erofs-utils | Inspecting an erofs blob (development / debugging). |
| `fsck.erofs` | erofs-utils | Verifying erofs blobs in CI / smoke tests. |

The `mkfs.erofs` binary is the headline reason this image exists.
Distro-shipped versions have at times produced filesystems no kernel
can read (e.g., erofs-utils 1.7.1 emits per-inode big-pcluster
headers without the superblock feature flag when invoked with
`-E force-inode-compact`). Pinning the version ensures every shed
release's published images use a known-good writer.

## Versioning

Every shed-server release publishes a matching `shed-build-tools` tag:

```
ghcr.io/charliek/shed-build-tools:v0.5.2
ghcr.io/charliek/shed-build-tools:v0.5.3
...
```

The image content rarely changes — only when the pinned upstream
tool versions bump or when new tools land. But every shed release
gets a tag so consumers can pin to the same version they're already
pinning shed-server to. Identical content across releases is
deduplicated in the registry.

The shed-build-tools image is currently versioned in lockstep with
shed-server because both ship out of this repository and the release
cadences are coupled. If/when shed-build-tools stabilizes and stops
needing per-release rebuilds, it may move to an independent tag
stream — until then, the lockstep model keeps the mental model
small.

## Bumping the pinned `erofs-utils` version

1. Pick a release at <https://github.com/erofs/erofs-utils/tags>.
2. Edit `build-tools/Dockerfile` and change the `ARG EROFS_UTILS_VERSION` line.
3. Build locally: `make build-tools`.
4. Sanity check that the new binary writes a filesystem the consumer
   kernel can actually read — see `build-tools/README.md` for the
   exact one-liner.
5. Commit the bump. The new version rides the next shed release.

There is no out-of-band release for `shed-build-tools` — it only
publishes alongside a shed-server tag.

## Adding new tools to the image

Tools that the shed image-build / publish pipeline calls should live
here when (a) the on-disk format they produce matters for
reproducibility, or (b) their host version varies enough across
distros to cause issues. Examples of tools that fit: a custom kernel
build environment, an initramfs builder, a squashfs writer if we
ever add one. Examples that don't: shed's own Go binaries (those
ship in the shed-server release artifacts directly), generic CLI
utilities already available everywhere.

When adding a tool:

1. Append it to `build-tools/Dockerfile` (preferably in the existing
   builder stage; copy the resulting binary into the runtime stage).
2. Document it in `build-tools/README.md` under "What's in the image".
3. List the consumer file(s) in `build-tools/README.md` so future
   binary renames / entrypoint changes can be traced.

## Local development

```sh
make build-tools                              # builds shed-build-tools:dev
docker run --rm shed-build-tools:dev --help   # mkfs.erofs help (default entrypoint)
```

When iterating on a consumer that hasn't been released yet, point
the consumer's `BUILD_TOOLS_VERSION` build arg at `dev` and it will
pick up your local image.

If you want to test against a yet-unreleased upstream version,
publish a temporary tag (`ghcr.io/charliek/shed-build-tools:dev-foo`),
run the consumer against it, then delete the tag with
`gh api --method DELETE /user/packages/container/shed-build-tools/versions/<id>`
once you're done. Avoid leaving long-lived `dev-*` tags on ghcr.
