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
  the row merge). The hub this app's eager source births is under the app's
  own HOME / `CRAZE_HOME` / a short `/tmp` `CRAZE_RUNTIME_DIR`.
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
import tempfile
import time
from pathlib import Path

import pytest

import ui
from client import ShedError, TauriClient, scaled_timeout
from fake_host_agent import FakeHostAgent
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
            # The recording wrapper: what the ladder found, run with what PATH.
            _write_exec(self.path_dir / "craze", (
                "#!/bin/sh\n"
                f"printf '%s\\t%s\\n' \"$PATH\" \"$*\" >> '{self.calls}'\n"
                f"exec '{self.real / 'craze'}' \"$@\"\n"))
        elif kind == "old":
            os.symlink(self.real / "craze-0.0.1", self.path_dir / "craze")

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
        run this namespace's private copy just before."""
        pids = self.hub_pids()
        assert pids, "no hub record"
        for pid in pids:
            args = _program_args(pid)
            assert args.startswith(str(self.real)), f"pid {pid} is not this rig's hub: {args!r}"
            os.kill(pid, signal.SIGTERM)

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
        self._next += 1
        rid = self._next
        self.proc.stdin.write((json.dumps({"jsonrpc": "2.0", "id": rid, "method": method,
                                           "params": params}) + "\n").encode())
        self.proc.stdin.flush()
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
    dormant = next(m for m in app_remote.machines_dump() if m["name"] == "craze-dormant")
    assert dormant.get("craze_note") is None, "dormant is not a problem to report"
    _shot(app_remote, "craze-machines.png")
