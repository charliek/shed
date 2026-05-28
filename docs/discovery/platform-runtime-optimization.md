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

## 0. Status & revised priorities (updated 2026-05-27)

This section supersedes the original priorities below where they conflict;
the later sections are kept for the reasoning trail.

### Released

- **v0.5.4** — shipped Phase 1 instrumentation (#118), Phase 2 CoW upper
  (#119), the network-setup fix (#123), and plugin distribution (#117).
  **Caveat:** Phase 2 was silently *inert* on fresh installs — see the
  build-tools-ref regression below.
- **v0.5.5** — fixes the Phase 2 activation (#124). **The VZ create
  speedup actually works as of 0.5.5.** Verified on the shipped brew
  binary from a clean state: warm `shed create` ~1.6 s (vs ~5.9 s pre-Phase-2).

> **Release rule going forward:** do NOT cut a release without discussion.

### Landed (on `main`)

- **Boot-phase instrumentation** (§9, PR #118) — server-side `PhaseTimer`
  logs one per-phase line per `CreateShed`. This is what made everything
  below measurable rather than guessed.
- **Host-side CoW template upper, VZ only** (§3, PR #119) — drops a warm
  VZ create from **~5.9 s → ~1.7 s** by skipping the in-guest `mkfs`.
- **`network-setup.sh` interface-rename fix** (item 3a, PR #123) —
  re-resolves the NIC name instead of latching one udev may rename
  (`eth0→enp0s1`). Validated on **both** VZ and Firecracker. Robustness
  fix + prerequisite for 3c.
- **build-tools ref v-prefix fix** (PR #124) — the activation fix for
  Phase 2; see §12.1.

> **Lesson (§12.1):** Phase 2 looked validated but wasn't, because dev
> testing used the `SHED_BUILD_TOOLS_REF` override and a cached template —
> both of which bypassed the real version-derived code path. The path that
> actually ships was never exercised until a clean-state, release-version
> test. **Validate the shipping path, not an overridden one.**

### Measured and ruled out

- **Firecracker CoW-upper mirror — NOT worth doing.** The in-guest
  `mkfs.ext4` that costs **~4.2 s on VZ** costs only **~0.18 s on
  Firecracker** (mini3, 2026-05-27: `/init` at 0.514 s, ext4 mounts at
  0.698 s). The cost is a **vfkit/VZ virtio-blk write-path
  characteristic** (~20× slower than Firecracker's), *not* a shared
  property of the `mkfs` step. So Phase 2 was correctly VZ-specific, and
  mirroring it to Firecracker would save ~0.18 s — not worth the code.
  This is the "lean into platform differences" thesis, quantified.
- **Phase 1c (guest `mkfs`/overlay sub-event) — shelved as low value.**
  It would decompose the kernel+initramfs lump, but that lump is now
  ~0.1 s on VZ (post-Phase-2) and ~0.7 s on Firecracker. No big lump left
  to attribute; `systemd-analyze` already covers the userspace side. High
  effort (initramfs + agent protocol + image rebuild) for little signal.

### The new top target (both backends)

With `mkfs` handled, the dominant remaining create cost is **guest
userspace boot to agent-healthy**. `shed-firstboot.service` looked like the
culprit (~0.97 s VZ / ~1.36 s FC), but §12 investigated + validated it and
found **firstboot is *not* the bottleneck**: the agent's real gate is
`network-setup.service` **waiting for DHCP (~1 s)**, and `shed-agent` is
vsock-only — it doesn't need an IP at all. Two byproducts of that
investigation:

- The latent `network-setup.sh` interface-rename race (`eth0→enp0s1`) is
  **fixed and shipped** (item 3a, #123, both platforms). The firstboot
  reorder is **dropped** (validated as not a win).
- **The real ~1 s win is item 3c** (the top remaining target): decouple
  *agent-healthy* (vsock) from *network-ready* (DHCP), gating `--repo`
  clone / provisioning on network separately. Concrete design + a
  ready-to-run kickoff plan are in **§13**.

### Update (2026-05-28): 3c measured — VZ has no win, FC firstboot reorder shipped instead

3c was implemented and benchmarked end-to-end (VZ on mac, FC on mini3,
shipping path: dev v0.5.5 server + locally-built `base` images, Phase-2
template active, template cache cleared once per §12.1). The measurement
**invalidates the premise** above. Full data in §14; the short version:

- **On VZ the literal 3c change (decouple `shed-agent.service` from
  `network-setup.service`) yields zero win** — `network-setup` was never
  the gate in the shipped config. The agent's actual gate is
  `shed-firstboot.service` (~633 ms → sysinit.target → basic.target → agent
  ~890 ms guest). `network-setup` runs *late* (982 ms) and *fast* (~12 ms)
  because DHCP completes before it runs.
- **The ~1 s VZ DHCP wait the doc projected was an artifact** of the
  firstboot-decoupled branch (where `network-setup` ran early and raced
  DHCP). That branch's "~1.058 s DHCP wait" was not present in the
  as-shipped config. We were measuring the side-effect of the workaround,
  not the bottleneck.
- **3c + reviving the firstboot reorder on VZ** moves the agent gate to
  systemd-resolved/sysinit (~650 ms guest), saving ~150 ms wall — but it
  also makes `--repo` creates **~450 ms slower** (network readiness no
  longer overlaps boot, so the host's `network-wait` pays for DHCP
  serially). Mixed-to-negative outcome on VZ; not shipped.
- **On FC the firstboot reorder alone is a clean ~20% plain-create win
  (~−450 ms median, every after sample beats every before sample), with
  no `--repo` regression.** FC uses a static IP (no DHCP), so leaving
  `network-setup` as the agent's gate keeps it fast and ordered correctly;
  clone has the network when it runs without any new host-side gate.

The shipping change (single file): `firecracker/shed-firstboot.service`
ordered `Before=ssh.service` only (the `ea78d53` text, FC-only). All other
3c artifacts (VZ unit edits, FC `network-setup` decoupling, the host-side
`WaitForNetwork`/`WaitForNetworkForHooks` machinery and its wiring) are
**not shipped**. This is "lean into platform differences" (§1c) made
concrete: VZ and FC have different bottlenecks; the fix lives where the
win actually is.

The `network-setup.sh` re-resolve fix (#123, both platforms) stands; so do
all prior status items. §13 is superseded by this update; §14 records the
measurements that drove it.

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
| Health readiness is a generic host poll | 500 ms tick shared by both; up to 500 ms of dead latency per boot | `internal/vmutil/agent.go` |
| `GetNetworkEndpoint` returns a bare string | VZ returns the sentinel `"127.0.0.1"`; the interface lies about what the value means | `internal/vz/client.go:686` vs `internal/firecracker/client.go:1051` |
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
> `internal/vz/uppertemplate.go`. Shipping a template with the image
> (instead of minting it on the host) is an open follow-up.
>
> **Firecracker does NOT need this** (measured 2026-05-27): its in-guest
> `mkfs` is ~0.18 s, because the slowness is vfkit/VZ's virtio-blk write
> path, not `mkfs` itself. The CoW mirror for Firecracker is ruled out —
> see §0.

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
   (`internal/vmutil/agent.go`). Cheapest interim step: drop the
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

| # | Item | Goal(s) | Risk | Status |
|---|---|---|---|---|
| 1 | Boot-phase instrumentation (§9) | speed (enables all) | low | ✅ shipped v0.5.4 (#118) |
| 2 | Host-side CoW template upper, VZ (§3) | speed + disk + simplicity | medium | ✅ shipped & working v0.5.5 (#119 + activation fix #124) |
| — | Firecracker CoW-upper mirror | speed (Linux) | medium | ❌ ruled out — FC `mkfs` ~0.18 s (§0) |
| — | Guest `mkfs`/overlay sub-event (1c) | observability | medium | ❌ shelved — lump too small now (§0) |
| 3a | `network-setup.sh` interface-rename fix (§12) | stability/robustness | low–med | ✅ shipped v0.5.5 (#123), validated VZ + FC |
| 3b | `shed-firstboot` time-to-agent reorder, **FC-only** | speed (~20% plain create, FC) | medium | ✅ shipped (PR #126) — `Before=ssh.service` on FC only; ~−450 ms (~20 %) plain-create win on mini3, no `--repo` regression. The 3b drop in §12 was based on a VZ-only measurement that exposed a DHCP race; FC has no DHCP wait. See §14. |
| 3c | ~~Decouple agent-healthy from network-setup/DHCP (§13)~~ | speed | — | ❌ superseded — implemented + benchmarked (§14); the literal change buys 0 ms on VZ (firstboot is the gate, not DHCP) and ~150 ms / **−450 ms `--repo` regression** when combined with the firstboot reorder. The realizable win on VZ is capped by fixed VMM/kernel overhead. FC's win is captured by 3b above. |
| 4 | Reaping / stop correctness (§5a) | stability | medium | open |
| 5 | Workspace mount retry (§5c) | stability | low | open |
| 6 | Parallelize create prefix (§4.1) | speed | medium | open |
| 7 | Event-driven readiness (§4.2) | speed | low–med | partial (poll 500→150 ms in #118) |
| 8 | Firecracker network hardening (§5b) | stability (Linux) | medium | open |
| 9 | erofs/lower cache eviction (§6.2) | disk | low | open |
| 10 | Honest `GetNetworkEndpoint` (§7) | simplicity | low–med | open |
| 11 | composefs (§6.5) | disk | high | open |

**Next** (2026-05-28 update): item **3c is superseded** — see §14. **3b
shipped FC-only** in PR #126 (~20 % FC plain-create win). The remaining
open items split into a **speed** track (#6 parallelize create prefix,
#7 finish event-driven readiness) and a **stability** track (#4
reaping/stop, #5 mount retry, #8 FC network hardening), plus **disk** (#9
cache eviction) and **simplicity** (#10 honest GetNetworkEndpoint). #11
composefs stays deferred. There is no obvious next big speed win on VZ —
the ~450 ms of fixed VMM/kernel/initramfs overhead + 150 ms host-poll
latency is the floor without changing vfkit or the boot path itself.

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
- Health poll interval was 500 ms at the time of this snapshot; lowered
  to 150 ms in #118 (`internal/vmutil/agent.go`).
- `FreshUpperSignature` / in-guest mkfs contract confirmed identical in
  `internal/vz/rootfs.go:119` and `internal/firecracker/rootfs.go:101`.
- `GetNetworkEndpoint` returns `"127.0.0.1"` for VZ
  (`internal/vz/client.go:686`).

## 12. `shed-firstboot` — investigation, root cause, and the call

**Goal:** the largest remaining create-time cost (§0). `shed-firstboot` is
a oneshot (`cmd/shed-firstboot`) that, on a shed whose recorded identity
doesn't match the `shed.name=` cmdline, sets the hostname and regenerates
the SSH host keys (`ssh-keygen -A`) so every shed has unique keys. Its
unit is ordered `Before=sysinit.target … systemd-journald.service
ssh.service shed-agent.service` and `WantedBy=sysinit.target`
(`vz/shed-firstboot.service`, `firecracker/shed-firstboot.service`,
identical) — so it gates **all** of boot, including the agent that
`shed create` waits on.

### Root cause (measured 2026-05-27)

`systemd-analyze blame`: `shed-firstboot.service` = **0.967 s (VZ) /
1.361 s (FC)**. But the work decomposes as:

- RSA-3072 keygen: **~220 ms** CPU; ecdsa/ed25519: ~1 ms each.
- hostname write + identity JSON: sub-ms.

The ~700 ms–1.1 s remainder is **`ssh-keygen` blocking on `crng`
initialization**. The guest has **no entropy source**: `hw_random`
current = none, `virtio_rng` is `CONFIG_HW_RANDOM_VIRTIO=m` (module, not
loaded early), the VZ-exposed ARM CPU does not advertise `rndr`
(`random.trust_cpu` has nothing to trust), and `efi: UEFI not found` (no
bootloader RNG seed). Result: `random: crng init done` lands at **~4.98 s**,
and `ssh-keygen`'s `getrandom()` blocks until then.

### Options considered

1. **Host-side entropy (virtio-rng / `random.trust_cpu` / EFI seed).** The
   clean fix — but **not available on VZ**: `vfkit v0.6.3` exposes no rng
   device flag, no CPU RNG, no UEFI. Dead end without a vfkit upgrade.
   (Firecracker *does* support an entropy device and its custom kernel
   could build in virtio-rng — a Linux-only follow-up, but FC create
   isn't `mkfs`-bound and its firstboot is the same shape.)
2. **ed25519-only host keys.** Saves the ~220 ms RSA CPU but **not** the
   `crng` wait (`getrandom` blocks regardless of key type/size). Small
   win, plus SSH-compat risk. Rejected.
3. **Decouple `ssh-keygen` from the agent/create critical path.** Keep
   key regen `Before=ssh.service` (correctness for SSH preserved) but stop
   it gating `sysinit.target`/`shed-agent.service`, so the agent (and thus
   `shed create`) no longer waits for the `crng`-blocked keygen. The
   subtlety: today firstboot is early specifically so the **hostname** is
   set before `journald` caches it; decoupling means relaxing that
   (journald may log the transient hostname for ~1 s — cosmetic) or
   splitting firstboot into a fast hostname/identity oneshot (stays early)
   plus a keygen oneshot ordered only before `sshd`.

### Risk / reward call

**Reward:** firstboot's blame time is 0.967 s (VZ) / 1.361 s (FC), but the
*realized* create win is smaller because of boot overlap — measured at
**~0.5 s on VZ** post-Phase-2 (firstboot exits 0.859 s, agent starts
0.899 s), more on FC. Still the top remaining create-time cost, and it's a
shared unit (one change helps both backends).

**Risk:** the fix is a **systemd-ordering change to the reliability-critical
first-boot path** — exactly the "key feature" that must stay correct.
Ordering bugs here are high-consequence: the rootfs ships with baked-in
host keys, and firstboot's job is to regenerate them per-shed *before*
`sshd` starts. If the new ordering ever lets `sshd` start before the
regen, every shed would serve identical (shared) host keys — a security
regression. So the keygen-before-sshd ordering is the invariant that must
be validated, not just asserted.

**Original call (2026-05-27): proceed + validate.** Superseded by the
validation finding below.

### Validation finding (2026-05-27): exposed a latent network-setup race — DO NOT ship alone

Implemented the reorder (`Before=ssh.service` only;
branch `optimize/firstboot-ordering`), built a VZ `base` image locally,
and created sheds. Outcome:

- **Security invariant held**: `ssh.service` started at 1.333 s, after
  `shed-firstboot` exited at 1.322 s — per-shed keygen still completes
  before sshd.
- **But create took ~31 s** (`shed-agent` started at 30.8 s).
  `network-setup.service` hung for **30.3 s**, and the agent waits on it.

**Mechanism.** `vz/network-setup.sh` resolves the NIC name, but the kernel
renames `eth0 → enp0s1` (predictable naming) at **0.603 s**. Originally
`shed-firstboot` was `Before=network-setup.service`, so its slow
`crng`-blocked keygen delayed network-setup until *after* the rename —
network-setup saw `enp0s1` and worked. Removing that edge let network-setup
start at **0.384 s**, *before* the rename; it captured `eth0`, which then
vanished ("Device eth0 does not exist"), and it polled for 30 s.

So **firstboot's early-blocking was accidentally masking a latent
interface-rename race in `network-setup.sh`** (it captures the interface
name once instead of re-resolving / waiting for udev to settle).

**Revised call: do NOT ship the firstboot reorder on its own.** It turns a
hidden race into a 30 s hang. The reward (~0.5 s on VZ, more on FC) does
not justify shipping a two-part change to the boot path unsupervised.
Sequence instead:

1. **Fix `network-setup.sh` first** (both backends — the FC one hardcodes
   `eth0` too): re-resolve the interface each poll iteration, or order the
   unit `After=systemd-udev-settle.service` / a `*.device` unit, so it can
   never latch a name that udev later renames. This is a standalone
   robustness fix and removes a real latent bug (any future change that
   shifts network-setup earlier would hit it).
2. **Then** the firstboot reorder is safe, and the two can be validated
   together (≥2 creates per platform + timing + host-key uniqueness).

The branch `optimize/firstboot-ordering` is left **unmerged** pending (1).

### Update (2026-05-27, cont.): network-setup fixed, reorder validated — NOT a win; real bottleneck found

Implemented (1) the `network-setup.sh` fix (re-resolve the interface each
pass; VZ + FC) and (2) the firstboot reorder together on branch
`optimize/fast-firstboot`, built a VZ image, and measured (2 creates):

- **network-setup fix works**: no 30 s hang; interface `enp0s1`, IP
  assigned, host keys unique per shed, sshd after firstboot. Good
  standalone robustness fix (commit `5008d5a`).
- **The firstboot reorder is NOT a win.** firstboot decoupled cleanly
  (exits 0.921 s, off the agent path), but create got *slower*
  (~2.15 s vs ~1.8 s): `shed-agent` now starts at **1.563 s**, gated by
  `network-setup.service` (exits 1.559 s, spends **1.058 s waiting for
  DHCP**). firstboot was never the real bottleneck — it merely ran
  concurrently with the DHCP wait. Removing it just exposed that wait on
  the critical path.

**Real bottleneck (the actual ~1 s win):** `shed-agent` talks over
**vsock and needs no IP**, yet `network-setup.service` is
`Before=shed-agent.service`, so the agent waits for DHCP. And the IP-wait
inside `network-setup.sh` is **purely informational** (it only logs
"Network ready"; systemd-networkd performs DHCP independently). So the
agent is blocked ~1 s on a wait it doesn't need.

**Next target:** decouple *agent-healthy* (vsock, fast) from *network-ready*
(DHCP). Care required: `--repo` clone and provisioning hooks run after
agent-healthy and **do** need the network, so they must gate on
network-ready themselves rather than relying on the agent's start
ordering. This is a create-flow change, not just unit ordering — design +
both-platform validation needed.

**Disposition:** keep the `network-setup.sh` fix (validated on VZ; pending
FC validation before merge). **Drop the firstboot reorder** (`ea78d53`) —
it doesn't help. Roadmap item 3b is superseded by 3c (agent/DHCP
decoupling).

---

## 12.1 build-tools-ref regression — the v0.5.4 Phase-2 miss

v0.5.4 shipped Phase 2 but it was **inert on fresh installs**. The upper
template is minted by `mkfs.ext4` in the `shed-build-tools` container, and
the VZ code resolved that image ref by string-concatenating
`version.Version`: `"ghcr.io/charliek/shed-build-tools:" + Version`.
Release binaries embed `Version="0.5.4"` (no leading `v`), but the
published tags are **v-prefixed** (`v0.5.4`). So the mint requested
`:0.5.4` → `docker ... exit status 125` → fall back to slow in-guest
`mkfs.ext4`. A clean v0.5.4 `shed create` ran ~6.4 s, not the advertised
~1.7 s.

Root causes:

1. **Divergent duplicated logic.** `cmd/shed/image.go` resolved the ref
   correctly (v-prefix + a `^v?\d+\.\d+\.\d+$` regex); `internal/vz`
   reimplemented it and got the prefix wrong. Fix (#124): one canonical
   resolver, `version.BuildToolsRefForTag` / `version.ReleaseBuildToolsRef`,
   used by both call sites.
2. **The test exercised an overridden path, not the shipping path.** All
   "Phase 2 works" validation used `SHED_BUILD_TOOLS_REF=shed-build-tools:dev`
   (env override, bypasses version resolution) and/or a cached template
   from a prior run (skips the mint entirely). The version-derived mint —
   the only path a real release takes — was never run until a clean-state,
   release-version (`Version=0.5.4`) test. **When validating, reproduce the
   shipping path: no overrides, no warm caches, a release-shaped version.**

Fixed in #124, shipped in v0.5.5; verified on the shipped brew binary from
a clean state (template minted via `build-tools:v0.5.5`, create ~1.6 s).

### Appendix B: follow-up measurements (2026-05-27)

VZ (this mac), in-guest `mkfs` A/B via stop/start:

| boot | `mkfs`? | ext4 mounts at | kernel time |
|---|---|---|---|
| fresh upper | yes | 4.29 s | 4.325 s |
| reused upper | skipped | 0.096 s | 113 ms |

→ in-guest `mkfs` on the raw 5 GB virtio-blk device ≈ **4.2 s** on VZ.
Post-Phase-2 (template clone) a warm create is **~1.7 s**.

Firecracker (mini3, fresh upper):

- `/init` at 0.514 s, `EXT4-fs (vda): mounted` at 0.698 s → in-guest
  `mkfs` ≈ **0.18 s** (not a bottleneck; CoW mirror ruled out).
- `systemd-analyze blame` top unit: `shed-firstboot.service` **1.361 s**.
- Trivial create wall time **~3.7 s** (kernel+initramfs ~0.7 s, rest is
  userspace to agent-healthy).

`shed-firstboot.service` is the biggest single userspace unit by
`systemd-analyze blame` on both (Firecracker 1.361 s, VZ 0.967 s) — but
blame time ≠ critical-path time: §12 found the agent's actual gate is
`network-setup.service`'s DHCP wait, not firstboot. See §12.

Firecracker-specific timings and line-level claims (§2c, §5b) are from
code review and need confirmation on a Linux/KVM host (e.g. `mini2` /
`mini3`).

---

## 13. Next session: 3c kickoff plan (decouple agent-healthy from network/DHCP)

> **SUPERSEDED — see §0 update (2026-05-28) and §14.** The plan below was
> implemented and benchmarked. Its core premise (a ~1 s VZ DHCP wait on
> the agent's critical path) was a misattribution; the agent's real gate
> is `shed-firstboot`, not `network-setup`. The literal 3c change is not
> shipped. The actual shipped change is `firecracker/shed-firstboot.service`
> ordered `Before=ssh.service` only (FC-only) — §14d. This section is kept
> as the reasoning trail; do NOT use it as a kickoff plan.

**Goal.** `shed-agent` talks over **vsock and needs no IP**, but
`network-setup.service` is `Before=shed-agent.service`, so the agent (and
thus `shed create`) waits ~1 s for DHCP it doesn't need. Decouple them so a
**plain `shed create` is ~1 s faster on both backends** (~1.7 s → ~0.7 s on
VZ; ~3.7 s → ~2.7 s on FC). `--repo` / provisioning creates stay the same
(they wait for the network right before they use it).

### Create-flow facts (verified, do not re-derive)

- Order in `internal/vz/client.go` `CreateShed` (FC analogous): `vm.Start`
  → `agent.WaitForHealth` (gates create) → mount workspace (VirtioFS/9P,
  vsock) → `SetupCredentials` (vsock) → `CloneRepo` (`git clone` — **needs
  network**) → `RunProvisioning` (hooks — **may need network**).
- `shed-agent.service` is `After=network.target` (passive). What makes it
  wait for DHCP is `network-setup.service` being `Before=shed-agent.service`
  (`vz/network-setup.service`, `firecracker/network-setup.service`).
- Today, network-readiness before clone/provision is *implicit* (the agent
  waited for network-setup). Decoupling removes that guarantee, so the
  create flow must re-establish it explicitly — **this is the correctness
  crux.**

### Design

1. **Boot ordering (guest, both backends):** remove `shed-agent.service`
   from `network-setup.service`'s `Before=` so the agent starts as soon as
   its own deps are met (early), not after DHCP. (Leave network-setup
   itself running; just stop it gating the agent.)
2. **Create flow (server):** before `CloneRepo` and before
   `RunProvisioning`, wait for guest network-ready — only when those steps
   actually run. A plain create skips the wait entirely. Implement once in
   shared code (`internal/vmutil`) so VZ and FC share it; the check can
   `agent.Exec` a tiny guest probe (poll for a default route / an IPv4
   address, bounded timeout) or wait on `network-online.target`.
3. Keep the existing per-phase timing; add a `network-wait` phase so the
   win (and the `--repo` cost) are both visible in the server log.

### Mandatory process for this work (per the owner)

- **PR / review / merge:** open a PR, run **`/git-commands:watch-pr`**,
  ensure a real review (CodeRabbit; if it's rate-limited/out of credits,
  fall back to **`/codex:rescue`** then **`/cursor:review`**, or a
  sub-agent self-review), address findings, then **`/git-commands:merge-pr`**
  once green. See memory `pr-code-review-workflow`.
- **No release without discussion.** Land 3c on `main`; do **not** tag a
  release. (Note: like all guest-image changes, 3c only takes effect once
  images are rebuilt at the next release.)
- **Benchmark the expected benefit on BOTH OSes**, the same way the prior
  items were validated: capture before/after per-phase timing showing the
  agent/create win on **mac (VZ)** and **Linux (FC, mini3)**. Reproduce the
  **shipping path** — no `SHED_BUILD_TOOLS_REF` override, no warm template
  cache, a release-shaped version when build-tools is involved (see §12.1).
- **Before each PR, start at least 2 sheds on each OS** (VZ on the mac, FC
  on mini3) and confirm they boot and work — including at least one
  **`--repo` create per OS** (the correctness crux: clone must still have
  the network).

### Gotchas / risks

- Validating a guest-unit change needs the unit in the booted image: build
  a rootfs image (VZ locally; FC on mini3 — note **mini3 has no `go`
  toolchain**, so either install Go, cross-compile `shed-agent` on the mac
  and stage it, or exercise the changed unit on a real boot via an
  **overlay override** of the unit file, as done for the network-setup fix).
- The network-ready probe must be reliable and bounded (don't reintroduce a
  30 s hang); fail with a clear error, not an indefinite wait.
- Confirm nothing else between agent-healthy and clone implicitly assumes
  the network is up.

### Ready-to-paste kickoff prompt for a new session

> Implement roadmap item **3c** from
> `docs/discovery/platform-runtime-optimization.md` (§13): decouple
> `shed-agent` (vsock, needs no IP) from `network-setup`/DHCP so a plain
> `shed create` is ~1 s faster on both backends, while `--repo` clone and
> provisioning still wait for the network. Follow the design and the
> mandatory process in §13: PR → `/git-commands:watch-pr` → review (
> CodeRabbit, else `/codex:rescue` / `/cursor:review` / sub-agent) →
> `/git-commands:merge-pr`; **no release without discussion**; benchmark
> before/after per-phase timing on **both mac (VZ) and Linux (FC/mini3)**
> reproducing the shipping path; and **before each PR, start ≥2 sheds on
> each OS** (including one `--repo` create per OS) to confirm no
> regression. Read §0, §12, §12.1, and §13 first for context.

---

## 14. 3c benchmark — measured data and what it taught us (2026-05-28)

Per §13's mandatory process, 3c was implemented end-to-end and benchmarked
on both backends, reproducing the shipping path (release-shaped
`Version=v0.5.5` in the dev `shed-server` so the upper-template mint
resolves `shed-build-tools:v0.5.5` naturally — see §12.1 — and the
`templates/` cache cleared once so the mint actually ran). Findings drove
the §0 update; this section is the record.

### 14a. VZ (this mac) — 3c does not realize the projected win

Measured progression on warm plain creates (`shed create … --image base`),
`agent` phase from the server-side `PhaseTimer`:

| Config | `agent` phase | Notes |
|---|---|---|
| Shipped v0.5.5 (brew, baseline) | 1501–1655 ms (median ~1503 ms) | Boot critical-chain: shed-firstboot (633 ms) → sysinit → basic → shed-agent @890 ms guest. |
| 3c only (network-setup decoupled from agent) | 1502 / 1503 / 2104 ms | **No change.** With firstboot still gating sysinit, `network-setup` runs *late* (982 ms guest) and *fast* (~12 ms) — DHCP already done by then. |
| 3c + firstboot reorder (both off agent path) | 1352 ms ×3 | ~150 ms wall. Agent now gated by systemd-resolved/sysinit (~650 ms guest). Fixed VMM+kernel+initramfs (~500 ms) + 150 ms health-poll dominate the rest. |

`--repo` create on the combined config **regressed** by ~450 ms (1856 ms
shipped → 2305 / 2312 ms after): with firstboot decoupled, `network-setup`
runs early and hits the inherent ~1 s DHCP wait while the agent comes up
~1.2 s. The host then pays `network-wait=756 ms` serially before clone,
where the shipped config absorbed the DHCP wait inside the boot. Network
readiness on VZ is DHCP-bound regardless — decoupling just changes who waits.

Host-key uniqueness (the firstboot security invariant) was preserved across
3 sheds (distinct ed25519 fingerprints; `ssh.service` ActiveEnter at
1176 ms > `shed-firstboot` exit at 1160 ms).

**Conclusion for VZ:** the projected ~1 s DHCP wait on the agent path was a
**misattribution** — it was measured on the firstboot-decoupled branch
(`optimize/fast-firstboot`) where `network-setup` ran early and raced DHCP.
In the shipped config that race doesn't exist; firstboot is the real gate.
Removing both gates buys only ~150 ms (fixed overhead dominates) and the
network-wait gate makes `--repo` worse. **Not shipped.**

### 14b. FC (mini3) — firstboot reorder alone is a clean ~20% win

Apples-to-apples: same dev `shed-server` (v0.5.5, built on mini3 from
this tree), same `shed-build-tools:v0.5.3` for the erofs mint, same image
build pipeline. The only difference between BEFORE and AFTER is the
`firecracker/shed-firstboot.service` `Before=` line. 5 plain creates per
phase, run sequentially within each phase (BEFORE block first, AFTER
block second) — not interleaved per sample. The fb*/fa* ranges overlap
in pattern, so order-of-day cache effects don't explain the median delta
(but a follow-up interleaved run would be the rigorous next step):

| Plain `create` (5 samples) | Median `agent` | Mean `agent` | Range | Median total |
|---|---|---|---|---|
| BEFORE (baseline image) | 2256 ms | 2345 ms | 2105–2705 | 2289 ms |
| AFTER (firstboot reorder) | **1804 ms** | **1774 ms** | 1654–1806 | **1839 ms** |
| **Δ** | **−452 ms (~20 %)** | −571 ms | — | −450 ms |

Every "after" sample beats every "before" sample.

`--repo` with the firstboot-reorder image (3 samples): total **2106 ms**
median, `agent` 1652–1802 ms, `clone` 411–462 ms. **No regression** — and
no `network-wait` phase (no host-side gate, no extra Go code path). FC's
static IP means `network-setup` stays fast and still gates the agent, so
clone has the network when it runs.

Boot order verification (kernel/journald, FC AFTER):
`Starting network-setup` + `Starting shed-firstboot` (parallel) →
`Finished network-setup` → `Reached sysinit.target` → `basic.target` →
`Started shed-agent` → … → `Finished shed-firstboot` (later). firstboot is
off the agent's critical path; `Before=ssh.service` still ensures
firstboot is ordered before sshd starts (security ordering invariant
preserved — see the failure-mode caveat in §14e).

The `~3.7 s` FC baseline in §11 / Appendix B predates this mini3
configuration; current mini3 plain create is ~2.3 s shipped, so the
firstboot reorder buys ~20 % rather than the ~1 s the doc projected. The
win is smaller in absolute terms but real, repeatable, and showed no
`--repo` regression across the samples taken (3 `--repo` creates after).

### 14c. What 3c got right, what it got wrong

Right:
- Validating end-to-end on the **shipping path** caught both the §12.1
  build-tools-ref class of footgun (avoided here by versioning the dev
  server as `v0.5.5`) and the misattribution above.
- Recognising that `shed-agent` is vsock-only and doesn't need an IP.
- The `network-setup.sh` re-resolve fix (#123) is a real robustness win
  and stays.

Wrong:
- Attributing the ~1 s VZ cost to a DHCP wait on the agent's critical path.
  It is not there in the shipped config; firstboot is the gate.
- Projecting ~1 s on FC from blame numbers. Mini3 today is faster than the
  reference; the realizable win is ~20 % (~450 ms), not ~50 %.
- Treating "decouple `network-setup` from agent" as the lever. On VZ it
  buys nothing and regresses `--repo`; on FC the lever is firstboot and
  `network-setup` should stay where it is.

### 14d. Shipped change

`firecracker/shed-firstboot.service` only: `Before=ssh.service` (the
`ea78d53` text from §12), FC-only. No host-side gate. No VZ unit change.
The PR also (i) updates `cmd/shed-firstboot`'s package comment to
describe the per-backend divergence, (ii) adds an inline
`Before=shed-agent.service` guardrail comment to
`firecracker/network-setup.service` (the line whose accidental loss or
slowdown would re-create the VZ-style `--repo` regression on FC), and
(iii) adds `internal/vmutil/firecracker_firstboot_service_test.go` — a
Go test that asserts the FC firstboot unit contains `Before=ssh.service`
AND bans the three previously-removed `Before=` tokens (`sysinit.target`,
`shed-agent.service`, `network-setup.service`). The test runs on every
CI build and locks in the single load-bearing line of this PR.

### 14e. Security-invariant honesty: what `Before=` guarantees

`Before=ssh.service` is an ORDERING edge, not a `Requires=`/`BindsTo=`
edge. systemd treats an ordered unit as "finished starting" after either
success OR failure (see `systemd.unit(5)`), so:

- **What we preserve (same as shipped):** sshd starts AFTER firstboot
  exits, regardless of firstboot's exit status. If firstboot succeeds
  (the overwhelming common case), per-shed host keys are in place and
  sshd serves them.
- **What we do NOT preserve, and never did:** sshd does NOT refuse to
  start if firstboot fails. `cmd/shed-firstboot/firstboot.go`
  `regenerateIdentity` removes the baked-in stale keys *before* running
  `ssh-keygen -A`; if `ssh-keygen -A` fails mid-regen, the rootfs is left
  with no/partial host keys and sshd's behavior is then up to sshd
  itself (typically: refuse to start, or auto-generate ephemeral keys
  depending on `HostKey` directives). The pre-PR-#126 unit had identical
  failure-mode semantics — the broad `Before=` list also did not
  propagate failure. This PR does not introduce a regression here.
- **Strengthening option (deliberately not in this PR):** adding a drop-in
  on `ssh.service` with `Requires=shed-firstboot.service` (or
  `BindsTo=`) would make sshd refuse to start on firstboot failure. That
  is a strict behavior change versus shipped (sheds with a broken
  firstboot would be unreachable via SSH instead of reachable with
  potentially-shared keys). Worth a separate, discussed PR if the
  tradeoff is desired; out of scope here.

The added Go test (§14d) asserts the *ordering edge* only. A future
follow-up that wants fail-closed semantics should also extend the test
to assert the `Requires=` drop-in or equivalent.

### 14f. Rollback

The shipped change is a single `Before=` line in one unit file. To
revert: restore the prior
`Before=sysinit.target systemd-machine-id-commit.service systemd-journald.service ssh.service shed-agent.service network-setup.service`
line in `firecracker/shed-firstboot.service`, rebuild the FC rootfs
image (e.g. `./scripts/build-firecracker-rootfs.sh --variant base
--build-tools-version vX.Y.Z`), and republish. The corresponding test in
`internal/vmutil/firecracker_firstboot_service_test.go` would also need
to be updated (or deleted) to reflect the reverted state. No data
migration, no on-disk format change.
