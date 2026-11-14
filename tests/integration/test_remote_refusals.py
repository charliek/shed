"""Refusals of a `default_backend: remote` server (cloud sheds).

A remote server has no host directories, snapshots, egress profiles, server
mounts or image store. It refuses each one with `FEATURE_UNSUPPORTED` and never
with a 409, which a client reads as "the shed exists":

  - a create that asks for one of those fields: 422, before the backend runs,
    so no cloud VM is created and no shed record is left;
  - a route that needs one of them (snapshot, reset, image and egress writes):
    501, before the handler runs.

The CLI refuses the same create flags locally before sending anything
(`cmd/shed/settle.go:remoteCreateRefusal`), so these tests talk to the API
directly over pinned HTTPS with the entry's client certificate. No case
creates a VM.

Runs only against a remote-backend server in mTLS mode
(`make test-integration-remote`); skips otherwise.
"""

from __future__ import annotations

import http.client
import json
import ssl
import uuid
from pathlib import Path
from typing import Optional
from urllib.parse import urlparse

import pytest
import yaml

from fixtures.tlsclient import fingerprint, server_cert_pem


class PinnedAPI:
    """A pinned, client-certificate HTTPS client for one `~/.shed/config.yaml`
    entry (the same trust the CLI uses)."""

    def __init__(self, entry: dict) -> None:
        url = urlparse(entry["api_url"])
        self.host, self.port = url.hostname, url.port
        pem = server_cert_pem(self.host, self.port)
        assert fingerprint(pem) == entry["tls_cert_fingerprint"], (
            "the server's certificate does not match the entry's pin; "
            "re-add the entry after a state_dir change"
        )
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        # The pin replaces the hostname check: trust exactly this cert.
        ctx.check_hostname = False
        ctx.load_verify_locations(cadata=pem)
        ctx.load_cert_chain(entry["client_cert_file"], entry["client_key_file"])
        self.ctx = ctx

    def call(self, method: str, path: str, body: Optional[dict] = None) -> tuple[int, dict]:
        conn = http.client.HTTPSConnection(self.host, self.port, context=self.ctx, timeout=30)
        try:
            data = json.dumps(body).encode() if body is not None else None
            headers = {"Content-Type": "application/json"} if data is not None else {}
            conn.request(method, path, body=data, headers=headers)
            resp = conn.getresponse()
            raw = resp.read()
            try:
                payload = json.loads(raw) if raw else {}
            except json.JSONDecodeError:
                payload = {"raw": raw.decode("utf-8", errors="replace")}
            return resp.status, payload
        finally:
            conn.close()


def _error_code(payload: dict) -> Optional[str]:
    return ((payload or {}).get("error") or {}).get("code")


@pytest.fixture(scope="module")
def api(remote_server) -> PinnedAPI:
    # remote_server's availability probe (`shed -s <entry> list`) has just
    # renewed the entry's client certificate when it was near expiry.
    data = yaml.safe_load((Path.home() / ".shed" / "config.yaml").read_text()) or {}
    entry = (data.get("servers") or {}).get(remote_server.name)
    if not entry or entry.get("auth_mode") != "mtls" or not entry.get("client_cert_file"):
        pytest.skip(
            f"~/.shed/config.yaml has no mtls entry {remote_server.name!r} "
            "with a client certificate"
        )
    return PinnedAPI(entry)


# Each create body asks for exactly one field the remote backend cannot honour.
# `field` is what the server names in its message.
UNSUPPORTED_CREATE_FIELDS = [
    pytest.param({"local_dir": "/tmp"}, "local_dir", id="local_dir"),
    pytest.param({"local_dir": "/tmp", "add_dirs": ["/var/tmp"]}, "local_dir", id="add_dirs"),
    pytest.param({"mounts": ["anything"]}, "mounts", id="mounts"),
    pytest.param({"from_snapshot": "anything"}, "from_snapshot", id="from_snapshot"),
    pytest.param({"egress": ["github"]}, "egress", id="egress"),
]


@pytest.mark.parametrize("extra, field", UNSUPPORTED_CREATE_FIELDS)
def test_unsupported_create_field_is_422(remote_server, api, extra, field):
    """A create asking for an unsupported field is 422 FEATURE_UNSUPPORTED
    and leaves no shed behind."""
    name = f"itest-refuse-{uuid.uuid4().hex[:8]}"
    try:
        status, payload = api.call("POST", "/api/sheds", {"name": name, **extra})
        assert status == 422, f"create with {extra} gave {status}: {payload}"
        assert _error_code(payload) == "FEATURE_UNSUPPORTED", payload
        message = (payload.get("error") or {}).get("message", "")
        assert field in message, f"message does not name {field!r}: {message!r}"
        assert name not in remote_server.list_shed_names(), (
            f"a refused create left shed {name!r} behind"
        )
    finally:
        # A refusal leaves nothing; this only matters if the server
        # regressed into creating the shed (a billed VM).
        remote_server.delete(name, ignore_missing=True)


UNSUPPORTED_ROUTES = [
    pytest.param("POST", "/api/snapshots", {"shed": "x", "name": "y"}, id="snapshot-create"),
    pytest.param("POST", "/api/sheds/itest-no-such-shed/reset", None, id="reset"),
    pytest.param("POST", "/api/images/pull", {"ref": "x"}, id="image-pull"),
    pytest.param("POST", "/api/egress/itest-no-such-shed", {"profiles": ["github"]}, id="egress-set"),
]


@pytest.mark.parametrize("method, path, body", UNSUPPORTED_ROUTES)
def test_unsupported_route_is_501(api, method, path, body):
    """Routes the remote backend lacks answer 501 FEATURE_UNSUPPORTED before
    their handler runs (so the shed need not exist)."""
    status, payload = api.call(method, path, body)
    assert status == 501, f"{method} {path} gave {status}: {payload}"
    assert _error_code(payload) == "FEATURE_UNSUPPORTED", payload
