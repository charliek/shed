# Storage Model

Shed uses a content-addressed image store with tag indirection — the
same model container runtimes use. This page explains the layout, why
it's structured this way, and how it interacts with sheds and snapshots.

## Layout

For each VM backend, all on-disk state lives under a single `images_dir`:

```text
{images_dir}/
  blobs/sha256/<digest>/
    rootfs.ext4         mode 0444   read-only image content
    kernel              mode 0444   extracted boot kernel (when present)
    initrd              mode 0444   extracted initrd (the shed-built initramfs)
    manifest.json       mode 0444   image metadata
  tags/
    <tag>.json                      {"digest": "sha256:...", "updated_at": "..."}
  uppers/
    <shed-name>/upper.ext4          per-shed writable upper (sparse)
  instances/
    <shed-name>/metadata.json       per-shed bookkeeping
  snapshots/
    <snap-name>/{rootfs.ext4,snapshot.json}    # rootfs.ext4 holds the captured upper

```

For Firecracker the default is `/var/lib/shed/firecracker/images/`; for
VZ it's `~/Library/Application Support/shed/vz/`.

Per-shed rootfs files and console logs live alongside under
`instances/`; snapshots live under `snapshots/`.

## Concepts, mapped to Docker

| Shed concept | Docker analog |
|---|---|
| **Blob** — `blobs/sha256/<digest>/rootfs.ext4` | Image layer / image content |
| **Digest** — `sha256:<64-hex>` over `rootfs.ext4` bytes | Image ID |
| **Tag** — `tags/<name>.json` pointing at a digest | Image tag (`my-image:latest`) |
| **Manifest** — `blobs/.../manifest.json` | Image manifest |
| **Dangling blob** — installed but no tag | `<none>:<none>` image |

A blob is *content*. A tag is a *name* that points at content. Two tags
can point at the same digest with no extra disk cost. A blob with no
tag is *dangling* and a candidate for prune.

## Identity, atomicity, and concurrency

The digest is `sha256(rootfs.ext4)`. Conversion runs on a per-call
staging directory; the file is hashed before install, so a partially
written rootfs never gets a digest.

Install is atomic:

1. Stage files into `blobs/sha256/<digest>.tmp/`.
2. `fsync` each file plus the staging dir.
3. `rename` the staging dir into place; `fsync` the parent.

Tag advancement is the same pattern, on `tags/<name>.json`. A flock on
`blobs/sha256/.<hex>.lock` serializes concurrent installs of the same
digest, and a flock on `tags/<name>.lock` serializes concurrent
EnsureImage calls for the same tag.

Because the digest depends on the bytes of the produced ext4 image and
`mkfs.ext4` writes a UUID and timestamps, two byte-identical builds on
different machines produce different digests. That's accepted today —
each host owns its own digests; tag-and-refcount semantics are
unaffected.

## Refcount and prune

Tags do **not** protect a digest from prune. Following Docker's model:

- `shed image rm <tag>` removes the tag, never the blob.
- `shed image prune` walks the on-disk state and removes any blob with
  zero protective references.

The protective references are:

- **Sheds** — `instances/<name>/metadata.json` records `lower_digest`.
  Every existing shed pins its lower digest.
- **Snapshots** — `snapshots/<name>/snapshot.json` records
  `lower_digest`. Snapshots inherit the pin from the source shed.

Pruning a tag whose digest is still pinned by a shed or snapshot
leaves the blob in place. Once nothing references the digest, the next
prune reclaims it.

Stopped sheds count as references, so a digest stays cached as long as
any shed (running or stopped) was created from it.

In-flight `shed create` calls also protect their target digest. The
server writes a `.creating` marker into `instances/<name>/` recording
the lower digest between resolving the image and saving the shed's
metadata, and a concurrent `shed image prune` treats every fresh
marker as a protective reference for up to **1 hour**. After that the
marker is considered crash residue and stops protecting — at which
point the blob becomes reclaimable on the next prune. A successful
create removes its marker on the spot, so the gate only matters for
crashed creates.

## Tag updates don't propagate to existing sheds

A shed's metadata pins the **digest** its lower was resolved from at
create time, not the tag string. After a `shed image pull <ref> -t
experimental` that advances the `experimental` tag to a new digest,
existing sheds keep booting from the old digest until they're deleted
and recreated — `shed stop && shed start` (and a future `shed restart`)
re-read the metadata, they do not re-resolve the tag. This is
intentional: live re-resolution would change the read-only lower out
from under a running guest. To roll a shed onto a new tag's content,
`shed delete <name>` and `shed create <name> --image experimental`.
Workspace contents under `--local-dir` are unaffected by that cycle.

## Lifecycle commands

| Command | What it does |
|---|---|
| `shed image build [-t <tag>]` | Build a Dockerfile, convert to ext4, install + tag |
| `shed image pull <ref> [-t <tag>]` | Pull a Docker reference, convert + tag |
| `shed image ls` | List tags + dangling blobs with their digests |
| `shed image inspect <tag-or-digest>` | Show the manifest + cached path + in-use status |
| `shed image tag <src> <new>` | Point a new tag at an existing digest |
| `shed image rm <tag>` | Remove a tag (blob persists for prune to GC) |
| `shed image prune` | Remove unreferenced (dangling) blobs |

`shed create --image <tag>` resolves through the tag → digest →
`blobs/sha256/<digest>/rootfs.ext4` chain.

## Why this matters

- **Multiple tags can share content.** A `_base` tag and an
  `experimental` tag pointing at the same Docker ref converge on the
  same digest and share the underlying blob — no hardlink trick needed.
- **Refcount-protected GC.** `shed image prune` is safe at any time:
  it never removes a digest a shed depends on, and the dry-run flow
  shows exactly what would be deleted.
- **Snapshot provenance is explicit.** A snapshot pins the digest its
  source shed was built on, so `shed create --from-snapshot` can use
  the same lower image months later as long as the digest is still
  cached. If the digest has been evicted, the operator gets a clear
  error pointing at the original Docker ref.
- **Overlay-in-guest, not host-side reflinks.** The digest-keyed blob
  is the read-only lower of an in-guest overlayfs, layered with a
  per-shed writable upper at `uppers/<shed>/upper.ext4`. This means
  per-shed disk cost is the upper alone (sparse, default 5 GB) and
  host filesystem reflink support is no longer load-bearing — the
  cost model is the same on APFS, ext4-reflink, and plain ext4.

See also: [Image Variants](images.md), [Disk Management](disk-management.md),
[Snapshots](snapshots.md).
