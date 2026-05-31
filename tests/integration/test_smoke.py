"""MVP smoke tests for the shed integration suite (§16).

Five tests, each parameterized over the `["vz", "fc"]` backends through
the `shed_server` fixture in conftest.py. Each test skips cleanly when
its target backend is unreachable from this host.

Anti-flake notes:
    - Tests that need PhaseTimer data check for `handle.timings is None`
      and skip with a clear reason (the server may pre-date PhaseTimer
      v0.5.4).
    - The timing-threshold test takes 5 samples with a 1-second gap to
      avoid CID-allocation conflicts on rapid back-to-back creates.
    - Shed names come from `test_shed_name`, which sanitizes the pytest
      node id; parameterized variants get distinct names.

Threshold sources are in `fixtures/server.py:DEFAULT_AGENT_P50_MS`; tighten
those over time as §15 1a / 1b / 2c land.
"""

from __future__ import annotations

import hashlib
import os
import statistics
import time

import pytest

from fixtures.server import DEFAULT_AGENT_P50_MS


# Pinned to the long-stable octocat/Hello-World head. This repo hasn't
# been touched in roughly a decade; if it ever moves, the test fails
# loudly and we update the constant. Better than the previous
# "stdout is non-empty" check, which would pass under partial failure.
OCTOCAT_HELLO_WORLD_HEAD_SHA = "7fd1a60b01f91b314f59955a4e4d4e80d8edf11d"


# ---------------------------------------------------------------------------
# 1. Happy-path lifecycle
# ---------------------------------------------------------------------------


def test_create_delete_lifecycle(shed_server, test_shed_name):
    """A shed can be created, observed in `shed list`, deleted explicitly,
    and then absent from `shed list`."""
    handle = shed_server.create(test_shed_name, image="base")
    assert handle.name == test_shed_name
    listed_before = shed_server.list_shed_names()
    assert test_shed_name in listed_before, (
        f"shed {test_shed_name!r} not visible in `shed list` after create; got {listed_before}"
    )
    # Explicit delete with FAIL-LOUD semantics — proves delete works,
    # not just that teardown silenced any failure.
    shed_server.delete(test_shed_name, ignore_missing=False)
    listed_after = shed_server.list_shed_names()
    assert test_shed_name not in listed_after, (
        f"shed {test_shed_name!r} still present after explicit delete; got {listed_after}"
    )


# ---------------------------------------------------------------------------
# 2. PhaseTimer emission
# ---------------------------------------------------------------------------


def test_phase_timer_emitted(shed_server, test_shed_name):
    """The server logs a PhaseTimer line for the create with expected phase keys."""
    handle = shed_server.create(test_shed_name, image="base")
    if handle.timings is None:
        pytest.skip(
            "no PhaseTimer line found in the server log within the polling "
            "window; the server is likely older than v0.5.4 (which added "
            "PhaseTimer via PR #118), or log access is restricted. Update "
            "the server, or check passwordless-sudo journalctl access for "
            "remote servers."
        )
    t = handle.timings
    assert t.backend == shed_server.backend, (
        f"timing line backend={t.backend!r} does not match server "
        f"backend={shed_server.backend!r}"
    )
    assert t.name == test_shed_name
    assert t.total_ms > 0, "total= must be a positive integer"
    # `setup` is always emitted (the gap between PhaseTimer start and
    # the first Progress event becomes `setup`, per phasetimer.go); `vm`
    # and `agent` are the two phases tests will most often assert on.
    # `total` is parsed separately and exposed as `total_ms`, not as
    # a phase key — don't require it inside `phases`.
    for required in ("setup", "vm", "agent"):
        assert required in t, (
            f"phase {required!r} missing from timing line; got phases={t.phases}"
        )


# ---------------------------------------------------------------------------
# 3. Repo clone
# ---------------------------------------------------------------------------


def test_repo_clone_https(shed_server, test_shed_name):
    """`--repo <https url>` clones; the cloned HEAD matches the known SHA."""
    handle = shed_server.create(
        test_shed_name,
        image="base",
        repo="https://github.com/octocat/Hello-World.git",
    )
    if handle.timings is not None:
        assert handle.timings.error is None, (
            f"server reported err={handle.timings.error!r}"
        )

    r = shed_server.exec(
        test_shed_name,
        ["git", "-C", "/workspace", "rev-parse", "HEAD"],
    )
    assert r.returncode == 0, (
        f"`git rev-parse HEAD` in guest failed: exit={r.returncode} stderr={r.stderr!r}"
    )
    head = r.stdout.strip()
    assert head == OCTOCAT_HELLO_WORLD_HEAD_SHA, (
        f"cloned HEAD {head!r} does not match the pinned "
        f"{OCTOCAT_HELLO_WORLD_HEAD_SHA!r}; the upstream repo may have moved "
        f"or the clone wrote to the wrong directory"
    )


# ---------------------------------------------------------------------------
# 4. Plain-create timing threshold (the regression gate — split)
# ---------------------------------------------------------------------------
#
# Split into two assertions because the per-create boot path has two
# structurally independent signals that used to share one gate:
#
#   - `agent` phase ceiling (`test_create_agent_p50`): vsock dial +
#     healthPoll + first health response. A genuine regression in
#     healthPoll, vsock setup, or agent init fires this.
#   - VZ upper-template fast-path active
#     (`test_create_rootfs_template_present`): a release-only
#     invariant — the VZ host-side AllocateUpper clones a
#     pre-formatted ext4 template instead of writing a raw upper that
#     the in-guest initramfs has to `mkfs.ext4` on first boot.
#
# Empirically the in-guest mkfs cost (~4 s on VZ) lands inside the
# `agent` phase, NOT the `rootfs` phase — `rootfs_ms` stays sub-100 ms
# in both modes because that phase only covers host-side allocation,
# which is fast either way. The original `test_plain_create_timing`
# couldn't distinguish "dev binary, in-guest mkfs is on the agent
# critical path" from "genuine agent-phase regression."
#
# Discriminator: the server log emits
# `[<shed-name>] upper template unavailable (...); formatting in guest`
# from `internal/vz/orchestrator.go:249` whenever the host-side fast
# path is unavailable (dev build, missing `SHED_BUILD_TOOLS_REF`, or a
# failed template clone). `ShedHandle.template_fallback` exposes that
# signal per-create. Both tests use it: `test_create_agent_p50` skips
# when at least one sample saw the fallback, and
# `test_create_rootfs_template_present` skips on dev mode and skips
# on FC entirely (FC has no host-side template fast path —
# see `internal/firecracker/orchestrator.go:AllocateUpper`).
#
# Dev-build isolation rationale: `BuildToolsRefForTag` in
# `internal/version/buildtools.go` returns "" for any non-release
# version string (including `-dirty` / `-N-g<hash>` suffixes), which
# means dev binaries DON'T resolve a build-tools image and fall back
# to in-guest mkfs.ext4 by design — a dev binary can't assume a
# published build-tools image matches its source state. The
# template_fallback signal lets the suite stay safe when run against
# either dev or release binaries on either backend.

# Sanity ceiling for the host-side rootfs phase when the fast path is
# active. Template clone + sibling-swap is ~5-10 ms on a healthy host;
# 100 ms is generous headroom that would still catch a 10x regression
# (e.g., a reflink that silently falls back to a full copy).
ROOTFS_TEMPLATE_FAST_PATH_CEILING_MS = 100


def test_create_agent_p50(shed_server):
    """Boot-path p50 regression gate for the `agent` phase.

    Reads `agent_ms` from PhaseTimer log lines; takes 5 samples
    (post-warm-up) and asserts the median stays below the per-backend
    ceiling in `fixtures/server.py:DEFAULT_AGENT_P50_MS`.

    Skips cleanly when ANY sample saw the VZ
    `template_fallback` signal — on dev binaries the in-guest mkfs.ext4
    falls inside the agent phase and inflates p50 by ~4 s, which would
    fire the gate for a structural reason that's NOT a real regression.
    `test_create_rootfs_template_present` covers the orthogonal "is the
    fast path active" question; this test focuses on the agent-init
    path only. See the module-level comment for the split rationale.
    """
    ceiling = DEFAULT_AGENT_P50_MS[shed_server.backend]
    run_id = hashlib.sha256(
        f"{os.getpid()}-{time.time_ns()}".encode()
    ).hexdigest()[:6]

    # Delete-between-samples so each measurement reflects the cost
    # of a SINGLE shed coming up — not the accumulating cost of
    # N previous sheds running concurrently. The first live FC e2e
    # run (mini3 v0.5.6) caught the accumulation effect when samples
    # rose monotonically (1956→2854 ms) under the old "create five,
    # delete five at end" pattern; the per-create signal is the
    # regression target we actually want to gate on.
    #
    # We also drop the FIRST measured sample because the very first
    # create after a fresh shed-server install touches a cold blob
    # store (image pull + erofs conversion) that's irrelevant to
    # boot-time tracking.
    samples: list[int] = []
    fallback_seen = False
    for i in range(6):  # 1 warm-up + 5 measured
        name = f"itest-perf-{shed_server.backend}-{run_id}-{i}"
        handle = shed_server.create(name, image="base")
        try:
            if handle.timings is None or handle.timings.agent_ms is None:
                pytest.skip(
                    "PhaseTimer not available; see "
                    "`test_phase_timer_emitted` for the underlying reason."
                )
            if handle.template_fallback:
                fallback_seen = True
            if i > 0:  # skip the warm-up sample
                samples.append(handle.timings.agent_ms)
        finally:
            shed_server.delete(name, ignore_missing=True)
        # Small gap before the next iteration so any async resource
        # release (vsock CID, TAP device) settles before re-use.
        time.sleep(1)

    if fallback_seen:
        pytest.skip(
            f"at least one sample used the VZ in-guest mkfs.ext4 "
            f"fallback (log marker '[<name>] upper template unavailable' "
            f"present). agent_p50 is inflated by ~4 s on VZ dev builds "
            f"because the in-guest mkfs lands inside the agent phase; "
            f"the gate would fire for a structural reason, not a real "
            f"regression. Set SHED_BUILD_TOOLS_REF on the shed-server "
            f"process (e.g. via `launchctl setenv SHED_BUILD_TOOLS_REF "
            f"ghcr.io/charliek/shed-build-tools:vX.Y.Z` + "
            f"`brew services restart shed`) or run a release binary "
            f"to exercise the fast path. samples_collected={samples}"
        )

    p50 = int(statistics.median(samples))
    assert p50 < ceiling, (
        f"agent p50 regressed on {shed_server.backend}: "
        f"{p50}ms >= {ceiling}ms ceiling; samples={samples}"
    )


def test_create_rootfs_template_present(shed_server, test_shed_name):
    """The VZ upper-template fast-path (pre-formatted template clone) is active.

    Release builds embed a `SHED_BUILD_TOOLS_REF` and the VZ
    orchestrator's `AllocateUpper` clones a pre-formatted ext4 template
    on the host — no in-guest mkfs needed. Dev builds (or release
    builds with the env var unset) fall back to writing a raw upper
    and letting the in-guest initramfs `mkfs.ext4` on first boot,
    which costs ~4 s on the agent critical path. The server log line
    `[<name>] upper template unavailable (...); formatting in guest`
    (`internal/vz/orchestrator.go:249`) is the canonical "fast path
    NOT active" signal, surfaced as `ShedHandle.template_fallback`.

    This test:
      - Skips on FC: `internal/firecracker/orchestrator.go:AllocateUpper`
        has no template path; FC always uses in-guest mkfs and its
        agent ceiling already accommodates that cost.
      - Skips on VZ dev mode: `template_fallback` is True → log says
        the fast path was unavailable.
      - On VZ release mode: asserts the host-side host phase
        (`rootfs_ms`) is sub-100 ms as a sanity check that the
        template clone actually happened fast.

    See the module-level comment for the split rationale.
    """
    if shed_server.backend != "vz":
        pytest.skip(
            f"backend={shed_server.backend!r} has no host-side "
            f"upper-template fast path; only VZ uses it (see "
            f"`internal/firecracker/orchestrator.go:AllocateUpper`)."
        )
    handle = shed_server.create(test_shed_name, image="base")
    if handle.timings is None or handle.timings.rootfs_ms is None:
        pytest.skip(
            "PhaseTimer / rootfs phase not available; see "
            "`test_phase_timer_emitted` for the underlying reason."
        )
    if handle.template_fallback:
        pytest.skip(
            "VZ upper-template fast path not active — log emitted "
            "'upper template unavailable ...; formatting in guest' "
            "(see `internal/vz/orchestrator.go:249`). Dev binaries "
            "intentionally don't embed a shed-build-tools image ref "
            "(see `internal/version/buildtools.go:BuildToolsRefForTag`); "
            "set SHED_BUILD_TOOLS_REF on the shed-server process or "
            "run a release binary to exercise the fast path."
        )
    rootfs_ms = handle.timings.rootfs_ms
    assert rootfs_ms <= ROOTFS_TEMPLATE_FAST_PATH_CEILING_MS, (
        f"VZ rootfs fast-path regressed: rootfs={rootfs_ms}ms > "
        f"{ROOTFS_TEMPLATE_FAST_PATH_CEILING_MS}ms ceiling. The host-side "
        f"template clone + sibling-swap should be sub-100ms on a healthy "
        f"reflink-capable FS; a regression here suggests a silent "
        f"clone-to-full-copy fallback or a slow stat path. Bisect "
        f"against `internal/vz/uppertemplate.go` and "
        f"`internal/vz/orchestrator.go:AllocateUpper`."
    )


# ---------------------------------------------------------------------------
# 5. `shed exec` smoke
# ---------------------------------------------------------------------------


def test_shed_exec_smoke(shed_server, test_shed_name):
    """`shed exec <name> -- echo hello` succeeds and returns `hello`."""
    shed_server.create(test_shed_name, image="base")
    r = shed_server.exec(test_shed_name, ["echo", "hello"])
    assert r.returncode == 0, f"shed exec echo failed: stderr={r.stderr!r}"
    assert "hello" in r.stdout, f"unexpected stdout: {r.stdout!r}"


# ---------------------------------------------------------------------------
# 6. Extensions image smoke (the shed-extensions layer is wired up)
# ---------------------------------------------------------------------------


# Binaries shipped by the upstream ghcr.io/charliek/shed-extensions image
# and copied into the `extensions` (and transitively `full`) rootfs layer
# by the Dockerfile's COPY-via-bind RUN. If a bump regresses any of them
# (missing binary, wrong path, lost +x bit, arch mismatch), this test
# catches it before the bump ships.
#
# Keep this list aligned with the install statements in
# vz/Dockerfile + firecracker/Dockerfile's `shed-vz-extensions` /
# `shed-fc-extensions` stage.
_SHED_EXTENSIONS_BINARIES = (
    "/usr/local/bin/shed-ext-ssh-agent",
    "/usr/local/bin/shed-ext-aws-credentials",
    "/usr/local/bin/docker-credential-shed",
)


def test_extensions_image_smoke(shed_server, test_shed_name):
    """The `extensions` image variant carries the shed-extensions binaries.

    Smoke test for the `extensions` (and transitively `full`) image
    variants — verifies that the Dockerfile's
    `COPY --from=ghcr.io/charliek/shed-extensions:vX.Y.Z` resolved
    cleanly at image-build time, every documented binary is present in
    the booted rootfs at the documented path, and each binary has the
    executable bit. This is the gate future shed-extensions bumps need
    to survive — added with the v0.3.1 → v0.3.2 bump so the existing
    `image="base"` tests don't carry the burden.

    Skips with a clear message if the server has no `extensions` tag
    configured (e.g. a dev box pulling only `base`); the test isn't a
    regression for that environment, it just doesn't apply.
    """
    try:
        shed_server.create(test_shed_name, image="extensions")
    except AssertionError as e:
        msg = str(e)
        if "extensions" in msg and (
            "no image tag" in msg or "not found" in msg or "unknown image" in msg
        ):
            pytest.skip(
                "server has no `extensions` image tag configured; this "
                "smoke gate only applies where the extensions variant is "
                "installed. Configure one with `shed image tag …` or pull "
                "ghcr.io/charliek/shed-{vz,fc}-extensions:vX.Y.Z."
            )
        raise

    for binary in _SHED_EXTENSIONS_BINARIES:
        r = shed_server.exec(test_shed_name, ["test", "-x", binary])
        assert r.returncode == 0, (
            f"{binary!r} missing or not executable in the booted shed: "
            f"exit={r.returncode} stdout={r.stdout!r} stderr={r.stderr!r}. "
            f"The Dockerfile's `COPY --from=shed-extensions` likely failed "
            f"to install this binary (check the COPY RUN in "
            f"vz/Dockerfile / firecracker/Dockerfile's extensions stage)."
        )
