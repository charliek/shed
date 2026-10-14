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

Four independent app instances (`test_tauri_lane.py`'s fixture shape — each
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
  connection once the hub has the create, so its answer is lost. It records
  the replies that come back too — every create's (`create-replies.log`) and
  every `session.attach`'s (`attaches.log`) — which is where a first-frame
  claim is asserted (P26): on what the app was told, not on a poll. The settings
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
* `app_shed` (plan 026 §3.2) — no local craze and no machines: one server,
  served by a PRIVATE mock that lists four sheds running, each reached
  through the same fake `ssh` in a jail of its own (a shed's ssh user is its
  name) and none running a roost-session — the running sheds a dormant craze
  is offered on without listing them.

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
from mockserver import MockShedServer

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
#: exists — craze runs the change, and the lane never reads its answer. While
#: `refuse-dials` exists (plan 026 §3.1), a NEW bridge refuses the connection
#: before any hub: logged to `refused.log`, a line on stderr, exit 1 — the
#: dial never reaches craze (Unreachable), and connections already up (the
#: roster's) are untouched. A create's `title` is logged with its requestId,
#: and every `session.setTitle`, `session.stop` and `session.prompt` (plan 029
#: C4: what the `/` menu's commands put on the wire) goes to `verbs.log` with
#: its params. While `hold-titles` (a FIFO, `SetGate`) exists, a logged
#: `session.setTitle` is HELD — not relayed until one byte is written to it —
#: so a rename stays pending for as long as a cell needs. The REPLY to every
#: `session.create` (the `session` row the hub answers with) goes to
#: `create-replies.log`, and the reply to every `session.attach` (whether it
#: carried a snapshot, and that snapshot's `settings.title`) to
#: `attaches.log` — each written as it comes DOWN the wire, before it is
#: relayed, so it is on record by the time the client has read it: what a
#: client was first told, which no poll of its state can prove (P26). While
#: `swap-plan` exists (plan 030 FU7), every slash catalog coming DOWN has the
#: mode id behind its `plan` row rewritten (`-swapped` appended): the same
#: rows in the same order, another action behind one of them — a catalog swap
#: no fake agent directive can make.
BRIDGE_PROXY = r"""
import json, os, subprocess, sys, threading, time
root = os.path.dirname(os.path.abspath(__file__))
if os.path.exists(os.path.join(root, "refuse-dials")):
    with open(os.path.join(root, "refused.log"), "a") as log:
        log.write(json.dumps({"argv": sys.argv[2:]}) + "\n")
    sys.stderr.write("shedtest: the bridge refused the connection\n")
    sys.stderr.flush()
    os._exit(1)
child = subprocess.Popen(sys.argv[1:], stdin=subprocess.PIPE, stdout=subprocess.PIPE)
cut = threading.Event()
# The requests whose REPLY is recorded too, by their id as JSON (so 1 and "1"
# stay apart): the log it goes to, and what the request said.
watched = {}
def note(name, entry):
    with open(os.path.join(root, name), "a") as log:
        log.write(json.dumps(entry) + "\n")
def answered(line):
    # A reply to a watched request, recorded as it came down — before it is
    # relayed (`relayed`: whether the client is given it at all).
    if not watched:
        return
    try:
        msg = json.loads(line)
    except ValueError:
        return
    if not isinstance(msg, dict) or "method" in msg or "id" not in msg:
        return
    w = watched.pop(json.dumps(msg["id"]), None)
    if w is None:
        return
    name, entry = w
    result = msg.get("result") if isinstance(msg.get("result"), dict) else {}
    entry["error"] = msg.get("error")
    entry["relayed"] = not cut.is_set()
    if name == "create-replies.log":
        entry["session"] = result.get("session")
    else:
        snap = result.get("snapshot")
        settings = (snap.get("settings") or {}) if isinstance(snap, dict) else {}
        entry["hostId"] = (result.get("session") or {}).get("hostId")
        entry["after"] = result.get("after")
        entry["snapshot"] = isinstance(snap, dict)
        entry["title"] = settings.get("title")
    note(name, entry)
def hold(gate):
    # While `gate` (a FIFO) exists, the line is held — logged, not yet relayed —
    # until one byte is read from it: the verb is on the wire and PENDING.
    # Removing the gate lets it through too, so a cell that fails while one is
    # held never wedges the lane's connection.
    if not os.path.exists(gate):
        return
    fd = os.open(gate, os.O_RDONLY | os.O_NONBLOCK)
    try:
        while os.path.exists(gate):
            try:
                if os.read(fd, 1):
                    return
            except BlockingIOError:
                pass
            time.sleep(0.02)
    finally:
        os.close(fd)
def swapped(line):
    # While `swap-plan` exists, a slash section coming down names another mode
    # behind its `plan` row; any other line is relayed byte for byte.
    if b'"slash"' not in line or not os.path.exists(os.path.join(root, "swap-plan")):
        return line
    try:
        msg = json.loads(line)
    except ValueError:
        return line
    hits = []
    # Only a catalog: a snapshot's `settings.slash` or a meta update's
    # `state.slash` — never a `slash` that some other payload happens to hold.
    def walk(v, parent=None):
        if isinstance(v, list):
            for x in v:
                walk(x)
        if not isinstance(v, dict):
            return
        for k, x in v.items():
            if (k == "slash" and parent in ("settings", "state") and isinstance(x, dict)
                    and isinstance(x.get("items"), list)):
                for it in x["items"]:
                    run = it.get("run") if isinstance(it, dict) else None
                    setting = run.get("setting") if isinstance(run, dict) else None
                    if (it.get("name") == "plan" and isinstance(setting, dict)
                            and isinstance(setting.get("value"), str)):
                        setting["value"] += "-swapped"
                        hits.append(it)
            else:
                walk(x, k)
    walk(msg)
    return (json.dumps(msg) + "\n").encode() if hits else line
def down():
    for line in child.stdout:
        answered(line)
        if cut.is_set():
            continue
        sys.stdout.buffer.write(swapped(line))
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
        params = msg.get("params") or {}
        entry = {"requestId": params.get("requestId"), "dropped": drop}
        if "title" in params:
            entry["title"] = params["title"]
        with open(os.path.join(root, "creates.log"), "a") as log:
            log.write(json.dumps(entry) + "\n")
        if "id" in msg:
            watched[json.dumps(msg["id"])] = ("create-replies.log", {"requestId": params.get("requestId")})
    if isinstance(msg, dict) and msg.get("method") == "session.attach" and "id" in msg:
        params = msg.get("params") or {}
        watched[json.dumps(msg["id"])] = ("attaches.log", {"sessionId": params.get("sessionId"),
                                                           "cursor": params.get("cursor") is not None})
    if isinstance(msg, dict) and msg.get("method") in ("session.setTitle", "session.stop", "session.prompt"):
        with open(os.path.join(root, "verbs.log"), "a") as log:
            log.write(json.dumps({"method": msg["method"], "params": msg.get("params")}) + "\n")
        if msg["method"] == "session.setTitle":
            hold(os.path.join(root, "hold-titles"))
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

    def _jsonl(self, name: str) -> list[dict]:
        """The proxy's log `name`, one JSON object a line, in order."""
        log = self.root / name
        if not log.exists():
            return []
        return [json.loads(line) for line in log.read_text().splitlines() if line.strip()]

    def creates(self) -> list[dict]:
        """Every `session.create` a proxied bridge carried: `{requestId,
        dropped}`, in order."""
        return self._jsonl("creates.log")

    def create_replies(self) -> list[dict]:
        """The hub's REPLY to every `session.create` a proxied bridge carried,
        recorded as it came down the wire: `{requestId, session, error,
        relayed}`, in order — `session` the roster row it answered with."""
        return self._jsonl("create-replies.log")

    def attaches(self) -> list[dict]:
        """The reply to every `session.attach` a proxied bridge carried,
        recorded as it came down the wire: `{sessionId, cursor, hostId,
        after, snapshot, title, error, relayed}`, in order — `snapshot`
        whether it carried one, `title` that snapshot's `settings.title`."""
        return self._jsonl("attaches.log")

    def refused(self) -> list[dict]:
        """Every NEW bridge the proxy refused while `refuse-dials` was on."""
        return self._jsonl("refused.log")

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

    def verbs(self, method: str | None = None) -> list[dict]:
        """Every `session.setTitle` / `session.stop` / `session.prompt` a
        proxied bridge carried (`{method, params}`, in order) — `method`'s
        alone when one is named."""
        return [v for v in self._jsonl("verbs.log") if method is None or v["method"] == method]

    def sets(self) -> list[dict]:
        """Every `session.set` a proxied bridge carried: `{params, dropped}`,
        in order."""
        return self._jsonl("sets.log")

    def switch(self, name: str, on: bool) -> None:
        """Turn one of the wrapper's switches (`outage`, `proxy`,
        `drop-creates`, `drop-sets`, `refuse-dials`, `swap-plan`) on or
        off."""
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
            ssh_bin: Path | None = None, config: Path = FIXTURES / "config-craze.yaml"):
    """An independent app instance (`test_tauri_lane.py`'s shape): its own
    HOME/XDG_RUNTIME_DIR, its own fake host-agent, the session app untouched."""
    cfg = ui._SUBPROC["tauri"]
    if not cfg.binary.exists():
        raise RuntimeError(f"tauri binary not found at {cfg.binary}; build it first (make tauri-build).")
    agent = FakeHostAgent()
    agent.start()
    client = proc = log_fh = None
    try:
        sock = runtime_dir / cfg.sock_rel
        log = runtime_dir / "craze-ui.log"
        env = ui.subproc_env(
            cfg, runtime_dir=runtime_dir, mock_base_url=mock.base_url,
            config_path=config, host_agent_socket=agent.socket_path,
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
    except BaseException:
        # A launch that failed after its spawn still owns what it started (plan
        # 026 §3.3, F9): the app — and with it its children — its log handle and
        # its agent, each released even if another's release fails.
        try:
            if client is not None:
                client.close()
        finally:
            try:
                if proc is not None:
                    ui.terminate(proc)
            finally:
                try:
                    if log_fh is not None:
                        log_fh.close()
                finally:
                    agent.stop()
        raise
    return client, proc, log_fh, agent


def _stop(client, proc, log_fh, agent) -> None:
    if client is not None:
        client.close()
    started = time.monotonic()
    ui.terminate(proc)
    took = time.monotonic() - started
    log_fh.close()
    agent.stop()
    print(f"\n[craze] app pid {proc.pid} stopped in {took:.2f}s (exit {proc.returncode})")
    # Plan 026 §3.3: the harness's SIGTERM runs the app's orderly shutdown,
    # which ends it by itself — the 5 s grace, and the SIGKILL after it, must
    # never be what ends an app.
    assert proc.returncode != -signal.SIGKILL, \
        f"the app needed the harness's SIGKILL after {took:.2f}s: its orderly shutdown hung"


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
    labels = {"cwd": "Directory", "title": "Name", "prompt": "First prompt"}

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
    echo, and its header says the session runs tools without asking. Exactly
    one session runs there."""
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
    # A sheet-created session runs bypass, and the panel it opened says so —
    # from the LIVE session row (`lane.messages`' `session`, which carries the
    # attach info document's `permissionMode`), never only from the row it was
    # opened with: that can be the create's own, with no permission mode, and
    # `lane.open` re-answers it for as long as the panel stays open (plan 025
    # §3.6.5; live leg 1). Which row the open got is a race this cell cannot
    # pin: the header rule is pinned by `crazeRows.test.mjs`'s REGRESSION, the
    # backend half by `lane.rs`'s `a_craze_lanes_permission_mode_arrives_on_
    # lane_messages_not_lane_open`.
    live = _messages(app, host)["session"]
    assert live and live["permission_mode"] == "bypass", live
    app.wait_until(lambda: (_dump(app) or {}).get("permission") == "runs tools without asking",
                   timeout=WAIT, what="the open panel's permission line, from the live row")
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


def _lose_a_create(app: TauriClient, rig: CrazeEnv, cwd: Path) -> tuple[str, str, int]:
    """Submit a create in `cwd` whose answer is LOST (the `drop-creates` seam:
    the hub has it, the bridge is cut — and so is the source's one automatic
    retry), wait for the sheet to say the outcome is unknown, and for the
    session the hub started to be listed. Answers the held request id, that
    session's hostId, and where `rig.creates()` stood before the submit. The
    `proxy` switch is left ON (the caller logs its retries through it)."""
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
    held = _sheet(app)["request_id"]
    assert held and held.startswith("shed-"), _sheet(app)
    sent = rig.creates()[before:]
    assert [c["requestId"] for c in sent] == [held, held], f"the source's own retry, same id: {sent}"
    app.wait_until(lambda: len(_rows_in(app, cwd)) == 1, timeout=WAIT,
                   what=f"the session the lost create started, in {cwd}")
    return held, _rows_in(app, cwd)[0]["slug"], before


def test_an_unknown_outcome_survives_its_provider_going_non_ready(app, rig):
    """**A held request id survives a re-read** (plan 026 §3.1, shed#401): after
    an unknown outcome, the create's provider goes non-ready (grok's agent no
    longer configured) and the sheet is closed and re-opened, so its options
    are read afresh. The held provider stays SELECTED (dimmed), the id is the
    same, and "Try again" works — readiness does not gate a replay, because
    craze answers a held id regardless. Pressed with grok STILL non-ready, it
    re-sends that id and craze answers with the session the first create
    started: its transcript opens, and exactly one session runs there."""
    cwd = _mkdir_0700(rig.root / "w-held-dimmed")
    held, host, before = _lose_a_create(app, rig, cwd)
    try:
        rig.set_agents()  # no [agents] at all: grok is not found, not ready
        _close_sheet(app)
        d = _open_sheet(app)
        grok = next((p for p in d["providers"] if p["id"] == "grok"), None)
        assert grok and grok["dimmed"] is True and grok["state"] != "ready", d["providers"]
        assert not any(p["dimmed"] is False for p in d["providers"]), \
            f"no provider is ready now: {d['providers']}"
        assert d["state"] == "unknown", d
        assert d["request_id"] == held, f"the same id after the re-read: {d}"
        assert [p["id"] for p in d["providers"] if p["selected"]] == ["grok"], d["providers"]
        assert d["primary"] == "Try again", d
        assert d["create_enabled"] is True, d
        assert "Changing the form starts a new request instead." in (d["note"] or ""), d["note"]
        assert "no provider is ready" not in (d["note"] or ""), d["note"]
        assert "held for the retry" in d["rendered"], d["rendered"]
        _shot(app, "craze-create-held-dimmed.png")

        _submit_when_ready(app)  # Try again, grok still not ready
        app.wait_until(lambda: _sheet(app) is None, timeout=WAIT, what="the sheet to close on Created")
        app.wait_until(lambda: (_dump(app) or {}).get("session_id") == host, timeout=WAIT,
                       what=f"the transcript of the ORIGINAL session {host}")
    finally:
        rig.set_grok(rig.grok_echo)
        rig.switch("proxy", False)
    sent = rig.creates()[before:]
    assert [c["requestId"] for c in sent] == [held, held, held], f"one id throughout: {sent}"
    assert sent[-1]["dropped"] is False, sent
    row = _exactly_one_session_in(app, cwd)
    assert row["slug"] == host, row
    _unmount(app)


def test_a_failed_manual_retry_keeps_the_held_id(app, rig):
    """**A held request id survives a failed retry** (plan 026 §3.1): after an
    unknown outcome, "Try again" is pressed while every NEW bridge refuses the
    connection — the retry never reaches craze, which says nothing about the
    request — so the sheet stays unknown, holding the SAME id (a definite error
    here would end it, and the next press would start a second session). With
    the bridges back, "Try again" re-sends that id and craze answers with the
    original session: one session, its hostId."""
    cwd = _mkdir_0700(rig.root / "w-held-refused")
    held, host, before = _lose_a_create(app, rig, cwd)
    refused_before = len(rig.refused())
    rig.switch("refuse-dials", True)
    try:
        _submit_when_ready(app)  # Try again, into a refused dial
        app.wait_until(
            lambda: "refused the connection" in (((_sheet(app) or {}).get("error") or {}).get("message") or ""),
            timeout=WAIT, what="the failed retry to settle")
        d = _sheet(app)
        assert d["state"] == "unknown", f"a retry that never reached craze is still unknown: {d}"
        assert d["request_id"] == held, f"the SAME id held: {d}"
        assert d["error"]["code"] == "outcome_unknown", d["error"]
        assert d["primary"] == "Try again" and d["create_enabled"] is True, d
        assert len(rig.refused()) - refused_before == 2, \
            f"the held attempt and its one retry, both refused: {rig.refused()}"
        assert len(rig.creates()) - before == 2, f"no create reached the hub: {rig.creates()[before:]}"
        _shot(app, "craze-create-held-refused.png")
    finally:
        rig.switch("refuse-dials", False)

    try:
        _submit_when_ready(app)  # Try again, the bridges back
        app.wait_until(lambda: _sheet(app) is None, timeout=WAIT, what="the sheet to close on Created")
        app.wait_until(lambda: (_dump(app) or {}).get("session_id") == host, timeout=WAIT,
                       what=f"the transcript of the ORIGINAL session {host}")
    finally:
        rig.switch("proxy", False)
    sent = rig.creates()[before:]
    assert [c["requestId"] for c in sent] == [held, held, held], f"one id throughout: {sent}"
    row = _exactly_one_session_in(app, cwd)
    assert row["slug"] == host, row
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
    # "Name" is the optional title field (plan 029 D14), shown on this hub.
    assert d["values"] == {"Directory": str(cwd), "Name": "", "First prompt": "this one will not start"}, \
        d["values"]
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
        # Offline, no hub says it takes a title: no Name field (none typed).
        assert d["values"] == {"Directory": typed["cwd"], "First prompt": typed["prompt"]}, d["values"]
        assert d["title_field"] is False, d
        assert [p["id"] for p in d["providers"] if p["selected"]] == ["grok"]
        _shot(app, "craze-create-offline.png")
    finally:
        rig.switch("outage", False)
    app.wait_until(lambda: (_status(app, LOCAL) or {}).get("craze", {}).get("state") == "live",
                   timeout=WAIT * 2, what="this machine's craze back")
    app.wait_until(lambda: (_sheet(app) or {}).get("note") is None, timeout=WAIT,
                   what="the offline note gone")
    d = _sheet(app)
    # Back live, the hub takes a title again: its Name field is back (empty).
    assert d["values"] == {"Directory": typed["cwd"], "Name": "", "First prompt": typed["prompt"]}, d["values"]
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
    holds every set behind it. Absent, sets run at once. Also the held
    session's turn gate (`CRAZE_FAKE_GATE`, `what="turn"`): one byte lets one
    held turn finish. And the proxy's rename hold (`hold-titles`,
    `what="rename"`): one byte relays one held `session.setTitle`."""

    def __init__(self, path: Path, what: str = "set"):
        self.path = path
        self.what = what

    def __enter__(self) -> "SetGate":
        os.mkfifo(self.path, 0o600)
        return self

    def release(self) -> None:
        """Let one waiting set (or turn) through (a reader holds the FIFO while
        one waits; open for writing only once it does)."""
        deadline = time.monotonic() + scaled_timeout(WAIT)
        while True:
            try:
                fd = os.open(self.path, os.O_WRONLY | os.O_NONBLOCK)
                break
            except OSError:
                assert time.monotonic() < deadline, f"no {self.what} is waiting at the gate"
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
    try:
        host = _hub_create(rig, "shedtest-settings-1", "w-settings", provider="cursor")
    finally:
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
        # Nor a `/` menu (plan 029 X4): opencode publishes no catalog.
        assert caps.get("slash") is False and caps.get("set_title") is False, caps
        assert app.call("lane.messages", target)["slash_menu"] is None
        assert _dump(app)["slash_menu"] is None
        time.sleep(scaled_timeout(0.5))
        assert _settings(app) is None, "no sheet, even when asked for"
        _shot(app, "craze-settings-none.png")
        _unmount(app)
    finally:
        app.call("machine.kill", {"machine": LOCAL, "slug": str(tab)})
        app.wait_until(lambda: tab not in roost.tab_ids(), timeout=WAIT, what="the opencode tab gone")
        oc.stop()


# ---------------------------------------------------------------------------
# (2d) the `/` menu, a rename, and a name at create (plan 029 C4)
# ---------------------------------------------------------------------------
#
# Every session here is created by the hub (a real `craze serve` running the
# fake agent), so it publishes craze 0.1.1's slash catalog; its lane runs
# through the rig's proxy, so what each command put on the wire is in
# `verbs.log` (`session.setTitle`, `session.stop`, `session.prompt`) or
# `sets.log` (`session.set`). The composer is driven through its doors —
# `ui.type_lane_composer`, `ui.key_lane_composer` (a REAL keydown on the
# textarea), `ui.pick_slash_row`, `ui.answer_lane_stop` — and read through
# `lane.dump`'s `draft` and `slash_menu`.

#: The rows a hub-created session's menu SHOWS (craze 0.1.1's builtins this
#: client can run, then the agent's `research`), in catalog order; `help`,
#: `clear`, `tasks`, `theme` (local) and `btw` (aside) are hidden in phase 1.
MENU_ROWS = ["model", "rename", "plan", "ask", "agent", "exit", "research"]
#: Rows that wait for the turn (no `whileWorking`, not a prompt): dimmed and
#: refused while one runs.
WAITING_ROWS = {"model", "plan", "ask", "agent"}


def _hub_create(rig: CrazeEnv, request_id: str, workdir: str, **params) -> str:
    """A session created through the harness's own bridge (`session.create`
    on the rig's hub) in a fresh directory under the rig; its hostId."""
    b = rig.bridge()
    try:
        assert b.call("hello", HELLO)["endpoint"]["kind"] == "hub"
        res = b.call("session.create", {"cwd": str(_mkdir_0700(rig.root / workdir)),
                                        "requestId": request_id, **params}, timeout=90)
        return res["session"]["hostId"]
    finally:
        b.close()


def _hub_session(app: TauriClient, rig: CrazeEnv, request_id: str, workdir: str) -> str:
    """`_hub_create`, its row listed, and the proxy switched on BEFORE its
    lane's first dial (so its verbs are logged). Its hostId."""
    host = _hub_create(rig, request_id, workdir)
    app.wait_until(lambda: _row(app, host) is not None, timeout=WAIT, what=f"{request_id}'s craze row")
    rig.switch("proxy", True)
    return host


def _catalog(app: TauriClient, host: str) -> list[str]:
    """The names of the resolved menu's rows on the lane (`lane.messages`)."""
    return [r["name"] for r in (_messages(app, host).get("slash_menu") or {}).get("rows", [])]


def _menu(app: TauriClient) -> dict | None:
    """The composer's `/` menu as the panel rendered it (`lane.dump`)."""
    return (_dump(app) or {}).get("slash_menu")


def _draft(app: TauriClient) -> str | None:
    return (_dump(app) or {}).get("draft")


def _type(app: TauriClient, text: str) -> None:
    """Set the composer's draft (test-mode door) and wait until the panel
    SHOWS it. Re-sent until it lands (it is idempotent): the panel registers
    the door's listener after it mounts, and an event sent before that lands
    on nobody. It is the last door the panel registers, so once it lands the
    others are live."""
    def typed() -> bool:
        app.call("ui.type_lane_composer", {"text": text})
        return _draft(app) == text
    app.wait_until(typed, timeout=20, what=f"the composer to hold {text!r}")


def _key(app: TauriClient, key: str, **flags) -> None:
    """A real keydown on the composer (test-mode door)."""
    app.call("ui.key_lane_composer", {"key": key, **flags})


def _wait_menu(app: TauriClient, token: str, open_: bool = True) -> dict:
    """Wait for the menu to have answered `token`, open (or closed)."""
    app.wait_until(lambda: bool((m := _menu(app)) and m["token"] == token and m["open"] is open_),
                   timeout=20, what=f"the menu {'open' if open_ else 'closed'} for {token!r}")
    return _menu(app)


def _names(menu: dict) -> list[str]:
    return [r["name"] for r in menu["rows"]]


def _highlighted(menu: dict) -> str | None:
    return next((r["name"] for r in menu["rows"] if r["highlighted"]), None)


def _menu_ready(app: TauriClient, host: str) -> dict:
    """The lane open and seeded, its resolved menu in, its panel mounted."""
    _ready(app, host)
    app.wait_until(lambda: bool(_catalog(app, host)), timeout=WAIT, what=f"the resolved slash menu on {host}")
    _panel(app, host)
    app.wait_until(lambda: _menu(app) is not None, timeout=20, what="the composer to have the menu")
    return _messages(app, host)


def _settle(seconds: float = 1.0) -> None:
    """A bounded beat for a NEGATIVE assertion ("nothing was sent"): there is
    no event to wait on for a thing that must not happen."""
    time.sleep(scaled_timeout(seconds))


def _poll(read, what: str, timeout: float = WAIT):
    """Poll `read` until it answers something other than `None`, and return
    THAT answer — the first one this poll saw, which is all a poll can say. It
    is NOT a thing's first frame: an untitled seed renamed between two reads
    is never seen. A first-frame claim (P26) is asserted on the wire — on the
    replies the bridge proxy recorded (`rig.create_replies()`,
    `rig.attaches()`) — never by polling. A `ShedError` (a lane not open yet)
    counts as no answer."""
    deadline = time.monotonic() + scaled_timeout(timeout)
    while True:
        try:
            got = read()
        except ShedError:
            got = None
        if got is not None:
            return got
        assert time.monotonic() < deadline, f"timed out waiting for {what}"
        time.sleep(0.02)


@pytest.fixture(scope="module")
def slashed(app, rig) -> str:
    """A hub-created grok session (the echo agent) for the menu cells, its
    lane dialled through the proxy (switched on BEFORE the lane's first
    dial). Its hostId."""
    return _hub_session(app, rig, "shedtest-slash-1", "w-slash")


def test_no_menu_without_a_slash_capability(app, rig):
    """**No catalog, no menu** (plan 029 D11): the fake host's session states
    no `slashCatalog`, so its capabilities say `slash: false`, its view has no
    menu and the panel renders none — and a typed `/help` goes out as TEXT,
    a prompt, exactly as before the menu existed."""
    if _dump(app):
        _unmount(app)
    # Its own connection through the proxy: an earlier cell may have left the
    # lane open on one that is not.
    rig.switch("proxy", True)
    _lane(app, "lane.close", FAKE_HOST)
    view = _ready(app, FAKE_HOST)
    assert view["capabilities"]["slash"] is False, view["capabilities"]
    assert view["slash_menu"] is None
    _panel(app, FAKE_HOST)
    before = len(rig.verbs("session.prompt"))
    _type(app, "/help")
    _settle(0.5)
    assert _menu(app) is None, "no menu on a session without a catalog"
    _key(app, "Enter")
    app.wait_until(lambda: any(v["params"]["text"] == "/help" for v in rig.verbs("session.prompt")[before:]),
                   timeout=WAIT, what="`/help` sent to the fake host as a prompt")
    app.wait_until(lambda: _draft(app) == "", timeout=20, what="the sent draft cleared")
    _unmount(app)


def test_slash_lists_the_sessions_commands(app, rig, slashed):
    """**`/` lists what the session can run** (plan 029 §3.4-§3.5): the rows
    the shared core shows for a hub-created session, in craze's order, the
    first highlighted, none dimmed while idle — and not one of the phase-1
    hidden builtins. The catalog is the AGENT's, live: a `SET-COMMANDS`
    directive (the agent re-advertising its commands) replaces its row, and
    the open menu follows."""
    host = slashed
    view = _menu_ready(app, host)
    assert view["capabilities"]["slash"] is True and view["capabilities"]["set_title"] is True
    hidden = {h["name"] for h in view["slash_menu"]["hidden"]}
    assert {"help", "clear", "tasks", "theme", "btw"} <= hidden, hidden
    _type(app, "/")
    m = _wait_menu(app, "/")
    assert _names(m) == MENU_ROWS, _names(m)
    assert _highlighted(m) == "model" and not any(r["dimmed"] for r in m["rows"]), m["rows"]
    _shot(app, "craze-slash-menu.png")

    # P9: the highlight goes back to the first row on EVERY filter change —
    # `/r` → `/re` lists the very same two rows, and still starts over.
    _type(app, "/r")
    m = _wait_menu(app, "/r")
    assert _names(m) == ["rename", "research"], _names(m)
    _key(app, "ArrowDown")
    app.wait_until(lambda: _highlighted(_menu(app)) == "research", timeout=20, what="↓ to research")
    _type(app, "/re")
    m = _wait_menu(app, "/re")
    assert _names(m) == ["rename", "research"], _names(m)
    assert _highlighted(m) == "rename", f"a new filter starts on its first row: {m['rows']}"

    # Dismissed with Esc, the menu STAYS closed when the catalog is replaced
    # under the same token — only a token change ends a dismissal (P9), and a
    # replaced menu awaiting its filter is no change of token.
    _key(app, "Escape")
    _wait_menu(app, "/re", open_=False)
    _lane(app, "lane.send", host, text="SET-COMMANDS alpha-cmd,beta-cmd")
    app.wait_until(lambda: _catalog(app, host)[-2:] == ["alpha-cmd", "beta-cmd"], timeout=WAIT,
                   what="the agent's new catalog on the lane")
    _settle()
    m = _menu(app)
    assert m["open"] is False and m["token"] == "/re" and _draft(app) == "/re", \
        f"a catalog refresh reopened a dismissed menu: {m}"
    _type(app, "/a")
    m = _wait_menu(app, "/a")
    assert _names(m) == ["ask", "agent", "alpha-cmd", "rename", "plan", "beta-cmd"], \
        f"prefix matches in menu order, then substring ones: {_names(m)}"
    _type(app, "/")
    assert "research" not in _names(_wait_menu(app, "/")), "the agent dropped it"
    _type(app, "")


def test_a_six_hundred_row_catalog_renders_whole(app, rig, slashed):
    """**No row cap** (plan 029 P16, P24): the agent re-advertises 600
    commands — a live catalog delta, which craze does not cap — and the menu
    lists every one of them after the builtins, the panel capped in height and
    scrolling (↑ from the first row wraps the highlight to the 607th). The
    agent's two-command catalog is put back for the cells after."""
    host = slashed
    _menu_ready(app, host)
    names = [f"cmd-{i:03}" for i in range(600)]
    _lane(app, "lane.send", host, text="SET-COMMANDS " + ",".join(names))
    app.wait_until(lambda: len(_catalog(app, host)) == 606, timeout=WAIT,
                   what="the 600-command catalog on the lane")
    _type(app, "/")
    app.wait_until(lambda: len((_menu(app) or {}).get("rows", [])) == 606, timeout=20,
                   what="the menu listing all 606 rows")
    m = _menu(app)
    assert _names(m)[:6] == MENU_ROWS[:-1] and _names(m)[6:] == names, _names(m)[:8]
    _key(app, "ArrowUp")
    app.wait_until(lambda: _highlighted(_menu(app)) == "cmd-599", timeout=20,
                   what="the highlight wrapped to the last row")
    _shot(app, "craze-slash-600.png")
    _type(app, "")
    _lane(app, "lane.send", host, text="SET-COMMANDS alpha-cmd,beta-cmd")
    app.wait_until(lambda: len(_catalog(app, host)) == 8, timeout=WAIT, what="the two-command catalog back")


def test_a_hidden_builtin_typed_is_refused_and_nothing_is_sent(app, rig, slashed):
    """**A builtin the menu hides is never sent as text** (plan 029 P5, E1):
    `/help` (a `local` row, hidden in phase 1) typed and sent is refused
    INLINE with the shared core's reason, the draft kept — and neither the
    hub nor the agent ever sees it. So is ` /help` (E9: read as it would be
    sent)."""
    host = slashed
    _menu_ready(app, host)
    before = len(rig.verbs("session.prompt"))
    for typed in ["/help", " /help"]:
        _type(app, typed)
        _key(app, "Enter")
        app.wait_until(lambda: (_menu(app) or {}).get("refusal") == "/help: not available here yet",
                       timeout=20, what=f"{typed!r} refused inline")
        assert _draft(app) == typed, "the draft is kept"
        if typed == "/help":
            _shot(app, "craze-slash-refused.png")
    _settle()
    assert len(rig.verbs("session.prompt")) == before, f"nothing was sent: {rig.verbs('session.prompt')[before:]}"
    assert not any("/help" in t for t in _texts(app, host)), "the agent never saw it"
    _type(app, "")
    app.wait_until(lambda: (_menu(app) or {}).get("refusal") is None, timeout=20,
                   what="an edit to clear the refusal")


def test_re_enter_accepts_then_enter_renames_via_the_delta(app, rig, slashed):
    """**`/re` + Enter completes, then Enter renames** (plan 029 P9, P19): the
    first Enter ACCEPTS the highlighted `rename` (the token does not spell it
    yet) — the draft becomes `/rename ` and nothing is sent; with a title
    typed, the second Enter runs it: ONE `session.setTitle` carrying exactly
    the trimmed argument, and the header shows the new title once craze's
    delta says so (the lane's session row). The draft clears, no note."""
    host = slashed
    _menu_ready(app, host)
    before = len(rig.verbs("session.setTitle"))
    _type(app, "/re")
    m = _wait_menu(app, "/re")
    assert _highlighted(m) == "rename", m["rows"]
    _key(app, "Enter")
    app.wait_until(lambda: _draft(app) == "/rename ", timeout=20, what="Enter to complete the token")
    _settle(0.5)
    assert len(rig.verbs("session.setTitle")) == before, "accepting sends nothing"
    _wait_menu(app, "/rename", open_=False)

    _type(app, "/rename   plan 029 desk ")
    _key(app, "Enter")
    app.wait_until(lambda: (_messages(app, host)["session"] or {}).get("title") == "plan 029 desk",
                   timeout=WAIT, what="the new title on the lane's session row (the delta)")
    app.wait_until(lambda: (_dump(app) or {}).get("title") == "plan 029 desk", timeout=20,
                   what="the panel header's new title")
    sent = rig.verbs("session.setTitle")[before:]
    assert [v["params"]["title"] for v in sent] == ["plan 029 desk"], sent
    app.wait_until(lambda: _draft(app) == "", timeout=20, what="the draft cleared on success")
    m = _menu(app)
    assert m["refusal"] is None and m["warning"] is None, m


def test_a_rename_that_lands_late_leaves_a_newer_draft_alone(app, rig, slashed):
    """**An earlier command's outcome never erases a newer draft** (plan 029
    §3.5, P8 — draft revision ownership): `/rename held late` + Enter goes
    out and is HELD on the wire (the proxy's `hold-titles` gate), pending,
    the composer still editable. A new draft typed meanwhile SURVIVES the
    rename's success: the rename lands — one `session.setTitle` with exactly
    its title, the lane's row titled by craze's delta — and the composer
    still holds the newer text, no note."""
    host = slashed
    _menu_ready(app, host)
    before = len(rig.verbs("session.setTitle"))
    newer = "a newer draft, typed while the rename was pending"
    title = lambda: (_messages(app, host)["session"] or {}).get("title")  # noqa: E731
    with SetGate(rig.root / "hold-titles", what="rename") as gate:
        _type(app, "/rename held late")
        _key(app, "Enter")
        app.wait_until(lambda: len(rig.verbs("session.setTitle")) > before, timeout=WAIT,
                       what="the rename on the wire, held")
        _type(app, newer)
        _settle(0.5)
        assert title() != "held late", "the rename is still pending"
        assert _draft(app) == newer
        gate.release()
        app.wait_until(lambda: title() == "held late", timeout=WAIT,
                       what="the held rename landing (craze's delta)")
    # The rename's own answer reaches the panel beside its delta; give it the
    # beat a negative assertion needs.
    _settle()
    assert _draft(app) == newer, "the rename's success erased the draft typed while it was pending"
    m = _menu(app)
    assert m["refusal"] is None and m["warning"] is None, m
    sent = rig.verbs("session.setTitle")[before:]
    assert [v["params"]["title"] for v in sent] == ["held late"], sent
    _type(app, "")


def test_ime_composition_and_shift_enter_send_nothing(app, rig, slashed):
    """**Every menu key is ignored while an IME composes, and Shift+Enter is
    a newline** (plan 029 P9, P28): with the menu open, a composing Enter,
    Tab, Esc or ↓ changes nothing; real ↑/↓ move the highlight (wrapping)
    instead of the caret; Shift+Enter inserts a newline (the menu closes on a
    multi-line draft) and sends nothing. A composing Enter on plain text sends
    nothing either — the composer's own Enter-to-send got the same guard —
    and a real Enter then sends it."""
    host = slashed
    _menu_ready(app, host)
    titles, prompts = len(rig.verbs("session.setTitle")), len(rig.verbs("session.prompt"))
    _type(app, "/")
    m = _wait_menu(app, "/")
    first, rows = _highlighted(m), _names(m)
    for key in ("Enter", "Tab", "Escape", "ArrowDown"):
        _key(app, key, composing=True)
    _settle(0.5)
    m = _menu(app)
    assert m["open"] is True and _draft(app) == "/" and _highlighted(m) == first, m
    _key(app, "ArrowDown")
    app.wait_until(lambda: _highlighted(_menu(app)) == rows[1], timeout=20, what="↓ to move the highlight")
    _key(app, "ArrowUp")
    _key(app, "ArrowUp")
    app.wait_until(lambda: _highlighted(_menu(app)) == rows[-1], timeout=20, what="↑ to wrap to the last row")
    assert _draft(app) == "/", "the arrows moved the highlight, not the text"

    _key(app, "Enter", shift=True)
    app.wait_until(lambda: _draft(app) == "/\n", timeout=20, what="Shift+Enter's newline")
    app.wait_until(lambda: (_menu(app) or {}).get("open") is False, timeout=20,
                   what="the menu closed on a multi-line draft")

    _type(app, "hello ime")
    _key(app, "Enter", composing=True)
    _settle()
    assert _draft(app) == "hello ime", "a composing Enter does not send"
    assert len(rig.verbs("session.prompt")) == prompts and len(rig.verbs("session.setTitle")) == titles
    _key(app, "Enter")
    app.wait_until(lambda: any(v["params"]["text"] == "hello ime" for v in rig.verbs("session.prompt")[prompts:]),
                   timeout=WAIT, what="the real Enter to send the text")
    app.wait_until(lambda: "echo: hello ime" in _texts(app, host), timeout=WAIT, what="the agent's echo")
    app.wait_until(lambda: _draft(app) == "", timeout=20, what="the sent draft cleared")


def test_double_escape_closes_the_menu_then_nothing_else(app, rig, slashed):
    """**Esc ordering, with real key events** (plan 029 P27, P9): the first
    Esc closes ONLY the menu — the draft, the panel and any open dialog stay
    — and the menu stays dismissed while the token is the same; the second
    Esc does nothing to the panel. With the craze create sheet open over it,
    the first Esc still closes only the menu (the sheet's Esc handler ignores
    an Esc the composer handled) and the second, no longer the composer's,
    closes the sheet — the panel untouched. A new token reopens the menu."""
    host = slashed
    _menu_ready(app, host)
    prompts = len(rig.verbs("session.prompt"))
    _type(app, "/")
    _wait_menu(app, "/")
    before = _dump(app)
    _key(app, "Escape")
    app.wait_until(lambda: (_menu(app) or {}).get("open") is False, timeout=20,
                   what="the first Esc to close the menu")
    after = _dump(app)
    assert after["session_id"] == host and after["draft"] == "/", after
    assert after["stop"] == before["stop"] and _settings(app) is None
    _key(app, "Escape")
    _settle(0.5)
    again = _dump(app)
    assert again is not None and again["session_id"] == host, "the panel stays"
    assert again["draft"] == "/" and again["slash_menu"]["open"] is False, again["slash_menu"]
    assert again["stop"] == before["stop"] and _settings(app) is None
    _type(app, "/r")
    _wait_menu(app, "/r")

    # With a dialog open over the panel.
    _type(app, "")
    _open_sheet(app)
    _type(app, "/")
    _wait_menu(app, "/")
    _key(app, "Escape")
    app.wait_until(lambda: (_menu(app) or {}).get("open") is False, timeout=20,
                   what="Esc to close the menu under the open sheet")
    _settle(0.5)
    assert _sheet(app) is not None, "the composer's Esc did not also close the sheet"
    _key(app, "Escape")
    app.wait_until(lambda: _sheet(app) is None, timeout=20, what="the second Esc to close the sheet")
    assert (_dump(app) or {}).get("session_id") == host and _draft(app) == "/", "the panel untouched"
    assert len(rig.verbs("session.prompt")) == prompts, "no Esc sent anything"
    _type(app, "")


def test_a_mode_runs_from_the_composer(app, rig, permodel):
    """**Modes from the composer** (plan 029 P7): `/ask` + Enter (the token
    spells the row, so Enter runs it) and a click on `/plan` each send ONE
    `session.set` mode change and the session moves — no settings sheet
    involved; re-selecting the current mode still sends. The draft clears."""
    host = permodel["host"]
    _menu_ready(app, host)
    mode = lambda: (_messages(app, host).get("settings") or {}).get("mode")  # noqa: E731
    sent = lambda: [e["params"]["setting"] for e in rig.sets()  # noqa: E731
                    if e["params"]["setting"].get("kind") == "mode"]
    before = len(sent())
    _type(app, "/ask")
    _wait_menu(app, "/ask")
    _key(app, "Enter")
    app.wait_until(lambda: mode() == "ask", timeout=WAIT, what="the session in ask mode")
    app.wait_until(lambda: _draft(app) == "", timeout=20, what="the draft cleared")
    _type(app, "/pl")
    m = _wait_menu(app, "/pl")
    assert "plan" in _names(m), m
    app.call("ui.pick_slash_row", {"name": "plan"})
    app.wait_until(lambda: mode() == "plan", timeout=WAIT, what="the session in plan mode")
    app.wait_until(lambda: _draft(app) == "", timeout=20, what="the draft cleared")
    _type(app, "/plan")
    _wait_menu(app, "/plan")
    _key(app, "Enter")
    app.wait_until(lambda: len(sent()) == before + 3, timeout=WAIT, what="the re-selected mode sent")
    assert sent()[before:] == [{"kind": "mode", "value": "ask"}, {"kind": "mode", "value": "plan"},
                               {"kind": "mode", "value": "plan"}], sent()[before:]
    assert _settings(app) is None, "no sheet was opened for it"


def test_model_opens_the_sheet_and_the_model_changes(app, rig, permodel):
    """**`/model` opens the picker** (plan 029 X2, §3.5): Enter on `/model`
    clears the draft and opens the settings sheet; a model pressed there
    changes the session's model end to end (`session.set` on the wire, the
    session's settings). `/model <id>` is refused inline (no inline model in
    phase 1), nothing sent."""
    host = permodel["host"]
    _menu_ready(app, host)
    before = len(rig.sets())
    prompts = len(rig.verbs("session.prompt"))
    _type(app, "/model composer-2.5")
    _key(app, "Enter")
    app.wait_until(lambda: (_menu(app) or {}).get("refusal")
                   == "/model: takes no argument — pick a model from the list",
                   timeout=20, what="an argument to /model refused")
    assert _draft(app) == "/model composer-2.5", "the refused draft is kept to fix"
    _settle()
    assert len(rig.sets()) == before and _settings(app) is None
    assert rig.verbs("session.prompt")[prompts:] == [], \
        f"nothing went out as text: {rig.verbs('session.prompt')[prompts:]}"
    assert _draft(app) == "/model composer-2.5", "still kept"
    _type(app, "/model")
    _wait_menu(app, "/model")
    _key(app, "Enter")
    app.wait_until(lambda: bool((d := _settings(app)) and d["session_id"] == host), timeout=WAIT,
                   what="the settings sheet, opened by /model")
    assert _draft(app) == "", "the picker clears the draft"
    _press(app, "model", "composer-2.5")
    app.wait_until(lambda: (_messages(app, host).get("settings") or {}).get("model") == "composer-2.5",
                   timeout=WAIT, what="the session on composer-2.5")
    assert {"kind": "model", "value": "composer-2.5"} in [e["params"]["setting"] for e in rig.sets()[before:]]
    _press(app, "model", "grok-4.6")
    app.wait_until(lambda: _row_of(app, "model").get("current") == "grok-4.6", timeout=WAIT,
                   what="the session back on grok-4.6")
    app.call("ui.close_lane_settings")
    app.wait_until(lambda: _settings(app) is None, timeout=20, what="the sheet to close")


@pytest.fixture(scope="module")
def held(app, rig) -> dict:
    """A hub-created session whose turns after the first HOLD until the
    harness writes a byte (`craze-fake-agent -script sigint-hold` with
    `CRAZE_FAKE_GATE` a FIFO; cursor's dialect, so it runs as `cursor` — grok
    stays the default and the config is restored at once), recording every
    prompt it reads (`CRAZE_FAKE_DUMP_PROMPTS`). Its first prompt is answered
    at the create. Answers `{host, gate, prompts}`, `gate` a `SetGate`."""
    prompts = rig.root / "hold-prompts.jsonl"
    with SetGate(rig.root / "hold-gate", what="turn") as gate:
        agent = _write_exec(rig.root / "hold-agent", (
            "#!/bin/sh\n"
            f"export CRAZE_FAKE_GATE='{gate.path}'\n"
            f"export CRAZE_FAKE_DUMP_PROMPTS='{prompts}'\n"
            f"exec '{rig.real / 'craze-fake-agent'}' -script sigint-hold \"$@\"\n"))
        rig.set_agents(grok=rig.grok_echo, cursor=agent)
        try:
            host = _hub_create(rig, "shedtest-held-1", "w-held", provider="cursor", prompt="warm up")
        finally:
            rig.set_grok(rig.grok_echo)
        app.wait_until(lambda: _row(app, host) is not None, timeout=WAIT, what="the held session's craze row")
        rig.switch("proxy", True)
        yield {"host": host, "gate": gate, "prompts": prompts}


def _agent_read(prompts: Path, text: str) -> bool:
    """Whether the agent has READ a prompt carrying `text`."""
    return prompts.exists() and any(text in line for line in prompts.read_text().splitlines())


def test_while_a_turn_runs_rows_dim_and_a_prompt_row_queues(app, rig, held):
    """**While working** (plan 029 X7/P29, P23, P26): with a turn held open,
    the rows that wait for the turn are listed DIMMED ("after this turn") —
    `rename`, `exit` and the agent's prompt row are not — and a click on a
    dimmed `/plan` is refused inline, no `session.set` sent. A PROMPT row
    queues: `/research deep dive` + Enter goes out as one queued
    `session.prompt` of the row's own spelling, the draft clears, and the
    agent does not read it while the turn holds; released, the agent reads
    the queued text and answers it.

    **The highlight starts on the first live row** (plan 030 FU8, §3.4): a
    `/` opened mid-turn highlights `rename` — the first row the turn does
    not refuse — under a dimmed `model`, and Enter right away COMPLETES
    `/rename ` (it takes a title): no refusal, nothing sent."""
    host = held["host"]
    _menu_ready(app, host)
    _lane(app, "lane.send", host, text="hold this turn")
    app.wait_until(lambda: _messages(app, host)["activity"] == "working", timeout=WAIT,
                   what="the held turn running")
    app.wait_until(lambda: _agent_read(held["prompts"], "hold this turn"), timeout=WAIT,
                   what="the agent holding the turn")
    # The PANEL's own view working (what the menu's first answer for `/` is
    # filtered with) — the lane's alone could still be ahead of it.
    app.wait_until(lambda: (_dump(app) or {}).get("activity") == "working", timeout=20,
                   what="the panel showing the turn")
    titles, prompts, sets = len(rig.verbs("session.setTitle")), len(rig.verbs("session.prompt")), len(rig.sets())
    _type(app, "/")
    m = _wait_menu(app, "/")
    assert _names(m) == MENU_ROWS, _names(m)
    assert m["rows"][0]["name"] == "model" and m["rows"][0]["dimmed"] is True, \
        f"the first row is dimmed (not vacuous): {m['rows']}"
    assert _highlighted(m) == "rename", f"the highlight starts on the first live row: {m['rows']}"
    _key(app, "Enter")
    app.wait_until(lambda: _draft(app) == "/rename ", timeout=20, what="Enter to complete /rename")
    m = _menu(app)
    assert m["refusal"] is None and m["warning"] is None, m
    _settle(0.5)
    assert (len(rig.verbs("session.setTitle")), len(rig.verbs("session.prompt")), len(rig.sets())) \
        == (titles, prompts, sets), "completing sends nothing"
    _type(app, "")

    _type(app, "/")
    app.wait_until(lambda: any(r["dimmed"] for r in (_menu(app) or {}).get("rows", [])), timeout=20,
                   what="the waiting rows dimmed")
    m = _wait_menu(app, "/")
    assert {r["name"] for r in m["rows"] if r["dimmed"]} == WAITING_ROWS, m["rows"]
    assert _names(m) == MENU_ROWS, _names(m)
    _shot(app, "craze-slash-dimmed.png")

    sets = len(rig.sets())
    app.call("ui.pick_slash_row", {"name": "plan"})
    app.wait_until(lambda: (_menu(app) or {}).get("refusal") == "/plan: runs after this turn",
                   timeout=20, what="the dimmed row refused")
    assert _draft(app) == "/", "the draft kept"
    _settle(0.5)
    assert len(rig.sets()) == sets, "nothing sent"

    prompts = len(rig.verbs("session.prompt"))
    _type(app, "/Research   deep dive ")
    _key(app, "Enter")
    app.wait_until(lambda: len(rig.verbs("session.prompt")) > prompts, timeout=WAIT,
                   what="the prompt row on the wire")
    sent = rig.verbs("session.prompt")[prompts:]
    assert [(v["params"]["text"], v["params"]["mode"]) for v in sent] == [("/research deep dive", "queue")], sent
    app.wait_until(lambda: _draft(app) == "", timeout=20, what="the queued prompt's draft cleared")
    _settle()
    assert not _agent_read(held["prompts"], "/research deep dive"), "queued: the agent has not read it"
    assert _messages(app, host)["activity"] == "working"

    held["gate"].release()  # the held turn ends; craze sends what it queued
    app.wait_until(lambda: _agent_read(held["prompts"], "/research deep dive"), timeout=WAIT,
                   what="the queued prompt reaching the agent")
    held["gate"].release()  # and its own turn (held too) ends
    app.wait_until(lambda: any("/research deep dive" in t and t.startswith("echo") for t in _texts(app, host)),
                   timeout=WAIT, what="the agent's answer to the queued prompt")
    app.wait_until(lambda: _messages(app, host)["activity"] != "working", timeout=WAIT,
                   what="the session idle again")
    _type(app, "")


def _plan_mode(app: TauriClient, host: str) -> str | None:
    """The mode id behind the resolved menu's `plan` row (`lane.messages`)."""
    for r in (_messages(app, host).get("slash_menu") or {}).get("rows", []):
        if r["name"] == "plan":
            return ((r.get("action") or {}).get("change") or {}).get("id")
    return None


def _idle(app: TauriClient, host: str) -> None:
    """The session idle — on the lane, and in the panel's own view."""
    app.wait_until(lambda: _messages(app, host)["activity"] == "idle", timeout=WAIT,
                   what="the session idle")
    app.wait_until(lambda: (_dump(app) or {}).get("activity") == "idle", timeout=20,
                   what="the panel showing it idle")


def test_a_working_flip_keeps_an_arrowed_to_row(app, rig, held):
    """**A working flip alone moves no highlight** (plan 030 §3.4): `/`
    opened idle and ↓↓ to `plan` (a row that waits for the turn); a turn
    starts under the open menu (`lane.send` never touches the composer) and
    `plan` dims — the highlight STAYS on it: Enter is refused "runs after
    this turn", nothing sent, and Tab still completes `/plan ` for after
    the turn (X7)."""
    host = held["host"]
    _menu_ready(app, host)
    _idle(app, host)
    _type(app, "/")
    m = _wait_menu(app, "/")
    assert _names(m) == MENU_ROWS and not any(r["dimmed"] for r in m["rows"]), m["rows"]
    assert _highlighted(m) == "model", m["rows"]
    _key(app, "ArrowDown")
    _key(app, "ArrowDown")
    app.wait_until(lambda: _highlighted(_menu(app)) == "plan", timeout=20, what="↓↓ to plan")

    sets, prompts = len(rig.sets()), len(rig.verbs("session.prompt"))
    _lane(app, "lane.send", host, text="hold the flip")
    app.wait_until(lambda: _agent_read(held["prompts"], "hold the flip"), timeout=WAIT,
                   what="the agent holding the turn")
    app.wait_until(lambda: {r["name"] for r in (_menu(app) or {}).get("rows", []) if r["dimmed"]} == WAITING_ROWS,
                   timeout=20, what="the waiting rows dimmed under the open menu")
    m = _menu(app)
    assert _highlighted(m) == "plan", f"the working flip moved the highlight: {m['rows']}"
    _key(app, "Enter")
    app.wait_until(lambda: (_menu(app) or {}).get("refusal") == "/plan: runs after this turn",
                   timeout=20, what="Enter on the dimmed row refused")
    assert _draft(app) == "/", "the draft kept"
    _key(app, "Tab")
    app.wait_until(lambda: _draft(app) == "/plan ", timeout=20, what="Tab to complete /plan")
    _settle(0.5)
    assert len(rig.sets()) == sets, f"no mode was sent: {rig.sets()[sets:]}"
    assert [v["params"]["text"] for v in rig.verbs("session.prompt")[prompts:]] == ["hold the flip"], \
        "nothing but the held turn's own prompt"

    held["gate"].release()
    _idle(app, host)
    _type(app, "")


def test_a_catalog_swap_under_the_same_names_resets_the_highlight(app, rig, held):
    """**A new catalog resets the highlight, even under the same names**
    (plan 029 P9; plan 030 FU7, §3.5): with `/` open and the highlight
    arrowed off the first row to `plan`, the session's catalog is replaced by
    one with the SAME rows in the same order and another action behind
    `plan` (another mode id) — and the highlight goes back to where a new
    filter starts, its `start`. The swap: the agent re-advertises
    `research,help` — `help` is craze's own builtin, so the catalog comes out
    the same, and craze restates it anyway because the commands moved — and
    the proxy's `swap-plan` rewrites `plan`'s mode id in that section as it
    comes down. That prompt is a held turn, so the new catalog is filtered
    WORKING: its `start` is `rename`, the first row the turn does not refuse.

    Red-first on the base (71914ad, `start` not yet there, so "the first
    row"): GREEN — FU7 does not reproduce on the screen. The panel replaces
    its menu only on a value change, and a filter answered for the replaced
    menu is no longer shown (by identity), so the render between them
    re-keys the highlight. Kept as the regression pin; the highlight's key
    now names the catalog outright."""
    host = held["host"]
    _menu_ready(app, host)
    _idle(app, host)
    before = _plan_mode(app, host)
    assert before, "the plan row sets a mode"
    swapped = f"{before}-swapped"
    _type(app, "/")
    m = _wait_menu(app, "/")
    assert _names(m) == MENU_ROWS and not any(r["dimmed"] for r in m["rows"]), m["rows"]
    _key(app, "ArrowDown")
    _key(app, "ArrowDown")
    app.wait_until(lambda: _highlighted(_menu(app)) == "plan", timeout=20, what="↓↓ to plan")

    rig.switch("swap-plan", True)
    try:
        _lane(app, "lane.send", host, text="SET-COMMANDS research,help")
        app.wait_until(lambda: _plan_mode(app, host) == swapped, timeout=WAIT,
                       what="the swapped catalog on the lane")
    finally:
        rig.switch("swap-plan", False)
    assert _catalog(app, host) == MENU_ROWS, "the same rows, in the same order"
    app.wait_until(lambda: _highlighted(_menu(app)) == "rename", timeout=20,
                   what="the highlight back where the new filter starts (its first live row)")
    m = _menu(app)
    assert _names(m) == MENU_ROWS and m["open"] is True and _draft(app) == "/", m
    assert m["rows"][0]["dimmed"] is True, f"filtered working: `model` dimmed above `rename`: {m['rows']}"

    # The turn ends; the agent's catalog goes back to `research` alone (the
    # commands moved again, so craze restates the section, unswapped).
    app.wait_until(lambda: _agent_read(held["prompts"], "SET-COMMANDS research,help"), timeout=WAIT,
                   what="the agent holding the swap's turn")
    held["gate"].release()
    _idle(app, host)
    _lane(app, "lane.send", host, text="SET-COMMANDS research")
    app.wait_until(lambda: _plan_mode(app, host) == before, timeout=WAIT, what="the catalog restored")
    held["gate"].release()
    _idle(app, host)
    _type(app, "")


def test_exit_asks_first_and_keep_sends_nothing(app, rig):
    """**`/exit` stops the session, behind the confirm** (plan 029 §3.5, P26),
    on a session of its OWN: Enter on `/exit` opens Stop's confirm with the
    draft kept; Keep closes it and sends NOTHING (no `session.stop`), the
    draft still `/exit`; Enter again and Stop session sends one
    `session.stop` — the session ends and its row leaves — and the draft
    clears."""
    host = _hub_session(app, rig, "shedtest-exit-1", "w-exit")
    _menu_ready(app, host)
    before = len(rig.verbs("session.stop"))
    _type(app, "/exit")
    _wait_menu(app, "/exit")
    _key(app, "Enter")
    app.wait_until(lambda: (_dump(app) or {}).get("stop") == {"confirming": True}, timeout=20,
                   what="Stop's confirm, opened by /exit")
    assert _draft(app) == "/exit"
    app.call("ui.answer_lane_stop", {"stop": False})
    app.wait_until(lambda: (_dump(app) or {}).get("stop") == {"confirming": False}, timeout=20,
                   what="Keep to close the confirm")
    _settle()
    assert len(rig.verbs("session.stop")) == before, "Keep sent nothing"
    assert _draft(app) == "/exit", "and kept the draft"
    assert _row(app, host) is not None, "the session runs on"

    _wait_menu(app, "/exit")
    _key(app, "Enter")
    app.wait_until(lambda: (_dump(app) or {}).get("stop") == {"confirming": True}, timeout=20,
                   what="the confirm again")
    app.call("ui.answer_lane_stop", {"stop": True})
    app.wait_until(lambda: _row(app, host) is None, timeout=WAIT, what="the stopped session's row to leave")
    assert len(rig.verbs("session.stop")) == before + 1, rig.verbs("session.stop")[before:]
    app.wait_until(lambda: _draft(app) == "", timeout=20, what="the draft cleared once the stop took")
    _unmount(app)


def test_a_name_at_create_titles_the_session_from_its_first_frame(app, rig):
    """**"Name (optional)" at create** (plan 029 D14, P26): the sheet offers
    the field on this hub (it takes a title). A name craze normalizes to
    nothing is refused `bad_request` and shown UNDER the field, no id held,
    nothing listed; a real name goes out on the create (the wire carries
    it), and the session is titled from its FIRST frame — never untitled and
    renamed after.

    The first frame is asserted on the WIRE, on what the bridge proxy
    recorded coming down it before the app read it: the hub's answer to the
    create — the session's row — already carries the name, and so does the
    snapshot of the lane connection's FIRST attach. No poll of the app's
    state can prove that (an untitled seed renamed before the first read
    passes any poll). The app's own row, the lane's seeded view and the
    transcript header are then checked to carry the name too — eventual
    checks of what the app shows. One session in that directory."""
    name = "plan 029 desk name"
    cwd = _mkdir_0700(rig.root / "w-named")
    rig.switch("proxy", True)
    before = len(rig.creates())
    replied = len(rig.create_replies())
    d = _open_sheet(app)
    assert d["title_field"] is True, "this hub takes a title"
    _fill(app, cwd=str(cwd), title="\u200b\u202e", prompt="")
    _submit_when_ready(app)
    app.wait_until(lambda: (_sheet(app) or {}).get("state") == "refused", timeout=WAIT,
                   what="craze's refusal of a blank-once-normalized name")
    d = _sheet(app)
    assert d["error"]["code"] == "bad_request" and d["error"]["where"] == "title", d["error"]
    assert d["title_problem"] == "params.title is blank once normalized: send none instead", d
    assert d["cwd_problem"] is None and d["request_id"] is None, d
    assert _rows_in(app, cwd) == [], "the refused create listed nothing"

    _fill(app, title=name)
    _submit_when_ready(app)
    # The FIRST frame, on the wire: the hub's answer to the create — the
    # session's row, recorded before the app read it — already carries the
    # name.
    app.wait_until(lambda: len(rig.create_replies()) - replied >= 2, timeout=WAIT,
                   what="the hub's answer to the titled create, on the wire")
    sent = rig.creates()[before:]
    assert [c.get("title") for c in sent] == ["\u200b\u202e", name], sent
    replies = rig.create_replies()[replied:]
    assert [r["requestId"] for r in replies] == [c["requestId"] for c in sent], (sent, replies)
    refused, created = replies
    assert ((refused["error"] or {}).get("data") or {}).get("code") == "bad_request", refused
    assert created["error"] is None and created["relayed"], created
    answer = created["session"]
    assert (answer.get("row") or {}).get("title") == name, \
        f"the create's own answer — the session's row — is already titled: {answer}"
    host = answer["hostId"]

    # What the app shows — EVENTUAL checks (`_poll`), not the first frame.
    shown = _poll(lambda: next(iter(_rows_in(app, cwd)), None), "the new session's row in rc.list")
    assert shown["slug"] == host, (shown, answer)
    assert shown["display_name"] == name, f"the session's row is titled: {shown}"
    seeded = _poll(lambda: v if (v := _messages(app, host))["generation"] >= 1 else None,
                   "the lane's seeded view")
    assert seeded["generation"] == 1, seeded["generation"]
    assert (seeded["session"] or {}).get("title") == name, \
        f"the lane's seeded view is titled: {seeded['session']}"

    # The FIRST frame of the lane, on the wire: its connection's first attach
    # for this session (recorded before the lane could seed from it) is a
    # fresh one, answered with a snapshot that already carries the name.
    first = next((a for a in rig.attaches() if a["sessionId"] == answer["sessionId"] and a["error"] is None),
                 None)
    assert first is not None, f"the lane's attach reply is on record: {rig.attaches()}"
    assert first["hostId"] == host and first["relayed"], first
    assert first["cursor"] is False and first["snapshot"] is True, \
        f"the lane's first attach is a fresh one, answered with a snapshot: {first}"
    assert first["title"] == name, f"the lane's FIRST attach snapshot is already titled: {first}"

    app.wait_until(lambda: _sheet(app) is None, timeout=WAIT, what="the sheet to close on Created")
    header = _poll(lambda: r if (r := _dump(app)) and r["session_id"] == host and r["generation"] >= 1 else None,
                   "the transcript's seeded report")
    assert header["lane_kind"] == KIND and header["title"] == name, \
        f"the transcript header: {header['title']!r}"
    row = _exactly_one_session_in(app, cwd)
    assert row["slug"] == host and row["display_name"] == name, row
    _unmount(app)


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

    * `-O exit` is a recorded no-op (each one's control path is appended to
      `exits.log`), held while the root's `wedge-exit` switch exists — a
      wedged master (plan 026 §3.3); `-O check` finds no master (there is no
      master process here); a remote `true` (roost's warm-up) succeeds;
      roost's `client-bridge` chain is answered `command not found` (these
      machines run no roost-session — they are craze hosts).
    * **A craze command naming an absolute ladder rung is REFUSED and
      recorded, never run** — the production ladder would reach this host's
      own craze there, and the remote dials must be the jailed composition.
      (roost's own bootstrap probe, which these machines also get, is not a
      craze command and is left alone.)
    * Everything else runs as `/bin/sh -c "$cmd"` under exactly the jail's
      `HOME`, `CRAZE_HOME`, `CRAZE_RUNTIME_DIR` and `PATH` (its own `bin`,
      which holds `sh` beside `craze`). A craze command (one `'sh' '-c' '…'`:
      the ladder, which `exec`s craze) is EXEC'd, so the process this fake ssh is
      becomes the craze process itself: its recorded pid (`pids/<record>`) is
      the bridge, and signalling it ends the connection the way a dropped ssh
      would (plan 026 §3.2).
    * Two switches in the JAIL's root (plan 026 §3.2): `probe-127` makes the
      find-only probe exit 127 (the ladder's not-found) before running
      anything; `hold-roster` (its content a number N) holds the (N+1)-th
      `bridge --hub` of that jail — recording its record's name in `held` —
      until the switch is removed, and lets every other one through.
    """
    return f"""#!/bin/sh
set -u
root='{root}'
ctl=
want=0
prev=
is_exit=0
is_check=0
dest=
cmd=
for arg in "$@"; do
    if [ "$want" -eq 1 ]; then ctl="$arg"; want=0; fi
    if [ "$arg" = "-S" ]; then want=1; fi
    if [ "$prev" = "-O" ] && [ "$arg" = "exit" ]; then is_exit=1; fi
    if [ "$prev" = "-O" ] && [ "$arg" = "check" ]; then is_check=1; fi
    case "$arg" in
        *" "*) ;;
        *@*) dest="$arg" ;;
    esac
    prev="$arg"
    cmd="$arg"
done
if [ "$is_exit" -eq 1 ]; then
    printf 'exit %s\n' "$ctl" >> "$root/exits.log"
    while [ -e "$root/wedge-exit" ]; do sleep 0.1; done
    if [ -n "$ctl" ]; then rm -f "$ctl"; fi
    exit 0
fi
if [ "$is_check" -eq 1 ]; then
    printf 'Control socket connect(%s): No such file or directory\n' "$ctl" >&2
    exit 255
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
printf '%s' "$$" > "$root/pids/${{rec##*/}}"
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
case "$cmd" in
  *"providers --hub --json"*)
    if [ -e "$jail/probe-127" ]; then
        printf '%s\\n' 'sh: 1: craze: not found' >&2
        exit 127
    fi
    ;;
  *"bridge --hub"*)
    if [ -e "$jail/hold-roster" ]; then
        n=$(grep -l -F 'bridge --hub' "$root/cmds/$user".* 2>/dev/null | wc -l)
        pass=$(cat "$jail/hold-roster")
        if [ "$n" -eq $((pass + 1)) ]; then
            printf '%s' "${{rec##*/}}" > "$jail/held"
            while [ -e "$jail/hold-roster" ]; do sleep 0.1; done
        fi
    fi
    ;;
esac
case "$cmd" in
  "'sh' '-c' "*craze*|"sh -c "*craze*) exec env -i HOME="$jail/h" CRAZE_HOME="$jail/ch" CRAZE_RUNTIME_DIR="$jail/r" \\
      PATH="$jail/bin" /bin/sh -c "exec $cmd" ;;
esac
exec env -i HOME="$jail/h" CRAZE_HOME="$jail/ch" CRAZE_RUNTIME_DIR="$jail/r" \\
    PATH="$jail/bin" /bin/sh -c "$cmd"
"""


#: The remote machines' jails: `(user, kind)` — `kind` as `CrazeEnv` takes it.
MACHINE_JAILS = (("craze-dormant", "recipe"), ("craze-live", "recipe"), ("craze-old", "old"))


class Record:
    """One remote command the fake ssh recorded: its record's name, its text,
    the pid the fake ssh ran as, and when it was recorded."""

    def __init__(self, path: Path, pids: Path):
        self.name = path.name
        self.text = path.read_text()
        pid = pids / path.name
        self.pid = int(pid.read_text()) if pid.exists() and pid.read_text().isdigit() else 0
        self.at = path.stat().st_mtime_ns

    def __repr__(self) -> str:
        return f"Record({self.name}, pid={self.pid}, {self.text[-40:]!r})"


class Remote:
    """The remote module rig: a fake `ssh`, and one jail per machine (or shed:
    a shed's ssh user is its name) — each a `CrazeEnv` whose `bin` also holds
    `sh` (the remote command is `sh -c …`)."""

    def __init__(self, bins: Path, jails=MACHINE_JAILS, prefix: str = "shcr-"):
        self.root = Path(tempfile.mkdtemp(prefix=prefix, dir="/tmp")).resolve()
        _mkdir_0700(self.root / "cmds")
        _mkdir_0700(self.root / "pids")
        _mkdir_0700(self.root / "jails")
        self.jails: dict[str, CrazeEnv] = {}
        for name, kind in jails:
            jail = self.root / "jails" / name
            env = CrazeEnv(bins, _mkdir_0700(jail), jail / "h", kind=kind)
            os.symlink("/bin/sh", env.path_dir / "sh")
            self.jails[name] = env
        # The dormant jail's sentinel: an absolute rung under ITS HOME.
        self.sentinel_log = self.root / "sentinel.log"
        if "craze-dormant" in self.jails:
            _write_exec(self.jails["craze-dormant"].home / ".nix-profile" / "bin" / "craze",
                        f"#!/bin/sh\necho ran >> '{self.sentinel_log}'\nexit 2\n")
        self.ssh = _write_exec(self.root / "ssh", _fake_ssh(self.root))

    def commands(self, user: str) -> list[str]:
        return [p.read_text() for p in sorted((self.root / "cmds").glob(f"{user}.*"))]

    def records(self, user: str) -> list[Record]:
        """Every command `user`'s jail was sent, in the order recorded."""
        recs = [Record(p, self.root / "pids") for p in (self.root / "cmds").glob(f"{user}.*")]
        return sorted(recs, key=lambda r: (r.at, r.name))

    def live_bridges(self, user: str) -> list[int]:
        """The recorded pids of `user`'s `bridge --hub` commands that still run
        that jail's craze — checked against its program path, so a reused pid
        is never one of them."""
        real = str(self.jails[user].real)
        return [r.pid for r in self.records(user)
                if "bridge --hub" in r.text and r.pid > 1 and _program_args(r.pid).startswith(real)]

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
    and attaches at once rather than at its next 30 s dormant probe. And the
    sheet's "Name (optional)" (plan 029 P10): absent from the dormant status
    (`create_title: false`, as `create_options`), the field appears on the
    open sheet once the woken hub is live and says it takes a title.

    LAST of the remote machines' cells: craze-dormant is no longer dormant
    after it."""
    machine = "craze-dormant"
    jail = remote.jails[machine]
    app_remote.wait_until(lambda: _craze_state(app_remote, machine).get("state") == "dormant",
                          timeout=WAIT, what="craze-dormant dormant")
    assert not jail.hub_pids()
    # Plan 029 P10: a dormant machine's status says it cannot name a session
    # at create — no hub has said so yet — exactly as it says no
    # `create_options`; its sheet offers "Name (optional)" once the hub is live.
    dormant = _craze_state(app_remote, machine)
    assert dormant.get("create_title") is False and dormant.get("create_options") is False, dormant
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
    assert _craze_state(app_remote, machine).get("create_title") is True, "craze 0.1.1's hub takes a title"
    # The open sheet grows its Name field once the hub is live (P10).
    app_remote.wait_until(lambda: (_sheet(app_remote) or {}).get("title_field") is True, timeout=WAIT,
                          what="the Name field on the now-live machine's sheet")
    assert "Name" in _sheet(app_remote)["values"], _sheet(app_remote)["values"]
    _shot(app_remote, "craze-create-dormant.png")
    _close_sheet(app_remote)


# ---------------------------------------------------------------------------
# (6) a running shed's craze: offer, don't list (plan 026 §3.2, shed#399)
# ---------------------------------------------------------------------------

SERVER = "mock"
#: The sheds the shed instance's private mock lists running, each with its own
#: jail (a shed's ssh user is its name) — so each cell's craze is its own:
#: `czoffer` is created on, `czold` runs the real v0.0.1, `czprobe` has its
#: probe switched, `czstop` is stopped.
SHED_JAILS = (("czoffer", "recipe"), ("czold", "old"), ("czprobe", "recipe"), ("czstop", "recipe"))
TOO_OLD_NOTE = "craze on this machine is too old for shed; update it"


def _shed_target(name: str) -> str:
    return f"roost:{SERVER}/{name}"


def _running_shed(name: str) -> dict:
    return {"name": name, "status": "running", "backend": "vz",
            "created_at": "2026-10-09T12:00:00Z", "started_at": "2026-10-09T12:00:01Z"}


@pytest.fixture(scope="module")
def shed_rig(bins):
    """The fake ssh and one jail per running shed."""
    r = Remote(bins, jails=SHED_JAILS, prefix="shcs-")
    try:
        yield r
    finally:
        for env in r.jails.values():
            for switch in ("hold-roster", "probe-127"):
                (env.root / switch).unlink(missing_ok=True)
        left = r.teardown()
        assert not left, f"craze processes left behind: {left}"


@pytest.fixture(scope="module")
def shed_mock():
    """A private mock shed-server listing the cells' sheds running — the
    session mock's sheds (and its per-test reset) never reach this instance."""
    m = MockShedServer()
    m.set_sheds([_running_shed(name) for name, _ in SHED_JAILS])
    m.start()
    try:
        yield m
    finally:
        m.stop()


@pytest.fixture(scope="module")
def app_shed(shed_rig, shed_mock):
    """No local craze, no machines (`config-craze-shed.yaml`): one server whose
    running sheds reach their jails through the fake ssh — so each shed has an
    attach-only craze source, and no roost-session to list it."""
    runtime_dir = Path(tempfile.mkdtemp(prefix="shsh-", dir="/tmp")).resolve()
    started = _launch(shed_mock, runtime_dir=runtime_dir, craze=None, roost=None,
                      ssh_bin=shed_rig.ssh, config=FIXTURES / "config-craze-shed.yaml")
    try:
        yield started[0]
    finally:
        _stop(*started)
        shutil.rmtree(runtime_dir, ignore_errors=True)


def _offered(app: TauriClient, name: str) -> dict | None:
    """The shed's `rc.list` `shed_craze` entry, or None."""
    return next((e for e in app.call("rc.list").get("shed_craze") or []
                 if e["origin"] == _shed_target(name)), None)


def _offered_as(app: TauriClient, name: str) -> dict:
    return (_offered(app, name) or {}).get("craze") or {}


def _listed(app: TauriClient, name: str) -> bool:
    """Whether the shed is a LISTED host: a status row of `rc.list`."""
    return any(m["origin"] == _shed_target(name) for m in app.call("rc.list")["machines"])


def _shed_row(app: TauriClient, name: str) -> dict | None:
    """The running shed's Sheds-pane row as rendered (`shed_roost.dump`)."""
    return (app.call("shed_roost.dump")["sheds"] or {}).get(_shed_target(name))


def _where(app: TauriClient) -> str:
    """The New-session dialog's rendered text — its "Where" choices among it —
    or "" while none is open."""
    return (app.call("launch.dump")["launch"] or {}).get("rendered") or ""


def _where_offers(app: TauriClient, name: str) -> bool:
    return f"{_shed_target(name)} — new craze session" in _where(app)


def _open_launch(app: TauriClient) -> None:
    app.call("ui.show_launch")
    app.wait_until(lambda: bool(app.call("launch.dump")["launch"]), timeout=20,
                   what="the launch dialog")


def _close_launch(app: TauriClient) -> None:
    app.call("ui.close_launch")
    app.wait_until(lambda: app.call("launch.dump")["launch"] is None, timeout=20,
                   what="the launch dialog to close")


def _shed_craze_commands(rig: Remote, name: str) -> list[str]:
    return [r.text for r in rig.records(name) if "craze" in r.text]


def _offered_on_its_row(app: TauriClient, name: str) -> None:
    """Wait for the dormant shed's entry, then for its Sheds-pane row's New
    craze session (the pane on screen)."""
    app.wait_until(lambda: _offered_as(app, name).get("state") == "dormant", timeout=WAIT,
                   what=f"{name} offered as dormant")
    app.navigate("sheds")
    app.wait_until(lambda: (_shed_row(app, name) or {}).get("craze_create") is True,
                   timeout=20, what=f"{name}'s row to offer New craze session")


def test_a_dormant_shed_offers_craze_on_its_row(app_shed, shed_rig):
    """**A dormant shed is offered craze, not listed** (plan 026 §3.2, shed#399).
    A running shed whose find-only probe says no hub runs there offers New
    craze session on its Sheds-pane row and in the New-session dialog's
    "Where", and is NOT a listed host (`rc.list.machines` lacks it); until the
    click, the fake ssh carried only the probe.

    The click opens the sheet with its options loaded — and with no note,
    where reading the status rows alone would say "… has no craze" — while
    the roster's `Ready` is HELD BACK (the fake ssh holds the roster's
    `bridge --hub`, the second, behind the sheet's), so the shed is still
    unlisted and only its `shed_craze` entry says what its craze is: Create
    is enabled. A create then lists the shed, and its card — no roost there —
    reads `craze only`, in the sidebar too."""
    app = app_shed
    name = "czoffer"
    target = _shed_target(name)
    jail = shed_rig.jails[name]
    _offered_on_its_row(app, name)
    assert not _listed(app, name), "a dormant shed is not a listed host"
    assert _shed_row(app, name)["craze_note"] is None
    _open_launch(app)
    try:
        app.wait_until(lambda: _where_offers(app, name), timeout=20,
                       what="Where to offer the dormant shed")
    finally:
        _close_launch(app)
    _shot(app, "craze-shed-offer.png")
    cmds = _shed_craze_commands(shed_rig, name)
    assert cmds and all("providers --hub --json" in c for c in cmds), \
        f"before the click, the find-only probe and nothing else: {cmds}"
    assert not jail.hub_pids(), "no hub before the click"

    (jail.root / "hold-roster").write_text("1")
    try:
        app.call("ui.show_craze_create", {"machine": target})
        seen: list[dict] = []

        def loaded() -> bool:
            d = _sheet(app)
            if not (d and d["machine"] == target):
                return False
            seen.append(d)
            return d["state"] != "loading"

        app.wait_until(loaded, timeout=WAIT, what="the sheet's options")
        d = seen[-1]
        assert d["state"] == "idle", d
        assert [p["id"] for p in d["providers"]] == ["cursor", "grok", "native"], d["providers"]
        # The roster has dialled, and is held: still unlisted, still dormant.
        app.wait_until(lambda: (jail.root / "held").exists(), timeout=WAIT,
                       what="the roster's bridge --hub to be held")
        assert not _listed(app, name), "the roster's Ready is held back: the shed is unlisted"
        assert _offered_as(app, name).get("state") == "dormant"
        d = _fill(app, cwd=str(jail.work))
        assert d["create_enabled"] is True, f"Create is disabled before the roster's Ready: {d}"
        assert d["note"] is None, d
        assert all(d["note"] is None for d in seen), \
            f"the sheet read the unlisted shed as without craze: {[d['note'] for d in seen]}"
        # The first bridge --hub is the sheet's: none ran before the click, no
        # hub ran (a probe-gated roster dial cannot run one without a hub), and
        # the second — the one held — is the roster's.
        bridges = [r for r in shed_rig.records(name) if "bridge --hub" in r.text]
        held = (jail.root / "held").read_text().strip()
        assert len(bridges) >= 2 and bridges[0].name != held and bridges[1].name == held, \
            (bridges, held)
        _shot(app, "craze-shed-sheet.png")

        _submit_when_ready(app)
        app.wait_until(lambda: _sheet(app) is None, timeout=WAIT, what="the sheet to close on Created")
        app.wait_until(lambda: _listed(app, name), timeout=WAIT, what="the shed listed by its create")
        assert _offered(app, name) is None, "listed now: no longer a shed_craze entry"
        app.navigate("machines")
        app.wait_until(lambda: any(m["origin"] == target for m in app.machines_dump() or []),
                       timeout=20, what="the shed's card in the Machines pane")
        card = next(m for m in app.machines_dump() if m["origin"] == target)
        assert card["status"] == "craze only", card
        assert card["detail"] == "no roost session here yet — craze sessions only", card
        bar = next(m for m in app.sidebar_dump()["machines"] if m["origin"] == target)
        assert (bar["status"], bar["note"], bar["tone"]) == ("craze only", "craze only", "neutral"), bar
        _shot(app, "craze-shed-craze-only.png")
    finally:
        (jail.root / "hold-roster").unlink(missing_ok=True)
        app.call("ui.close_craze_create")
        app.call("ui.close_lane")
    app.wait_until(lambda: _dump(app) is None, timeout=20, what="the transcript panel to unmount")


def test_a_too_old_shed_says_so_on_its_row(app_shed, shed_rig):
    """**A too-old shed says so on its row** (plan 026 §3.2): the real craze
    v0.0.1 in a running shed reads too old — its `shed_craze` entry — and its
    Sheds-pane row shows the note and no New craze session; "Where" offers it
    nothing; it is not listed, and nothing but the probe reached it."""
    app = app_shed
    name = "czold"
    app.wait_until(lambda: _offered_as(app, name).get("cause") == "too_old", timeout=WAIT,
                   what=f"{name} read as too old")
    assert not _listed(app, name), "a too-old shed is not a listed host"
    app.navigate("sheds")
    app.wait_until(lambda: (_shed_row(app, name) or {}).get("craze_note"), timeout=20,
                   what="the too-old shed's row note")
    row = _shed_row(app, name)
    assert row["craze_note"] == TOO_OLD_NOTE, row
    assert row["craze_create"] is False, "too old: no create entry"
    _open_launch(app)
    try:
        # The dialog has rendered the running sheds — this one among them —
        # before the absence of its craze choice means anything.
        app.wait_until(lambda: f"{SERVER}/{name}" in _where(app), timeout=20,
                       what="the dialog's shed targets")
        assert not _where_offers(app, name), _where(app)
    finally:
        _close_launch(app)
    cmds = _shed_craze_commands(shed_rig, name)
    assert cmds and all("providers --hub --json" in c for c in cmds), cmds
    _shot(app, "craze-shed-too-old.png")


def test_a_dormant_shed_offer_follows_its_probe(app_shed, shed_rig):
    """**The offer follows the probe** (plan 026 §3.2), with the sheet closed —
    so the source stays dormant and keeps probing: a probe that exits 127 (not
    installed) takes the row's New craze session away and "Where" drops it,
    and a probe that says no hub again brings both back, each on the UI's own
    re-read. No `bridge --hub` ran.

    An OPEN sheet births the hub and the roster attaches, so the loop stops
    probing; the sheet's half is therefore its own: with the probe switched
    to exit 127, the roster's bridge is killed (by the pid the fake ssh
    recorded), the source probes, and the open sheet shows the note and
    disables Create."""
    app = app_shed
    name = "czprobe"
    target = _shed_target(name)
    jail = shed_rig.jails[name]
    probe_127 = jail.root / "probe-127"
    app.wait_until(lambda: _offered_as(app, name).get("state") == "dormant", timeout=WAIT,
                   what=f"{name} offered as dormant")
    app.navigate("sheds")
    # The dialog re-reads `rc.list` as it opens, so the offer below is on screen
    # whatever repainted before; what follows is the probe's own repaints.
    _open_launch(app)
    try:
        app.wait_until(lambda: (_shed_row(app, name) or {}).get("craze_create") is True
                       and _where_offers(app, name), timeout=20,
                       what="its row and Where to offer it")
        probe_127.touch()
        # The next dormant probe (every 30 s) is the one that exits 127.
        app.wait_until(lambda: (_shed_row(app, name) or {}).get("craze_create") is False
                       and not _where_offers(app, name),
                       timeout=45, what="the offer to go once the probe exits 127")
        assert _offered(app, name) is None, "not installed: no entry"
        probe_127.unlink()
        # Not installed re-probes every 30 s too.
        app.wait_until(lambda: (_shed_row(app, name) or {}).get("craze_create") is True
                       and _where_offers(app, name),
                       timeout=45, what="the offer back once the probe says no hub")
    finally:
        probe_127.unlink(missing_ok=True)
        _close_launch(app)
    assert not any("bridge --hub" in c for c in shed_rig.commands(name)), \
        "the sheet was closed: nothing dialled bridge --hub"

    d = _open_sheet(app, target)
    assert d["state"] == "idle" and d["note"] is None, d
    app.wait_until(lambda: _listed(app, name) and (_status(app, target) or {}).get("craze", {})
                   .get("state") == "live", timeout=WAIT, what="the roster to attach")
    d = _fill(app, cwd=str(jail.work))
    assert d["create_enabled"] is True, d
    probe_127.touch()
    try:
        pids = shed_rig.live_bridges(name)
        assert pids, "the roster's bridge runs: " + repr(
            [(r.pid, _program_args(r.pid)) for r in shed_rig.records(name) if "bridge --hub" in r.text])
        for pid in pids:
            # Guarded twice: `live_bridges` matched the program path, and it
            # is read again just before the signal (a pid can be reused).
            if pid > 1 and _program_args(pid).startswith(str(jail.real)):
                os.kill(pid, signal.SIGTERM)
        want = f"craze is not installed on {target}"
        app.wait_until(lambda: (_sheet(app) or {}).get("note") == want, timeout=WAIT,
                       what="the open sheet to say craze is gone")
        d = _sheet(app)
        assert d["create_enabled"] is False, d
        assert d["values"].get("Directory") == str(jail.work), "the form is kept"
        _shot(app, "craze-shed-sheet-gone.png")
    finally:
        probe_127.unlink(missing_ok=True)
        _close_sheet(app)


def test_a_stopped_dormant_shed_leaves_where(app_shed, shed_rig, shed_mock):
    """**A stopped dormant shed leaves "Where"** (plan 026 §3.2): stopping it in
    the mock server takes away its row's offer and its "Where" choice on the
    app's own shed refresh — no refresh by hand: the removal of a shed that
    was only OFFERED (never listed) still tells the UI to re-read."""
    app = app_shed
    name = "czstop"
    _offered_on_its_row(app, name)
    _open_launch(app)
    try:
        app.wait_until(lambda: _where_offers(app, name), timeout=20, what="Where to offer it")
        _, sheds = shed_mock.snapshot()
        shed_mock.set_sheds([dict(s, status="stopped") if s["name"] == name else s
                             for s in sheds or []])
        app.wait_until(lambda: not _where_offers(app, name), timeout=WAIT,
                       what="Where to drop the stopped shed on the app's own refresh")
        app.wait_until(lambda: _shed_row(app, name) is None, timeout=20,
                       what="the stopped shed's row to offer nothing")
    finally:
        _close_launch(app)
    assert _offered(app, name) is None, "a stopped shed is no entry"
    assert not any("bridge --hub" in c for c in shed_rig.commands(name))


# ---------------------------------------------------------------------------
# (7) the orderly shutdown, from every exit path (plan 026 §3.3, shed#389)
# ---------------------------------------------------------------------------

#: The signal cells' remote machine: a live hub there (the harness's own
#: bridge holds it, started BEFORE launch), reached through the fake ssh.
SIG_MACHINE = "craze-sig"
SIG_FAKE_HOST = "0e0e0e0e0e0e"
SIG_CONFIG = f"""servers:
    mock:
        host: 127.0.0.1
        http_port: 8080
        ssh_port: 2222
default_server: mock
sheds: {{}}
machines:
    {SIG_MACHINE}:
        host: craze-host
        user: {SIG_MACHINE}
"""


def _ps_table() -> dict[int, tuple[int, str]]:
    """pid → (ppid, args), every process — `ps`, POSIX, no `/proc`."""
    out = subprocess.run(["ps", "-A", "-ww", "-o", "pid=,ppid=,args="],
                         capture_output=True, text=True, check=False)
    table: dict[int, tuple[int, str]] = {}
    for line in out.stdout.splitlines():
        parts = line.split(None, 2)
        if len(parts) >= 2 and parts[0].isdigit() and parts[1].isdigit():
            table[int(parts[0])] = (int(parts[1]), parts[2] if len(parts) > 2 else "")
    return table


def _descendants(root: int, table: dict[int, tuple[int, str]] | None = None) -> dict[int, str]:
    """Every process descended from `root` (a ppid walk), `root` excluded: pid → args."""
    table = table if table is not None else _ps_table()
    kids: dict[int, list[int]] = {}
    for pid, (ppid, _) in table.items():
        kids.setdefault(ppid, []).append(pid)
    found: dict[int, str] = {}
    todo = list(kids.get(root, []))
    while todo:
        pid = todo.pop()
        if pid in found or pid in (0, 1, root):
            continue
        found[pid] = table[pid][1]
        todo.extend(kids.get(pid, []))
    return found


def _pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


class SigRig:
    """One signal cell's world (plan 026 §3.3) — everything its OWN app
    instance needs, never the module's shared `app`: this machine's craze
    (the recording seam over the real binary, its own HOME and hub), and one
    remote machine through the fake ssh with a live hub of its own."""

    def __init__(self, bins: Path, *, wedge: bool = False):
        self.root = Path(tempfile.mkdtemp(prefix="shsg-", dir="/tmp")).resolve()
        self.started = None
        self.holder = None
        self.local = None
        self.remote = None
        try:
            # 0700 all the way down: craze refuses a runtime dir beneath a
            # group-writable ancestor.
            local = _mkdir_0700(self.root / "local")
            self.local = CrazeEnv(bins, local, local / "h")
            self.remote = Remote(bins, jails=((SIG_MACHINE, "recipe"),), prefix="shsr-")
            live = self.remote.jails[SIG_MACHINE]
            live.fake_host(SIG_FAKE_HOST, "craze-sig-fake")
            self.holder = live.bridge()
            assert self.holder.call("hello", HELLO)["endpoint"]["kind"] == "hub"
            self.local.fake_host(FAKE_HOST, FAKE_SESSION)
            self.config = self.root / "config.yaml"
            self.config.write_text(SIG_CONFIG)
            if wedge:
                (self.remote.root / "wedge-exit").touch()
        except BaseException:
            # A rig that failed halfway still owns what it started. The
            # original failure is the one reported.
            try:
                self.teardown()
            except BaseException:
                pass
            raise

    def launch(self, mock):
        """An app instance of this rig's own: its HOME is the local namespace's."""
        self.started = _launch(mock, runtime_dir=self.local.home, craze=self.local, roost=None,
                               ssh_bin=self.remote.ssh, config=self.config)
        return self.started

    def exits(self) -> list[str]:
        log = self.remote.root / "exits.log"
        return log.read_text().splitlines() if log.exists() else []

    def teardown(self) -> list[int]:
        """Every step runs, whatever an earlier one raised (plan 026 §3.3, F9):
        an app that needed the harness's SIGKILL is reported only once the
        holder is closed and both namespaces are swept — the first failure is
        re-raised at the end, with any craze process left behind printed."""
        failures: list[BaseException] = []
        left: list[int] = []

        def step(do):
            try:
                do()
            except BaseException as failure:  # re-raised below, once all ran
                failures.append(failure)

        if self.remote is not None:
            step(lambda: (self.remote.root / "wedge-exit").unlink(missing_ok=True))
        if self.started is not None:
            started, self.started = self.started, None
            step(lambda: _stop(*started))
        if self.holder is not None:
            step(self.holder.close)
        if self.local is not None:
            step(lambda: left.extend(self.local.teardown()))
        if self.remote is not None:
            step(lambda: left.extend(self.remote.teardown()))
        shutil.rmtree(self.root, ignore_errors=True)
        if failures:
            if left:
                print(f"\n[craze] signal rig: craze processes left behind: {left}")
            raise failures[0]
        return left


@pytest.fixture
def sig_rig(bins):
    """A FUNCTION-scoped rig (plan 026 §3.3): each signal cell signals an app
    instance of its own, the way `test_tauri_downhost.py`'s fixture does —
    signalling the module's shared `app` would end every later cell's app."""
    rig = SigRig(bins)
    try:
        yield rig
    finally:
        left = rig.teardown()
        assert not left, f"craze processes left behind: {left}"


@pytest.fixture
def wedged_rig(bins):
    """A signal cell's rig whose masters WEDGE on `-O exit` (the fake ssh holds
    it while its `wedge-exit` switch exists)."""
    rig = SigRig(bins, wedge=True)
    try:
        yield rig
    finally:
        left = rig.teardown()
        assert not left, f"craze processes left behind: {left}"


def _remote_lane(app: TauriClient, host_id: str) -> None:
    """Open the remote machine's craze lane on `host_id` and wait for its seed."""
    args = {"machine": SIG_MACHINE, "kind": KIND, "session_id": host_id}
    app.call("lane.open", args)
    app.wait_until(lambda: app.call("lane.messages", args)["generation"] >= 1, timeout=WAIT,
                   what=f"the seeded remote craze lane on {host_id}")


def _set_up_children(app: TauriClient, rig: SigRig) -> str:
    """Every child class the harness can reach (plan 026 §3.3's table): this
    machine's roster and an open local lane (class C, local craze bridges), a
    session CREATED through the app (its `craze serve` is the hub's, so it must
    outlive the app), and the remote machine's roster and an open lane over the
    fake ssh, with that host's `SshExec` master (class B). Answers the created
    session's hostId."""
    app.wait_until(lambda: _row(app, FAKE_HOST) is not None, timeout=WAIT,
                   what="this machine's craze row")
    cwd = _mkdir_0700(rig.local.root / "w-sig")
    created = app.call("craze.create", {"machine": LOCAL, "cwd": str(cwd),
                                        "prompt": "hello quit"})
    host = created["host_id"]
    app.wait_until(lambda: _row(app, host) is not None, timeout=WAIT,
                   what="the created session's row")
    _ready(app, FAKE_HOST)
    _ready(app, host)
    app.wait_until(lambda: _row(app, SIG_FAKE_HOST, machine=SIG_MACHINE) is not None,
                   timeout=WAIT, what="the remote machine's craze row")
    _remote_lane(app, SIG_FAKE_HOST)
    assert rig.remote.live_bridges(SIG_MACHINE), "the remote lane's ssh bridge runs"
    return host


def _app_owned(app_pid: int, rig: SigRig) -> tuple[dict[int, str], set[int]]:
    """The app's DESCENDANT tree, recorded by walking ppids from its pid — and,
    listed apart, the persistent processes it does not own: this machine's
    craze hub (by its own record) and everything under it (its session hosts
    and their agents). A hub is the bridge's child until that bridge exits, so
    it sits inside the app's tree while the app runs, and must be told apart."""
    table = _ps_table()
    tree = _descendants(app_pid, table)
    persistent: set[int] = set()
    for hub in rig.local.hub_pids():
        if hub in table and table[hub][1].startswith(str(rig.local.real)):
            persistent.add(hub)
            persistent.update(_descendants(hub, table))
    owned = {pid: args for pid, args in tree.items() if pid not in persistent}
    return owned, persistent


def _quit(app: TauriClient, proc: subprocess.Popen, how: str) -> None:
    if how in ("app.quit", "app.terminate"):
        app.call(how)
    else:
        os.kill(proc.pid, getattr(signal, how))


def _quit_leaves_no_app_children(rig: SigRig, mock, how: str) -> str:
    """One exit path, end to end (plan 026 §3.3 and AM7). An app instance of
    the rig's own is set up with a local craze lane (jailed), a session created
    through it, and a remote fake-ssh machine with an open lane and its
    `SshExec` master; its descendant tree is recorded; then it is quit by `how`
    ([`_quit`]). Then:

    * it exits by itself within 5 s (the harness's kill is never used), with
      status 0;
    * every recorded app-owned descendant is dead — checked before any sweep;
    * the fake ssh logged `-O exit` for the remote host's `SshExec` master;
    * the orderly shutdown ran to its end;
    * the craze hub and the created session still run, and `craze ps` lists
      the session.

    Answers the created session's hostId."""
    app, proc, _log_fh, _agent = rig.launch(mock)
    host = _set_up_children(app, rig)
    owned, persistent = _app_owned(proc.pid, rig)
    hubs = [p for p in rig.local.hub_pids() if p in persistent]
    assert hubs, "this machine's hub was recorded among the persistent processes"
    bridges = [pid for pid, args in owned.items() if "bridge" in args or "ssh" in args]
    assert bridges, f"the app's craze bridges were recorded among its children: {owned}"

    started = time.monotonic()
    _quit(app, proc, how)
    try:
        code = proc.wait(timeout=5)
    except subprocess.TimeoutExpired:
        pytest.fail(f"{how}: the app did not exit by itself within 5 s")
    took = time.monotonic() - started

    # Every recorded app-owned descendant: dead — checked before any sweep.
    # The shutdown kills AND awaits its own children before the app exits, so
    # they are gone already; the half second (F9: at most 0.5 s) is for what
    # the shutdown does not own — a WebKit helper noticing its parent went, a
    # killed child's zombie being reaped once it is reparented.
    deadline = time.monotonic() + 0.5
    while time.monotonic() < deadline and [p for p in owned if _pid_alive(p)]:
        time.sleep(0.05)
    survivors = {p: owned[p] for p in owned if _pid_alive(p)}
    assert not survivors, f"{how}: app-owned children outlived the app: {survivors}"
    # The child that WOULD outlive a hard kill: the remote host's `SshExec`
    # ControlMaster. ssh daemonizes it, so it is never in the descendant tree
    # above (the craze bridges and WebKit's helpers end with the app on their
    # own) — its proof is the `-O exit` the fake ssh logged.
    exits = rig.exits()
    assert any("shed-roost-x" in line for line in exits), \
        f"{how}: the remote host's SshExec master was never asked to -O exit: {exits}"

    assert code == 0, f"{how}: an orderly exit is a clean one (exit {code})"
    said = [line for line in (rig.local.home / "craze-ui.log").read_text(errors="replace").splitlines()
            if "shutdown" in line]
    assert any("shutdown: done" in line for line in said), \
        f"{how}: the orderly shutdown did not run: {said}"
    print(f"\n{how}: exited by itself in {took:.2f}s\n" + "\n".join(said))

    # The persistent processes the app does not own: still there.
    for hub in hubs:
        assert _pid_alive(hub) and _program_args(hub).startswith(str(rig.local.real)), \
            f"{how}: the craze hub {hub} did not survive the app"
    ps = subprocess.run([str(rig.local.real / "craze"), "ps", "--json"], env=rig.local.env(),
                        capture_output=True, text=True, timeout=30, cwd=str(rig.local.root))
    assert host in ps.stdout, f"{how}: craze ps no longer lists the session: {ps.stdout} {ps.stderr}"
    return host


@pytest.mark.parametrize("how", ["SIGTERM", "SIGINT", "SIGHUP", "app.quit"])
def test_sigterm_leaves_no_app_children(sig_rig, mock, how):
    """**Every exit path ends every child the app owns, and nothing it does
    not** (plan 026 §3.3, shed#389). The app is sent SIGTERM, SIGINT or SIGHUP
    by its RECORDED pid — or the test-mode `app.quit` door, the tray's own
    `app.exit(0)` — and [`_quit_leaves_no_app_children`] checks the exit: by
    itself within 5 s, every app-owned descendant dead, the remote `SshExec`
    master asked to `-O exit`, the craze hub and the created session still
    running. Then a relaunched app re-opens its lane."""
    rig = sig_rig
    host = _quit_leaves_no_app_children(rig, mock, how)

    # A relaunched app re-opens its lane (the first instance's own handles
    # released first: it has exited).
    first, rig.started = rig.started, None
    first[0].close()
    first[2].close()
    first[3].stop()
    app2, _proc2, _fh2, _agent2 = rig.launch(mock)
    app2.wait_until(lambda: _row(app2, host) is not None, timeout=WAIT,
                    what="the session's row after the relaunch")
    _ready(app2, host)


@pytest.mark.skipif(sys.platform != "darwin",
                    reason="app.terminate is the macOS system-quit path ([NSApplication terminate:])")
def test_a_macos_system_quit_leaves_no_app_children(sig_rig, mock):
    """**A macOS system quit ends every child the app owns, and nothing it
    does not** (plan 026 AM7). A Dock Quit, a logout and an AppleScript `quit`
    all take AppKit's `[NSApplication terminate:]`, which reaches the app ONLY
    as `RunEvent::Exit` (never `ExitRequested`), so only C3's `Exit` arm can run
    the orderly shutdown. The test-mode `app.terminate` door drives that path:
    an AppleScript quit sent from a harness raises a TCC Automation prompt.
    The same world and checks as `test_sigterm_leaves_no_app_children`'s
    `app.quit` case ([`_quit_leaves_no_app_children`]): the app exits by
    itself within 5 s with status 0, every recorded descendant is dead, the
    remote master got `-O exit`, and the hub and the session survive."""
    _quit_leaves_no_app_children(sig_rig, mock, "app.terminate")


def test_a_second_signal_exits_at_once(wedged_rig, mock):
    """**A second signal is the escape hatch** (plan 026 §3.3): with the remote
    masters' `-O exit` WEDGED (the fake ssh holds it), the first SIGTERM's
    orderly shutdown is held in its tunnels/masters phase; a second SIGTERM
    ends the app at once, with status 143 (128 + SIGTERM)."""
    rig = wedged_rig
    app, proc, _log_fh, _agent = rig.launch(mock)
    app.wait_until(lambda: _row(app, SIG_FAKE_HOST, machine=SIG_MACHINE) is not None,
                   timeout=WAIT, what="the remote machine's craze row")
    # The wedge is on from launch, so roost's own replace path may have logged
    # (and been held by) `-O exit`s already: wait for one the SHUTDOWN sends,
    # past its craze phase — and only then signal again. A second SIGTERM sent
    # before the signal thread took the first is coalesced with it by the OS
    # and tokio, which is no test of the escape.
    before = len(rig.exits())
    ui_log = rig.local.home / "craze-ui.log"
    os.kill(proc.pid, signal.SIGTERM)
    deadline = time.monotonic() + scaled_timeout(15)
    while not (len(rig.exits()) > before
               and "shutdown: phase 2" in ui_log.read_text(errors="replace")):
        assert proc.poll() is None, f"the app exited ({proc.returncode}) before any -O exit wedged"
        assert time.monotonic() < deadline, "the shutdown sent no -O exit to the wedged fake ssh"
        time.sleep(0.05)
    assert proc.poll() is None, "the shutdown is held by the wedge"
    second = time.monotonic()
    os.kill(proc.pid, signal.SIGTERM)
    code = proc.wait(timeout=5)
    took = time.monotonic() - second
    assert code == 143, f"a second SIGTERM exits with 128 + 15, not {code}"
    assert took < 1.5, f"the second signal took {took:.2f}s to end the app"

