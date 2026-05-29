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

import statistics
import time

import pytest

from fixtures.server import DEFAULT_AGENT_P50_MS


# ---------------------------------------------------------------------------
# 1. Happy-path lifecycle
# ---------------------------------------------------------------------------


def test_create_delete_lifecycle(shed_server, test_shed_name):
    """`shed create` succeeds; the `test_shed_name` teardown then deletes it."""
    handle = shed_server.create(test_shed_name, image="base")
    assert handle.name == test_shed_name


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
    for required in ("vm", "agent"):
        assert required in t, (
            f"phase {required!r} missing from timing line; got phases={t.phases}"
        )


# ---------------------------------------------------------------------------
# 3. Repo clone
# ---------------------------------------------------------------------------


def test_repo_clone_https(shed_server, test_shed_name):
    """`--repo <https url>` clones; `git log` in the guest succeeds."""
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
        ["git", "-C", "/workspace", "log", "--oneline", "-1"],
    )
    assert r.returncode == 0, (
        f"`git log` in guest failed: exit={r.returncode} stderr={r.stderr!r}"
    )
    # The known head commit of octocat/Hello-World is the "Spaceghost" merge.
    # We assert weakly to tolerate occasional future updates to the repo.
    assert r.stdout.strip(), "`git log` produced no output"


# ---------------------------------------------------------------------------
# 4. Plain-create timing threshold (the regression gate)
# ---------------------------------------------------------------------------


def test_plain_create_timing(shed_server):
    """The `agent` phase p50 (5 samples) stays below the per-backend ceiling.

    Catches general boot-path regressions across both backends — not just
    the unit-file ordering tests from PR #127 protect against. This is the
    dynamic gate that PR-time GHA CI can't be (no /dev/kvm).
    """
    ceiling = DEFAULT_AGENT_P50_MS[shed_server.backend]
    samples: list[int] = []
    created: list[str] = []
    try:
        for i in range(5):
            name = f"itest-perf-{shed_server.backend}-{i}"
            handle = shed_server.create(name, image="base")
            created.append(name)
            if handle.timings is None or handle.timings.agent_ms is None:
                pytest.skip(
                    "PhaseTimer not available; see "
                    "`test_phase_timer_emitted` for the underlying reason."
                )
            samples.append(handle.timings.agent_ms)
            time.sleep(1)  # avoid CID-allocation conflicts between rapid creates
    finally:
        for n in created:
            shed_server.delete(n, ignore_missing=True)

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
