# Layer Storage Optimization

Design notes for reducing the per-layer disk overhead in the OCI image
store. Background context for operators running many cached tags or
disk-bound CI runners.

## 1. Current State: 1.4× Overhead

Shed's v1 OCI store keeps two copies of every layer:

| Form | Where | Purpose |
|---|---|---|
| Layer tar.gz | `blobs/sha256/<hex>` | Canonical OCI blob; byte-perfect for `shed image push` and registry round-trips |
| Derived ext4 | `cache/sha256/<hex>.ext4` | Mounted directly as an overlayfs lower at boot time |

For typical content (rootfs of an Ubuntu-based shed image), the tar.gz
is roughly **0.4× the size of the ext4**. Total cost is ~1.4× the ext4
alone.

The trade-off is intentional:

- **Boot is fast.** No on-demand tar extraction in the hot path.
- **Push is byte-perfect.** The manifest digest at the destination
  equals the local manifest digest, which means
  `shed image push --byte-perfect` is real, not approximate.
- **Inspectability.** `shed image inspect <tag>` matches
  `docker manifest inspect` and `crane manifest --from-archive` for the
  same tag.

For a default `full` install with 3 layers totaling ~3 GB, the
overhead is ~1.2 GB. Acceptable for single-developer macOS hosts;
worth optimizing on multi-tenant or disk-bound hosts.

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

**Stay on ext4 for v1.** Both squashfs and erofs are worth a
prototype, but neither is a free swap — initramfs changes, kernel
config audit, performance benchmarking on real boot paths. Park for v2.

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
| v1.0 | Current — 1.4× overhead, no eviction. |
| v1.5 | `mkfs.ext4 -O ^has_journal -m 0 -N <auto>` for layers under 256 MB. Manual `shed image prune --layer-cache`. |
| v2.0 | Squashfs or erofs lower option behind a feature flag, with benchmarks. |
| v2.5 | Auto LRU eviction with configurable budget. |

Nothing in this sketch is committed. Treat it as "if we don't get
distracted by something more important."

## 7. Open Questions

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
