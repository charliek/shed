# Platform Runtime Optimization: VZ and Firecracker

Discovery notes for stabilizing and speeding up shed creation now that
the feature set is largely complete. The thesis: keep the **user-facing
interface identical** across macOS (VZ) and Linux (Firecracker), but
stop forcing the **host-side runtime** of the two backends to be
structurally the same. Lean into each platform's native primitives.

> **Provenance.** This document is based on a full read of the backend
> code (2026-05-26) plus a live VZ boot on the author's Apple-Silicon
> mac (`shed 0.5.3`, `shed-vz-full:v0.5.3`). The VZ path is
> verified end-to-end on real hardware; the Firecracker path is from
> code review only and its phase timings are **hypotheses to measure on
> a Linux/KVM host**, not observed numbers. Every "this is slow / this
> is fragile" claim below is a candidate for instrumentation (§9),
> not an established fact.

---

## 1. Corrected mental model: what is actually shared

A common misconception is that VZ and Firecracker are two parallel VM
stacks. They are not. **The guest is the same on both platforms.**

- `vz/Dockerfile` installs the identical `shed-agent` binary and
  `shed-agent.service` systemd unit that Firecracker uses
  (`vz/Dockerfile:95-117`).
- `cmd/shed-agent` is `//go:build linux` because the "Mac" VZ VM *is a
  Linux guest* running on Apple's hypervisor. On Apple Silicon it is an
  `aarch64` Ubuntu guest (verified live: `uname -a` →
  `Linux vztest 6.8.0-117-generic ... aarch64`).

So the divergence between platforms is entirely **host-side**: the VMM,
the host↔guest transport, the workspace-share mechanism, networking,
and the disk/copy primitives. Everything from the agent upward is one
codebase.

### 1a. Genuinely shared — keep it shared (the stable core)

| Layer | Where |
|---|---|
| Guest Linux image, `shed-agent`, systemd units, provisioning hooks | `vz/Dockerfile`, `firecracker/Dockerfile`, `cmd/shed-agent` |
| vsock binary protocol | `internal/agentproto/protocol.go` |
| Agent client, health, exec, provisioning, shutdown sequence | `internal/vmutil/agent.go`, `internal/vmutil/provisioning.go` |
| OCI content-addressed blob store, erofs lower, refcount GC | `internal/vmimage/manager.go` |
| CLI + HTTP/SSE interface | `cmd/shed`, `internal/api` |

This is the part that should *stay* uniform. The interface constraint
the project owner cares about lives here.

### 1b. Already platform-divergent — good, deepen it

These are places where the code already leaned into platform
differences. They are working well and are the model for everything
else.

| Concern | VZ (macOS) | Firecracker (Linux) |
|---|---|---|
| VMM | `vfkit` subprocess | `firecracker` via `firecracker-go-sdk` |
| Workspace share | **VirtioFS** (`virtio-fs` device) | **9P** (`internal/firecracker/p9server.go`, `p9_remap.go`) |
| Networking | NAT, host reaches guest at `127.0.0.1` | TAP + `shed-br0` bridge + netlink (`internal/firecracker/network.go`) |
| Host↔guest transport | per-port Unix socket (`internal/vz/dialer.go`) | vsock-over-UDS with `CONNECT <port>` handshake (`internal/firecracker/dialer.go`) |

### 1c. Forced-uniform where it hurts — candidates to diverge

These are the seams where "make both backends look the same" is
currently costing speed, robustness, or clarity. The rest of this
document is mostly about these.

| Forced uniformity | Symptom | File |
|---|---|---|
| Writable upper is `mkfs.ext4`'d **inside the guest on first boot** | Every new shed's boot blocks on a synchronous in-guest format | `internal/vz/rootfs.go:98-120`, `internal/firecracker/rootfs.go:97-102` (identical `FreshUpperSignature` contract) |
| Health readiness is a generic host poll | 500 ms tick shared by both; up to 500 ms of dead latency per boot | `internal/vmutil/agent.go:102` |
| `GetNetworkEndpoint` returns a bare string | VZ returns the sentinel `"127.0.0.1"`; the interface lies about what the value means | `internal/vz/client.go:671` vs `internal/firecracker/client.go:1051` |
| Two near-identical metadata schemas | Drift risk; VZ carries/omits fields to mirror Firecracker | `internal/vz/metadata.go`, `internal/firecracker/metadata.go` |

---

## 2. The boot critical path

### 2a. Shared shape

Both backends run the same ordered chain inside `CreateShed`. None of it
is parallelized today:

```text
resolve image (pull if needed)      ── network-bound, 0–minutes
  → allocate writable upper          ── sparse file + fsync
    → [FC only] allocate CID/IP/TAP  ── netlink, privileged
      → launch VMM (vfkit/firecracker)
        → guest kernel boot
          → initramfs: mkfs.ext4 on fresh upper   ← SYNCHRONOUS, gates everything
            → overlay assemble + switch_root
              → systemd → shed-agent up
                → host health poll succeeds (500 ms tick)
                  → mount workspace (VirtioFS / 9P)   ← fatal on failure, no retry
                    → mount credentials (sequential)
                      → clone repo (if --repo)        ← non-fatal warning on failure
                        → provisioning hooks          ← non-fatal warning on failure
```

VZ entry points: `internal/vz/client.go:112` (`CreateShed`),
`internal/vz/vm.go:50` (`Start`), `internal/vz/vm.go:129`
(`buildVfkitArgs`). Firecracker entry points:
`internal/firecracker/client.go` (`CreateShed`), `internal/firecracker/vm.go`
(`Start`).

### 2b. Verified VZ numbers (this machine, 2026-05-26)

```bash
shed -s my-server create vztest --local-dir <tmp>   #  real 0m6.061s
```

with a warm image cache (no pull) and no `--repo`/provisioning. The
guest rootfs at the end is:

```text
overlay on / type overlay (rw, lowerdir=/lower, upperdir=/upper/data, workdir=/upper/work)
workspace on /workspace type virtiofs (rw)
```

So ~6 s is the floor for a trivial create on this hardware. We do **not
know the phase breakdown** of those 6 s — that is the entire point of
§9. The hypotheses below predict that in-guest `mkfs.ext4` and the
health-poll granularity are meaningful fractions of it.

### 2c. Phase cost hypotheses (UNMEASURED)

| Phase | Hypothesized cost | Why we suspect it |
|---|---|---|
| Image resolve (cached) | small | content-addressed, local |
| Image resolve (cold pull) | 0–minutes | network + registry; dominant variable cost |
| Upper alloc + fsync | 0.1–1 s | sparse `Truncate` + multiple `fsync` (`rootfs.go`) |
| VMM spawn | <1 s | subprocess / SDK start |
| Kernel boot | 1–? s | uninstrumented |
| **in-guest `mkfs.ext4`** | **1–several s** | synchronous format of a fresh 20 GB-logical ext4 on first boot |
| Health detection slack | 0–0.5 s | 500 ms poll granularity |
| Workspace + cred mounts | 0.5–2 s | post-boot exec + mount, sequential |

---

## 3. Headline win: kill the in-guest `mkfs.ext4`

This is the single change that pays off across **speed, disk, and
complexity simultaneously**, and it is a textbook "lean into platform
differences" move.

> **Status: implemented for VZ and measured (2026-05-26).** An A/B reboot
> (fresh upper vs. reused formatted upper) isolated the in-guest
> `mkfs.ext4` on the raw 5 GB virtio-blk device at **~4.2 s** — it is the
> dominant cost of a fresh boot, not the cheap operation a sparse-file
> `mkfs` test suggests. Pre-formatting the upper on the host (`mkfs.ext4`
> in the build-tools container) and CoW-cloning it per shed
> (`clone.CloneFile`, APFS clonefile) drops a warm-template create from
> **~5.9 s to ~1.7 s** (ext4 mounts at 0.1 s instead of 4.3 s), with the
> template only ~4 MB on disk for a 5 GB fs. See
> `internal/vz/uppertemplate.go`. The Firecracker mirror (reflink) and
> shipping a template with the image (instead of minting on the host) are
> open follow-ups.

### 3a. What happens today

Both backends allocate a per-shed sparse `upper.ext4`, stamp a
16-byte `FreshUpperSignature` at offset 1024 (`rootfs.go`), and rely on
the **initramfs inside the guest** to detect that signature and run
`mkfs.ext4` before the overlay can be assembled. Every brand-new shed
pays an in-guest format on its first boot, on the critical path, with no
host-side visibility or progress.

The `FreshUpperSignature` contract is duplicated verbatim in
`internal/vz/rootfs.go:119` and `internal/firecracker/rootfs.go:101`.

### 3b. Proposed model: host-side copy-on-write clone of a template

1. Build (or `mkfs` once, lazily) a **pre-formatted empty `upper.ext4`
   template** per size tier, stored in the image dir.
2. Per-shed create = **clone the template**, using each platform's
   native CoW primitive:
   - **macOS / APFS:** `clonefile(2)` — always available on APFS, near
     instant, shares blocks until divergence.
   - **Linux:** `ioctl(FICLONE)` reflink on XFS / Btrfs / reflink-ext4,
     with a plain copy (or the current in-guest `mkfs`) as fallback when
     the backing filesystem doesn't support reflink.
3. The guest mounts the upper directly — no signature, no in-guest
   `mkfs`, no initramfs detect-and-format branch.

### 3c. Why this is a triple win

- **Speed:** removes a synchronous in-guest format from every first
  boot; a CoW clone is effectively free.
- **Disk:** cloned uppers share extents with the template until written
  (same mechanism snapshots already use — see §6).
- **Complexity:** deletes the `FreshUpperSignature` contract and the
  initramfs format branch on *both* sides. Net code removed.

### 3d. Notes / risks

- The reflink helper likely **already exists** — snapshots reflink today
  (`docs/reference/snapshots.md`, the snapshot create path). This is
  reuse, not green-field.
- Reliability differs by platform: APFS `clonefile` is guaranteed; Linux
  reflink depends on the filesystem under `images_dir`. This *is* the
  per-platform divergence — guaranteed fast path on mac, best-effort
  with safe fallback on Linux.
- Variable `--size`: keep templates per size tier, or format the
  template at a base size and grow lazily (`resize2fs`). A fixed ext4
  UUID across clones is fine — each shed is a separate VM/kernel.

---

## 4. Other speed wins (after instrumentation confirms)

1. **Parallelize the independent prefix of `CreateShed`.** Image
   resolve and upper allocation are independent; on Firecracker, TAP/CID
   allocation is independent of the image. Today they run strictly
   serially (`internal/vz/client.go:227→276`, Firecracker analog).
2. **Event-driven readiness instead of polling.** Have `shed-agent`
   push a "ready" notification on the existing notify port the instant
   it is up, rather than the host polling every 500 ms
   (`internal/vmutil/agent.go:102`). Cheapest interim step: drop the
   tick to 100–200 ms. Real fix: push notification.
3. **Make post-boot mounts parallel and retriable.** Workspace and each
   credential mount run sequentially after agent-ready; the workspace
   mount is *fatal* (§5). They can fan out.

---

## 5. Stability / fragility hotspots

These are independent of speed — worth their own track if stability is
the priority.

### 5a. Process reaping and stop semantics (both backends)

- VZ launches `vfkit` and reaps it in a background goroutine that
  **logs but swallows** an unexpected exit; the caller never sees it
  (`internal/vz/vm.go` reap goroutine).
- `StopShed` marks `status=stopped` even if the process did not actually
  reap (SIGKILL sent, VMM hung). A later `StartShed` can then spawn a
  second process over a zombie.
- **Fix direction:** verify the PID is reaped before reporting success;
  surface reap errors into shed status.

### 5b. Firecracker networking (Linux-only, the most fragile surface)

`internal/firecracker/network.go` — TAP creation via netlink needs
`CAP_NET_ADMIN`; the `shed-br0` bridge must pre-exist; IP allocation is
offset-based (`gateway + index`) with **no conflict detection** against
external services. netlink ops can fail transiently with no retry.

- **Fix direction:** conflict detection on IP allocation, bounded
  retries on transient netlink failures, clearer teardown on partial
  failure. This is a Linux-native hardening task with no VZ analog —
  exactly the kind of thing that should *not* be abstracted into shared
  code.

### 5c. Fatal, non-retriable workspace mount

If the `--local-dir` mount fails, the whole create rolls back and
destroys the VM (`internal/vz/client.go` post-boot mount step). A
transient kernel hiccup costs the user the entire shed.

- **Fix direction:** bounded retry with backoff before declaring the
  mount failed.

### 5d. Timeout layering

A client-side create timeout (config default ~10 m) sits over the
backend's agent `StartTimeout` (90 s in this box's config,
`internal/config/server.go` default 60 s). A long cold image pull can
trip the inner timeout even though the outer budget is huge.

- **Fix direction:** separate the "pull image" budget from the "agent
  comes up" budget so they don't alias.

---

## 6. Disk optimization

The storage model is already strong: content-addressed blobs, a single
erofs lower shared across all sheds on a manifest, refcount GC, lz4
compression. Confirmed live: the v0.5.3 erofs ships as a **blob** in
`blobs/sha256/` (the 540 MB / 551 MB / 1.2 GB files), with `cache/` now
just empty scaffolding — the v0.5.2+ model works as designed.

Remaining levers:

1. **Reflink uppers** — folded into §3; uppers stop being full
   allocations and share extents with a template.
2. **erofs / lower cache eviction.** On VZ the manifest erofs is
   presented to `vfkit` as a file under `cache/sha256/<digest>.erofs`,
   materialized from the canonical blob. There is no eviction policy for
   these materializations or for unreferenced lowers beyond `prune`.
   Options (from `docs/discovery/layer-storage-optimization.md`): LRU on
   access time, refcount-zero eviction, or a manual
   `shed image prune --cache-only`. *(The exact VZ materialization
   mechanism — hardlink vs reflink vs copy, and when it's cleaned —
   should be confirmed in `internal/vmimage/manager.go`
   `resolveManifestLower` and the VZ lower-device path in
   `internal/vz/vm.go buildVfkitArgs`; observed behavior is that
   `cache/sha256` is empty at rest after a delete.)*
3. **Sparse-aware `df`.** Report `st_blocks × 512` (actual allocated
   blocks) instead of the logical upper size so `shed system df` tells
   the truth about a 20 GB-logical, 200 MB-real upper.
4. **Orphan sweeping on a schedule.** `internal/systemprune` already
   finds crashed-create `.creating` markers and orphaned upper files;
   make it run periodically, not only on demand.
5. **composefs (big, deferred).** Mount layer blobs directly via
   composefs metadata instead of a per-manifest flattened erofs, sharing
   blobs across manifests. ~20–30 % store savings in multi-manifest
   deployments but needs `mkcomposefs` tooling and is Linux-centric.
   Track, don't schedule.

---

## 7. Complexity reduction (do carefully, lower priority)

These reduce forced uniformity but trade refactor risk for modest gain.
Sequence them *after* the speed/stability wins, ideally piggy-backing on
work that already touches the area.

- **Make `GetNetworkEndpoint` honest.** Either introduce a
  `NetworkInfo{Type: nat|routable, Value}` type, or retire the string
  contract entirely and route all service traffic through `DialService`
  (VZ already requires `DialService` — the `"127.0.0.1"` it returns is a
  lie callers must special-case).
- **Split the metadata schema.** A shared core type with per-backend
  extension fields (Firecracker: CID, TAP, IP; VZ: PID) would stop the
  two `metadata.go` files from drifting.
- **Don't over-share the Dialer.** The `vmutil.Dialer` interface papers
  over genuinely different transports (per-port UDS vs vsock-CONNECT).
  It's a fine seam, but it should stay narrow enough that each backend
  can evolve its transport without dragging the other along — the
  opposite of forcing them together.

---

## 8. Quality / test gaps

From a scan of `_test.go` coverage:

- **Untested packages:** `internal/backend` (the interface itself),
  `internal/sshd`, `internal/sshconfig`, `internal/terminal`,
  `internal/version`.
- **Untested behaviors that matter most for the above work:**
  - `StopShed` edge cases — reaping, metadata-vs-process consistency
    (§5a).
  - `DialService` context-cancellation races.
  - VZ health-timeout interacting with `Stop()`.
- **If §3 lands**, the new host-side clone path needs solid tests on
  *both* branches: APFS `clonefile` (mac) and reflink-with-copy-fallback
  (Linux). It would sit on the create critical path for every shed.

`internal/firecracker` and `internal/vz` themselves are reasonably
covered (≈8–11 `_test.go` files each).

---

## 9. Measure first — the actual prerequisite

Nothing originally instrumented the boot phases. The wildly different
create-time estimates in early analysis (2–5 s vs 20–45 s) existed
precisely because the phases were invisible. **Before tuning anything,
add per-phase timing to `CreateShed`** across the chain:

```text
resolve-image  → upper-alloc → [net-alloc] → vmm-spawn →
kernel-boot → agent-ready → workspace-mount → cred-mount →
clone → provision
```

Timing is captured **server-side against one clock and logged to the
server log only** — it never travels on the SSE wire. SSE stays the
user-facing CLI progress channel (clean phase messages); the millisecond
breakdown is a developer signal read from the server log (over SSH for
remote hosts). This is small, low-risk, and turns §3–§6 from guesswork
into a ranked, data-backed backlog, plus a regression signal.

**Status: landed.** A `PhaseTimer` taps the existing `ProgressEvent`
boundaries (`internal/backend/phasetimer.go`), is installed for every
`CreateShed` (`internal/api` create handler), and logs one line per
create. The 6 s VZ create decomposes as e.g.:

```text
timing: create name=vztest backend=vz total=5744ms setup=0ms image=5ms \
  rootfs=3ms vm=1ms agent=5703ms mount=11ms credentials=17ms err=<nil>
```

confirming ~99 % of a warm create is the `agent` phase (guest boot incl.
in-guest `mkfs`) — which is what §3 targets.

---

## 10. Prioritized roadmap

| # | Item | Goal(s) | Risk | Depends on |
|---|---|---|---|---|
| 1 | Boot-phase instrumentation (§9) | speed (enables all) | low | — |
| 2 | Host-side CoW template upper (§3) | speed + disk + simplicity | medium | 1 (to prove the win) |
| 3 | Reaping / stop correctness (§5a) | stability | medium | — |
| 4 | Workspace mount retry (§5c) | stability | low | — |
| 5 | Parallelize create prefix (§4.1) | speed | medium | 1 |
| 6 | Event-driven readiness (§4.2) | speed | low–med | — |
| 7 | Firecracker network hardening (§5b) | stability (Linux) | medium | — |
| 8 | erofs/lower cache eviction (§6.2) | disk | low | — |
| 9 | Honest `GetNetworkEndpoint` (§7) | simplicity | low–med | — |
| 10 | composefs (§6.5) | disk | high | — |

**Recommended start:** #1 (unblocks everything, ~hours) then #2 (the
multi-win). #3/#4 can run in parallel as a stability track.

---

## 11. Appendix: verified facts (this machine, 2026-05-26)

- Host: Apple-Silicon mac, `shed 0.5.3` (Homebrew), `vfkit v0.6.3`,
  Docker 29.4.3, VZ backend.
- Guest: `Linux 6.8.0-117-generic aarch64` Ubuntu, user `shed`.
- Trivial create (warm cache, no repo/provision): **6.06 s** wall.
- Rootfs: overlay, `lowerdir=/lower` (manifest erofs blob),
  `upperdir=/upper/data`; `/workspace` is `virtiofs` and round-tripped a
  host file correctly.
- Image store after fresh v0.5.3 pull: 3.2 GB across `base` /
  `extensions` / `full`; erofs present as content-addressed blobs;
  `cache/sha256` empty at rest.
- Health poll interval confirmed at 500 ms (`internal/vmutil/agent.go:102`).
- `FreshUpperSignature` / in-guest mkfs contract confirmed identical in
  `internal/vz/rootfs.go:119` and `internal/firecracker/rootfs.go:101`.
- `GetNetworkEndpoint` returns `"127.0.0.1"` for VZ
  (`internal/vz/client.go:671`).

Firecracker-specific timings and line-level claims (§2c, §5b) are from
code review and need confirmation on a Linux/KVM host (e.g. `mini2` /
`mini3`).
