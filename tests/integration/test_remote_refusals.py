"""Refusals of a `default_backend: remote` server (cloud sheds).

A remote server's sheds are machines of the account service it is enrolled
with (plan 041). It has no host directories, snapshots, egress profiles,
server mounts or image store. It refuses each one with `FEATURE_UNSUPPORTED`
and never with a 409, which a client reads as "the shed exists":

  - a create that asks for one of those fields: 422, before the backend runs,
    so no machine is created and no shed record is left;
  - a route that needs one of them (snapshot, reset, image and egress
    writes): 501, before the handler runs. (`assign`, 501 on every cloud
    shed, needs a shed to exist: `test_remote_compute.py` case 1.)

And what plan 041 added, as the entry's local admin:

  - a local admin creates no cloud shed: 422 `COMPUTE_POLICY_REFUSED` with
    reason `person`, and no shed is left;
  - `lease` on a create is a service client's: 422 `FEATURE_UNSUPPORTED`
    from anyone else;
  - an extend outside 1 to 720 hours is 400, before the shed is looked up;
  - the cloud usage of a local admin is the organization's alone
    (`person` null).

The CLI refuses the same create flags locally before sending anything
(`cmd/shed/settle.go:remoteCreateRefusal`), so these tests talk to the API
directly over pinned HTTPS with the entry's client certificate. No case
creates a machine.

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
    data = yaml.safe_load(((remote_server.home or Path.home()) / ".shed" / "config.yaml").read_text()) or {}
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


def test_local_admins_create_is_refused_for_person(remote_server, api):
    """A local admin manages cloud sheds and creates none: 422
    COMPUTE_POLICY_REFUSED with reason person, before the service is asked
    for a machine, and no shed is left."""
    name = f"itest-refuse-{uuid.uuid4().hex[:8]}"
    try:
        status, payload = api.call("POST", "/api/sheds", {"name": name})
        assert status == 422, f"the local admin's create gave {status}: {payload}"
        assert _error_code(payload) == "COMPUTE_POLICY_REFUSED", payload
        assert (payload.get("error") or {}).get("reason") == "person", payload
        assert name not in remote_server.list_shed_names(), f"a refused create left shed {name!r} behind"
    finally:
        remote_server.delete(name, ignore_missing=True)


def test_a_lease_from_anyone_but_a_client_is_422(remote_server, api):
    """`lease` on a create is a service client's own: 422 FEATURE_UNSUPPORTED
    from a person or a local admin, before anything else is checked."""
    name = f"itest-refuse-{uuid.uuid4().hex[:8]}"
    try:
        status, payload = api.call("POST", "/api/sheds", {"name": name, "lease": "1h"})
        assert status == 422, f"a create with a lease gave {status}: {payload}"
        assert _error_code(payload) == "FEATURE_UNSUPPORTED", payload
        assert name not in remote_server.list_shed_names()
    finally:
        remote_server.delete(name, ignore_missing=True)


@pytest.mark.parametrize("hours", [0, 721, -1])
def test_extend_outside_its_range_is_400(api, hours):
    """An extend of 1 to 720 hours only: anything else is 400
    INVALID_REQUEST before the shed is looked up."""
    status, payload = api.call("POST", "/api/sheds/itest-no-such-shed/extend", {"hours": hours})
    assert status == 400, f"extend by {hours} gave {status}: {payload}"
    assert _error_code(payload) == "INVALID_REQUEST", payload


def test_extend_of_no_such_shed_is_404(api):
    status, payload = api.call("POST", "/api/sheds/itest-no-such-shed/extend", {"hours": 1})
    assert status == 404, f"extend of a missing shed gave {status}: {payload}"
    assert _error_code(payload) == "SHED_NOT_FOUND", payload


def test_cloud_usage_of_a_local_admin_is_the_organizations(api):
    """A local admin is not a person of the service: the usage it reads is
    the organization's alone."""
    status, payload = api.call("GET", "/api/cloud/usage")
    assert status == 200, f"GET /api/cloud/usage gave {status}: {payload}"
    assert payload.get("person") is None, payload
    org = payload.get("org") or {}
    assert {"machines", "running", "stopped", "max"} <= set(org), payload
    assert org["machines"] == org["running"] + org["stopped"], payload
