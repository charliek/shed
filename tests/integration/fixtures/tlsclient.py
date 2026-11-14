"""Pinned-TLS client helpers for the integration tests that drive the HTTPS
API directly.

An mtls server is TLS-only: it serves no plain-HTTP listener, only the
pinned-TLS (`https_port`) listener, and that listener requires a client
certificate the server's CA issued (`fixtures.devcontrol.bootstrap_cert`).
These helpers drive requests over it against the server's self-signed cert,
trusting it by the cert itself (the pin) rather than a CA chain, and present
a client certificate when given one. No helper here sends a bearer token:
the server has no token mode any more.

  - `server_cert_pem(host, port)` — fetch the presented cert WITHOUT trusting
    it, exactly as a client does before pinning. The returned PEM is the pin
    material passed back as `ca_pem` to `https_status`.
  - `fingerprint(pem)` — the stable `sha256:<hex>` pin a client computes from
    the cert (used to assert pin stability / rotation).
  - `https_status(port, path, ca_pem, client_cert, ...)` — a pinned-HTTPS
    request to `localhost` (GET, or POST with a JSON body) that returns the
    HTTP status code. When `ca_pem` is set it trusts EXACTLY that cert, with
    hostname verification against its SANs; when None it uses the system CA
    bundle (which must NOT trust the self-signed cert, so an unpinned client
    cannot connect at all — surfaced as a URLError).
  - `pinned_request(host, port, path, pin, client_cert, ...)` — the same for
    any host, pinned by fingerprint (the form a bootstrap bundle carries)
    instead of by hostname, returning `(status, decoded JSON body)`, or
    `(None, None)` when no HTTP response arrives at all (an mtls listener
    refuses a request without a client certificate during the handshake).
"""

from __future__ import annotations

import hashlib
import http.client
import json
import ssl
import urllib.error
import urllib.request
from typing import Optional


def server_cert_pem(host: str, port: int) -> str:
    """Retrieve the presented cert without trusting it (the pre-pin fetch a
    client does). Works against an mtls listener too: under TLS 1.3 the
    client sees the server's certificate before the server judges the
    client's."""
    return ssl.get_server_certificate((host, port))


def fingerprint(pem: str) -> str:
    """The stable `sha256:<hex>` pin a client computes from a cert PEM."""
    der = ssl.PEM_cert_to_DER_cert(pem)
    return "sha256:" + hashlib.sha256(der).hexdigest()


def _ctx(ca_pem: Optional[str], client_cert: Optional[tuple[str, str]]) -> ssl.SSLContext:
    # ca_pem trusts exactly the server's cert (the pin); None uses the system
    # CA bundle (which must NOT trust the self-signed cert).
    if ca_pem:
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        ctx.load_verify_locations(cadata=ca_pem)
    else:
        ctx = ssl.create_default_context()
    if client_cert:
        ctx.load_cert_chain(*client_cert)
    return ctx


def https_status(
    port: int,
    path: str,
    ca_pem: Optional[str],
    client_cert: Optional[tuple[str, str]] = None,
    *,
    host: str = "localhost",
    method: str = "GET",
    body: Optional[dict] = None,
) -> Optional[int]:
    """Issue a pinned-HTTPS request and return the HTTP status code.

    `ca_pem` is the pin (the server's cert PEM from `server_cert_pem`); pass
    None to use the system CA bundle, which won't trust the self-signed cert
    (the caller asserts the resulting URLError). `client_cert`, when set, is
    the `(cert, key)` file pair presented at the handshake. For POSTs, set
    `method="POST"` and pass `body` (encoded as JSON with a Content-Type
    header).
    """
    req = urllib.request.Request(f"https://{host}:{port}{path}", method=method)
    if body is not None:
        req.data = json.dumps(body).encode()
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=10, context=_ctx(ca_pem, client_cert)) as resp:
            return resp.status
    except urllib.error.HTTPError as e:
        e.close()  # warnings-as-errors: don't leak the response body
        return e.code


class PinMismatch(AssertionError):
    """The server presented a certificate other than the pinned one."""


def pinned_request(
    host: str,
    port: int,
    path: str,
    pin: str,
    client_cert: Optional[tuple[str, str]] = None,
    *,
    method: str = "GET",
    body: Optional[dict] = None,
    timeout: float = 15,
) -> tuple[Optional[int], Optional[dict]]:
    """One HTTPS request to `host:port`, trusting only the certificate whose
    fingerprint is `pin` (`sha256:<hex>`, as `tls_cert_fingerprint` in a
    bootstrap bundle and in a client entry). Hostname checks do not apply:
    the pin replaces them, as it does in every shed client.

    Returns `(status, JSON body or None)`, or `(None, None)` when the server
    gives no HTTP response at all — which is what an mtls listener does to a
    request that presents no certificate, or one its CA did not issue. Raises
    `PinMismatch` when the server's certificate is not the pinned one, so a
    wrong server can never pass as a refusal.
    """
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE  # verified below, by fingerprint
    if client_cert:
        ctx.load_cert_chain(*client_cert)
    conn = http.client.HTTPSConnection(host, port, context=ctx, timeout=timeout)
    try:
        try:
            conn.connect()
        except (ssl.SSLError, ConnectionError):
            return None, None
        der = conn.sock.getpeercert(binary_form=True)
        got = "sha256:" + hashlib.sha256(der).hexdigest()
        if got != pin:
            raise PinMismatch(f"{host}:{port} presented {got}, pinned {pin}")
        data = json.dumps(body).encode() if body is not None else None
        headers = {"Content-Type": "application/json"} if data is not None else {}
        try:
            conn.request(method, path, body=data, headers=headers)
            resp = conn.getresponse()
            raw = resp.read()
        except (ssl.SSLError, ConnectionError, http.client.HTTPException):
            # TLS 1.3: the client's side of the handshake completes before the
            # server has judged the client's certificate, so the refusal (an
            # alert) arrives on the first read or write instead.
            return None, None
        try:
            decoded = json.loads(raw) if raw.strip() else None
        except json.JSONDecodeError:
            decoded = None
        return resp.status, decoded
    finally:
        conn.close()
