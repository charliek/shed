"""Machine targets in the Tauri app (plan 012 S4, roadmap R4) — `--target tauri`.

**Hermetic, and genuinely exercising the real code.** A machine is reached over
SSH, which a hermetic harness cannot do — so the app is launched with the
test-mode `SHED_TAURI_MACHINE_HUB_PORTS` seam, which swaps the `ssh -N -L`
forward for a direct `FixedPort`, per machine. Everything above that port is the production path: the
real `HubClient`, the real `MachineHubWatcher`, the real decode, the real fold.

That seam exists for shed-mobile (which supplies its own Dart-side forward). It
turning out to be exactly what a hermetic harness needs is a good sign the cut
landed in the right place.

The alternative — injecting rendered rows like `rc.inject_test` does — would test
the renderer and nothing else, leaving the hub client, the watcher, the reconnect
loop and the unreachable posture uncovered. Those are where the bugs were.
"""

from __future__ import annotations

import json
import os
import socket
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

import ui
from client import ShedError, TauriClient

pytestmark = pytest.mark.skipif(
    os.environ.get("SHED_TEST_TARGET", "mac") != "tauri",
    reason="tauri-only: the mac app has no machine layer",
)

FIXTURES = Path(__file__).resolve().parent / "fixtures"

# The identity token a real hub returns from /v1/health (Go `rc.HubAppID`).
HUB_APP_ID = "shed-rc-hub"

# Shaped like a live machine hub's snapshot — note `target_label: machine:…` and
# the activity dimension the one-shot `list` never carries.
SNAPSHOT = {
    "sessions": [
        {
            "slug": "mch001",
            "tmux_session": "rc-mch001",
            "kind": "shell",
            "state": "ready",
            "managed": True,
            "lane": "tui",
            "display_name": "machine probe",
            "workdir": "/home/charliek",
            "created_by": "sx",
            "target_label": "machine:mini3",
            "activity": "working",
        }
    ]
}


class _HubServer(ThreadingHTTPServer):
    """Daemon threads + no block-on-close, so teardown cannot hang.

    `/v1/events` is an open-ended SSE stream the app HOLDS, and Python's default
    `block_on_close=True` makes `shutdown()` wait for in-flight handlers — so a
    handler still blocked in `wfile.write` (app quitting slowly, socket buffer
    full) would hang the module until the pytest timeout. The host-agent-diff
    harness sets `daemon_threads` for the same reason.
    """

    daemon_threads = True
    block_on_close = False


class _HubHandler(BaseHTTPRequestHandler):
    def log_message(self, *args):  # keep pytest output clean
        pass

    def do_GET(self):  # noqa: N802 (stdlib API)
        if self.path == "/v1/health":
            self._json({"app": HUB_APP_ID, "version": "shedtest-fake"})
        elif self.path == "/v1/sessions":
            self._json(SNAPSHOT)
        elif self.path == "/v1/events":
            # An SSE stream that stays OPEN: the watcher holds this connection,
            # so closing it immediately would look like a disconnect and the app
            # would flip the machine to unreachable mid-test.
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            try:
                while not self.server.stopping.is_set():  # type: ignore[attr-defined]
                    # A comment frame is the hub's own heartbeat shape.
                    self.wfile.write(b": ok\n\n")
                    self.wfile.flush()
                    self.server.stopping.wait(0.5)  # type: ignore[attr-defined]
            except OSError:
                # Any closed-socket write — macOS does not always raise the
                # BrokenPipeError/ConnectionResetError subclasses.
                pass
        else:
            self.send_response(404)
            self.end_headers()

    def _json(self, body: dict) -> None:
        payload = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@pytest.fixture(scope="module")
def fake_hub():
    """A loopback hub the app reaches through the FixedPort seam."""
    port = _free_port()
    server = _HubServer(("127.0.0.1", port), _HubHandler)
    server.stopping = threading.Event()  # type: ignore[attr-defined]
    threading.Thread(target=server.serve_forever, daemon=True).start()
    yield port
    server.stopping.set()  # type: ignore[attr-defined]
    server.shutdown()
    server.server_close()


@pytest.fixture(scope="module")
def machine_app(fake_hub, mock):
    """A SECOND, self-managed app instance pointed at the fake hub.

    Self-managed rather than the shared session app because the machine config
    and the hub port must be set at LAUNCH — `machines:` is read once at startup
    to spawn the watchers, exactly as it would be in production. The down-host
    suite uses the same pattern for the same reason.
    """
    # `tempfile.mkdtemp` with a SHORT prefix, not pytest's tmp_path_factory: the
    # IPC socket lives under this dir, and pytest's nested path under macOS's
    # long TMPDIR overruns the Unix-socket limit (SUN_LEN) — the app then aborts
    # at bind. Same reason the session fixture does it this way.
    state_dir = Path(tempfile.mkdtemp(prefix="shed-e2e-mach-"))
    ui.quit("tauri")
    ui.launch(
        "tauri",
        mock_base_url=mock.base_url,
        config_path=FIXTURES / "config-machines.yaml",
        state_dir=state_dir,
        # mini3 gets the fake hub; `sleepy` is deliberately UNMAPPED, which
        # the app treats as permanently unreachable — the everyday asleep case,
        # with no ssh and no real machine involved.
        machine_hub_ports={"mini3": fake_hub},
    )
    client = TauriClient(ui.socket_path("tauri"))
    # The WebView mounts AFTER `identify`, so wait for its first snapshot before
    # any test drives a UI op — otherwise `ui.navigate` races the frontend and
    # answers `frontend_not_ready`. Same wait the session fixture does.
    client.wait_until(
        lambda: client.current_pane() is not None,
        timeout=30,
        what="tauri frontend ready",
    )
    try:
        yield client
    finally:
        client.close()
        ui.quit("tauri")


def test_a_machine_session_is_listed_beside_shed_sessions(machine_app):
    """AC4's core: a machine session appears in the SAME `rc.list` payload as
    shed sessions, stamped with its origin.

    The origin stamp is what the UI keys and labels by — `shed` is empty for
    every machine session by construction, so keying on it would collide two
    machines that happen to share a slug.
    """
    machine_app.wait_until(
        lambda: any(
            s.get("origin_kind") == "machine"
            for s in machine_app.call("rc.list").get("sessions", [])
        ),
        timeout=30,
        what="the machine session to arrive",
    )
    payload = machine_app.call("rc.list")
    rows = [s for s in payload["sessions"] if s.get("origin_kind") == "machine"]
    assert len(rows) == 1, f"expected one machine session, got {rows!r}"
    row = rows[0]
    assert row["slug"] == "mch001"
    assert row["origin"] == "machine:mini3"
    assert row["machine"] == "mini3"
    assert row["shed"] == "", "a machine session belongs to no shed"
    assert row["stale"] is False, "a reachable machine's rows are not stale"
    # The ENRICHED dimension the one-shot list never carries — proof the row came
    # off the hub snapshot rather than being synthesized.
    assert row["activity"] == "working"


def test_a_reachable_machine_reports_its_health(machine_app):
    """Every configured machine has a status row, whether or not it has
    sessions — the row is how the UI renders a machine group."""
    machine_app.wait_until(
        lambda: any(
            m.get("reachable") for m in machine_app.call("machines.list")["machines"]
        ),
        timeout=30,
        what="the machine to report reachable",
    )
    machines = machine_app.call("machines.list")["machines"]
    assert [m["name"] for m in machines] == ["mini3", "sleepy"]
    live = next(m for m in machines if m["name"] == "mini3")
    assert live["reachable"] is True
    assert live["connected_once"] is True
    assert live["sessions"] == 1
    assert live["detail"] is None
    assert live["origin"] == "machine:mini3"


def test_an_unreachable_machine_is_a_row_with_a_reason_not_an_error(machine_app):
    """**Unreachable is a first-class state.** `sleepy` is unmapped in the hub
    port map, so nothing answers for it — the everyday case of a machine that is
    asleep or off-network.

    It must be LISTED, marked unreachable, and carry a reason. A machine being
    down must never fail the sessions view or surface as an error.
    """
    machine_app.wait_until(
        lambda: next(
            (
                m
                for m in machine_app.call("machines.list")["machines"]
                if m["name"] == "sleepy"
            ),
            {},
        ).get("detail")
        is not None,
        timeout=30,
        what="the unreachable machine to report why",
    )
    sleepy = next(
        m for m in machine_app.call("machines.list")["machines"] if m["name"] == "sleepy"
    )
    assert sleepy["reachable"] is False
    assert sleepy["connected_once"] is False, "it never connected"
    assert sleepy["sessions"] == 0
    assert sleepy["detail"], "an unreachable machine must say WHY"
    # `rc.list` still answers — a down machine does not break the view.
    assert isinstance(machine_app.call("rc.list").get("sessions"), list)


def test_the_agents_pane_renders_machine_rows(machine_app):
    """The rendered truth, not just the payload: the Agents pane reports its
    sessions via `agents.dump`, so the machine row must reach the UI."""
    machine_app.call("ui.navigate", {"pane": "agents"})
    machine_app.wait_until(
        lambda: any(
            s.get("origin_kind") == "machine"
            for s in machine_app.agents_dump()
        ),
        timeout=30,
        what="the machine row to render",
    )
    rendered = machine_app.agents_dump()
    row = next(s for s in rendered if s.get("origin_kind") == "machine")
    assert row["display_name"] == "machine probe"
    assert row["origin"] == "machine:mini3"


def test_killing_a_machine_session_routes_over_the_machine_not_a_server(machine_app):
    """`machine.kill` is addressed by (machine, slug), not (host, shed, slug).

    The kill itself cannot succeed hermetically — it shells `ssh` to a host that
    does not exist — and that is exactly what this asserts: the op must REACH the
    machine transport and fail there, naming the machine. A wrong route would
    instead fail as a bad shed/host lookup, or silently no-op.

    The row must also survive a failed kill: the optimistic drop is only applied
    on success, so a machine that could not be reached keeps its sessions.
    """
    with pytest.raises(ShedError) as excinfo:
        machine_app.call("machine.kill", {"machine": "mini3", "slug": "mch001"})
    message = str(excinfo.value)
    assert "machine:mini3" in message, (
        f"the failure must name the machine it tried to reach: {message!r}"
    )

    # A failed kill leaves the row alone — the snapshot is still authoritative.
    rows = [
        s for s in machine_app.call("rc.list")["sessions"]
        if s.get("origin_kind") == "machine"
    ]
    assert [r["slug"] for r in rows] == ["mch001"], "a failed kill must not drop the row"


def test_killing_an_unknown_machine_is_rejected(machine_app):
    """A slug on a machine that is not configured fails with the configured
    names, rather than a confusing transport error."""
    with pytest.raises(ShedError) as excinfo:
        machine_app.call("machine.kill", {"machine": "ghost", "slug": "x"})
    message = str(excinfo.value)
    assert "ghost" in message
    assert "mini3" in message or "sleepy" in message, (
        f"an unknown machine should name the configured ones: {message!r}"
    )


# ---------------------------------------------------------------------------
# The app HOSTING the hub (plan 012 S4 / roadmap R4's hub-home graduation)
# ---------------------------------------------------------------------------


def test_the_app_hosts_the_rc_hub_when_it_brokers_in_process(mock, tmp_path):
    """**R4's hub-home graduation, proven end to end.**

    The rc-hub role moved out of the `shed-host-agent` bin into
    `shed_broker::rc_hub::role` so a second consumer could host it. This is that
    consumer: with no daemon to broker for it, the app runs the broker
    in-process — and now the hub with it, from the same code the daemon runs.

    Hermetic on two axes: `SHED_TAURI_HOST_AGENT_SOCKET` is cleared so the app
    resolves EMBEDDED mode (rather than dialling a real daemon), and
    `SHED_RC_HUB_ADDR` pins the hub to an ephemeral port instead of the
    production 1029 — otherwise this test would fight a real daemon on the
    developer's machine and two concurrent runs would fight each other.
    """
    import http.client
    import shutil
    import subprocess

    cfg = ui._SUBPROC["tauri"]
    runtime_dir = Path(tempfile.mkdtemp(prefix="shed-e2e-hub-"))
    hub_port = _free_port()
    try:
        shed_config = runtime_dir / "config.yaml"
        shutil.copyfile(FIXTURES / "config.yaml", shed_config)

        env = ui.subproc_env(
            cfg,
            runtime_dir=runtime_dir,
            mock_base_url=mock.base_url,
            config_path=shed_config,
            host_agent_socket=None,
        )
        # No desktop socket => no daemon => EMBEDDED mode, which is the mode that
        # hosts the hub. (subproc_env only sets the key when given one; clear an
        # inherited value so None really means "no daemon".)
        env.pop("SHED_TAURI_HOST_AGENT_SOCKET", None)
        env.pop("SHED_HOST_AGENT_SOCKET_DIR", None)
        # Pin the hub off the production port — see the docstring.
        env["SHED_RC_HUB_ADDR"] = f"127.0.0.1:{hub_port}"

        sock = runtime_dir / cfg.sock_rel
        log = runtime_dir / "hub-ui.log"
        log_fh = open(log, "wb")
        proc = subprocess.Popen(
            [str(cfg.binary)], env=env, stdout=log_fh, stderr=subprocess.STDOUT
        )
        try:
            ui.await_hermetic(
                "tauri", sock=sock, mock_base_url=mock.base_url, proc=proc, log=log
            )
            client = TauriClient(sock)
            try:
                # The app must SERVE the hub wire: identity, and a snapshot. This
                # is the same `/v1` surface `sx watch` and the phone read, so a
                # pass here means the app is a real hub host, not a stub.
                def hub_answers() -> bool:
                    try:
                        conn = http.client.HTTPConnection("127.0.0.1", hub_port, timeout=2)
                        conn.request("GET", "/v1/health")
                        body = json.loads(conn.getresponse().read())
                        conn.close()
                        return body.get("app") == HUB_APP_ID
                    except OSError:
                        return False

                client.wait_until(
                    hub_answers, timeout=60, what="the app's hub to answer /v1/health"
                )

                conn = http.client.HTTPConnection("127.0.0.1", hub_port, timeout=5)
                conn.request("GET", "/v1/sessions")
                snapshot = json.loads(conn.getresponse().read())
                conn.close()
                assert "sessions" in snapshot, f"the hub served no snapshot: {snapshot!r}"
            finally:
                client.close()
        finally:
            ui.terminate(proc)
            log_fh.close()
            # The hub must go away with the app — a leaked listener would hold the
            # port for every later run.
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                try:
                    with socket.create_connection(("127.0.0.1", hub_port), timeout=0.25):
                        time.sleep(0.1)
                except OSError:
                    break
            else:
                pytest.fail("the hub still answers after the app exited")
    finally:
        shutil.rmtree(runtime_dir, ignore_errors=True)
