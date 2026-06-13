"""Shed server fixtures: `LocalServer` + `RemoteServer`.

Both classes drive a running `shed-server` via the `shed` CLI:

    shed -s <name> create / exec / delete / list

`shed -s <name>` resolves through `~/.shed/config.yaml` and selects either
a local server (HTTP to `localhost:<port>`) or a remote one (HTTP+SSH to
the named host). Most operations are identical between local and remote;
the only meaningful divergence is *where the server log lives*:

  - brew-installed Mac shed-server: a file at
    `/opt/homebrew/var/log/shed-server.log`. `LocalServer(log_path=...)`.
  - deb-installed Linux shed-server: journald via
    `journalctl -u shed-server`. `RemoteServer(remote_log_path=None)`.
  - parallel-dev shed-server (Mac or Linux, launched via the Makefile's
    `dev-server-up`/`-fc` targets): a known file at
    `~/.shed/dev/server.log` (Mac) or `/tmp/shed-server-dev.log`
    (remote). `LocalServer(log_path=...)` or
    `RemoteServer(remote_log_path=...)`.

That divergence is encapsulated in `_log_offset` + `_read_log_since`.

Fabric is intentionally NOT used here (see §16): for the MVP, plain
`subprocess` + `ssh` covers everything we need. Fabric earns its place
later for deploy-dev-binary-then-capture-logs flows; until then it's
unnecessary surface.
"""

from __future__ import annotations

import json
import shlex
import shutil
import subprocess
import sys
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


# Default per-backend ceilings for the AGENT-phase timing assertion.
# Calibrated to tolerate normal variance on a moderately-loaded host
# while still flagging a ~300 ms+ regression on a healthy run.
#
# SCOPE: AGENT phase only (vsock dial + healthPoll + first health
# response). This ceiling is structurally identical between dev and
# release builds — `make build` and the brew/deb binary share the same
# agent path. Use `test_create_agent_p50` to gate on regressions here.
#
# OUT OF SCOPE: the ROOTFS phase. The upper-allocation path diverges
# sharply between dev and release builds:
#   - Release builds: pre-formatted template clone, sub-100 ms.
#   - Dev builds without SHED_BUILD_TOOLS_REF: in-guest mkfs.ext4, ~4 s.
# That divergence is intentional — see
# `internal/version/buildtools.go:BuildToolsRefForTag` returning `""`
# for non-release version strings. Rootfs is gated by
# `test_create_rootfs_template_present`, which skips cleanly on dev
# builds rather than firing as a false-positive against THIS ceiling.
# The split is what makes it safe to run the integration suite against
# dev binaries — see the module-level comment in
# `tests/integration/test_smoke.py` for the full rationale.
#
# Keys are the full backend names as reported by the server's PhaseTimer
# line (`backend=vz` / `backend=firecracker`), NOT the short pytest-param
# labels ("vz" / "fc"). Keeping them aligned with the server's own
# identifier avoids a translation layer at the call site.
#
# Calibration data:
#   - VZ (this mac, post-§15 1a healthPoll cut): median ~1551 ms,
#     range ~1450-1700. Ceiling 2200 leaves ~500 ms regression budget.
#   - Firecracker (mini3 v0.5.6 .deb, 19+ day uptime, 2026-05-29):
#     median ~2405 ms agent phase, range ~1955-3005 across 5 isolated
#     samples. Higher than #126's apples-to-apples measurement
#     (median 1804 ms on a fresh dev binary in May 2026) — production
#     variance is wider than a single benchmarking session showed.
#     Ceiling 2900 ms accommodates today's p50 + headroom for normal
#     load drift without crying wolf, while still catching a 500ms+
#     regression. Tighten when Phase 1's healthPoll cut ships to FC
#     users (the next release will bring agent_p50 down on FC too).
DEFAULT_AGENT_P50_MS = {
    "vz": 2200,
    "firecracker": 2900,
}


def resolve_server_entry(name: str, timeout: int = 10) -> Optional[dict]:
    """Return the `~/.shed/config.yaml` entry for `name`, or None.

    Runs `shed --json server list` and finds the dict whose `name`
    matches — the single parser both `LocalServer._resolve_ssh_endpoint`
    and the dev-control primitives (`fixtures/devcontrol.py`) share, so
    the `shed --json server list` shape (and its failure modes) is
    decoded in exactly one place.
    """
    try:
        r = subprocess.run(
            ["shed", "--json", "server", "list"],
            capture_output=True, text=True, timeout=timeout,
        )
    except (subprocess.TimeoutExpired, FileNotFoundError, OSError):
        return None
    if r.returncode != 0:
        return None
    try:
        entries = json.loads(r.stdout) or []
    except json.JSONDecodeError:
        return None
    for e in entries:
        if isinstance(e, dict) and e.get("name") == name:
            return e
    return None


@dataclass
class ShedHandle:
    """A reference to a successfully-created shed.

    `timings` is None when the server is too old to emit PhaseTimer lines
    (pre-v0.5.4) or when the log fetch failed. Tests that need timing data
    should check for None and skip with a clear reason.

    `template_fallback` is True if the server logged
    `[<name>] upper template unavailable ...; formatting in guest` during
    this create — i.e., the VZ upper-template fast path was inactive and
    the writable upper was instead created raw, with the in-guest
    initramfs `mkfs.ext4`'ing it on first boot. This is the canonical
    "dev binary or `SHED_BUILD_TOOLS_REF` unset" signal and is what
    `test_create_agent_p50` / `test_create_rootfs_template_present` use
    to skip cleanly instead of false-positive on the resulting ~4 s
    cost. FC has no host-side template path; this flag stays False on
    FC regardless. See `internal/vz/orchestrator.go:249` for the
    emitter and the module-level comment in
    `tests/integration/test_smoke.py` for the split-gate rationale.
    """

    name: str
    timings: Optional[PhaseTimings] = None
    template_fallback: bool = False


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
        add_dirs: Optional[list[str]] = None,
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
        for d in add_dirs or []:
            cmd += ["--add-dir", d]
        if from_snapshot is not None:
            cmd += ["--from-snapshot", from_snapshot]

        offset = self._log_offset()
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
        if r.returncode != 0:
            raise AssertionError(
                f"shed create failed (exit {r.returncode}) on {self.name}: "
                f"stdout={r.stdout!r} stderr={r.stderr!r}"
            )
        timings, template_fallback = self._read_timing(name, offset)
        return ShedHandle(
            name=name, timings=timings, template_fallback=template_fallback,
        )

    def exec(
        self,
        name: str,
        cmd: list[str],
        timeout: int = 60,
    ) -> subprocess.CompletedProcess:
        """Run a command inside the shed via `shed exec`."""
        full = ["shed", "-s", self.name, "exec", name, "--"] + cmd
        return subprocess.run(full, capture_output=True, text=True, timeout=timeout)

    def stop(self, name: str, timeout: int = 60) -> None:
        """Stop a running shed via `shed stop` (fail-loud)."""
        r = subprocess.run(
            ["shed", "-s", self.name, "stop", name],
            capture_output=True, text=True, timeout=timeout,
        )
        if r.returncode != 0:
            raise AssertionError(
                f"shed stop failed (exit {r.returncode}) on {self.name}: "
                f"stdout={r.stdout!r} stderr={r.stderr!r}"
            )

    def start(self, name: str, timeout: int = 120) -> None:
        """Start a stopped shed via `shed start` (fail-loud)."""
        r = subprocess.run(
            ["shed", "-s", self.name, "start", name],
            capture_output=True, text=True, timeout=timeout,
        )
        if r.returncode != 0:
            raise AssertionError(
                f"shed start failed (exit {r.returncode}) on {self.name}: "
                f"stdout={r.stdout!r} stderr={r.stderr!r}"
            )

    def ssh_exec(
        self,
        name: str,
        raw_command: str,
        timeout: int = 60,
    ) -> subprocess.CompletedProcess:
        """Drive the SSH wire directly with a raw command string,
        bypassing the `shed exec` CLI's argv quoter.

        This exercises the server-side `bash -lc` wrap on the same path
        OpenSSH, Zed Remote-SSH, VS Code Remote-SSH, and JetBrains
        Gateway take. The `shed exec` CLI path is covered by
        `exec(...)` above; use *this* helper when you want to assert
        that shell metacharacters in the raw command actually fire on
        the server side.

        Resolves connection params (host, SSH port, known-hosts file)
        via `shed --json server list` so we line up with whatever the
        CLI would have used. Uses `~/.shed/known_hosts` with
        StrictHostKeyChecking=yes when it exists (the file the CLI
        populates at create-time); falls back to
        StrictHostKeyChecking=no for fresh test environments. The
        integration suite only ever talks to known-good test sheds, so
        the fallback is safe.
        """
        host, port, known_hosts = self._ssh_connect_params()
        ssh_argv = [
            "ssh",
            "-p", str(port),
            "-o", "BatchMode=yes",
            "-o", "ConnectTimeout=5",
        ]
        if known_hosts is not None:
            ssh_argv += [
                "-o", f"UserKnownHostsFile={known_hosts}",
                "-o", "StrictHostKeyChecking=yes",
            ]
        else:
            ssh_argv += ["-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null"]
        ssh_argv += [f"{name}@{host}", raw_command]
        return subprocess.run(
            ssh_argv,
            capture_output=True,
            text=True,
            timeout=timeout,
        )

    def _ssh_connect_params(self) -> tuple[str, int, Optional[Path]]:
        """Return (host, ssh_port, known_hosts) for raw `ssh` invocations.

        For LocalServer the host is the localhost-mapped ssh_port from
        `~/.shed/config.yaml`. Subclasses (RemoteServer) override to
        return the remote host's address. Known-hosts path is
        ~/.shed/known_hosts (per `config.GetKnownHostsPath` — the same
        file `shed console` uses), or None if it doesn't exist yet.
        """
        # The CLI's known-hosts default; only set if it exists so a
        # fresh test environment skips StrictHostKeyChecking rather than
        # failing with "no known-hosts file."
        kh = Path.home() / ".shed" / "known_hosts"
        if not kh.exists():
            kh = None
        host, port = self._resolve_ssh_endpoint()
        return host, port, kh

    def _resolve_ssh_endpoint(self) -> tuple[str, int]:
        """LocalServer: assume the server hosts the shed on localhost.

        Reads the SSH port from the server's config via the `shed`
        CLI's view of the server entry; falls back to 2222 (the brew
        default).
        """
        entry = resolve_server_entry(self.name, timeout=5)
        if entry is not None:
            try:
                return entry.get("host", "localhost"), int(entry.get("ssh_port", 2222))
            except (TypeError, ValueError):
                pass
        return "localhost", 2222

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

    def list_images(self) -> list[dict]:
        """Return the server's images (the `GET /api/images` wire shape).

        `shed --json image ls` emits `config.ImagesResponse`
        (`{"images": [...]}`, see `cmd/shed/image.go:runImageList`); we
        return that array of image dicts. Raises on a failed call so a
        misconfigured server surfaces loudly (unlike `list_shed_names`,
        callers here assert on field presence, not just membership).
        """
        r = subprocess.run(
            ["shed", "--json", "-s", self.name, "image", "ls"],
            capture_output=True,
            text=True,
            timeout=15,
        )
        if r.returncode != 0:
            raise AssertionError(
                f"shed image ls failed (exit {r.returncode}) on {self.name}: "
                f"stdout={r.stdout!r} stderr={r.stderr!r}"
            )
        resp = json.loads(r.stdout)
        return resp.get("images") or []

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
    ) -> tuple[Optional[PhaseTimings], bool]:
        """Find the PhaseTimer line for `shed_name`, plus whether the
        VZ upper-template fast path fell back to in-guest mkfs.

        Returns `(timings, template_fallback)`. `template_fallback` is
        True if the server emitted the canonical
        `[<shed_name>] upper template unavailable ...; formatting in
        guest` line (see `internal/vz/orchestrator.go:249`) during this
        create. Both signals come from the same log blob so we only
        pay the journalctl / file-read cost once.

        Polls briefly because the log line may not have flushed before
        `shed create` returns. Bounded by `TIMING_LOOKUP_BUDGET_S` of
        wall-clock time, not by an iteration count — that keeps a
        single slow `_read_log_since` call from blowing past the
        budget.
        """
        marker = f"name={shed_name} "
        # Per `internal/vz/orchestrator.go:249` — emitted from the
        # host-side VZ orchestrator path only. FC has no template
        # fast path and never emits this line.
        fallback_marker = f"[{shed_name}] upper template unavailable"
        deadline = time.monotonic() + TIMING_LOOKUP_BUDGET_S
        # `fallback` is sticky: once we've seen the marker in any blob,
        # keep it set even if a subsequent re-read window misses it.
        fallback = False
        while time.monotonic() < deadline:
            blob = self._read_log_since(offset)
            if fallback_marker in blob:
                fallback = True
            for line in blob.splitlines():
                if "timing: create" in line and marker in line:
                    return parse_timing_line(line), fallback
            time.sleep(TIMING_LOOKUP_INTERVAL_S)
        return None, fallback


class RemoteServer(LocalServer):
    """A shed-server reachable on a remote host (e.g. mini3 over SSH).

    Most operations are identical to LocalServer because the `shed` CLI
    already encapsulates the remote transport. The only divergence is
    log fetching, which goes over SSH:

      - If `remote_log_path` is set (the parallel-dev case), reads the
        remote file directly via `ssh <host> sudo -n cat <path>`.
        The dev shed-server is launched with stdout/stderr redirected to
        a known file owned by root, so this is the cheapest read.
      - Otherwise (the deb-installed case), falls through to
        `ssh <host> sudo -n journalctl -u shed-server --since <when>`
        because the deb's systemd unit captures stdout into journald.

    `remote_log_path` is a str (not Path) because it names a file on the
    REMOTE host; `pathlib.Path` semantics (existence checks, parent
    resolution) would apply to the local filesystem and mislead.
    """

    def __init__(
        self,
        ssh_host: str,
        name: str,
        backend: str,
        remote_log_path: Optional[str] = None,
    ) -> None:
        super().__init__(name=name, backend=backend, log_path=None)
        self.ssh_host = ssh_host
        self.remote_log_path = remote_log_path

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

    def _log_offset(self) -> Union[int, str]:
        """Return an offset opaque to the caller but understood by
        `_read_log_since`. For remote-file mode, that's the current
        byte length of the remote file; for the journald fallback,
        it's an ISO-ish timestamp (the parent class's default).
        """
        if self.remote_log_path is not None:
            try:
                r = subprocess.run(
                    [
                        "ssh",
                        "-o", "BatchMode=yes",
                        "-o", "ConnectTimeout=5",
                        self.ssh_host,
                        # `stat -c %s FILE` returns the byte size and
                        # is opened by `stat` (under sudo), so root-only
                        # log files are readable here. We can't use
                        # `wc -c < FILE` because the shell redirect runs
                        # BEFORE sudo and fails to open a root-only
                        # file — leaving us with offset=0 and the suite
                        # reading the entire log every time (stale
                        # PhaseTimer lines from prior tests can then
                        # match a re-used shed name).
                        f"sudo -n stat -c %s {shlex.quote(self.remote_log_path)}",
                    ],
                    capture_output=True,
                    text=True,
                    timeout=10,
                )
                if r.returncode == 0:
                    return int(r.stdout.strip() or "0")
            except (subprocess.TimeoutExpired, FileNotFoundError, ValueError):
                pass
            return 0
        return super()._log_offset()

    def _read_log_since(self, offset: Union[int, str]) -> str:
        # 10 s upper bound — remote operations are slower than local
        # (SSH handshake + remote process spawn), but a stuck ssh
        # should never block the test loop indefinitely. The wall-clock
        # budget in `_read_timing` is the ultimate safety net.
        if self.remote_log_path is not None:
            # File-backed mode: read everything past the byte offset
            # captured at the start of the operation. `tail -c +N` reads
            # from byte N (1-indexed), so +1 means from the start;
            # +(offset+1) means "past the bytes that existed before".
            try:
                start = int(offset)
            except (TypeError, ValueError):
                start = 0
            try:
                r = subprocess.run(
                    [
                        "ssh",
                        "-o", "BatchMode=yes",
                        "-o", "ConnectTimeout=5",
                        self.ssh_host,
                        # No `2>/dev/null || true` — let the SSH
                        # command's stderr surface naturally so a
                        # misconfigured sudo (NOPASSWD missing) or
                        # missing log file reports the real error
                        # rather than silently returning "" (which
                        # the suite would interpret as "no PhaseTimer
                        # found" and skip the test with a misleading
                        # reason).
                        f"sudo -n tail -c +{start + 1} {shlex.quote(self.remote_log_path)}",
                    ],
                    capture_output=True,
                    text=True,
                    timeout=10,
                )
            except (subprocess.TimeoutExpired, FileNotFoundError):
                return ""
            if r.returncode != 0:
                # Surface the underlying error to pytest's stderr so a
                # misconfigured dev server (wrong log path, sudo NOPASSWD
                # not set) is diagnosable from the test output instead
                # of looking like a flaky timing-test skip.
                print(
                    f"[fixtures.RemoteServer] remote log read failed "
                    f"(exit {r.returncode}) for {self.ssh_host}:"
                    f"{self.remote_log_path}: {r.stderr.strip()!r}",
                    file=sys.stderr,
                )
                return ""
            return r.stdout

        # Journald fallback (deb-installed shed-server).
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
