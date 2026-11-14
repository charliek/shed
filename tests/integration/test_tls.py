"""Native pinned TLS — server side + CLI client pin, under mtls.

Configures the VZ dev server with `tls_names` and its own cert/key files (via
`dev_config`; the committed base is `auth.mode: mtls`, the only mode with a TLS
listener) and verifies live:

  - the server presents a self-signed cert whose SANs cover the configured
    names plus the loopback defaults, the fingerprint a client computes is
    stable (the pin), `curl --cacert` trusts it for a SAN when it also
    presents a client certificate the server issued, and plain HTTP is not
    served (test_tls_listener_serves_pinnable_cert);
  - the real `shed` CLI pins the cert at `shed server add` from the bootstrap
    bundle, drives the control plane over the pinned connection, and rejects
    a wrong out-of-band `--tls-fingerprint` (test_tls_client_pin);
  - rotating the cert breaks the old pin until `shed server update --refetch`
    re-pins it (test_tls_pin_rotation).

The bearer-token-over-TLS test that lived here went with the token mode; the
same checks with certificates are test_http_auth.py's. VZ-only (config
mutation restarts the local dev server); the cert-generation + pin-verify
logic is backend-agnostic Go covered by `internal/servertls` + `cmd/shed` unit
tests. The CLI tests run against a throwaway $HOME so they never touch the
developer's real ~/.shed/config.yaml.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
from pathlib import Path

import pytest
import yaml

from fixtures.devcontrol import (
    DEV_BASE_CONFIG,
    SHED_SERVER_BIN,
    bootstrap_cert,
    dev_config,
    isolated_home_with_identity,
)
from fixtures.server import resolve_server_entry
from fixtures.tlsclient import fingerprint as _fingerprint
from fixtures.tlsclient import server_cert_pem as _server_cert_pem

TEST_SAN = "shed-tls-test.example"
# Persist the cert outside ~/.shed so it never collides with a real server's
# pinned material; a fresh path per run regenerates with the configured SANs.
DEV_TLS_DIR = Path.home() / ".shed" / "dev" / "tls-itest"
SHED_BIN = SHED_SERVER_BIN.parent / "shed"


def _base_ports() -> tuple[int, int]:
    """The committed dev-parallel base config's `http_port` and `https_port`.

    `http_port` is the port an open-mode dev server would bind (18080, offset
    from prod's 8080); under mtls nothing listens there, which the first test
    asserts. `https_port` is the mtls listener."""
    with DEV_BASE_CONFIG.open("r", encoding="utf-8") as f:
        cfg = yaml.safe_load(f) or {}
    return int(cfg.get("http_port") or 8080), int(cfg.get("https_port") or 8443)


def _tls_overrides() -> dict:
    return {
        "tls_names": [TEST_SAN],
        "tls_cert_file": str(DEV_TLS_DIR / "tls_cert.pem"),
        "tls_key_file": str(DEV_TLS_DIR / "tls_key.pem"),
    }


def _shed(args: list[str], home: str, timeout: float = 60) -> subprocess.CompletedProcess:
    # Throwaway $HOME so `shed server add` writes a scratch config + known_hosts,
    # never the developer's real ~/.shed.
    env = dict(os.environ, HOME=home)
    return subprocess.run(
        [str(SHED_BIN), *args], capture_output=True, text=True, timeout=timeout, env=env
    )


def _ssh_port(server: str) -> int:
    entry = resolve_server_entry(server)
    assert entry is not None, f"{server!r} not found in ~/.shed/config.yaml"
    return int(entry["ssh_port"])


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


@pytest.fixture
def tls_dir():
    shutil.rmtree(DEV_TLS_DIR, ignore_errors=True)
    DEV_TLS_DIR.mkdir(parents=True, exist_ok=True)
    yield DEV_TLS_DIR
    shutil.rmtree(DEV_TLS_DIR, ignore_errors=True)


@pytest.mark.vz
@pytest.mark.slow
def test_tls_listener_serves_pinnable_cert(vz_server_dev, tls_dir, tmp_path):
    server = vz_server_dev.name
    http_port, https_port = _base_ports()

    with dev_config(_tls_overrides(), server):
        # The HTTPS listener presents a cert; fetch it the way a client does
        # before pinning (no trust yet), then confirm its SANs cover the
        # configured name and the always-on loopback defaults.
        pem = _server_cert_pem("localhost", https_port)
        sans = _sans(pem)
        assert TEST_SAN in sans, f"configured tls_name missing from SANs: {sans!r}"
        assert "DNS:localhost" in sans, f"loopback DNS SAN missing: {sans!r}"
        assert "IP Address:127.0.0.1" in sans, f"loopback IP SAN missing: {sans!r}"

        # The pin a client computes is stable across fetches (same cert), and
        # it is the pin the bootstrap bundle hands an enrolling client.
        fp = _fingerprint(pem)
        assert fp.startswith("sha256:") and len(fp) == len("sha256:") + 64
        assert _fingerprint(_server_cert_pem("localhost", https_port)) == fp
        cert = bootstrap_cert(server, "control", tmp_path)
        assert cert.pin == fp, f"bootstrap bundle pins {cert.pin}, the listener presents {fp}"

        # curl trusts the cert when handed it as the CA (the documented
        # --cacert path) and hostname verification passes for a SAN; it
        # presents the issued client certificate, which mtls requires.
        ca_path = tmp_path / "server.pem"
        ca_path.write_text(pem)
        r = subprocess.run(
            ["curl", "-sS", "--cacert", str(ca_path),
             "--cert", str(cert.cert_path), "--key", str(cert.key_path),
             "-o", "/dev/null", "-w", "%{http_code}", f"https://localhost:{https_port}/api/info"],
            capture_output=True, text=True, timeout=15,
        )
        assert r.stdout.strip() == "200", f"curl --cacert --cert failed: {r.stdout!r} {r.stderr!r}"

        # Without a client certificate the same request gets no HTTP status.
        r = subprocess.run(
            ["curl", "-sS", "--cacert", str(ca_path), "-o", "/dev/null", "-w", "%{http_code}",
             f"https://localhost:{https_port}/api/info"],
            capture_output=True, text=True, timeout=15,
        )
        assert r.returncode != 0 and r.stdout.strip() in ("", "000"), (
            f"an HTTP response without a client certificate: {r.stdout!r} {r.stderr!r}"
        )

        # mtls is TLS-only: the plain-HTTP listener is NOT served, so nothing
        # answers on http_port.
        r = subprocess.run(
            ["curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}",
             f"http://localhost:{http_port}/api/info"],
            capture_output=True, text=True, timeout=15,
        )
        assert r.returncode != 0, (
            f"plain HTTP must not be served in mtls mode, got exit 0: {r.stdout!r} {r.stderr!r}"
        )
        assert r.stdout.strip() != "200", f"plain HTTP must not answer in mtls mode: {r.stdout!r}"


@pytest.mark.vz
@pytest.mark.slow
def test_tls_client_pin(vz_server_dev, tls_dir):
    server = vz_server_dev.name
    ssh_port = _ssh_port(server)
    _, https_port = _base_ports()
    with dev_config(_tls_overrides(), server), isolated_home_with_identity() as home:
        # SSH-first `shed server add`: the host key is pinned (TOFU here), the
        # bootstrap bundle carries the TLS pin and a client certificate, and
        # the entry is verified over the pinned connection — all into a
        # throwaway $HOME.
        r = _shed(
            ["server", "add", "localhost", "--ssh-port", str(ssh_port),
             "--trust-on-first-use", "--name", "tlspin"],
            home,
        )
        assert r.returncode == 0, f"server add failed: {r.stdout!r} {r.stderr!r}"
        cfg = yaml.safe_load((Path(home) / ".shed" / "config.yaml").read_text()) or {}
        entry = (cfg.get("servers") or {}).get("tlspin") or {}
        want = _fingerprint(_server_cert_pem("localhost", https_port))
        assert entry.get("tls_cert_fingerprint") == want, f"entry pins {entry.get('tls_cert_fingerprint')!r}, want {want}"

        # The pinned entry drives the control plane over TLS.
        r = _shed(["-s", "tlspin", "list"], home)
        assert r.returncode == 0, f"control plane over pinned TLS failed: {r.stdout!r} {r.stderr!r}"

        # A wrong out-of-band fingerprint is rejected before anything is pinned.
        r = _shed(
            ["server", "add", "localhost", "--ssh-port", str(ssh_port),
             "--tls-fingerprint", "sha256:" + "00" * 32, "--name", "tlsbad"],
            home,
        )
        assert r.returncode != 0, "a wrong --tls-fingerprint must be rejected"
        assert "mismatch" in (r.stdout + r.stderr).lower(), f"expected a mismatch error: {r.stdout!r} {r.stderr!r}"
        cfg = yaml.safe_load((Path(home) / ".shed" / "config.yaml").read_text()) or {}
        assert "tlsbad" not in (cfg.get("servers") or {}), "a refused add wrote an entry"


@pytest.mark.vz
@pytest.mark.slow
def test_tls_pin_rotation(vz_server_dev, tls_dir):
    """Rotating the server cert breaks the old pin until the client re-pins.
    `server add` pins v1; the cert is regenerated (v2); the v1-pinned client is
    rejected; `server update --refetch` re-pins v2 (from a fresh bootstrap
    bundle) and the control plane works."""
    server = vz_server_dev.name
    ssh_port = _ssh_port(server)
    _, https_port = _base_ports()
    with isolated_home_with_identity() as home:
        # Round 1: pin v1.
        with dev_config(_tls_overrides(), server):
            v1 = _fingerprint(_server_cert_pem("localhost", https_port))
            r = _shed(["server", "add", "localhost", "--ssh-port", str(ssh_port),
                       "--trust-on-first-use", "--name", "rot"], home)
            assert r.returncode == 0, f"add v1 failed: {r.stdout!r} {r.stderr!r}"
            assert _shed(["-s", "rot", "list"], home).returncode == 0

        # Rotate: drop the cert so the server regenerates a fresh one (v2).
        shutil.rmtree(DEV_TLS_DIR, ignore_errors=True)
        DEV_TLS_DIR.mkdir(parents=True, exist_ok=True)

        # Round 2: server now presents v2.
        with dev_config(_tls_overrides(), server):
            v2 = _fingerprint(_server_cert_pem("localhost", https_port))
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
