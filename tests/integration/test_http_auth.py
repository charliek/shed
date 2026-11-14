"""HTTP authorization by client-certificate scope over mtls (live).

The parallel dev servers run `auth.mode: mtls`, the only enforced mode. This
enrols two certificates over the `_bootstrap` SSH channel, one per scope, the
way every client enrols, and drives the pinned HTTPS listener with each:

  - with no client certificate the server gives no HTTP response at all, on
    any route, `/api/info` included (no bootstrap exemptions under mtls);
  - the control plane takes a control certificate and refuses a credentials
    one (403);
  - the credential bus takes a credentials certificate and refuses a control
    one (403);
  - a bearer token is not a credential: the server has no token code, so an
    `Authorization` header neither substitutes for a certificate nor widens
    one's scope;
  - a client that does not pin the server's self-signed certificate cannot
    connect at all.

It replaces the bearer-token scope test that ran against the removed token
mode. Runs against either dev server (`shed_server_dev`): it reads the running
server and changes nothing. The route/scope table itself is Go
(`internal/api/auth_mtls_test.go`).
"""

from __future__ import annotations

import hashlib
import http.client
import ssl
import urllib.error
import urllib.request

import pytest

from fixtures.devcontrol import bootstrap_cert
from fixtures.server import resolve_server_entry
from fixtures.tlsclient import pinned_request

# Shaped like a control token a 0.9.x token-mode server minted.
_BEARER = "Bearer shed_control_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"


def _host(server) -> str:
    entry = resolve_server_entry(server.name)
    assert entry is not None, f"{server.name!r} not found in ~/.shed/config.yaml"
    return entry.get("host", "localhost")


def _bearer_status(host: str, port: int, path: str, pin: str, client_cert) -> int | None:
    """A request carrying a bearer header, pinned by fingerprint."""
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    if client_cert:
        ctx.load_cert_chain(*client_cert)
    conn = http.client.HTTPSConnection(host, port, context=ctx, timeout=15)
    try:
        try:
            conn.connect()
            der = conn.sock.getpeercert(binary_form=True)
            assert "sha256:" + hashlib.sha256(der).hexdigest() == pin, "server certificate is not the pinned one"
            conn.request("GET", path, headers={"Authorization": _BEARER})
            resp = conn.getresponse()
            resp.read()
            return resp.status
        except (ssl.SSLError, ConnectionError, http.client.HTTPException):
            return None
    finally:
        conn.close()


def test_mtls_enforces_certificate_scopes(shed_server_dev, tmp_path):
    server = shed_server_dev
    host = _host(server)
    control = bootstrap_cert(server.name, "control", tmp_path / "control")
    creds = bootstrap_cert(server.name, "credentials", tmp_path / "credentials")
    assert control.pin == creds.pin, "two bootstraps of one server reported different TLS pins"
    port, pin = control.https_port, control.pin
    assert port > 0, f"bootstrap bundle named no https_port: {control.bundle!r}"

    def status(path: str, cert, method: str = "GET") -> int | None:
        return pinned_request(host, port, path, pin, cert.pair if cert else None, method=method)[0]

    # No certificate: refused at the TLS layer, on every route.
    for path in ("/api/info", "/api/ssh-host-key", "/api/sheds", "/api/plugins/listeners"):
        assert status(path, None) is None, f"{path}: an HTTP response without a client certificate"

    # The control plane: control yes, credentials no.
    assert status("/api/info", control) == 200
    assert status("/api/sheds", control) == 200
    assert status("/api/sheds", creds) == 403

    # The credential bus: credentials yes, control no.
    assert status("/api/plugins/listeners", creds) == 200
    assert status("/api/plugins/listeners", control) == 403

    # A bearer header changes nothing: no certificate is still no response,
    # and it does not lift a credentials certificate onto the control plane.
    assert _bearer_status(host, port, "/api/sheds", pin, None) is None
    assert _bearer_status(host, port, "/api/sheds", pin, creds.pair) == 403
    assert _bearer_status(host, port, "/api/sheds", pin, control.pair) == 200


@pytest.mark.vz
def test_unpinned_client_cannot_connect(vz_server_dev, tmp_path):
    """A client that trusts the system CAs instead of the pin fails the
    server-certificate check before anything else, certificate or not."""
    cert = bootstrap_cert(vz_server_dev.name, "control", tmp_path)
    ctx = ssl.create_default_context()
    ctx.load_cert_chain(*cert.pair)
    with pytest.raises(urllib.error.URLError) as exc:
        urllib.request.urlopen(f"https://localhost:{cert.https_port}/api/info", timeout=10, context=ctx)
    assert "CERTIFICATE_VERIFY_FAILED" in str(exc.value), f"expected a cert-verify failure: {exc.value!r}"
