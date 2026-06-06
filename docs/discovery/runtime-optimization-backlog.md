# Runtime optimization backlog & boot-path invariants

Status: **Living backlog.** This is the slim successor to two retired
discovery docs — `platform-runtime-optimization.md` (the create-latency /
boot-path engineering journal) and the runtime/storage open items from
`layer-storage-optimization.md`. Their *shipped* content (v0.5.4–v0.6.0:
PhaseTimer, the CoW upper template, healthPoll tightening, the firstboot
reorder, the orchestrator refactor, the integration suite) is captured in
the CHANGELOG, the code, and the tests. What survives here is the part
that still has forward value: the **open backlog** and the **durable
invariants** that source comments and tests point at.

The storage-strategy ideas from the old docs (VirtioFS-lowers, squashfs)
were **dropped** during cleanup because they conflict with the chosen
direction — see [`lazy-rootfs-streaming.md`](lazy-rootfs-streaming.md),
which also records composefs as a dedup-axis alternative.

---

## Part 1 — Open backlog

Each item is vetted against the current direction. They are mostly
*orthogonal* to whichever storage strategy we pick, which is why they
survived the cull.

### Parallelize the create prefix

`CreateShed`'s independent prefix runs strictly serially today: image
resolution, then writable-upper allocation, then (Firecracker) TAP/CID
allocation. These have no data dependency and could run concurrently.
(Note: PR #177's parallel *blob downloads* is image-fetch parallelism —
a different thing; the create-prefix parallelization is still open.)
**Caveat:** measure against the `agent_p50` timing gate; the win is small
and must not destabilize the floor.

### Event-driven agent readiness

The host polls the agent for readiness (tightened to a 50 ms interval —
see [healthPoll](#per-backend-timing-floors-and-the-healthpoll)). The
"right" fix is for `shed-agent` to **push** a ready notification on the
existing notify port instead of being polled. The poll shipped as the
pragmatic interim; the push design did not.

### Firecracker IP-conflict: residual (active probe)

Most of the original FC-network gap is now closed: allocation skips an
index whose IP is already claimed on the host (passive `AddrList` + bridge
`NeighList` check), TAP setup retries transient netlink errors, and the
create path tears down partial state via the LIFO cleanup stack.

The **residual** is the one case the passive check can't see: an IP held by
a *silent* host that has never been ARP'd and isn't a host interface.
Catching that needs an **active ARP probe**, deliberately *not* done — it
would add a fixed wait to the create hot path (the `agent_p50` gate).
Revisit only if duplicate-IP incidents actually occur on a shared-bridge
deployment.

> The earlier "stop/reap correctness" item was removed: verification
> against current code + a live VZ stop/start showed it is already fixed
> (verify-before-flip on stop, `ErrZombiePresent` guard on start; PRs
> #151/#156).

### Blob & lower cache eviction

The content-addressed blob store grows without bound; `shed image prune`
is reachability-only (it GCs unreferenced blobs) — there is no
size-capped or age-based eviction. Designs considered (reframed for the
v0.5.2+ blob model, since the old per-host `cache/<digest>.erofs` file is
gone): **LRU on access time**, **refcount-zero eviction**, or a manual
`shed image prune --cache-only`. **This becomes more important under
[strategy C](lazy-rootfs-streaming.md)** — a lazy chunk cache *needs* an
eviction policy — so treat it as a near-prerequisite if that path is ever
taken.

### vsock-first console; move sshd outside the boot transaction

`shed console` is SSH-based today. A vsock-first console would remove the
SSH dependency for the local-console case, and — the larger payoff — let
sshd move **outside the agent boot transaction** (lazy / socket-activated)
so the agent's readiness gate no longer waits on sshd coming up.

### Minor housekeeping

- **Sparse-aware `df`** — report `st_blocks × 512`, not apparent size, so
  the sparse erofs/template files aren't over-counted (the upper template
  is a 5 GB-apparent / ~4 MB-real sparse file, for example).
- **Scheduled orphan sweeping** in `internal/systemprune` (beyond the
  on-demand path).
- **Build-script consolidation** — `build-vz-rootfs.sh` and
  `build-fc-rootfs.sh` still duplicate much of their logic.
- **Vestigial-code audit** — `--from-snapshot` / `StartShed` paths after
  the orchestrator refactor.
- **Metadata-schema split** — separate the on-disk metadata schema from
  the wire shape.

---

## Part 2 — Durable invariants & learnings

These are **referenced from source comments and tests** (the boot-ordering
test failure messages deliberately link here so a contributor has
"somewhere to read before changing the line"). Do not delete this section
without rehoming what cites it.

> **Section-number breadcrumbs.** Some source comments still cite the
> retired `platform-runtime-optimization.md` by its old section numbers.
> They map here as: **§12** → [network-setup NIC re-resolution](#network-setup-nic-re-resolution);
> **§14 / §14a / §14b / §14e** → [guest boot-ordering invariants](#guest-boot-ordering-invariants);
> **§15a** → [per-backend timing floors and the healthPoll](#per-backend-timing-floors-and-the-healthpoll);
> **§9** → [PhaseTimer boot instrumentation](#phasetimer-boot-instrumentation);
> **§16** → [integration test suite design](#integration-test-suite-design).

### Guest boot-ordering invariants

The guest systemd unit files baked into the VZ and Firecracker rootfs
images encode ordering decisions that look arbitrary but are load-bearing.
Locked by `internal/vmutil/guest_unit_ordering_test.go` (pure file
parsing, no VM booted). Origin: PR #126/#127.

- **Firecracker firstboot is `Before=ssh.service` only** (not broadly
  ordered). FC's agent-readiness gate is the static-IP `network-setup`
  path, so firstboot should not re-gate the agent behind an earlier-boot
  unit. Adding a broad `After=`/`Before=` here re-creates a measured
  regression.
- **VZ firstboot is intentionally left untouched** (broad ordering kept).
  VZ's gate is firstboot itself; the FC reorder does not apply.
- **`WantedBy=` is mandatory in `[Install]`.** Two concrete failure modes
  the tests guard:
  - Without `WantedBy=sysinit.target`, per-shed SSH host keys are never
    regenerated → every shed would serve the **baked-in keys**.
  - Without `WantedBy=multi-user.target`, the `Before=shed-agent.service`
    guardrail is unreachable → the agent could start before the network
    is configured.
- **Honesty note (security):** `Before=` is *ordering*, not `Requires=`.
  sshd does **not** fail-closed if firstboot fails; the tests assert the
  ordering edge only. A fail-closed `Requires=shed-firstboot` was
  considered and deliberately not shipped.
- **"Blame ≠ critical-path time."** `systemd-analyze blame` flagged
  firstboot as the biggest unit, but the agent's real gate was twice
  misattributed (firstboot vs network-setup/DHCP). The corrected model —
  VZ gated by firstboot, FC gated by the static-IP `network-setup` — is
  *why* the firstboot fix is FC-only.

### Per-backend timing floors and the healthPoll

- **healthPoll = 50 ms** (`internal/vmutil/agent.go`). Interim tightening
  pending [event-driven readiness](#event-driven-agent-readiness).
- **The two backends have different floors and the same code can be
  faster on one and slower on the other.** The vfkit virtio-blk write
  path is ~20× slower than Firecracker's (an in-guest `mkfs` measured
  ~4.2 s on VZ vs ~0.18 s on FC) — which is *why* the CoW-mirror upper
  template is VZ-only and why "lean into platform differences" beats
  forcing the host-side runtimes to match.
- **FC `agent_p50` inflates under concurrency** (e.g. ~2100 → ~2900 ms)
  because concurrent running sheds raise per-shed CID/IP/TAP cost on FC,
  while VZ's shared NAT masks it. Measure with sheds deleted between
  samples. The split timing gate (`tests/integration/fixtures/server.py`
  `DEFAULT_AGENT_P50_MS`) is the regression floor.

### PhaseTimer boot instrumentation

`internal/backend/phasetimer.go` emits per-phase boot timings (installed
in `internal/api/handlers.go`). It is the measurement substrate for every
boot-path change; FC live timing tests depend on these log lines. Shipped
v0.5.4 (PR #118).

### network-setup NIC re-resolution

`vz/network-setup.sh` and `firecracker/network-setup.sh` **re-resolve the
NIC on each pass** rather than caching it once — a transient absence of
the interface early in boot otherwise wedged the static-IP setup. Shipped
v0.5.5 (PR #123).

### Integration test suite design

The pytest + subprocess (+ Fabric for remote orchestration) suite under
`tests/integration/`, managed with `uv`, exists to catch live
boot-path / SSE / timing regressions that unit tests can't. The full
architecture and the rationale for that stack live in
[`../development/testing.md`](../development/testing.md). Shipped v0.5.7.

### The v0.5.4 build-tools-ref regression — validate the shipping path

A whole release's worth of work (the publish-time-erofs fast path) shipped
**inert**: `internal/vz` string-concatenated `"…shed-build-tools:" +
Version`, and release binaries embed `0.5.4` (no `v`) while the published
tags are `v0.5.4` — so the ref never resolved and the host silently fell
back to slow in-guest `mkfs`. Two durable lessons:

1. **One canonical resolver, no duplicated version logic**
   (`internal/version/buildtools.go` is now the single source).
2. **Validate the *shipping* path, not an overridden one.** The dev
   testing that "passed" used a `SHED_BUILD_TOOLS_REF` override *and* a
   warm template cache — both of which bypassed the broken code path. This
   is the reason CLAUDE.md's "Performance impact" section insists on
   per-backend measurement against the released binary.

---

## See also

- [`lazy-rootfs-streaming.md`](lazy-rootfs-streaming.md) — the chosen
  future direction for shrinking pull/disk (strategy C), and where the
  dropped storage ideas' rationale lives.
- [`../development/testing.md`](../development/testing.md) — the dev-server
  workflow and per-backend performance vetting these invariants feed.
- [`../reference/storage-model.md`](../reference/storage-model.md) — the
  current on-disk model.
