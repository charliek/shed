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

Secure mode is TLS-only — the server serves no plain-HTTP listener, only the
pinned-TLS (https_port) listener faces clients — so `_tls_overrides()` carries a
secure auth block (https_port is a secure-mode-only surface).
VZ-only (config mutation restarts the local dev server); the cert-generation +
pin-verify logic is backend-agnostic Go covered by `internal/servertls` +
`cmd/shed` unit tests. The CLI test runs against a throwaway $HOME so it never
touches the developer's real ~/.shed/config.yaml.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
import urllib.error
from pathlib import Path

import pytest

from fixtures.devcontrol import SHED_SERVER_BIN, bootstrap_mint, dev_config, skip_mtls_reconfigure
from fixtures.server import resolve_server_entry
from fixtures.tlsclient import fingerprint as _fingerprint
from fixtures.tlsclient import https_status as _https_status
from fixtures.tlsclient import server_cert_pem as _server_cert_pem

HTTPS_PORT = 18443
TEST_SAN = "shed-tls-test.example"
# Persist the cert outside ~/.shed so it never collides with a real server's
# pinned material; a fresh path per run regenerates with the configured SANs.
DEV_TLS_DIR = Path.home() / ".shed" / "dev" / "tls-itest"
SHED_BIN = SHED_SERVER_BIN.parent / "shed"

# https_port is a secure-mode-only surface (an open-mode server serves plain
# http only and is rejected at startup with https_port set), so the TLS
# overrides carry a secure auth block with the local key allowlisted — the same
# allowlisting the other secure-mode tests use.
_LOCAL_PUBKEY = (Path.home() / ".ssh" / "id_ed25519.pub").read_text().strip()


def _tls_overrides() -> dict:
    return {
        "https_port": HTTPS_PORT,
        "tls_names": [TEST_SAN],
        "tls_cert_file": str(DEV_TLS_DIR / "tls_cert.pem"),
        "tls_key_file": str(DEV_TLS_DIR / "tls_key.pem"),
        "auth": {"mode": "secure", "ssh": {"authorized_keys": [_LOCAL_PUBKEY]}},
    }


def _shed(args: list[str], home: str, timeout: float = 30) -> subprocess.CompletedProcess:
    # Throwaway $HOME so `shed server add` writes a scratch config + known_hosts,
    # never the developer's real ~/.shed.
    env = dict(os.environ, HOME=home)
    return subprocess.run(
        [str(SHED_BIN), *args], capture_output=True, text=True, timeout=timeout, env=env
    )


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


@skip_mtls_reconfigure
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

            # Secure mode is TLS-only: the plain-HTTP listener is NOT served, so
            # nothing answers on http_port. curl gets a connection failure
            # (non-zero exit, no HTTP status), confirming the plaintext surface
            # is gone — only the pinned-TLS listener faces clients.
            r = subprocess.run(
                ["curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}",
                 f"http://localhost:{http_port}/api/info"],
                capture_output=True, text=True, timeout=15,
            )
            assert r.returncode != 0, (
                f"plain HTTP must not be served in secure mode, got exit 0: "
                f"{r.stdout!r} {r.stderr!r}"
            )
            assert r.stdout.strip() != "200", (
                f"plain HTTP must not answer in secure mode: {r.stdout!r} {r.stderr!r}"
            )
    finally:
        shutil.rmtree(DEV_TLS_DIR, ignore_errors=True)


@skip_mtls_reconfigure
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


@skip_mtls_reconfigure
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

                # The client still pins v1 → control plane is rejected, and
                # specifically for a pin mismatch (not some unrelated error).
                stale = _shed(["-s", "rot", "list"], home)
                assert stale.returncode != 0, "stale v1 pin must reject the rotated cert"
                assert "fingerprint mismatch" in (stale.stdout + stale.stderr).lower(), \
                    f"expected a pin-mismatch error, got: {stale.stdout!r} {stale.stderr!r}"

                # Re-pin v2 (a rotation of an existing pin needs an explicit trust).
                r = _shed(["server", "update", "rot", "--refetch", "--trust-on-first-use"], home)
                assert r.returncode == 0, f"refetch re-pin failed: {r.stdout!r} {r.stderr!r}"

                # Control plane works again on the new pin.
                assert _shed(["-s", "rot", "list"], home).returncode == 0, \
                    "control plane must work after re-pinning v2"
    finally:
        shutil.rmtree(DEV_TLS_DIR, ignore_errors=True)


@skip_mtls_reconfigure
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
    # _tls_overrides() already carries auth.mode: secure with the local key
    # allowlisted (https_port is a secure-mode-only surface), so the pinned-TLS
    # shape here is the same as the other secure-mode tests. secure enforces HTTP
    # tokens and keeps the explicitly-set https_port (18443, not the 8443
    # default); tokens are minted over SSH (the static auth.http.tokens list was
    # removed).
    try:
        with dev_config(_tls_overrides(), server):
            creds = bootstrap_mint(server, "credentials")
            control = bootstrap_mint(server, "control")
            pin = _server_cert_pem("localhost", HTTPS_PORT)

            # Pinned cert + credentials token → the bus is reachable.
            assert _https_status(HTTPS_PORT, "/api/plugins/listeners", pin, creds) == 200
            # Pinned cert but wrong scope / no token → rejected by auth.
            assert _https_status(HTTPS_PORT, "/api/plugins/listeners", pin, control) == 403
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
