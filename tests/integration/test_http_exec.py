"""HTTPS exec (`POST /api/sheds/{name}/exec`) against the parallel FC dev server.

Firecracker only, and only against a dev server started in mtls mode
(`SHED_DEV_AUTH_MODE=mtls`): the module authenticates as the admin with the
client certificate the CLI keeps for the dev entry
(`~/.shed/creds/<entry>/client.pem` and `client.key`, refreshed by the
`fc_server_dev` fixture's `shed -s <entry> list` probe), and pins the server by
the entry's `tls_cert_fingerprint`. It talks HTTP itself (stdlib
`http.client`), because the point is the wire: NDJSON frames, exit codes, and
what happens to the guest process on a timeout or a disconnect.

One shed, from the dev config's `itest` alias (`shed-fc-base:v0.9.0`, whose
agent SIGHUPs the command's process group when the host goes away). The
guest agent notices a silent command's lost host through a keepalive probe
every 15 s, so the cleanup probes wait up to 45 s.

Run: `make build && make dev-server-restart-fc SHED_DEV_AUTH_MODE=mtls &&
make test-integration-dev-fc SHED_DEV_AUTH_MODE=mtls PYTEST_ARGS="test_http_exec.py"`.
"""

from __future__ import annotations

import base64
import hashlib
import http.client
import json
import ssl
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional
from urllib.parse import urlparse

import pytest
import yaml

from fixtures.devcontrol import DEV_AUTH_MODE
from fixtures.tlsclient import fingerprint, server_cert_pem

pytestmark = pytest.mark.skipif(
    DEV_AUTH_MODE != "mtls",
    reason=(
        f"SHED_DEV_AUTH_MODE={DEV_AUTH_MODE!r} (want 'mtls'): HTTPS exec is "
        "tested as an mtls admin against the FC dev server. Run "
        "`make test-integration-dev-fc SHED_DEV_AUTH_MODE=mtls`."
    ),
)

# A disconnected or timed-out command dies when the guest agent notices the
# host is gone: at once if it writes, else at the next keepalive probe (15 s).
CLEANUP_WAIT_S = 45


@dataclass
class ExecResult:
    status: int
    error_code: Optional[str] = None
    stdout: bytes = b""
    stderr: bytes = b""
    frames: list = field(default_factory=list)
    exit: Optional[dict] = None


class HTTPSExec:
    """A pinned, client-certificate HTTPS client for one server entry."""

    def __init__(self, entry: dict) -> None:
        url = urlparse(entry["api_url"])
        self.host, self.port = url.hostname, url.port
        pem = server_cert_pem(self.host, self.port)
        assert fingerprint(pem) == entry["tls_cert_fingerprint"], (
            "the dev server's certificate does not match the entry's pin; "
            "re-add the dev entry after a state_dir change"
        )
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        # Trust exactly the pinned certificate. Its SANs need not name the
        # host this suite dials, so the pin replaces the hostname check.
        ctx.check_hostname = False
        ctx.load_verify_locations(cadata=pem)
        ctx.load_cert_chain(entry["client_cert_file"], entry["client_key_file"])
        self.ctx = ctx

    def open(self, method: str, path: str, body: Optional[dict] = None, timeout: float = 30):
        conn = http.client.HTTPSConnection(self.host, self.port, context=self.ctx, timeout=timeout)
        data = json.dumps(body).encode() if body is not None else None
        headers = {"Content-Type": "application/json"} if data is not None else {}
        conn.request(method, path, body=data, headers=headers)
        return conn, conn.getresponse()

    def get_json(self, path: str) -> tuple[int, dict]:
        conn, resp = self.open("GET", path)
        try:
            return resp.status, json.loads(resp.read())
        finally:
            conn.close()

    def exec(self, name: str, body: dict, timeout: float = 30) -> ExecResult:
        conn, resp = self.open("POST", f"/api/sheds/{name}/exec", body, timeout=timeout)
        try:
            if resp.status != 200:
                payload = json.loads(resp.read() or b"{}")
                return ExecResult(resp.status, (payload.get("error") or {}).get("code"))
            assert resp.getheader("Content-Type") == "application/x-ndjson"
            return read_frames(resp)
        finally:
            conn.close()


def read_frames(resp) -> ExecResult:
    """Read an exec stream's (remaining) NDJSON frames to its end."""
    res = ExecResult(200)
    for line in resp:
        frame = json.loads(line)
        res.frames.append(frame)
        if frame["t"] == "exit":
            assert res.exit is None, "two exit frames"
            res.exit = frame
        else:
            data = base64.b64decode(frame["d"])
            if frame["t"] == "stdout":
                res.stdout += data
            else:
                res.stderr += data
    return res


def _load_entry(name: str) -> dict:
    data = yaml.safe_load((Path.home() / ".shed" / "config.yaml").read_text()) or {}
    entry = (data.get("servers") or {}).get(name)
    if not entry or entry.get("auth_mode") != "mtls" or not entry.get("client_cert_file"):
        pytest.skip(f"~/.shed/config.yaml has no mtls entry {name!r} with a client certificate")
    return entry


@pytest.fixture(scope="module")
def client(fc_server_dev) -> HTTPSExec:
    # fc_server_dev's availability probe (`shed -s <entry> list`) has just
    # renewed the entry's client certificate when it was near expiry.
    return HTTPSExec(_load_entry(fc_server_dev.name))


@pytest.fixture(scope="module")
def shed(fc_server_dev, client):
    name = f"itest-http-exec-{uuid.uuid4().hex[:8]}"
    fc_server_dev.create(name, image="itest")
    yield name
    fc_server_dev.delete(name, ignore_missing=True)


def _grep_count(client: HTTPSExec, shed: str, pattern: str) -> int:
    """How many guest processes' command lines match a grep regex. The
    pattern must bracket a character so grep never matches its own line."""
    r = client.exec(shed, {"argv": ["bash", "-c", f"ps -eo args | grep -c '{pattern}' || true"]})
    assert r.exit and r.exit["code"] == 0, r
    return int(r.stdout.strip() or b"0")


@dataclass
class Probe:
    """A command whose shell and whose child each carry their own marker.

    The command is `bash -lc "<prefix>sleep <secs>; : <marker>"`. The trailing
    no-op keeps bash from exec'ing sleep, so the shell stays the parent and
    only its command line carries the marker. The child's own argv is
    `sleep <secs>`, with a fractional length unique to this probe, so a
    cleanup that kills the shell but not its process group is caught.
    """

    marker: str
    secs: str

    @classmethod
    def new(cls, base_secs: int) -> "Probe":
        return cls(f"marker-{uuid.uuid4().hex}", f"{base_secs}.{uuid.uuid4().int % 10**9:09d}")

    def argv(self, prefix: str = "") -> list:
        return ["bash", "-lc", f"{prefix}sleep {self.secs}; : {self.marker}"]

    def counts(self, client: HTTPSExec, shed: str) -> tuple:
        shell = _grep_count(client, shed, f"[{self.marker[0]}]{self.marker[1:]}")
        child = _grep_count(client, shed, "^ *[s]leep " + self.secs.replace(".", "\\."))
        return shell, child

    def assert_running(self, client: HTTPSExec, shed: str) -> None:
        shell, child = self.counts(client, shed)
        assert shell >= 1, "the command's shell is not running"
        assert child >= 1, "the command's child (sleep) is not running"

    def wait_gone(self, client: HTTPSExec, shed: str) -> tuple:
        """Wait for the shell and its child to exit; returns the last counts."""
        deadline = time.time() + CLEANUP_WAIT_S
        while True:
            counts = self.counts(client, shed)
            if counts == (0, 0) or time.time() >= deadline:
                return counts
            time.sleep(1)


def test_info_advertises_http_exec(client):
    status, info = client.get_json("/api/info")
    assert status == 200
    assert "http_exec" in info.get("features", [])


def test_frames_and_exit_code(client, shed):
    r = client.exec(shed, {"argv": ["bash", "-lc", "echo out; echo err >&2; exit 3"]})
    assert r.status == 200
    assert r.stdout == b"out\n"
    assert r.stderr == b"err\n"
    assert (r.exit["code"], r.exit["timed_out"], r.exit["truncated"]) == (3, False, False)
    assert r.frames[-1]["t"] == "exit"


def test_argv_is_literal(client, shed):
    r = client.exec(shed, {"argv": ["echo", "$HOME", "a;b"], "env": {"SHED_T": "x y"}, "cwd": "/tmp"})
    assert r.exit["code"] == 0
    assert r.stdout == b"$HOME a;b\n"
    r = client.exec(shed, {"argv": ["bash", "-c", 'echo "$SHED_T"; pwd'], "cwd": "/tmp", "env": {"SHED_T": "x y"}})
    assert r.stdout == b"x y\n/tmp\n"


def test_stdin_round_trip(client, shed):
    payload = bytes(range(256)) * 64 + b"tail\n"
    r = client.exec(shed, {"argv": ["cat"], "stdin": base64.b64encode(payload).decode()})
    assert r.exit["code"] == 0
    assert r.stdout == payload


def test_timeout_gives_124_and_kills_the_command(client, shed):
    probe = Probe.new(300)
    start = time.time()
    conn, resp = client.open(
        "POST",
        f"/api/sheds/{shed}/exec",
        {"argv": probe.argv("echo started; "), "timeout_seconds": 8},
    )
    try:
        assert resp.status == 200
        first = json.loads(resp.readline())
        assert first["t"] == "stdout" and base64.b64decode(first["d"]) == b"started\n"
        probe.assert_running(client, shed)
        r = read_frames(resp)
    finally:
        conn.close()
    assert r.exit and r.exit["code"] == 124 and r.exit["timed_out"] is True, r.exit
    assert time.time() - start < 25
    assert probe.wait_gone(client, shed) == (0, 0), "the timed-out command (shell, child) is still running"


def test_disconnect_kills_the_command(client, shed):
    probe = Probe.new(600)
    conn, resp = client.open(
        "POST",
        f"/api/sheds/{shed}/exec",
        {"argv": probe.argv("echo started; "), "timeout_seconds": 900},
    )
    try:
        assert resp.status == 200
        first = json.loads(resp.readline())
        assert first["t"] == "stdout" and base64.b64decode(first["d"]) == b"started\n"
        probe.assert_running(client, shed)
    finally:
        conn.close()
    assert probe.wait_gone(client, shed) == (0, 0), "the command (shell, child) survived the client's disconnect"


def test_large_output_arrives_intact(client, shed):
    size = 8 * 1024 * 1024
    r = client.exec(
        shed,
        {
            "argv": ["bash", "-lc", f"head -c {size} /dev/urandom > /tmp/http-exec.bin && cat /tmp/http-exec.bin"],
            "max_output_bytes": 16 * 1024 * 1024,
        },
        timeout=120,
    )
    assert r.exit["code"] == 0 and r.exit["truncated"] is False
    assert len(r.stdout) == size
    s = client.exec(shed, {"argv": ["sha256sum", "/tmp/http-exec.bin"]})
    assert s.stdout.split()[0].decode() == hashlib.sha256(r.stdout).hexdigest()


def test_truncation_keeps_the_exit_code(client, shed):
    r = client.exec(shed, {"argv": ["bash", "-lc", "head -c 1000000 /dev/zero; exit 0"], "max_output_bytes": 1000})
    assert len(r.stdout) == 1000
    assert r.exit["truncated"] is True and r.exit["code"] == 0


def test_long_quiet_stream_is_not_cut(client, shed):
    # Over the server's 60 s ReadTimeout, with no output until the end.
    # Timed here, not by duration_ms alone: the guest's clock can run a little
    # fast, so its `sleep 70` may end a few ms short of 70 s of host time.
    start = time.time()
    r = client.exec(shed, {"argv": ["bash", "-lc", "sleep 70; echo done"], "timeout_seconds": 120}, timeout=150)
    assert r.exit and r.exit["code"] == 0, r
    assert r.stdout == b"done\n"
    assert time.time() - start > 65
    assert r.exit["duration_ms"] > 65_000


def test_missing_shed_is_404_shed_not_found(client):
    r = client.exec(f"itest-no-such-{uuid.uuid4().hex[:8]}", {"argv": ["true"]})
    assert (r.status, r.error_code) == (404, "SHED_NOT_FOUND")


def test_validation_errors_are_json(client, shed):
    assert client.exec(shed, {"argv": []}).status == 400
    r = client.exec(shed, {"argv": ["true"], "user": "root"})
    assert (r.status, r.error_code) == (400, "INVALID_REQUEST")


# Last: it stops the module's shed.
def test_stopped_shed_is_409_shed_not_running(fc_server_dev, client, shed):
    fc_server_dev.stop(shed)
    r = client.exec(shed, {"argv": ["true"]})
    assert (r.status, r.error_code) == (409, "SHED_NOT_RUNNING")
