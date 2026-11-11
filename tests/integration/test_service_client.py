"""Service clients (`auth.clients`) against the parallel FC dev server.

Firecracker only, and only against a dev server started in mtls mode
(`SHED_DEV_AUTH_MODE=mtls`), whose rendered config carries the `itest` service
client (`scripts/render-dev-config.sh --mtls`: prefix `itest-svc-`,
`max_sheds: 3`, images `[itest]`, no server mounts).

The bundle is issued the way an operator would, on the dev host:
`sudo -n /tmp/shed-server-dev client issue itest --config /tmp/shed-server-dev.yaml`
over ssh. The client talks HTTPS itself (stdlib `http.client`), pinned by the
bundle's `tls_cert_fingerprint`; the admin side uses the CLI's mtls credential
for the dev entry, as `test_http_exec.py` does.

What it shows, live: a client creates and execs in its own shed, which has no
9P mounts before or after stop/start (an admin shed created by the CLI next to
it does, as the positive control); it cannot see or touch the admin's shed;
policy and route refusals are 403; the admin sees the client's shed with its
owner and can exec in it; the dev server's audit log (read with `sudo cat`)
has the client's api lines and an exec.end line with argv[0] and the count,
never the arguments. An admin create with `"mounts": []` also boots with
no 9P mounts, before and after stop/start.

Run: `make build && make dev-server-restart-fc SHED_DEV_AUTH_MODE=mtls &&
make test-integration-dev-fc SHED_DEV_AUTH_MODE=mtls PYTEST_ARGS="test_service_client.py"`.
"""

from __future__ import annotations

import base64
import http.client
import json
import os
import ssl
import stat
import uuid
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
        f"SHED_DEV_AUTH_MODE={DEV_AUTH_MODE!r} (want 'mtls'): service clients "
        "exist only in mtls mode. Run "
        "`make test-integration-dev-fc SHED_DEV_AUTH_MODE=mtls`."
    ),
)

DEV_BIN = os.environ.get("SHED_FC_DEV_BIN_PATH", "/tmp/shed-server-dev")
DEV_CONFIG = os.environ.get("SHED_FC_DEV_CONFIG", "/tmp/shed-server-dev.yaml")
CLIENT = "itest"
PREFIX = "itest-svc-"
CREATE_TIMEOUT = 300
# The audit log sits in the FC backend's state dir, the parent of the dev
# config's instance_dir (/var/lib/shed-dev/firecracker/instances).
AUDIT_LOG = os.environ.get("SHED_FC_DEV_AUDIT_LOG", "/var/lib/shed-dev/firecracker/audit.jsonl")


class Pinned:
    """An HTTPS client with a client certificate, trusting exactly one
    server certificate (the pin replaces the hostname check)."""

    def __init__(self, host: str, port: int, pin: str, cert_file: str, key_file: str) -> None:
        self.host, self.port = host, port
        pem = server_cert_pem(host, port)
        assert fingerprint(pem) == pin, "the dev server's certificate does not match the pin"
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        ctx.check_hostname = False
        ctx.load_verify_locations(cadata=pem)
        ctx.load_cert_chain(cert_file, key_file)
        self.ctx = ctx

    def request(self, method: str, path: str, body: Optional[dict] = None, timeout: float = 60) -> tuple[int, dict | None]:
        conn = http.client.HTTPSConnection(self.host, self.port, context=self.ctx, timeout=timeout)
        try:
            data = json.dumps(body).encode() if body is not None else None
            headers = {"Content-Type": "application/json"} if data is not None else {}
            conn.request(method, path, body=data, headers=headers)
            resp = conn.getresponse()
            raw = resp.read()
            if resp.getheader("Content-Type", "").startswith("application/x-ndjson"):
                return resp.status, {"frames": [json.loads(line) for line in raw.splitlines() if line.strip()]}
            return resp.status, (json.loads(raw) if raw.strip() else None)
        finally:
            conn.close()

    def exec(self, shed: str, argv: list, timeout_seconds: int = 60) -> tuple[int, bytes, Optional[int], dict | None]:
        """Run argv; returns (status, stdout, exit code, error body)."""
        status, body = self.request(
            "POST", f"/api/sheds/{shed}/exec", {"argv": argv, "timeout_seconds": timeout_seconds},
            timeout=timeout_seconds + 30,
        )
        if status != 200:
            return status, b"", None, body
        out, code = b"", None
        for f in body["frames"]:
            if f["t"] == "stdout":
                out += base64.b64decode(f["d"])
            elif f["t"] == "exit":
                code = f["code"]
        return status, out, code, None


def error_code(body: dict | None) -> Optional[str]:
    return ((body or {}).get("error") or {}).get("code")


def _load_entry(name: str) -> dict:
    data = yaml.safe_load((Path.home() / ".shed" / "config.yaml").read_text()) or {}
    entry = (data.get("servers") or {}).get(name)
    if not entry or entry.get("auth_mode") != "mtls" or not entry.get("client_cert_file"):
        pytest.skip(f"~/.shed/config.yaml has no mtls entry {name!r} with a client certificate")
    return entry


@pytest.fixture(scope="module")
def entry(fc_server_dev) -> dict:
    return _load_entry(fc_server_dev.name)


@pytest.fixture(scope="module")
def admin(entry) -> Pinned:
    url = urlparse(entry["api_url"])
    return Pinned(url.hostname, url.port, entry["tls_cert_fingerprint"], entry["client_cert_file"], entry["client_key_file"])


@pytest.fixture(scope="module")
def bundle(fc_server_dev, tmp_path_factory) -> dict:
    r = fc_server_dev.ssh_run(f"sudo -n {DEV_BIN} client issue {CLIENT} --config {DEV_CONFIG}", timeout=60)
    assert r.returncode == 0, f"client issue failed: {r.stderr}"
    b = json.loads(r.stdout)
    assert set(b) == {
        "client", "auth_mode", "https_port", "tls_cert_fingerprint",
        "client_cert", "client_key", "cert_serial", "expires_at",
    }, b.keys()
    assert b["client"] == CLIENT and b["auth_mode"] == "mtls"
    d = tmp_path_factory.mktemp("svc-bundle")
    for name, key in (("client.pem", "client_cert"), ("client.key", "client_key")):
        p = d / name
        p.write_text(b[key])
        p.chmod(0o600)
    b["cert_file"], b["key_file"] = str(d / "client.pem"), str(d / "client.key")
    return b


@pytest.fixture(scope="module")
def svc(entry, bundle) -> Pinned:
    host = urlparse(entry["api_url"]).hostname
    assert oct(stat.S_IMODE(os.stat(bundle["key_file"]).st_mode)) == "0o600"
    return Pinned(host, bundle["https_port"], bundle["tls_cert_fingerprint"], bundle["cert_file"], bundle["key_file"])


def ninep_mounts(client: Pinned, shed: str) -> list[str]:
    status, out, code, err = client.exec(shed, ["cat", "/proc/mounts"])
    assert status == 200 and code == 0, (status, code, err)
    return [line for line in out.decode().splitlines() if " 9p " in line]


@pytest.fixture(scope="module")
def admin_shed(fc_server_dev):
    name = f"itest-admin-{uuid.uuid4().hex[:8]}"
    fc_server_dev.create(name, image="itest")
    yield name
    fc_server_dev.delete(name, ignore_missing=True)


@pytest.fixture(scope="module")
def client_shed(svc, fc_server_dev):
    name = f"{PREFIX}{uuid.uuid4().hex[:8]}"
    status, body = svc.request("POST", "/api/sheds", {"name": name, "image": "itest"}, timeout=CREATE_TIMEOUT)
    assert status == 201, (status, body)
    yield name, body
    # The client deletes its own shed in a test below; this is the safety net.
    fc_server_dev.delete(name, ignore_missing=True)


def test_info_features(svc):
    status, info = svc.request("GET", "/api/info")
    assert status == 200
    for f in ("service_clients", "http_exec", "mount_selection"):
        assert f in info["features"], info["features"]


def test_client_shed_has_owner_and_no_mounts(svc, client_shed):
    name, created = client_shed
    assert created["owner"] == CLIENT
    assert created["mounts"] == {"mode": "selected"}
    status, got = svc.request("GET", f"/api/sheds/{name}")
    assert status == 200 and got["owner"] == CLIENT
    assert ninep_mounts(svc, name) == []


def test_admin_shed_has_mounts_and_is_invisible_to_the_client(svc, admin, admin_shed):
    # Positive control: the CLI's default create gets every server mount.
    assert ninep_mounts(admin, admin_shed), "an admin shed has no 9P mounts; the no-mount checks would prove nothing"
    status, body = svc.request("GET", "/api/sheds")
    assert status == 200
    names = [s["name"] for s in body["sheds"]]
    assert admin_shed not in names
    assert all(s.get("owner") == CLIENT for s in body["sheds"]), body["sheds"]
    for method, path, payload in (
        ("GET", f"/api/sheds/{admin_shed}", None),
        ("POST", f"/api/sheds/{admin_shed}/exec", {"argv": ["true"]}),
        ("POST", f"/api/sheds/{admin_shed}/stop", None),
        ("DELETE", f"/api/sheds/{admin_shed}", None),
    ):
        status, body = svc.request(method, path, payload)
        assert (status, error_code(body)) == (404, "SHED_NOT_FOUND"), (method, path, status, body)
    # Still running and untouched.
    status, body = admin.request("GET", f"/api/sheds/{admin_shed}")
    assert status == 200 and body["status"] == "running"


@pytest.mark.parametrize(
    "payload, rule",
    [
        ({"name": "not-prefixed-x"}, "must start with"),
        ({"name": PREFIX + "ld", "local_dir": "/tmp"}, "local_dir"),
        ({"name": PREFIX + "img", "image": "full"}, "image"),
        ({"name": PREFIX + "mnt", "mounts": ["claude"]}, "mount"),
    ],
)
def test_policy_refusals(svc, payload, rule):
    status, body = svc.request("POST", "/api/sheds", payload)
    assert (status, error_code(body)) == (403, "POLICY_VIOLATION"), body
    assert body["error"]["message"].startswith(f"client {CLIENT}: ")
    assert rule in body["error"]["message"]


@pytest.mark.parametrize("method, path", [
    ("GET", "/api/system/df"),
    ("GET", "/api/overview"),
    ("GET", "/api/snapshots"),
    ("GET", "/api/plugins/listeners"),
    ("POST", "/api/images/pull"),
])
def test_route_refusals(svc, method, path):
    status, body = svc.request(method, path, {} if method == "POST" else None)
    assert (status, error_code(body)) == (403, "FORBIDDEN"), body


def test_admin_sees_owner_and_can_exec(admin, client_shed):
    name, _ = client_shed
    status, body = admin.request("GET", "/api/sheds")
    assert status == 200
    mine = [s for s in body["sheds"] if s["name"] == name]
    assert mine and mine[0]["owner"] == CLIENT
    status, out, code, _ = admin.exec(name, ["echo", "admin-here"])
    assert (status, code, out) == (200, 0, b"admin-here\n")


def read_audit(fc_server_dev) -> tuple[str, list[dict]]:
    r = fc_server_dev.ssh_run(f"sudo -n cat {AUDIT_LOG}", timeout=30)
    assert r.returncode == 0, f"reading {AUDIT_LOG} failed: {r.stderr}"
    return r.stdout, [json.loads(line) for line in r.stdout.splitlines() if line.strip()]


def test_audit_log_records_client_requests_and_execs(svc, admin, bundle, client_shed, fc_server_dev):
    name, _ = client_shed
    secret = f"audit-secret-{uuid.uuid4().hex}"
    status, out, code, _ = svc.exec(name, ["echo", secret])
    assert (status, code, out) == (200, 0, f"{secret}\n".encode())
    bogus = f"/api/no-such-route-{uuid.uuid4().hex[:8]}"
    status, body = svc.request("GET", bogus)
    assert (status, error_code(body)) == (403, "FORBIDDEN")
    # An admin request, which must not be logged.
    status, _ = admin.request("GET", "/api/sheds")
    assert status == 200

    raw, lines = read_audit(fc_server_dev)
    assert secret not in raw, "the exec's arguments reached the audit log (exec_argv is not full)"

    serial = bundle["cert_serial"]
    api = [l for l in lines if l.get("event") == "api"]
    assert api and all(l["identity"]["kind"] == "client" for l in api), "admin requests must not be logged"
    mine = [l for l in api if l["identity"].get("name") == CLIENT and l["identity"].get("serial") == serial]
    exec_lines = [l for l in mine if l["route"] == "/api/sheds/{name}/exec" and l["shed"] == name and l["method"] == "POST"]
    assert exec_lines and exec_lines[-1]["status"] == 200, exec_lines
    refused = [l for l in mine if l["path"] == bogus]
    assert len(refused) == 1 and refused[0]["status"] == 403 and refused[0]["route"] == "", refused
    for l in mine:
        assert {"ts", "request_id", "remote", "duration_ms"} <= set(l), l

    ends = [
        l for l in lines
        if l.get("event") == "exec.end" and l["shed"] == name
        and l["identity"].get("kind") == "client" and l["identity"].get("serial") == serial
    ]
    echo = [e for e in ends if e.get("argv0") == "echo"]
    assert echo, ends
    e = echo[-1]
    assert e["argc"] == 2 and "argv" not in e, e
    assert e["exit_code"] == 0 and e["timed_out"] is False and e["disconnected"] is False, e


def test_client_stop_start_keeps_no_mounts(svc, client_shed):
    name, _ = client_shed
    status, body = svc.request("POST", f"/api/sheds/{name}/stop", timeout=120)
    assert status == 200 and body["status"] == "stopped", body
    status, body = svc.request("POST", f"/api/sheds/{name}/start", timeout=CREATE_TIMEOUT)
    assert status == 200 and body["status"] == "running", body
    assert ninep_mounts(svc, name) == []


def test_admin_create_with_no_mounts(admin, fc_server_dev):
    name = f"itest-nomnt-{uuid.uuid4().hex[:8]}"
    try:
        status, body = admin.request("POST", "/api/sheds", {"name": name, "image": "itest", "mounts": []}, timeout=CREATE_TIMEOUT)
        assert status == 201, body
        assert body.get("owner") is None
        assert body["mounts"] == {"mode": "selected"}
        assert ninep_mounts(admin, name) == []
        fc_server_dev.stop(name)
        fc_server_dev.start(name)
        assert ninep_mounts(admin, name) == []
    finally:
        fc_server_dev.delete(name, ignore_missing=True)


def test_client_deletes_its_own_and_admin_deletes_a_client_shed(svc, admin, client_shed):
    name, _ = client_shed
    status, _ = svc.request("DELETE", f"/api/sheds/{name}", timeout=120)
    assert status == 204
    status, body = svc.request("GET", f"/api/sheds/{name}")
    assert (status, error_code(body)) == (404, "SHED_NOT_FOUND")

    second = f"{PREFIX}{uuid.uuid4().hex[:8]}"
    status, body = svc.request("POST", "/api/sheds", {"name": second, "image": "itest"}, timeout=CREATE_TIMEOUT)
    assert status == 201, body
    try:
        status, _ = admin.request("DELETE", f"/api/sheds/{second}", timeout=120)
        assert status == 204
        status, body = svc.request("GET", "/api/sheds")
        assert second not in [s["name"] for s in body["sheds"]]
    finally:
        admin.request("DELETE", f"/api/sheds/{second}", timeout=120)
