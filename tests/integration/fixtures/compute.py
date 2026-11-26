"""Cloud sheds from the account service (plan 041 §6): the helpers of
`test_remote_compute.py`.

The cases run against one of two servers:

  - `[remote]`: the remote dev server (`make test-integration-remote`,
    `SHED_REMOTE_SERVER`), enrolled with an account service whose kind
    `SHED_REMOTE_SERVICE` names: `ref`, the in-tree reference service
    (`make dev-compute-ref-up`, its control directory under
    `SHED_COMPUTE_REF_DIR`), or `service`, a local account service, steered
    (when at all) through the hook `SHED_COMPUTE_CONTROL` names;
  - `[hermetic]`: a private server and reference service of the case's own
    (`fixtures/directory.py`'s `compute_world`, `make
    test-integration-compute`), on loopback, with the local link.

What the service is asked to do outside the contract goes through a
`ServiceControl`:

  - `RefControl`, the reference service's control directory: set-up steps
    appended to `compute.json` (the compute contract's named additions:
    `policy.set`, `quota.fill`, `clock.advance`, `client.register`,
    `compute.allow`), each applied once, by its id, at the service's next
    tick (every second) or compute call; and, where the run reaches the
    service through a `ServiceGate`, stopping and starting it (the gate
    refuses connections while down; the service keeps its state). An
    `outage` file would not do: the service answers 503, which leaves a
    create's outcome unknown rather than refused, and its user list fails
    too.
  - `HookControl`, any other service: the executable `SHED_COMPUTE_CONTROL`
    names, run with one verb and its arguments:

        <hook> policy KEY=VALUE...   set the organization's compute policy
                                     (keys: initial_lease_s, max_age_s,
                                     client_max_lease_s, person_max, org_max,
                                     warn_s; values in seconds or counts)
        <hook> policy-reset          put back the policy the run started with
        <hook> stop                  make the service unreachable (stop it)
        <hook> start                 bring it back, its state kept
        <hook> machines              print the shed id of every machine the
                                     service holds for this server, one a
                                     line (one being deleted counts until
                                     it is gone)

    and exits 0 when done. A verb a case needs that no control offers skips
    the case with the reason (`Unsupported`).

`run_with_tty` runs a command with any of its standard streams on a new
pseudo-terminal (the banner counts need a stderr that is a terminal, and
an interactive session needs stdin and stdout on one).
"""

from __future__ import annotations

import errno
import json
import os
import re
import selectors
import shlex
import socket
import subprocess
import threading
import time
import uuid
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Callable, Mapping, Optional, Sequence

REPO_ROOT = Path(__file__).resolve().parents[3]
# The reference service's world: the policy it starts with.
WORLD_JSON = REPO_ROOT / "tests" / "compute-contract" / "v1" / "fixtures" / "world.json"

SERVICE_KINDS = ("ref", "service")
# The policy keys a case may set (the reference service's policy.set and the
# hook's `policy` verb take the same names).
POLICY_KEYS = ("initial_lease_s", "max_age_s", "client_max_lease_s", "person_max", "org_max", "warn_s")
HOOK_VERBS = ("policy", "policy-reset", "stop", "start", "machines")

# The words the cases match on (test_fixture_units checks each is still in
# the code that writes it).
# internal/config/cloud.go LeaseBanner
BANNER = "shed: this cloud shed is deleted at "
# internal/remote/sweep.go: a record dropped for a deletion the service
# reported, with its cause.
DELETED_BY_SERVICE = "the account service deleted its machine (cause {cause}); dropping its record"
# cmd/shed/cloud.go apiErrorText: "CODE (reason): words".
REFUSAL = "{code} ({reason}):"


class Unsupported(Exception):
    """A control step this run's service cannot be asked for: the case skips
    with the message."""


# ----------------------------------------------------------------------------
# Pure helpers (unit-tested in test_fixture_units.py)
# ----------------------------------------------------------------------------


def service_kind(environ: Mapping[str, str] = os.environ) -> tuple[Optional[str], str]:
    """The `[remote]` run's service kind from SHED_REMOTE_SERVICE: (kind,
    "") or (None, why the cases cannot run)."""
    kind = environ.get("SHED_REMOTE_SERVICE", "").strip()
    if not kind:
        return None, ("SHED_REMOTE_SERVICE is not set: set it to ref (the reference service, "
                      "make dev-compute-ref-up) or service (a local account service) for the compute cases")
    if kind not in SERVICE_KINDS:
        return None, f"SHED_REMOTE_SERVICE={kind!r}: expected one of {', '.join(SERVICE_KINDS)}"
    return kind, ""


def parse_time(s: str) -> datetime:
    """An RFC 3339 time from the API (Go's encoding, `Z` or an offset, with
    or without fractional seconds), as an aware datetime."""
    s = s.strip()
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    m = re.match(r"^(.*T\d\d:\d\d:\d\d)(\.\d+)?([+-]\d\d:\d\d)$", s)
    if not m:
        raise ValueError(f"not an RFC 3339 time: {s!r}")
    frac = (m.group(2) or "")[:7]  # at most microseconds for fromisoformat
    frac = frac.ljust(7, "0") if frac else ""
    return datetime.fromisoformat(m.group(1) + frac + m.group(3)).astimezone(timezone.utc)


def lease_of(shed: Mapping) -> dict:
    """A shed record's cloud lease (fail-loud: every remote record carries
    `cloud`, and a running one its lease)."""
    cloud = shed.get("cloud")
    assert isinstance(cloud, dict), f"shed {shed.get('name')!r} carries no cloud object: {json.dumps(shed)[:600]}"
    lease = cloud.get("lease")
    assert isinstance(lease, dict), f"shed {shed.get('name')!r}'s cloud has no lease yet: {json.dumps(cloud)[:600]}"
    return lease


def expires_at(shed: Mapping) -> datetime:
    return parse_time(lease_of(shed)["expires_at"])


def banner_count(text: str) -> int:
    """How many lease banners `text` holds."""
    return text.count(BANNER)


def refusal_text(code: str, reason: str) -> str:
    """How the CLI words a refusal with a reason (`CODE (reason):`)."""
    return REFUSAL.format(code=code, reason=reason)


def policy_args(values: Mapping[str, int]) -> list[str]:
    """The hook's `policy` arguments (`KEY=VALUE`, in POLICY_KEYS order),
    refusing a key the hook does not take or a value that is not a whole
    number."""
    unknown = set(values) - set(POLICY_KEYS)
    if unknown:
        raise ValueError(f"policy keys {sorted(unknown)} are not among {', '.join(POLICY_KEYS)}")
    out = []
    for k in POLICY_KEYS:
        if k in values:
            v = values[k]
            if isinstance(v, bool) or not isinstance(v, int) or v < 0:
                raise ValueError(f"policy {k}={v!r}: a whole number of seconds or machines")
            out.append(f"{k}={v}")
    return out


def world_policy(path: Path = WORLD_JSON) -> dict:
    """The reference service's policy as it starts (the contract's world)."""
    return json.loads(path.read_text(encoding="utf-8"))["policy"]


def with_steps(document: Optional[Mapping], steps: Sequence[tuple[str, object]], prefix: str) -> dict:
    """`compute.json` with `steps` appended, each under a new id starting
    with `prefix`; every other member (forced replies, auto_settle_s,
    host_key, access, earlier steps) kept as it is."""
    out = json.loads(json.dumps(dict(document or {})))
    have = out.get("steps")
    if have is None:
        have = []
    if not isinstance(have, list):
        raise ValueError("compute.json: steps is not a list")
    for n, (name, value) in enumerate(steps):
        have.append({"id": f"{prefix}-{n}", "step": {name: value}})
    out["steps"] = have
    return out


def client_policy(config: Mapping, client: str) -> dict:
    """The service client `client`'s policy from a server config: its name
    prefix, max_age in seconds (0 when none), images and network."""
    clients = ((config.get("auth") or {}).get("clients") or {})
    if client not in clients:
        raise KeyError(f"the server config has no service client {client!r} (auth.clients)")
    pol = (clients[client] or {}).get("policy") or {}
    return {
        "name_prefix": pol.get("name_prefix") or "",
        "max_age_s": duration_seconds(pol.get("max_age") or "0s"),
        "images": list(pol.get("images") or []),
        "network": pol.get("network") or "",
        "max_sheds": int(pol.get("max_sheds") or 0),
    }


_DURATION = re.compile(r"(\d+)(h|m|s)")


def duration_seconds(text: str) -> int:
    """A Go duration of hours, minutes and seconds (`30h`, `1h30m`, `90s`)
    in seconds."""
    text = str(text).strip()
    if text in ("0", "0s"):
        return 0
    parts = _DURATION.findall(text)
    if not parts or "".join(n + u for n, u in parts) != text:
        raise ValueError(f"not a duration of h, m and s: {text!r}")
    return sum(int(n) * {"h": 3600, "m": 60, "s": 1}[u] for n, u in parts)


def binary_script() -> str:
    """A bash script that writes every byte from 0 to 255, once, in order,
    to stdout (and nothing else): the binary-stdout check."""
    return 'for i in $(seq 0 255); do printf "\\\\$(printf %03o "$i")"; done'


# ----------------------------------------------------------------------------
# A command with streams on a pseudo-terminal
# ----------------------------------------------------------------------------


@dataclass
class TtyRun:
    """One `run_with_tty`: the exit status (None when it had to be killed),
    what the terminal showed (every stream on it, merged), and the streams
    that were pipes."""

    code: Optional[int]
    tty: bytes
    stdout: bytes
    stderr: bytes

    def text(self) -> str:
        return (self.tty + self.stdout + self.stderr).decode("utf-8", errors="replace")

    def __str__(self) -> str:
        return (f"exit {self.code}; tty={self.tty[-1500:]!r}; stdout={self.stdout[-500:]!r}; "
                f"stderr={self.stderr[-1500:]!r}")


STREAMS = ("stdin", "stdout", "stderr")


def run_with_tty(argv: Sequence[str], env: Mapping[str, str], *, on_tty: Sequence[str] = STREAMS,
                 input: bytes = b"", timeout: float = 90, resend_every: float = 0) -> TtyRun:
    """Run `argv` with the streams named in `on_tty` on one new
    pseudo-terminal (80x24) and the others on pipes (stdin from /dev/null).
    `input` is typed on the terminal once it has shown something, or after
    two seconds, and again every `resend_every` seconds while the command
    runs (0: once); it needs stdin on the terminal. Returns once the
    command has exited and the terminal and pipes are drained, or kills it
    at `timeout`.

    The command runs in a session of its own with no controlling terminal:
    every check here reads isatty() on a stream, which the pseudo-terminal
    answers."""
    import fcntl
    import pty
    import struct
    import termios

    bad = set(on_tty) - set(STREAMS)
    if bad:
        raise ValueError(f"on_tty names {sorted(bad)}, not among {STREAMS}")
    if input and "stdin" not in on_tty:
        raise ValueError("input is typed on the terminal: put stdin on it")
    master = slave = -1
    if on_tty:
        master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 80, 0, 0))
    try:
        proc = subprocess.Popen(
            list(argv),
            stdin=slave if "stdin" in on_tty else subprocess.DEVNULL,
            stdout=slave if "stdout" in on_tty else subprocess.PIPE,
            stderr=slave if "stderr" in on_tty else subprocess.PIPE,
            env=dict(env), start_new_session=True, close_fds=True,
        )
    finally:
        if slave >= 0:
            os.close(slave)
    got = {"tty": b"", "stdout": b"", "stderr": b""}
    sel = selectors.DefaultSelector()
    if master >= 0:
        sel.register(master, selectors.EVENT_READ, "tty")
    if proc.stdout is not None:
        sel.register(proc.stdout, selectors.EVENT_READ, "stdout")
    if proc.stderr is not None:
        sel.register(proc.stderr, selectors.EVENT_READ, "stderr")
    start = time.monotonic()
    deadline = start + timeout
    sent_at: Optional[float] = None
    exited_at: Optional[float] = None
    killed = False
    try:
        while sel.get_map():
            now = time.monotonic()
            if now >= deadline:
                killed = proc.poll() is None
                break
            if proc.poll() is not None:
                exited_at = exited_at or now
                if now - exited_at > 3:
                    # Something the command left behind still holds a
                    # stream; what it wrote so far is read below.
                    break
            elif input and master >= 0:
                due = sent_at is None and (got["tty"] or now - start >= 2)
                again = sent_at is not None and resend_every > 0 and now - sent_at >= resend_every
                if due or again:
                    try:
                        os.write(master, input)
                    except OSError:
                        pass
                    sent_at = now
            for key, _ in sel.select(timeout=0.2):
                fd = key.fileobj if isinstance(key.fileobj, int) else key.fileobj.fileno()
                try:
                    chunk = os.read(fd, 65536)
                except OSError as e:
                    # A pseudo-terminal's master reads EIO once every slave
                    # is closed: the end of its stream.
                    if e.errno != errno.EIO:
                        raise
                    chunk = b""
                if chunk:
                    got[key.data] += chunk
                else:
                    sel.unregister(key.fileobj)
    finally:
        sel.close()
        # The whole session: the command and anything it left running.
        try:
            os.killpg(proc.pid, 9)
        except OSError:
            pass
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(timeout=10)
        for name, f in (("stdout", proc.stdout), ("stderr", proc.stderr)):
            if f is not None:
                os.set_blocking(f.fileno(), False)
                try:
                    got[name] += f.read() or b""
                except OSError:
                    pass
                f.close()
        if master >= 0:
            os.set_blocking(master, False)
            try:
                while True:
                    chunk = os.read(master, 65536)
                    if not chunk:
                        break
                    got["tty"] += chunk
            except OSError:
                pass
            os.close(master)
    return TtyRun(None if killed else proc.returncode, got["tty"], got["stdout"], got["stderr"])


# ----------------------------------------------------------------------------
# A gate in front of the service
# ----------------------------------------------------------------------------


class ServiceGate:
    """A TCP relay on 127.0.0.1:`port` in front of `target` (a port on
    127.0.0.1) that can go down and come back on the same port: while down
    nothing listens there, so a connection is refused, as to a service that
    is not running, and every connection it relayed is closed; the service
    behind it keeps its state. The compute world serves the reference
    service through one (its `--issuer` is the gate's address)."""

    def __init__(self, port: int, target: int, host: str = "127.0.0.1") -> None:
        self.port, self.target, self.host = port, target, host
        self._lock = threading.Lock()
        self._listener: Optional[socket.socket] = None
        self._socks: set[socket.socket] = set()
        self._threads: list[threading.Thread] = []
        self._acceptor: Optional[threading.Thread] = None

    @property
    def is_up(self) -> bool:
        with self._lock:
            return self._listener is not None

    def up(self) -> "ServiceGate":
        with self._lock:
            if self._listener is not None:
                return self
            ls = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            ls.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            ls.bind((self.host, self.port))
            ls.listen(64)
            ls.settimeout(0.2)
            self._listener = ls
        self._acceptor = self._spawn(self._serve, ls)
        return self

    def down(self) -> None:
        with self._lock:
            ls, self._listener = self._listener, None
            socks, self._socks = list(self._socks), set()
            acceptor, self._acceptor = self._acceptor, None
        if ls is not None:
            # A listening socket closed while a thread waits in accept stays
            # open until that wait ends: shut it down first, and wait for the
            # thread, so the port refuses from the moment down returns.
            try:
                ls.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            ls.close()
        if acceptor is not None:
            acceptor.join(5)
        for s in socks:
            try:
                s.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            s.close()

    def close(self, timeout: float = 5) -> None:
        self.down()
        with self._lock:
            threads = list(self._threads)
        for t in threads:
            t.join(timeout)

    def _spawn(self, fn, *args) -> threading.Thread:
        t = threading.Thread(target=fn, args=args, daemon=True)
        with self._lock:
            self._threads = [x for x in self._threads if x.is_alive()] + [t]
        t.start()
        return t

    def _keep(self, s: socket.socket, ls: socket.socket) -> bool:
        with self._lock:
            if self._listener is ls:
                self._socks.add(s)
                return True
        s.close()
        return False

    def _serve(self, ls: socket.socket) -> None:
        while True:
            try:
                conn, _ = ls.accept()
            except socket.timeout:
                if not self._is(ls):
                    return
                continue
            except OSError:
                return
            conn.settimeout(None)
            if self._keep(conn, ls):
                self._spawn(self._pair, conn, ls)

    def _is(self, ls: socket.socket) -> bool:
        with self._lock:
            return self._listener is ls

    def _pair(self, conn: socket.socket, ls: socket.socket) -> None:
        try:
            up = socket.create_connection((self.host, self.target), timeout=10)
            up.settimeout(None)
        except OSError:
            conn.close()
            return
        if not self._keep(up, ls):
            conn.close()
            return
        back = threading.Thread(target=_pump, args=(up, conn), daemon=True)
        back.start()
        _pump(conn, up)
        back.join()
        for x in (conn, up):
            x.close()


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


# ----------------------------------------------------------------------------
# Steering the service
# ----------------------------------------------------------------------------


def _write_private(path: Path, data: bytes) -> None:
    """Replace `path` with `data` atomically, mode 0600 (the reference
    service refuses a control file writable by group or others, and must
    never read half a file)."""
    tmp = path.with_name(f".{path.name}.{uuid.uuid4().hex[:8]}.tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        os.fchmod(fd, 0o600)
        os.write(fd, data)
        os.fsync(fd)
    finally:
        os.close(fd)
    os.replace(tmp, path)


class ServiceControl:
    """What a case may ask of the service outside the contract. Every method
    raises `Unsupported` unless the control offers it.

    `stopped` is set before a stop is attempted and cleared once a start
    succeeds, so whoever cleans up (the case's `finally`, then its fixture)
    knows to start the service again whatever happened in between."""

    kind = ""
    stopped = False

    def restore_limits(self, person_max: int, org_max: int) -> None:
        """Put the limits back after a case raised them: to the values read
        before (the reference service), or the run's starting policy (a
        hook's `policy-reset`)."""
        raise Unsupported(f"this run cannot set the {self.kind} service's policy")

    def machines(self) -> set[str]:
        """The shed ids of the machines the service holds for this server,
        read from the service itself (not the shed server's records)."""
        raise Unsupported(f"this run cannot read the {self.kind} service's machines")

    def policy(self, **values: int) -> None:
        raise Unsupported(f"this run cannot set the {self.kind} service's policy")

    def policy_reset(self, *keys: str) -> None:
        raise Unsupported(f"this run cannot set the {self.kind} service's policy")

    def quota_fill_person(self, handle: str) -> None:
        raise Unsupported("quota.fill is the reference service's")

    def quota_fill_org(self) -> None:
        raise Unsupported("quota.fill is the reference service's")

    def clock_advance(self, seconds: int) -> None:
        raise Unsupported("clock.advance is the reference service's")

    def check_stop(self) -> None:
        """Raise `Unsupported` unless `stop` and `start` can be asked (before
        a case makes anything)."""
        self.stop_reason()

    def stop_reason(self) -> None:
        raise Unsupported(f"this run cannot stop the {self.kind} service")

    def stop(self) -> None:
        self.stop_reason()

    def start(self) -> None:
        raise Unsupported(f"this run cannot start the {self.kind} service")


class RefControl(ServiceControl):
    """The reference service's control directory (`control`): steps go into
    `compute.json`; `gate`, when given, is the relay the server reaches it
    through. `server` is the name the shed server is enrolled under. `log` reads what the service has
    logged (its standard error), for the steps it could not apply."""

    kind = "ref"
    # The service applies compute.json at its next tick (every second) or
    # compute call: a step is in force after this long.
    APPLY_WAIT = 2.5

    def __init__(self, control: Path, server: str, log: Optional[Callable[[], str]] = None,
                 gate: Optional[ServiceGate] = None, inventory: Optional[Path] = None) -> None:
        self.control, self.server = Path(control), server
        self._log = log or (lambda: "")
        # The file the service rewrites every second with its machines
        # (shed-directory-ref --inventory).
        self.inventory = Path(inventory) if inventory else None
        # The relay the server reaches the service through, when the run
        # has one: `stop` and `start` take it down and up. Without one the
        # service cannot be stopped here (its state lives in its process).
        self.gate = gate

    @property
    def compute_json(self) -> Path:
        return self.control / "compute.json"

    def read(self) -> dict:
        try:
            return json.loads(self.compute_json.read_text(encoding="utf-8"))
        except FileNotFoundError:
            return {}

    def steps(self, *steps: tuple[str, object], wait: bool = True) -> None:
        """Append `steps` to compute.json and, with `wait`, wait for the
        service to apply them; fails when it logged a step it could not
        apply."""
        before = self._log()
        doc = with_steps(self.read(), steps, f"itest-{uuid.uuid4().hex[:12]}")
        _write_private(self.compute_json, json.dumps(doc, indent=1).encode())
        if wait:
            time.sleep(self.APPLY_WAIT)
            new = self._log()[len(before):]
            bad = [line for line in new.splitlines() if "compute.json" in line]
            assert not bad, f"the reference service could not apply {steps}: {bad}"

    def settings(self, **members: object) -> None:
        """Set compute.json's top-level members (auto_settle_s, host_key,
        access) beside its steps."""
        doc = self.read()
        doc.update(members)
        doc.setdefault("steps", [])
        _write_private(self.compute_json, json.dumps(doc, indent=1).encode())

    def policy(self, **values: int) -> None:
        policy_args(values)  # the same keys and values the hook takes
        self.steps(("policy.set", dict(values)))

    def policy_reset(self, *keys: str) -> None:
        start = world_policy()
        self.steps(("policy.set", {k: start[k] for k in (keys or POLICY_KEYS)}))

    def quota_fill_person(self, handle: str) -> None:
        self.steps(("quota.fill", ["person", handle]))

    def quota_fill_org(self) -> None:
        self.steps(("quota.fill", ["org"]))

    def clock_advance(self, seconds: int) -> None:
        self.steps(("clock.advance", int(seconds)))

    def allow(self) -> None:
        self.steps(("compute.allow", self.server))

    def register_client(self, client: str) -> None:
        self.steps(("client.register", [self.server, client]))

    def stop_reason(self) -> None:
        if self.gate is None:
            raise Unsupported(
                "the reference service keeps its state (enrolment, machines) in its process, so stopping it "
                "forgets the server; the service-down case runs hermetically (make test-integration-compute), "
                "or against a local account service (SHED_REMOTE_SERVICE=service with SHED_COMPUTE_CONTROL)")

    def stop(self) -> None:
        self.stop_reason()
        self.stopped = True
        self.gate.down()

    def start(self) -> None:
        self.stop_reason()
        self.gate.up()
        self.stopped = False

    def restore_limits(self, person_max: int, org_max: int) -> None:
        self.policy(person_max=person_max, org_max=org_max)

    def machines(self) -> set[str]:
        if self.inventory is None or not self.inventory.is_file():
            raise Unsupported(f"the reference service writes no inventory ({self.inventory}): start it with "
                              f"--inventory (make dev-compute-ref-down; make dev-compute-ref-up)")
        doc = json.loads(self.inventory.read_text(encoding="utf-8"))
        return set((doc.get("servers") or {}).get(self.server) or [])


class HookControl(ServiceControl):
    """A local account service steered by the executable `argv` (from
    SHED_COMPUTE_CONTROL): `<argv> <verb> <args>`."""

    kind = "service"

    def __init__(self, argv: Sequence[str], timeout: float = 300) -> None:
        self.argv, self.timeout = list(argv), timeout

    @classmethod
    def from_env(cls, environ: Mapping[str, str] = os.environ) -> Optional["HookControl"]:
        raw = environ.get("SHED_COMPUTE_CONTROL", "").strip()
        return cls(shlex.split(raw)) if raw else None

    def run(self, verb: str, *args: str) -> str:
        if verb not in HOOK_VERBS:
            raise ValueError(f"{verb!r} is not a hook verb ({', '.join(HOOK_VERBS)})")
        r = subprocess.run([*self.argv, verb, *args], capture_output=True, text=True, timeout=self.timeout,
                           stdin=subprocess.DEVNULL)
        if r.returncode != 0:
            raise AssertionError(f"SHED_COMPUTE_CONTROL {verb} {' '.join(args)}: exit {r.returncode}: "
                                 f"{(r.stdout + r.stderr).strip()[-800:]!r}")
        return r.stdout

    def machines(self) -> set[str]:
        return {line.strip() for line in self.run("machines").splitlines() if line.strip()}

    def policy(self, **values: int) -> None:
        self.run("policy", *policy_args(values))

    def policy_reset(self, *keys: str) -> None:
        self.run("policy-reset")

    def stop_reason(self) -> None:
        return None

    def stop(self) -> None:
        self.stopped = True
        self.run("stop")

    def start(self) -> None:
        self.run("start")
        self.stopped = False

    def restore_limits(self, person_max: int, org_max: int) -> None:
        self.run("policy-reset")


class NoControl(ServiceControl):
    """A local account service with no hook: nothing can be asked of it."""

    kind = "service"

    def _why(self, what: str) -> Unsupported:
        return Unsupported(f"{what}: set SHED_COMPUTE_CONTROL to a hook for the local account service "
                           f"(verbs: {', '.join(HOOK_VERBS)}; see fixtures/compute.py)")

    def policy(self, **values: int) -> None:
        raise self._why("the case sets the service's policy")

    def policy_reset(self, *keys: str) -> None:
        raise self._why("the case sets the service's policy")

    def stop_reason(self) -> None:
        raise self._why("the case stops the service")

    def machines(self) -> set[str]:
        raise self._why("the clean-up confirms the machines are gone by their ids")

    def stop(self) -> None:
        raise self._why("the case stops the service")

    def start(self) -> None:
        raise self._why("the case starts the service")


# ----------------------------------------------------------------------------
# The service's people, and a person driving the shared cases
# ----------------------------------------------------------------------------

# /api/info's token for a server whose remote sheds are the account service's
# machines (internal/config/cloud.go FeatureLease).
FEATURE_LEASE = "lease"


def list_itest_people(users_json: Path, keys_dir: Optional[Path] = None) -> None:
    """Put the itest people (`fixtures.identity.directory_users`) in a
    reference service's users.json, keeping everyone else it lists."""
    from .identity import directory_users, ensure_keys

    try:
        have = json.loads(users_json.read_text(encoding="utf-8"))
    except FileNotFoundError:
        have = []
    ours = directory_users(ensure_keys(keys_dir))
    names = {u["name"] for u in ours}
    merged = [u for u in have if u.get("name") not in names] + ours
    data = json.dumps(sorted(merged, key=lambda u: u["name"]), indent=1).encode() + b"\n"
    _write_private(users_json, data)


def unlisted_people(server, names: Sequence[str] = ()) -> list[str]:
    """Which of `names` (default itest-alice and itest-bob) the server's users
    list lacks, after a refresh (`server` a local admin's view)."""
    from .identity import ALICE, BOB

    names = list(names) or [ALICE, BOB]
    server.cli("users", "refresh", timeout=120)
    r = server.cli("--json", "users", "status", timeout=60)
    if r.returncode != 0:
        return names
    have = {u.get("name") for u in (json.loads(r.stdout).get("users") or [])}
    return [n for n in names if n not in have]


def person_identity(user: str, entry: str, host: str, ssh_port: int, keys_dir: Optional[Path] = None,
                    suffix: str = "shared"):
    """An itest person (`fixtures.identity.Identity`) with a HOME of its own
    (`<keys>/<user>/home-<entry>-<suffix>`, made fresh), apart from the one
    `users_environment` makes for the same person, so both can live in one
    run. Not enrolled: call `add_server()`."""
    import shutil

    from .identity import Identity, ensure_keys, key_path

    base = ensure_keys(keys_dir)
    key = key_path(user, keys_dir=base)
    home = key.parent / f"home-{entry}-{suffix}"
    if home.exists():
        shutil.rmtree(home)
    home.mkdir(mode=0o700)
    ident = Identity(user=user, key=key, home=home, entry=entry, host=host, ssh_port=int(ssh_port),
                     shim=home / "bin" / "ssh")
    ident._write_shim()
    return ident
