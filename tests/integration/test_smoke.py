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
# 4. Plain-create timing threshold (the regression gate)
# ---------------------------------------------------------------------------


def test_plain_create_timing(shed_server):
    """The `agent` phase p50 (5 samples) stays below the per-backend ceiling.

    Catches general boot-path regressions across both backends — the
    dynamic gate that PR-time GHA CI can't be (no /dev/kvm). Names
    include a per-process hash so a concurrent run against the same
    server doesn't collide on `itest-perf-{backend}-0`. The suite isn't
    *designed* for concurrent runs, but failing-closed beats
    failing-weird.
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
    for i in range(6):  # 1 warm-up + 5 measured
        name = f"itest-perf-{shed_server.backend}-{run_id}-{i}"
        handle = shed_server.create(name, image="base")
        try:
            if handle.timings is None or handle.timings.agent_ms is None:
                pytest.skip(
                    "PhaseTimer not available; see "
                    "`test_phase_timer_emitted` for the underlying reason."
                )
            if i > 0:  # skip the warm-up sample
                samples.append(handle.timings.agent_ms)
        finally:
            shed_server.delete(name, ignore_missing=True)
        # Small gap before the next iteration so any async resource
        # release (vsock CID, TAP device) settles before re-use.
        time.sleep(1)

    p50 = int(statistics.median(samples))
    assert p50 < ceiling, (
        f"agent p50 regressed on {shed_server.backend}: "
        f"{p50}ms >= {ceiling}ms ceiling; samples={samples}"
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
