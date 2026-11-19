"""Service clients (`auth.clients`) against the parallel FC dev server.

Firecracker only, against the FC dev server, which runs mtls: its committed
config (`configs/server.dev-parallel.linux-fc.yaml`) carries the `itest`
service client (prefix `itest-svc-`, `max_sheds: 3`, images `[itest]`, no
server mounts).

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
never the arguments, and an admin's exec in the client's shed is recorded as
a person (`kind: user`, `admin: true`; `fixtures/auditlog.py`): by the
developer's user name on a server with a user list (`SHED_ITEST_USERS=fc`, or
the directory source), as the implicit user on one without. An admin create
with `"mounts": []` also boots with no 9P mounts, before and after stop/start.

Run: `make build && make dev-server-restart-fc &&
make test-integration-dev-fc PYTEST_ARGS="test_service_client.py"`.
"""

from __future__ import annotations

import json
import os
import stat
import uuid
from pathlib import Path
from urllib.parse import urlparse

import pytest
import yaml

from fixtures.auditlog import client_identity_problems, developer_audit_user, person_identity_problems
from fixtures.identity import itest_source, skip_unless_users_source_ready, started_with_users, users_environment
from fixtures.tlsclient import Pinned, error_code

# No auth-mode gate: the FC dev server is always mtls, and `_load_entry`
# skips cleanly when the dev entry has no mtls client certificate.

DEV_BIN = os.environ.get("SHED_FC_DEV_BIN_PATH", "/tmp/shed-server-dev")
DEV_CONFIG = os.environ.get("SHED_FC_DEV_CONFIG", "/tmp/shed-server-dev.yaml")
CLIENT = "itest"
PREFIX = "itest-svc-"
CREATE_TIMEOUT = 300
# The audit log sits in the FC backend's state dir, the parent of the dev
# config's instance_dir (/var/lib/shed-dev/firecracker/instances).
AUDIT_LOG = os.environ.get("SHED_FC_DEV_AUDIT_LOG", "/var/lib/shed-dev/firecracker/audit.jsonl")


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


@pytest.fixture(scope="module")
def fc_users(fc_server_dev):
    """The itest identities, when the FC dev server was started with the
    itest users: with the itest user list (`SHED_ITEST_USERS=fc`), or, under
    the directory source (`SHED_ITEST_SOURCE=directory`), from the directory,
    which the server itself must confirm; skips otherwise. The server is
    never reconfigured here, so the gate follows the run's source like
    `users_env`'s. See `fixtures/identity.py`."""
    try:
        source = itest_source()
    except ValueError as e:
        pytest.fail(str(e))
    skip_unless_users_source_ready("fc", fc_server_dev.name, source)
    with users_environment("fc", fc_server_dev, reconfigure=False, image="itest", source=source) as env:
        yield env


def test_users_and_the_client_do_not_reach_each_others_sheds(svc, client_shed, fc_users, fc_server_dev):
    """With a user list, the client's shed is no user's: a user who is not an
    admin cannot see or enter it, and an admin may enter it (admins may enter
    a service client's sheds). The client cannot see a user's shed."""
    name, _ = client_shed
    alice, admin = fc_users.alice, fc_users.admin

    mine = f"itest-ucs-{uuid.uuid4().hex[:8]}"
    try:
        r = alice.cli("create", mine, "--image", "itest", "--no-sync", timeout=CREATE_TIMEOUT)
        assert r.returncode == 0, f"alice's create failed: {r.stderr[-800:]!r}"
        # Control: alice enters her own shed through the same shim and
        # known_hosts, so the refusal below is about whose shed it is.
        marker = f"itest-entered-{uuid.uuid4().hex[:8]}"
        r = alice.ssh(mine, f"echo {marker}")
        assert r.returncode == 0 and marker in r.stdout, f"alice could not enter her own shed: {r.stderr!r}"

        status, body = alice.api().request("GET", f"/api/sheds/{name}")
        assert (status, error_code(body)) == (404, "SHED_NOT_FOUND"), (status, body)
        assert name not in {s["name"] for s in alice.list_sheds()}
        marker = f"itest-entered-{uuid.uuid4().hex[:8]}"
        r = alice.ssh(name, f"echo {marker}")
        assert r.returncode != 0 and marker not in r.stdout, f"alice entered the client's shed: {r.stdout!r}"
        # The server's refusal (internal/sshd/wake.go), the same words as for
        # a shed that does not exist: not an authentication or connection
        # failure.
        assert f"shed '{name}' not found" in r.stderr, f"not the server's refusal: {r.stderr!r}"

        marker = f"itest-entered-{uuid.uuid4().hex[:8]}"
        r = admin.ssh(name, f"echo {marker}")
        assert r.returncode == 0 and marker in r.stdout, f"an admin could not enter the client's shed: {r.stderr!r}"

        status, body = svc.request("GET", f"/api/sheds/{mine}")
        assert (status, error_code(body)) == (404, "SHED_NOT_FOUND"), (status, body)
        status, body = svc.request("GET", "/api/sheds")
        assert status == 200 and mine not in {s["name"] for s in body["sheds"]}
    finally:
        # Checked: fails naming the shed and server if it cannot be shown gone.
        fc_server_dev.ensure_deleted(mine)


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
    # An admin's HTTPS exec in the client's shed: its exec.end line names a
    # person (kind user), never the retired kind admin.
    status, _, code, _ = admin.exec(name, ["true"])
    assert (status, code) == (200, 0)

    raw, lines = read_audit(fc_server_dev)
    assert secret not in raw, "the exec's arguments reached the audit log (exec_argv is not full)"

    serial = bundle["cert_serial"]
    api = [l for l in lines if l.get("event") == "api"]
    assert api and all(l["identity"]["kind"] == "client" for l in api), "admin requests must not be logged"
    mine = [l for l in api if l["identity"].get("name") == CLIENT and l["identity"].get("serial") == serial]
    for l in mine:
        assert not client_identity_problems(l["identity"]), (client_identity_problems(l["identity"]), l)

    # The shed's name is new to this run, so every line about it was written
    # by this server version: a person's lines are kind user, admin true, and
    # name exactly who the developer's entry is on this server: its own user
    # (`dev`, or `localadmin` under the directory source) on a server with a
    # user list, never the implicit user there; the implicit user on one
    # without.
    try:
        source = itest_source()
    except ValueError as e:
        pytest.fail(str(e))
    want_user = developer_audit_user(source, started_with_users("fc"))
    people = [
        l for l in lines
        if l.get("event") == "exec.end" and l["shed"] == name and l["identity"].get("kind") != "client"
    ]
    assert people, f"no exec.end line for the admin's exec in {name}"
    for l in people:
        problems = person_identity_problems(l["identity"], admin=True, user=want_user)
        assert not problems and "subject" in l["identity"], (problems, l)
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
    assert not client_identity_problems(e["identity"]), (client_identity_problems(e["identity"]), e)
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
