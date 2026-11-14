"""The configured credential lifetime reaches issuance and is enforced (live).

`auth.token_ttl` (the name is kept from the removed bearer-token mode) is the
lifetime of the client certificate the `_bootstrap` channel issues. This
restarts the VZ dev server with a short one and checks the whole path, from
the YAML to the API refusing the credential:

  - the issued certificate's notAfter is the configured lifetime from now,
    not the 24 h default, so the setting reached the bootstrap;
  - the certificate is accepted at once;
  - once it has expired, a new request presenting it is refused;
  - a certificate bootstrapped after that is accepted, so the refusal is the
    expiry and not a server that went away.

It replaces `test_token_ttl.py`, which checked the same wiring for a bearer
token. VZ-only: it restarts the dev server with a changed config
(`dev_config`), which reaches only the local Mac dev server. The expiry check
inside a request, on a reused connection, is Go (`internal/api`).
"""

from __future__ import annotations

import ssl
import subprocess
import time

import pytest

from fixtures.devcontrol import bootstrap_cert, dev_config
from fixtures.server import resolve_server_entry
from fixtures.tlsclient import pinned_request

# Long enough for a bootstrap over loopback SSH and one request; short enough
# to wait out. X.509 times have one-second resolution.
TTL_SECONDS = 6


def _not_after(cert_path) -> float:
    """The certificate's notAfter as a Unix time, read with the openssl CLI
    (which `bootstrap_cert` already needs)."""
    r = subprocess.run(
        ["openssl", "x509", "-in", str(cert_path), "-noout", "-enddate"],
        capture_output=True, text=True, timeout=30,
    )
    assert r.returncode == 0, r.stderr
    _, _, stamp = r.stdout.strip().partition("=")
    return float(ssl.cert_time_to_seconds(stamp))


@pytest.mark.vz
@pytest.mark.slow
def test_configured_cert_lifetime_is_issued_and_enforced(vz_server_dev, tmp_path):
    server = vz_server_dev.name
    with dev_config({"auth": {"token_ttl": f"{TTL_SECONDS}s"}}, server):
        entry = resolve_server_entry(server)
        assert entry is not None, f"{server!r} not found in ~/.shed/config.yaml"
        host = entry.get("host", "localhost")

        before = time.time()
        cert = bootstrap_cert(server, "control", tmp_path / "short")
        not_after = _not_after(cert.cert_path)
        # The configured lifetime, not the default: notAfter is now + TTL
        # (one second of slack each way for X.509's whole seconds).
        assert before + TTL_SECONDS - 1 <= not_after <= time.time() + TTL_SECONDS + 1, (
            f"notAfter is {not_after - before:.0f}s after the bootstrap; auth.token_ttl is {TTL_SECONDS}s"
        )

        # Fresh: accepted.
        status, _ = pinned_request(host, cert.https_port, "/api/sheds", cert.pin, cert.pair)
        assert status == 200, f"a freshly issued certificate got {status}"

        # Expired: refused. A new connection fails the TLS handshake (no HTTP
        # response, None); a 401 would be the per-request check refusing it.
        time.sleep(max(0.0, not_after - time.time()) + 2)
        status, _ = pinned_request(host, cert.https_port, "/api/sheds", cert.pin, cert.pair)
        assert status in (None, 401), f"an expired certificate got {status}"

        # The control: the same server still serves a certificate it has just
        # issued, so the refusal above was the expiry.
        fresh = bootstrap_cert(server, "control", tmp_path / "fresh")
        status, _ = pinned_request(host, fresh.https_port, "/api/sheds", fresh.pin, fresh.pair)
        assert status == 200, f"a certificate issued after the expiry got {status}"
