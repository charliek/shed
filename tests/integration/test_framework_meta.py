"""Meta-tests for the integration framework itself.

These tests cover the common error scenarios a developer running the
suite against a misconfigured dev or prod shed-server will actually
hit. They don't need a live shed-server — they exercise the fixture
code paths in `fixtures/server.py` directly, mocking out `subprocess`
where needed.

Why these matter: PR #157–#160 shipped a swap-based workflow whose
failure modes (server unreachable, log path missing, sudo NOPASSWD
not configured, ssh BatchMode failures) produced confusing skips and
mysterious crashes during the validation cycles. The parallel-dev
workflow that supersedes it adds new failure modes — wrong dev port
in `~/.shed/config.yaml`, dev log written to a path the fixture isn't
configured for, cross-talk between concurrent prod + dev sheds in the
same log file. These meta-tests pin the expected behavior so a
regression in any of those paths fires here, not as a confusing skip
deep in the live-run suite.

See `/Users/charliek/.claude/plans/lets-defer-remote-mac-deep-brook.md`
§1.3 for the motivating list and the rationale for each test.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
from pathlib import Path

import pytest

from fixtures.server import LocalServer, RemoteServer


# ---------------------------------------------------------------------------
# 1. log_path missing
# ---------------------------------------------------------------------------


def test_meta_log_path_missing_returns_empty(monkeypatch):
    """LocalServer with a non-existent log_path falls through to
    journald (which we mock to return empty). Tests don't crash; they
    skip via the calling test's PhaseTimer-not-found check.

    Catches: dev server writes to a path the fixture isn't configured
    for. The test we'd ACTUALLY run (test_create_agent_p50) skips
    cleanly with a clear reason instead of crashing the suite runner.
    """
    # Mock journalctl to return empty (the journald fallback path).
    def fake_run(*args, **kwargs):
        class R:
            returncode = 0
            stdout = ""
        return R()
    monkeypatch.setattr(subprocess, "run", fake_run)

    s = LocalServer(name="any", backend="vz", log_path=Path("/nonexistent"))
    # Offset resolution doesn't raise.
    offset = s._log_offset()
    # Read doesn't raise.
    blob = s._read_log_since(offset)
    assert blob == ""


# ---------------------------------------------------------------------------
# 2. available() when shed CLI is missing
# ---------------------------------------------------------------------------


def test_meta_available_probe_missing_cli(monkeypatch):
    """available() returns False (not raises) when the shed CLI isn't
    on PATH. The fixture's `if not s.available(): pytest.skip(...)`
    chain then produces a clean skip.
    """
    monkeypatch.setattr(shutil, "which", lambda _: None)
    s = LocalServer(name="any", backend="vz")
    assert s.available() is False


# ---------------------------------------------------------------------------
# 3. available() when server is unreachable
# ---------------------------------------------------------------------------


def test_meta_available_probe_unreachable_server(monkeypatch):
    """available() returns False when `shed -s NAME list` times out
    or returns non-zero. Catches: dev server not running → skip, not
    crash.
    """
    monkeypatch.setattr(shutil, "which", lambda _: "/fake/path/shed")

    # Case A: subprocess.run returns non-zero.
    def fake_run_nonzero(*args, **kwargs):
        class R:
            returncode = 1
            stdout = ""
            stderr = "connection refused"
        return R()
    monkeypatch.setattr(subprocess, "run", fake_run_nonzero)
    assert LocalServer(name="any", backend="vz").available() is False

    # Case B: subprocess.run raises TimeoutExpired.
    def fake_run_timeout(*args, **kwargs):
        raise subprocess.TimeoutExpired(cmd=args[0], timeout=kwargs.get("timeout", 0))
    monkeypatch.setattr(subprocess, "run", fake_run_timeout)
    assert LocalServer(name="any", backend="vz").available() is False


# ---------------------------------------------------------------------------
# 4. SSH endpoint resolution honors the configured port (not the 2222 fallback)
# ---------------------------------------------------------------------------


def test_meta_ssh_endpoint_resolves_dev_port(monkeypatch):
    """`_resolve_ssh_endpoint` returns the port from
    `shed --json server list` for the dev entry, not the fallback 2222.

    Catches: suite running raw SSH against the wrong port when both a
    prod and a dev server are configured. The raw-SSH path is what
    the `ssh_exec_*` tests use.
    """
    fake_json = """[
        {"name": "my-server", "host": "localhost", "ssh_port": 2222},
        {"name": "my-server-dev", "host": "localhost", "ssh_port": 12222}
    ]"""

    def fake_run(cmd, **kwargs):
        class R:
            returncode = 0
            stdout = fake_json
            stderr = ""
        return R()
    monkeypatch.setattr(subprocess, "run", fake_run)

    s = LocalServer(name="my-server-dev", backend="vz")
    host, port = s._resolve_ssh_endpoint()
    assert host == "localhost"
    assert port == 12222


# ---------------------------------------------------------------------------
# 5. template_fallback marker matches per-shed-name
# ---------------------------------------------------------------------------


def test_meta_template_fallback_marker_is_per_shed_name(monkeypatch):
    """`_read_timing` only sets template_fallback=True when the
    `[<name>] upper template unavailable` marker contains THIS test's
    shed name. A concurrent create from the OTHER server (which would
    write a marker with a different shed name) doesn't pollute this
    one.

    Catches: when prod and dev shed-servers are both running and
    both happen to create sheds at the same time, the dev server's
    "no template" log shouldn't be misattributed to a prod-server
    create (or vice-versa).
    """
    # Synthetic log blob: marker for "other-shed", PhaseTimer line for
    # "target-shed". The target's marker is NOT present.
    blob = (
        "2026/05/30 12:34:56 [other-shed] upper template unavailable "
        "(no shed-build-tools ref configured); formatting in guest\n"
        "2026/05/30 12:34:57 timing: create name=target-shed backend=vz "
        "total=1500ms setup=1ms image=2ms rootfs=5ms vm=3ms "
        "agent=1489ms err=<nil>\n"
    )
    s = LocalServer(name="any", backend="vz")
    monkeypatch.setattr(s, "_read_log_since", lambda offset: blob)
    monkeypatch.setattr(s, "_log_offset", lambda: 0)

    timings, fallback = s._read_timing("target-shed", offset=0)
    assert timings is not None
    assert timings.name == "target-shed"
    assert fallback is False, (
        "fallback was incorrectly attributed from another shed's marker"
    )

    # Sanity: same blob, but ASK about other-shed → fallback IS set.
    timings_other, fallback_other = s._read_timing("other-shed", offset=0)
    # other-shed has the marker; no PhaseTimer line for it though, so
    # timings_other is None — but the fallback flag is still set
    # because the marker matched.
    assert timings_other is None
    assert fallback_other is True


# ---------------------------------------------------------------------------
# 6. RemoteServer(remote_log_path=...) uses ssh tail -c, not journalctl
# ---------------------------------------------------------------------------


def test_meta_remote_log_path_uses_ssh_cat(monkeypatch):
    """When `RemoteServer` is constructed with a `remote_log_path`,
    `_read_log_since` reads the remote file via `ssh + sudo tail -c +N`,
    NOT journalctl.

    Catches: regression where `remote_log_path` is silently dropped
    and the fixture falls through to journalctl, which would fail to
    find PhaseTimer lines from the parallel-dev shed-server (it's not
    a systemd unit, so journalctl has no records).
    """
    captured: dict = {}

    def fake_run(cmd, **kwargs):
        captured["cmd"] = cmd
        class R:
            returncode = 0
            stdout = "fake log content"
            stderr = ""
        return R()
    monkeypatch.setattr(subprocess, "run", fake_run)

    s = RemoteServer(
        ssh_host="fake-host",
        name="any",
        backend="firecracker",
        remote_log_path="/tmp/shed-server-dev.log",
    )
    result = s._read_log_since(42)

    assert result == "fake log content"
    assert captured["cmd"][0] == "ssh"
    assert "fake-host" in captured["cmd"]
    # The remote command should be a tail-from-offset of the configured
    # log path. Format: `sudo -n tail -c +<offset+1> '/tmp/shed-server-dev.log' ...`
    remote_cmd = captured["cmd"][-1]
    assert "tail" in remote_cmd
    assert "/tmp/shed-server-dev.log" in remote_cmd
    # +(offset+1) because tail -c +N is 1-indexed.
    assert "+43" in remote_cmd
    # journalctl MUST NOT be invoked in this path.
    assert "journalctl" not in remote_cmd


# ---------------------------------------------------------------------------
# 6b. RemoteServer._log_offset uses `stat -c %s` (sudo opens the file)
# ---------------------------------------------------------------------------


def test_meta_remote_log_offset_uses_stat_not_wc_redirect(monkeypatch):
    """`RemoteServer._log_offset` for `remote_log_path` mode uses
    `sudo -n stat -c %s FILE`, NOT `sudo -n wc -c < FILE`. The shell
    redirect `<` would run BEFORE sudo, so a root-owned dev log file
    couldn't be opened and offset would always be 0 — defeating the
    stale-PhaseTimer protection in `_read_timing`.

    Catches: regression where someone "simplifies" the SSH command
    back to `wc -c < ...` and the suite silently re-introduces a
    flake class where re-running the same test name matches a prior
    run's PhaseTimer line.
    """
    captured: dict = {}

    def fake_run(cmd, **kwargs):
        captured["cmd"] = cmd
        class R:
            returncode = 0
            stdout = "12345\n"
            stderr = ""
        return R()
    monkeypatch.setattr(subprocess, "run", fake_run)

    s = RemoteServer(
        ssh_host="fake-host",
        name="any",
        backend="firecracker",
        remote_log_path="/tmp/shed-server-dev.log",
    )
    offset = s._log_offset()
    assert offset == 12345

    remote_cmd = captured["cmd"][-1]
    assert "stat -c" in remote_cmd, (
        f"expected `stat -c %s` (sudo opens the file), got: {remote_cmd!r}"
    )
    # The `wc -c < FILE` antipattern is the regression we're preventing.
    assert "wc -c <" not in remote_cmd, (
        f"`wc -c < FILE` shell-redirects BEFORE sudo runs; for a "
        f"root-only dev log the shell can't open it. Use "
        f"`sudo -n stat -c %s FILE` instead. Got: {remote_cmd!r}"
    )


# ---------------------------------------------------------------------------
# 7. RemoteServer with no remote_log_path uses journalctl
# ---------------------------------------------------------------------------


def test_meta_remote_no_log_path_uses_journalctl(monkeypatch):
    """When `RemoteServer` is constructed without `remote_log_path`
    (the deb-installed shed-server case), `_read_log_since` uses
    journald.

    Catches: regression where the journald branch is broken — would
    break the existing brew/deb-targeted suite's FC tests.
    """
    captured: dict = {}

    def fake_run(cmd, **kwargs):
        captured["cmd"] = cmd
        class R:
            returncode = 0
            stdout = ""
            stderr = ""
        return R()
    monkeypatch.setattr(subprocess, "run", fake_run)

    s = RemoteServer(
        ssh_host="fake-host",
        name="any",
        backend="firecracker",
        # remote_log_path omitted → journald path
    )
    s._read_log_since("2026-05-30 12:00:00")

    remote_cmd = captured["cmd"][-1]
    assert "journalctl" in remote_cmd
    assert "shed-server" in remote_cmd
    assert "tail" not in remote_cmd


# ---------------------------------------------------------------------------
# 8. Full-suite regression backstop — existing brew run still works
# ---------------------------------------------------------------------------


# Sentinel env var to prevent recursion if the meta-test were ever
# (accidentally) run by the subprocess pytest itself.
_RECURSION_GUARD = "SHED_META_TEST_RECURSION_GUARD"


def test_meta_full_suite_still_passes_against_brew():
    """Backstop: a single canonical test from `test_smoke.py` still
    passes against the brew-installed shed-server with default env
    vars. This is the load-bearing regression gate that PR 1's
    fixture changes don't break the existing brew-targeted flow.

    Skips cleanly when:
      - The brew shed-server isn't reachable (`shed -s my-server list`
        non-zero). Common on a non-Mac dev workstation.
      - We're being run recursively via this test's own subprocess
        invocation (the sentinel env var).

    Deliberately slow-but-deterministic over fast-but-flaky. Picks
    one test (test_create_delete_lifecycle[vz]) that's the cheapest
    end-to-end probe of the create/delete path.
    """
    if os.environ.get(_RECURSION_GUARD) == "1":
        pytest.skip("recursion guard")

    # Probe brew reachability before invoking pytest.
    if shutil.which("shed") is None:
        pytest.skip("shed CLI not on PATH")
    probe = subprocess.run(
        ["shed", "-s", "my-server", "list"],
        capture_output=True, text=True, timeout=10,
    )
    if probe.returncode != 0:
        pytest.skip(
            f"brew shed-server (my-server) not reachable; "
            f"this backstop only runs on a configured Mac dev workstation. "
            f"probe stderr: {probe.stderr!r}"
        )

    # Run the cheapest canary against the brew server, in a subprocess
    # so this test's own pytest state doesn't tangle.
    env = os.environ.copy()
    env[_RECURSION_GUARD] = "1"
    integration_dir = Path(__file__).parent
    result = subprocess.run(
        [
            sys.executable, "-m", "pytest",
            "-v", "--tb=short",
            "test_smoke.py::test_create_delete_lifecycle[vz]",
        ],
        cwd=str(integration_dir),
        env=env,
        capture_output=True, text=True,
        # Generous: one VZ create + delete typically ~10-20 s; allow
        # plenty of headroom for cold-state.
        timeout=180,
    )

    assert result.returncode == 0, (
        f"brew-targeted backstop test failed (exit {result.returncode}):\n"
        f"stdout:\n{result.stdout}\n"
        f"stderr:\n{result.stderr}"
    )
