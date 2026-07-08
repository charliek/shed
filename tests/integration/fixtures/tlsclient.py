"""Pinned-TLS client helpers for the secure-mode integration tests.

Secure mode is TLS-only: the server serves no plain-HTTP listener, only the
pinned-TLS (`https_port`) listener faces clients. The tests therefore drive
their assertions over HTTPS against the server's self-signed cert, trusting it
by the fingerprint the client computes (the pin) rather than a CA chain.

These three helpers were originally local to `test_tls.py`; they are promoted
here so `test_tls.py` and the five secure-mode HTTP tests
(`test_http_auth`, `test_token_ttl`, `test_bootstrap`, `test_cred_bus`) share
one implementation. Public behavior is identical to the prior `test_tls.py`
locals.

  - `server_cert_pem(host, port)` — fetch the presented cert WITHOUT trusting
    it, exactly as `shed server add` does before pinning. The returned PEM is
    the pin material passed back as `ca_pem` to the request helpers.
  - `fingerprint(pem)` — the stable `sha256:<hex>` pin a client computes from
    the cert (used to assert pin stability / rotation).
  - `https_status(port, path, ca_pem, ...)` — issue a pinned-HTTPS request
    (GET, or POST with a JSON body) and return the HTTP status code. When
    `ca_pem` is set it trusts EXACTLY that cert (the pin); when None it uses the
    system CA bundle (which must NOT trust the self-signed cert, so an
    unpinned client cannot connect at all — surfaced as a URLError).
"""

from __future__ import annotations

import hashlib
import json
import ssl
import urllib.error
import urllib.request
from typing import Optional


def server_cert_pem(host: str, port: int) -> str:
    """Retrieve the presented cert without trusting it (the pre-pin fetch
    `shed server add` does)."""
    return ssl.get_server_certificate((host, port))


def fingerprint(pem: str) -> str:
    """The stable `sha256:<hex>` pin a client computes from a cert PEM."""
    der = ssl.PEM_cert_to_DER_cert(pem)
    return "sha256:" + hashlib.sha256(der).hexdigest()


def _ctx(ca_pem: Optional[str]) -> ssl.SSLContext:
    # ca_pem trusts exactly the server's cert (the pin); None uses the system
    # CA bundle (which must NOT trust the self-signed cert).
    if ca_pem:
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        ctx.load_verify_locations(cadata=ca_pem)
        return ctx
    return ssl.create_default_context()


def https_status(
    port: int,
    path: str,
    ca_pem: Optional[str],
    token: Optional[str] = None,
    *,
    host: str = "localhost",
    method: str = "GET",
    body: Optional[dict] = None,
) -> Optional[int]:
    """Issue a pinned-HTTPS request and return the HTTP status code.

    `ca_pem` is the pin (the server's cert PEM from `server_cert_pem`); pass
    None to use the system CA bundle, which won't trust the self-signed cert
    (the caller asserts the resulting URLError). `token`, when set, is sent as
    a Bearer header. For POSTs, set `method="POST"` and pass `body` (encoded as
    JSON with a Content-Type header).
    """
    data = None
    req = urllib.request.Request(f"https://{host}:{port}{path}", method=method)
    if body is not None:
        data = json.dumps(body).encode()
        req.data = data
        req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(req, timeout=10, context=_ctx(ca_pem)) as resp:
            return resp.status
    except urllib.error.HTTPError as e:
        e.close()  # warnings-as-errors: don't leak the response body
        return e.code
