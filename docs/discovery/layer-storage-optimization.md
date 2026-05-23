# Layer Storage Optimization

Design notes for reducing disk overhead in the OCI image store.
Background context for operators running many cached tags or
disk-bound CI runners.

> **Status note (v0.5.1+):** the materialize model changed in v0.5.1
> from per-layer cache files to a single flattened erofs per manifest.
> Sections 1 and 2 below describe the current state; subsequent
> sections (composefs, blob-sharing, cache eviction) are still-open
> exploration that applies on top of the v0.5.1 design.

## 1. Current State: per-manifest flattened erofs

Shed's OCI store keeps two distinct artifacts:

| Form | Where | Purpose |
|---|---|---|
| Layer tar.gz blobs | `blobs/sha256/<hex>` | Canonical OCI blobs; byte-perfect for `shed image push` and registry round-trips. Deduplicated across manifests. |
| Flattened manifest lower | `cache/sha256/<manifest-digest>.erofs` | Single read-only erofs representing all of a manifest's layers merged with OCI whiteouts applied. Mounted at boot. Shared across every shed using that manifest. |

For typical Ubuntu-rootfs content, erofs+lz4 lands around **0.5–0.7×
the equivalent uncompressed ext4 size**, comparable to the gzipped
tar.gz blobs. Total cost for a manifest is:

- Sum of layer blob sizes (deduplicated across manifests — `apt-get
  install` is one blob shared by `base`, `extensions`, and `full`).
- Plus the per-manifest erofs (one file per variant; the flattened form
  is not deduplicated across manifests because OCI-whiteout application
  + ordering is manifest-specific).

In aggregate this is ~1.0–1.3× the equivalent ext4 alone for a typical
shed deployment. The trade-off is intentional:

- **Boot is fast.** Mount-and-go; no on-demand tar extraction.
- **Push is byte-perfect.** The manifest digest at the destination
  equals the local manifest digest.
- **Inspectability.** `shed image inspect <tag>` matches
  `docker manifest inspect` and `crane manifest --from-archive` for the
  same tag.

For a default `full` install with ~3 GB of uncompressed rootfs across
7-ish layers, the on-disk cost is now ~3.2 GB for `full` alone, then
roughly +0.5 GB to also keep `extensions` and `base` resident (their
flattened erofs files are per-manifest, but the underlying layer blobs
that make up `base` and `extensions` are already on disk because `full`
references them).

## 2. Tradeoffs in the flatten design

The per-manifest flatten is simpler to boot but loses one optimization
the older per-layer model had: the per-layer erofs files were shared
across manifests that referenced the same layer blob. With flatten,
each manifest gets its own erofs. For a user who keeps base +
extensions + full all cached, that's three full-rootfs erofs files,
not one + two thin diff layers.

In practice this matters less than it sounds:

- The layer blobs themselves still dedupe across manifests (the big
  APT layer is one blob, no matter how many flat erofs files
  reference its content).
- Most users care about one variant at a time.
- mkfs.erofs over a 3 GB merged tree is single-digit seconds — fast
  enough that lazy-materialize-on-first-create is a non-event.

The dual-format question (do you keep the layer blobs alongside the
flattened erofs, or evict one of them?) is open work. See section 4.

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

Disk pressure shows up on hosts that:

- **Cache many pulled-but-idle tags.** A CI runner that pulls every
  release tag for regression testing keeps each manifest's blobs +
  flattened erofs. Ten releases at ~3 GB each = ~32 GB instead of
  ~30 GB (blob deduplication absorbs most of the multi-tag cost; the
  per-manifest erofs is what scales linearly with manifest count).
- **Run a single tag with frequent rebuilds.** Every `shed image
  build` of a derived image lands new layer blobs and a new manifest
  erofs. Old manifests stay referenced by their tags (or by sheds
  pinning them) until prune runs.
- **Use multi-arch indexes.** Pulling `--platform linux/arm64` AND
  `--platform linux/amd64` doubles every layer blob AND produces a
  separate flattened erofs per platform.

Single-developer hosts that pull one tag per release and prune
quarterly typically don't notice the overhead.

## 6. Roadmap Sketch

| Version | Change |
|---|---|
| v0.5.0 | Multi-layer OCI store; per-layer ext4 cache (~1.4× ext4 overhead); no eviction. |
| v0.5.1 | **Flatten + host-native materialize.** Per-manifest erofs cache replaces per-layer cache. mkfs.erofs runs on the host (no Docker, no materializer VM). OCI whiteouts applied at flatten time. Boot path drops from N-lower overlay to single-lower. |
| Next | `shed image prune --cache-only` to evict orphaned flattened erofs without touching layer blobs. Auto LRU eviction with configurable cache budget. |
| Later | composefs: keep the layer blobs as the canonical artifact, generate composefs metadata that maps onto them at boot, eliminating the flat erofs entirely and getting blob-level sharing across manifests at boot. Needs `mkcomposefs` on the host (today Linux-only — would re-introduce a VM step on Mac). |

Nothing in this sketch is committed. Treat it as "if we don't get
distracted by something more important."

## 7. Whiteout Translation — RESOLVED in v0.5.1

OCI middle-layer tarballs encode file deletions as `.wh.<name>` and
opaque-directory markers as `.wh..wh..opq`. Pre-v0.5.1, shed's
per-layer materializer did a plain tar extract into a fresh ext4 and
passed whiteouts through verbatim, so middle-layer deletions in
arbitrary foreign images were silently ignored.

The flatten pipeline added in v0.5.1
(`internal/vmimage/flatten.go:MergeLayersFromManifest`) handles
whiteouts correctly:

- A `path/.wh.name` marker at layer N suppresses any entry whose path
  is `path/name` or a descendant of `path/name` from layers below N.
  The marker itself is never emitted into the merged tar.
- A `path/.wh..wh..opq` marker at layer N suppresses any entry
  strictly under `path/` from layers below N (but keeps same-layer
  siblings).

Implementation walks layers in REVERSE OCI order, emitting the first
entry seen for each path. See `flatten_test.go` for the test cases
covering simple flatten, file whiteouts (recursive on directories),
opaque-dir whiteouts with same-layer re-adds, re-add-after-whiteout
across layers, and symlink passthrough.

For shed's own variants (`base` ⊂ `extensions` ⊂ `full`) whiteouts
don't appear — each stage only adds files. But foreign images with
`RUN rm /something/from/parent` now flatten correctly.

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
  still shells out to `docker buildx` for the Dockerfile path. The
  `--from-oci-archive` flag (added in v0.5.x) lets users build with
  podman / buildah / nix-build / etc. and ingest the resulting OCI
  archive without invoking Docker. See
  [build-your-own-image.md § 2a](../tutorials/build-your-own-image.md#2a-alternative-building-without-docker)
  for the workflow.
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
