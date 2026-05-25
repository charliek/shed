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
                                                #   manifests, configs, layer tar.gz,
                                                #   kernel, initrd, rootfs erofs
  tags/<tag>.json                               # {"digest":"sha256:...","updated_at":"..."}
  uppers/<shed>/upper.ext4                      # per-shed writable overlay upper
  instances/<shed>/metadata.json                # per-shed bookkeeping
  snapshots/<snap>/snapshot.json                # per-snapshot bookkeeping
```

For Firecracker the default is `/var/lib/shed/firecracker/images/`; for
VZ it's `~/Library/Application Support/shed/vz/`.

**Everything in `blobs/sha256/<hex>` is a flat file**, not a directory.
The blob can be a manifest JSON, an image config, a gzipped tar layer,
a raw kernel, a raw initrd, or a raw erofs filesystem. All are
deduplicated by sha256 — the `apt-get install` layer is one blob no
matter how many manifests reference it.

The read-only rootfs the VM mounts at `/dev/vdb` is the erofs blob
referenced by the manifest's `io.shed.rootfs.erofs.digest` annotation.
It's built once at image-publish time by `mkfs.erofs` inside the
[shed-build-tools container](build-tools.md) (pinned `erofs-utils`),
shipped as a content-addressed OCI blob, and downloaded verbatim by
every host. The on-host pull path does not invoke `mkfs.erofs`.

Through v0.5.1 the erofs was built lazily on the host into a separate
`cache/sha256/<manifest-digest>.erofs` directory. That directory is no
longer used; older installs may still have one and can `rm -rf` it.
See the [v0.5.1 → v0.5.2 upgrade guide](../upgrades/v0.5.1-to-v0.5.2.md).

## Concepts, mapped to Docker

| Shed concept | Docker analog |
|---|---|
| **Manifest blob** — JSON file at `blobs/sha256/<hex>` whose media type is `application/vnd.oci.image.manifest.v1+json` | Image manifest |
| **Config blob** — JSON file in `blobs/sha256/<hex>` referenced by the manifest | Image config |
| **Layer blob** — gzipped tar at `blobs/sha256/<hex>` | Image layer |
| **Rootfs erofs blob** — raw erofs at `blobs/sha256/<hex>` referenced by `io.shed.rootfs.erofs.digest` | (no direct analog — read-only root the VM mounts) |
| **Kernel / initrd blobs** — raw binaries at `blobs/sha256/<hex>` referenced by `io.shed.kernel.digest` / `io.shed.initrd.digest` | (no direct analog — VM boot artifacts) |
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
3. Looks up the manifest's `io.shed.rootfs.erofs.digest` annotation
   and resolves it to a blob path under `blobs/sha256/`. The blob IS
   the read-only lower the VM mounts — no host-side `mkfs.erofs`.
4. Creates the per-shed upper at `uppers/<name>/upper.ext4`.

Subsequent `shed start` reads the same metadata. The shed boots from the
exact manifest it was created against — re-tagging `extensions` to a new
digest after the fact does not change what an existing shed boots.

## Reachability and prune

`shed image prune` walks reachability rather than refcounting:

1. **Seed set** — for every `instances/*/metadata.json` and
   `snapshots/*/snapshot.json`, collect the pinned `lower_digest` (the
   manifest digest) and the layer digests.
2. **Expand** — for every manifest digest in the seed set, parse the
   manifest and add its config blob and layer blob digests.
3. **Sweep** — delete any blob in `blobs/sha256/` not in the reachable
   set. (The reachable set includes layer blobs, manifest configs, the
   kernel / initrd loose blobs, and the rootfs erofs blob via their
   respective annotations.)

Tags do **not** protect blobs. Following the Docker model,
`shed image rm <tag>` only removes the tag — the manifest and its layers
stay until prune walks them out.

Stopped sheds count as references. In-flight creates protect their
target manifest for up to 1 hour via a `.creating` marker in
`instances/<shed>/`; after that, a crashed-create marker stops
protecting.

## Disk overhead

The flattened erofs lower lands around 0.5–0.7× the equivalent
uncompressed ext4 (lz4 compression), and the gzipped layer tar.gz blobs
together are roughly the same. Total cost for a manifest is:

- Layer blobs (canonical, deduplicated across manifests)
- One flattened erofs (per-manifest, not deduplicated)

In practice keeping both forms costs ~1.0–1.3× the equivalent ext4
alone for typical Ubuntu-rootfs content. The trade-off:

- Pulls and pushes are byte-perfect — the manifest digest at the source
  equals the manifest digest at the destination.
- Boots are fast — no on-demand tar extraction; mount-and-go.
- `shed image inspect` matches `docker manifest inspect` for the same
  reference.

See [Layer storage optimization](../discovery/layer-storage-optimization.md)
for design notes on reducing this further (composefs, blob-level shared
content, cache eviction).

## Initramfs panic codes

If the in-guest initramfs cannot boot a shed it panics with a numbered
code. Quick reference:

| Code | Hint |
|---|---|
| `SHED-INIT-02` | Lower block device absent (`/dev/vdb` didn't appear) |
| `SHED-INIT-03` | erofs mount of `/dev/vdb` failed (corrupt or wrong block size) |
| `SHED-INIT-04` | overlay mount failed (kernel module / lowerdir issue) |
| `SHED-INIT-05` | Upper block device absent (`/dev/vda`) |
| `SHED-INIT-06` | Upper has no ext4 magic and no fresh-upper signature; corrupt — `shed reset <name>` |
| `SHED-INIT-07` | `mkfs.ext4` on the upper failed |
| `SHED-INIT-08` | Mounting the upper as ext4 failed |
| `SHED-INIT-09` | `switch_root` failed (should never reach the user) |

See [Image Variants → Boot stack](images.md#boot-stack) for the full
description of each code.

## Atomicity and concurrency

Blob install is atomic:

1. Stream the blob into `blobs/sha256/<hex>.tmp`.
2. `fsync` the file.
3. `rename` to `blobs/sha256/<hex>`; `fsync` the parent dir.

Tag advancement follows the same pattern on `tags/<name>.json`.

Concurrent installs of the same blob are serialized by a flock on
`blobs/sha256/.<hex>.lock`. Concurrent `EnsureImage` calls for the same
tag take a flock on `tags/<name>.lock`. Since v0.5.2 ships the erofs as
a content-addressed blob there's no separate cache materialization
lock — `EnsureImage` is pure pull + path resolution.

## Lifecycle commands

| Command | What it does |
|---|---|
| `shed image build [-t <tag>]` | Build a Dockerfile to OCI, install + tag |
| `shed image pull <ref> [-t <tag>] [--platform <os/arch>]` | Pull a registry ref directly into the store |
| `shed image push <src> <dst>` | Push a tag or digest to a registry, byte-perfect |
| `shed image save <tag> -o <file>` | Write a tag to an OCI archive (for air-gap transport) |
| `shed image load -i <file>` | Load an OCI archive into the store |
| `shed image ls` | List tags + dangling manifests |
| `shed image history <tag>` | List layers (top-down) for a manifest |
| `shed image inspect <tag-or-digest>` | Show manifest + annotations + cached path |
| `shed image tag <src> <new>` | Point a new tag at an existing digest |
| `shed image rm <tag>` | Remove a tag (blobs persist for prune to GC) |
| `shed image prune` | Reachability-sweep unreachable blobs and flattened erofs files |

`shed create --image <tag>` resolves through the tag → manifest digest
chain and materializes the flattened erofs lower lazily if it's not
already cached.

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
