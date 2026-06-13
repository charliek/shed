"""Phase 5: native pinned TLS — server side (HTTPS listener + cert).

Configures the VZ dev server with `https_port` + `tls_names` (via the Phase
0.5 harness) and verifies live that the server presents a self-signed cert
whose SANs cover the configured names plus the loopback defaults, that the
fingerprint a client computes from the presented cert is stable (the pin), and
that `curl --cacert` trusts it for a `tls_names`/loopback SAN. The plain-HTTP
listener must keep answering so the legacy path is unaffected.

VZ-only (config mutation restarts the local dev server); the cert-generation
logic itself is backend-agnostic Go covered by `internal/servertls` unit tests.
Client-side pin enforcement (Go/Swift/CLI) lands in later Phase 5 sub-steps and
gets its own coverage.
"""

from __future__ import annotations

import hashlib
import shutil
import ssl
import subprocess
import tempfile
from pathlib import Path

import pytest

from fixtures.devcontrol import dev_config
from fixtures.server import resolve_server_entry

HTTPS_PORT = 18443
TEST_SAN = "shed-tls-test.example"
# Persist the cert outside ~/.shed so it never collides with a real server's
# pinned material; a fresh path per run regenerates with the configured SANs.
DEV_TLS_DIR = Path.home() / ".shed" / "dev" / "tls-itest"


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
    overrides = {
        "https_port": HTTPS_PORT,
        "tls_names": [TEST_SAN],
        "tls_cert_file": str(DEV_TLS_DIR / "tls_cert.pem"),
        "tls_key_file": str(DEV_TLS_DIR / "tls_key.pem"),
    }
    try:
        with dev_config(overrides, server):
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
