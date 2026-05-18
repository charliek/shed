# Storage Model

Shed uses an [OCI image-layout-v1](https://github.com/opencontainers/image-spec/blob/main/image-layout.md)
store on disk. Layers, manifests, and configs are addressed by their
sha256 digests, exactly like a container registry — which means
`shed image push` is a byte-perfect upload and `shed image pull` works
without a Docker daemon.

## Layout

For each VM backend, all on-disk state lives under a single `images_dir`:

```text
{images_dir}/
  oci-layout                                    # {"imageLayoutVersion":"1.0.0"}
  index.json                                    # OCI image index (manifest references)
  blobs/sha256/<hex>                            # FILES, not dirs — OCI blobs:
                                                #   manifests, configs, layer tar.gz
  cache/sha256/<hex>.ext4                       # derived per-layer ext4 (lazy)
  tags/<tag>.json                               # {"digest":"sha256:...","updated_at":"..."}
  uppers/<shed>/upper.ext4                      # per-shed writable overlay upper
  instances/<shed>/metadata.json                # per-shed bookkeeping
  snapshots/<snap>/snapshot.json                # per-snapshot bookkeeping
```

For Firecracker the default is `/var/lib/shed/firecracker/images/`; for
VZ it's `~/Library/Application Support/shed/vz/`.

The key shift from earlier versions:

- **`blobs/sha256/<hex>` is a flat file**, not a directory. It contains
  the raw OCI blob — a manifest JSON, an image config, or a gzipped tar
  layer.
- **`cache/sha256/<hex>.ext4` is the derived form** of a layer blob,
  built lazily the first time a layer is needed for boot. The blob is
  the canonical form (so `shed image push` round-trips byte-perfectly);
  the ext4 is the boot-fast form.

## Concepts, mapped to Docker

| Shed concept | Docker analog |
|---|---|
| **Manifest blob** — JSON file at `blobs/sha256/<hex>` whose media type is `application/vnd.oci.image.manifest.v1+json` | Image manifest |
| **Config blob** — JSON file in `blobs/sha256/<hex>` referenced by the manifest | Image config |
| **Layer blob** — gzipped tar at `blobs/sha256/<hex>` | Image layer |
| **Cached layer ext4** — `cache/sha256/<hex>.ext4` derived from a layer | (no direct analog — boot-time cache) |
| **Tag** — `tags/<name>.json` pointing at a manifest digest | Image tag |
| **Dangling manifest** — manifest with no tag and no shed/snapshot reference | `<none>:<none>` image |

Two tags can point at the same manifest digest, and two manifests can
share layer blobs — both forms of sharing are zero extra disk cost.

## How a shed pins an image

When `shed create --image extensions` runs, the server:

1. Resolves the `extensions` tag to a manifest digest via
   `tags/extensions.json`.
2. Writes `instances/<name>/metadata.json` with
   `"lower_digest": "sha256:<manifest-digest>"`, `"schema_version": 3`,
   and the list of layer digests captured at create time.
3. Materializes any missing layer ext4s into `cache/sha256/`.
4. Creates the per-shed upper at `uppers/<name>/upper.ext4`.

Subsequent `shed start` reads the same metadata. The shed boots from the
exact manifest it was created against — re-tagging `extensions` to a new
digest after the fact does not change what an existing shed boots.

## Reachability and prune

`shed image prune` walks reachability rather than refcounting:

1. **Seed set** — for every `instances/*/metadata.json` and
   `snapshots/*/snapshot.json`, collect the pinned `lower_digest` and
   the layer digests.
2. **Expand** — for every manifest digest in the seed set, parse the
   manifest and add its config blob and layer blob digests.
3. **Sweep** — delete any blob in `blobs/sha256/` not in the reachable
   set, and any `cache/sha256/<hex>.ext4` whose corresponding layer blob
   is unreachable.

Tags do **not** protect blobs. Following the Docker model,
`shed image rm <tag>` only removes the tag — the manifest and its layers
stay until prune walks them out.

Stopped sheds count as references. In-flight creates protect their
target manifest for up to 1 hour via a `.creating` marker in
`instances/<shed>/`; after that, a crashed-create marker stops
protecting.

## 1.4× per-layer overhead

Keeping both the gzipped tar layer (canonical) and the derived ext4
(boot-fast) on disk costs roughly 1.4× the size of the ext4 alone for
typical content (tar.gz ≈ 0.4× ext4). The trade-off:

- Pulls and pushes are byte-perfect — the manifest digest at the source
  equals the manifest digest at the destination.
- Boots are fast — no on-demand tar extraction.
- `shed image inspect` matches `docker manifest inspect` for the same
  reference.

See [Layer storage optimization](../discovery/layer-storage-optimization.md)
for the design notes on reducing this overhead in future versions
(squashfs/erofs lowers, cache eviction, mkfs.ext4 tuning).

## Initramfs panic codes

If the in-guest initramfs cannot boot a shed it panics with a numbered
code. Quick reference:

| Code | Hint |
|---|---|
| `SHED-INIT-02` | Missing `shed.lower.N=` directive on kernel cmdline |
| `SHED-INIT-03` | Layer device absent in the guest |
| `SHED-INIT-04` | Layer ext4 superblock check failed |
| `SHED-INIT-05` | Upper ext4 corrupt; recover with `shed reset <name>` |
| `SHED-INIT-06` | overlayfs mount returned `-EINVAL` |
| `SHED-INIT-07` | Pivot into `/sysroot` failed |
| `SHED-INIT-08` | More than `MaxLayers` (16) layers declared |
| `SHED-INIT-09` | Schema-version mismatch (expected v3) |

See [Image Variants → Boot stack](images.md#boot-stack) for the full
description of each code.

## Atomicity and concurrency

Blob install is atomic:

1. Stream the blob into `blobs/sha256/<hex>.tmp`.
2. `fsync` the file.
3. `rename` to `blobs/sha256/<hex>`; `fsync` the parent dir.

Tag advancement follows the same pattern on `tags/<name>.json`. Cache
materialization is similarly atomic on `cache/sha256/<hex>.ext4`.

Concurrent installs of the same blob are serialized by a flock on
`blobs/sha256/.<hex>.lock`. Concurrent `EnsureImage` calls for the same
tag take a flock on `tags/<name>.lock`. Concurrent ext4 materializations
of the same layer take a flock on `cache/sha256/.<hex>.lock`.

## Lifecycle commands

| Command | What it does |
|---|---|
| `shed image build [-t <tag>] [--builder docker]` | Build a Dockerfile to OCI, install + tag |
| `shed image pull <ref> [-t <tag>] [--platform <os/arch>]` | Pull a registry ref directly into the store |
| `shed image push <src> <dst>` | Push a tag or digest to a registry, byte-perfect |
| `shed image save <tag> -o <file>` | Write a tag to an OCI archive (for air-gap transport) |
| `shed image load -i <file>` | Load an OCI archive into the store |
| `shed image ls` | List tags + dangling manifests |
| `shed image history <tag>` | List layers (top-down) for a manifest |
| `shed image inspect <tag-or-digest>` | Show manifest + annotations + cached path |
| `shed image tag <src> <new>` | Point a new tag at an existing digest |
| `shed image rm <tag>` | Remove a tag (blobs persist for prune to GC) |
| `shed image prune` | Reachability-sweep unreachable blobs and cached ext4s |

`shed create --image <tag>` resolves through the tag → manifest digest →
layer set chain and materializes any missing ext4 caches.

## Tag updates don't propagate to existing sheds

A shed's metadata pins the manifest digest its lower was resolved from
at create time, not the tag string. After
`shed image pull <ref> -t full` advances the `full` tag to a new digest,
existing sheds keep booting from the old digest. `shed stop && shed start`
re-reads the metadata and does not re-resolve the tag. This is
intentional: live re-resolution would change the read-only lowers out
from under a running guest. To roll a shed onto new content,
`shed delete <name>` and `shed create <name> --image full`.

## Why this matters

- **Registry-native.** The store is a registry on disk. `crane` and
  similar tools work against it directly.
- **Daemon-free pull.** No Docker required for `shed image pull` —
  `shed-server pull-images` runs on cloud VPSes without installing
  Docker.
- **Byte-perfect push.** `shed image push <local-tag> <remote-ref>`
  produces a manifest at the destination whose digest matches the local
  manifest digest.
- **Layer sharing across variants.** `base`, `extensions`, and `full`
  share base layers; pulling all three is barely more than pulling
  `full`.
- **Reachability-based GC.** Prune walks from the live shed and
  snapshot set, so unreferenced layers go away even if they were once
  tagged.
- **Per-shed cost is the upper alone.** Read-only layers are shared
  across every shed pinning the same manifest; host filesystem reflink
  support is no longer load-bearing.

See also: [Image Variants](images.md),
[Disk Management](disk-management.md), [Snapshots](snapshots.md),
[Layer storage optimization](../discovery/layer-storage-optimization.md).
