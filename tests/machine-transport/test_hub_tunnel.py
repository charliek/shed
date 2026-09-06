"""The forwarded-hub family: `ssh -L` to a hub, through a real sshd.

**Why this family exists, stated bluntly.** Plan 012 S2 found that every event
from a directly-read hub was being dropped at decode — a client opened a healthy
tunnel to a healthy hub, connected its SSE stream, and rendered nothing forever.
A "the tunnel is up and /v1/health answers" check passes with a completely dead
feed. That was the actual bug.

So the assertions here are about DELIVERED FRAMES, not liveness: the tunnel must
carry the snapshot, the SSE stream, and the specific frame shape a machine hub
emits — which notably carries an EMPTY `shed`, because a hub read directly has no
shed to name.

The hub is a stdlib HTTP server in-process, shaped byte-for-byte like the real
one (verified against a live `shed-host-agent rc-hub` on mini3). No Rust, no
network beyond loopback.
"""

from __future__ import annotations

import json
import socket
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from conftest import _free_port, isolation_argv

# The identity token the real hub returns from /v1/health (Go `rc.HubAppID`).
HUB_APP_ID = "shed-rc-hub"

# A snapshot + two frames captured off a live machine hub (mini3, plan 012 S2).
# Note the EMPTY `shed` on both frames — that is the shape that was being
# dropped, and pinning it here is the point of the family.
SNAPSHOT = {
    "sessions": [
        {
            "slug": "hkn4vd",
            "tmux_session": "rc-hkn4vd",
            "kind": "shell",
            "state": "ready",
            "managed": True,
            "lane": "tui",
            "display_name": "plan012-probe",
            "workdir": "/home/charliek",
            "created_by": "sx",
            "target_label": "machine:mini3",
            "activity": "working",
        }
    ]
}

FRAMES = (
    "event: session.updated\n"
    'data: {"shed":"","slug":"evtprb","session":{"slug":"evtprb",'
    '"tmux_session":"rc-evtprb","kind":"shell","state":"ready","managed":true,'
    '"lane":"tui","display_name":"evtprobe","target_label":"local"}}\n'
    "\n"
    "event: activity.changed\n"
    'data: {"shed":"","slug":"evtprb","activity":"working",'
    '"activity_at":"2026-08-22T02:05:30Z","state":"ready"}\n'
    "\n"
)


class _HubHandler(BaseHTTPRequestHandler):
    def log_message(self, *args):  # keep the test output clean
        pass

    def do_GET(self):  # noqa: N802 (stdlib API)
        if self.path == "/v1/health":
            self._json({"app": HUB_APP_ID, "version": "machine-transport-fake"})
        elif self.path == "/v1/sessions":
            self._json(SNAPSHOT)
        elif self.path == "/v1/events":
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.end_headers()
            self.wfile.write(FRAMES.encode())
            self.wfile.flush()
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


@pytest.fixture(scope="module")
def fake_hub():
    port = _free_port()
    server = ThreadingHTTPServer(("127.0.0.1", port), _HubHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    yield port
    server.shutdown()
    server.server_close()


def _forward(sshd: dict, local_port: int, remote_port: int) -> subprocess.Popen:
    """`ssh -N -L` with the same posture `shed_core::machine::forward_argv` uses,
    including `ExitOnForwardFailure` so a lost local port fails loudly instead of
    forwarding nothing — plus the hermeticity options (see `isolation_argv`).

    stderr is CAPTURED, not inherited: a test that asserts "this failed" has to be
    able to say *why* it failed, or any ssh error at all satisfies it.
    """
    argv = (
        ["ssh", "-N", "-o", "ExitOnForwardFailure=yes"]
        + isolation_argv(sshd)
        + [
            "-L",
            f"127.0.0.1:{local_port}:127.0.0.1:{remote_port}",
            f"{sshd['user']}@127.0.0.1",
        ]
    )
    return subprocess.Popen(
        argv,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.PIPE,
        text=True,
    )


def _wait_ready(port: int, proc: subprocess.Popen, timeout: float = 10.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise RuntimeError(f"the forward exited immediately (rc={proc.returncode})")
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.25):
                return
        except OSError:
            time.sleep(0.05)
    raise RuntimeError("the forward never came up")


@pytest.mark.live
def test_a_forwarded_hub_delivers_health_snapshot_and_FRAMES(sshd, fake_hub):
    """End to end through a real sshd: health, the snapshot, and — the part that
    was broken — actual SSE frames.

    The frame assertions are deliberately about CONTENT. Asserting only that the
    stream opened is exactly the check that passed while the feed was dead.
    """
    import http.client

    local = _free_port()
    proc = _forward(sshd, local, fake_hub)
    try:
        _wait_ready(local, proc)

        conn = http.client.HTTPConnection("127.0.0.1", local, timeout=10)

        conn.request("GET", "/v1/health")
        health = json.loads(conn.getresponse().read())
        assert health["app"] == HUB_APP_ID, "the forward reached something that is not a hub"

        conn.request("GET", "/v1/sessions")
        snapshot = json.loads(conn.getresponse().read())
        assert [s["slug"] for s in snapshot["sessions"]] == ["hkn4vd"]
        # The snapshot is the resync: it must carry the activity dimension the
        # one-shot `list` never has.
        assert snapshot["sessions"][0]["activity"] == "working"

        conn.request("GET", "/v1/events", headers={"Accept": "text/event-stream"})
        body = conn.getresponse().read().decode()
        conn.close()

        events = [
            line.split(": ", 1)[1]
            for line in body.splitlines()
            if line.startswith("event: ")
        ]
        assert events == ["session.updated", "activity.changed"], (
            f"the tunnel did not carry both frames intact: {body!r}"
        )

        # THE REGRESSION: a machine hub names no shed. A client that requires a
        # non-empty `shed` drops every one of these, which is precisely what
        # happened. Pin the shape here too, so the harness would have caught it.
        payloads = [
            json.loads(line.split(": ", 1)[1])
            for line in body.splitlines()
            if line.startswith("data: ")
        ]
        assert all(p["shed"] == "" for p in payloads), (
            "a directly-read hub must send an empty shed — if this fixture ever "
            "grows a non-empty one it stops covering the bug it exists for"
        )
        assert [p["slug"] for p in payloads] == ["evtprb", "evtprb"]
    finally:
        proc.terminate()
        proc.communicate(timeout=5)


@pytest.mark.live
def test_a_taken_local_port_fails_loudly(sshd, fake_hub):
    """`ExitOnForwardFailure=yes` must turn a taken local port into an immediate
    non-zero exit, not a tunnel that silently forwards nothing.

    A silent no-op forward is the worst failure mode available here: the client
    would connect to whatever else holds the port and report a confusing error
    far from the cause.
    """
    # Holding the v4 address is now sufficient because the forward pins its bind
    # address (`-L 127.0.0.1:<port>:…`, matching what `forward_argv` emits and
    # what the hub client dials). Before that pin, ssh bound whatever `localhost`
    # resolved to — on a dual-stack host ::1 AND 127.0.0.1 — so holding only v4
    # let ssh bind ::1, succeed, and never trip ExitOnForwardFailure.
    v4 = socket.socket(socket.AF_INET)
    v4.bind(("127.0.0.1", 0))
    v4.listen(1)
    taken = v4.getsockname()[1]

    try:
        proc = _forward(sshd, taken, fake_hub)
        try:
            _, stderr = proc.communicate(timeout=20)
        except subprocess.TimeoutExpired:
            proc.terminate()
            proc.communicate(timeout=5)
            pytest.fail(
                "the forward did not exit on a taken local port — "
                "ExitOnForwardFailure is not doing its job"
            )
        assert proc.returncode != 0, "a taken local port must be a non-zero exit"
        # …and it must have failed FOR THE RIGHT REASON. Asserting only `rc != 0`
        # would be satisfied by any ssh failure at all — a bad key, an auth
        # refusal, a stray ssh_config — which is exactly how this test could go
        # green while proving nothing.
        assert "forward" in stderr.lower() or "bind" in stderr.lower(), (
            f"exited non-zero, but not with a forwarding failure: {stderr!r}"
        )
    finally:
        v4.close()


@pytest.mark.live
def test_the_forward_dies_with_its_client(sshd, fake_hub):
    """Killing the forward makes the local port stop answering — the property the
    client's kill-on-drop guard depends on for not leaking tunnels."""
    local = _free_port()
    proc = _forward(sshd, local, fake_hub)
    try:
        # `_wait_ready` can raise (the forward died, or never came up), and a
        # bare raise here would leak the ssh child into the rest of the session.
        _wait_ready(local, proc)
        proc.terminate()
        proc.communicate(timeout=5)

        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            try:
                with socket.create_connection(("127.0.0.1", local), timeout=0.25):
                    time.sleep(0.05)
            except OSError:
                return
        pytest.fail("the local port still answers after the forward was killed")
    finally:
        if proc.poll() is None:
            proc.terminate()
            proc.communicate(timeout=5)
