"""Phase 5: native pinned TLS — server side + CLI client pin.

Configures the VZ dev server with `https_port` + `tls_names` (via the Phase
0.5 harness) and verifies live:

  - the server presents a self-signed cert whose SANs cover the configured
    names plus the loopback defaults, the fingerprint a client computes is
    stable (the pin), and `curl --cacert` trusts it for a SAN
    (test_tls_listener_serves_pinnable_cert);
  - the real `shed` CLI pins the cert at `shed server add --https-port`, drives
    the control plane over the pinned TLS connection, and rejects a wrong
    out-of-band `--tls-fingerprint` (test_tls_client_pin).

The plain-HTTP listener must keep answering so the legacy path is unaffected.
VZ-only (config mutation restarts the local dev server); the cert-generation +
pin-verify logic is backend-agnostic Go covered by `internal/servertls` +
`cmd/shed` unit tests. The CLI test runs against a throwaway $HOME so it never
touches the developer's real ~/.shed/config.yaml.
"""

from __future__ import annotations

import hashlib
import os
import shutil
import ssl
import subprocess
import tempfile
import urllib.error
import urllib.request
from pathlib import Path

import pytest

from fixtures.devcontrol import SHED_SERVER_BIN, dev_config
from fixtures.server import resolve_server_entry

HTTPS_PORT = 18443
TEST_SAN = "shed-tls-test.example"
# Hermetic scoped tokens for the bus-over-TLS lockstep (the host-agent posture).
CREDS_TOKEN = "shed_credentials_itesttls"
CONTROL_TOKEN = "shed_control_itesttls0"
# Persist the cert outside ~/.shed so it never collides with a real server's
# pinned material; a fresh path per run regenerates with the configured SANs.
DEV_TLS_DIR = Path.home() / ".shed" / "dev" / "tls-itest"
SHED_BIN = SHED_SERVER_BIN.parent / "shed"


def _tls_overrides() -> dict:
    return {
        "https_port": HTTPS_PORT,
        "tls_names": [TEST_SAN],
        "tls_cert_file": str(DEV_TLS_DIR / "tls_cert.pem"),
        "tls_key_file": str(DEV_TLS_DIR / "tls_key.pem"),
    }


def _shed(args: list[str], home: str, timeout: float = 30) -> subprocess.CompletedProcess:
    # Throwaway $HOME so `shed server add` writes a scratch config + known_hosts,
    # never the developer's real ~/.shed.
    env = dict(os.environ, HOME=home)
    return subprocess.run(
        [str(SHED_BIN), *args], capture_output=True, text=True, timeout=timeout, env=env
    )


def _server_cert_pem(host: str, port: int) -> str:
    # get_server_certificate retrieves the presented cert without trusting it,
    # which is exactly what `shed server add` does before pinning.
    return ssl.get_server_certificate((host, port))


def _fingerprint(pem: str) -> str:
    der = ssl.PEM_cert_to_DER_cert(pem)
    return "sha256:" + hashlib.sha256(der).hexdigest()


def _sans(pem: str) -> str:
    with tempfile.NamedTemporaryFile("w", suffix=".pem", delete=False) as f:
        f.write(pem)
        path = f.name
    try:
        out = subprocess.run(
            ["openssl", "x509", "-in", path, "-noout", "-ext", "subjectAltName"],
            capture_output=True, text=True, timeout=10,
        )
        return out.stdout
    finally:
        Path(path).unlink(missing_ok=True)


@pytest.mark.vz
@pytest.mark.slow
def test_tls_listener_serves_pinnable_cert(vz_server_dev):
    server = vz_server_dev.name
    entry = resolve_server_entry(server)
    http_port = int(entry["http_port"])

    shutil.rmtree(DEV_TLS_DIR, ignore_errors=True)
    DEV_TLS_DIR.mkdir(parents=True, exist_ok=True)
    try:
        with dev_config(_tls_overrides(), server):
            # The HTTPS listener presents a cert; fetch it the way `server add`
            # would (no trust yet), then confirm its SANs cover the configured
            # name and the always-on loopback defaults.
            pem = _server_cert_pem("localhost", HTTPS_PORT)
            sans = _sans(pem)
            assert TEST_SAN in sans, f"configured tls_name missing from SANs: {sans!r}"
            assert "DNS:localhost" in sans, f"loopback DNS SAN missing: {sans!r}"
            assert "IP Address:127.0.0.1" in sans, f"loopback IP SAN missing: {sans!r}"

            # The pin a client computes is stable across fetches (same cert).
            fp = _fingerprint(pem)
            assert fp.startswith("sha256:") and len(fp) == len("sha256:") + 64
            assert _fingerprint(_server_cert_pem("localhost", HTTPS_PORT)) == fp

            # curl trusts the cert when handed it as the CA (the documented
            # --cacert path) and hostname verification passes for a SAN.
            with tempfile.NamedTemporaryFile("w", suffix=".pem", delete=False) as f:
                f.write(pem)
                ca_path = f.name
            try:
                r = subprocess.run(
                    ["curl", "-sS", "--cacert", ca_path, "-o", "/dev/null",
                     "-w", "%{http_code}", f"https://localhost:{HTTPS_PORT}/api/info"],
                    capture_output=True, text=True, timeout=15,
                )
                assert r.stdout.strip() == "200", f"curl --cacert failed: {r.stdout!r} {r.stderr!r}"
            finally:
                Path(ca_path).unlink(missing_ok=True)

            # The plain-HTTP listener is unaffected (legacy path).
            r = subprocess.run(
                ["curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}",
                 f"http://localhost:{http_port}/api/info"],
                capture_output=True, text=True, timeout=15,
            )
            assert r.stdout.strip() == "200", f"plain HTTP regressed: {r.stdout!r} {r.stderr!r}"
    finally:
        shutil.rmtree(DEV_TLS_DIR, ignore_errors=True)


@pytest.mark.vz
@pytest.mark.slow
def test_tls_client_pin(vz_server_dev):
    server = vz_server_dev.name
    shutil.rmtree(DEV_TLS_DIR, ignore_errors=True)
    DEV_TLS_DIR.mkdir(parents=True, exist_ok=True)
    try:
        with dev_config(_tls_overrides(), server), tempfile.TemporaryDirectory() as home:
            # `shed server add --https-port` fetches the presented cert, pins it
            # (TOFU here), and drives /api/info + /api/ssh-host-key over the
            # pinned TLS connection — all into a throwaway $HOME.
            r = _shed(
                ["server", "add", "localhost", "--https-port", str(HTTPS_PORT),
                 "--trust-on-first-use", "--name", "tlspin"],
                home,
            )
            assert r.returncode == 0, f"server add over TLS failed: {r.stdout!r} {r.stderr!r}"

            # The pinned entry drives the control plane over TLS.
            r = _shed(["-s", "tlspin", "list"], home)
            assert r.returncode == 0, f"control plane over pinned TLS failed: {r.stdout!r} {r.stderr!r}"

            # A wrong out-of-band fingerprint is rejected before anything is pinned.
            r = _shed(
                ["server", "add", "localhost", "--https-port", str(HTTPS_PORT),
                 "--tls-fingerprint", "sha256:" + "00" * 32, "--name", "tlsbad"],
                home,
            )
            assert r.returncode != 0, "a wrong --tls-fingerprint must be rejected"
            assert "mismatch" in (r.stdout + r.stderr).lower(), f"expected a mismatch error: {r.stdout!r} {r.stderr!r}"
    finally:
        shutil.rmtree(DEV_TLS_DIR, ignore_errors=True)


@pytest.mark.vz
@pytest.mark.slow
def test_tls_pin_rotation(vz_server_dev):
    """Rotating the server cert breaks the old pin until the client re-pins.
    `server add` pins v1; the cert is regenerated (v2); the v1-pinned client is
    rejected; `server update --refetch` re-pins v2 and the control plane works."""
    server = vz_server_dev.name
    shutil.rmtree(DEV_TLS_DIR, ignore_errors=True)
    DEV_TLS_DIR.mkdir(parents=True, exist_ok=True)
    try:
        with tempfile.TemporaryDirectory() as home:
            # Round 1: pin v1.
            with dev_config(_tls_overrides(), server):
                v1 = _fingerprint(_server_cert_pem("localhost", HTTPS_PORT))
                r = _shed(["server", "add", "localhost", "--https-port", str(HTTPS_PORT),
                           "--trust-on-first-use", "--name", "rot"], home)
                assert r.returncode == 0, f"add v1 failed: {r.stdout!r} {r.stderr!r}"
                assert _shed(["-s", "rot", "list"], home).returncode == 0

            # Rotate: drop the cert so the server regenerates a fresh one (v2).
            shutil.rmtree(DEV_TLS_DIR, ignore_errors=True)
            DEV_TLS_DIR.mkdir(parents=True, exist_ok=True)

            # Round 2: server now presents v2.
            with dev_config(_tls_overrides(), server):
                v2 = _fingerprint(_server_cert_pem("localhost", HTTPS_PORT))
                assert v2 != v1, "expected a fresh cert after rotation"

                # The client still pins v1 → control plane is rejected.
                assert _shed(["-s", "rot", "list"], home).returncode != 0, \
                    "stale v1 pin must reject the rotated cert"

                # Re-pin v2 (a rotation of an existing pin needs an explicit trust).
                r = _shed(["server", "update", "rot", "--refetch", "--trust-on-first-use"], home)
                assert r.returncode == 0, f"refetch re-pin failed: {r.stdout!r} {r.stderr!r}"

                # Control plane works again on the new pin.
                assert _shed(["-s", "rot", "list"], home).returncode == 0, \
                    "control plane must work after re-pinning v2"
    finally:
        shutil.rmtree(DEV_TLS_DIR, ignore_errors=True)


def _https_status(port: int, path: str, ca_pem: str | None, token: str | None) -> int | None:
    # ca_pem trusts exactly the server's cert (the pin); None uses the system
    # CA bundle (which must NOT trust the self-signed cert).
    if ca_pem:
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        ctx.load_verify_locations(cadata=ca_pem)
    else:
        ctx = ssl.create_default_context()
    req = urllib.request.Request(f"https://localhost:{port}{path}")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(req, timeout=10, context=ctx) as resp:
            return resp.status
    except urllib.error.HTTPError as e:
        e.close()  # warnings-as-errors: don't leak the response body
        return e.code


@pytest.mark.vz
@pytest.mark.slow
def test_tls_bus_over_tls_with_token(vz_server_dev):
    """The host-agent posture: credential bus reached over pinned TLS + a
    credentials-scoped token. Proves the server accepts exactly what the pinned,
    authed host-agent (sdk WithTLSPin + WithToken) presents, and rejects the
    negatives — wrong scope, no token, and an untrusted (mis-pinned) cert."""
    server = vz_server_dev.name
    shutil.rmtree(DEV_TLS_DIR, ignore_errors=True)
    DEV_TLS_DIR.mkdir(parents=True, exist_ok=True)
    overrides = {
        **_tls_overrides(),
        "auth": {
            "http": {
                "mode": "enforce",
                "tokens": [
                    {"scope": "credentials", "token": CREDS_TOKEN},
                    {"scope": "control", "token": CONTROL_TOKEN},
                ],
            }
        },
    }
    try:
        with dev_config(overrides, server):
            pin = _server_cert_pem("localhost", HTTPS_PORT)

            # Pinned cert + credentials token → the bus is reachable.
            assert _https_status(HTTPS_PORT, "/api/plugins/listeners", pin, CREDS_TOKEN) == 200
            # Pinned cert but wrong scope / no token → rejected by auth.
            assert _https_status(HTTPS_PORT, "/api/plugins/listeners", pin, CONTROL_TOKEN) == 403
            assert _https_status(HTTPS_PORT, "/api/plugins/listeners", pin, None) == 401
            # Bootstrap stays open over TLS (no token needed).
            assert _https_status(HTTPS_PORT, "/api/info", pin, None) == 200

            # Without the pin (system CAs), the self-signed cert is untrusted:
            # a client that doesn't pin it cannot connect at all. urllib wraps
            # the TLS verification failure in a URLError.
            with pytest.raises(urllib.error.URLError) as exc:
                _https_status(HTTPS_PORT, "/api/info", None, None)
            assert "CERTIFICATE_VERIFY_FAILED" in str(exc.value), f"expected a cert-verify failure: {exc.value!r}"
    finally:
        shutil.rmtree(DEV_TLS_DIR, ignore_errors=True)
