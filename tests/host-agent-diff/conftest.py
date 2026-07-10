"""Fixtures for the hermetic Go-vs-Rust host-agent differential harness (slice 0).

The session `binaries` fixture builds BOTH daemon binaries (`go build` + `cargo
build`, with `~/.cargo/bin` on PATH) once and returns their paths. `run_cli` drives
a one-shot subcommand (`version`, `status`, ...) against a chosen impl in an isolated
HOME + socket dir. `daemon` is a context manager that launches a daemon with a valid
config, waits for its status socket, yields a handle that can query `status
[--json]`, and on exit sends SIGTERM, asserts a clean exit 0, and asserts both sockets
are unlinked.

Hermetic by construction: every test gets a fresh `$SHED_HOST_AGENT_SOCKET_DIR` and
`$HOME` (so no real `~/.ssh` / `~/.shed` is read), `SSH_AUTH_SOCK` is stripped (the Go
daemon falls back to an empty local-keys backend), and there is no network.
"""

from __future__ import annotations

import contextlib
import dataclasses
import os
import shutil
import signal
import subprocess
import tempfile
import time
from pathlib import Path

import pytest

# tests/host-agent-diff/conftest.py -> tests -> repo root.
REPO_ROOT = Path(__file__).resolve().parents[2]

# Committed, throwaway, passphrase-less OpenSSH ed25519 test key (see
# fixtures/test_ed25519). The `daemon` fixture writes it to a daemon's isolated
# `<HOME>/.ssh/id_ed25519` so BOTH the Go local-keys backend and the Rust
# `LocalEd25519Backend` load the SAME key — ed25519 is deterministic (RFC 8032), so
# the two sign the same challenge to the same 64 bytes (compared UNMASKED).
FIXTURES_DIR = Path(__file__).resolve().parent / "fixtures"
TEST_ED25519_KEY = FIXTURES_DIR / "test_ed25519"

# The status/desktop socket basenames (fixed public interface — sockets.go /
# sockets.rs). The dir is the only configurable part (`$SHED_HOST_AGENT_SOCKET_DIR`).
STATUS_SOCK_NAME = "host-agent-status.sock"
DESKTOP_SOCK_NAME = "host-agent.sock"

# The "watch none" launch config: ssh delegated to shed-desktop, aws approve-all,
# docker deny-all, logging on, discovery watching zero servers. Chosen so BOTH impls
# report an EMPTY `servers` list (Go's supervisor watches nothing; Rust has no
# supervisor yet) while still exercising a non-trivial policy/gate mix. Written in
# BLOCK style on purpose: the Rust slice-0 `yaml_lite` parser handles block maps only,
# not inline flow maps like `ssh: { approval: { policy: X } }` — see README "Known
# contract gaps". `{audit_log}` / `{source}` are filled in with per-test temp paths.
WATCH_NONE_CONFIG = """\
ssh:
  approval:
    policy: shed-desktop
aws:
  approval:
    policy: approve-all
docker:
  approval:
    policy: deny-all
logging:
  enabled: true
  path: {audit_log}
discovery:
  servers: []
  watch: off
  source: {source}
"""

# The single-server (NO `discovery:` block) launch config for the surface-B bus
# tests. With no `discovery:` key BOTH impls run in single-server mode (Go
# `cfg.Discovery == nil`; Rust `is_single_server()`), connect to `server:`, and
# subscribe to `ssh-agent`. `server:` is filled by `single_server_config(bus_url)`
# (via a plain string replace, leaving `{audit_log}` for the `daemon` fixture's
# `.format()` — an unused `{source}` kwarg there is harmless). BLOCK style on
# purpose (the Rust `yaml_lite` parser is block-only; see WATCH_NONE_CONFIG /
# README "Known contract gaps"). `ssh.approval.policy: approve-all` is a valid ssh
# policy on both sides — but ping is NOT gated, so it's irrelevant to the pong; it
# just exercises a non-empty gate. No `aws:`/`docker:` blocks → those backends stay
# nil → the daemon subscribes to ssh-agent only (plus, on Go, the always-on egress
# GET, which the synthetic bus 501s).
SINGLE_SERVER_CONFIG = """\
server: {server}
ssh:
  approval:
    policy: approve-all
logging:
  enabled: true
  path: {audit_log}
"""


def _single_server_config(bus_url: str) -> str:
    """Fill `server:` with `bus_url`, leaving `{audit_log}` for the `daemon`
    fixture. Uses `str.replace` (not `.format`) so the still-unfilled `{audit_log}`
    placeholder survives to the fixture's own `.format(audit_log=..., source=...)`."""
    return SINGLE_SERVER_CONFIG.replace("{server}", bus_url)


# The single-server config for the gated cross-surface **sign** differential
# (`test_bus_sign_gated.py`). Same single-server shape as SINGLE_SERVER_CONFIG (no
# `discovery:` block → both impls run single-server, subscribe to `ssh-agent`), but
# with `ssh.mode: local-keys` (so the committed ed25519 key the `daemon` fixture
# installs is loaded — on BOTH impls) and `ssh.approval.policy: shed-desktop` (so a
# `sign` is DELEGATED to the connected desktop consumer; Go builds a `desktopGate`,
# Rust a `DesktopGate`, and both fan the resulting audit out to that consumer as an
# `event`). BLOCK style on purpose (the Rust `yaml_lite` parser is block-only; see
# WATCH_NONE_CONFIG / README "Known contract gaps"). `{server}` is filled by
# `sign_config(bus_url)`; `{audit_log}` survives to the `daemon` fixture's `.format`.
SIGN_CONFIG = """\
server: {server}
ssh:
  mode: local-keys
  approval:
    policy: shed-desktop
logging:
  enabled: true
  path: {audit_log}
"""


def _sign_config(bus_url: str) -> str:
    """Fill `server:` with `bus_url`, leaving `{audit_log}` for the `daemon` fixture
    (str.replace, same reason as `_single_server_config`)."""
    return SIGN_CONFIG.replace("{server}", bus_url)


@dataclasses.dataclass
class CliResult:
    """The outcome of one one-shot subcommand invocation."""

    returncode: int
    stdout: str
    stderr: str
    socket_dir: str
    home: str


def _clean_env(socket_dir, home) -> dict:
    """A hermetic environment: real PATH (so `go`/system tools resolve) but an
    isolated HOME + socket dir, and no ambient ssh-agent."""
    env = dict(os.environ)
    env["SHED_HOST_AGENT_SOCKET_DIR"] = str(socket_dir)
    env["HOME"] = str(home)
    env.pop("SSH_AUTH_SOCK", None)
    env.pop("XDG_RUNTIME_DIR", None)
    return env


def _run_cli(binary: str, args, socket_dir, home) -> CliResult:
    proc = subprocess.run(
        [binary, *args],
        env=_clean_env(socket_dir, home),
        capture_output=True,
        timeout=30,
    )
    return CliResult(
        returncode=proc.returncode,
        stdout=proc.stdout.decode("utf-8"),
        stderr=proc.stderr.decode("utf-8"),
        socket_dir=str(socket_dir),
        home=str(home),
    )


def _build(cmd, cwd, env=None) -> None:
    proc = subprocess.run(cmd, cwd=cwd, env=env, capture_output=True, text=True)
    if proc.returncode != 0:
        raise AssertionError(
            f"build failed: {' '.join(cmd)} (cwd={cwd})\n"
            f"--- stdout ---\n{proc.stdout}\n--- stderr ---\n{proc.stderr}"
        )


@pytest.fixture(scope="session")
def binaries(tmp_path_factory) -> dict:
    """Build both daemon binaries once and return `{"go": path, "rust": path}`."""
    go_bin = tmp_path_factory.mktemp("go-bin") / "shed-host-agent-go"
    _build(["go", "build", "-o", str(go_bin), "./cmd/shed-host-agent"], cwd=REPO_ROOT)

    cargo_env = dict(os.environ)
    cargo_env["PATH"] = (
        str(Path.home() / ".cargo" / "bin") + os.pathsep + cargo_env.get("PATH", "")
    )
    _build(
        ["cargo", "build", "-p", "shed-host-agent"],
        cwd=REPO_ROOT / "crates",
        env=cargo_env,
    )
    rust_bin = REPO_ROOT / "crates" / "target" / "debug" / "shed-host-agent"

    assert go_bin.exists(), f"go binary missing: {go_bin}"
    assert rust_bin.exists(), f"rust binary missing: {rust_bin}"
    return {"go": str(go_bin), "rust": str(rust_bin)}


@pytest.fixture
def run_cli(binaries, tmp_path_factory):
    """Return `run(impl, *args, socket_dir=None, home=None) -> CliResult`. Each call
    gets a fresh isolated socket dir + HOME unless one is passed (to let a scenario
    point both impls at the same dir)."""

    def _call(impl, *args, socket_dir=None, home=None) -> CliResult:
        if socket_dir is None:
            socket_dir = tmp_path_factory.mktemp("sock")
        if home is None:
            home = tmp_path_factory.mktemp("home")
        return _run_cli(binaries[impl], list(args), socket_dir, home)

    return _call


class DaemonHandle:
    """A running daemon under test: query it, inspect its socket dir."""

    def __init__(
        self, impl, binary, proc, socket_dir, home, config_path, log_path, audit_log_path
    ):
        self.impl = impl
        self.binary = binary
        self.proc = proc
        self.socket_dir = str(socket_dir)
        self.home = str(home)
        self.config_path = str(config_path)
        self.log_path = Path(log_path)
        # The DURABLE audit JSONL (`logging.path`), distinct from the operational log
        # above. Populated by gated ops (the sign flow); a fan-out `event` is sent
        # AFTER the file line is written on both impls (Rust `JsonlAuditSink::log`,
        # Go `AuditLogger.LogEntry`), so a durable line is readable once the desktop
        # consumer has seen the matching `event`.
        self.audit_log_path = Path(audit_log_path)
        self.status_sock = Path(socket_dir) / STATUS_SOCK_NAME
        self.desktop_sock = Path(socket_dir) / DESKTOP_SOCK_NAME

    def status(self, json: bool = False) -> CliResult:
        args = ["status"] + (["--json"] if json else [])
        return _run_cli(self.binary, args, self.socket_dir, self.home)

    def read_log(self) -> str:
        return _safe_read(self.log_path)

    def read_audit_jsonl(self, expect: int = 1, timeout: float = 5.0) -> list:
        """Poll the durable audit file until it holds `expect` non-empty JSONL lines
        (a deadline poll, never a fixed sleep) and return them parsed. Raises on
        timeout or on a line that isn't a JSON object. Robust against the small window
        between the desktop `event` and the file flush landing on disk."""
        deadline = time.monotonic() + timeout
        last: list = []
        while time.monotonic() < deadline:
            try:
                raw = self.audit_log_path.read_text()
            except OSError:
                raw = ""
            last = [line for line in raw.splitlines() if line.strip()]
            if len(last) >= expect:
                import json as _json

                return [_json.loads(line) for line in last]
            time.sleep(0.02)
        raise AssertionError(
            f"{self.impl}: audit file {self.audit_log_path} held {len(last)} line(s), "
            f"expected {expect} within {timeout}s; contents={last!r}"
        )


@pytest.fixture
def daemon(binaries, tmp_path_factory):
    """Return a context-manager factory `make(impl, config_text) -> DaemonHandle`.

    On enter: writes `config_text` (a `.format(audit_log=..., source=...)` template)
    into an isolated dir, launches the daemon with `-config`/`-log-file`, and waits
    up to 3s for the status socket to appear. On exit: SIGTERM, assert exit 0, and
    assert BOTH the status and desktop sockets are unlinked.
    """

    @contextlib.contextmanager
    def _make(impl, config_text, install_ssh_key: bool = False):
        root = tmp_path_factory.mktemp(f"daemon-{impl}")
        home = root / "home"
        home.mkdir()
        audit_log = root / "audit.log"
        source = root / "nonexistent-discovery.yaml"
        config_path = root / "extensions.yaml"
        log_path = root / "op.log"
        config_path.write_text(config_text.format(audit_log=audit_log, source=source))

        # Install the committed ed25519 key into this daemon's isolated
        # `<HOME>/.ssh/id_ed25519` (dir 0700, file 0600) BEFORE launch, so a
        # local-keys `sign` finds the SAME key on both impls (Go `os.UserHomeDir()`
        # + Rust `user_home_dir()` both resolve `$HOME`, set by `_clean_env`).
        if install_ssh_key:
            ssh_dir = home / ".ssh"
            ssh_dir.mkdir(mode=0o700, exist_ok=True)
            key_dst = ssh_dir / "id_ed25519"
            key_dst.write_bytes(TEST_ED25519_KEY.read_bytes())
            os.chmod(ssh_dir, 0o700)
            os.chmod(key_dst, 0o600)

        # The socket dir must be SHORT: an AF_UNIX bind path caps at ~104 bytes
        # (macOS) / ~108 (Linux), and pytest's nested tmp tree blows past that. A
        # shallow mkdtemp under $TMPDIR keeps `<dir>/host-agent-status.sock` well
        # under the limit. (One-shot subcommands don't bind, so `run_cli` can use the
        # long pytest tmp dir.) Cleaned up in the finally below.
        socket_dir = Path(tempfile.mkdtemp(prefix="hadiff-"))

        status_sock = socket_dir / STATUS_SOCK_NAME
        desktop_sock = socket_dir / DESKTOP_SOCK_NAME

        # The daemon writes its operational log to -log-file, so stdout/stderr carry
        # nothing worth capturing; DEVNULL avoids leaving un-read pipe file objects
        # (which would trip the suite's warnings-as-errors as ResourceWarnings).
        proc = subprocess.Popen(
            [binaries[impl], "-config", str(config_path), "-log-file", str(log_path)],
            env=_clean_env(socket_dir, home),
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )

        # Readiness: the status socket file exists once the daemon has bound it.
        deadline = time.monotonic() + 3.0
        while time.monotonic() < deadline:
            if status_sock.exists():
                break
            if proc.poll() is not None:
                raise AssertionError(
                    f"{impl} daemon exited early rc={proc.returncode}\n"
                    f"oplog={_safe_read(log_path)}"
                )
            time.sleep(0.02)
        else:
            proc.kill()
            proc.wait()
            raise AssertionError(
                f"{impl} daemon did not bind {status_sock} within 3s\n"
                f"oplog={_safe_read(log_path)}"
            )

        handle = DaemonHandle(
            impl, binaries[impl], proc, socket_dir, home, config_path, log_path, audit_log
        )
        try:
            yield handle
        finally:
            _shutdown(impl, proc, status_sock, desktop_sock)
            shutil.rmtree(socket_dir, ignore_errors=True)

    return _make


def _shutdown(impl, proc, status_sock, desktop_sock) -> None:
    """SIGTERM the daemon, assert a clean exit 0, and assert both sockets unlinked.

    The daemon MUST still be running at teardown: an early exit (a crash after
    serving a request, say) would otherwise false-pass this "clean SIGTERM
    shutdown" check, since `proc.wait()` would just return the prior code."""
    assert proc.poll() is None, (
        f"{impl} daemon already exited (rc={proc.returncode}) before SIGTERM — "
        "a daemon must stay up until teardown; an early exit is a bug"
    )
    proc.send_signal(signal.SIGTERM)
    try:
        rc = proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait()
        raise AssertionError(f"{impl} daemon did not exit within 5s of SIGTERM")
    assert rc == 0, f"{impl} daemon exit={rc} on SIGTERM (want 0)"
    assert not status_sock.exists(), (
        f"{impl}: status socket not unlinked on shutdown: {status_sock}"
    )
    assert not desktop_sock.exists(), (
        f"{impl}: desktop socket not unlinked on shutdown: {desktop_sock}"
    )


def _safe_read(path) -> str:
    try:
        return Path(path).read_text()
    except OSError:
        return "(no operational log)"


@pytest.fixture
def watch_none_config() -> str:
    """The block-style 'watch none' launch-config template (see WATCH_NONE_CONFIG)."""
    return WATCH_NONE_CONFIG


@pytest.fixture
def single_server_config():
    """Return `make(bus_url) -> config_text`: the block-style single-server (no
    `discovery:`) launch config with `server:` pointed at the synthetic bus. The
    returned text still carries the `{audit_log}` placeholder the `daemon` fixture
    fills. See SINGLE_SERVER_CONFIG."""
    return _single_server_config


@pytest.fixture
def sign_config():
    """Return `make(bus_url) -> config_text`: the block-style single-server config for
    the gated cross-surface **sign** differential — `ssh.mode: local-keys` +
    `ssh.approval.policy: shed-desktop`, `server:` pointed at the synthetic bus. Pair
    with `daemon(..., install_ssh_key=True)` so the committed ed25519 key is loaded.
    See SIGN_CONFIG."""
    return _sign_config


@pytest.fixture
def differential():
    """Return `run(scenario) -> value`, where `scenario(impl) -> normalized value`.
    Runs the scenario for both `go` and `rust`, asserts the normalized values are
    equal (with a readable diff on mismatch), and returns the common value."""

    def _run(scenario):
        go = scenario("go")
        rust = scenario("rust")
        assert go == rust, (
            "differential mismatch (go != rust):\n"
            f"--- go ---\n{go!r}\n--- rust ---\n{rust!r}"
        )
        return go

    return _run
