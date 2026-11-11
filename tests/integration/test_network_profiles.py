"""Network profiles (`open`, `isolated`) against the parallel FC dev server.

Firecracker only, against a dev server started in mtls mode
(`SHED_DEV_AUTH_MODE=mtls`), whose rendered config carries the `itest` service
client with no `network:` in its policy (so its sheds are isolated by default).

What it shows, live, from inside real guests (probes are
`timeout 5 bash -c 'exec 3<>/dev/tcp/HOST/PORT'`, and `curl` for a source
address): an isolated shed reaches the internet and resolves names, and cannot
reach the gateway's ports (the dev server's SSH and HTTPS), the host's LAN and
tailnet addresses, cloud metadata, or another shed; a second address it adds
to its NIC gets nowhere; an open shed next to it is unchanged; the service
client's shed is isolated without asking. On mini3, `nft` shows the dev
server's tables with exactly the guarded addresses; `make dev-server-down-fc`
removes only those two tables (shed-br0, the NAT rule and the FORWARD rules
compare equal), and `make dev-server-up-fc` rebuilds them from metadata; a
table deleted by hand is back within 40 s (the presence check, logged); the
tables are gone once the last isolated shed is deleted.

The isolated profile's rule set itself is proven first, as root inside
network namespaces, by `make test-netns-remote`.

Run: `make build && make dev-server-restart-fc SHED_DEV_AUTH_MODE=mtls &&
make test-integration-dev-fc SHED_DEV_AUTH_MODE=mtls PYTEST_ARGS="test_network_profiles.py"`.
"""

from __future__ import annotations

import http.client
import json
import os
import re
import ssl
import subprocess
import time
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
        f"SHED_DEV_AUTH_MODE={DEV_AUTH_MODE!r} (want 'mtls'): run "
        "`make test-integration-dev-fc SHED_DEV_AUTH_MODE=mtls`."
    ),
)

REPO_ROOT = Path(__file__).resolve().parents[2]
DEV_BIN = os.environ.get("SHED_FC_DEV_BIN_PATH", "/tmp/shed-server-dev")
DEV_CONFIG = os.environ.get("SHED_FC_DEV_CONFIG", "/tmp/shed-server-dev.yaml")
DEV_LOG = os.environ.get("SHED_FC_DEV_LOG_PATH", "/tmp/shed-server-dev.log")
TABLE = "shed_mini3_dev"  # the dev config's `name: mini3-dev`
GATEWAY = "172.30.0.1"
DEV_SSH_PORT, DEV_HTTPS_PORT = 12222, 18443
SPOOF_IP = "172.30.0.250"  # a bridge address no shed holds (checked below)
CLIENT, PREFIX = "itest", "itest-svc-"
CREATE_TIMEOUT = 300


class Pinned:
    """An HTTPS client with a client certificate, trusting exactly the pinned
    server certificate."""

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
            return resp.status, (json.loads(raw) if raw.strip() else None)
        finally:
            conn.close()


def shed(server, *args: str, timeout: int = 60) -> subprocess.CompletedProcess:
    return subprocess.run(["shed", "-s", server.name, *args], capture_output=True, text=True, timeout=timeout)


def reach(server, name: str, host: str, port: int) -> bool:
    """Whether a TCP connection from inside shed `name` to host:port opens."""
    r = server.exec(name, ["bash", "-c", f"timeout 5 bash -c 'exec 3<>/dev/tcp/{host}/{port}'"], timeout=60)
    return r.returncode == 0


def https_from(server, name: str, source: Optional[str] = None) -> bool:
    """Whether `curl https://example.com` from shed `name` (optionally bound
    to source) completes."""
    argv = ["curl", "-sS", "-o", "/dev/null", "-m", "8"]
    if source:
        argv += ["--interface", source]
    r = server.exec(name, argv + ["https://example.com"], timeout=60)
    return r.returncode == 0


def host_run(server, command: str, timeout: int = 30) -> subprocess.CompletedProcess:
    return server.ssh_run(command, timeout=timeout)


def guarded_ips(server) -> Optional[set[str]]:
    """The dev server's guarded4 set on mini3, or None when its table is gone."""
    r = host_run(server, f"sudo -n nft -j list set inet {TABLE} guarded4")
    if r.returncode != 0:
        return None
    for item in json.loads(r.stdout)["nftables"]:
        if "set" in item:
            return set(item["set"].get("elem", []))
    return set()


def tables(server) -> set[str]:
    r = host_run(server, "sudo -n nft list tables")
    assert r.returncode == 0, r.stderr
    return set(line.strip() for line in r.stdout.splitlines() if line.strip())


def shared_host_state(server) -> str:
    """The shared bridge, NAT and FORWARD state no step may change."""
    r = host_run(
        server,
        # The bridge's existence, kind and address (not its carrier flags,
        # which follow the TAPs on it).
        "ip -d -o link show shed-br0 | grep -o 'shed-br0: \\|bridge '; ip -4 -o addr show dev shed-br0 | awk '{print $4}'; "
        "sudo -n iptables -t nat -S POSTROUTING; sudo -n iptables -S FORWARD",
    )
    assert r.returncode == 0, r.stderr
    return r.stdout


def shed_ip(server, name: str) -> str:
    r = shed(server, "--json", "list")
    assert r.returncode == 0, r.stderr
    for s in json.loads(r.stdout):
        if s["name"] == name:
            return s["ip_address"]
    raise AssertionError(f"{name} not listed")


def shed_json(server, name: str) -> dict:
    r = shed(server, "--json", "list")
    assert r.returncode == 0, r.stderr
    return next(s for s in json.loads(r.stdout) if s["name"] == name)


def ensure_running(server, name: str) -> None:
    if shed_json(server, name)["status"] != "running":
        server.start(name, timeout=CREATE_TIMEOUT)


def make(*args: str, timeout: int = 300) -> subprocess.CompletedProcess:
    r = subprocess.run(["make", *args], cwd=REPO_ROOT, capture_output=True, text=True, timeout=timeout)
    assert r.returncode == 0, f"make {' '.join(args)} failed: {r.stdout[-2000:]} {r.stderr[-2000:]}"
    return r


def wait_listed(server, timeout: float = 90) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if shed(server, "list", timeout=20).returncode == 0:
            return
        time.sleep(2)
    raise AssertionError("dev server did not come back")


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
def svc(fc_server_dev, entry, tmp_path_factory) -> Pinned:
    r = fc_server_dev.ssh_run(f"sudo -n {DEV_BIN} client issue {CLIENT} --config {DEV_CONFIG}", timeout=60)
    assert r.returncode == 0, f"client issue failed: {r.stderr}"
    b = json.loads(r.stdout)
    d = tmp_path_factory.mktemp("netsvc")
    for fname, key in (("client.pem", "client_cert"), ("client.key", "client_key")):
        (d / fname).write_text(b[key])
        (d / fname).chmod(0o600)
    host = urlparse(entry["api_url"]).hostname
    return Pinned(host, b["https_port"], b["tls_cert_fingerprint"], str(d / "client.pem"), str(d / "client.key"))


@pytest.fixture(scope="module")
def baseline(fc_server_dev) -> str:
    return shared_host_state(fc_server_dev)


@pytest.fixture(scope="module")
def sheds(fc_server_dev, svc, baseline):
    """An isolated admin shed, an open admin shed and a service client's shed
    (which asks for no profile)."""
    suffix = uuid.uuid4().hex[:8]
    iso, opn, cli = f"itest-net-iso-{suffix}", f"itest-net-open-{suffix}", f"{PREFIX}net-{suffix}"
    names = [iso, opn, cli]
    try:
        r = shed(fc_server_dev, "create", iso, "--image", "itest", "--network", "isolated", "--no-sync", timeout=CREATE_TIMEOUT)
        assert r.returncode == 0, f"create --network isolated failed: {r.stdout!r} {r.stderr!r}"
        fc_server_dev.create(opn, image="itest", timeout=CREATE_TIMEOUT)
        status, body = svc.request("POST", "/api/sheds", {"name": cli, "image": "itest"}, timeout=CREATE_TIMEOUT)
        assert status == 201, (status, body)
        yield {"iso": iso, "open": opn, "client": cli, "client_body": body}
    finally:
        for n in names:
            fc_server_dev.delete(n, ignore_missing=True)


def test_feature_and_profiles(admin, fc_server_dev, sheds):
    status, info = admin.request("GET", "/api/info")
    assert status == 200 and "network_profiles" in info["features"], info
    assert shed_json(fc_server_dev, sheds["iso"])["network_profile"] == "isolated"
    assert shed_json(fc_server_dev, sheds["open"])["network_profile"] == "open"
    # The client asked for nothing; its policy pins isolated.
    assert sheds["client_body"]["network_profile"] == "isolated"
    r = shed(fc_server_dev, "-v", "list")
    assert r.returncode == 0
    header = r.stdout.splitlines()[0].split()
    row = next(l.split() for l in r.stdout.splitlines() if l.startswith(sheds["iso"] + " "))
    assert row[header.index("NETWORK")] == "isolated", r.stdout


def test_nft_holds_exactly_the_guarded_addresses(fc_server_dev, sheds):
    want = {shed_ip(fc_server_dev, sheds["iso"]), shed_ip(fc_server_dev, sheds["client"])}
    assert guarded_ips(fc_server_dev) == want
    assert shed_ip(fc_server_dev, sheds["open"]) not in want
    assert {f"table bridge {TABLE}", f"table inet {TABLE}"} <= tables(fc_server_dev)


def test_isolated_reaches_the_internet(fc_server_dev, sheds):
    name = sheds["iso"]
    assert reach(fc_server_dev, name, "example.com", 443)
    r = fc_server_dev.exec(name, ["getent", "ahostsv4", "example.com"])
    assert r.returncode == 0 and r.stdout.strip(), r.stderr
    assert https_from(fc_server_dev, name)


def test_isolated_reaches_nothing_else(fc_server_dev, sheds):
    lan = host_run(fc_server_dev, "ip -4 -o addr show scope global | awk '$2 !~ /^(shed-br0|docker|br-|tailscale)/ {sub(/\\/.*/, \"\", $4); print $4; exit}'").stdout.strip()
    ts = host_run(fc_server_dev, "ip -4 -o addr show dev tailscale0 | awk '{sub(/\\/.*/, \"\", $4); print $4; exit}'").stdout.strip()
    assert re.fullmatch(r"\d+\.\d+\.\d+\.\d+", lan), lan
    iso, opn = sheds["iso"], sheds["open"]
    open_ip, client_ip = shed_ip(fc_server_dev, opn), shed_ip(fc_server_dev, sheds["client"])

    # Positive controls: an open shed reaches the dev server's SSH port on
    # the gateway, the LAN and the tailnet address, and the other open
    # shed's sshd, so the failures below are the profile's doing. (mini3
    # runs no sshd on 22; the dev server listens on every address.)
    assert reach(fc_server_dev, opn, GATEWAY, DEV_SSH_PORT)
    assert reach(fc_server_dev, opn, lan, DEV_SSH_PORT)
    if ts:
        assert reach(fc_server_dev, opn, ts, DEV_SSH_PORT)
    assert reach(fc_server_dev, opn, open_ip, 22)
    # A guarded shed refuses new inbound connections, even from an open one.
    assert not reach(fc_server_dev, opn, shed_ip(fc_server_dev, iso), 22)

    targets = [
        (GATEWAY, DEV_SSH_PORT), (GATEWAY, DEV_HTTPS_PORT), (lan, DEV_SSH_PORT),
        ("169.254.169.254", 80), (open_ip, 22), (client_ip, 22),
    ]
    if ts:
        targets.append((ts, DEV_SSH_PORT))
    for host, port in targets:
        assert not reach(fc_server_dev, iso, host, port), f"isolated shed reached {host}:{port}"
    # The client's shed is isolated too.
    assert not reach(fc_server_dev, sheds["client"], GATEWAY, DEV_SSH_PORT)
    assert reach(fc_server_dev, sheds["client"], "example.com", 443)


def test_isolated_cannot_use_another_source(fc_server_dev, sheds):
    iso = sheds["iso"]
    r = host_run(fc_server_dev, f"ip neigh show {SPOOF_IP} dev shed-br0")
    assert SPOOF_IP not in r.stdout, f"{SPOOF_IP} is in use on the bridge: {r.stdout}"
    r = fc_server_dev.exec(iso, ["sudo", "ip", "addr", "add", f"{SPOOF_IP}/24", "dev", "eth0"])
    assert r.returncode == 0, r.stderr
    try:
        assert not https_from(fc_server_dev, iso, SPOOF_IP), "a spoofed source reached the internet"
        assert https_from(fc_server_dev, iso, shed_ip(fc_server_dev, iso)), "the shed's own address stopped working"
    finally:
        fc_server_dev.exec(iso, ["sudo", "ip", "addr", "del", f"{SPOOF_IP}/24", "dev", "eth0"])


def test_down_removes_only_its_tables_and_up_rebuilds(fc_server_dev, sheds, baseline):
    before = guarded_ips(fc_server_dev)
    assert before
    make("dev-server-down-fc")
    try:
        assert not {f"table bridge {TABLE}", f"table inet {TABLE}"} & tables(fc_server_dev), "teardown left a table"
        assert shared_host_state(fc_server_dev) == baseline, "dev-server-down-fc changed the bridge, NAT or FORWARD rules"
    finally:
        make("dev-server-up-fc", "SHED_DEV_AUTH_MODE=mtls")
        wait_listed(fc_server_dev)
    assert guarded_ips(fc_server_dev) == before, "the rebuild from metadata differs"
    r = host_run(fc_server_dev, f"sudo -n grep -c 'Network policy: tables bridge/inet {TABLE} installed' {DEV_LOG}")
    assert r.stdout.strip() not in ("", "0"), "no install line in the new server's log"
    ensure_running(fc_server_dev, sheds["iso"])
    assert not reach(fc_server_dev, sheds["iso"], GATEWAY, DEV_SSH_PORT)
    assert reach(fc_server_dev, sheds["iso"], "example.com", 443)


def test_deleted_table_is_reapplied(fc_server_dev, sheds):
    before = guarded_ips(fc_server_dev)
    r = host_run(fc_server_dev, f"sudo -n nft delete table inet {TABLE}")
    assert r.returncode == 0, r.stderr
    deadline = time.monotonic() + 40
    while time.monotonic() < deadline and guarded_ips(fc_server_dev) is None:
        time.sleep(2)
    assert guarded_ips(fc_server_dev) == before, "the table did not come back within 40 s"
    r = host_run(fc_server_dev, f"sudo -n grep -c 'tables {TABLE} reapplied' {DEV_LOG}")
    assert r.stdout.strip() not in ("", "0"), "no reapply line in the log"


def test_open_shed_unchanged_and_tables_go_with_the_last_guarded_shed(fc_server_dev, sheds, baseline):
    opn = sheds["open"]
    ensure_running(fc_server_dev, opn)
    assert reach(fc_server_dev, opn, GATEWAY, DEV_SSH_PORT)
    assert reach(fc_server_dev, opn, "example.com", 443)

    iso_ip = shed_ip(fc_server_dev, sheds["iso"])
    fc_server_dev.delete(sheds["iso"])
    remaining = guarded_ips(fc_server_dev)
    assert remaining is not None and iso_ip not in remaining
    fc_server_dev.delete(sheds["client"])
    assert not {f"table bridge {TABLE}", f"table inet {TABLE}"} & tables(fc_server_dev), "tables kept with no guarded shed"
    assert shared_host_state(fc_server_dev) == baseline
