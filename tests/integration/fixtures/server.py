"""Shed server fixtures: `LocalServer` + `RemoteServer`.

Both classes drive a running `shed-server` via the `shed` CLI:

    shed -s <name> create / exec / delete / list

`shed -s <name>` resolves through `~/.shed/config.yaml` and selects either
a local server (HTTP to `localhost:8080`) or a remote one (HTTP+SSH to the
named host). Most operations are identical between local and remote; the
only meaningful divergence is *where the server log lives* — a file path
on a brew-installed mac vs `journalctl -u shed-server` on a systemd Linux
host. That divergence is encapsulated in `_read_log_since`.

Fabric is intentionally NOT used here (see §16): for the MVP, plain
`subprocess` + `ssh` covers everything we need. Fabric earns its place
later for deploy-dev-binary-then-capture-logs flows; until then it's
unnecessary surface.
"""

from __future__ import annotations

import json
import shutil
import subprocess
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Optional, Union

from .timing import PhaseTimings, parse_timing_line


# Budget for finding the PhaseTimer log line for a create. Per-iteration
# subprocess timeouts (LocalServer journalctl: 5 s, RemoteServer ssh:
# 10 s) plus a 200 ms sleep between probes mean a stuck log-reader can
# eat most of this budget on one iteration — that's intentional. We
# bound the WALL CLOCK, not the iteration count, so a single slow
# journalctl call still terminates the probe.
TIMING_LOOKUP_BUDGET_S = 15.0
TIMING_LOOKUP_INTERVAL_S = 0.2


# Default per-backend ceilings for timing assertions. Generous enough to
# tolerate noise on a moderately-loaded host; tight enough to catch a
# >200 ms regression on a healthy run. Anchored on #126's measured
# medians (VZ ~1.5 s, FC ~1.8 s post-firstboot-reorder).
#
# Keys are the full backend names as reported by the server's PhaseTimer
# line (`backend=vz` / `backend=firecracker`), NOT the short pytest-param
# labels ("vz" / "fc"). Keeping them aligned with the server's own
# identifier avoids a translation layer at the call site.
DEFAULT_AGENT_P50_MS = {
    "vz": 2200,
    "firecracker": 2100,
}


@dataclass
class ShedHandle:
    """A reference to a successfully-created shed.

    `timings` is None when the server is too old to emit PhaseTimer lines
    (pre-v0.5.4) or when the log fetch failed. Tests that need timing data
    should check for None and skip with a clear reason.
    """

    name: str
    timings: Optional[PhaseTimings] = None


class LocalServer:
    """Drives a shed-server reachable via the local `shed` CLI.

    `name` is the entry in `~/.shed/config.yaml` (e.g. `my-server` for the
    brew-installed mac server, or `mini3` for a remote shed-server). The
    `backend` field is the string the server reports ("vz" or "firecracker"
    today; matches the PhaseTimer `backend=` token).

    `log_path`, when set, points at a file the server writes to (homebrew
    default: /opt/homebrew/var/log/shed-server.log). When None the class
    falls back to journald.
    """

    def __init__(
        self,
        name: str,
        backend: str,
        log_path: Optional[Path] = None,
    ) -> None:
        self.name = name
        self.backend = backend
        self.log_path = log_path

    # ------------------------------------------------------------------
    # Availability
    # ------------------------------------------------------------------

    def available(self) -> bool:
        """Whether this server can actually be reached from this host.

        Returns False (not raises) so the calling fixture can skip cleanly.

        Probes via `shed -s NAME list`. This has a minor side effect
        (the CLI may rewrite `~/.shed/config.yaml`'s `sheds:` map on
        completion); a side-effect-free probe via `/api/info` is a
        follow-up, tracked in §16 as a "first big test" improvement.
        """
        if not shutil.which("shed"):
            return False
        try:
            r = subprocess.run(
                ["shed", "-s", self.name, "list"],
                capture_output=True,
                text=True,
                timeout=10,
            )
        except (subprocess.TimeoutExpired, FileNotFoundError, OSError):
            return False
        return r.returncode == 0

    # ------------------------------------------------------------------
    # Shed lifecycle
    # ------------------------------------------------------------------

    def create(
        self,
        name: str,
        image: Optional[str] = "base",
        repo: Optional[str] = None,
        local_dir: Optional[str] = None,
        from_snapshot: Optional[str] = None,
        timeout: int = 180,
    ) -> ShedHandle:
        """Create a shed and return a handle.

        Captures the server log offset before the create, then attempts to
        find the PhaseTimer line for this shed afterward. Caller MUST
        delete the shed (the `test_shed_name` fixture in conftest.py does
        this automatically).

        `image` and `from_snapshot` are mutually exclusive at the server
        (see `internal/api/handlers.go`); pass `image=None` together with
        `from_snapshot=...` to skip the `--image` flag.
        """
        cmd = ["shed", "-s", self.name, "create", name]
        if image is not None:
            cmd += ["--image", image]
        if repo is not None:
            cmd += ["--repo", repo]
        if local_dir is not None:
            cmd += ["--local-dir", local_dir]
        if from_snapshot is not None:
            cmd += ["--from-snapshot", from_snapshot]

        offset = self._log_offset()
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
        if r.returncode != 0:
            raise AssertionError(
                f"shed create failed (exit {r.returncode}) on {self.name}: "
                f"stdout={r.stdout!r} stderr={r.stderr!r}"
            )
        timings = self._read_timing(name, offset)
        return ShedHandle(name=name, timings=timings)

    def exec(
        self,
        name: str,
        cmd: list[str],
        timeout: int = 60,
    ) -> subprocess.CompletedProcess:
        """Run a command inside the shed via `shed exec`."""
        full = ["shed", "-s", self.name, "exec", name, "--"] + cmd
        return subprocess.run(full, capture_output=True, text=True, timeout=timeout)

    def delete(self, name: str, ignore_missing: bool = False) -> None:
        """Delete a shed.

        Default is FAIL-LOUD (`ignore_missing=False`): if the delete
        fails (including "shed not found"), the test sees an
        AssertionError. Pass `ignore_missing=True` only from cleanup
        teardown, where you want best-effort delete that doesn't
        confuse a real-bug signal.
        """
        try:
            r = subprocess.run(
                ["shed", "-s", self.name, "delete", name, "-f"],
                capture_output=True,
                text=True,
                timeout=60,
            )
        except subprocess.TimeoutExpired:
            if ignore_missing:
                return
            raise
        if r.returncode != 0 and not ignore_missing:
            raise AssertionError(
                f"shed delete failed: exit={r.returncode} stderr={r.stderr}"
            )

    def list_shed_names(self) -> list[str]:
        """Return the list of shed names known to the server.

        `shed --json list` emits a bare JSON array of `shedJSON`
        objects (see `cmd/shed/shed.go:runList`); we extract the `name`
        field of each. Returns `[]` on any parsing or connectivity
        failure since the primary use is positive assertions in tests
        ("did my create show up?"); a quiet `[]` makes the assertion
        fail with a clear message rather than crash the test runner.
        """
        try:
            r = subprocess.run(
                ["shed", "--json", "-s", self.name, "list"],
                capture_output=True,
                text=True,
                timeout=10,
            )
        except (subprocess.TimeoutExpired, FileNotFoundError):
            return []
        if r.returncode != 0:
            return []
        try:
            sheds = json.loads(r.stdout)
        except json.JSONDecodeError:
            return []
        if not isinstance(sheds, list):
            return []
        return [s["name"] for s in sheds if isinstance(s, dict) and "name" in s]

    # ------------------------------------------------------------------
    # Log handling (overridden in RemoteServer)
    # ------------------------------------------------------------------

    def _log_offset(self) -> Union[int, str]:
        """Return an opaque offset that `_read_log_since` understands.

        For file-based logs: byte offset (int). For journald-based logs:
        an ISO-ish timestamp (str). Either way, treat as opaque.
        """
        if self.log_path is not None and self.log_path.exists():
            return self.log_path.stat().st_size
        return time.strftime("%Y-%m-%d %H:%M:%S")

    def _read_log_since(self, offset: Union[int, str]) -> str:
        """Read all log content emitted since `offset`."""
        if self.log_path is not None and self.log_path.exists():
            with self.log_path.open("rb") as f:
                f.seek(int(offset))
                return f.read().decode("utf-8", errors="replace")
        # journald path: requires `sudo -n` to be non-interactive
        # (passwordless). 5 s upper bound — local journalctl over a
        # small time window is fast; if it doesn't return in 5 s,
        # something else is wrong and we shouldn't block the test.
        try:
            r = subprocess.run(
                [
                    "sudo", "-n",
                    "journalctl", "-u", "shed-server",
                    "--since", str(offset),
                    "--no-pager",
                ],
                capture_output=True,
                text=True,
                timeout=5,
            )
        except (subprocess.TimeoutExpired, FileNotFoundError):
            return ""
        return r.stdout if r.returncode == 0 else ""

    def _read_timing(
        self,
        shed_name: str,
        offset: Union[int, str],
    ) -> Optional[PhaseTimings]:
        """Find the PhaseTimer line for `shed_name`. Polls briefly because
        the log line may not have flushed before `shed create` returns.

        Bounded by `TIMING_LOOKUP_BUDGET_S` of wall-clock time, not by
        an iteration count — that keeps a single slow `_read_log_since`
        call from blowing past the budget.
        """
        marker = f"name={shed_name} "
        deadline = time.monotonic() + TIMING_LOOKUP_BUDGET_S
        while time.monotonic() < deadline:
            blob = self._read_log_since(offset)
            for line in blob.splitlines():
                if "timing: create" in line and marker in line:
                    return parse_timing_line(line)
            time.sleep(TIMING_LOOKUP_INTERVAL_S)
        return None


class RemoteServer(LocalServer):
    """A shed-server reachable on a remote host (e.g. mini3 over SSH).

    Most operations are identical to LocalServer because the `shed` CLI
    already encapsulates the remote transport. The only divergence is
    journald log fetching, which needs an `ssh <host> sudo -n journalctl
    …` round trip.
    """

    def __init__(self, ssh_host: str, name: str, backend: str) -> None:
        super().__init__(name=name, backend=backend, log_path=None)
        self.ssh_host = ssh_host

    def available(self) -> bool:
        """Verify both the `shed -s NAME` CLI path AND raw SSH access.

        The CLI may go through SSH transparently for some operations,
        but `_read_log_since` shells out to `ssh <host> sudo -n
        journalctl` directly — so a timing-threshold test could fail
        for the wrong reason ("test failed: timing not found") when
        the actual cause is "ssh to mini3 doesn't work non-interactively".
        Catching that here turns it into a clean skip.
        """
        if not super().available():
            return False
        try:
            r = subprocess.run(
                [
                    "ssh",
                    "-o", "BatchMode=yes",
                    "-o", "ConnectTimeout=5",
                    self.ssh_host,
                    "true",
                ],
                capture_output=True,
                text=True,
                timeout=10,
            )
        except (subprocess.TimeoutExpired, FileNotFoundError):
            return False
        return r.returncode == 0

    def _read_log_since(self, offset: Union[int, str]) -> str:
        # 10 s upper bound — remote journalctl is slower than local
        # (SSH handshake + remote process spawn), but a stuck ssh
        # should never block the test loop indefinitely. The wall-clock
        # budget in `_read_timing` is the ultimate safety net.
        try:
            r = subprocess.run(
                [
                    "ssh",
                    "-o", "BatchMode=yes",
                    "-o", "ConnectTimeout=5",
                    self.ssh_host,
                    f"sudo -n journalctl -u shed-server --since '{offset}' --no-pager",
                ],
                capture_output=True,
                text=True,
                timeout=10,
            )
        except (subprocess.TimeoutExpired, FileNotFoundError):
            return ""
        return r.stdout if r.returncode == 0 else ""
