"""Hermetic sshd + the shared machine-transport contract.

Everything here runs against a throwaway OpenSSH server on 127.0.0.1: a fresh
host key, a fresh client key, a fresh `authorized_keys`, a fresh `known_hosts`,
all under a per-session temp dir. Nothing touches the developer's `~/.ssh`, no
agent is consulted, and no remote host is contacted.

Why a REAL sshd rather than a fake: the thing under test is what a remote shell
does to a composed command line, and only a real server + a real login shell
answers that. A mock would happily agree with whatever the goldens already say.
"""

from __future__ import annotations

import base64
import json
import os
import shutil
import socket
import subprocess
import time
from pathlib import Path

import pytest

HERE = Path(__file__).parent
CONTRACT = HERE / "scenarios.json"
GOLDENS = HERE / "goldens"

# The receiver: prints each received argument on its own line, NUL-free and
# unambiguous. `printf '%s\0'` would be tidier but we want the transcript
# readable in a failure message, so newlines are escaped by the caller instead.
RECEIVER = "receiver.sh"


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _wait_for_port(port: int, proc: subprocess.Popen, timeout: float = 10.0) -> None:
    """Poll the port AND the child, so an sshd that died is reported at once
    rather than after the full timeout."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            # sshd reports config errors on BOTH streams depending on the
            # failure; read each without blocking on the other, or a config
            # typo surfaces as an empty message (which cost a debugging cycle).
            out, err = proc.communicate(timeout=5)
            raise RuntimeError(
                f"sshd exited early (rc={proc.returncode})\n"
                f"stdout:\n{out}\nstderr:\n{err}"
            )
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.25):
                return
        except OSError:
            time.sleep(0.05)
    raise RuntimeError(f"sshd did not come up on 127.0.0.1:{port}")


@pytest.fixture(scope="session")
def contract() -> dict:
    return json.loads(CONTRACT.read_text())


@pytest.fixture(scope="session")
def scenarios(contract: dict) -> list[dict]:
    return contract["scenarios"]


@pytest.fixture(scope="session")
def sshd(tmp_path_factory) -> dict:
    """A throwaway sshd on 127.0.0.1, plus the client-side bits to reach it.

    Returns a dict with `port`, `key` (client private key path), `known_hosts`,
    and `user` (the current user — sshd runs as us, so the login is a no-op).
    """
    # All three OpenSSH executables are used: `sshd` serves, `ssh-keygen` mints
    # the host/client keys, `ssh` is the client under test. Skipping on only the
    # first would fail confusingly mid-fixture on a host that has a server but no
    # client tools.
    sshd_bin = shutil.which("sshd") or "/usr/sbin/sshd"
    missing = [name for name in ("ssh", "ssh-keygen") if shutil.which(name) is None]
    if not os.path.exists(sshd_bin) or missing:
        pytest.skip(
            "the live transport leg needs sshd, ssh and ssh-keygen "
            f"(missing: {', '.join(missing) or 'sshd'})"
        )

    root = tmp_path_factory.mktemp("sshd")
    # OpenSSH refuses keys/dirs that are group- or world-readable.
    root.chmod(0o700)

    host_key = root / "host_ed25519"
    client_key = root / "client_ed25519"
    for path in (host_key, client_key):
        subprocess.run(
            ["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(path)],
            check=True,
        )
        path.chmod(0o600)

    authorized = root / "authorized_keys"
    authorized.write_text((client_key.with_suffix(".pub")).read_text())
    authorized.chmod(0o600)

    known_hosts = root / "known_hosts"
    port = _free_port()
    hostpub = (host_key.with_suffix(".pub")).read_text().split()
    known_hosts.write_text(f"[127.0.0.1]:{port} {hostpub[0]} {hostpub[1]}\n")

    user = subprocess.run(["id", "-un"], capture_output=True, text=True, check=True).stdout.strip()

    config = root / "sshd_config"
    config.write_text(
        "\n".join(
            [
                f"Port {port}",
                "ListenAddress 127.0.0.1",
                f"HostKey {host_key}",
                "PidFile none",
                "StrictModes no",
                "UsePAM no",
                "PasswordAuthentication no",
                "KbdInteractiveAuthentication no",
                "ChallengeResponseAuthentication no",
                "PubkeyAuthentication yes",
                f"AuthorizedKeysFile {authorized}",
                # Loopback-only, throwaway host: keep the surface minimal.
                "PermitRootLogin no",
                # The forwarded-hub family needs -L; everything else stays off.
                "AllowTcpForwarding yes",
                "X11Forwarding no",
                "PrintMotd no",
                "LogLevel ERROR",
            ]
        )
        + "\n"
    )
    config.chmod(0o600)

    # `-D` foreground, `-e` log to stderr. Absolute path is required by sshd.
    proc = subprocess.Popen(
        [sshd_bin, "-D", "-e", "-f", str(config)],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    try:
        _wait_for_port(port, proc)
    except Exception:
        proc.kill()
        proc.wait(timeout=5)
        raise

    info = {
        "port": port,
        "key": str(client_key),
        "known_hosts": str(known_hosts),
        "user": user,
        "root": root,
    }
    yield info

    proc.terminate()
    try:
        # `communicate` (not `wait`) so the stdout/stderr pipes are drained AND
        # closed. A bare `wait` leaves both FileIO objects open, which the
        # suite's warnings-are-errors setting correctly reports as a leak at
        # interpreter shutdown.
        proc.communicate(timeout=5)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.communicate(timeout=5)


@pytest.fixture(scope="session")
def receiver(sshd: dict) -> str:
    """A remote script that reports the argv it was handed — ONE base64 line per
    element.

    This is the whole measurement: whatever the composed wire line means to the
    remote shell, this is the argv the process actually got.

    base64 rather than escaping, because the payloads deliberately include
    newlines, tabs, backslashes and non-ASCII, and every escaping scheme that
    has to survive `sh` + `sed` is a second thing that can be wrong. An empty
    argument encodes to an empty line, which round-trips correctly — and "the
    empty argument survives" is itself one of the scenarios.
    """
    path = Path(sshd["root"]) / RECEIVER
    path.write_text(
        "#!/bin/sh\n"
        'for a in "$@"; do\n'
        "  printf '%s' \"$a\" | base64 | tr -d '\\n'\n"
        "  printf '\\n'\n"
        "done\n"
    )
    path.chmod(0o755)
    return str(path)


def decode_received(stdout: str) -> list[str]:
    """Decode the receiver's base64 lines back into the argv it saw."""
    return [
        base64.b64decode(line).decode("utf-8", "surrogateescape")
        for line in stdout.splitlines()
    ]


def isolation_argv(sshd: dict) -> list[str]:
    """The options that make an ssh invocation actually hermetic.

    **`IdentitiesOnly=yes` alone is NOT enough**, and this repo already learned
    it the hard way — see `tests/integration/test_bootstrap.py`'s
    `_bootstrap_with_key`. Without `-F /dev/null` OpenSSH still reads the user's
    `~/.ssh/config` and the system one, so a `Host *` stanza can inject a
    `ProxyJump`/`ProxyCommand` (the run leaves loopback), a `ControlMaster`
    (multiplex onto a foreign socket), extra `IdentityFile`s (offered in
    addition to `-i`, exhausting `MaxAuthTries`), or a `RemoteCommand` — which
    would silently change the very thing this suite measures. `IdentityAgent=none`
    keeps a running agent out of it, and pinning `GlobalKnownHostsFile` stops a
    system `127.0.0.1` entry from fail-closing `StrictHostKeyChecking=yes`.
    """
    return [
        "-F",
        "/dev/null",
        "-o",
        "IdentityAgent=none",
        "-o",
        "BatchMode=yes",
        "-o",
        "StrictHostKeyChecking=yes",
        "-o",
        f"UserKnownHostsFile={sshd['known_hosts']}",
        "-o",
        "GlobalKnownHostsFile=/dev/null",
        "-o",
        "IdentitiesOnly=yes",
        "-i",
        sshd["key"],
        "-o",
        "ConnectTimeout=10",
        "-p",
        str(sshd["port"]),
    ]


def ssh_argv(sshd: dict, wire_line: str) -> list[str]:
    """The client-side argv, mirroring `shed_core::machine::ssh_argv`'s posture:
    BatchMode, a pinned host key, an explicit port, and the remote command as
    ONE argument after `--` — plus the isolation options above."""
    return (
        ["ssh"]
        + isolation_argv(sshd)
        + [
            f"{sshd['user']}@127.0.0.1",
            "--",
            wire_line,
        ]
    )


def run_wire(sshd: dict, wire_line: str) -> subprocess.CompletedProcess:
    return subprocess.run(
        ssh_argv(sshd, wire_line),
        capture_output=True,
        text=True,
        timeout=60,
    )


def load_golden(name: str) -> dict:
    path = GOLDENS / name
    if not path.exists():
        # A MISSING golden is a failure, not an auto-record (the rc-parity
        # rule): for an existing cell it means the file was deleted, and
        # re-recording would bless whatever the code does today.
        if os.environ.get("UPDATE_GOLDEN") != "1":
            raise AssertionError(
                f"missing golden {path} — record deliberately with UPDATE_GOLDEN=1"
            )
        return {}
    return json.loads(path.read_text())


def save_golden(name: str, value: dict) -> None:
    GOLDENS.mkdir(exist_ok=True)
    path = GOLDENS / name
    # Recording is idempotent by content, so an unchanged golden leaves a clean
    # `git status`.
    text = json.dumps(value, indent=2, ensure_ascii=False, sort_keys=True) + "\n"
    if not path.exists() or path.read_text() != text:
        path.write_text(text)


def updating() -> bool:
    return os.environ.get("UPDATE_GOLDEN") == "1"
