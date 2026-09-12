"""Putting a `roost-session` on a shed, from the Tauri app (plan 019 S5).
`--target tauri`.

**Hermetic, and the real transports.** Everything else in this harness that
touches roost uses `SHED_TAURI_ROOST_SOCKETS`, which swaps the SSH client-bridge
for a direct `LocalSession` on a Unix socket. That seam is perfect for reading a
session that is already there and **useless here**, because the subject of a
bootstrap is a host with NO session: the probe's three-way answer comes from
`ssh` exit codes and `ssh` stderr, and the install runs roost's own scripts
through a remote `/bin/sh -s`. A `LocalSession` has none of that.

So these cells keep the REAL stack — the real `SshBridge` (roost's `SshTunnel`,
its candidate ladder, its stderr classification), the real `SshExec` with its
private `ControlMaster`, the real sans-IO bootstrap machines — and replace only
the `ssh` binary, through the test-mode `SHED_TAURI_SSH_BIN` seam. The fake `ssh`
runs the remote command locally under a throwaway `$HOME` per shed, which is what
makes "a cold shed", "a stale binary", "a session somebody else is using" and "an
install that lands" all reachable with no remote host anywhere.

Two more knobs make that honest:

* `SHED_TAURI_ROOST_JAIL=1` turns on roost's own `jail_fs_root`, so the candidate
  ladder's ABSOLUTE rungs (`/usr/bin/roost-session`, the linuxbrew and nix paths)
  are prefixed with `${ROOST_BOOTSTRAP_FS_ROOT}` — expanded by the fake `ssh`,
  which points it at an empty directory. Without it a cold-host cell finds the
  developer's own protocol-2 `/usr/bin/roost-session` and reads `Mismatch` on a
  workstation and `Missing` in CI's container.
* `ROOST_SESSION_INSTALL_BIN` is roost's own override rung, and the bytes it
  names are a shell script that answers `identify`, `start` and `client-bridge`
  the way the real daemon does. `_fake_session` is deliberately the same
  stand-in `crates/shed-core/src/roost/bootstrap/tests.rs` uses, one language
  over — and `test_the_real_binary_is_installed_and_started_behind_the_fake_ssh`
  is where that claim is checked against the actual protocol-4 binary.

The far side of `client-bridge` is `fake_roost.FakeRoost`, which answers from
roost's own vendored wire vectors — so the `session.set_agent_hooks` these cells
assert reached the host cannot pass against a shape roost does not publish.

**The app instance is self-managed and INDEPENDENT** (the down-host suite's
pattern): its own throwaway `HOME`/`XDG_RUNTIME_DIR`, its own IPC socket, its own
fake host-agent, and it never touches `ui._state`. The session-scoped instance
keeps running beside it, which is what keeps the autouse `_reset_policy` /
`_reset_mock` fixtures — which reconnect to the session socket before every test
— working for the modules that come after this one.

**Every cell owns its own sheds.** A bootstrap writes to the far side, so a shed
shared between two cells would make the second one depend on the first having
run. `Rig.host` mints one on demand.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import tempfile
import time
from contextlib import contextmanager
from pathlib import Path

import pytest

import ui
from client import ShedError, TauriClient, scaled_timeout
from fake_host_agent import FakeHostAgent
from fake_roost import FakeRoost
from mockserver import MockShedServer

pytestmark = pytest.mark.skipif(
    os.environ.get("SHED_TEST_TARGET", "mac") != "tauri",
    reason="tauri-only: the mac app has no roost bootstrap surface",
)

FIXTURES = Path(__file__).resolve().parent / "fixtures"

#: The mock server's only configured server name (see `fixtures/config.yaml`).
SERVER = "mock"

#: The identity a protocol-4 `roost-session` prints. `libghostty_build` is
#: deliberately something shed could never have guessed: the compatibility gate
#: is the protocol number and only that (plan 019 §3.4).
IDENTITY_V4 = json.dumps(
    {
        "app_version": "0.0.19",
        "session_protocol": 4,
        "libghostty_build": "ghostty-f2d5758f6305867d+snapshot.v1",
    }
)
#: A protocol-2 `roost-session` — every released build today, which is why this
#: is the realistic stale incumbent.
IDENTITY_V2 = json.dumps(
    {
        "app_version": "0.0.19",
        "session_protocol": 2,
        "libghostty_build": "ghostty-older",
    }
)

#: The tools the far-side scripts may use. Symlinked into a directory of its own
#: which is the WHOLE `PATH` a script runs under — `jail_fs_root` cannot jail the
#: ladder's `command -v` rung (it resolves through `PATH`), so this is what jails
#: it. `$HOME/.local/bin` is deliberately absent, which is the ordinary shed case
#: (roost execs the absolute path) and is what makes the post-install PATH
#: warning fire. The list mirrors the Rust rig's.
UTILS = ("sh", "uname", "head", "tail", "mv", "mkdir", "rm", "tee", "chmod", "cat")


def _fake_session(identity: str) -> str:
    """A `roost-session` stand-in: one `sh` script answering the three
    subcommands the bootstrap and the bridge use.

    `identify` prints one JSON line (roost's `parse_identity_line`); `start`
    prints roost's readiness verdict and leaves the marker that makes the host
    reachable — the FAR side starting is what makes it so, not a flag this side
    sets; `client-bridge` is the byte pump, and it refuses with roost's own
    `client-bridge: no session` sentence until a start has happened, which is the
    difference between `NoSession` and `NotInstalled` on the wire.

    Every invocation is appended to `$HOME/.calls`, which is how pin P6's "never
    stopped, never restarted" is asserted as an absence rather than a hope.
    """
    return f"""#!/bin/sh
printf '%s\\n' "$1" >> "${{HOME}}/.calls"
case "$1" in
  identify)
    printf '%s\\n' '{identity}'
    ;;
  start)
    : > "${{HOME}}/.session-running"
    printf 'ready pid=%s\\n' "$$"
    ;;
  client-bridge)
    if [ ! -f "${{HOME}}/.session-running" ]; then
      printf '%s\\n' 'client-bridge: no session is listening' >&2
      exit 1
    fi
    exec python3 -c '
import os, socket, sys, threading
s = socket.socket(socket.AF_UNIX)
s.connect(sys.argv[1])
def up():
    try:
        while True:
            chunk = os.read(0, 65536)
            if not chunk:
                break
            s.sendall(chunk)
    except Exception:
        pass
    try:
        s.shutdown(socket.SHUT_WR)
    except Exception:
        pass
threading.Thread(target=up, daemon=True).start()
try:
    while True:
        chunk = s.recv(65536)
        if not chunk:
            break
        os.write(1, chunk)
except Exception:
    pass
' "${{SHED_FAKE_ROOST_SOCKET}}"
    ;;
  *)
    exit 2
    ;;
esac
"""


def _fake_ssh(root: Path) -> str:
    """A stand-in for `ssh` that runs the remote command **locally**, under the
    throwaway `$HOME` of the shed the destination names.

    ## Finding the destination

    The two stacks under test hand `ssh` two different argv shapes, and both are
    pinned by tests on their own side: roost's bridge ends `… -T
    ssh://<shed>@<host>:<port> <remote-command>` (`exec_argv`), and shed's
    `SshExec` ends `… -T [-p <port>] <shed>@<host> -- <remote-command>`. So the
    destination is found by what it LOOKS like — the last argument that carries
    an `@` and no whitespace — rather than by position, which is the one rule
    that holds for both and would not quietly pick up a remote command.

    The shed name is the ssh USER: `<shed>@<server host>` is a shed's ssh
    identity everywhere in shed (the terminal, RC, the phone), and it is what
    picks the jail here.

    ## The three behaviours

    Each one is a real thing `ssh` does that the code under test depends on:

    * **`-O exit` is a recorded no-op** that removes the control socket, the way
      a real master does on its way out. It is how `SshExec`'s teardown (and
      `RoostHosts::shutdown`) is observable at all.
    * **A remote command of exactly `true` succeeds**, because that is roost's
      tunnel warm-up (`establish_argv`). Its success while a later per-connection
      exec fails is what produces a LATE classification on the tunnel — the only
      way `Down.kind` can carry `no-session` (plan 019 §3.6).
    * **The candidate ladder is answered from the jail.** A remote command
      carrying `client-bridge` execs the shed's own
      `$HOME/.local/bin/roost-session`, or exits 127 with roost's own
      `command not found` sentence when there is none. That branch is emulated
      rather than run because the BRIDGE's exec chain is deliberately unjailed
      (shed pins `jail_fs_root: false` for it in production), and running it here
      would find the developer's own `/usr/bin/roost-session`.

    Everything else — roost's `/bin/sh -s` scripts, its `tee` stream, its
    `command -v` path check, its `start` — runs through `sh -c` with the narrow
    jail environment, exactly as the Rust rig runs it.
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
    printf 'exit\\t%s\\n' "$dest" >> "$root/ssh.log"
    if [ -n "$ctl" ]; then rm -f "$ctl"; fi
    exit 0
fi
if [ -n "$ctl" ] && [ ! -e "$ctl" ]; then : > "$ctl"; fi
user=${{dest##*://}}
user=${{user%@*}}
jail="$root/hosts/$user"
printf '%s\\t%s\\n' "$user" "$cmd" >> "$root/ssh.log"
if [ "$cmd" = "true" ]; then
    # The warm-up. It must succeed even for a host with no jail: a real `ssh`
    # that can reach a box succeeds here whatever is installed on it.
    exit 0
fi
if [ -z "$user" ] || [ ! -d "$jail" ]; then
    printf '%s\\n' "ssh: Could not resolve hostname $dest" >&2
    exit 255
fi
socket=
if [ -f "$jail/socket" ]; then socket=$(cat "$jail/socket"); fi
case "$cmd" in
  *client-bridge*)
    p="$jail/home/.local/bin/roost-session"
    if [ -x "$p" ]; then
        exec env -i HOME="$jail/home" \
            PATH="$root/utils:/usr/bin:/bin" \
            XDG_RUNTIME_DIR="$jail/home/run" \
            XDG_DATA_HOME="$jail/home/data" \
            XDG_STATE_HOME="$jail/home/state" \
            XDG_CACHE_HOME="$jail/home/cache" \
            SHELL=/bin/sh \
            ROOST_SHELL_FEATURES= \
            SHED_FAKE_ROOST_SOCKET="$socket" \
            "$p" client-bridge
    fi
    printf '%s\\n' 'roost-session: command not found' >&2
    exit 127
    ;;
esac
exec env -i HOME="$jail/home" \
    PATH="$root/utils" \
    ROOST_BOOTSTRAP_FS_ROOT="$root/fsroot" \
    XDG_RUNTIME_DIR="$jail/home/run" \
    XDG_DATA_HOME="$jail/home/data" \
    XDG_STATE_HOME="$jail/home/state" \
    XDG_CACHE_HOME="$jail/home/cache" \
    SHELL=/bin/sh \
    ROOST_SHELL_FEATURES= \
    SHED_FAKE_ROOST_SOCKET="$socket" \
    /bin/sh -c "$cmd"
"""


def _write_exec(path: Path, body: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(body)
    path.chmod(0o755)


def _wait_for(what: str, probe, timeout: float = 30.0):
    """Poll `probe` until it answers something truthy, and RETURN it.

    `TauriClient.wait_until` asserts a condition; a cell that needs the VALUE the
    condition was about (the `down_kind` that arrived, say) would otherwise have
    to re-read it afterwards and could read a newer one. Polling, never sleeping
    — the harness rule.
    """
    deadline = time.monotonic() + scaled_timeout(timeout)
    while True:
        value = probe()
        if value:
            return value
        if time.monotonic() >= deadline:
            raise AssertionError(f"timed out waiting for {what}")
        time.sleep(0.1)


class Rig:
    """The far side: a throwaway `$HOME` per shed, a fake `ssh`, and the two
    `FakeRoost` daemons an installed session pumps to — plus the mock server that
    has to agree the shed is running."""

    def __init__(self, root: Path, v4: FakeRoost, v2: FakeRoost, mock):
        self.root = root
        self.v4 = v4
        self.v2 = v2
        self.mock = mock
        self.hosts: list[str] = []
        self.extra: list[FakeRoost] = []
        #: The app whose view of the world [`list_running`] refreshes. Set (and
        #: restored) by `_boot_app` for whichever instance is driving the rig, so
        #: a cell with its OWN app refreshes that one and hands the module-scoped
        #: instance back on the way out.
        self.app: TauriClient | None = None

    def roost(self, *, protocol: int = 4) -> FakeRoost:
        """A `roost-session` daemon of a cell's OWN.

        Cells that assert about ROWS need one each: every shed pumps to whichever
        fake its jail names, and two sheds sharing a daemon share its tab list —
        so a row cell would see the other cell's tabs, and hanging one up
        (`close_all`) would disconnect an unrelated shed. Shut down with the
        module's rig.
        """
        fake = FakeRoost().start()
        fake.session_protocol = protocol
        self.extra.append(fake)
        return fake

    def agent_tab(self, fake: FakeRoost, tab_id: int = 7, *, kind: str = "codex") -> str:
        """Claim a tab on `fake` so it produces a durable session row.

        A `tab.open` alone does not: shed stamps an OPTIMISTIC row for the kind it
        asked for, and the next snapshot replaces the row set with the daemon's
        own inventory — where an unclaimed tab is somebody's terminal and
        deliberately not a card. An adapter claiming the tab is what makes the row
        survive, which is exactly how it works on a real host.
        """
        session_id = f"ses_{kind}_{tab_id}"
        fake.add_tab(
            tab_id,
            cwd="/home/shed/work",
            title=f"{kind} | work",
            source=kind,
            session_id=session_id,
            lifecycle="working",
            detail="session_status",
        )
        return session_id

    # -- minting a shed ----------------------------------------------------
    def host(
        self,
        shed: str,
        *,
        identity: str | None = None,
        running: bool = False,
        roost: FakeRoost | None = None,
    ) -> str:
        """Mint one shed's far side and answer with its grammar token.

        `identity` pre-installs a `roost-session` that identifies like that (an
        incumbent); `running` also leaves the marker, so its `client-bridge`
        pumps and `session.identify` answers. Nothing else about a shed is
        configured anywhere — its ssh identity is derived from the server entry
        (plan 019 §3.6), which is the whole point of that cut.

        It is also listed as RUNNING on the mock, because that is not optional:
        an authoritative refresh removes a roost host whose shed an answering
        server no longer lists, and the app's own frontend runs one on every
        `refresh` event. A shed the rig minted but the server never heard of
        would have its watcher torn down from under the cell.
        """
        home = self.home(shed)
        for name in ("run", "data", "state", "cache"):
            path = home / name
            path.mkdir(parents=True, exist_ok=True)
            # **0700, every level.** A real `roost-session` refuses to bind its
            # socket under a group- or world-writable ancestor without the sticky
            # bit (`roost_ipc::runtime_dir`), and a developer umask of 002 makes
            # every `mkdir` here group-writable — so the `real_roost` cell would
            # fail on a check that has nothing to do with what it is testing.
            for level in (path, path.parent, path.parent.parent):
                level.chmod(0o700)
        (self.root / "hosts" / shed / "socket").write_text(
            str((roost or self.v4).socket_path)
        )
        if identity is not None:
            _write_exec(self.installed(shed), _fake_session(identity))
        if running:
            assert identity is not None, "a session needs a binary to be running from"
            self.marker(shed).touch()
        if shed not in self.hosts:
            self.hosts.append(shed)
        self.list_running()
        return f"roost:{SERVER}/{shed}"

    def list_running(self, *, stopped: str | None = None) -> None:
        """Serve every minted shed as running, except `stopped`, and make the app
        take one authoritative look at the result.

        The mock's payload is restored before every test (the autouse
        `_reset_mock`), so this is how a cell's sheds get back into the listing —
        and how one of them is taken OUT of it, which is what `shed stop` looks
        like to a client.

        **The `sheds.list` is not optional.** A shed is only addressable once the
        app has seen a server LIST it (`RoostHosts::ssh_entry`'s gate — a
        configured server is not a licence for every shed name on it), and
        `sheds.list` is the authoritative refresh that records that. Without it a
        cell that mints a shed and immediately drives `roost.probe` would be
        racing the frontend's own 5 s shed poll, the only other thing that tells
        the app a new shed exists.
        """
        for shed in self.hosts:
            if shed != stopped:
                self.mock.add_shed({"name": shed, "status": "running", "backend": "vz"})
        if self.app is not None:
            self.app.call("sheds.list")

    # -- per-shed state a cell reads or pokes ------------------------------
    def home(self, shed: str) -> Path:
        return self.root / "hosts" / shed / "home"

    def installed(self, shed: str) -> Path:
        return self.home(shed) / ".local/bin/roost-session"

    def marker(self, shed: str) -> Path:
        """The file the fake session's `start` leaves — "something is serving"."""
        return self.home(shed) / ".session-running"

    def calls(self, shed: str) -> list[str]:
        """Every subcommand the shed's `roost-session` was invoked with."""
        path = self.home(shed) / ".calls"
        return path.read_text().split() if path.exists() else []

    def ssh_log(self) -> list[str]:
        log = self.root / "ssh.log"
        return log.read_text().splitlines() if log.exists() else []


@pytest.fixture(scope="module")
def rig(mock):
    """Build the far side. Module-scoped: the app is launched pointing at it."""
    # `mkdtemp` is 0700 already; `hosts/` is made here so it is too, because a
    # real `roost-session` validates every ancestor of its socket directory.
    root = Path(tempfile.mkdtemp(prefix="shed-boot-", dir="/tmp")).resolve()
    (root / "hosts").mkdir()
    (root / "hosts").chmod(0o700)
    v4 = FakeRoost().start()
    v2 = FakeRoost().start()
    # A protocol-2 DAEMON, which is what pin P6 is about: shed reports it and
    # never restarts it. `session_protocol` is a plain control on the fake —
    # roost keeps one `session.identify` vector per generation and shed vendors
    # only the current one.
    v2.session_protocol = 2
    rig = Rig(root, v4, v2, mock)
    try:
        (root / "fsroot").mkdir(parents=True, exist_ok=True)
        utils = root / "utils"
        utils.mkdir(parents=True, exist_ok=True)
        for tool in UTILS:
            real = shutil.which(tool)
            assert real, f"the rig needs {tool} on PATH"
            (utils / tool).symlink_to(real)
        # The bytes an install streams: roost's own override rung.
        _write_exec(root / "src/roost-session", _fake_session(IDENTITY_V4))
        _write_exec(root / "bin/fake-ssh", _fake_ssh(root))
        yield rig
    finally:
        for fake in [v4, v2, *rig.extra]:
            fake.shutdown()
        shutil.rmtree(root, ignore_errors=True)


#: A path that names no file, for `ROOST_SESSION_BIN`. See `_boot_app`.
ABSENT = Path("/nonexistent/roost-session")


@contextmanager
def _boot_app(
    rig: Rig,
    mock_base_url: str,
    *,
    install_bin: Path | None,
    session_bin: Path | None = None,
):
    """Launch an INDEPENDENT tauri instance pointed at `rig` — its own throwaway
    `HOME`/`XDG_RUNTIME_DIR`, its own socket, its own fake host-agent — and yield
    a ready client.

    Independent, never `ui.launch`, for the down-host suite's reason: a
    self-managed instance that replaced the session one would have to quit it,
    and the autouse policy fixture that runs before the NEXT test would then find
    nothing listening (or, worse, find the developer's own app).

    `roost_sockets` is deliberately EMPTY: these cells want the real bridge, and
    `ssh_bin` is what makes that hermetic — in test mode `build_ssh_reach`
    refuses to build a bridge for a host with neither a mapped socket nor a fake
    `ssh`.

    `session_bin` points roost's own `ROOST_SESSION_BIN` at [`ABSENT`] for the
    no-source cell: the source ladder's SIBLING rung borrows that variable, and
    without it the rung finds whatever `roost-session` is on the developer's PATH
    (this workstation has a protocol-2 one; CI has none) and the cell reads
    differently in the two places.
    """
    cfg = ui._SUBPROC["tauri"]
    if not cfg.binary.exists():
        raise RuntimeError(
            f"tauri binary not found at {cfg.binary}; build it first (make tauri-build)."
        )
    agent = FakeHostAgent()
    agent.start()
    try:
        runtime_dir = Path(tempfile.mkdtemp(prefix="shed-boot-app-"))
        sock = runtime_dir / cfg.sock_rel
        log = runtime_dir / "bootstrap-ui.log"
        env = ui.subproc_env(
            cfg,
            runtime_dir=runtime_dir,
            mock_base_url=mock_base_url,
            config_path=FIXTURES / "config.yaml",
            host_agent_socket=agent.socket_path,
            ssh_bin=rig.root / "bin/fake-ssh",
            roost_jail=True,
            roost_install_bin=install_bin,
            roost_session_bin=session_bin,
        )
        log_fh = open(log, "wb")
        proc = subprocess.Popen(
            [str(cfg.binary)], env=env, stdout=log_fh, stderr=subprocess.STDOUT
        )
        try:
            ui.await_hermetic(
                "tauri", sock=sock, mock_base_url=mock_base_url, proc=proc, log=log
            )
            client = TauriClient(sock)
            # This instance is the one `Rig.list_running` refreshes while it is
            # up — restored on the way out, so a cell with its own app hands the
            # module-scoped one back. See `Rig.app`.
            previous_app = rig.app
            rig.app = client
            try:
                client.wait_until(
                    lambda: client.current_pane() is not None,
                    timeout=30,
                    what="bootstrap frontend ready",
                )
                yield client
            finally:
                rig.app = previous_app
                client.close()
        finally:
            ui.terminate(proc)
            log_fh.close()
            shutil.rmtree(runtime_dir, ignore_errors=True)
    finally:
        agent.stop()


@pytest.fixture(scope="module")
def bootstrap_app(rig, mock):
    """The app instance every cell but the no-source and real-binary ones drive.

    Module-scoped so the `ssh` ControlMaster, the bridges and the lease table
    live across cells the way they do in a running app.
    """
    with _boot_app(rig, mock.base_url, install_bin=rig.root / "src/roost-session") as app:
        yield app


@pytest.fixture
def boot(bootstrap_app, rig, _reset_mock):
    """The app, ordered AFTER the autouse mock reset, with the rig's sheds
    re-listed.

    Depending on `_reset_mock` explicitly is what puts it first: it restores the
    mock's default payload before every test, so every shed an earlier cell minted
    has to be put back — a host whose shed the server stops listing loses its
    watcher, by design.
    """
    rig.list_running()
    return bootstrap_app


def _refresh(app: TauriClient) -> None:
    """An AUTHORITATIVE shed refresh — the one that carries which servers
    answered, and so the one that drives `observe_sheds`: a probe per running shed
    not yet watched, a watcher for each one a session answers on, and a removal
    for each one an answering server no longer lists."""
    app.call("sheds.list")


def _roost_rows(app: TauriClient, of: str | None = None, params: dict | None = None) -> list[dict]:
    """The ROOST-sourced rows in an `rc.list` answer.

    `of` selects one shed's rows client-side; `params` is the op's own
    `{host, shed}` filter, which is a different question — "what did the server
    narrow to" rather than "which of these are about this shed" — and the two are
    asked together in the union cell.
    """
    rows = [
        s
        for s in app.call("rc.list", params or {}).get("sessions", [])
        if s.get("source") == "roost"
    ]
    return [r for r in rows if of is None or r.get("shed") == of]


def _host_status(app: TauriClient, shed: str, **params) -> dict:
    """One shed's roost-host status row from `rc.list`, or `{}`."""
    hosts = app.call("rc.list", params).get("machines", [])
    return next(
        (h for h in hosts if h.get("origin") == f"roost:{SERVER}/{shed}"), {}
    )


def _bootstrap(app: TauriClient, target: str) -> dict:
    """Probe, consent to what the probe found, and bootstrap — the client flow,
    for the cells whose subject is what happens AFTER a successful one."""
    probe = app.call("roost.probe", {"target": target})["probe"]
    answer = app.call(
        "roost.bootstrap",
        {"target": target, "fingerprint": probe["fingerprint"], "consent": True},
    )
    assert answer["ok"] is True, answer
    return answer


# ---------------------------------------------------------------------------
# the plan matrix, read-only
# ---------------------------------------------------------------------------


def test_a_cold_shed_reads_missing_and_offers_an_install(boot, rig):
    """`roost.probe` on a shed with nothing on it, and the preview that follows.

    `Missing` is the whole point of the jail: without `jail_fs_root` this cell
    would find the developer's own `/usr/bin/roost-session` and report
    `Mismatch`.
    """
    target = rig.host("p19-cold")
    answer = boot.call("roost.probe", {"target": target})
    assert answer["target"] == target
    probe = answer["probe"]
    assert probe["outcome"]["kind"] == "missing", probe
    assert probe["session"]["state"] == "not-installed", probe
    assert probe["arch"] in ("amd64", "arm64"), probe
    assert probe["home"] == str(rig.home("p19-cold")), probe
    assert probe["dest"] == str(rig.installed("p19-cold")), probe
    assert probe["fingerprint"], probe
    assert answer["plan"]["kind"] == "install"
    assert answer["plan"]["dest"] == str(rig.installed("p19-cold"))

    # Nothing was written: a probe is a read.
    assert not rig.installed("p19-cold").exists()

    preview = boot.call("roost.preview", {"target": target})
    assert preview["plan"]["kind"] == "install"
    assert preview["actionable"] is True
    assert preview["source"]["rung"] == "override"
    assert preview["client_label"] == "shed-desktop"
    assert "ROOST_SESSION_INSTALL_BIN" in preview["source"]["sentence"], preview


def test_a_shed_with_a_usable_binary_and_no_session_offers_a_start(boot, rig):
    """Compatible + NoSession → Start.

    The row a pre-installed protocol-4 binary lands on, and the one that proves
    the bridge's `client-bridge: no session` refusal is read as a STATE (the
    binary is there, nothing is running) rather than as a failed probe.
    """
    target = rig.host("p19-stop", identity=IDENTITY_V4)
    answer = boot.call("roost.probe", {"target": target})
    probe = answer["probe"]
    assert probe["outcome"]["kind"] == "compatible", probe
    assert probe["outcome"]["path"] == str(rig.installed("p19-stop"))
    assert probe["outcome"]["identity"]["session_protocol"] == 4
    assert probe["session"]["state"] == "no-session", probe
    assert answer["plan"] == {
        "kind": "start",
        "needs_source": False,
        "path": str(rig.installed("p19-stop")),
    }

    preview = boot.call("roost.preview", {"target": target})
    assert preview["actionable"] is True, "a Start needs no bytes at all"


def test_a_stale_binary_with_no_session_offers_an_update(boot, rig):
    """Mismatch + NoSession → Update. shed's gate is the protocol number and
    nothing else, so a 0.0.19/protocol-2 build is the realistic incumbent."""
    target = rig.host("p19-stale", identity=IDENTITY_V2)
    answer = boot.call("roost.probe", {"target": target})
    probe = answer["probe"]
    assert probe["outcome"]["kind"] == "mismatch", probe
    assert probe["outcome"]["identity"]["session_protocol"] == 2
    assert probe["session"]["state"] == "no-session", probe
    plan = answer["plan"]
    assert plan["kind"] == "update"
    assert plan["needs_source"] is True
    assert plan["replaces_newer"] is False
    assert plan["incumbent"]["session_protocol"] == 2
    assert plan["dest"] == str(rig.installed("p19-stale"))


def test_a_mismatched_running_session_is_reported_and_never_stopped(boot, rig):
    """**Pin P6**, as an absence.

    A session somebody is using answers `session.identify` with protocol 2. shed
    reports it — and the assertion that matters is the negative one: no `start`,
    nothing stopped, the marker still there, and the far side's own call log
    carrying nothing but the `client-bridge` reads.
    """
    target = rig.host("p19-busy", identity=IDENTITY_V2, running=True, roost=rig.v2)
    answer = boot.call("roost.probe", {"target": target})
    probe = answer["probe"]
    assert probe["session"]["state"] == "running", probe
    assert probe["session"]["session_protocol"] == 2, probe
    plan = answer["plan"]
    assert plan["kind"] == "report"
    assert plan["session_protocol"] == 2
    assert "speaks protocol 2" in plan["message"], plan
    assert "roostctl session stop" in plan["message"], plan

    # Acting on it is refused at the same row, with the same copy — and still
    # nothing is done to the host.
    refusal = boot.call(
        "roost.bootstrap",
        {"target": target, "fingerprint": probe["fingerprint"], "consent": True},
    )
    assert refusal["ok"] is False
    assert refusal["error"]["stage"] == "report"
    assert "speaks protocol 2" in refusal["error"]["message"]

    assert rig.marker("p19-busy").exists(), "the session was left running"
    calls = rig.calls("p19-busy")
    assert calls, "the far side was reached at all"
    # A probe READS: `identify` asks the binary who it is and `client-bridge`
    # asks the session who it is. Neither changes anything. What must be absent
    # is the one verb that would: `start`.
    assert "start" not in calls, f"P6: never restarted — {calls}"
    assert set(calls) <= {"identify", "client-bridge"}, f"P6: reads only — {calls}"


def test_a_shed_the_server_never_listed_is_refused_and_never_contacted(boot, rig):
    """**A configured server is not a licence for every shed name on it.**

    `roost:<server>/<shed>` carries a server the user wrote down and a shed name
    that is DISCOVERED, and only the first half used to be checked: any name under
    a configured server composed an ssh identity out of the server entry plus
    whatever the caller invented. So previewing `roost:mock/never-existed` took a
    fingerprint off a host nobody had ever seen, and the bootstrap behind it would
    have ssh'd to `never-existed@<that server's host>` — a host the user neither
    configured nor discovered.

    The refusal lands before any transport exists, which is why the `ssh` log is
    the assertion that matters: every fake-`ssh` invocation is recorded with the
    shed name as its ssh user, and there must be no line for this one.
    """
    rig.host("p19-unlisted-peer")  # the server IS configured, live, and listing
    invented = f"roost:{SERVER}/p19-never-existed"
    for op in ("roost.probe", "roost.preview"):
        with pytest.raises(ShedError) as refused:
            boot.call(op, {"target": invented})
        assert refused.value.code == "probe", refused.value
        assert "has not listed" in str(refused.value), refused.value
        assert "p19-never-existed" in str(refused.value), refused.value

    answer = boot.call(
        "roost.bootstrap",
        {"target": invented, "fingerprint": "anything", "consent": True},
    )
    assert answer["ok"] is False, answer
    assert answer["error"]["stage"] == "probe", answer
    assert "has not listed" in answer["error"]["message"], answer

    reached = [line for line in rig.ssh_log() if line.startswith("p19-never-existed\t")]
    assert not reached, f"nothing may be contacted for an unlisted shed: {reached}"
    assert not (rig.root / "hosts" / "p19-never-existed").exists()


# ---------------------------------------------------------------------------
# consent, the fingerprint, and the install
# ---------------------------------------------------------------------------


def test_a_bootstrap_without_consent_is_refused_and_touches_nothing(boot, rig):
    """Consent is the client's to obtain, and its absence is a CALLER bug — an
    error envelope, before the host is even probed."""
    target = rig.host("p19-noconsent")
    probe = boot.call("roost.probe", {"target": target})["probe"]
    for params in (
        {"target": target, "fingerprint": probe["fingerprint"]},
        {"target": target, "fingerprint": probe["fingerprint"], "consent": False},
    ):
        with pytest.raises(ShedError) as refusal:
            boot.call("roost.bootstrap", params)
        assert refusal.value.code == "consent_required", refusal.value
        assert "nothing was changed" in str(refusal.value)
        assert not rig.installed("p19-noconsent").exists()


def test_a_stale_fingerprint_is_refused_before_anything_is_written(boot, rig):
    """What the user consented to has to be what is there.

    A fingerprint from another host is the cheapest way to say "not what you were
    asked about", and it must be refused with the host untouched — the machine
    re-probes too, but this check happens before a single byte is resolved.
    """
    target = rig.host("p19-moved")
    other = rig.host("p19-other", identity=IDENTITY_V4)
    stale = boot.call("roost.probe", {"target": other})["probe"]["fingerprint"]
    answer = boot.call(
        "roost.bootstrap",
        {"target": target, "fingerprint": stale, "consent": True},
    )
    assert answer["ok"] is False
    assert answer["error"]["stage"] == "fingerprint"
    assert "changed since you were asked" in answer["error"]["message"]
    assert "nothing was changed" in answer["error"]["message"]
    assert not rig.installed("p19-moved").exists()


def test_a_consented_bootstrap_installs_starts_and_wires_the_hooks(boot, rig, mock):
    """The whole payoff, end to end on a cold shed.

    Install → Start → the lease dialogue → the shed's roost rows in `rc.list`.
    The hooks half is the one that turns a shed's codex and cursor rows from
    liveness-only into roost-sourced, and it is asserted where it actually
    happens: on the far side, in the params the host session received.
    """
    rig.v4.agent_hooks_calls.clear()
    target = rig.host("p19-new")
    answer = _bootstrap(boot, target)
    assert answer["plan"]["kind"] == "install"
    assert answer["dest"] == str(rig.installed("p19-new"))
    assert answer["verdict"].startswith("ready pid="), answer
    assert answer["session_protocol"] == 4, answer

    # The binary really landed, and the far side really started it.
    assert rig.installed("p19-new").is_file()
    assert rig.marker("p19-new").exists()
    assert "start" in rig.calls("p19-new")

    # **Pin P5**: the far side's own shell cannot find `roost-session` (its
    # `.local/bin` is not on the jail's PATH), so the warning is reported and
    # nothing edits a dotfile to fix it.
    assert answer["path_warning"], answer
    assert "roost-session" in answer["path_warning"]

    # The hooks dialogue, as the HOST received it.
    hooks = answer["hooks"]
    assert hooks["applied"] is True, hooks
    assert hooks["client"] == "shed-desktop"
    assert hooks["mode"] == "auto"
    assert hooks["lease_held"] is True, "the bearer token is kept for the app run"
    assert hooks["wired"] == ["claude", "codex"], hooks
    assert [s["agent"] for s in hooks["skipped"]] == ["cursor", "grok"], hooks
    served = rig.v4.agent_hooks_calls
    assert len(served) == 1, served
    assert served[0]["mode"] == "auto"
    assert served[0]["client"] == "shed-desktop"
    assert served[0]["skip"] == []
    assert served[0]["lease"] == rig.v4.lease, "the lease it minted, presented back"
    assert rig.v4.lease_label == "shed-desktop"

    # And the shed is a readable roost host now: a watcher was spawned because
    # shed itself started the session.
    _wait_for(
        "the freshly bootstrapped shed to read as reachable",
        lambda: _host_status(boot, "p19-new").get("reachable") is True or None,
    )
    status = _host_status(boot, "p19-new")
    assert status["kind"] == "shed"
    assert status["server"] == SERVER
    assert status["down_kind"] is None

    # A second probe now lands on the "nothing to do" row — the same host, one
    # install later.
    again = boot.call("roost.probe", {"target": target})
    assert again["probe"]["session"]["state"] == "running"
    assert again["plan"]["kind"] == "up-to-date"


# ---------------------------------------------------------------------------
# the rows
# ---------------------------------------------------------------------------


def test_a_running_shed_is_probed_and_then_watched(boot, rig, mock):
    """**Probed, not watched by default** (plan 019 §3.6).

    A refresh gives every running shed one cheap `session.identify`. The one a
    session answers on earns a watcher — with no bootstrap, no click and no
    launch. The one that has a binary and nothing serving earns nothing, which is
    the whole reason the probe exists: a watcher is a live ssh client-bridge.
    """
    live = rig.roost()
    rig.host("p19-live", identity=IDENTITY_V4, running=True, roost=live)
    quiet = rig.host("p19-quiet", identity=IDENTITY_V4)
    _refresh(boot)

    _wait_for(
        "the running shed to be probed and watched",
        lambda: _host_status(boot, "p19-live").get("reachable") is True or None,
    )
    assert _host_status(boot, "p19-quiet") == {}, "no session, no host row"
    # …and the probe of the quiet one really did happen, over the bridge.
    assert "client-bridge" in rig.calls("p19-quiet")
    # A second refresh must not fork a second watcher for the same shed.
    _refresh(boot)
    live = [
        h
        for h in boot.call("rc.list")["machines"]
        if h["origin"] == f"roost:{SERVER}/p19-live"
    ]
    assert len(live) == 1, live
    assert boot.call("roost.preview", {"target": quiet})["plan"]["kind"] == "start"


def test_hub_and_roost_rows_are_a_union_and_a_filter_returns_both(boot, rig, mock):
    """**The union rule** (plan 019 §3.6), including under a filter.

    One shed, two row sets: the hub's, listed over ssh, and roost's, read from
    the session on it. They are stamped so a consumer can tell them apart, they
    share the `host`/`shed` a filter selects on, and a filtered `rc.list` — the
    shed card's own query, which used to drop the roost half entirely — returns
    both.
    """
    shed = "p19-union"
    fake = rig.roost()
    target = rig.host(shed, identity=IDENTITY_V4, roost=fake)
    _bootstrap(boot, target)
    # An adapter-claimed tab, so the roost half of the union is a row that
    # survives the watcher's next snapshot — see `Rig.agent_tab`.
    rig.agent_tab(fake)

    launched = boot.call(
        "roost.launch",
        {"target": target, "kind": "codex", "workdir": "/home/shed/work"},
    )
    assert launched["origin"] == target
    assert launched["source"] == "roost"
    assert launched["origin_kind"] == "shed"
    assert launched["host"] == SERVER, "the SERVER, as the hub row spells it"
    assert launched["shed"] == shed
    assert launched["machine"] == target, "the address a lane op takes"
    opened = fake.opens[-1]
    assert opened["cwd"] == "/home/shed/work"
    # `shed_core::roost::launch_argv`'s recipe for the kind: the agent's own
    # binary, resolved by roost on the far side. (The S4 provider's `bash -lc`
    # wrapper is its own path — this op is plan 013's `tab.open`, generalized.)
    assert opened["argv"] == ["codex"], opened

    # A hub row for the SAME shed: test mode synthesizes it, which is exactly the
    # other half of the union.
    hub = boot.call("rc.launch", {"shed": shed, "kind": "claude-rc"})

    # Wait for the WATCHER's own snapshot rather than reading the optimistic row
    # `roost.launch` inserted: the union is a claim about what a listing says once
    # both halves are real, and the adapter-claimed tab is what makes the roost
    # half survive a resync.
    _wait_for(
        "the claimed tab to reach the listing",
        lambda: [r for r in _roost_rows(boot, shed) if r["slug"] == "7"] or None,
    )
    everything = boot.call("rc.list")["sessions"]
    hub_rows = [s for s in everything if s.get("shed") == shed and s.get("source") == "hub"]
    roost_rows = [s for s in everything if s.get("shed") == shed and s.get("source") == "roost"]
    assert hub_rows, f"the hub half is missing: {everything}"
    assert roost_rows, f"the roost half is missing: {everything}"
    assert {r["origin"] for r in hub_rows} == {f"{SERVER}/{shed}"}
    assert {r["origin"] for r in roost_rows} == {target}
    assert hub["slug"] in {r["slug"] for r in hub_rows}

    # The two origins carry DIFFERENT contracts, which is what lets a card read
    # the one the button it is drawing belongs to.
    caps = boot.call("rc.list")["capabilities"]
    assert target in caps, list(caps)
    assert caps[target]["kind_features"]["codex"]["attach"] == "native-remote"

    # **The filtered query.** Same rows, asked the way a shed card asks.
    filtered = boot.call("rc.list", {"host": SERVER, "shed": shed})
    assert {s.get("source") for s in filtered["sessions"]} == {"hub", "roost"}, filtered
    assert _roost_rows(boot, shed, {"host": SERVER, "shed": shed}), filtered
    assert target in filtered["capabilities"]
    # A machine belongs to no server, so a filter omits every machine — and the
    # other sheds' rows are out of scope too.
    assert all(s.get("origin_kind") == "shed" for s in filtered["sessions"])
    assert all(s.get("shed") == shed for s in filtered["sessions"])
    assert not _roost_rows(boot, "p19-new", {"host": SERVER, "shed": shed})


def test_a_down_kind_reaches_rc_list_and_a_flap_keeps_the_roost_rows(boot, rig, mock):
    """`Down.kind` end to end, and the flap that must not blank the card.

    The classification is recorded on the TUNNEL, after the fact, by a
    per-connection `ssh` exec that failed — the warm-up (`true`) succeeded, so
    nothing but the tunnel can know. Here the far side's session goes away between
    two bridge connections, which is precisely the late `NoSession` plan 019 §3.6
    describes, and the kind has to survive the trip to `rc.list`.

    Then it comes back, and the assertion is that the rows never left: a `Down`
    keeps the last snapshot, dimmed, exactly as a machine's does.
    """
    shed = "p19-flap"
    fake = rig.roost()
    target = rig.host(shed, identity=IDENTITY_V2, roost=fake)
    installed = _bootstrap(boot, target)
    assert installed["plan"]["kind"] == "update", installed
    rig.agent_tab(fake)
    boot.call("roost.launch", {"target": target, "kind": "codex", "workdir": "/home/shed"})
    # The WATCHER's rows, not the optimistic one `roost.launch` inserted: an
    # unclaimed tab leaves the agent-owned inventory on the next snapshot (it is
    # somebody's terminal), so the row that must survive the flap is the claimed
    # one.
    rows_before = _wait_for(
        "the shed's roost rows",
        lambda: [r for r in _roost_rows(boot, shed) if r["slug"] == "7"] or None,
    )

    # The session stops. The MARKER is what the fake's `client-bridge` reads, so
    # removing it makes the next per-connection exec answer with roost's own
    # `client-bridge: no session` — installed, not running.
    rig.marker(shed).unlink()
    fake.close_all()  # hang the live stream up so the watcher re-dials

    kind = _wait_for(
        "the down kind to reach rc.list",
        lambda: _host_status(boot, shed).get("down_kind"),
        timeout=60,
    )
    assert kind == "no-session", _host_status(boot, shed)
    dimmed = _roost_rows(boot, shed)
    assert dimmed, "a Down keeps the last rows on screen"
    assert all(r["stale"] is True for r in dimmed), dimmed
    assert {r["slug"] for r in dimmed} == {r["slug"] for r in rows_before}

    # And back: Snapshot → Down → Snapshot, with the rows intact throughout.
    rig.marker(shed).touch()
    _wait_for(
        "the shed to come back",
        lambda: _host_status(boot, shed).get("reachable") is True or None,
        timeout=90,
    )
    back = _host_status(boot, shed)
    assert back["down_kind"] is None, "recovery clears the classification"
    restored = _roost_rows(boot, shed)
    assert {r["slug"] for r in restored} == {r["slug"] for r in rows_before}
    assert all(r["stale"] is False for r in restored), restored


def test_a_stopped_shed_loses_its_watcher_and_its_rows(boot, rig, mock):
    """A shed that stopped is not a shed that is unreachable.

    The authoritative refresh is what can tell the two apart — it knows which
    SERVERS answered — so a running shed that disappears from an answering
    server's listing loses its watcher and its rows outright, rather than sitting
    there dimmed forever.
    """
    shed = "p19-gone"
    fake = rig.roost()
    target = rig.host(shed, identity=IDENTITY_V4, roost=fake)
    _bootstrap(boot, target)
    rig.agent_tab(fake)
    _wait_for(
        "the started shed to read as reachable",
        lambda: _host_status(boot, shed).get("reachable") is True or None,
    )

    # `shed stop`: the server stops listing it as running. Every other shed the
    # rig minted stays listed, which is what makes this about THIS shed.
    mock.reset()
    rig.list_running(stopped=shed)
    _refresh(boot)
    _wait_for(
        "the stopped shed's roost host to go away",
        lambda: _host_status(boot, shed) == {} or None,
    )
    assert not _roost_rows(boot, shed)


# ---------------------------------------------------------------------------
# no bytes to install
# ---------------------------------------------------------------------------


def test_without_a_source_there_is_no_button(rig, mock, _reset_mock):
    """The sixth plan-matrix row: the host implies an Install, and shed has
    nothing to install.

    Its own app instance, because the source ladder reads the process
    environment: no `ROOST_SESSION_INSTALL_BIN`, no protocol-4 roost beside this
    app, and `RELEASE_PIN` is `None` until roost ships one — so the ladder ends at
    `NoSource` and the preview says so in the words plan 019 §3.5 pinned.
    """
    with _boot_app(rig, mock.base_url, install_bin=None, session_bin=ABSENT) as app:
        target = rig.host("p19-nosource")
        preview = app.call("roost.preview", {"target": target})
        assert preview["plan"]["kind"] == "install", preview
        assert preview["source"]["rung"] == "none"
        assert preview["actionable"] is False, "no bytes, no button"
        sentence = preview["source"]["sentence"]
        assert "no roost release speaking session protocol 4 is published yet" in sentence
        assert "(the latest, 0.0.19, speaks 2)" in sentence
        assert "ROOST_SESSION_INSTALL_BIN" in sentence
        assert f"{target} was left untouched." in sentence

        # And acting on it anyway refuses at the source stage, with that sentence.
        probe = app.call("roost.probe", {"target": target})["probe"]
        answer = app.call(
            "roost.bootstrap",
            {"target": target, "fingerprint": probe["fingerprint"], "consent": True},
        )
        assert answer["ok"] is False
        assert answer["error"]["stage"] == "source"
        assert "no roost release speaking session protocol 4" in answer["error"]["message"]
        assert not rig.installed("p19-nosource").exists()


# ---------------------------------------------------------------------------
# Against a REAL roost-session binary (opt-in — CI has none)
# ---------------------------------------------------------------------------


def _real_session_binary() -> Path | None:
    """The protocol-4 `roost-session` to install, or None to skip.

    `SHED_TAURI_ROOST_SESSION_BIN` is the same knob the machine suite's
    `real_roost` cell uses; the plan-014 build cache is the default because that
    is where this repo's only protocol-4 binary lives (roost's own releases speak
    2). **Read-only — never rebuilt here**: roost's tree is not this repo's to
    compile.
    """
    raw = os.environ.get("SHED_TAURI_ROOST_SESSION_BIN") or str(
        Path.home() / ".cache/shed-plan014/roost-c67ac27/target/release/roost-session"
    )
    path = Path(raw).expanduser()
    return path if os.access(path, os.X_OK) else None


@pytest.mark.real_roost
@pytest.mark.skipif(
    _real_session_binary() is None,
    reason="no protocol-4 roost-session available (set SHED_TAURI_ROOST_SESSION_BIN)",
)
def test_the_real_binary_is_installed_and_started_behind_the_fake_ssh(rig, mock, _reset_mock):
    """The fake session's fidelity claim, checked against the real thing.

    Every other cell installs a shell script that answers `identify`, `start` and
    `client-bridge`. That script is written from roost's source, but a stand-in is
    a stand-in: the assertion that binds is this one, where the REAL c67ac27
    binary is streamed through roost's own install order and then identifies
    itself — all of it behind the fake `ssh`, in a throwaway `$HOME`.

    The `start` really starts a daemon (roost's `start_script` `setsid`s), so the
    pid roost's own readiness verdict reports is killed in teardown.
    """
    binary = _real_session_binary()
    assert binary is not None  # the skipif above
    shed = "p19-real"
    verdict = None
    try:
        with _boot_app(rig, mock.base_url, install_bin=binary) as app:
            target = rig.host(shed)
            probe = app.call("roost.probe", {"target": target})
            assert probe["probe"]["outcome"]["kind"] == "missing", probe
            answer = app.call(
                "roost.bootstrap",
                {
                    "target": target,
                    "fingerprint": probe["probe"]["fingerprint"],
                    "consent": True,
                },
            )
            assert answer["ok"] is True, answer
            verdict = answer["verdict"]
            assert verdict.startswith("ready pid="), answer
            assert answer["session_protocol"] == 4, "the real binary speaks 4"
            assert answer["session_id"], answer
            installed = rig.installed(shed)
            assert installed.is_file()
            assert installed.stat().st_size == binary.stat().st_size, "the whole binary"
            # It answers `identify` for itself, out of band — the same question
            # the staged verify asked, now asked of the file that landed.
            out = subprocess.run(
                [str(installed), "identify"],
                capture_output=True,
                text=True,
                check=True,
                env={"HOME": str(rig.home(shed)), "PATH": "/usr/bin:/bin"},
            ).stdout
            assert json.loads(out)["session_protocol"] == 4, out
    finally:
        if verdict and verdict.startswith("ready pid="):
            pid = verdict.split("=", 1)[1].strip()
            subprocess.run(["kill", pid], check=False)
            time.sleep(0.5)
            subprocess.run(["kill", "-9", pid], check=False)


# ---------------------------------------------------------------------------
# the UI (C8): the status line + plan-matrix button, the consent dialog's
# copy, and the toast — all UI TRUTH, read the `machines.dump` way rather than
# by re-deriving what the backend already proved above.
# ---------------------------------------------------------------------------


def _shed_roost_dump(app: TauriClient) -> dict:
    """Every RUNNING shed's roost row as the Sheds pane actually rendered it —
    `{}` off-pane or before the pane's board has answered its first preview.

    Deliberately a different question from `roost.preview`: that is the
    backend's answer regardless of what's on screen; this is what a person
    looking at the pane reads — the whole point of a `*.dump` op (`machines.dump`'s
    rule, extended to roost)."""
    return app.call("shed_roost.dump")["sheds"] or {}


def _roost_consent_dump(app: TauriClient) -> dict | None:
    """The open consent card's rendered copy, or `None` with none mounted."""
    return app.call("roost_consent.dump")["consent"]


def _toast_dump(app: TauriClient) -> dict | None:
    """The toast currently on screen as it reported itself, or `None`."""
    return app.call("toast.dump")["toast"]


def _on_screen(dump: dict, *fields: str) -> None:
    """Assert each named copy field is IN THE DOM, not merely in the dump.

    **The difference between testing the UI and testing a parallel data
    structure.** Every other field in a `*.dump` is derived from React state, so
    on their own they would all still pass with the component deleted from the
    tree. `rendered` is the component's own `textContent`, read back after the
    commit (`lib/roost.ts`'s `renderedText`) — so a copy assertion that also
    checks `rendered` is an assertion about the screen.
    """
    rendered = dump.get("rendered")
    assert rendered, f"nothing was rendered at all: {dump}"
    for field in fields:
        value = dump[field]
        if value is None:
            continue
        assert value in rendered, f"{field}={value!r} is not on screen: {rendered!r}"


def _wait_for_roost_row(app: TauriClient, target: str, *, timeout: float = 30.0) -> dict:
    """Poll `shed_roost.dump` until `target` has a row with a preview loaded
    (a `status` present — `None` while the board's fetch is still in flight)."""
    box: dict = {}

    def ready() -> bool:
        row = _shed_roost_dump(app).get(target)
        if row and (row.get("status") is not None or row.get("error") is not None):
            box["row"] = row
            return True
        return False

    app.wait_until(ready, timeout=timeout, what=f"the sheds pane to render a roost row for {target}")
    return box["row"]


def _open_consent(app: TauriClient, target: str) -> dict:
    """Drive the consent card open the way the harness has to — no click — and
    wait for it to report itself. `ui.show_roost_consent` re-previews `target`
    host-side, so this is exercising the SAME door a card's button uses, not a
    shortcut around it."""
    app.call("ui.show_roost_consent", {"target": target})
    app.wait_until(
        lambda: (_roost_consent_dump(app) or {}).get("target") == target,
        timeout=20,
        what="the consent card to open and report itself",
    )
    return _roost_consent_dump(app)


def _close_consent(app: TauriClient) -> None:
    app.call("ui.close_roost_consent")
    app.wait_until(lambda: _roost_consent_dump(app) is None, timeout=10, what="the consent card to close")


def test_the_sheds_pane_renders_install_and_update_buttons(boot, rig):
    """The actionable rows: a cold shed's card offers Install, a stale binary's
    offers Update — the SAME plan `roost.preview` computed (C7), now read off
    what the pane rendered rather than off the wire directly."""
    boot.navigate("sheds")
    cold = rig.host("p19-ui-cold")
    stale = rig.host("p19-ui-stale", identity=IDENTITY_V2)

    row = _wait_for_roost_row(boot, cold)
    assert row["button"] == "Install", row
    assert row["status"], "a status line accompanies the button"
    _on_screen(row, "status", "button")

    row = _wait_for_roost_row(boot, stale)
    assert row["button"] == "Update", row
    _on_screen(row, "status", "button")


def test_the_sheds_pane_renders_start_with_no_button_states(boot, rig):
    """A Start row (installed, not running), and the two non-actionable rows
    the plan matrix carries no button for: up to date, and a mismatched
    session's Report — whose message is PINNED COPY and must reach the pane
    verbatim (plan 019 §3.4)."""
    boot.navigate("sheds")
    startable = rig.host("p19-ui-start", identity=IDENTITY_V4)
    up_to_date = rig.host("p19-ui-uptodate", identity=IDENTITY_V4, running=True, roost=rig.v4)
    reported = rig.host("p19-ui-mismatch", identity=IDENTITY_V2, running=True, roost=rig.v2)

    row = _wait_for_roost_row(boot, startable)
    assert row["button"] == "Start", row
    _on_screen(row, "status", "button")

    row = _wait_for_roost_row(boot, up_to_date)
    assert row["button"] is None, row
    assert "running" in row["status"], row
    _on_screen(row, "status")

    row = _wait_for_roost_row(boot, reported)
    assert row["button"] is None, row
    assert row["status"] == (
        f"roost-session on {reported} speaks protocol 2, this build speaks 4 — "
        "upgrade whichever is older; stop it there with `roostctl session stop` "
        "and reconnect once it is."
    ), "the Report row's message must reach the pane verbatim (pinned copy)"
    # …and verbatim ON SCREEN, not merely in the dump.
    _on_screen(row, "status")


def _mint_private_shed(rig: Rig, shed: str) -> None:
    """`Rig.host`'s jail setup, minus the part that lists the shed on the
    SHARED session `mock` — for a cell that needs its own private server (see
    `test_a_blocked_install_shows_the_pinned_no_source_sentence_and_no_button`).

    `rig.host` always calls `self.mock.add_shed(...)`, which makes the shed
    visible to EVERY app pointed at the shared mock — including the
    module-scoped `boot` app, which polls `sheds.list` every 5s for the whole
    module's lifetime and starts its OWN `observe_sheds` bridge probe on
    anything newly running. A UI cell that then drives the Sheds pane and
    polls for up to 30s gives that probe repeated chances to collide with this
    cell's own preview on the exact same fake-ssh jail (`another Roost is
    connected` — a real refusal, just aimed at the wrong opponent: two
    read-only probes from two harness-only processes, not a real second
    client). Still registered in `rig.hosts` so the module's final
    hermeticity audit recognizes it.
    """
    home = rig.home(shed)
    for name in ("run", "data", "state", "cache"):
        path = home / name
        path.mkdir(parents=True, exist_ok=True)
        for level in (path, path.parent, path.parent.parent):
            level.chmod(0o700)
    (rig.root / "hosts" / shed / "socket").write_text(str(rig.v4.socket_path))
    if shed not in rig.hosts:
        rig.hosts.append(shed)


def test_a_blocked_install_shows_the_pinned_no_source_sentence_and_no_button(rig):
    """The sixth plan-matrix row, rendered: `NoSource` blocks an otherwise
    actionable plan, and the card shows the pinned sentence IN PLACE of a
    button — never a button, never a paraphrase (plan 019 §3.5/C8 bullet 2).

    Its own app AND its own private mock server — see `_mint_private_shed`:
    the source ladder needs an app with no override env, and this shed must
    stay invisible to the `boot` app's own background prober."""
    target_name = "p19-ui-nosource"
    _mint_private_shed(rig, target_name)
    private_mock = MockShedServer()
    private_mock.start()
    try:
        private_mock.add_shed({"name": target_name, "status": "running", "backend": "vz"})
        with _boot_app(rig, private_mock.base_url, install_bin=None, session_bin=ABSENT) as app:
            app.navigate("sheds")
            # The authoritative refresh that makes the shed addressable at all
            # (`RoostHosts::ssh_entry`'s gate — see `Rig.list_running`). The
            # frontend's own 5 s poll would get there too; asking directly keeps
            # the cell off that clock.
            app.call("sheds.list")
            target = f"roost:{SERVER}/{target_name}"
            row = _wait_for_roost_row(app, target)
            assert row["button"] is None, row
            # EXACT equality, not a substring check: this is the pinned
            # `NoSource` sentence (plan 019 §3.5) and must reach the pane
            # verbatim — a substring match would pass even if the renderer
            # appended or reworded anything around it.
            assert row["status"] == (
                "no roost release speaking session protocol 4 is published yet "
                "(the latest, 0.0.19, speaks 2). On a Linux machine with a "
                "protocol-4 roost installed the desktop uses that roost-session; "
                "otherwise point ROOST_SESSION_INSTALL_BIN at a protocol-4 build. "
                f"{target} was left untouched."
            ), row["status"]
            # …and the whole of it is on screen, in place of a button.
            _on_screen(row, "status")
    finally:
        private_mock.stop()


def test_the_consent_dialog_names_what_where_from_and_the_hook_wiring(boot, rig):
    """The Install consent card's four required lines (plan 019 §3.5): what,
    where, from where, and the hook-wiring sentence — no backup note for a
    plain install."""
    boot.navigate("sheds")
    target = rig.host("p19-ui-consent-install")
    _wait_for_roost_row(boot, target)  # the board has to have a plan before consent opens meaningfully
    try:
        card = _open_consent(boot, target)
        assert card["action"] == "Install", card
        assert target in card["what"]
        assert target in card["where"] and ".local/bin/roost-session" in card["where"]
        # **The "from where" sentence, exactly.** `assert card["from"]` accepts any
        # non-empty string, so the one line that tells a person whose bytes are
        # about to be installed on their host could become meaningless and still
        # pass. This module's app runs with `ROOST_SESSION_INSTALL_BIN` pointed at
        # the rig's stand-in, so the sentence is the override rung's
        # (`source.rs::override_origin`): the path, and the variable that named it.
        assert card["from"] == (
            f"{rig.root / 'src/roost-session'} (ROOST_SESSION_INSTALL_BIN)"
        ), card["from"]
        assert card["hooks"] == (
            "roost-session will also wire its hooks into the agents already configured "
            "there — claude, codex, cursor, opencode, grok — and nothing else."
        ), "the hook-wiring sentence is pinned copy (plan 019 §3.4)"
        assert card["backup"] is None, "nothing to back up on a plain install"
        # Every one of those lines is in the card's own DOM, so this is a test of
        # the dialog and not of a parallel copy of its state.
        _on_screen(card, "what", "where", "from", "hooks")
    finally:
        _close_consent(boot)


def test_the_consent_dialog_names_the_backup_for_an_update(boot, rig):
    """The Update-only line: the previous file will be backed up (plan 019 §3.5).

    **Exact equality.** `"backed up" in card["backup"]` also accepts the REVERSED
    promise — "The previous file will not be backed up" is non-null and contains
    "backed up" — which is the one thing this line exists to rule out.
    """
    boot.navigate("sheds")
    shed = "p19-ui-consent-update"
    target = rig.host(shed, identity=IDENTITY_V2)
    _wait_for_roost_row(boot, target)
    try:
        card = _open_consent(boot, target)
        assert card["action"] == "Update", card
        assert card["backup"] == (
            f"The file currently at {rig.installed(shed)} will be backed up "
            "before it's replaced."
        ), card["backup"]
        _on_screen(card, "what", "where", "from", "hooks", "backup")
    finally:
        _close_consent(boot)


def test_confirming_consent_installs_and_toasts_the_result(boot, rig):
    """The whole loop, driven the way a click would: open the card, confirm it
    (the harness's stand-in for the click, `ui.confirm_roost_consent`), and
    read the toast + the row settling to Start-or-better — all UI truth, none
    of it re-derived from the `roost.bootstrap` answer directly.

    **And one confirm is one bootstrap.** The confirm door is driven TWICE here,
    which is what a double-click looks like from the app's side, and the host's own
    call log is asserted to carry exactly one `start`. A count rather than a
    membership test: `"start" in calls` passes just as happily when a second
    bootstrap streamed, committed and started over the first — the symptom of
    running side effects inside a React state updater, which StrictMode invokes
    twice.
    """
    boot.navigate("sheds")
    shed = "p19-ui-confirm"
    target = rig.host(shed)
    _wait_for_roost_row(boot, target)
    _open_consent(boot, target)
    boot.call("ui.confirm_roost_consent")
    boot.call("ui.confirm_roost_consent")
    # The card closes immediately (no cancel op exists to hold it open across a
    # 10-minute budget) — progress shows on the ROW, not a lingering modal.
    boot.wait_until(lambda: _roost_consent_dump(boot) is None, timeout=10, what="the card to close on confirm")

    def toasted() -> bool:
        t = _toast_dump(boot)
        return bool(t and any(target in line for line in t["lines"]))

    boot.wait_until(toasted, timeout=30, what="a toast naming the target to appear")
    toast = _toast_dump(boot)
    assert f"roost-session started on {target}." in toast["lines"]
    # The rig's utils jail has no `$HOME/.local/bin` on PATH (module docstring) —
    # the PATH warning is exactly what should ride the toast, verbatim.
    assert any("isn't on" in line and "PATH" in line for line in toast["lines"]), toast
    assert toast["tone"] == "warn", "a PATH warning downgrades the tone from ok"
    # The lines were painted, not just reported — `rendered` is the toast's own
    # DOM text (see `_on_screen`).
    for line in toast["lines"]:
        assert line in toast["rendered"], (line, toast["rendered"])

    # And the row itself moved on — the confirm bumped the board's refresh, no
    # manual pane refresh needed. Waits for the SETTLED state specifically
    # (not just "any answer"): the row's very first answer, from before the
    # bootstrap ran, already had a non-null status ("roost-session not
    # found"), so `_wait_for_roost_row`'s generic readiness would return that
    # stale snapshot immediately.
    boot.wait_until(
        lambda: _shed_roost_dump(boot).get(target, {}).get("button") is None,
        timeout=30,
        what="the row to settle on a no-button (Start-or-better) state after the confirm",
    )
    row = _shed_roost_dump(boot)[target]
    assert "running" in row["status"], row
    _on_screen(row, "status")
    assert rig.installed(shed).is_file()
    calls = rig.calls(shed)
    assert calls.count("start") == 1, f"one confirm is one bootstrap: {calls}"


def test_the_fake_ssh_never_reached_a_real_host(boot, rig):
    """A hermeticity audit, last so it sees every cell's traffic.

    The one thing that must be true of every line in the log: it was a command
    for a shed this rig minted. A destination the rig has no jail for answers 255
    (`ssh`'s own unreachable code) — so this asserts the seam did the work, not
    merely that nothing crashed.
    """
    log = rig.ssh_log()
    assert log, "the cells above went through the fake ssh"
    # Every shed this rig minted, plus the mock's own default shed (which every
    # refresh probes and which has no jail, so it answers `ssh`'s own 255).
    known = set(rig.hosts) | {"hello-world"}
    # An invocation that names NO host is control-socket management — roost's
    # `-O exit` teardown and its scratch sweep — and reaches nothing.
    seen = {line.split("\t", 1)[0] for line in log} - {"exit", ""}
    assert seen, f"no host was ever addressed: {log[:3]}"
    assert seen <= known, f"the fake ssh was asked about {seen - known}"
