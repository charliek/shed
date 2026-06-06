# Lazy rootfs streaming (strategy C) — on-demand erofs fetch

Status: **Exploratory / DEFERRED.** Not scheduled. This captures the
design space for fetching a shed's read-only rootfs *lazily* (on first
read) instead of pulling the whole erofs up front, so the decision and
its tradeoffs are recorded for a future revisit. Reading time ~15 min.

> **TL;DR.** Today (strategy D) a host pulls the whole ~1.2 GB erofs
> blob, then boots from it — network-free thereafter. Strategy C would
> fetch only the filesystem chunks the guest actually reads, shrinking
> the cold pull to tens of MB at the cost of a *runtime* network
> dependency and host-side machinery in the data path. The target shape
> is **C-full** (Nydus RAFS v6 / erofs-over-fscache), optionally reached
> via a low-effort **C-lite** stepping stone (range-GET the existing
> monolithic erofs). It is deferred because this *class* of host-side
> runtime complexity is exactly what the v0.5.0→v0.5.2 rework removed —
> see [§9](#9-relationship-to-the-early-05x-failures).

## 1. Context: where shed is today (strategy D)

Since v0.5.2 the read-only rootfs is a single **erofs** blob, minted at
image-publish time inside the pinned `shed-build-tools` container and
shipped as a content-addressed OCI blob referenced by the
`io.shed.rootfs.erofs.digest` manifest annotation. Since v0.6.2 the pull
is **boot-only by default**: the host fetches config + kernel + initrd +
erofs + manifest and skips the OCI layer tarballs (the host boots from
the flattened erofs, never the layers).

Boot path (both backends):

```text
guest mounts erofs read-only  →  /dev/vdb (the whole 1.2 GB blob, local)
        +  per-shed writable upper (overlay)
        →  switch_root
```

The defining property: **once the erofs is pulled, boot and run are
network-free.** A registry outage cannot stall a running shed.

The cost: the **whole** 1.2 GB erofs is downloaded before the first
boot of a never-seen image, even though a cold boot only *reads* a few
hundred MB of it (init, libc, the shell, the agent binary, the user's
first commands).

## 2. The four distribution strategies

| Strategy | What ships | Downloads unused bytes? | Examples |
|---|---|---|---|
| A. Monolithic disk image | one qcow2/raw/ISO | No layers to skip; no dedup; update = re-pull whole disk | Docker Machine, Podman machine, Lima/Colima, OrbStack, Multipass |
| B. OCI image → materialize at deploy | OCI layers | Yes — pull *and* extract every layer | bootc/ostree, Kata guest, container2vm |
| C. Lazy / seekable layers | layers in mountable form | No — fetch only chunks read | eStargz, **Nydus (erofs-based)**, SOCI |
| **D. Hybrid: OCI transport + prebaked rootfs** | layers **+** prebuilt erofs blob | No (boot-only skips layers) | **shed (current)** |

shed deliberately sits in D. Strategy C is the one direction that could
shrink the cold pull *below* boot-only, by not fetching erofs bytes the
guest never reads.

**A note on composefs (a different axis).** composefs — used by
bootc / ostree / Fedora's image-based OS work — is the other
industry-backed technique worth naming, but it optimizes a *different*
axis than C. It gives content-addressed, integrity-verified,
**deduplicated read-only images at rest** (file data shared across images
via a local objects store, mounted with an erofs/overlay combo). It does
**not** shrink the cold pull — a composefs image's data must still be
present locally. So composefs is a real alternative for the *dedup +
integrity* goal, not for C's *lazy-cold-pull* goal; the two could even
compose (composefs for at-rest dedup, C-style lazy fetch for the
objects). The industry's lazy-startup direction (Nydus / eStargz / SOCI)
is squarely strategy C, which is why C is the primary candidate here and
composefs is recorded only as this adjacent alternative.

## 3. The central constraint: the rootfs is a block device

Because a shed is a VM, its rootfs reaches the guest as a **block
device** (`/dev/vdb`, an erofs image). That single fact dictates the
whole design. There are two places to put the on-demand fetcher:

- **Guest-side / filesystem (virtio-fs).** The host mounts a lazy
  filesystem and exposes it to the guest over virtio-fs. **Rejected:**
  Firecracker does not implement virtio-fs (it is intentionally minimal
  — block devices + vsock only). This would split the backends and
  rewrite the rootfs boot path. Both backends are required, so this is
  out.
- **Host-side / block device.** Keep the guest *exactly as it is*
  (erofs on `/dev/vdb` + writable upper), and make only the host's
  *provisioning of the device bytes* lazy. "Pull whole erofs → attach
  file" becomes "attach a virtual block device whose bytes are
  demand-fetched + cached."

**Decision for shed: a lazily-backed block device, guest boot path
unchanged.** This preserves the flattened-erofs + overlay model shed
already relies on and keeps VZ/FC guest behavior identical.

## 4. C-full — Nydus RAFS v6 (erofs-over-fscache)

The "proper" version. RAFS v6 *is* erofs, with the file data externalized
into chunked blobs that can be fetched lazily through the kernel's erofs
+ **fscache** backend (Linux 5.19+).

Publish-time change (in `shed-build-tools`, versioned in lockstep like
the current `mkfs.erofs`):

- Replace `mkfs.erofs → one blob` with a chunked build (`nydus-image
  create`-equivalent), producing:
  - a small erofs **bootstrap / metadata** blob (the directory tree +
    inode table — MB-scale), and
  - one or more **chunked data** blobs + a chunk index.
- Carry both via new manifest annotations alongside the existing
  `io.shed.rootfs.erofs.digest` (e.g. `io.shed.rootfs.rafs.bootstrap`
  and `io.shed.rootfs.rafs.blob`), so D and C variants can coexist in
  the same image.

Host-side runtime:

- A `nydusd`-equivalent daemon backs the erofs data blocks via fscache,
  range-GETting chunks from the registry blob(s) and caching them on a
  local content-addressed store.
- **Chunk-level dedup across images** — two images sharing a base share
  chunks. This is *better* dedup than D's whole-erofs-blob sharing.
- **Prefetch** the boot working set (Nydus supports prefetch hints /
  policies) so `agent_p50` does not regress waiting on cold chunk faults
  for the agent binary + its deps.

Cost: a new long-lived host daemon, a chunk cache with its own
eviction/GC/integrity lifecycle, kernel-feature dependencies, and the
build-tools pipeline overhaul.

## 5. C-lite — range-GET the existing erofs (stepping stone)

The pragmatic first step. **No publish-time change at all** — the
current monolithic erofs blob is used as-is.

- Treat the erofs blob as a network-backed block device: a userspace
  block device serves the guest's reads by issuing **HTTP `Range` GETs**
  against the blob, filling a **sparse local cache file**, and serving
  cached ranges thereafter.
- Works because (a) OCI registries generally support `Range` on blob
  GETs and (b) erofs's on-disk layout means a guest file read maps to a
  known, bounded byte range.
- You give up chunk-level cross-image dedup and clean per-chunk
  integrity, but you get ~all of the cold-boot win for a fraction of the
  effort, and the *host plumbing* (the userspace block device, the
  cache) is shared with C-full — so C-lite is a genuine stepping stone,
  not throwaway.

Open problem for C-lite — **digest verification.** D verifies the whole
erofs blob digest before mounting. With partial fetches you cannot verify
the whole-blob digest up front. Options: verify the full digest lazily as
the sparse cache fills and fail the shed if it ever completes-and-
mismatches; rely on TLS + registry trust for in-flight bytes; or publish
a Merkle/chunk-hash sidecar (which starts to converge on C-full). This
must be resolved before C-lite is more than a prototype.

## 6. How it works per backend (both required)

The guest is identical on both — erofs on `/dev/vdb` + writable upper.
Only the *host-side attachment* differs.

### VZ (macOS, Apple Virtualization.framework)

- macOS 14+ exposes **`VZNetworkBlockDeviceStorageDeviceAttachment`** —
  VZ can attach an **NBD** device directly.
- Run a host **NBD server** that implements the lazy fetch (C-lite:
  range-GET + sparse cache; C-full: nydusd + fscache exported as a block
  device), and attach it to the VM as the rootfs disk.
- Cleanest of the two: the laziness lives entirely in a userspace NBD
  server; VZ needs no special guest support.
- Floor: requires macOS 14+. Older macOS would fall back to D (whole
  pull) — acceptable since C is opt-in.

### Firecracker (Linux, KVM)

- Firecracker drives are **file-backed**; there is no NBD attach and no
  virtio-fs.
- Back the drive with a userspace block layer:
  - **ublk** (io_uring userspace block driver, Linux 6.0+) — present a
    `/dev/ublkbN` device whose reads are served by a userspace fetcher,
    and hand that to Firecracker; **or**
  - a **FUSE-backed file** that demand-fetches ranges, used as the drive
    file.
- C-full additionally needs the host kernel's **erofs-over-fscache**
  (5.19+) if you mount RAFS on the host and re-export; or you keep the
  fetch logic in the ublk server and treat the erofs purely as opaque
  blocks (simpler, mirrors the VZ NBD server).

**The asymmetry tax.** Even with the guest untouched, VZ (NBD) and FC
(ublk/FUSE) need *different host-side mechanisms*. shed has worked to keep
the boot path symmetric across backends; C splits the host plumbing in
two. Sharing the actual fetch+cache core (a single library; only the
device-presentation shim differs: NBD vs ublk) keeps the divergence to
the edges, but it does not eliminate it.

## 7. Cold-start sequence (C-lite, concrete)

1. Pull only: kernel + initrd whole (~60 MB, needed to boot) + the erofs
   superblock/metadata region (faulted in first). Initial bytes ≈ tens
   of MB vs 1.2 GB.
2. Host starts the lazy block daemon (NBD on VZ / ublk on FC) pointed at
   the erofs blob URL + a sparse local cache.
3. Attach the device to the VM.
4. Guest boots, mounts erofs from `/dev/vdb` as today. Reads fault in
   ranges on demand → host range-GETs + caches + serves.
5. First boot fetches only the working set (~150–300 MB). Subsequent
   creates from the same image are cache hits — effectively D-speed.

## 8. Tradeoffs

| Axis | Gain | Cost / risk |
|---|---|---|
| Cold pull | Tens of MB to first boot vs 1.2 GB | — |
| Disk | Only accessed chunks stored | New cache state: eviction, GC, integrity |
| Dedup | Chunk-level across images (C-full) | C-lite keeps only whole-blob dedup |
| **Robustness** | — | **Runtime network dependency** — a registry/CDN blip mid-boot stalls or errors guest I/O. Removes the network-free property D guarantees. |
| Perf | Faster time-to-first-shell on cold images | Cold-file access = network RTT per miss; **`agent_p50` regresses** without boot-working-set prefetch |
| Complexity | — | Host fetch daemon + NBD(VZ)/ublk(FC) split; C-full also rebuilds the build-tools pipeline |
| Debuggability | — | Failures are runtime/boot-time and intermittent (same hard-to-repro class as the 0.5.x boot bugs) |

## 9. Relationship to the early-0.5.x failures

This is the most important section for deciding *whether* to pursue C,
because shed already lived through a closely related complexity and
backed out of it. The throughline of v0.5.0 → v0.5.2 was **moving
filesystem construction and machinery off the host.**

The lineage (condensed here from the now-retired v0.5.1 materializer and
layer-storage discovery docs; the blow-by-blow is in the CHANGELOG for
v0.5.0/v0.5.1/v0.5.2):

- **v0.5.0 / pre-v0.5.1 — the materializer VM.** Distribution was
  layered OCI; the guest booted an **N-lower stacked overlayfs**, with
  each layer turned into a per-layer erofs by a one-shot vfkit
  "materializer" VM (`internal/vz/materializer.go`, ~430 LOC) plus an
  initramfs `shed.mode=materialize` branch (dd / gunzip / busybox tar /
  `mkfs.erofs`). It *mechanically worked* (6/6 layers in ~30 s) but was
  ~1100 LOC of fragile host/guest choreography, and a `systemd-firstboot`
  boot blocker masked debugging until console-log preservation was added.
  Unwound in v0.5.1.
- **v0.5.1 — host-native flatten.** Merge layers + apply whiteouts →
  **`mkfs.erofs` on the host** → one erofs lower. Adopted the
  bootc/Podman-Machine single-lower pattern. **It failed end-to-end on
  Linux/Firecracker:** the on-host `mkfs.erofs` hit an `erofs-utils`
  1.7.1 writer bug (big pcluster without the matching superblock feature
  flag) and the **guest kernel rejected the rootfs at boot**
  (`erofs: per-inode big pcluster without sb feature`). Root cause:
  **host-distro tooling variance** producing a filesystem the guest
  couldn't read.
- **v0.5.2 — erofs at publish time.** Moved `mkfs.erofs` off the host
  into the pinned `shed-build-tools` container; ship the erofs as a
  content-addressed OCI blob. Eliminated host variance, the ~30 s mkfs
  step, and a duplicate cache file (~37 % host-disk drop). This is the
  foundation strategy D still stands on.

**What C does *not* repeat.** C does **not** reintroduce host-side erofs
*construction*. The erofs (or RAFS) is still built at publish time with
pinned tooling — C keeps the v0.5.2 win intact. So C does **not** bring
back the specific v0.5.1 `erofs-utils`-version-skew failure. This is
worth stating plainly: *C is not "v0.5.1 again."*

**What C *does* risk repeating.** C re-introduces the *class* of problem
the 0.5.x cleanup fled — **host-side runtime machinery in the boot/data
path**:

- A long-lived host daemon + cache (echoes the materializer's moving
  parts), kernel-feature dependencies (fscache, NBD on VZ, ublk on FC —
  echoes the old `insmod erofs.ko` / module choreography), and
  **backend-divergent host plumbing** (the very asymmetry v0.5.1's
  single-lower flatten was praised for removing).
- Failure modes that are **runtime / boot-time and intermittent** — the
  same hard-to-repro class as the 0.5.x boot bugs; the console-log
  preservation lesson applies again.
- **Plus a brand-new runtime *network* dependency** that none of the
  0.5.x designs had and that D explicitly removed.

In short: the 0.5.x saga is *direct evidence* for the instinct that "this
complexity has caused issues before." C avoids the specific 1.7.1 bug but
lands squarely back in the complexity *category* that took three releases
to escape. That history is the strongest argument for keeping C deferred
and behind a high bar.

### Specific lessons carried from the v0.5.1 materializer (now deleted)

Learned the hard way building the materializer/flatten path — worth not
re-learning if C is ever built:

- **`mkfs.erofs` tooling is version-sensitive.** `mkfs.erofs --tar=f` on
  erofs-utils 1.8.6 failed with `[Error 74] Bad message` on Ubuntu base
  layers (PAX / `@LongLink` tar records); 1.9.1 handles them; 1.7.1 had
  the big-pcluster writer bug. Any C variant that re-chunks or re-mints
  erofs must pin the tool (as publish-time already does) — never run it
  host-side against host-distro variance.
- **`mkfs.erofs --aufs` converts whiteouts but does *not* flatten.** It
  preserves whiteouts as overlayfs xattrs for downstream overlay use; to
  get a single merged tree you must apply whiteouts yourself first.
- **Preserve the boot console log past instance-dir cleanup.** The
  materializer bugs were only diagnosable because `console.log` was kept
  after a failed create. C's failures are runtime and intermittent —
  without a preserved log every investigation degrades to "rerun and hope
  it repeats."

## 10. Recommendation

- **Target: C-full**, reached via a **C-lite** stepping stone if pursued
  — C-lite proves the userspace block device + cache + per-backend
  attachment (the risky host plumbing) against the *unchanged* erofs
  blob, before committing to the build-tools/RAFS pipeline overhaul.
- **Ship it as C-on-top-of-D, opt-in** — mirroring how boot-only landed
  (default-safe + `--with-layers` opt-out). Keep the prebaked whole erofs
  as the canonical, default, network-free artifact; publish the chunked/
  lazy variant additionally; let `pull_policy` or a flag pick "whole"
  (default, robust) vs "lazy" (fast cold start, needs network). Persistent
  dev servers stay on whole; an ephemeral/CI-style fleet opts into lazy.
  The robust common case never regresses.
- **The bar to clear before doing this.** C optimizes for **many images
  × ephemeral hosts × instant-first-boot-of-a-never-seen-image.** shed's
  current world is **few images × persistent servers × network-free
  robustness**, where D is the right call. Pursue C only when the usage
  pattern actually shifts — i.e., when the 1.2 GB whole-pull on cold
  images becomes the dominant, measured bottleneck for real users, and
  not before. Re-validate the per-backend timing gate (`agent_p50`) with
  prefetch on, since cold-fault latency is the thing most likely to
  regress.

## 11. See also

- [`runtime-optimization-backlog.md`](runtime-optimization-backlog.md) — the
  runtime/boot backlog and durable invariants (successor to the retired
  platform-runtime and layer-storage discovery docs).
- [`../reference/images.md`](../reference/images.md) and
  [`../reference/storage-model.md`](../reference/storage-model.md) — the
  current pull/build/push/on-disk model (strategy D).
- [`../upgrades/v0.5.1-to-v0.5.2.md`](../upgrades/v0.5.1-to-v0.5.2.md) — the
  move to publish-time erofs that C preserves.
