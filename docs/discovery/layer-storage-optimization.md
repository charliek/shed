# Layer Storage Optimization

Design notes for reducing the per-layer disk overhead in the OCI image
store. Background context for operators running many cached tags or
disk-bound CI runners.

## 1. Current State: ~1.0–1.1× Overhead

Shed's OCI store keeps two copies of every layer:

| Form | Where | Purpose |
|---|---|---|
| Layer tar.gz | `blobs/sha256/<hex>` | Canonical OCI blob; byte-perfect for `shed image push` and registry round-trips |
| Derived erofs | `cache/sha256/<hex>.erofs` | Mounted directly as an overlayfs lower at boot time (lz4-compressed) |

For typical content (rootfs of an Ubuntu-based shed image), the tar.gz
is roughly the same size as the erofs cache file — both are compressed
representations of the same content. erofs+lz4 typically lands around
**0.5–0.7× the equivalent uncompressed ext4 size**, with gzipped tar.gz
in the same ballpark. Total cost is now ~1.0–1.1× the equivalent ext4
alone, down from ~1.4× when the cache was uncompressed ext4.

Legacy `.ext4` cache files from v0.5.0 and earlier are still mounted at
boot; new materializes produce `.erofs`.

The trade-off is intentional:

- **Boot is fast.** No on-demand tar extraction in the hot path.
- **Push is byte-perfect.** The manifest digest at the destination
  equals the local manifest digest, which means
  `shed image push --byte-perfect` is real, not approximate.
- **Inspectability.** `shed image inspect <tag>` matches
  `docker manifest inspect` and `crane manifest --from-archive` for the
  same tag.

For a default `full` install with 3 layers totaling ~3 GB of
uncompressed rootfs, the on-disk cost is now in the ~3.2 GB range
rather than ~4.2 GB. The two-forms model is still an intentional
trade — fast boot and byte-perfect push in exchange for a compressed
cache copy alongside the canonical blob.

## 2. mkfs.ext4 Floor

Every derived ext4 carries a fixed-cost prelude — journal, group
descriptors, inode tables, root directory entry — that's about 1.5 MiB
even for an empty filesystem.

For tiny custom layers (think a 5 MB layer that just drops a config
file into `/etc`), the floor is 30% of the layer cost. Default
`mkfs.ext4` flags overshoot for read-only lowers:

| Flag | Default | Recommended for read-only lowers |
|---|---|---|
| `-O has_journal` | on | **off** (`-O ^has_journal`) — no writes happen to a lower, journal is dead weight |
| `-m <pct>` | 5% reserved | **0** (`-m 0`) |
| `-N <inodes>` | auto | size to actual content, not the formula |

These three together cut the prelude from ~1.5 MiB to ~256 KiB for a
small layer. The trade-off is that the ext4 cannot be remounted
read-write — which is fine, lowers are mounted read-only.

**Status:** not yet wired up in `materializeLayer`. Tracked as a
follow-up; expected to land as `mkfs.ext4 -O ^has_journal -m 0 -N
<computed>` for any layer under N MB.

**Note (v0.5.1).** The optimizations in this section are now moot for
freshly-materialized images. Materialize uses `mkfs.erofs`, which has
no journal and no group-descriptor / reserved-block overhead — the
floor is a few KiB rather than 1.5 MiB. The section is kept because
(a) legacy `.ext4` cache files from earlier versions still exist and
mount, and (b) it documents the design space we explored before
switching.

## 3. Cache Eviction Designs

The `cache/sha256/<hex>.ext4` files are derived data — they can be
re-materialized from the tar.gz blob at any time. That makes them
prime candidates for eviction when disk gets tight.

### Option A: LRU

Track access time on `cache/sha256/*.ext4` (we already update mtime on
materialize and atime on every overlay mount). On `shed image prune`,
evict ext4s past a size budget, oldest-first.

**Pros:** zero per-operation cost; works well for "I have a budget,
keep the hot N layers".

**Cons:** requires a budget config knob; the next `shed start` for an
evicted layer pays a re-materialize cost (~5–30 s for typical layers).

### Option B: Refcount-Based

Drop the ext4 the moment its refcount (live sheds + snapshots) hits
zero, even if the tar.gz blob is still tagged. The blob stays for
`shed image push` and future `shed create`; the ext4 only exists when
something needs to boot from it.

**Pros:** zero standing overhead beyond actively-booted images.

**Cons:** every `shed create` from a "cold" tag pays a re-materialize.
Surprising for users used to "I pulled it, it's ready."

### Option C: Manual

`shed image prune --layer-cache` evicts all ext4s with refcount zero.
Operator runs it when disk gets tight.

**Pros:** simplest; zero policy decisions; predictable.

**Cons:** operator has to remember.

### Recommendation

**Start with Option C** (manual) in v1.5. Zero policy complexity, easy
to undo, doesn't introduce a surprise latency in `shed create`. Revisit
A or B if disk-pressure complaints come in.

## 4. Alternative Read-Only Filesystems

ext4 is the path of least resistance because the in-guest kernel
already speaks it. But it's not the only option for the lowers.

### squashfs

| Aspect | squashfs vs ext4 |
|---|---|
| Size | **Smaller** — built-in xz/zstd compression typically 2–4× over uncompressed ext4 |
| Materialize time | **Slower** — compression is CPU-bound on creation |
| Mount overhead | Similar |
| Kernel support | `CONFIG_SQUASHFS=y` — already in the FC kernel and most Ubuntu kernels |
| Mutability | Immutable, like the read-only ext4 lowers |

A squashfs lower could replace the cache ext4 1:1 at roughly 0.4–0.6×
the cost. The tar.gz blob still pays its 0.4× canonical cost, so total
overhead drops from 1.4× to ~0.8–1.0× the equivalent ext4 size.

### erofs

| Aspect | erofs vs squashfs |
|---|---|
| Size | Comparable, sometimes slightly smaller |
| Materialize time | Faster than squashfs at equivalent ratios |
| Mount overhead | Slightly lower |
| Kernel support | `CONFIG_EROFS_FS=y` — in mainline since 5.4. FC kernel does NOT include it today |

erofs is the up-and-coming choice (Android uses it for system
partitions). If we ever rebuild the FC kernel, picking up erofs is
easy. For VZ we inherit whatever Ubuntu ships, which is erofs-capable.

### Decision

**Switched to erofs in v0.5.1.** erofs+lz4 was preferred over squashfs
because:

- `mkfs.erofs` is roughly 2–3× faster than `mksquashfs` at equivalent
  compression ratios — matters when materialize runs once per layer
  and a typical image has 5–10 layers.
- Random reads are faster, which shows up during `shed start` storms
  and systemd boot from the layer.
- Android ships erofs on system partitions at fleet scale, so the
  format is battle-tested under real workloads.

Kernel support landed alongside the format switch: the Firecracker
kernel was rebuilt with `CONFIG_EROFS_FS=y`. On the VZ side, Ubuntu's
`linux-image-virtual` already enables erofs, so no kernel work was
needed there.

## 5. When This Matters

The 1.4× overhead is the dominant disk cost on hosts that:

- **Cache many pulled-but-idle tags.** A CI runner that pulls every
  release tag for regression testing keeps every layer's tar.gz and
  every layer's ext4. Ten releases at 3 GB each = 42 GB instead of
  30 GB.
- **Run a single tag with frequent rebuilds.** Every `shed image
  build` of a derived image lands a new layer ext4. The old layer is
  still referenced by the old tag (or the manifest you just pruned),
  so disk grows monotonically until prune runs.
- **Use multi-arch indexes.** Pulling `--platform linux/arm64` AND
  `--platform linux/amd64` doubles every layer.

Single-developer hosts that pull one tag per release and prune
quarterly typically don't notice the overhead.

## 6. Roadmap Sketch

| Version | Change |
|---|---|
| v0.5.0 | Multi-layer OCI store; 1.4× overhead with uncompressed ext4 cache; no eviction. |
| v0.5.1 | erofs+lz4 cache replaces ext4; overhead drops to ~1.0–1.1×. Materialize moved off Docker (native `mkfs.erofs` on Linux, materializer VM on macOS). |
| Next | Manual `shed image prune --layer-cache`. Whiteout translation for foreign multi-layer images. |
| Later | VirtioFS lowers (eliminates the materialize step entirely). Auto LRU eviction with configurable budget. |

Nothing in this sketch is committed. Treat it as "if we don't get
distracted by something more important."

## 7. Whiteout Translation for Foreign Multi-Layer Images

OCI middle-layer tarballs encode file deletions as `.wh.<name>` marker
files. `EnsureExt4FromLayer` in `internal/vmimage/cache.go` does a
plain `tar -xzf` into a fresh ext4, so a `.wh.foo` arrives as a regular
file rather than the character-device-zero (`mknod foo c 0 0`) that
overlayfs honors. The result: **deletions in middle layers are
silently ignored at boot** for any image that uses them.

For shed's own variants (`base` ⊂ `extensions` ⊂ `full`) this is a
non-issue — each stage only ADDS files on top of its parent, never
deletes. But it's a real bug for arbitrary foreign images:
`shed image pull` of a random image whose Dockerfile uses
`RUN rm /something/from/parent` will produce a guest where
`/something/from/parent` *still exists*.

**Fix sketch.** During tar extraction in `EnsureExt4FromLayer`, before
writing each tar entry to the staging ext4:

1. If `entry.Name` matches `*/\.wh\.<name>` (an opaque whiteout marker)
   — convert to `mknod <dir>/<name> c 0 0` and skip writing the marker
   itself.
2. If `entry.Name` matches `*/\.wh\.\.wh\..wh\.\.opq` (opaque-directory
   marker — "ignore everything below this dir in lower layers") — set
   the `trusted.overlay.opaque="y"` xattr on the parent directory.

Both forms are defined in the [OCI image-spec layer changesets][oci-wh].
Implementations to look at: [containerd's diff/walking package][containerd-diff],
[crane's mutate.Extract][crane-extract], [moby's archive package][moby-archive].

[oci-wh]: https://github.com/opencontainers/image-spec/blob/main/layer.md#whiteouts
[containerd-diff]: https://github.com/containerd/containerd/blob/main/pkg/archive/tar.go
[crane-extract]: https://github.com/google/go-containerregistry/blob/main/pkg/v1/mutate/mutate.go
[moby-archive]: https://github.com/moby/moby/blob/master/pkg/archive/diff.go

**Acceptance test once implemented:**

```dockerfile
FROM ghcr.io/charliek/shed-vz-extensions:latest
RUN echo placeholder > /tmp/marker
RUN rm /tmp/marker
```

Build with `shed image build`, boot, exec `ls /tmp/marker` — should
report "No such file or directory". Today it incorrectly reports the
file as present.

## 8. Build-Time Layer Non-Determinism

Buildkit's tar.gz emission isn't byte-stable: rebuilding the same
Dockerfile from a hot cache can yield layer digests that differ by a
handful of bytes (observed 6 B and 32 B differences between `base` and
`extensions` for what should be identical bind-mount staging layers).
The root cause is some combination of gzip implementation, mtime
preservation, and tar header field ordering.

Consequences:

- Cross-variant sharing for the *intended-identical* staging layers
  doesn't quite happen — two ~7 MB layers diverge between `base` and
  `extensions` instead of being shared.
- Local builds vs the published `ghcr.io/charliek/shed-*` tags have
  different layer digests, so `shed image pull` after a local build of
  the same content re-downloads identical content.

Workarounds to investigate:

- `BUILDKIT_INLINE_CACHE=1` + cache import/export to make the staging
  layer hashes reproducible across builds.
- `--source-date-epoch` (buildkit 0.13+) to pin mtimes.
- Reproducible gzip via `--build-arg SOURCE_DATE_EPOCH=...` once
  buildkit normalizes its compression.

Loss from current state: ~14 MB across `base`+`extensions`+`full`
(two small layers × ~7 MB unshared). Small enough that this is a
"nice to have" not a "must fix".

## 9. Spurious 32-Byte Empty Layers

Each variant's manifest carries one or more 32-byte tar.gz layers
(digest `sha256:4f4fb700ef54461cfa02571ae0db9a0dc1e0cdb5577484a6d75e68dc38e8acc1`
— gzipped empty tar). They come from buildkit's handling of `ENV`,
`LABEL`, and `WORKDIR` instructions when they're the *only* thing in
a stage's diff.

These layers cost almost nothing on disk (1 mkfs.ext4 prelude ≈ 1.5 MiB
each — about 4–6 MiB across the three variants), but they pollute
`shed image history` and bump the `MaxLayers=16` budget unnecessarily.

Workarounds:

- Fold the `ENV`/`LABEL` into the surrounding `RUN` via a `&&` chain.
- Use buildkit's `--metadata-only-cache-prune` mode (if it ever lands)
  to drop empty diff layers.

This is fully cosmetic — the layer cap (16) gives plenty of headroom
for the 9–10 we ship today.

## 10. Open Questions

- **Cache key for `-O ^has_journal`:** if we ever change the
  materialize parameters, the digest of `cache/sha256/<hex>.ext4` is
  no longer a function of the layer alone — it's a function of (layer,
  mkfs params). Either commit to fixed params forever, version the
  cache directory, or accept rebuilds on upgrade. Probably the third.
- **Squashfs reproducibility:** different versions of mksquashfs
  produce different bytes for the same input. The cache layer is local
  so reproducibility doesn't matter for `shed image push`, but it does
  matter for fleet-wide rolling upgrades.
- **Live ext4 evict:** if a layer's ext4 is currently overlay-mounted
  in a running shed, can we evict the cache file? Linux holds the
  inode alive via the open fd, but new `shed start` calls for
  *another* shed pinning the same layer would fail to mount until the
  layer is re-materialized. Probably safer to refuse eviction while a
  shed is running on it; clarify before implementing Option B.
- **Whiteout translation testing path:** the acceptance test above
  needs a guest-side comparison harness — a way to walk a layer's tar
  pre-extract and a `find /` post-boot diff. Worth building once;
  reusable for any future layer-semantics work.

## 11. The Materialize-via-Docker History

The path from "single ext4 per image" to "per-layer erofs via a
materializer VM" took three releases and one production incident.

**v0.4.x: single flat ext4 per image.** Each pulled image was
materialized once into a single ext4 file via
`docker run ubuntu:24.04 mkfs.ext4`. Slow per invocation, but it ran
once per image and the cost was amortized across every `shed start`
from that image. Tolerable.

**v0.5.0: multi-layer rollout amplified the cost N×.** Switching to
the OCI multi-layer store meant one materialize per layer rather than
one per image — typically 5–10 layers for a default image. The
per-materialize cost also included `apt-get install e2fsprogs` inside a
fresh Ubuntu container every time, since the materialize container was
ephemeral.

**#99: Docker Desktop hung under load.** Once 5–10 materialize
containers ran back-to-back during a fresh `shed image pull`, Docker
Desktop on macOS would intermittently lock up — not a shed bug per se,
but shed's usage pattern reliably triggered it. What had been a
tolerable annoyance in v0.4 became a production blocker in v0.5.

### Options Considered for v0.5.1

| Option | Pros | Cons |
|---|---|---|
| Published helper image (`ghcr.io/charliek/shed-cache-builder`) | Cleanest tooling-wise; single thin container | Introduces a separate publish workflow; still depends on Docker at runtime |
| Locally-built Dockerfile shipped with shed | No external dependency | ~30 s one-time build per host; couples `shed-server` to repo files; awkward with brew/deb installs |
| Native `mkfs` on the host (no Docker) | Zero indirection on Linux | macOS has no native ext4/erofs userland — needs *something* Linux-flavored |
| VirtioFS lowers (no cache filesystem images at all) | Architecturally cleanest — eliminates materialize entirely | Largest scope; separate quarter of work |
| **Materializer VM** (chosen) | Zero Docker dependency; reuses shed's existing kernel + initrd; smallest blast radius; aligns with "we already run Linux VMs" | One extra build artifact (the materializer-mode initramfs) |

### Why the Materializer VM Was the Right Size for v0.5.1

The materializer VM is a tactical fix: shed-server launches a one-shot
vfkit VM with shed's own kernel and a materializer-mode initramfs that
runs `mkfs.erofs` inside the VM, then exits. On Linux hosts the same
work is done natively via the `erofs-utils` package — no VM.

It addresses #99 today without committing to the larger VirtioFS-lowers
redesign, and it leaves that door open as a future "even simpler"
follow-up. The change is contained to `internal/vmimage/` and the
materializer initramfs build; nothing in the rest of the system needs
to know that materialize used to involve Docker.

### Reflection

The v0.5.0 multi-layer rollout was net-positive — ~60% disk savings
across the variant set and byte-perfect interop with arbitrary OCI
registries — but the materialize complexity was higher than
anticipated going in. The Docker-based materialize path worked in
isolation and worked in v0.4 at one-per-image rates; it didn't survive
the N× multiplier of multi-layer at production load. The materializer
VM closes the specific gap that #99 surfaced and leaves the
architecture in a better place to take the next step (VirtioFS lowers)
when scope allows.

## 12. Remaining Roadmap

What's still open after v0.5.1:

- **Cache eviction policy** (LRU / refcount / manual). Still parked.
  Section 3 covers the design options; no implementation yet.
- **`shed image build` without a Docker daemon.** `shed image build`
  still shells out to `docker buildx`. Tracked separately in
  `docs/discovery/docker-free-builds.md`.
- **VirtioFS lowers.** The "even cleaner" architectural follow-up:
  lower layers stay as directory trees on the host (no filesystem
  image at all), mounted into VMs via VirtioFS (macOS) or 9P (Linux).
  Eliminates the materialize step entirely. Big change; parked here
  for future scope.
- **Whiteout translation for foreign multi-layer images**
  (`.wh.foo` → `mknod c 0 0`). Section 7 covers the fix sketch.
  Affects only foreign images that delete files in middle layers;
  shed's own variants only add.
- **32-byte empty layers in published manifests** (from ENV/LABEL
  diffs in buildkit). Section 9. Cosmetic; polish for later.

See [issue #90](https://github.com/charliek/shed/issues/90) which
tracks several of these.
