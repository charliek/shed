"""A server whose users come from an account service, and the service, on
loopback: the world of `test_directory.py` (`make test-integration-directory`).

The account service is the in-tree reference server of the directory
contract (`bin/test/shed-directory-ref`, `internal/directory/refserver`):
plain HTTP on a loopback port, all of its state in memory, and its users,
enrolment codes and outage read from a control directory this module writes
(`users.json`, `codes`, `outage`). The shed server is `bin/shed-server` on the
remote backend's test drivers (`provider: fake`, `transport: local`), so it
needs no root and no cloud VM: a "shed" there is a real `shed-agent` on this
host, run by the server as the user running the tests, with its sockets under
the server's `TMPDIR` (inside the world's folder). Sessions into such a shed
run on THIS host as THIS user: the cases assert that a session opens, runs,
closes or is refused, never the guest user, its home or its landing
directory, and they run nothing in one but `echo` and `cat`.

One `World` per case (`directory_world()`), all inside one fresh temporary
folder:

  - the reference server, started first (its issuer is the server's
    `auth.directory.url`), with an empty list and one enrolment code;
  - the server's config (`render`ed here: mtls, `allow_plain_http`,
    `refresh: 1m`, `trust: 2m`, the shortest the config allows with a trust
    time longer than one refresh; an explicit `state_dir`; ports the kernel
    picked on 127.0.0.1, never one of `FORBIDDEN_PORTS`), and one local admin,
    `localadmin`, with its own key (the reference server never serves a
    reserved name such as `admin`);
  - `shed-server directory enroll`, the code on its standard input;
  - `shed-server serve`, with its own `HOME` and `TMPDIR` in the folder;
  - the local admin's identity enrolled with `shed server add`.

Identities are `fixtures/identity.py`'s (their own `HOME`, an `ssh` shim
that offers exactly one key), with `bin/` first on their `PATH`, so they never
read or write the developer's `~/.shed`. A held connection (`Held`) is a real
OpenSSH control master; channels through it never fall back to a new
connection (`ProxyCommand` is `false` for them), so a refused channel cannot
pass as a new connection's answer.

At the end of every case, pass or fail: every ssh process the world started
is stopped, every shed deleted (as the local admin), the server and the
reference server stopped, then every process still running whose command
line names a path inside the world's folder (an agent the server started
that outlived it, whether or not the server is still there), with whatever
runs under it, whoever's it is, and the folder removed. Every clean-up path
finds what to stop through one helper (`owned_processes`). A process the
fixture did not start itself is signalled only through a handle that pins it
(its pid and start time, read from /proc before anything else of it is read,
so the readings that decide it is the world's are confirmed to be that
process's by reading its start time again after them; checked again just
before each signal, the signal sent through a pidfd), so a reused pid is
never selected or signalled. What could not be cleaned up (a process that
could not be signalled included) fails the case, naming it. The logs'
last lines are printed at teardown (pytest shows them for a failed case).

An API request (`api_request`) is either answered (an HTTP status and body)
or refused by the server itself: an HTTP 401 or 403 carrying the server's
error code, or the server's `bad_certificate` alert at the TLS handshake
(how an mtls listener refuses a client certificate whose key no longer
resolves to its user, `servertls.ClientConnectionVerifier`). The handshake
is TLS 1.2, so the server judges the certificate before the handshake ends
and its alert cannot race the request. Anything else (no connection, a
reset, a timeout, another TLS error) raises `TransportFailure`: a server
that crashed can never pass as one that refused.
"""

from __future__ import annotations

import base64
import fnmatch
import hashlib
import http.client
import json
import os
import platform
import re
import secrets
import selectors
import shlex
import shutil
import signal
import socket
import ssl
import stat
import subprocess
import sys
import tempfile
import threading
import time
from contextlib import contextmanager
from dataclasses import dataclass, replace
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Callable, Iterator, Mapping, Optional, Sequence

import yaml

from .identity import LOCAL_ADMIN, Identity, shim_text
from .tlsclient import PinMismatch, error_code, fingerprint, server_cert_pem

HOST = "127.0.0.1"
# The CLI entry name every identity of a world uses for its server.
ENTRY = "dir-itest"
# The local admin (LOCAL_ADMIN, `localadmin`) is a separate, local-only
# identity (auth.users beside auth.directory). Not "admin": the reference
# server refuses to serve a list holding a reserved name, so the case of a
# list entry named like the local admin could not be written with that name.
# The server's name on the account service, and the shed server's own name.
SERVER_NAME = "itest-dir"
# The shortest the config allows: refresh at least 1m, trust at least the
# refresh. Trust is two refreshes, so one late pull does not end it.
REFRESH = "1m"
TRUST = "2m"
# internal/directory: an unknown key pulls at most once a minute while pulls
# succeed (UnknownKeyEvery), waits for that pull at most 8 s
# (UnknownKeyWait); the trust watch ticks every 30 s.
UNKNOWN_KEY_EVERY = 60
UNKNOWN_KEY_WAIT = 8
TRUST_TICK = 30
# Ports other work on this machine holds: never picked, even if free now.
FORBIDDEN_PORTS = frozenset({8000, 8100, 8114, 5179, 5533, 1126, 8126, 12222, 18443, 12290, 18490, 12936, 18936})
# A base for the world's folder longer than this would push the agent's and
# the control masters' socket paths past the kernel's limit (as the local
# link itself does, the fixture then uses /tmp).
MAX_SOCKET_BASE = 40

# The server's own words the cases match on (test_fixture_units checks each
# is still in the code).
NEEDS_CSR = "upgrade shed (client certificate support)"  # internal/sshd/bootstrap.go
BOOTSTRAP_NOT_AUTHORIZED = "bootstrap: key not authorized"  # internal/sshd/bootstrap.go
KEY_MOVED = "no longer belongs to the user this connection authenticated as"  # internal/sshd/identity.go
UNKNOWN_KEY_PULL = "directory: unknown key "  # internal/directory/runtime.go
# A shed name's background pull (NudgeForUnknownKey): its own prefix, which
# UNKNOWN_KEY_PULL's count does not include.
SHED_NAME_PULL = "directory: shed-name key "  # internal/directory/runtime.go
TRUST_ENDED = "directory: TRUST ENDED"  # internal/directory/lookup.go
STARTUP_KEPT_COPY = "Users: kept copy from "  # internal/directory/runtime.go StartupLines
STARTUP_NO_COPY = "Users: no kept copy; only the local admins can log in until the first good pull"
PULL_GOOD = "Directory pull: good"  # cmd/shed/users.go
SKIPPED_LOCAL_ADMIN_NAME = "local-admin-name"  # internal/directory/lookup.go SkipLocalAdminName
SKIPPED_LOCAL_ADMIN_KEY = "local-admin-key"  # internal/directory/lookup.go SkipLocalAdminKey
SKIPPED_LOG = 'directory: list entry "{name}" skipped: '  # internal/directory/runtime.go
READY_LINES = ("HTTPS server listening on", "SSH server listening on")  # cmd/shed-server/serve.go
# The world's trust time in seconds (TRUST).
TRUST_SECONDS = 120

# The CLI's own words the entry cases match on (test_fixture_units checks each
# is still in the code). Each is a template: fill it with `.format`.
# A re-enrolment that would make an entry another user (internal/config
# entrycert.go, RefusedIssuedUserChange).
USER_CHANGE_REFUSAL = ('the server issued a certificate for user "{cert}", but entry "{entry}" is held to user '
                       '"{held}"; nothing was saved.')
# A stored certificate for a user the entry does not record, the entry
# recording none (RefusedStoredUserMismatch), and the end of that refusal.
STORED_UNRECORDED_REFUSAL = 'entry "{entry}" cannot be used: its certificate is for user "{cert}" but the entry records no user'
NO_REQUEST_SENT = "no request was sent"
# `shed server add` and `shed server update --refetch` (cmd/shed/server_add.go).
ENTRY_USER_LINE = "  User: {user}\n"
ENTRY_USER_CHANGED_LINE = '  User changed from "{was}" to "{now}"\n'
# `shed users status` as a user who is not an admin: the CLI's wrapping
# (cmd/shed/users.go usersStatusError), its rendering of an API error
# (client.go parseError) and the server's refusal (internal/api requireAdmin,
# handleUsersStatus).
USERS_STATUS_FORBIDDEN = "users status on {entry}: FORBIDDEN: admin only: the status of the server's users"
# The reference server's line for each token request (internal/directory/
# refserver/token.go tokenRequestLine): every line starts with the prefix,
# and a request naming the directory scope, and nothing else, is the second.
TOKEN_REQUEST = "token request: "
TOKEN_REQUEST_DIRECTORY_SCOPE = 'token request: scope="directory:read"'
# GET /api/info's `exec` member (internal/config/types.go ExecLimits) and the
# feature tokens of the exec limits and the uniform shed record
# (internal/api/overview.go).
EXEC_LIMIT_MEMBERS = frozenset({
    "max_stdin_bytes", "max_timeout_seconds", "default_timeout_seconds", "max_output_bytes",
    "default_output_bytes", "max_concurrent_per_shed", "max_argv_elements", "max_env_keys", "max_argv_env_bytes",
})
FEATURE_EXEC_LIMITS = "exec_limits"
FEATURE_UNIFORM_SHED_RECORD = "uniform_shed_record"

# How the server refuses a client certificate at the TLS handshake: crypto/tls
# answers a failed VerifyConnection (servertls.ClientConnectionVerifier) with a
# bad_certificate alert, which OpenSSL names so.
REFUSAL_ALERTS = frozenset({"SSLV3_ALERT_BAD_CERTIFICATE"})

# The world's files that are meant to hold a private key, relative to its
# folder; no other file of the world may hold one. Each is named:
WORLD_PRIVATE_KEY_FILES = (
    "keys/*/id_ed25519",  # the world's throwaway SSH keys (World.key)
    "certs/*/client.key",  # client certificate keys the cases make (enrol_cert, forged_cert)
    "homes/*/.shed/creds/*/client.key",  # each identity's CLI client key (shed server add)
    "state/ca_key.pem",  # the server's client CA
    "state/tls_key.pem",  # the server's HTTPS key
    "state/host_key",  # the server's SSH host key
    "state/directory/enrolment.json",  # the server's key on the account service (enrol)
)
# The one file of the world that is meant to hold the enrolment code: the
# reference server's codes (it reads the code from there).
WORLD_CODE_FILES = ("control/codes",)
# A private key in PEM form, raw or JSON-escaped: the header and the start of
# its body.
_PRIVATE_PEM = re.compile(rb"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(?:\\n|\\r|\r|\n)+[A-Za-z0-9+/=]{40}")
# A JWT-shaped string (an access token, a client assertion): "eyJ" (the
# base64url of '{"'), 20 or more base64url characters, a dot, more, a dot.
_JWT = re.compile(rb"eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]+\.")


# ----------------------------------------------------------------------------
# Pure helpers (unit-tested in test_fixture_units.py)
# ----------------------------------------------------------------------------


def skip_reason(environ: Mapping[str, str] = os.environ, system: str = sys.platform) -> Optional[str]:
    """Why the directory cases cannot run here, or None. They run only under
    `make test-integration-directory`, which builds `bin/` and the reference
    server and sets SHED_DIRECTORY_ITEST=1, and only on Linux (the remote
    backend's local link runs a shed-agent on this host)."""
    if environ.get("SHED_DIRECTORY_ITEST", "") != "1":
        return ("the directory cases start their own reference server and shed-server from bin/: "
                "run make test-integration-directory")
    if not system.startswith("linux"):
        return (f"the directory cases run the remote backend's local link, which is Linux only "
                f"(this host is {system})")
    if not environ.get("SHED_BIN_DIR", "").strip():
        return "SHED_BIN_DIR is not set: run make test-integration-directory"
    return None


def required_binaries(bin_dir: Path) -> list[Path]:
    """What the world runs from `bin/`."""
    return [bin_dir / "shed", bin_dir / "shed-server", bin_dir / "shed-agent",
            bin_dir / "test" / "shed-directory-ref"]


def missing_binaries(bin_dir: Path) -> list[Path]:
    return [p for p in required_binaries(bin_dir) if not (p.is_file() and os.access(p, os.X_OK))]


def _kernel_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind((HOST, 0))
        return s.getsockname()[1]


def free_ports(n: int, *, avoid: frozenset[int] = FORBIDDEN_PORTS,
               pick: Callable[[], int] = _kernel_port, attempts: int = 200) -> list[int]:
    """`n` distinct loopback ports the kernel says are free now, none of them
    in `avoid`. (Free when asked: the caller binds them soon after.)"""
    got: list[int] = []
    for _ in range(attempts):
        p = pick()
        if p in avoid or p in got:
            continue
        got.append(p)
        if len(got) == n:
            return got
    raise RuntimeError(f"no {n} free loopback ports outside {sorted(avoid)} after {attempts} tries")


def socket_base(tmp: str) -> str:
    """Where the world's folder goes: the temporary directory, unless its path
    is too long for the unix sockets inside the folder, then /tmp."""
    return tmp if len(tmp) <= MAX_SOCKET_BASE else "/tmp"


def agent_arch(machine: str) -> str:
    """The GOARCH of this host, for the agent's file name
    (`<agent_dir>/shed-agent-linux-<arch>`)."""
    arch = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(machine.lower())
    if arch is None:
        raise RuntimeError(f"no shed-agent build for machine {machine!r}")
    return arch


def ssh_fingerprint(pub_line: str) -> str:
    """The SHA-256 fingerprint of an authorized_keys line, as ssh-keygen -l
    prints it (and as a client certificate's CN carries it)."""
    blob = base64.b64decode(pub_line.split()[1])
    return "SHA256:" + base64.b64encode(hashlib.sha256(blob).digest()).decode("ascii").rstrip("=")


def server_config(*, http_port: int, https_port: int, ssh_port: int, state_dir: Path, issuer: str,
                  admin_key: str, agent_dir: Path, refresh: str = REFRESH, trust: str = TRUST) -> dict:
    """The shed server's config: the remote backend on its test drivers (no
    cloud VM, no root), mtls, one local admin, and the directory at `issuer`
    over plain HTTP on loopback."""
    return {
        "name": SERVER_NAME,
        "http_port": http_port,
        "https_port": https_port,
        "ssh_port": ssh_port,
        "bind_address": HOST,
        "default_backend": "remote",
        "log_level": "debug",
        # Explicit: without it the server would keep its identity in ~/.shed.
        "state_dir": str(state_dir),
        "auth": {
            "mode": "mtls",
            "users": {LOCAL_ADMIN: {"admin": True, "keys": [{"key": admin_key, "label": "itest"}]}},
            "directory": {"url": issuer, "refresh": refresh, "trust": trust, "allow_plain_http": True},
        },
        "remote": {
            "provider": "fake",
            "transport": "local",
            "allow_test_drivers": True,
            "environment": "itest-dir",
            "state_dir": str(state_dir / "remote"),
            "default_image": "family/fake-image",
            # serve runs <agent_dir>/shed-agent-linux-<arch>.
            "agent_dir": str(agent_dir),
            "agent_install": "never",
            # Static mode needs a source; a documentation address, which the
            # fake provider's rule is made to match.
            "firewall": {"mode": "static", "source_ranges": ["192.0.2.10/32"]},
        },
    }


def listed_user(name: str, keys: Sequence[tuple[str, str]], admin: bool = False) -> dict:
    """One user of the reference server's users.json: `keys` are
    (authorized_keys line, label) pairs."""
    return {"name": name, "admin": bool(admin), "keys": [{"key": k, "label": label} for k, label in keys]}


def write_private(path: Path, data: bytes) -> None:
    """Replace `path` with `data` atomically, mode 0600 (the reference server
    refuses a control file writable by group or others, and must never read
    half a file)."""
    tmp = path.with_name(f".{path.name}.tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        os.fchmod(fd, 0o600)
        os.write(fd, data)
        os.fsync(fd)
    finally:
        os.close(fd)
    os.replace(tmp, path)


_LISTENING = re.compile(r"^listening on (http://127\.0\.0\.1:\d+)\s*$", re.MULTILINE)


def parse_issuer(stdout: str) -> Optional[str]:
    """The issuer the reference server printed ("listening on <url>")."""
    m = _LISTENING.search(stdout)
    return m.group(1) if m else None


_CERT_USER = re.compile(r"(?:^|[,/\s=])(?:UID|userId)\s*=\s*([^,/+\s]+)")


def cert_user(subject: str) -> Optional[str]:
    """The user a client certificate names (its userId attribute), from
    `openssl x509 -noout -subject`; None when it names none."""
    m = _CERT_USER.search(subject)
    return m.group(1) if m else None


def cert_request_config(fingerprint: str, scope: str, kind: str, user: Optional[str]) -> str:
    """An `openssl req` config whose subject is a user certificate's as the
    server's CA issues it (servertls.clientSubject): CN the key's
    fingerprint, OU the scope, O the client kind, and the userId attribute
    only when `user` is given. Without it the certificate is one issued
    before certificates named their user (unbound)."""
    lines = ["[req]", "prompt = no", "distinguished_name = dn", "", "[dn]",
             f"O = {kind}", f"OU = {scope}", f"CN = {fingerprint}"]
    if user is not None:
        lines.append(f"UID = {user}")
    return "\n".join(lines) + "\n"


CLIENT_CERT_EXTENSIONS = """[v3]
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature
extendedKeyUsage = clientAuth
"""


def parse_stamp(s: str) -> datetime:
    """An RFC 3339 instant from the server (status JSON)."""
    return datetime.fromisoformat(s.replace("Z", "+00:00")).astimezone(timezone.utc)


def trust_deadline(directory: Mapping, trust_seconds: float = TRUST_SECONDS) -> datetime:
    """When the list in force stops being trusted, computed here rather than
    taken from the server: its last good pull (the instant its answer
    arrived) plus the configured trust time."""
    return parse_stamp(directory["last_good_pull"]) + timedelta(seconds=trust_seconds)


def _cmdline(pid: int, proc: Path = Path("/proc")) -> str:
    try:
        return (proc / str(pid) / "cmdline").read_bytes().replace(b"\0", b" ").decode("utf-8", "replace")
    except OSError:
        return ""


def _cwd(pid: int, proc: Path = Path("/proc")) -> str:
    try:
        return os.readlink(proc / str(pid) / "cwd")
    except OSError:
        return ""


def _stat_fields(pid: int, proc: Path = Path("/proc")) -> Optional[tuple[str, int, int]]:
    """A process's state letter, start time (clock ticks after boot, field 22
    of /proc/<pid>/stat) and parent's pid (field 4), from one reading, or None
    when there is no such process."""
    try:
        data = (proc / str(pid) / "stat").read_text(encoding="utf-8", errors="replace")
    except OSError:
        return None
    # The name (field 2) may hold spaces and parentheses: fields 3 on follow
    # the last ")".
    try:
        rest = data[data.rindex(")") + 2:].split()
        return rest[0], int(rest[19]), int(rest[1])
    except (ValueError, IndexError):
        return None


def proc_stat(pid: int, proc: Path = Path("/proc")) -> Optional[tuple[str, int]]:
    """A process's state letter and start time (clock ticks after boot, field
    22 of /proc/<pid>/stat), or None when there is no such process. The start
    time tells a process from a later one that reuses its pid."""
    st = _stat_fields(pid, proc)
    return None if st is None else (st[0], st[1])


@dataclass(frozen=True)
class ProcHandle:
    """A process the fixture did not start itself, pinned by its pid and the
    start time read when it was found. It is signalled only while that pid
    still has that start time, checked just before each signal, and the
    signal goes through a pidfd opened before the check, so a pid reused
    between the check and the signal is never signalled."""

    pid: int
    start: int
    cmdline: str = ""
    # The parent's pid, read with the start time (one reading of stat).
    ppid: int = 0

    @classmethod
    def of(cls, pid: int, proc: Path = Path("/proc")) -> Optional["ProcHandle"]:
        st = _stat_fields(pid, proc)
        if st is None or st[0] == "Z":
            return None
        return cls(pid, st[1], _cmdline(pid, proc), st[2])

    @classmethod
    def pin(cls, pid: int, proc: Path = Path("/proc")) -> Optional["ProcHandle"]:
        """The handle alone (pid, start time, parent), read from one reading
        of stat, before anything else is read of the process."""
        st = _stat_fields(pid, proc)
        if st is None or st[0] == "Z":
            return None
        return cls(pid, st[1], "", st[2])

    def alive(self, proc: Path = Path("/proc")) -> bool:
        """Still this process, and not a zombie."""
        st = proc_stat(self.pid, proc)
        return st is not None and st[1] == self.start and st[0] != "Z"

    def signal(self, sig: int) -> bool:
        """Send `sig` to this very process; False (not an error) when it has
        gone, or its pid is another process's now."""
        try:
            fd = os.pidfd_open(self.pid)
        except ProcessLookupError:
            return False
        try:
            if not self.alive():
                return False
            signal.pidfd_send_signal(fd, sig)
            return True
        except ProcessLookupError:
            return False
        finally:
            os.close(fd)

    def stop(self, grace: float = 5) -> Optional[str]:
        """SIGTERM, then SIGKILL after `grace`; how it ended, or None when it
        had already gone. A process this user may not signal (another user's
        child of the world's) is left running and said so, never passed
        over."""
        try:
            if not self.signal(signal.SIGTERM):
                return None
            if _wait_for(lambda: not self.alive(), grace, interval=0.1):
                return "stopped on SIGTERM"
            if not self.signal(signal.SIGKILL):
                return "stopped on SIGTERM"
        except PermissionError as e:
            return f"could not be signalled ({e.strerror or e}), and is still running"
        return "killed" if _wait_for(lambda: not self.alive(), 5, interval=0.1) else "still running after SIGKILL"


def _proc_uid(pid: int, proc: Path = Path("/proc")) -> Optional[int]:
    """The uid that owns a process's /proc entry, or None when it has gone."""
    try:
        return (proc / str(pid)).stat().st_uid
    except OSError:
        return None


def owned_processes(marker: Optional[str], proc: Path = Path("/proc"), uid: Optional[int] = None,
                    me: Optional[int] = None, roots: Sequence[ProcHandle] = ()) -> list[ProcHandle]:
    """The processes the world owns, as handles: every live process of this
    user (not this one) whose command line or working directory holds
    `marker` (a path inside a world's folder; None: none is looked for), and
    every process below each of those or below one of `roots` (processes the
    caller started itself and pinned; they are followed, not returned),
    whatever its user. Every clean-up path finds what to stop here.

    The handle comes first: every process is pinned (its pid, start time and
    parent, from one reading of stat) before anything else of it is read.
    Ownership is then decided from readings made after the handle (the user,
    the command line, the working directory), and holds only when the start
    time, read again after them, is still the handle's: so the readings were
    that process's, and a pid reused at any point between the handle and the
    decision is never selected. A process below one selected (or below a
    root) is selected, whatever its user, when its parent (from its own
    handle's reading) is one selected, started no later than it, and still
    that process (its start time read again) after the child's handle was
    taken; its command line is then read and its start time checked again.
    A child of another user is so followed and returned: if it cannot be
    signalled, `stop_survivors` reports it."""
    uid = os.getuid() if uid is None else uid
    me = os.getpid() if me is None else me
    handles: dict[int, ProcHandle] = {}
    for d in proc.iterdir():
        if not d.name.isdigit() or int(d.name) == me:
            continue
        h = ProcHandle.pin(int(d.name), proc)
        if h is not None:
            handles[h.pid] = h
    root_pids = {r.pid for r in roots}
    found: dict[int, ProcHandle] = {r.pid: r for r in roots if r.alive(proc)}
    if marker is not None:
        for h in handles.values():
            if h.pid in found:
                continue
            owner, cmd, cwd = _proc_uid(h.pid, proc), _cmdline(h.pid, proc), _cwd(h.pid, proc)
            if owner == uid and (marker in cmd or (cwd and marker in cwd + "/")) and h.alive(proc):
                found[h.pid] = replace(h, cmdline=cmd)
    grew = True
    while grew:
        grew = False
        for h in handles.values():
            parent = found.get(h.ppid)
            if h.pid in found or parent is None or parent.start > h.start or not parent.alive(proc):
                continue
            cmd = _cmdline(h.pid, proc)
            if h.alive(proc):
                found[h.pid] = replace(h, cmdline=cmd)
                grew = True
    return [h for h in found.values() if h.pid not in root_pids]


def stop_survivors(handles: Sequence[ProcHandle], what: str) -> list[str]:
    """Stop every handle still alive; one problem line for each."""
    problems = []
    seen: set[tuple[int, int]] = set()
    for h in handles:
        if (h.pid, h.start) in seen:
            continue
        seen.add((h.pid, h.start))
        if not h.alive():
            continue
        how = h.stop()
        if how is not None:
            verb = "" if how.startswith("could not") else "was "
            problems.append(f"process {h.pid} {what} outlived it and {verb}{how}: {h.cmdline!r}")
    return problems


def secrets_in(root: Path, code: str) -> list[str]:
    """What under `root` (a world's folder) holds the enrolment `code`, a
    private key or a JWT-shaped string (an access token or an assertion)
    where it should not: every regular file of the folder is read (the
    server's HOME and TMPDIR, its config, its state, the kept copy, every
    log, the reference server's included, the certificate folders and the
    identities' homes) except `WORLD_CODE_FILES` for the code and
    `WORLD_PRIVATE_KEY_FILES` (the files meant to hold a key) for a private
    key or a JWT-shaped string. A file that holds a base64 `body` (the kept
    copy holds the directory's answer so) is also searched decoded."""
    hits = []
    needle = code.encode()
    for p in sorted(root.rglob("*")):
        if p.is_symlink() or not p.is_file():
            continue
        rel = p.relative_to(root).as_posix()
        try:
            data = p.read_bytes()
        except OSError as e:
            hits.append(f"{rel}: unreadable ({e})")
            continue
        key_file = any(path_matches(rel, pat) for pat in WORLD_PRIVATE_KEY_FILES)
        views = [("", data)]
        body = _kept_body(data)
        if body is not None:
            views.append(("its kept body ", body))
        for where, text in views:
            if needle in text and rel not in WORLD_CODE_FILES:
                hits.append(f"{rel}: {where}holds the enrolment code")
            if _PRIVATE_PEM.search(text) and not key_file:
                hits.append(f"{rel}: {where}holds a private key")
            if _JWT.search(text) and not key_file:
                hits.append(f"{rel}: {where}holds a JWT-shaped string")
    return hits


def _kept_body(data: bytes) -> Optional[bytes]:
    """The decoded `body` of a JSON object that holds one in base64 (the kept
    copy, `users.json`, holds the directory's answer so), or None."""
    if not data.lstrip().startswith(b"{"):
        return None
    try:
        obj = json.loads(data)
        body = obj.get("body") if isinstance(obj, dict) else None
        return base64.b64decode(body, validate=True) if isinstance(body, str) else None
    except ValueError:
        return None


def path_matches(rel: str, pattern: str) -> bool:
    """Whether the relative path `rel` is `pattern`, whose `*` stands for
    part of one path component, never a `/`."""
    parts, pats = rel.split("/"), pattern.split("/")
    return len(parts) == len(pats) and all(fnmatch.fnmatchcase(a, p) for a, p in zip(parts, pats))


def shed_tree(home: Path) -> dict[str, tuple]:
    """Everything the CLI keeps for an identity: every entry under its
    `~/.shed`, the folder itself included (`config.yaml`, `known_hosts`, the
    credential files, lock files, temporary files, staging folders, empty
    folders, anything else), each as its path relative to `home` and a
    tuple of its type, its permission bits and its content: a regular file's
    SHA-256, a symbolic link's target, None for a folder (whose entries are
    listed on their own), the file type for anything else. No link is
    followed. Two equal results mean nothing the CLI keeps was written, made
    or removed. Times are not compared, so a lock taken and released, which
    leaves its file's bytes as they were, is no change; nothing is left
    out."""
    out: dict[str, tuple] = {}

    def visit(p: Path) -> None:
        st = os.lstat(p)
        rel, mode = p.relative_to(home).as_posix(), stat.S_IMODE(st.st_mode)
        if stat.S_ISLNK(st.st_mode):
            out[rel] = ("symlink", mode, os.readlink(p))
        elif stat.S_ISDIR(st.st_mode):
            out[rel] = ("dir", mode, None)
            for name in sorted(os.listdir(p)):
                visit(p / name)
        elif stat.S_ISREG(st.st_mode):
            out[rel] = ("file", mode, hashlib.sha256(p.read_bytes()).hexdigest())
        else:
            out[rel] = ("other", mode, stat.S_IFMT(st.st_mode))

    root = home / ".shed"
    if os.path.lexists(root):
        visit(root)
    return out


# One member of a server entry as the CLI writes it: a plain key, a colon,
# and a value on the same line.
_ENTRY_MEMBER = re.compile(r"([A-Za-z_][A-Za-z0-9_-]*):(?:[ ]+(\S.*))?")
_DOUBLE_QUOTED = re.compile(r'"(?:[^"\\]|\\.)*"(?:[ ]+#.*)?')
_SINGLE_QUOTED = re.compile(r"'(?:[^']|'')*'(?:[ ]+#.*)?")


def _single_line_value(value: Optional[str]) -> bool:
    """Whether `value`, what follows `key: ` on a member's line, is a whole
    scalar on that line: not missing (a nested collection or a null), not
    the start of a flow collection or a block scalar, no anchor, alias or
    tag, and a quoted value closed on the line."""
    if not value or value[0] in "{[|>&*!%@`":
        return False
    if value[0] == '"':
        return _DOUBLE_QUOTED.fullmatch(value) is not None
    if value[0] == "'":
        return _SINGLE_QUOTED.fullmatch(value) is not None
    return True


def drop_entry_field(text: str, entry: str, field: str) -> str:
    """config.yaml's text with the one `<field>:` line of the server entry
    `entry` removed, as a person removes it by hand: every other byte is
    kept. The entry is under the top-level `servers:`, indented with spaces,
    and each of its lines must be one member at the indentation of its first
    member with its value on that line (`key: value`), as the CLI writes it.
    Raises ValueError on any line inside the entry that is not (a nested
    member, a key with no value, a value that opens a flow collection or a
    block scalar, a quoted value that does not end on its line), on a line
    indented with a tab, and unless exactly one such `<field>:` line is
    found."""
    lines = text.splitlines(keepends=True)
    in_servers, entry_indent, inside, member_indent = False, None, False, None
    hits = []
    for i, line in enumerate(lines):
        body = line.rstrip("\r\n")
        stripped = body.strip()
        if not stripped or stripped.startswith("#"):
            continue
        lead = body[:len(body) - len(body.lstrip(" \t"))]
        if "\t" in lead:
            raise ValueError(f"line {i + 1} is indented with a tab")
        indent = len(lead)
        if indent == 0:
            in_servers, entry_indent, inside = stripped == "servers:", None, False
            continue
        if not in_servers:
            continue
        if entry_indent is None:
            entry_indent = indent
        if indent <= entry_indent:
            inside, member_indent = stripped == f"{entry}:", None
            continue
        if inside:
            if member_indent is None:
                member_indent = indent
            m = _ENTRY_MEMBER.fullmatch(stripped)
            if indent != member_indent or m is None or not _single_line_value(m.group(2)):
                raise ValueError(f"line {i + 1} of server entry {entry!r} is not a one-line `key: value` member: "
                                 f"{stripped!r}")
            if m.group(1) == field:
                hits.append(i)
    if len(hits) != 1:
        raise ValueError(f"{len(hits)} {field!r} lines in server entry {entry!r}, not one")
    del lines[hits[0]]
    return "".join(lines)


_AGENT_SHIM = """#!/bin/sh
# Integration-suite identity {user!s}: the system ssh with every key of the
# world's key agent, in the order the agent holds them, as a person's ssh
# offers its agent's keys.
#
# It adds only what keeps the run apart from the account running the tests:
# no ssh config of that account (-F /dev/null); the world's agent and never
# the caller's (IdentityAgent, SSH_AUTH_SOCK unset); no default key file (the
# IdentityFile, which does not exist: ssh reads the default key files from
# the account's home in the password database, never from $HOME). Every other
# option (BatchMode, the password and keyboard-interactive switches,
# UpdateHostKeys, ControlMaster, ControlPath, IdentitiesOnly, -i) is the
# CLI's own to set, and the cases check the CLI sets what it should.
#
# Each run first records the arguments it was given (before its own), NUL
# after each, in a new file of the identity's argv folder.
unset SSH_AUTH_SOCK
rec=$(mktemp {record}) || exit 255
if [ "$#" -gt 0 ]; then printf '%s\\0' "$@" >"$rec" || exit 255; fi
exec {ssh} -F /dev/null -o IdentityAgent={sock} -o IdentityFile={no_key} "$@"
"""

# What a path the shim hands ssh may not hold: whitespace and quotes (ssh
# splits an option's value on them), `%` and `~` and `$` (ssh expands
# tokens, a home folder and environment variables in IdentityAgent and
# IdentityFile), a backslash (it unescapes) and `#` (a comment). A world's paths are
# short ASCII; one that holds any of these is refused, never quoted around.
_SHIM_PATH_REFUSED = frozenset(" \t\r\n\"'%~$\\#")


def agent_shim_text(user: str, sock: Path, no_key: Path, ssh: str, argv_dir: Path) -> str:
    """The `ssh` shim of an identity whose keys are an agent's (`KeyAgent`),
    recording each run's arguments in `argv_dir` (`recorded_argv`): split
    out so the unit tests can run it. Raises ValueError for a path that is
    not absolute or holds a character ssh would read as more than a path."""
    for p in (sock, no_key, argv_dir, Path(ssh)):
        s = str(p)
        bad = sorted(set(s) & _SHIM_PATH_REFUSED)
        if not s.startswith("/") or bad:
            raise ValueError(f"the shim cannot hand ssh the path {s!r}: "
                             + (f"it holds {''.join(bad)!r}" if bad else "it is not absolute"))
    return _AGENT_SHIM.format(user=user, ssh=shlex.quote(ssh), sock=shlex.quote(str(sock)),
                              no_key=shlex.quote(str(no_key)), record=shlex.quote(f"{argv_dir}/run.XXXXXXXX"))


def recorded_argv(argv_dir: Path) -> list[list[str]]:
    """The arguments of each run the agent shim recorded in `argv_dir`, in
    the order the runs were made."""
    runs = sorted(argv_dir.glob("run.*"), key=lambda p: (p.stat().st_mtime_ns, p.name))
    out = []
    for p in runs:
        data = p.read_bytes()
        out.append(data.split(b"\0")[:-1] if data else [])
    return [[a.decode("utf-8", "surrogateescape") for a in run] for run in out]


def has_option(argv: Sequence[str], flag: str, value: str) -> bool:
    """Whether `argv` holds `flag` followed by `value` (`-o X=y`, `-i path`)."""
    return any(a == flag and b == value for a, b in zip(argv, argv[1:]))


# ----------------------------------------------------------------------------
# A counting relay
# ----------------------------------------------------------------------------


class Relay:
    """A test-owned TCP relay on 127.0.0.1, in front of `target` (a port on
    127.0.0.1): it counts every connection it accepts, before a byte is
    read, then forwards bytes both ways until either side ends. A connection
    that is opened and closed with nothing sent is counted too. One thread
    accepts; each connection has a thread per direction. `close` stops all
    of it (the world's clean-up calls it)."""

    def __init__(self, target: int) -> None:
        """Nothing listens until `start` (so the world can register it for
        clean-up first)."""
        self.target = target
        self.port = 0
        self._accepts = 0
        self._lock = threading.Lock()
        self._closed = threading.Event()
        self._listener: Optional[socket.socket] = None
        self._socks: set[socket.socket] = set()
        self._threads: list[threading.Thread] = []
        # The relay's own marker connections (`settle`), by their port.
        self._markers: dict[int, threading.Event] = {}

    @property
    def accepts(self) -> int:
        with self._lock:
            return self._accepts

    def settle(self, timeout: float = 10) -> int:
        """The count once every connection made before this call has been
        accepted: a marker connection of the relay's own joins the same
        queue behind them, and the relay neither counts nor forwards it, so
        once it is accepted every one before it has been counted. (Reading
        `accepts` straight after a command could miss a connection the
        kernel took that the relay's thread had not yet accepted.)"""
        marker = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        try:
            marker.bind((HOST, 0))
            seen = threading.Event()
            port = marker.getsockname()[1]
            with self._lock:
                self._markers[port] = seen
            try:
                marker.settimeout(timeout)
                marker.connect((HOST, self.port))
                if not seen.wait(timeout):
                    raise AssertionError(f"the relay on port {self.port} did not accept its marker in {timeout} s")
            finally:
                with self._lock:
                    self._markers.pop(port, None)
        finally:
            marker.close()
        return self.accepts

    def start(self) -> "Relay":
        ls = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        ls.bind((HOST, 0))
        ls.listen(64)
        # accept wakes up to see `close`.
        ls.settimeout(0.2)
        self._listener, self.port = ls, ls.getsockname()[1]
        self._spawn(self._serve)
        return self

    def _spawn(self, fn: Callable, *args) -> None:
        t = threading.Thread(target=fn, args=args, daemon=True)
        with self._lock:
            self._threads.append(t)
        t.start()

    def _keep(self, s: socket.socket) -> bool:
        """Track `s` to close it; False (and `s` closed) once closing."""
        with self._lock:
            if not self._closed.is_set():
                self._socks.add(s)
                return True
        s.close()
        return False

    def _serve(self) -> None:
        while not self._closed.is_set():
            try:
                conn, peer = self._listener.accept()
            except socket.timeout:
                continue
            except OSError:
                return
            with self._lock:
                marker = self._markers.get(peer[1])
                if marker is None:
                    self._accepts += 1
            if marker is not None:
                conn.close()
                marker.set()
                continue
            conn.settimeout(None)
            if self._keep(conn):
                self._spawn(self._pair, conn)

    def _pair(self, conn: socket.socket) -> None:
        try:
            up = socket.create_connection((HOST, self.target), timeout=10)
            up.settimeout(None)
        except OSError:
            conn.close()
            return
        if not self._keep(up):
            conn.close()
            return
        back = threading.Thread(target=self._pump, args=(up, conn), daemon=True)
        back.start()
        self._pump(conn, up)
        back.join()
        for s in (conn, up):
            s.close()

    @staticmethod
    def _pump(src: socket.socket, dst: socket.socket) -> None:
        try:
            while True:
                data = src.recv(65536)
                if not data:
                    break
                dst.sendall(data)
        except OSError:
            pass
        finally:
            try:
                dst.shutdown(socket.SHUT_WR)
            except OSError:
                pass

    def close(self, timeout: float = 5) -> None:
        with self._lock:
            self._closed.set()
            socks, self._socks = list(self._socks), set()
            threads = list(self._threads)
        if self._listener is not None:
            self._listener.close()
        for s in socks:
            try:
                s.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            s.close()
        for t in threads:
            t.join(timeout)


# ----------------------------------------------------------------------------
# The API: answered, refused by the server, or a failure
# ----------------------------------------------------------------------------


class TransportFailure(AssertionError):
    """An API request the server neither answered nor refused (no connection,
    a reset, a timeout, a TLS error other than its refusal): a test failure,
    never a refusal."""


@dataclass
class ApiAnswer:
    """The server's answer to one API request. `status` and `body` are None
    when it refused the certificate at the handshake; `refusal` says how the
    server refused ("" when it did not)."""

    status: Optional[int]
    body: Optional[dict]
    refusal: str = ""

    @property
    def refused(self) -> bool:
        return bool(self.refusal)

    def __str__(self) -> str:
        if self.status is None:
            return f"refused at the TLS handshake ({self.refusal})"
        return f"HTTP {self.status} {json.dumps(self.body)[:300]}"


def http_refusal(status: Optional[int], body: Optional[dict]) -> str:
    """How an HTTP answer refuses the caller: a 401 or 403 carrying the
    server's error code; "" for any other answer."""
    code = error_code(body) if isinstance(body, dict) else None
    if status in (401, 403) and code:
        return f"HTTP {status} {code}"
    return ""


def tls_refusal(err: BaseException) -> str:
    """How a TLS error is the server's refusal of the client certificate (its
    bad_certificate alert); "" for any other error."""
    reason = getattr(err, "reason", None) if isinstance(err, ssl.SSLError) else None
    return f"TLS alert {reason}" if reason in REFUSAL_ALERTS else ""


def api_request(host: str, port: int, pin: str, cert: tuple[str, str], path: str,
                timeout: float = 15) -> ApiAnswer:
    """GET `path` with the client certificate `cert` (cert, key), trusting
    only the server certificate whose fingerprint is `pin`. Raises
    `TransportFailure` unless the server answered or refused (module
    docstring), and `PinMismatch` for another server."""
    where = f"{host}:{port}{path}"
    try:
        pem = server_cert_pem(host, port)
    except OSError as e:
        raise TransportFailure(f"{where}: no TLS connection to the server: {e!r}") from e
    if fingerprint(pem) != pin:
        raise PinMismatch(f"{host}:{port} presented {fingerprint(pem)}, pinned {pin}")
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    ctx.check_hostname = False
    ctx.load_verify_locations(cadata=pem)
    # TLS 1.2: the server judges the client certificate before it finishes
    # the handshake, so its refusal arrives here and not after the request.
    ctx.maximum_version = ssl.TLSVersion.TLSv1_2
    ctx.load_cert_chain(*cert)
    conn = http.client.HTTPSConnection(host, port, context=ctx, timeout=timeout)
    try:
        try:
            conn.connect()
        except ssl.SSLError as e:
            why = tls_refusal(e)
            if why:
                return ApiAnswer(None, None, why)
            raise TransportFailure(f"{where}: the TLS handshake failed, not as the server's refusal: {e!r}") from e
        except OSError as e:
            raise TransportFailure(f"{where}: no connection to the server: {e!r}") from e
        try:
            conn.request("GET", path)
            resp = conn.getresponse()
            raw = resp.read()
        except (OSError, http.client.HTTPException) as e:
            raise TransportFailure(f"{where}: the request got no answer: {e!r}") from e
        try:
            body = json.loads(raw) if raw.strip() else None
        except json.JSONDecodeError:
            body = {"raw": raw.decode("utf-8", errors="replace")[:500]}
        return ApiAnswer(resp.status, body, http_refusal(resp.status, body))
    finally:
        conn.close()


def _tail(path: Path, lines: int = 60) -> str:
    try:
        return "\n".join(path.read_text(encoding="utf-8", errors="replace").splitlines()[-lines:])
    except OSError as e:
        return f"(unreadable: {e})"


def _wait_for(cond: Callable[[], bool], timeout: float, interval: float = 0.25) -> bool:
    deadline = time.monotonic() + timeout
    while True:
        if cond():
            return True
        if time.monotonic() >= deadline:
            return False
        time.sleep(interval)


# ----------------------------------------------------------------------------
# Processes
# ----------------------------------------------------------------------------


def _stop(proc: subprocess.Popen, timeout: float = 15) -> bool:
    """SIGTERM, then SIGKILL after `timeout`; True when it stopped on its own."""
    if proc.poll() is not None:
        return True
    proc.terminate()
    try:
        proc.wait(timeout=timeout)
        return True
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait(timeout=10)
        return False


def _base_env(home: Path, extra: Optional[Mapping[str, str]] = None) -> dict:
    env = {k: v for k, v in os.environ.items() if k != "SSH_AUTH_SOCK"}
    env["HOME"] = str(home)
    env["LC_ALL"] = "C"
    env.update(extra or {})
    return env


class RefServer:
    """The reference server of the directory contract, on a loopback port the
    kernel picks, reading `control/`."""

    def __init__(self, binary: Path, control: Path, logs: Path, home: Path) -> None:
        self.binary, self.control, self.logs, self.home = binary, control, logs, home
        self.proc: Optional[subprocess.Popen] = None
        self.issuer = ""
        self.out = logs / "ref.out"
        self.err = logs / "ref.err"

    def start(self, timeout: float = 20) -> None:
        with self.out.open("wb") as out, self.err.open("wb") as err:
            self.proc = subprocess.Popen(
                [str(self.binary), "--control", str(self.control), "--listen", f"{HOST}:0"],
                stdout=out, stderr=err, stdin=subprocess.DEVNULL, env=_base_env(self.home),
            )
        if not _wait_for(lambda: parse_issuer(self.out.read_text()) is not None or self.proc.poll() is not None,
                         timeout):
            raise AssertionError(f"the reference server did not print its address in {timeout} s:\n{_tail(self.err)}")
        issuer = parse_issuer(self.out.read_text())
        if issuer is None:
            raise AssertionError(f"the reference server exited ({self.proc.returncode}):\n{_tail(self.err)}")
        self.issuer = issuer

    def set_users(self, users: Sequence[dict]) -> None:
        write_private(self.control / "users.json", json.dumps(list(users), indent=1).encode())

    def set_codes(self, codes: Mapping[str, str]) -> None:
        write_private(self.control / "codes", "".join(f"{n} {c}\n" for n, c in codes.items()).encode())

    def outage(self, on: bool) -> None:
        """Every lookup the service makes fails, as in a storage outage, while
        on."""
        path = self.control / "outage"
        if on:
            write_private(path, b"")
        else:
            path.unlink(missing_ok=True)

    def log(self) -> str:
        """What the reference server has logged (its standard error)."""
        try:
            return self.err.read_text(encoding="utf-8", errors="replace")
        except OSError:
            return ""

    def token_requests(self) -> list[str]:
        """Each token request line it has logged (`TOKEN_REQUEST`), from the
        prefix on."""
        return [line[line.index(TOKEN_REQUEST):] for line in self.log().splitlines() if TOKEN_REQUEST in line]

    def stop(self) -> bool:
        return self.proc is None or _stop(self.proc)


class ShedServer:
    """`bin/shed-server` with the world's config, its own HOME and TMPDIR.
    Each start writes a new log file."""

    def __init__(self, binary: Path, config: Path, home: Path, tmpdir: Path, logs: Path, marker: str) -> None:
        self.binary, self.config, self.home, self.tmpdir, self.logs = binary, config, home, tmpdir, logs
        # Every agent the server starts runs from the world's agent folder
        # (`<marker>shed-agent-linux-<arch>`); this is how one that outlives
        # the server is told from anyone else's.
        self.marker = marker
        self.proc: Optional[subprocess.Popen] = None
        self.starts = 0
        self.log_path = logs / "server-0.log"
        self.ssh_port = 0
        # Where processes are read (a fake one in the fixture's unit tests).
        self.proc_root = Path("/proc")

    def env(self) -> dict:
        return _base_env(self.home, {"TMPDIR": str(self.tmpdir)})

    def run(self, *args: str, input: Optional[str] = None, timeout: float = 60) -> subprocess.CompletedProcess:
        """`shed-server --config <config> <args>`, once."""
        return subprocess.run(
            [str(self.binary), "--config", str(self.config), *args], capture_output=True, text=True,
            timeout=timeout, env=self.env(), input=input, stdin=None if input is not None else subprocess.DEVNULL,
        )

    def start(self, timeout: float = 60) -> None:
        self.starts += 1
        self.log_path = self.logs / f"server-{self.starts}.log"
        with self.log_path.open("wb") as log:
            self.proc = subprocess.Popen([str(self.binary), "--config", str(self.config), "serve"],
                                         stdout=log, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL,
                                         env=self.env())

        def ready() -> bool:
            text = self.log()
            return self.proc.poll() is not None or all(line in text for line in READY_LINES)

        if not _wait_for(ready, timeout) or self.proc.poll() is not None:
            raise AssertionError(f"shed-server did not come up (exit {self.proc.poll()}):\n{_tail(self.log_path)}")
        if not _wait_for(self._ssh_answers, 10):
            raise AssertionError(f"shed-server's SSH port {self.ssh_port} does not answer:\n{_tail(self.log_path)}")

    def _ssh_answers(self) -> bool:
        try:
            with socket.create_connection((HOST, self.ssh_port), timeout=2):
                return True
        except OSError:
            return False

    def log(self) -> str:
        """This start's log."""
        try:
            return self.log_path.read_text(encoding="utf-8", errors="replace")
        except OSError:
            return ""

    def stop(self) -> list[str]:
        """Stop the server, then every process it started that outlived it:
        those below it when it was stopped, and (whether or not it was still
        running) every agent of the local link, recognised by the agents'
        folder in its command line, with whatever runs under it. Returns what
        did not stop on its own."""
        problems: list[str] = []
        kids: list[ProcHandle] = []
        if self.proc is not None and self.proc.poll() is None:
            # The server is this fixture's own child, not yet reaped: its pid
            # is its own until it is waited for. What runs below it is found
            # through the one helper, handles first.
            server = ProcHandle.pin(self.proc.pid, self.proc_root)
            kids = owned_processes(None, self.proc_root, roots=[server] if server is not None else [])
            if not _stop(self.proc, timeout=20):
                problems.append(f"shed-server (pid {self.proc.pid}) did not stop on SIGTERM and was killed")
        kids += owned_processes(self.marker, self.proc_root)
        problems += stop_survivors(kids, "started by shed-server")
        return problems

    def restart(self) -> None:
        problems = self.stop()
        if problems:
            raise AssertionError("restart: " + "; ".join(problems))
        self.start()


# ----------------------------------------------------------------------------
# Identities, held connections, sessions
# ----------------------------------------------------------------------------


@dataclass
class Key:
    """A throwaway ed25519 key pair of the world."""

    label: str
    path: Path
    pub: str

    @property
    def fingerprint(self) -> str:
        return ssh_fingerprint(self.pub)


@dataclass
class DirIdentity(Identity):
    """An identity of the world: `bin/` first on its PATH, after its shim."""

    bin_dir: Optional[Path] = None
    # Where an agent identity's shim records each ssh run's arguments
    # (`World.agent_identity`); None for a one-key identity.
    argv_dir: Optional[Path] = None

    def env(self) -> dict:
        env = super().env()
        if self.bin_dir is not None:
            env["PATH"] = f"{self.shim.parent}{os.pathsep}{self.bin_dir}{os.pathsep}{os.environ.get('PATH', '')}"
        return env

    def ssh_runs(self) -> list[list[str]]:
        """The arguments of each ssh run so far, as the CLI gave them to the
        shim (`recorded_argv`)."""
        assert self.argv_dir is not None, f"{self.user}'s shim records nothing"
        return recorded_argv(self.argv_dir)

    def forget_ssh_runs(self) -> None:
        assert self.argv_dir is not None, f"{self.user}'s shim records nothing"
        for p in self.argv_dir.glob("run.*"):
            p.unlink()


@dataclass
class Probe:
    """One new SSH connection's answer to `_bootstrap control cli` (no CSR):
    a connection the server admits reaches the bootstrap handler, which asks
    for a CSR; one it refuses never gets past authentication."""

    code: int
    stdout: str
    stderr: str
    seconds: float

    @property
    def admitted(self) -> bool:
        return NEEDS_CSR in self.stderr

    @property
    def refused(self) -> bool:
        return self.code == 255 and "Permission denied" in self.stderr and not self.admitted

    def __str__(self) -> str:
        return f"exit {self.code} after {self.seconds:.1f} s, stderr={self.stderr.strip()[-400:]!r}"


@dataclass
class Cert:
    """A client certificate and its key."""

    cert: Path
    key: Path
    pin: str
    https_port: int

    def subject(self) -> str:
        r = subprocess.run(["openssl", "x509", "-in", str(self.cert), "-noout", "-subject", "-nameopt", "RFC2253"],
                           capture_output=True, text=True, timeout=30)
        assert r.returncode == 0, r.stderr
        return r.stdout.strip()

    def user(self) -> Optional[str]:
        return cert_user(self.subject())

    def get(self, path: str) -> ApiAnswer:
        """GET `path` with this certificate: answered or refused by the
        server, else `TransportFailure`."""
        return api_request(HOST, self.https_port, self.pin, (str(self.cert), str(self.key)), path)


class Session:
    """A long-running session channel whose command echoes its input (`cat`),
    with stdin and stdout held open: `echo` shows it still carries data."""

    def __init__(self, argv: Sequence[str], env: Mapping[str, str], err: Path) -> None:
        self.err = err
        with err.open("wb") as e:
            self.proc = subprocess.Popen(list(argv), stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=e,
                                         env=dict(env))
        self.buf = b""

    def read_until(self, token: bytes, timeout: float) -> bool:
        deadline = time.monotonic() + timeout
        sel = selectors.DefaultSelector()
        sel.register(self.proc.stdout, selectors.EVENT_READ)
        try:
            while token not in self.buf:
                left = deadline - time.monotonic()
                if left <= 0 or not sel.select(left):
                    return False
                chunk = os.read(self.proc.stdout.fileno(), 4096)
                if not chunk:
                    return False
                self.buf += chunk
            return True
        finally:
            sel.close()

    def echo(self, text: str, timeout: float = 15) -> bool:
        """Write a line and see it come back."""
        try:
            self.proc.stdin.write(text.encode() + b"\n")
            self.proc.stdin.flush()
        except (BrokenPipeError, OSError):
            return False
        return self.read_until(text.encode() + b"\n", timeout)

    def wait_closed(self, timeout: float) -> bool:
        return _wait_for(lambda: self.proc.poll() is not None, timeout)

    def close(self) -> None:
        try:
            self.proc.stdin.close()
        except OSError:
            pass
        _stop(self.proc, timeout=5)
        self.proc.stdout.close()

    def stderr(self) -> str:
        return _tail(self.err, 20)


class Held:
    """A held SSH connection: an OpenSSH control master (`-N`) as `who`, to
    `target` (`_bootstrap` or a shed). New channels go through its control
    socket only."""

    def __init__(self, world: "World", who: DirIdentity, target: str, n: int) -> None:
        """Nothing runs until `start` (so the world can register it for
        clean-up first)."""
        self.world, self.who, self.target = world, who, target
        self.ctl = world.sockets / f"m{n}"
        self.log = world.logs / f"master-{n}.log"
        self.sessions: list[Session] = []
        self.proc: Optional[subprocess.Popen] = None

    def argv(self) -> list[str]:
        return [
            self.world.ssh, "-F", "/dev/null", "-i", str(self.who.key),
            "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "BatchMode=yes",
            "-o", "PasswordAuthentication=no", "-o", "KbdInteractiveAuthentication=no",
            "-o", "UpdateHostKeys=no", "-o", f"UserKnownHostsFile={self.world.known_hosts}",
            "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=10",
            "-o", "ControlMaster=yes", "-o", f"ControlPath={self.ctl}", "-o", "ControlPersist=no",
            "-N", "-p", str(self.world.ssh_port), f"{self.target}@{HOST}",
        ]

    def start(self) -> "Held":
        who, target = self.who, self.target
        with self.log.open("wb") as log:
            self.proc = subprocess.Popen(self.argv(), stdin=subprocess.DEVNULL, stdout=log, stderr=log,
                                         env=who.env())
        if not _wait_for(lambda: self.proc.poll() is not None or (self.ctl.exists() and self.check()), 30):
            raise AssertionError(f"the held connection as {who.user} to {target} did not come up:\n{_tail(self.log)}")
        if self.proc.poll() is not None:
            raise AssertionError(f"the held connection as {who.user} to {target} was refused "
                                 f"(exit {self.proc.returncode}):\n{_tail(self.log)}")
        return self

    def _mux(self) -> list[str]:
        # A channel through the control socket. ProxyCommand=false: were the
        # socket gone, ssh would fall back to a new connection, and that one
        # fails at once instead of authenticating afresh.
        return [
            self.world.ssh, "-F", "/dev/null", "-i", str(self.who.key),
            "-o", f"ControlPath={self.ctl}", "-o", "ControlMaster=no", "-o", f"ProxyCommand={self.world.false}",
            "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "BatchMode=yes",
            "-o", f"UserKnownHostsFile={self.world.known_hosts}", "-o", "StrictHostKeyChecking=yes",
            "-p", str(self.world.ssh_port),
        ]

    def check(self) -> bool:
        r = subprocess.run([*self._mux(), "-O", "check", f"{self.target}@{HOST}"], capture_output=True, text=True,
                           timeout=15, env=self.who.env(), stdin=subprocess.DEVNULL)
        return r.returncode == 0

    def probe(self, timeout: float = 30) -> Probe:
        """`_bootstrap control cli` on a new channel of this connection (a
        `_bootstrap` connection only)."""
        t0 = time.monotonic()
        r = subprocess.run([*self._mux(), "-T", f"{self.target}@{HOST}", "control", "cli"], capture_output=True,
                           text=True, timeout=timeout, env=self.who.env(), stdin=subprocess.DEVNULL)
        return Probe(r.returncode, r.stdout, r.stderr, time.monotonic() - t0)

    def run(self, command: str, timeout: float = 30) -> subprocess.CompletedProcess:
        """A new session channel running `command` (a shed connection)."""
        return subprocess.run([*self._mux(), "-T", f"{self.target}@{HOST}", command], capture_output=True,
                              text=True, timeout=timeout, env=self.who.env(), stdin=subprocess.DEVNULL)

    def open_cat(self, n: int) -> Session:
        """A new session channel running `echo ready; exec cat`, ready."""
        s = Session([*self._mux(), "-T", f"{self.target}@{HOST}", "echo ready; exec cat"], self.who.env(),
                    self.world.logs / f"session-{n}.err")
        self.sessions.append(s)
        if not s.read_until(b"ready\n", 30):
            raise AssertionError(f"the session through the held connection did not start: {s.stderr()}")
        return s

    def alive(self) -> bool:
        return self.proc is not None and self.proc.poll() is None

    def wait_closed(self, timeout: float) -> bool:
        return _wait_for(lambda: self.proc.poll() is not None, timeout)

    def close(self) -> None:
        for s in self.sessions:
            s.close()
        if self.proc is not None and self.proc.poll() is None:
            subprocess.run([*self._mux(), "-O", "exit", f"{self.target}@{HOST}"], capture_output=True,
                           timeout=15, env=self.who.env(), stdin=subprocess.DEVNULL)
            # 255 is ssh's normal exit after -O exit.
            _stop(self.proc, timeout=10)


class KeyAgent:
    """An `ssh-agent` of the world, in the foreground (`-D`), on a socket in
    the world's folder (its command line names the folder, so the world's
    clean-up finds it whatever happened). It holds the keys added to it, in
    that order, and ssh offers them in that order."""

    def __init__(self, agent: str, add: str, sock: Path, log: Path, home: Path) -> None:
        """Nothing runs until `start` (so the world can register it for
        clean-up first)."""
        self.agent, self.add_bin, self.sock, self.log, self.home = agent, add, sock, log, home
        self.proc: Optional[subprocess.Popen] = None

    def env(self) -> dict:
        return _base_env(self.home, {"SSH_AUTH_SOCK": str(self.sock)})

    def start(self, timeout: float = 10) -> "KeyAgent":
        with self.log.open("wb") as log:
            self.proc = subprocess.Popen([self.agent, "-D", "-a", str(self.sock)], stdin=subprocess.DEVNULL,
                                         stdout=log, stderr=log, env=_base_env(self.home))
        if not _wait_for(lambda: self.proc.poll() is not None or self.sock.exists(), timeout, interval=0.05):
            raise AssertionError(f"ssh-agent made no socket in {timeout} s:\n{_tail(self.log)}")
        if self.proc.poll() is not None:
            raise AssertionError(f"ssh-agent exited ({self.proc.returncode}):\n{_tail(self.log)}")
        return self

    def add(self, key: "Key") -> None:
        """Add `key` (its file path is the only argument; the key has no
        passphrase)."""
        r = subprocess.run([self.add_bin, str(key.path)], capture_output=True, text=True, timeout=30,
                           env=self.env(), stdin=subprocess.DEVNULL)
        assert r.returncode == 0, f"ssh-add {key.label}: {r.stderr}"

    def listed(self) -> list[str]:
        """The public keys the agent holds, in its order, as `type base64`."""
        r = subprocess.run([self.add_bin, "-L"], capture_output=True, text=True, timeout=30, env=self.env(),
                           stdin=subprocess.DEVNULL)
        if r.returncode == 1 and not r.stdout.strip():
            return []
        assert r.returncode == 0, f"ssh-add -L: {r.stderr}"
        return [" ".join(line.split()[:2]) for line in r.stdout.splitlines() if line.strip()]

    def close(self) -> None:
        if self.proc is not None:
            _stop(self.proc, timeout=5)
        self.sock.unlink(missing_ok=True)


# ----------------------------------------------------------------------------
# The world
# ----------------------------------------------------------------------------


class World:
    """The reference server, the directory server, the local admin, and the
    people of one case."""

    def __init__(self, root: Path, bin_dir: Path, ssh: str, false: str) -> None:
        self.root, self.bin_dir = root, bin_dir
        self.ssh, self.false = ssh, false
        self.keys_dir = root / "keys"
        self.homes = root / "homes"
        self.logs = root / "logs"
        self.control = root / "control"
        self.state = root / "state"
        self.agent_dir = root / "agent"
        self.sockets = root / "s"
        self.tmpdir = root / "t"
        for d in (self.keys_dir, self.homes, self.logs, self.control, self.state, self.agent_dir, self.sockets,
                  self.tmpdir):
            d.mkdir(mode=0o700)
        self.config_path = root / "server.yaml"
        self.known_hosts = root / "known_hosts"
        self.code = secrets.token_hex(16)
        self.client_id = ""
        self.ref = RefServer(bin_dir / "test" / "shed-directory-ref", self.control, self.logs, self.homes)
        self.server = ShedServer(bin_dir / "shed-server", self.config_path, self.homes / "server", self.tmpdir,
                                 self.logs, f"{self.agent_dir}/")
        self.users: dict[str, dict] = {}
        self.admin: Optional[DirIdentity] = None
        self.admin_key: Optional[Key] = None
        self.https_port = 0
        self.ssh_port = 0
        self._helpers: list = []
        self._n = 0

    # -- start -------------------------------------------------------------

    def start(self) -> None:
        (self.homes / "server").mkdir(mode=0o700)
        admin_key = self.admin_key = self.key(LOCAL_ADMIN)
        self.publish()  # an empty list: the reference server answers no list at all without a file
        self.ref.set_codes({SERVER_NAME: self.code})
        self.ref.start()

        http_port, self.https_port, self.ssh_port = free_ports(3)
        self.server.ssh_port = self.ssh_port
        agent = self.agent_dir / f"shed-agent-linux-{agent_arch(platform.machine())}"
        shutil.copyfile(self.bin_dir / "shed-agent", agent)
        agent.chmod(0o755)
        cfg = server_config(http_port=http_port, https_port=self.https_port, ssh_port=self.ssh_port,
                            state_dir=self.state, issuer=self.ref.issuer, admin_key=admin_key.pub,
                            agent_dir=self.agent_dir)
        write_private(self.config_path, yaml.safe_dump(cfg, sort_keys=False).encode())
        r = self.server.run("config-validate")
        assert r.returncode == 0, f"config-validate: {r.stdout}{r.stderr}"

        # Enrol: the code on standard input, never an argument.
        r = self.server.run("directory", "enroll", "--name", SERVER_NAME, input=self.code + "\n")
        self.enrol_output = r.stdout + r.stderr
        assert r.returncode == 0, f"directory enroll failed (exit {r.returncode}): {self.enrol_output}"
        m = re.search(r"\bas (srv_[0-9a-f]{32})\b", r.stdout)
        assert m, f"directory enroll named no client id: {r.stdout!r}"
        self.client_id = m.group(1)

        self.server.start()
        self.admin = self.identity(LOCAL_ADMIN, admin_key)
        self.admin.add_server()
        shutil.copyfile(self.admin.known_hosts, self.known_hosts)
        self.wait_state("trusted", 60)

    # -- keys, identities, the list ---------------------------------------

    def key(self, label: str) -> Key:
        """A new ed25519 key pair."""
        d = self.keys_dir / label
        d.mkdir(mode=0o700)
        path = d / "id_ed25519"
        r = subprocess.run(["ssh-keygen", "-t", "ed25519", "-N", "", "-q", "-C", f"itest-{label}", "-f", str(path)],
                           capture_output=True, text=True, timeout=30)
        assert r.returncode == 0, r.stderr
        pub = " ".join(path.with_suffix(".pub").read_text().split()[:2])
        return Key(label, path, pub)

    def identity(self, user: str, key: Key) -> DirIdentity:
        """`user` acting with `key` only: its own HOME, shim and CLI entry."""
        self._n += 1
        home = self.homes / f"{user}-{self._n}"
        home.mkdir(mode=0o700)
        shim = home / "bin" / "ssh"
        shim.parent.mkdir()
        shim.write_text(shim_text(user, key.path, self.ssh), encoding="utf-8")
        shim.chmod(0o755)
        return DirIdentity(user=user, key=key.path, home=home, entry=ENTRY, host=HOST, ssh_port=self.ssh_port,
                           shim=shim, bin_dir=self.bin_dir)

    def agent(self, keys: Sequence[Key]) -> KeyAgent:
        """A new key agent of the world holding `keys`, in that order."""
        tools = shutil.which("ssh-agent"), shutil.which("ssh-add")
        if not all(tools):
            raise AssertionError("ssh-agent and ssh-add must be on PATH")
        self._n += 1
        a = KeyAgent(tools[0], tools[1], self.sockets / f"a{self._n}", self.logs / f"agent-{self._n}.log",
                     self.homes)
        # Registered for clean-up before anything starts.
        self._helpers.append(a)
        a.start()
        for k in keys:
            a.add(k)
        assert a.listed() == [k.pub for k in keys], (a.listed(), [k.label for k in keys])
        return a

    def agent_identity(self, user: str, agent: KeyAgent) -> DirIdentity:
        """A person whose ssh offers every key `agent` holds, in its order,
        and no key file (`agent_shim_text`): its own HOME, shim and CLI
        entry, as `identity` makes them."""
        self._n += 1
        home = self.homes / f"{user}-{self._n}"
        home.mkdir(mode=0o700)
        no_key = self.keys_dir / "none" / "absent"
        argv_dir = self.root / "argv" / f"{user}-{self._n}"
        argv_dir.mkdir(parents=True, mode=0o700)
        shim = home / "bin" / "ssh"
        shim.parent.mkdir()
        shim.write_text(agent_shim_text(user, agent.sock, no_key, self.ssh, argv_dir), encoding="utf-8")
        shim.chmod(0o755)
        return DirIdentity(user=user, key=no_key, home=home, entry=ENTRY, host=HOST, ssh_port=self.ssh_port,
                           shim=shim, bin_dir=self.bin_dir, argv_dir=argv_dir)

    def relay(self, target: int) -> Relay:
        """A new counting relay of the world in front of `target`."""
        r = Relay(target)
        # Registered for clean-up before anything starts.
        self._helpers.append(r)
        return r.start()

    def set_user(self, name: str, keys: Sequence[Key], admin: bool = False) -> None:
        """Put `name` in the list (not served until `publish`)."""
        self.users[name] = listed_user(name, [(k.pub, k.label) for k in keys], admin)

    def remove_user(self, name: str) -> None:
        del self.users[name]

    def publish(self) -> None:
        """Serve the list as it now is (the next pull reads it)."""
        self.ref.set_users([self.users[n] for n in sorted(self.users)])

    # -- the local admin's view ---------------------------------------------

    def refresh(self) -> subprocess.CompletedProcess:
        """`shed users refresh` as the local admin (exits non-zero when the
        pull failed)."""
        return self.admin.cli("users", "refresh", timeout=120)

    def refresh_ok(self) -> None:
        r = self.refresh()
        assert r.returncode == 0 and PULL_GOOD in r.stdout, (
            f"shed users refresh: exit {r.returncode}: {r.stdout!r} {r.stderr!r}\n{_tail(self.server.log_path)}")

    def status(self) -> dict:
        """`shed --json users status` as the local admin."""
        r = self.admin.shed("--json", "-s", ENTRY, "users", "status", timeout=60)
        assert r.returncode == 0, f"shed users status: exit {r.returncode}: {r.stderr!r}"
        return json.loads(r.stdout)

    def wait_state(self, state: str, timeout: float) -> dict:
        got: dict = {}

        def reached() -> bool:
            nonlocal got
            got = self.status()
            return (got.get("directory") or {}).get("state") == state

        if not _wait_for(reached, timeout, interval=1):
            raise AssertionError(f"the directory's state is not {state!r} after {timeout} s: "
                                 f"{json.dumps(got.get('directory'))}\n{_tail(self.server.log_path)}")
        return got

    def user_row(self, name: str, st: Optional[dict] = None) -> Optional[dict]:
        for u in (st or self.status())["users"]:
            if u["name"] == name:
                return u
        return None

    def assert_local_admin_works(self, when: str) -> None:
        r = self.admin.cli("list", timeout=60)
        assert r.returncode == 0, f"{when}: the local admin's shed list failed: {r.stderr!r}"
        a = self.api(self.admin, "/api/sheds")
        assert a.status == 200, f"{when}: the local admin's API request: {a}"
        p = self.probe(self.admin)
        assert p.admitted, f"{when}: the local admin's SSH login: {p}"

    def assert_api_refused(self, cert: "Cert", when: str, path: str = "/api/sheds") -> ApiAnswer:
        """`cert`'s request is refused by the server itself; then the local
        admin's request is answered, so a server that went down cannot pass
        as one that refused."""
        a = cert.get(path)
        assert a.refused, f"{when}: the request was not refused: {a}"
        control = self.api(self.admin, "/api/sheds")
        assert control.status == 200, f"{when}: after the refusal, the local admin's request: {control}"
        return a

    # -- doors ------------------------------------------------------------

    def probe(self, who: DirIdentity, timeout: float = 60) -> Probe:
        """A NEW SSH connection as `who` (its one key), to `_bootstrap`."""
        t0 = time.monotonic()
        r = subprocess.run(
            [str(who.shim), "-T", "-p", str(self.ssh_port), "-o", f"UserKnownHostsFile={self.known_hosts}",
             "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=10", f"_bootstrap@{HOST}", "control", "cli"],
            capture_output=True, text=True, timeout=timeout, env=who.env(), stdin=subprocess.DEVNULL,
        )
        return Probe(r.returncode, r.stdout, r.stderr, time.monotonic() - t0)

    def ssh_run(self, who: DirIdentity, shed: str, command: str, timeout: float = 60) -> subprocess.CompletedProcess:
        """A new SSH connection as `who` into `shed`, running `command`."""
        return subprocess.run(
            [str(who.shim), "-T", "-p", str(self.ssh_port), "-o", f"UserKnownHostsFile={self.known_hosts}",
             "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=10", f"{shed}@{HOST}", command],
            capture_output=True, text=True, timeout=timeout, env=who.env(), stdin=subprocess.DEVNULL,
        )

    def session(self, who: DirIdentity, shed: str) -> Session:
        """A new SSH connection as `who` into `shed` running `cat`, ready."""
        self._n += 1
        s = Session([str(who.shim), "-T", "-p", str(self.ssh_port), "-o", f"UserKnownHostsFile={self.known_hosts}",
                     "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=10", f"{shed}@{HOST}",
                     "echo ready; exec cat"], who.env(), self.logs / f"session-{self._n}.err")
        self._helpers.append(s)
        if not s.read_until(b"ready\n", 60):
            raise AssertionError(f"the session as {who.user} into {shed} did not start: {s.stderr()}")
        return s

    def held(self, who: DirIdentity, target: str = "_bootstrap") -> Held:
        self._n += 1
        h = Held(self, who, target, self._n)
        # Registered for clean-up before anything starts.
        self._helpers.append(h)
        return h.start()

    def cli_cert(self, who: DirIdentity) -> Cert:
        """The client certificate `who`'s `shed server add` enrolled."""
        e = who.entry_config()
        assert e and e.get("client_cert_file"), f"{who.user} has no client certificate"
        return Cert(Path(e["client_cert_file"]), Path(e["client_key_file"]), e["tls_cert_fingerprint"],
                    self.https_port)

    def api(self, who: DirIdentity, path: str) -> ApiAnswer:
        """GET `path` with `who`'s CLI certificate: answered or refused by the
        server, else `TransportFailure`."""
        return self.cli_cert(who).get(path)

    def enrol_cert(self, who: DirIdentity, scope: str = "control") -> Cert:
        """A certificate of `scope` over `_bootstrap` on a NEW connection as
        `who`, as `shed server add` gets one."""
        self._n += 1
        d = self.root / "certs" / f"c{self._n}"
        d.mkdir(parents=True, mode=0o700)
        key, csr = d / "client.key", d / "client.csr.der"
        for argv in (["openssl", "ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", str(key)],
                     ["openssl", "req", "-new", "-key", str(key), "-subj", "/CN=itest", "-outform", "DER",
                      "-out", str(csr)]):
            r = subprocess.run(argv, capture_output=True, text=True, timeout=30)
            assert r.returncode == 0, r.stderr
        r = subprocess.run(
            [str(who.shim), "-T", "-p", str(self.ssh_port), "-o", f"UserKnownHostsFile={self.known_hosts}",
             "-o", "StrictHostKeyChecking=yes", "-o", "ConnectTimeout=10", f"_bootstrap@{HOST}", scope, "cli",
             "csr=" + base64.b64encode(csr.read_bytes()).decode("ascii")],
            capture_output=True, text=True, timeout=60, env=who.env(), stdin=subprocess.DEVNULL,
        )
        assert r.returncode == 0, f"bootstrap as {who.user} refused (exit {r.returncode}): {r.stderr!r}"
        bundle = json.loads(r.stdout)
        cert = d / "client.pem"
        cert.write_text(bundle["client_cert"], encoding="utf-8")
        return Cert(cert, key, bundle["tls_cert_fingerprint"], int(bundle["https_port"]))

    def forged_cert(self, key: Key, user: Optional[str], scope: str = "control") -> Cert:
        """A certificate for `key` signed by the server's own CA (the one in
        its state directory), shaped as the CA issues one, naming `user`, or
        no user at all (`None`: a certificate from before certificates named
        their user)."""
        self._n += 1
        d = self.root / "certs" / f"f{self._n}"
        d.mkdir(parents=True, mode=0o700)
        ckey, csr, cert = d / "client.key", d / "client.csr", d / "client.pem"
        (d / "req.cnf").write_text(cert_request_config(key.fingerprint, scope, "cli", user), encoding="utf-8")
        (d / "ext.cnf").write_text(CLIENT_CERT_EXTENSIONS, encoding="utf-8")
        for argv in (
            ["openssl", "ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", str(ckey)],
            ["openssl", "req", "-new", "-key", str(ckey), "-config", str(d / "req.cnf"), "-out", str(csr)],
            ["openssl", "x509", "-req", "-in", str(csr), "-CA", str(self.state / "ca_cert.pem"),
             "-CAkey", str(self.state / "ca_key.pem"), "-set_serial", "0x" + secrets.token_hex(16), "-days", "1",
             "-sha256", "-extfile", str(d / "ext.cnf"), "-extensions", "v3", "-out", str(cert)],
        ):
            r = subprocess.run(argv, capture_output=True, text=True, timeout=30)
            assert r.returncode == 0, f"{' '.join(argv[:2])}: {r.stderr}"
        return Cert(cert, ckey, self.cli_cert(self.admin).pin, self.https_port)

    # -- what shed keeps ---------------------------------------------------

    def assert_state_private(self) -> None:
        d = self.state / "directory"
        assert stat.S_IMODE(d.stat().st_mode) == 0o700, oct(d.stat().st_mode)
        for name in ("enrolment.json", "users.json"):
            p = d / name
            assert stat.S_IMODE(p.stat().st_mode) == 0o600, (name, oct(p.stat().st_mode))

    def secrets_found(self) -> list[str]:
        """Every file of the world's folder that holds the enrolment code, a
        private key or a JWT-shaped string where it should not
        (`secrets_in`)."""
        return secrets_in(self.root, self.code)

    # -- clean-up ------------------------------------------------------------

    def close(self) -> list[str]:
        """Stop and delete everything the world made; return what could not be
        cleaned up (never raises)."""
        problems: list[str] = []
        for h in reversed(self._helpers):
            try:
                h.close()
            except Exception as e:  # noqa: BLE001 - reported with the rest
                problems.append(f"closing {type(h).__name__}: {e}")
        if self.admin is not None and self.server.proc is not None and self.server.proc.poll() is None:
            try:
                problems += self._delete_sheds()
            except Exception as e:  # noqa: BLE001
                problems.append(f"deleting sheds: {e}")
        try:
            problems += self.server.stop()
        except Exception as e:  # noqa: BLE001
            problems.append(f"stopping shed-server: {e}")
        try:
            if not self.ref.stop():
                problems.append("the reference server did not stop on SIGTERM and was killed")
        except Exception as e:  # noqa: BLE001
            problems.append(f"stopping the reference server: {e}")
        # Whatever still runs from the world's folder, whoever started it.
        try:
            problems += stop_survivors(owned_processes(f"{self.root}/"), "started in the world")
        except Exception as e:  # noqa: BLE001
            problems.append(f"looking for processes left running: {e}")
        return problems

    def _delete_sheds(self) -> list[str]:
        r = self.admin.shed("--json", "-s", ENTRY, "list", timeout=60)
        if r.returncode != 0:
            return [f"listing sheds to delete them: {r.stderr.strip()[-300:]!r}"]
        problems = []
        for s in json.loads(r.stdout or "[]"):
            d = self.admin.cli("delete", s["name"], "-f", timeout=120)
            if d.returncode != 0:
                problems.append(f"shed {s['name']!r} could not be deleted: {d.stderr.strip()[-300:]!r}")
        return problems

    def print_logs(self) -> None:
        for p in sorted(self.logs.glob("*")):
            print(f"----- {p.name} (last lines) -----\n{_tail(p)}")


@contextmanager
def directory_world() -> Iterator[World]:
    """A fresh world, removed when the block ends, whatever happened in it.
    A clean-up that failed fails the case (or is noted on its own failure)."""
    bin_dir = Path(os.environ["SHED_BIN_DIR"]).resolve()
    missing = missing_binaries(bin_dir)
    if missing:
        raise AssertionError(f"missing {', '.join(map(str, missing))}: make test-integration-directory builds them")
    ssh, false = shutil.which("ssh"), shutil.which("false")
    if not ssh or not false:
        raise AssertionError("ssh and false must be on PATH")
    root = Path(tempfile.mkdtemp(prefix="shed-dir-", dir=socket_base(tempfile.gettempdir())))
    world: Optional[World] = None
    err: Optional[BaseException] = None
    problems: list[str] = []
    try:
        root.chmod(0o700)
        world = World(root, bin_dir, ssh, false)
        world.start()
        yield world
    except BaseException as e:
        err = e
        raise
    finally:
        if world is not None:
            problems = world.close()
            world.print_logs()
        shutil.rmtree(root, ignore_errors=True)
        if root.exists():
            problems.append(f"{root} could not be removed")
        if problems:
            msg = "clean-up:\n" + "\n".join(problems)
            if err is not None:
                err.add_note(msg)
            else:
                raise AssertionError(msg)
