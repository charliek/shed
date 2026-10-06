"""The **craze source and lane** in the Tauri app (plan 025 §3.6, C9) —
`--target tauri`.

**Hermetic, against the REAL craze hub.** Every craze process here is one of
the pinned binaries (`make craze-binaries`, CI's `craze-binaries` action):
`craze` itself, `craze-fake-host` (a registry entry whose stdin ops raise asks
and stream text), `craze-fake-agent` (what a created session runs as `grok`),
and `craze-0.0.1` (the real v0.0.1, for the too-old cell). They come from
`SHED_CRAZE_BIN_DIR`; without it the module SKIPS — and under
`SHED_CRAZE_REQUIRE=1` (CI) it FAILS instead, so a run can never go green on
skipped craze cells.

Three independent app instances (`test_tauri_lane.py`'s fixture shape — each
its own throwaway HOME/XDG_RUNTIME_DIR, so its own socket and single-instance
lock, the session app untouched):

* `app` — THIS machine's craze, through the test-mode `SHED_TAURI_CRAZE_PATH`
  seam: a directory holding a RECORDING `craze` wrapper (it notes the `PATH`
  and argv it was run with, then execs a private copy of the real binary), so
  the jailed ladder is observable. `localhost`'s roost is a `FakeRoost` (for
  the row merge, and Open in terminal's `tab.open`). The hub this app's eager
  source births is under the app's own HOME / `CRAZE_HOME` / a short `/tmp`
  `CRAZE_RUNTIME_DIR`. Two switches in the rig's root steer the wrapper for
  the create sheet's cells (C10): `outage` makes every craze run fail at once
  (exit 1, before any hub — an unreachable machine), and `proxy` runs each NEW
  `bridge --hub` through `bridge_proxy.py`, which logs every `session.create`'s
  requestId to `creates.log` and — while `drop-creates` exists — cuts the
  connection once the hub has the create, so its answer is lost. The settings
  cells (C11) use the same proxy for `session.set`: every one is logged to
  `sets.log` (its params, so `forModel` and the commandId are on record), and
  while `drop-sets` exists the connection that carried it is cut the same way.
* `app_absent` — an EMPTY seam directory (craze not installed) and a
  host-installed-craze SENTINEL at `$HOME/.nix-profile/bin/craze` (an absolute
  rung the production ladder would reach and the jailed one never may).
* `app_remote` — no local craze; three remote machines reached through a fake
  `ssh` that runs each remote command in a per-machine jail and RECORDS every
  command — and refuses (and records) any command naming an absolute ladder
  rung, so the jailed composition is asserted without ever running one.

**Every craze process is torn down** at module end by its program path (each
rig's private copies, never this host's own craze), checked just before each
signal — every rig root is RESOLVED (`/tmp` is a symlink on macOS, and craze
runs itself again by its resolved path, so an unresolved prefix would miss
the hub); `/usr/local/bin/craze`, `~/.craze` and `~/.cache/craze` are never
touched — no rig's `PATH`, `HOME` or `CRAZE_HOME` reaches them.
"""

from __future__ import annotations

import json
import os
import platform
import shutil
import signal
import subprocess
import sys
import tempfile
import time
from pathlib import Path

import pytest

import ui
from client import ShedError, TauriClient, scaled_timeout
from fake_host_agent import FakeHostAgent
from fake_opencode import FakeOpencode
from fake_roost import FakeRoost

pytestmark = pytest.mark.skipif(
    os.environ.get("SHED_TEST_TARGET", "mac") != "tauri",
    reason="tauri-only: the mac app has no machine layer, and so no craze source",
)

FIXTURES = Path(__file__).resolve().parent / "fixtures"
PNG_MAGIC = b"\x89PNG\r\n\x1a\n"
#: Where the craze screenshots are KEPT when a runner asks (`SHED_CRAZE_SHOTS`);
#: every cell asserts the PNG it captured either way.
SHOTS = os.environ.get("SHED_CRAZE_SHOTS")

LOCAL = "localhost"
KIND = "craze"
#: The fake host's registry identity (twelve hex digits, as craze mints them).
FAKE_HOST = "0c0c0c0c0c0c"
FAKE_SESSION = "craze-desk-fake"
#: The roost tab a craze TUI would run in, for the row merge.
CRAZE_TAB = 7
#: Every craze wait is bounded by this, scaled like every other harness wait.
WAIT = 30.0
BIN_NAMES = ("craze", "craze-fake-host", "craze-fake-agent", "craze-0.0.1")

HELLO = {"protocols": [1], "client": {"kind": "test", "name": "shedtest"}}

#: The create-logging, create-dropping `bridge --hub` proxy the recording
#: wrapper runs while the rig's `proxy` switch is on (C10). It relays NDJSON
#: lines both ways, unchanged; each client line that is a `session.create` is
#: logged (`creates.log`: its requestId, and whether it was dropped) — and while
#: `drop-creates` exists, relayed with nothing relayed back from then on, given
#: a second for the hub to take it, and then the connection is CUT: the hub has
#: the create (and a waiter that disconnects does not cancel one), the client
#: never sees its answer — an unknown outcome, exactly. A `session.set` (C11) is
#: the same: logged to `sets.log` with its params, and cut while `drop-sets`
#: exists — craze runs the change, and the lane never reads its answer.
BRIDGE_PROXY = r"""
import json, os, subprocess, sys, threading, time
root = os.path.dirname(os.path.abspath(__file__))
child = subprocess.Popen(sys.argv[1:], stdin=subprocess.PIPE, stdout=subprocess.PIPE)
cut = threading.Event()
def down():
    for line in child.stdout:
        if cut.is_set():
            continue
        sys.stdout.buffer.write(line)
        sys.stdout.buffer.flush()
    os._exit(0)
threading.Thread(target=down, daemon=True).start()
for line in sys.stdin.buffer:
    drop = False
    try:
        msg = json.loads(line)
    except ValueError:
        msg = None
    if isinstance(msg, dict) and msg.get("method") == "session.create":
        drop = os.path.exists(os.path.join(root, "drop-creates"))
        rid = (msg.get("params") or {}).get("requestId")
        with open(os.path.join(root, "creates.log"), "a") as log:
            log.write(json.dumps({"requestId": rid, "dropped": drop}) + "\n")
    if isinstance(msg, dict) and msg.get("method") == "session.set":
        drop = os.path.exists(os.path.join(root, "drop-sets"))
        with open(os.path.join(root, "sets.log"), "a") as log:
            log.write(json.dumps({"params": msg.get("params"), "dropped": drop}) + "\n")
    if drop:
        cut.set()
    child.stdin.write(line)
    child.stdin.flush()
    if drop:
        time.sleep(1.0)
        child.kill()
        os._exit(0)
try:
    child.stdin.close()
except OSError:
    pass
child.wait()
os._exit(0)
"""


# ---------------------------------------------------------------------------
# the binaries, and the skip-or-require rule
# ---------------------------------------------------------------------------


def _bins() -> Path:
    """`SHED_CRAZE_BIN_DIR`, every binary present — or a skip, which
    `SHED_CRAZE_REQUIRE=1` turns into a failure (plan 025 §3.6.7)."""
    raw = os.environ.get("SHED_CRAZE_BIN_DIR", "")
    missing: list[str] = []
    if not raw:
        why = "SHED_CRAZE_BIN_DIR is unset (make craze-binaries prints it)"
    else:
        missing = [n for n in BIN_NAMES if not os.access(Path(raw) / n, os.X_OK)]
        why = f"SHED_CRAZE_BIN_DIR={raw} lacks {', '.join(missing)}" if missing else ""
    if why:
        if os.environ.get("SHED_CRAZE_REQUIRE") == "1":
            pytest.fail(f"SHED_CRAZE_REQUIRE=1 and the craze binaries are missing: {why}",
                        pytrace=False)
        pytest.skip(f"craze cells skipped: {why}")
    return Path(raw)


@pytest.fixture(scope="module")
def bins() -> Path:
    return _bins()


# ---------------------------------------------------------------------------
# the rigs
# ---------------------------------------------------------------------------


def _mkdir_0700(p: Path) -> Path:
    p.mkdir(parents=True, exist_ok=True)
    p.chmod(0o700)
    return p


def _write_exec(path: Path, body: str) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(body)
    path.chmod(0o755)
    return path


def _program_args(pid: int) -> str:
    out = subprocess.run(["ps", "-ww", "-o", "args=", "-p", str(pid)],
                         capture_output=True, text=True, check=False)
    return out.stdout.strip()


def _processes_under(prefix: str) -> list[int]:
    """Pids whose PROGRAM (argv[0]) is under `prefix` — a rig's private copies."""
    out = subprocess.run(["ps", "-A", "-ww", "-o", "pid=,args="],
                         capture_output=True, text=True, check=False)
    pids = []
    for line in out.stdout.splitlines():
        pid_s, _, args = line.strip().partition(" ")
        if not pid_s.isdigit():
            continue
        pid = int(pid_s)
        if pid in (0, 1, os.getpid()):
            continue
        if args.strip().startswith(prefix):
            pids.append(pid)
    return pids


def _signal_under(prefix: str, sig: int) -> None:
    for pid in _processes_under(prefix):
        # Re-read just before the signal: a pid can be reused.
        if _program_args(pid).startswith(prefix):
            try:
                os.kill(pid, sig)
            except ProcessLookupError:
                pass


def _teardown_under(prefix: str) -> list[int]:
    """SIGTERM, a bounded wait, SIGKILL — everything run from `prefix`. Answers
    what was still there at the end (nothing, normally)."""
    _signal_under(prefix, signal.SIGTERM)
    deadline = time.monotonic() + 10
    while _processes_under(prefix) and time.monotonic() < deadline:
        time.sleep(0.1)
    _signal_under(prefix, signal.SIGKILL)
    time.sleep(0.2)
    return _processes_under(prefix)


class CrazeEnv:
    """One craze namespace: a HOME, a `CRAZE_HOME`, a short 0700 runtime dir
    under /tmp, a `PATH` directory, and the binaries COPIED into a private
    directory (`real/`) so every craze process of this namespace's — each
    bridge, the hub, each host it creates — runs a program under it, which is
    how teardown tells them from anything else.

    `kind` is what the PATH directory offers: `recipe` (a recording `craze`
    wrapper over the private copy), `empty` (craze not installed), `old` (the
    real v0.0.1 as `craze`)."""

    def __init__(self, bins: Path, root: Path, home: Path, kind: str = "recipe"):
        self.root = root
        self.home = _mkdir_0700(home)
        self.craze_home = _mkdir_0700(root / "ch")
        self.runtime = _mkdir_0700(root / "r")
        self.path_dir = _mkdir_0700(root / "bin")
        self.real = _mkdir_0700(root / "real")
        self.work = _mkdir_0700(root / "w")
        self.calls = root / "craze-calls.log"
        self.fake_hosts: list[subprocess.Popen] = []
        self.bridges: list[subprocess.Popen] = []
        shutil.copy(bins / "craze", self.real / "craze")
        shutil.copy(bins / "craze-fake-host", self.real / "craze-fake-host")
        shutil.copy(bins / "craze-0.0.1", self.real / "craze-0.0.1")
        agent = self.real / "craze-fake-agent"
        shutil.copy(bins / "craze-fake-agent", agent)
        for p in self.real.iterdir():
            p.chmod(0o755)
        self.grok_echo = _write_exec(
            root / "grok-echo-agent",
            f"#!/bin/sh\nexec '{agent}' -script grok-echo \"$@\"\n")
        (self.craze_home / "config.toml").write_text(
            'provider = "grok"\nhost_idle_exit = "10m"\n\n'
            f'[agents]\ngrok = "{self.grok_echo}"\n')
        if kind == "recipe":
            # The recording wrapper: what the ladder found, run with what PATH
            # — and the C10 switches (the class doc): an outage, and the
            # create-logging (and -dropping) bridge proxy.
            (self.root / "bridge_proxy.py").write_text(BRIDGE_PROXY)
            _write_exec(self.path_dir / "craze", (
                "#!/bin/sh\n"
                f"printf '%s\\t%s\\n' \"$PATH\" \"$*\" >> '{self.calls}'\n"
                f"if [ -e '{self.root / 'outage'}' ]; then\n"
                "  printf '%s\\n' 'craze: a simulated outage (shedtest)' >&2; exit 1\n"
                "fi\n"
                f"if [ \"$*\" = 'bridge --hub' ] && [ -e '{self.root / 'proxy'}' ]; then\n"
                f"  exec '{sys.executable}' '{self.root / 'bridge_proxy.py'}' '{self.real / 'craze'}' \"$@\"\n"
                "fi\n"
                f"exec '{self.real / 'craze'}' \"$@\"\n"))
        elif kind == "old":
            os.symlink(self.real / "craze-0.0.1", self.path_dir / "craze")

    def creates(self) -> list[dict]:
        """Every `session.create` a proxied bridge carried: `{requestId,
        dropped}`, in order."""
        log = self.root / "creates.log"
        if not log.exists():
            return []
        return [json.loads(line) for line in log.read_text().splitlines() if line.strip()]

    def set_grok(self, agent: Path) -> None:
        """Point `[agents].grok` at `agent` (craze reads it at every create)."""
        self.set_agents(grok=agent)

    def set_agents(self, **agents: Path) -> None:
        """Point each named `[agents].<provider>` at its agent, grok staying the
        default provider (craze reads it at every create). Naming `cursor` makes
        cursor READY here — so the C10 sheet cell, which asserts it is not,
        must never see it: restore with `set_grok` once the create is done."""
        lines = "".join(f'{p} = "{a}"\n' for p, a in agents.items())
        (self.craze_home / "config.toml").write_text(
            'provider = "grok"\nhost_idle_exit = "10m"\n\n'
            f'[agents]\n{lines}')

    def sets(self) -> list[dict]:
        """Every `session.set` a proxied bridge carried: `{params, dropped}`,
        in order."""
        log = self.root / "sets.log"
        if not log.exists():
            return []
        return [json.loads(line) for line in log.read_text().splitlines() if line.strip()]

    def switch(self, name: str, on: bool) -> None:
        """Turn one of the wrapper's switches (`outage`, `proxy`,
        `drop-creates`, `drop-sets`) on or off."""
        flag = self.root / name
        if on:
            flag.touch()
        else:
            flag.unlink(missing_ok=True)

    def env(self) -> dict[str, str]:
        """Exactly what a craze process of this namespace sees."""
        return {"HOME": str(self.home), "CRAZE_HOME": str(self.craze_home),
                "CRAZE_RUNTIME_DIR": str(self.runtime), "PATH": str(self.path_dir)}

    def recorded(self) -> list[tuple[str, str]]:
        """Every `(PATH, argv)` the recording wrapper was run with."""
        if not self.calls.exists():
            return []
        return [tuple(line.split("\t", 1)) for line in self.calls.read_text().splitlines()]

    def fake_host(self, host_id: str, session_id: str) -> subprocess.Popen:
        """A `craze-fake-host` listed in this namespace's registry, its stdin the
        ops channel; returns once its ready line (registry entry written) is in."""
        proc = subprocess.Popen(
            [str(self.real / "craze-fake-host"), "--registry", str(self.home),
             "--host-id", host_id, "--session-id", session_id],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
            env=self.env(), cwd=str(self.root))
        self.fake_hosts.append(proc)
        line = proc.stdout.readline()
        assert line, "craze-fake-host exited before its ready line"
        return proc

    def op(self, proc: subprocess.Popen, op: dict) -> None:
        proc.stdin.write((json.dumps(op) + "\n").encode())
        proc.stdin.flush()

    def bridge(self) -> "Bridge":
        """A `craze bridge --hub` of this namespace's own (its hub, born by the
        first one), for the harness's own hub calls."""
        b = Bridge([str(self.real / "craze"), "bridge", "--hub"], self.env(), self.root)
        self.bridges.append(b.proc)
        return b

    def hub_pids(self) -> list[int]:
        hubs = self.home / ".cache" / "craze" / "hubs"
        pids = []
        for rec in hubs.glob("*.json"):
            try:
                pids.append(int(json.loads(rec.read_text())["pid"]))
            except (ValueError, KeyError, json.JSONDecodeError):
                pass
        return pids

    def sigterm_hub(self) -> None:
        """SIGTERM this namespace's hub, by the pid its record names, checked to
        run this namespace's private copy just before. A record whose pid no
        longer runs (a hub stopped earlier in the module) is skipped; at least
        one live hub must be signalled."""
        pids = self.hub_pids()
        assert pids, "no hub record"
        signalled = 0
        for pid in pids:
            args = _program_args(pid)
            if not args:
                continue
            assert args.startswith(str(self.real)), f"pid {pid} is not this rig's hub: {args!r}"
            os.kill(pid, signal.SIGTERM)
            signalled += 1
        assert signalled, f"no live hub among the records: {pids}"

    def teardown(self) -> list[int]:
        for proc in self.fake_hosts + self.bridges:
            try:
                proc.stdin.close()
            except (BrokenPipeError, OSError, AttributeError):
                pass
        for proc in self.fake_hosts + self.bridges:
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
        return _teardown_under(str(self.real))


class Bridge:
    """One NDJSON JSON-RPC connection to a hub through `craze bridge --hub` —
    the harness's own client, for what the app does not offer in C9 (a create)."""

    def __init__(self, argv: list[str], env: dict[str, str], cwd: Path):
        self.proc = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                     stderr=subprocess.DEVNULL, env=env, cwd=str(cwd))
        self._next = 0
        self.notes: list[dict] = []

    def call(self, method: str, params: dict, timeout: float = WAIT) -> dict:
        return self.wait(self.send(method, params), method, timeout)

    def send(self, method: str, params: dict) -> int:
        """Write one request and return its id, without waiting for the
        answer ([`Bridge.wait`] reads it)."""
        self._next += 1
        rid = self._next
        self.proc.stdin.write((json.dumps({"jsonrpc": "2.0", "id": rid, "method": method,
                                           "params": params}) + "\n").encode())
        self.proc.stdin.flush()
        return rid

    def wait(self, rid: int, method: str, timeout: float = WAIT) -> dict:
        """The answer to request `rid` (`method` names it in a failure)."""
        deadline = time.monotonic() + scaled_timeout(timeout)
        while time.monotonic() < deadline:
            line = self.proc.stdout.readline()
            if not line:
                raise AssertionError(f"{method}: the bridge closed before answering")
            msg = json.loads(line)
            if msg.get("id") == rid:
                assert "error" not in msg, f"{method} was refused: {msg}"
                return msg["result"]
            self.notes.append(msg)
        raise AssertionError(f"{method}: no answer within {timeout}s")

    def close(self) -> None:
        try:
            self.proc.stdin.close()
        except (BrokenPipeError, OSError):
            pass
        try:
            self.proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self.proc.kill()
            self.proc.wait()


def _launch(mock, *, runtime_dir: Path, craze: CrazeEnv | None, roost: FakeRoost | None,
            ssh_bin: Path | None = None):
    """An independent app instance (`test_tauri_lane.py`'s shape): its own
    HOME/XDG_RUNTIME_DIR, its own fake host-agent, the session app untouched."""
    cfg = ui._SUBPROC["tauri"]
    if not cfg.binary.exists():
        raise RuntimeError(f"tauri binary not found at {cfg.binary}; build it first (make tauri-build).")
    agent = FakeHostAgent()
    agent.start()
    sock = runtime_dir / cfg.sock_rel
    log = runtime_dir / "craze-ui.log"
    env = ui.subproc_env(
        cfg, runtime_dir=runtime_dir, mock_base_url=mock.base_url,
        config_path=FIXTURES / "config-craze.yaml", host_agent_socket=agent.socket_path,
        roost_sockets={LOCAL: roost.socket_path} if roost else None,
        ssh_bin=ssh_bin,
        craze_path=craze.path_dir if craze else None,
        craze_home=craze.craze_home if craze else None,
        craze_runtime_dir=craze.runtime if craze else None)
    log_fh = open(log, "wb")
    proc = subprocess.Popen([str(cfg.binary)], env=env, stdout=log_fh, stderr=subprocess.STDOUT)
    ui.await_hermetic("tauri", sock=sock, mock_base_url=mock.base_url, proc=proc, log=log)
    client = TauriClient(sock)
    client.wait_until(lambda: client.current_pane() is not None, timeout=30,
                      what="tauri frontend ready")
    return client, proc, log_fh, agent


def _stop(client, proc, log_fh, agent) -> None:
    if client is not None:
        client.close()
    ui.terminate(proc)
    log_fh.close()
    agent.stop()


@pytest.fixture(scope="module")
def rig(bins):
    """THIS machine's craze namespace — the recording seam over the real binary."""
    root = Path(tempfile.mkdtemp(prefix="shcz-", dir="/tmp")).resolve()
    env = CrazeEnv(bins, root, root / "h")
    try:
        yield env
    finally:
        left = env.teardown()
        shutil.rmtree(root, ignore_errors=True)
        assert not left, f"craze processes left behind: {left}"


@pytest.fixture(scope="module")
def roost():
    fake = FakeRoost().start()
    try:
        yield fake
    finally:
        fake.shutdown()


@pytest.fixture(scope="module")
def app(rig, roost, mock):
    """The main instance: this machine's craze through the seam, its roost a
    `FakeRoost`. A fake host is registered BEFORE launch, so the first roster
    seeds with it."""
    host = rig.fake_host(FAKE_HOST, FAKE_SESSION)
    started = _launch(mock, runtime_dir=rig.home, craze=rig, roost=roost)
    client = started[0]
    try:
        client.wait_until(lambda: _row(client, FAKE_HOST) is not None, timeout=WAIT,
                          what="the fake host's craze row")
        client.fake_host = host  # type: ignore[attr-defined]
        yield client
    finally:
        _stop(*started)


@pytest.fixture(scope="module")
def created(app, rig) -> dict:
    """A session created through the hub (the harness's own bridge: the create
    sheet is C10's) — a real `craze serve` running the fake agent as grok, its
    first prompt taken. Answers the created row as the app lists it."""
    b = rig.bridge()
    try:
        assert b.call("hello", HELLO)["endpoint"]["kind"] == "hub"
        res = b.call("session.create", {"cwd": str(rig.work), "prompt": "hello desk",
                                        "requestId": "shedtest-craze-1"}, timeout=90)
        assert res["prompt"] == "accepted", res
        host_id = res["session"]["hostId"]
    finally:
        b.close()
    app.wait_until(lambda: _row(app, host_id) is not None, timeout=WAIT,
                   what="the created session's craze row")
    return _row(app, host_id)


# ---------------------------------------------------------------------------
# reading
# ---------------------------------------------------------------------------


def _rows(app: TauriClient, machine: str = LOCAL) -> list[dict]:
    return [r for r in app.call("rc.list").get("sessions", []) if r.get("machine") == machine]


def _row(app: TauriClient, host_id: str, machine: str = LOCAL) -> dict | None:
    return next((r for r in _rows(app, machine)
                 if r.get("source") == "craze" and r.get("slug") == host_id), None)


def _status(app: TauriClient, name: str) -> dict | None:
    return next((m for m in app.call("machines.list")["machines"] if m["name"] == name), None)


def _lane(app: TauriClient, op: str, host_id: str, **extra) -> dict:
    return app.call(op, {"machine": LOCAL, "kind": KIND, "session_id": host_id, **extra})


def _messages(app: TauriClient, host_id: str) -> dict:
    return _lane(app, "lane.messages", host_id)


def _texts(app: TauriClient, host_id: str) -> list[str]:
    return [m.get("text") or "" for m in _messages(app, host_id)["messages"]]


def _ready(app: TauriClient, host_id: str) -> dict:
    _lane(app, "lane.open", host_id)
    app.wait_until(lambda: _messages(app, host_id)["generation"] >= 1, timeout=WAIT,
                   what=f"the seeded craze lane on {host_id}")
    return _messages(app, host_id)


def _dump(app: TauriClient) -> dict | None:
    return app.call("lane.dump")["lane"]


def _panel(app: TauriClient, host_id: str) -> dict:
    app.call("ui.show_lane", {"machine": LOCAL, "kind": KIND, "session_id": host_id})
    app.wait_until(lambda: bool((d := _dump(app)) and d["session_id"] == host_id
                                and (d["generation"] >= 1 or d["error"])),
                   timeout=WAIT, what="the transcript panel to mount and report")
    return _dump(app)


def _unmount(app: TauriClient) -> None:
    app.call("ui.close_lane")
    app.wait_until(lambda: _dump(app) is None, timeout=20, what="the panel to unmount")


def _shot(app: TauriClient, name: str) -> None:
    if platform.system() == "Darwin":
        return
    png, w, h = app.screenshot(scale=1)
    assert png[:8] == PNG_MAGIC and w > 0 and h > 0
    if SHOTS:
        out = Path(SHOTS).expanduser()
        out.mkdir(parents=True, exist_ok=True)
        (out / name).write_bytes(png)


def _error(fn) -> ShedError:
    with pytest.raises(ShedError) as caught:
        fn()
    return caught.value


# ---------------------------------------------------------------------------
# (1) rows — this machine's hub, eager
# ---------------------------------------------------------------------------


def test_rows_from_a_fake_host_and_a_created_session(app, created):
    """**This machine's hub rows are rows** (plan 025 §3.6.3): the fake host's
    and a session the hub created, each `source: "craze"`, keyed by its hostId
    (P11), with the roster's own facts, and an `agent_lane` that names the kind
    and the hostId. The machine's craze is `live`."""
    fake = _row(app, FAKE_HOST)
    assert fake["kind"] == KIND and fake["source"] == "craze", fake
    assert fake["origin"] == "machine:localhost" and fake["origin_kind"] == "machine"
    assert fake["agent_lane"] == {"kind": KIND, "session_id": FAKE_HOST}
    assert fake["stale"] is False
    assert created["slug"] == created["agent_lane"]["session_id"]
    assert created["provider"] == "grok", created
    assert created["workdir"].endswith("/w"), created
    status = _status(app, LOCAL)
    assert status["craze"]["state"] == "live", status
    assert status["craze"]["create"] is True

    app.navigate("agents")
    app.wait_until(lambda: any(s.get("slug") == created["slug"]
                               for s in app.call("agents.dump")["sessions"]),
                   timeout=20, what="the Agents pane to render the craze rows")
    rendered = {s["slug"]: s for s in app.call("agents.dump")["sessions"]}
    assert rendered[FAKE_HOST]["source"] == "craze"
    _shot(app, "craze-rows.png")


def test_the_local_dial_is_jailed(app, rig):
    """**The test-mode local dial is the JAILED ladder** (plan 025 §3.6.1):
    every craze the app's dials ran was the seam's, found by rung 2 and run with
    `PATH` exactly the seam directory — the production ladder prepends its
    exec PATH (`/opt/homebrew/bin`, `/usr/local/bin`, …), which is how a craze
    installed on the host could reach a hermetic run."""
    calls = rig.recorded()
    assert calls, "the app's craze dials ran the seam's craze"
    paths = {path for path, _ in calls}
    assert paths == {str(rig.path_dir)}, f"a dial ran with another PATH: {paths}"
    assert any(argv == "bridge --hub" for _, argv in calls), calls


# ---------------------------------------------------------------------------
# (2) the lane
# ---------------------------------------------------------------------------


def test_the_lane_seeds_sends_and_echoes(app, created):
    """A craze lane opens by hostId with `kind: "craze"` and seeds the created
    session's transcript (its first prompt and the fake agent's echo); a send is
    echoed; the panel renders it, badge and all."""
    host = created["slug"]
    view = _ready(app, host)
    caps = view["capabilities"]
    assert caps["kind"] == KIND and caps["stop"] is True, caps
    app.wait_until(lambda: "echo: hello desk" in _texts(app, host), timeout=WAIT,
                   what="the first prompt's echo")
    _lane(app, "lane.send", host, text="again please")
    app.wait_until(lambda: "echo: again please" in _texts(app, host), timeout=WAIT,
                   what="the send's echo")
    panel = _panel(app, host)
    app.wait_until(lambda: any(r["text"] == "echo: again please" for r in _dump(app)["rows"]),
                   timeout=WAIT, what="the panel to render the echo")
    panel = _dump(app)
    assert panel["kind"] == KIND and panel["lane_kind"] == KIND
    assert panel["stop"] == {"confirming": False}, "a hub-created session offers Stop"
    _shot(app, "craze-lane.png")
    _unmount(app)


def test_the_lane_key_carries_the_kind(app, created):
    """**The kind is half the lane's address** (plan 025 §3.6.4): the same
    hostId under another kind is no lane at all — never the craze one."""
    host = created["slug"]
    refused = _error(lambda: app.call("lane.open", {"machine": LOCAL, "kind": "opencode",
                                                    "session_id": host}))
    assert refused.code == "no_lane", refused
    missing = _error(lambda: app.call("lane.open", {"machine": LOCAL, "session_id": host}))
    assert missing.code == "bad_request" and "kind" in missing.message, missing


def test_asks_are_answered_through_the_lane(app, rig):
    """A permission, a question and a plan raised by the fake host reach the
    panel as cards and are answered through `lane.answer` — by option id, by
    label, and by the plan's synthesized `accept`."""
    host = FAKE_HOST
    _ready(app, host)
    _panel(app, host)
    rig.op(app.fake_host, {"name": "permission", "id": "perm-1", "tool": "Shell",
                           "options": [{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
                                       {"optionId": "deny", "name": "Deny", "kind": "reject_once"}]})
    app.wait_until(lambda: any(c["id"] == "perm-1" for c in _dump(app)["approvals"]),
                   timeout=WAIT, what="the permission card")
    card = next(c for c in _dump(app)["approvals"] if c["id"] == "perm-1")
    assert [o["id"] for o in card["options"]] == ["allow", "deny"], card
    _shot(app, "craze-permission.png")
    _lane(app, "lane.answer", host, approval_id="perm-1", answer={"choice": "deny"})
    app.wait_until(lambda: not _lane(app, "lane.approvals", host)["approvals"], timeout=WAIT,
                   what="the permission resolved")

    rig.op(app.fake_host, {"name": "question", "id": "q-1", "title": "Pick", "questions": [
        {"id": "lang", "prompt": "Which language?",
         "options": [{"id": "rs", "label": "Rust"}, {"id": "go", "label": "Go"}]}]})
    app.wait_until(lambda: any(c["id"] == "q-1" for c in _dump(app)["approvals"]),
                   timeout=WAIT, what="the question card")
    _lane(app, "lane.answer", host, approval_id="q-1", answer={"question": [["Rust"]]})
    app.wait_until(lambda: "? Which language? → Rust" in _texts(app, host), timeout=WAIT,
                   what="the question's note")

    rig.op(app.fake_host, {"name": "plan", "id": "plan-1", "planName": "Refactor",
                           "overview": "Split it", "plan": "1. split"})
    app.wait_until(lambda: any(a["id"] == "plan-1"
                               for a in _lane(app, "lane.approvals", host)["approvals"]),
                   timeout=WAIT, what="the plan approval")
    _lane(app, "lane.answer", host, approval_id="plan-1", answer={"choice": "accept"})
    app.wait_until(lambda: "plan Refactor → accepted" in _texts(app, host), timeout=WAIT,
                   what="the plan's note")
    _unmount(app)


def test_cancel_ends_a_running_turn(app, rig):
    """Cancel on a WORKING session ends its turn (the fake host holds the turn
    open, `hang_next`); on an idle one craze answers `not_accepting`."""
    host = FAKE_HOST
    _ready(app, host)
    idle = _error(lambda: _lane(app, "lane.cancel", host))
    assert idle.code == "not_accepting", idle
    rig.op(app.fake_host, {"name": "hang_next"})
    _lane(app, "lane.send", host, text="a long one")
    app.wait_until(lambda: _messages(app, host)["activity"] == "working", timeout=WAIT,
                   what="the turn to be running")
    _lane(app, "lane.cancel", host)
    app.wait_until(lambda: _messages(app, host)["activity"] != "working", timeout=WAIT,
                   what="the cancelled turn to end")


def test_stop_ends_the_session_and_its_row(app, created):
    """`lane.stop` ends the SESSION: the lane ends (`ended`) and the row leaves
    the roster — and its lane is evicted with it."""
    host = created["slug"]
    _ready(app, host)
    _lane(app, "lane.stop", host)
    app.wait_until(lambda: _row(app, host) is None, timeout=WAIT,
                   what="the stopped session's row to leave")
    gone = _error(lambda: _messages(app, host))
    assert gone.code == "no_lane", gone


# ---------------------------------------------------------------------------
# (2b) the create sheet and Open in terminal (plan 025 §3.6.6, §3.8 — C10)
# ---------------------------------------------------------------------------

#: What the C10 cells hand on: the session the sheet created (Open in
#: terminal's subject).
C10: dict[str, str] = {}


def _sheet(app: TauriClient) -> dict | None:
    return app.call("craze_create.dump")["craze_create"]


def _open_sheet(app: TauriClient, machine: str = LOCAL) -> dict:
    """`ui.show_craze_create`, then the sheet's options read (or failed)."""
    app.call("ui.show_craze_create", {"machine": machine})
    app.wait_until(lambda: bool((d := _sheet(app)) and d["machine"] == machine
                                and d["state"] != "loading"),
                   timeout=WAIT, what=f"the create sheet on {machine} to read its options")
    return _sheet(app)


def _close_sheet(app: TauriClient) -> None:
    app.call("ui.close_craze_create")
    app.wait_until(lambda: _sheet(app) is None, timeout=20, what="the create sheet to close")


def _fill(app: TauriClient, **fill) -> dict:
    """Fill the sheet (test-mode door) and wait until it SHOWS what was typed."""
    app.call("ui.fill_craze_create", fill)
    labels = {"cwd": "Directory", "prompt": "First prompt"}

    def shown() -> bool:
        d = _sheet(app) or {}
        return all(d.get("values", {}).get(labels[k]) == v for k, v in fill.items() if k in labels)

    app.wait_until(shown, timeout=20, what=f"the sheet to show {fill}")
    return _sheet(app)


def _submit_when_ready(app: TauriClient) -> None:
    app.wait_until(lambda: bool((d := _sheet(app)) and d["create_enabled"]), timeout=20,
                   what="the sheet's primary button to be enabled")
    app.call("ui.submit_craze_create")


def _rows_in(app: TauriClient, cwd: Path) -> list[dict]:
    return [r for r in _rows(app) if r.get("source") == "craze" and r.get("workdir") == str(cwd)]


def _exactly_one_session_in(app: TauriClient, cwd: Path) -> dict:
    """The one craze session in `cwd` — waited for, then given a moment for a
    second (which must never come) to be listed too."""
    app.wait_until(lambda: len(_rows_in(app, cwd)) >= 1, timeout=WAIT,
                   what=f"the session in {cwd} to be listed")
    time.sleep(scaled_timeout(2.0))
    rows = _rows_in(app, cwd)
    assert len(rows) == 1, f"exactly one session in {cwd}: {rows}"
    return rows[0]


def test_the_sheet_renders_crazes_options_in_order(app, created, rig):
    """**The create sheet** (plan 025 §3.8) renders craze's own createOptions,
    in craze's order: cursor UNAVAILABLE (no cursor-agent on the rig's PATH)
    and native NEEDS_SETUP (no key) both dimmed, each with craze's reason and
    fix, and NOT selectable — a click on one is refused; grok ready and
    preselected as craze's default. The recent directories are craze's (the
    created session's), one tap each."""
    d = _open_sheet(app)
    assert d["state"] == "idle", d
    assert [p["id"] for p in d["providers"]] == ["cursor", "grok", "native"], d["providers"]
    cursor, grok, native = d["providers"]
    assert cursor["state"] == "unavailable" and cursor["dimmed"] is True, cursor
    assert cursor["reason"] == "cursor-agent not found on PATH", cursor
    assert "install cursor-agent" in (cursor["fix"] or ""), cursor
    assert native["state"] == "needs_setup" and native["dimmed"] is True, native
    assert native["reason"] and native["fix"], native
    assert grok["dimmed"] is False and grok["selected"] is True, grok
    assert d["preselected"] == "grok" == d["default_provider"], d
    assert not cursor["selected"] and not native["selected"]

    # A dimmed provider refuses the click (the prompt, typed in the same fill,
    # proves the click was delivered before it).
    d = _fill(app, provider="cursor", prompt="probe")
    assert [p["id"] for p in d["providers"] if p["selected"]] == ["grok"], d["providers"]
    _fill(app, prompt="")

    recent = d["recent_dirs"]
    assert str(rig.work) in recent, f"the created session's directory is a recent one: {recent}"
    d = _fill(app, cwd="")
    app.call("ui.fill_craze_create", {"recent": recent.index(str(rig.work))})
    app.wait_until(lambda: (_sheet(app) or {}).get("values", {}).get("Directory") == str(rig.work),
                   timeout=20, what="the recent directory tapped into the field")
    assert _sheet(app)["create_enabled"] is True
    _shot(app, "craze-create-sheet.png")
    _close_sheet(app)


def test_the_launch_dialogs_target_list_leads_to_the_sheet(app):
    """The New-session dialog's "Where" lists a craze target for this machine
    (its craze is live and can create); picking it and pressing Continue opens
    craze's own sheet there."""
    app.call("ui.show_launch")
    app.wait_until(lambda: bool(app.call("launch.dump")["launch"]), timeout=20,
                   what="the launch dialog")
    assert "localhost — new craze session" in app.call("launch.dump")["launch"]["rendered"]
    app.call("ui.fill_launch", {"target": f"craze:{LOCAL}"})
    app.wait_until(lambda: "Continue" in (app.call("launch.dump")["launch"] or {}).get("rendered", ""),
                   timeout=20, what="the craze target selected")
    app.call("ui.submit_launch")
    app.wait_until(lambda: bool((d := _sheet(app)) and d["machine"] == LOCAL), timeout=WAIT,
                   what="the craze sheet, from the launch dialog")
    assert app.call("launch.dump")["launch"] is None, "the launch dialog gave way to the sheet"
    _close_sheet(app)


def test_a_create_from_the_sheet_opens_its_transcript_at_once(app, rig):
    """**Create, then the transcript at once** (plan 025 §3.8, §3.6.4): a
    provider, a fresh directory and a first prompt; Create closes the sheet and
    opens the new session's transcript straight away — the row was folded in
    on the create's own answer — and it shows the prompt and the fake agent's
    echo. Exactly one session runs there."""
    cwd = _mkdir_0700(rig.root / "w-sheet")
    rig.switch("proxy", True)
    before = len(rig.creates())
    _open_sheet(app)
    _fill(app, cwd=str(cwd), prompt="hello sheet")
    _shot(app, "craze-create-filled.png")
    _submit_when_ready(app)
    app.wait_until(lambda: _sheet(app) is None, timeout=WAIT, what="the sheet to close on Created")
    app.wait_until(lambda: bool((lane := _dump(app)) and lane["lane_kind"] == KIND
                                and any(r["text"] == "echo: hello sheet" for r in lane["rows"])),
                   timeout=WAIT, what="the new session's transcript, with its first prompt's echo")
    lane = _dump(app)
    host = lane["session_id"]
    assert any(r["text"] == "hello sheet" for r in lane["rows"]), lane["rows"]
    assert lane["permission"] == "runs tools without asking", "a sheet-created session runs bypass"
    row = _exactly_one_session_in(app, cwd)
    assert row["slug"] == host and row["provider"] == "grok", row
    sent = rig.creates()[before:]
    assert len(sent) == 1 and sent[0]["dropped"] is False, sent
    rig.switch("proxy", False)
    _shot(app, "craze-create-transcript.png")
    _unmount(app)
    C10["host"] = host


def test_an_unknown_outcome_is_retried_under_the_same_id(app, rig):
    """**An unknown outcome never makes a second session** (plan 025 §3.8): the
    bridge carrying the create is cut once the hub has it — and so is the
    source's one automatic retry, under the SAME requestId — so the sheet says
    the outcome is unknown and HOLDS that id. "Try again" resumes it: the same
    id a third time, craze answers with the session the first one started, and
    exactly one session runs."""
    cwd = _mkdir_0700(rig.root / "w-unknown")
    rig.switch("proxy", True)
    rig.switch("drop-creates", True)
    before = len(rig.creates())
    try:
        _open_sheet(app)
        _fill(app, cwd=str(cwd), prompt="")
        _submit_when_ready(app)
        try:
            app.wait_until(lambda: (_sheet(app) or {}).get("state") == "unknown", timeout=WAIT,
                           what="the sheet to report an unknown outcome")
        except Exception as e:
            raise AssertionError(f"{e}: sheet={_sheet(app)} creates={rig.creates()}") from e
    finally:
        rig.switch("drop-creates", False)
    d = _sheet(app)
    held = d["request_id"]
    assert held and held.startswith("shed-"), d
    assert d["primary"] == "Try again", d
    assert "check the session list" in d["rendered"], d["rendered"]
    assert d["error"]["code"] == "outcome_unknown", d["error"]
    sent = rig.creates()[before:]
    assert [c["requestId"] for c in sent] == [held, held], f"the source's own retry, same id: {sent}"
    assert all(c["dropped"] for c in sent), sent
    _shot(app, "craze-create-unknown.png")

    app.call("ui.submit_craze_create")  # Try again
    app.wait_until(lambda: _sheet(app) is None, timeout=WAIT, what="the sheet to close on Created")
    sent = rig.creates()[before:]
    assert [c["requestId"] for c in sent] == [held, held, held], f"Try again resumed the same request: {sent}"
    assert sent[-1]["dropped"] is False
    rig.switch("proxy", False)
    _exactly_one_session_in(app, cwd)
    _unmount(app)


def test_a_start_failure_shows_crazes_cause_and_try_again_mints_a_new_id(app, rig):
    """**A definite failure** (plan 025 §3.8): grok's agent dies at its start
    (`[agents]` → the fake's `exit-two-lines`), and the sheet shows craze's
    cause VERBATIM — the agent's two stderr lines — with the typed form intact
    and NO id held (craze would replay that failure under it). With the config
    fixed, "Try again" sends a NEW id, and one session results."""
    cwd = _mkdir_0700(rig.root / "w-fail")
    failing = _write_exec(rig.root / "grok-fail-agent",
                          f"#!/bin/sh\nexec '{rig.real / 'craze-fake-agent'}' -script exit-two-lines \"$@\"\n")
    rig.set_grok(failing)
    rig.switch("proxy", True)
    before = len(rig.creates())
    try:
        _open_sheet(app)
        _fill(app, cwd=str(cwd), prompt="this one will not start")
        _submit_when_ready(app)
        app.wait_until(lambda: (_sheet(app) or {}).get("state") == "refused", timeout=WAIT,
                       what="the start failure")
    finally:
        rig.set_grok(rig.grok_echo)
    d = _sheet(app)
    assert d["error"]["code"] == "failed", d["error"]
    assert "KEYCHAIN LOCKED" in d["cause"] and "Run unlock and retry." in d["cause"], d["cause"]
    assert d["cause"] == d["error"]["message"], "craze's cause, verbatim"
    assert d["request_id"] is None, "a definite answer ends the id's life"
    assert d["primary"] == "Try again"
    assert d["values"] == {"Directory": str(cwd), "First prompt": "this one will not start"}, d["values"]
    app.wait_until(lambda: not _rows_in(app, cwd), timeout=WAIT,
                   what="the failed start to leave no session")
    _shot(app, "craze-create-start-failed.png")

    app.call("ui.submit_craze_create")  # Try again, the config fixed
    app.wait_until(lambda: _sheet(app) is None, timeout=WAIT, what="the sheet to close on Created")
    sent = rig.creates()[before:]
    assert len(sent) == 2, sent
    assert sent[1]["requestId"] != sent[0]["requestId"], f"a NEW id after a definite failure: {sent}"
    rig.switch("proxy", False)
    _exactly_one_session_in(app, cwd)
    _unmount(app)


def test_the_typed_form_survives_the_machine_going_offline(app, rig):
    """**No state clears the typed form** (plan 025 §3.8): with the sheet open
    and filled, this machine's craze goes OFFLINE under it (its hub stopped, and
    craze unreachable for a while) — the sheet says so and disables Create, and
    keeps every typed character; when craze is back, the note goes and Create
    works again, the form as it was."""
    _open_sheet(app)
    typed = {"cwd": "/typed/before/the/outage", "prompt": "kept\nacross the outage"}
    _fill(app, **typed)
    rig.switch("outage", True)
    try:
        rig.sigterm_hub()
        app.wait_until(lambda: "offline" in ((_sheet(app) or {}).get("note") or ""), timeout=WAIT,
                       what="the sheet's offline note")
        d = _sheet(app)
        assert d["create_enabled"] is False, d
        assert d["values"] == {"Directory": typed["cwd"], "First prompt": typed["prompt"]}, d["values"]
        assert [p["id"] for p in d["providers"] if p["selected"]] == ["grok"]
        _shot(app, "craze-create-offline.png")
    finally:
        rig.switch("outage", False)
    app.wait_until(lambda: (_status(app, LOCAL) or {}).get("craze", {}).get("state") == "live",
                   timeout=WAIT * 2, what="this machine's craze back")
    app.wait_until(lambda: (_sheet(app) or {}).get("note") is None, timeout=WAIT,
                   what="the offline note gone")
    d = _sheet(app)
    assert d["values"] == {"Directory": typed["cwd"], "First prompt": typed["prompt"]}, d["values"]
    assert d["create_enabled"] is True, d
    _fill(app, cwd="", prompt="")
    _close_sheet(app)


def test_open_in_terminal_attaches_a_tab_across_roost_snapshots(app, roost):
    """**Open in terminal** (plan 025 §3.6.6) on the headless session the sheet
    created: a roost `tab.open` whose argv is the `sh -c` ladder running
    `craze attach --session '<hostId>'` — it RUNS exactly that, through a real
    `sh` and a stub craze at the ladder's first rung — and whose cwd is the
    session's workspace. The row shows that tab, and still does after a full
    roost resync (roost reports the tab unowned; the app's own map keeps it);
    End tab on it only detaches — the session runs on."""
    host = C10.get("host")
    assert host, "the sheet's session (test_a_create_from_the_sheet_opens_its_transcript_at_once)"
    row = _row(app, host)
    assert row and "tab_id" not in row, row
    opens = len(roost.opens)
    out = app.call("craze.open_terminal", {"machine": LOCAL, "session_id": host})
    assert len(roost.opens) == opens + 1, "one tab.open reached roost"
    call = roost.opens[-1]
    assert call["argv"] == out["argv"], (call, out)
    assert call["argv"][:2] == ["sh", "-c"], call["argv"]
    assert f"attach --session '{host}'" in call["argv"][2], call["argv"][2]
    assert call["cwd"] == row["workdir"], (call, row)
    # What it runs: the first rung ($HOME/.local/bin) is a stub that records.
    stub_home = Path(tempfile.mkdtemp(prefix="shcz-attach-", dir="/tmp")).resolve()
    try:
        record = stub_home / "argv"
        _write_exec(stub_home / ".local" / "bin" / "craze",
                    f"#!/bin/sh\nprintf '%s\\n' \"$*\" > '{record}'\n")
        subprocess.run(call["argv"], env={"HOME": str(stub_home), "PATH": "/usr/bin:/bin"},
                       check=True, timeout=10)
        assert record.read_text().strip() == f"attach --session {host}"
    finally:
        shutil.rmtree(stub_home, ignore_errors=True)

    tab = out["tab_id"]
    app.wait_until(lambda: (_row(app, host) or {}).get("tab_id") == tab, timeout=WAIT,
                   what="the row to show its tab")
    lists = roost.tab_list_calls
    roost.end_stream()
    app.wait_until(lambda: roost.tab_list_calls > lists, timeout=WAIT, what="roost's resync")
    deadline = time.monotonic() + scaled_timeout(1.5)
    while time.monotonic() < deadline:
        assert (_row(app, host) or {}).get("tab_id") == tab, "the row keeps its tab across a roost snapshot"
        time.sleep(0.1)
    app.navigate("agents")
    _shot(app, "craze-open-terminal.png")

    app.call("machine.kill", {"machine": LOCAL, "slug": tab})
    app.wait_until(lambda: "tab_id" not in (_row(app, host) or {"tab_id": tab}), timeout=WAIT,
                   what="the row headless again")
    assert _row(app, host) is not None, "closing the attach tab only detached: the session runs on"
    assert int(tab) not in roost.tab_ids()


# ---------------------------------------------------------------------------
# (2c) the settings sheet (plan 025 §3.10 — C11)
# ---------------------------------------------------------------------------

#: The permodel session's starting state, cursor's own (`craze-fake-agent
#: -script permodel`): four models, each with option catalog of its OWN.
PERMODEL_MODELS = ["grok-4.6", "composer-2.5", "claude-opus-5", "glm-5.2"]
#: The stale-model refusal as the sheet words it (`laneSettings.ts`).
STALE_MODEL_TEXT = "the model changed; try again"


def _settings(app: TauriClient) -> dict | None:
    return app.call("lane_settings.dump")["lane_settings"]


def _row_of(app: TauriClient, row: str) -> dict:
    d = _settings(app) or {}
    return next((r for r in d.get("rows", []) if r["id"] == row), {})


def _press(app: TauriClient, row: str, value: str) -> None:
    """Press `value` on the sheet's `row` (test-mode door) — the press a person
    makes, through the row's own gate."""
    app.call("ui.pick_lane_setting", {"row": row, "value": value})


def _agent_sets(calls: Path) -> list[str]:
    """The config sets the permodel agent was ASKED for (`<id>=<value>`), in
    order — its own record (`CRAZE_FAKE_DUMP_CALLS`)."""
    if not calls.exists():
        return []
    return [line.split(" ", 1)[1] for line in calls.read_text().splitlines()
            if line.startswith("session/set_config_option ")]


class SetGate:
    """The permodel agent's set gate (`CRAZE_FAKE_SET_GATE`): while the FIFO
    exists, each `set_config_option` waits for one byte before the agent
    answers — so a change stays PENDING, and the engine's one-at-a-time queue
    holds every set behind it. Absent, sets run at once."""

    def __init__(self, path: Path):
        self.path = path

    def __enter__(self) -> "SetGate":
        os.mkfifo(self.path, 0o600)
        return self

    def release(self) -> None:
        """Let one waiting set through (a reader holds the FIFO while one
        waits; open for writing only once it does)."""
        deadline = time.monotonic() + scaled_timeout(WAIT)
        while True:
            try:
                fd = os.open(self.path, os.O_WRONLY | os.O_NONBLOCK)
                break
            except OSError:
                assert time.monotonic() < deadline, "no set is waiting at the gate"
                time.sleep(0.05)
        try:
            os.write(fd, b"x")
        finally:
            os.close(fd)

    def __exit__(self, *_exc) -> None:
        self.path.unlink(missing_ok=True)


@pytest.fixture(scope="module")
def permodel(app, rig) -> dict:
    """A craze session running cursor's per-model catalogs — `craze-fake-agent
    -script permodel` as `cursor` (`[agents]` makes cursor ready; grok stays the
    default and the config is restored at once, so nothing else here sees
    cursor ready) — created through the harness's own bridge. Its lane's bridge
    runs through the rig's proxy (`sets.log`, `drop-sets`), which stays on for
    the settings cells. Answers `{host, calls, gate}`."""
    calls = rig.root / "permodel-calls"
    gate = rig.root / "set-gate"
    agent = _write_exec(rig.root / "permodel-agent", (
        "#!/bin/sh\n"
        f"export CRAZE_FAKE_DUMP_CALLS='{calls}'\n"
        f"export CRAZE_FAKE_SET_GATE='{gate}'\n"
        f"exec '{rig.real / 'craze-fake-agent'}' -script permodel \"$@\"\n"))
    rig.set_agents(grok=rig.grok_echo, cursor=agent)
    b = rig.bridge()
    try:
        assert b.call("hello", HELLO)["endpoint"]["kind"] == "hub"
        res = b.call("session.create", {"cwd": str(_mkdir_0700(rig.root / "w-settings")),
                                        "provider": "cursor",
                                        "requestId": "shedtest-settings-1"}, timeout=90)
        host = res["session"]["hostId"]
    finally:
        b.close()
        rig.set_grok(rig.grok_echo)
    app.wait_until(lambda: _row(app, host) is not None, timeout=WAIT,
                   what="the permodel session's craze row")
    rig.switch("proxy", True)
    try:
        yield {"host": host, "calls": calls, "gate": gate}
    finally:
        rig.switch("proxy", False)


def _sheet_open(app: TauriClient, host: str) -> dict:
    """`ui.show_lane_settings` on `host`, then the sheet's first report."""
    app.call("ui.show_lane_settings", {"machine": LOCAL, "kind": KIND, "session_id": host})
    app.wait_until(lambda: bool((d := _settings(app)) and d["session_id"] == host and d["rows"]),
                   timeout=WAIT, what="the settings sheet to open and report its rows")
    return _settings(app)


def test_the_chip_and_sheet_render_crazes_settings(app, permodel):
    """**The chip and the sheet render the session's settings** (plan 025
    §3.10): the transcript header's chip reads `<model> · <effort> · fast` from
    the CURRENT values; the sheet lists the model (a list, craze's order), the
    current model's options — the model and mode rows excluded, each ≤ 4 values
    a segmented control — and the mode; no context meter (an ACP session
    reports no usage)."""
    host = permodel["host"]
    _ready(app, host)
    _panel(app, host)
    app.wait_until(lambda: (_dump(app) or {}).get("settings_chip") == "Grok 4.6 · High · fast",
                   timeout=WAIT, what="the header's settings chip")
    d = _sheet_open(app, host)
    assert d["chip"] == "Grok 4.6 · High · fast", d
    rows = {r["id"]: r for r in d["rows"]}
    assert [r["id"] for r in d["rows"]] == ["model", "effort", "fast", "mode"], d["rows"]
    assert rows["model"]["control"] == "list"
    assert [v["id"] for v in rows["model"]["values"]] == PERMODEL_MODELS
    assert rows["model"]["current"] == "grok-4.6"
    assert rows["effort"]["control"] == "segmented" and rows["effort"]["current"] == "high"
    assert [v["name"] for v in rows["effort"]["values"]] == ["Low", "Medium", "High", "Extra High"]
    assert rows["fast"]["control"] == "segmented" and rows["fast"]["current"] == "true"
    assert rows["mode"]["control"] == "segmented"
    assert [v["id"] for v in rows["mode"]["values"]] == ["agent", "plan", "ask"]
    assert all(r["state"] is None and r["enabled"] for r in d["rows"]), d["rows"]
    assert d["usage"] is None, "an ACP session reports no usage: no context meter"
    _shot(app, "craze-settings-sheet.png")


def test_a_model_change_is_pending_then_redraws_the_options(app, rig, permodel):
    """**A press applies at once and the sheet re-renders from the next
    `Settings`** (plan 025 §3.10): with the agent holding its answer, the model
    row reads PENDING (and takes no other press); once it answers, the sheet
    redraws claude-opus-5's OWN options — thinking and effort (thought_level)
    before context and fast (model_config), effort now five values and so a
    list. An option change and a mode change come back the same way."""
    host = permodel["host"]
    _sheet_open(app, host)
    with SetGate(permodel["gate"]) as gate:
        _press(app, "model", "claude-opus-5")
        app.wait_until(lambda: _row_of(app, "model").get("state") == "pending", timeout=WAIT,
                       what="the model row pending")
        row = _row_of(app, "model")
        assert row["text"] == "applying…" and row["enabled"] is False, row
        assert row["current"] == "grok-4.6", "no optimistic value while pending"
        _shot(app, "craze-settings-pending.png")
        gate.release()
        app.wait_until(lambda: _row_of(app, "model").get("current") == "claude-opus-5",
                       timeout=WAIT, what="the model change in the sheet")
    app.wait_until(lambda: _row_of(app, "model").get("state") is None, timeout=WAIT,
                   what="the model row settled")
    d = _settings(app)
    assert [r["id"] for r in d["rows"]] == ["model", "thinking", "effort", "context", "fast", "mode"], d["rows"]
    effort = _row_of(app, "effort")
    assert effort["control"] == "list" and len(effort["values"]) == 5, effort
    assert [v["id"] for v in _row_of(app, "model")["values"]][0] == "claude-opus-5", \
        "the current model first"
    assert d["chip"] == "Claude Opus 5 · High", "fast is off on claude-opus-5: the chip says nothing of it"
    _shot(app, "craze-settings-model-changed.png")

    _press(app, "effort", "low")
    app.wait_until(lambda: _row_of(app, "effort").get("current") == "low", timeout=WAIT,
                   what="the option change")
    _press(app, "mode", "plan")
    app.wait_until(lambda: _row_of(app, "mode").get("current") == "plan", timeout=WAIT,
                   what="the mode change")
    assert _settings(app)["chip"] == "Claude Opus 5 · Low"
    sets = [e["params"]["setting"] for e in rig.sets()]
    assert {"kind": "config", "id": "effort", "value": "low", "forModel": "claude-opus-5"} in sets, sets
    assert {"kind": "mode", "value": "plan"} in sets, sets


def test_a_stale_model_is_refused_inline_and_the_retry_is_a_new_command(app, rig, permodel):
    """**`stale_model`, inline** (plan 025 §3.10): a second client moves the
    session back to grok-4.6 while the agent holds that change; the sheet's
    effort press — chosen on claude-opus-5, and bound to it (`forModel`) —
    queues behind it, and once the move lands craze refuses it `stale_model`:
    the effort row says "the model changed; try again" and the sheet shows
    grok-4.6's options. The change never reached the agent. The retry is a NEW
    command, bound to grok-4.6, and takes."""
    host = permodel["host"]
    calls = permodel["calls"]
    _sheet_open(app, host)
    assert _row_of(app, "model").get("current") == "claude-opus-5"
    other = rig.bridge()
    try:
        assert other.call("hello", HELLO)["endpoint"]["kind"] == "hub"
        other.call("session.connect", {"sessionId": host})
        other.call("hello", HELLO)
        sid = other.call("sessions.list", {})["sessions"][0]["sessionId"]
        with SetGate(permodel["gate"]) as gate:
            moved = other.send("session.set", {"sessionId": sid, "commandId": "1",
                                               "setting": {"kind": "model", "value": "grok-4.6"}})
            app.wait_until(lambda: _agent_sets(calls)[-1:] == ["model=grok-4.6"], timeout=WAIT,
                           what="the other client's move at the agent")
            before = len(rig.sets())
            _press(app, "effort", "max")
            app.wait_until(lambda: len(rig.sets()) > before, timeout=WAIT,
                           what="the sheet's change on the wire")
            chosen = rig.sets()[-1]["params"]
            assert chosen["setting"] == {"kind": "config", "id": "effort", "value": "max",
                                         "forModel": "claude-opus-5"}, chosen
            assert _row_of(app, "effort").get("state") == "pending"
            # Let the change reach craze's queue behind the held move.
            time.sleep(scaled_timeout(0.5))
            gate.release()
            app.wait_until(lambda: _row_of(app, "effort").get("state") == "refused", timeout=WAIT,
                           what="the stale_model refusal on the effort row")
        assert other.wait(moved, "session.set")["value"] == "grok-4.6"
    finally:
        other.close()
    row = _row_of(app, "effort")
    assert row["text"] == STALE_MODEL_TEXT, row
    assert _row_of(app, "model")["current"] == "grok-4.6", "the sheet redrew the session's model"
    assert [r["id"] for r in _settings(app)["rows"]] == ["model", "effort", "fast", "mode"]
    assert "effort=max" not in _agent_sets(calls), "a change chosen for another model reached the agent"
    _shot(app, "craze-settings-stale-model.png")

    _press(app, "effort", "medium")
    app.wait_until(lambda: _row_of(app, "effort").get("current") == "medium", timeout=WAIT,
                   what="the retry taking")
    assert _row_of(app, "effort")["state"] is None
    retry = rig.sets()[-1]["params"]
    assert retry["setting"]["forModel"] == "grok-4.6", retry
    assert retry["commandId"] != chosen["commandId"], "the retry is a NEW command"


def _other_client(rig: CrazeEnv, host: str) -> tuple["Bridge", str]:
    """A second client of `host`'s session — the rig's own `craze bridge
    --hub`, spliced to the host — and the craze session id it speaks for."""
    other = rig.bridge()
    assert other.call("hello", HELLO)["endpoint"]["kind"] == "hub"
    other.call("session.connect", {"sessionId": host})
    other.call("hello", HELLO)
    return other, other.call("sessions.list", {})["sessions"][0]["sessionId"]


def test_an_option_is_bound_to_the_model_the_sheet_displayed(app, rig, permodel):
    """**The model the sheet DISPLAYED binds an option** (plan 025 Amendment
    A13): a second client moves the session to composer-2.5 and the lane FOLDS
    it (`lane.messages` says composer-2.5), while the sheet still shows
    grok-4.6 (the panel's view held, `ui.hold_lane_view` — the frame between
    the adapter folding a change and the panel rendering it). The fast press —
    an option composer-2.5 also has — goes out bound to grok-4.6, and craze
    refuses it `stale_model`, inline on its row, rather than apply it to
    composer-2.5. It never reaches the agent. Released, the sheet shows
    composer-2.5; the session is moved back to grok-4.6 for the next cell."""
    host = permodel["host"]
    calls = permodel["calls"]
    target = {"machine": LOCAL, "kind": KIND, "session_id": host}
    _sheet_open(app, host)
    assert _row_of(app, "model").get("current") == "grok-4.6"
    assert _row_of(app, "fast").get("current") == "true"
    other, sid = _other_client(rig, host)
    try:
        app.call("ui.hold_lane_view", {"hold": True})
        try:
            other.call("session.set", {"sessionId": sid, "commandId": "1",
                                       "setting": {"kind": "model", "value": "composer-2.5"}})
            app.wait_until(lambda: (app.call("lane.messages", target).get("settings") or {})
                           .get("model") == "composer-2.5",
                           timeout=WAIT, what="the move folded by the lane")
            assert _row_of(app, "model")["current"] == "grok-4.6", "the sheet still shows grok-4.6"
            before = len(rig.sets())
            _press(app, "fast", "false")
            app.wait_until(lambda: _row_of(app, "fast").get("state") == "refused", timeout=WAIT,
                           what="the stale_model refusal on the fast row")
            assert _row_of(app, "fast")["text"] == STALE_MODEL_TEXT
            sent = rig.sets()[before:]
            assert len(sent) == 1, sent
            assert sent[0]["params"]["setting"] == {"kind": "config", "id": "fast", "value": "false",
                                                    "forModel": "grok-4.6"}, sent
            assert "fast=false" not in _agent_sets(calls), "an option chosen on grok-4.6 reached composer-2.5"
            _shot(app, "craze-settings-displayed-model.png")
        finally:
            app.call("ui.hold_lane_view", {"hold": False})
        app.wait_until(lambda: _row_of(app, "model").get("current") == "composer-2.5", timeout=WAIT,
                       what="the sheet, released, showing the move")
        other.call("session.set", {"sessionId": sid, "commandId": "2",
                                   "setting": {"kind": "model", "value": "grok-4.6"}})
        app.wait_until(lambda: _row_of(app, "model").get("current") == "grok-4.6", timeout=WAIT,
                       what="the session back on grok-4.6")
    finally:
        other.close()


def test_a_change_lost_to_a_drop_is_not_confirmed_until_the_next_settings(app, rig, permodel):
    """**A lost answer is "not confirmed", never resent** (plan 025 §3.10): the
    lane's bridge is cut once craze has the fast change (and the redial fails a
    while — this machine's craze unreachable), so the fast row reads "not
    confirmed", still showing the old value; when craze is back the lane
    resumes, and its next `Settings` — the change DID run — replaces the mark
    with the real value. The agent was asked once, and the wire carried it
    once."""
    host = permodel["host"]
    calls = permodel["calls"]
    _sheet_open(app, host)
    assert _row_of(app, "fast").get("current") == "true"
    before = len(rig.sets())
    rig.switch("outage", True)
    rig.switch("drop-sets", True)
    try:
        _press(app, "fast", "false")
        app.wait_until(lambda: _row_of(app, "fast").get("state") == "not_confirmed", timeout=WAIT,
                       what="the fast row not confirmed")
        row = _row_of(app, "fast")
        assert row["current"] == "true", "the old value until the session says otherwise"
        assert row["text"].startswith("not confirmed"), row
        app.wait_until(lambda: bool((_dump(app) or {}).get("stale")), timeout=WAIT,
                       what="the panel to say the lane is reconnecting")
        assert _row_of(app, "fast").get("state") == "not_confirmed", "still not confirmed"
        assert "fast=false" in _agent_sets(calls), "craze ran the change"
        _shot(app, "craze-settings-not-confirmed.png")
    finally:
        rig.switch("drop-sets", False)
        rig.switch("outage", False)
    app.wait_until(lambda: _row_of(app, "fast").get("state") is None
                   and _row_of(app, "fast").get("current") == "false",
                   timeout=WAIT * 2, what="the resume's Settings replacing the mark")
    time.sleep(scaled_timeout(1.0))
    sent = [e for e in rig.sets()[before:] if e["params"]["setting"].get("id") == "fast"]
    assert len(sent) == 1 and sent[0]["dropped"] is True, f"never resent: {sent}"
    assert _agent_sets(calls).count("fast=false") == 1, _agent_sets(calls)
    app.call("ui.close_lane_settings")
    app.wait_until(lambda: _settings(app) is None, timeout=20, what="the sheet to close")
    assert (_dump(app) or {}).get("settings_chip"), "closing the sheet leaves the chip"
    _unmount(app)


def test_no_settings_where_the_capabilities_say_none(app, roost):
    """**Hidden, never disabled** (plan 025 §3.10): an opencode lane on this
    machine — its capabilities say `settings: false` — gets no chip, and
    `ui.show_lane_settings` opens its transcript and NO sheet."""
    session = "ses_settingless"
    tab = 90
    oc = FakeOpencode()
    oc.add_session(session, title="no settings here", directory="/home/shed/oc")
    oc.set_simple_transcript(session, "a question", "an answer")
    oc.set_status(session, "idle")
    try:
        roost.add_tab(tab, cwd="/home/shed/oc", title="oc", source="opencode", session_id=session,
                      lifecycle="working", detail="session_status", shell_state="unknown",
                      metadata={"server_url": oc.base_url})
        app.wait_until(lambda: any((r.get("agent_lane") or {}).get("session_id") == session
                                   for r in _rows(app)),
                       timeout=WAIT, what="the opencode row's lane stamp")
        target = {"machine": LOCAL, "kind": "opencode", "session_id": session}
        app.call("lane.open", target)
        app.call("ui.show_lane_settings", target)
        app.wait_until(lambda: bool((d := _dump(app)) and d["session_id"] == session
                                    and d["kind"] == "opencode" and d["generation"] >= 1),
                       timeout=WAIT, what="the opencode transcript, seeded")
        caps = app.call("lane.messages", target)["capabilities"]
        assert caps["settings"] is False, caps
        assert _dump(app)["settings_chip"] is None, "no chip"
        time.sleep(scaled_timeout(0.5))
        assert _settings(app) is None, "no sheet, even when asked for"
        _shot(app, "craze-settings-none.png")
        _unmount(app)
    finally:
        app.call("machine.kill", {"machine": LOCAL, "slug": str(tab)})
        app.wait_until(lambda: tab not in roost.tab_ids(), timeout=WAIT, what="the opencode tab gone")
        oc.stop()


# ---------------------------------------------------------------------------
# (3) the row merge — D4, on this machine
# ---------------------------------------------------------------------------


def test_a_craze_tab_folds_into_its_hub_row_and_returns_when_the_feed_drops(app, rig, roost):
    """**D4 literally** (plan 025 §3.6.3): a roost tab owned `(craze, X)` is
    absorbed into the hub row whose providerSessionId is X — one row, the
    hub's, carrying the tab's id — and with the hub feed down roost's row
    stands alone again, beside the hub row's last-known, STALE copy.

    LAST in the module: it takes this machine's craze away (the seam's craze
    renamed, so the eager source cannot birth a new hub) and stops the hub."""
    fake = _row(app, FAKE_HOST)
    provider_session = fake.get("provider_session_id")
    assert provider_session, f"the fake host's row names its provider session: {fake}"
    roost.add_tab(CRAZE_TAB, cwd=str(rig.work), title="craze", source="craze",
                  session_id=provider_session, lifecycle="working")
    app.wait_until(lambda: (_row(app, FAKE_HOST) or {}).get("tab_id") == str(CRAZE_TAB),
                   timeout=WAIT, what="the tab folded into the hub row")
    assert not [r for r in _rows(app) if r.get("source") == "roost"], \
        "an absorbed craze tab is not a roost row"

    # The feed goes down for good: no craze to redial, and the hub stopped.
    (rig.path_dir / "craze").rename(rig.path_dir / "craze.off")
    rig.sigterm_hub()
    app.wait_until(lambda: any(r.get("source") == "roost" and r["slug"] == str(CRAZE_TAB)
                               for r in _rows(app)),
                   timeout=WAIT, what="roost's row back once the feed is down")
    retained = _row(app, FAKE_HOST)
    assert retained["stale"] is True and retained["approximate"] is True, retained
    assert "tab_id" not in retained, "nothing is absorbed while the feed is down"
    assert _status(app, LOCAL)["craze"]["state"] in ("offline", "absent")


# ---------------------------------------------------------------------------
# (4) not installed — and the host's own craze never answers
# ---------------------------------------------------------------------------


@pytest.fixture(scope="module")
def app_absent(bins, roost, mock):
    """An EMPTY seam (craze not installed) and a sentinel craze at an absolute
    rung under this app's HOME (`$HOME/.nix-profile/bin`), which the jailed
    ladder never reaches."""
    root = Path(tempfile.mkdtemp(prefix="shcz-", dir="/tmp")).resolve()
    env = CrazeEnv(bins, root, root / "h", kind="empty")
    sentinel_log = root / "sentinel.log"
    _write_exec(env.home / ".nix-profile" / "bin" / "craze",
                f"#!/bin/sh\necho ran >> '{sentinel_log}'\nexit 2\n")
    started = _launch(mock, runtime_dir=env.home, craze=env, roost=roost)
    client = started[0]
    client.sentinel_log = sentinel_log  # type: ignore[attr-defined]
    try:
        client.wait_until(lambda: (_status(client, LOCAL) or {}).get("craze", {}).get("cause")
                          == "not_installed", timeout=WAIT, what="craze read as not installed")
        yield client
    finally:
        _stop(*started)
        left = env.teardown()
        shutil.rmtree(root, ignore_errors=True)
        assert not left, f"craze processes left behind: {left}"


def test_not_installed_shows_no_craze_rows_and_no_note(app_absent):
    """No craze → no craze rows, `absent`, and NOTHING in the machines pane —
    a machine without craze is not a problem to report. And the sentinel at an
    absolute rung never ran."""
    status = _status(app_absent, LOCAL)
    assert status["craze"]["state"] == "absent", status
    assert not [r for r in _rows(app_absent) if r.get("source") == "craze"]
    app_absent.navigate("machines")
    app_absent.wait_until(lambda: app_absent.machines_dump(), timeout=20,
                          what="the machines pane to report")
    local = next(m for m in app_absent.machines_dump() if m["name"] == LOCAL)
    assert local.get("craze_note") is None, local
    assert not app_absent.sentinel_log.exists(), "a host-installed craze answered a hermetic run"


# ---------------------------------------------------------------------------
# (5) remote: attach-only, through a fake ssh
# ---------------------------------------------------------------------------


def _fake_ssh(root: Path) -> str:
    """A stand-in for `ssh` that runs each remote command in the jail its
    destination's USER names, recording every command first.

    * `-O exit` is a recorded no-op; a remote `true` (roost's warm-up)
      succeeds; roost's `client-bridge` chain is answered `command not found`
      (these machines run no roost-session — they are craze hosts).
    * **A craze command naming an absolute ladder rung is REFUSED and
      recorded, never run** — the production ladder would reach this host's
      own craze there, and the remote dials must be the jailed composition.
      (roost's own bootstrap probe, which these machines also get, is not a
      craze command and is left alone.)
    * Everything else runs as `/bin/sh -c "$cmd"` under exactly the jail's
      `HOME`, `CRAZE_HOME`, `CRAZE_RUNTIME_DIR` and `PATH` (its own `bin`,
      which holds `sh` beside `craze`).
    """
    return f"""#!/bin/sh
set -u
root='{root}'
ctl=
want=0
prev=
is_exit=0
dest=
cmd=
for arg in "$@"; do
    if [ "$want" -eq 1 ]; then ctl="$arg"; want=0; fi
    if [ "$arg" = "-S" ]; then want=1; fi
    if [ "$prev" = "-O" ] && [ "$arg" = "exit" ]; then is_exit=1; fi
    case "$arg" in
        *" "*) ;;
        *@*) dest="$arg" ;;
    esac
    prev="$arg"
    cmd="$arg"
done
if [ "$is_exit" -eq 1 ]; then
    if [ -n "$ctl" ]; then rm -f "$ctl"; fi
    exit 0
fi
if [ -n "$ctl" ] && [ ! -e "$ctl" ]; then : > "$ctl"; fi
user=${{dest##*://}}
user=${{user%@*}}
user=${{user%%:*}}
jail="$root/jails/$user"
# A refusal is marked BEFORE the command is recorded, so a reader that sees the
# record can trust the marker's absence.
refuse=0
case "$cmd" in
  *craze*)
    case "$cmd" in
      */usr/local/bin*|*/opt/homebrew/bin*) refuse=1; : > "$root/refused" ;;
    esac
    ;;
esac
rec=$(mktemp "$root/cmds/$user.XXXXXX")
printf '%s' "$cmd" > "$rec"
if [ "$refuse" -eq 1 ]; then
    printf '%s\\n' 'fake ssh: refused a craze command naming an absolute rung' >&2
    exit 255
fi
if [ "$cmd" = "true" ]; then exit 0; fi
case "$cmd" in
  *client-bridge*)
    printf '%s\\n' 'roost-session: command not found' >&2
    exit 127
    ;;
esac
if [ ! -d "$jail" ]; then
    printf '%s\\n' "ssh: Could not resolve hostname $dest" >&2
    exit 255
fi
exec env -i HOME="$jail/h" CRAZE_HOME="$jail/ch" CRAZE_RUNTIME_DIR="$jail/r" \\
    PATH="$jail/bin" /bin/sh -c "$cmd"
"""


class Remote:
    """The remote module rig: a fake `ssh`, and one jail per machine — each a
    `CrazeEnv` whose `bin` also holds `sh` (the remote command is `sh -c …`)."""

    def __init__(self, bins: Path):
        self.root = Path(tempfile.mkdtemp(prefix="shcr-", dir="/tmp")).resolve()
        _mkdir_0700(self.root / "cmds")
        _mkdir_0700(self.root / "jails")
        self.jails: dict[str, CrazeEnv] = {}
        for name, kind in (("craze-dormant", "recipe"), ("craze-live", "recipe"),
                           ("craze-old", "old")):
            jail = self.root / "jails" / name
            env = CrazeEnv(bins, _mkdir_0700(jail), jail / "h", kind=kind)
            os.symlink("/bin/sh", env.path_dir / "sh")
            self.jails[name] = env
        # The dormant jail's sentinel: an absolute rung under ITS HOME.
        self.sentinel_log = self.root / "sentinel.log"
        _write_exec(self.jails["craze-dormant"].home / ".nix-profile" / "bin" / "craze",
                    f"#!/bin/sh\necho ran >> '{self.sentinel_log}'\nexit 2\n")
        self.ssh = _write_exec(self.root / "ssh", _fake_ssh(self.root))

    def commands(self, user: str) -> list[str]:
        return [p.read_text() for p in sorted((self.root / "cmds").glob(f"{user}.*"))]

    def teardown(self) -> list[int]:
        left: list[int] = []
        for env in self.jails.values():
            left += env.teardown()
        shutil.rmtree(self.root, ignore_errors=True)
        return left


@pytest.fixture(scope="module")
def remote(bins):
    r = Remote(bins)
    try:
        yield r
    finally:
        left = r.teardown()
        assert not left, f"craze processes left behind: {left}"


@pytest.fixture(scope="module")
def app_remote(remote, mock):
    """No local craze; the three remote machines through the fake ssh. The live
    machine's hub runs BEFORE launch (the harness's own bridge holds it), with a
    fake host in its roster, so the app's first probe finds it."""
    live = remote.jails["craze-live"]
    live.fake_host("0d0d0d0d0d0d", "craze-remote-fake")
    holder = live.bridge()
    assert holder.call("hello", HELLO)["endpoint"]["kind"] == "hub"
    runtime_dir = Path(tempfile.mkdtemp(prefix="shrm-", dir="/tmp")).resolve()
    started = _launch(mock, runtime_dir=runtime_dir, craze=None, roost=None, ssh_bin=remote.ssh)
    client = started[0]
    try:
        yield client
    finally:
        _stop(*started)
        holder.close()
        shutil.rmtree(runtime_dir, ignore_errors=True)


def _craze_state(app: TauriClient, name: str) -> dict:
    return ((_status(app, name) or {}).get("craze") or {})


def test_a_remote_hub_is_attached_over_ssh(app_remote):
    """A remote machine whose hub is running is attached — the probe found it,
    and ONE roster connection over the ssh duplex lists its sessions."""
    app_remote.wait_until(lambda: _craze_state(app_remote, "craze-live").get("state") == "live",
                          timeout=WAIT, what="the live remote's craze attached")
    app_remote.wait_until(lambda: _row(app_remote, "0d0d0d0d0d0d", machine="craze-live"),
                          timeout=WAIT, what="the remote fake host's row")
    row = _row(app_remote, "0d0d0d0d0d0d", machine="craze-live")
    assert row["origin"] == "machine:craze-live" and row["source"] == "craze", row


def test_no_remote_hub_is_born_while_dormant(app_remote, remote):
    """**Attach-only** (plan 025 §3.6.1): a remote machine with craze and NO hub
    stays dormant — the fake ssh recorded the find-only probe and never a
    `bridge --hub`, and no hub record exists there."""
    # Dormant — or a `bridge --hub`, which must never come.
    app_remote.wait_until(
        lambda: _craze_state(app_remote, "craze-dormant").get("state") == "dormant"
        or any("bridge --hub" in c for c in remote.commands("craze-dormant")),
        timeout=WAIT, what="the dormant remote's probe")
    cmds = remote.commands("craze-dormant")
    assert not any("bridge --hub" in c for c in cmds), "a remote hub was dialled while dormant"
    craze_cmds = [c for c in cmds if "craze" in c]
    assert craze_cmds, cmds
    assert all("providers --hub --json" in c for c in craze_cmds), \
        f"a dormant remote got something but the find-only probe: {craze_cmds}"
    assert not remote.jails["craze-dormant"].hub_pids(), "a remote hub was born"


def test_remote_dials_are_jailed_and_never_reach_a_host_craze(app_remote, remote):
    """Every remote craze command is the JAILED composition — the fake ssh
    refuses (and records) one that names an absolute rung — and the dormant
    jail's sentinel at an absolute rung never ran."""
    app_remote.wait_until(lambda: any("craze" in c for c in remote.commands("craze-dormant")),
                          timeout=WAIT, what="the dormant remote's first craze command")
    assert not (remote.root / "refused").exists(), "a remote craze command named an absolute rung"
    app_remote.wait_until(lambda: _craze_state(app_remote, "craze-dormant").get("state") == "dormant",
                          timeout=WAIT, what="the dormant remote's probe")
    assert not (remote.root / "refused").exists(), "a remote craze command named an absolute rung"
    assert not remote.sentinel_log.exists(), "a host-installed craze answered a remote probe"


def test_too_old_shows_the_machines_pane_note(app_remote):
    """craze v0.0.1 on a machine (the real binary) reads too old, and the
    machines pane SAYS so — with what to do."""
    app_remote.wait_until(lambda: _craze_state(app_remote, "craze-old").get("cause") == "too_old",
                          timeout=WAIT, what="craze-old read as too old")
    app_remote.navigate("machines")
    app_remote.wait_until(
        lambda: any(m["name"] == "craze-old" and m.get("craze_note")
                    for m in (app_remote.machines_dump() or [])),
        timeout=20, what="the machines pane's craze note")
    row = next(m for m in app_remote.machines_dump() if m["name"] == "craze-old")
    assert row["craze_note"] == "craze on this machine is too old for shed; update it", row
    assert row["craze_create"] is False, "too old: no create entry (plan 025 §3.8)"
    dormant = next(m for m in app_remote.machines_dump() if m["name"] == "craze-dormant")
    assert dormant.get("craze_note") is None, "dormant is not a problem to report"
    assert dormant["craze_create"] is True, "dormant: create is offered (plan 025 §3.6.5)"
    _shot(app_remote, "craze-machines.png")


def test_the_sheet_on_a_dormant_machine_starts_its_hub_and_attaches(app_remote, remote):
    """**Create on a DORMANT machine** (plan 025 §3.6.5, §3.6.1): opening the
    sheet is the explicit action that starts a remote hub — its
    `create_options` dials `bridge --hub` (jailed, like every remote command in
    test mode) where no hub ran — and the machine's source, WOKEN by it, probes
    and attaches at once rather than at its next 30 s dormant probe.

    LAST in the module: craze-dormant is no longer dormant after it."""
    machine = "craze-dormant"
    jail = remote.jails[machine]
    app_remote.wait_until(lambda: _craze_state(app_remote, machine).get("state") == "dormant",
                          timeout=WAIT, what="craze-dormant dormant")
    assert not jail.hub_pids()
    # Anchor on a fresh dormant probe: without the wake, the next one — the
    # earliest the source could attach — is a full DORMANT_PROBE (30 s) later.
    probes = lambda: [c for c in remote.commands(machine) if "providers --hub --json" in c]  # noqa: E731
    seen = len(probes())
    app_remote.wait_until(lambda: len(probes()) > seen, timeout=45, what="a fresh dormant probe")
    probed_at = time.monotonic()
    d = _open_sheet(app_remote, machine)
    assert d["state"] == "idle", d
    assert [p["id"] for p in d["providers"]] == ["cursor", "grok", "native"], d["providers"]
    assert d["preselected"] == "grok"
    assert any("bridge --hub" in c for c in remote.commands(machine)), \
        "the sheet's create_options dialled bridge --hub (an explicit action)"
    assert jail.hub_pids(), "a hub runs there now"
    assert not (remote.root / "refused").exists(), "a remote craze command named an absolute rung"
    app_remote.wait_until(lambda: _craze_state(app_remote, machine).get("state") == "live",
                          timeout=WAIT, what="craze-dormant attached")
    assert time.monotonic() - probed_at < 25, "woken by the sheet, not left to its next dormant probe"
    _shot(app_remote, "craze-create-dormant.png")
    _close_sheet(app_remote)
