"""HTTP bearer-token enforcement under v0.7.1 secure mode (live).

Puts the VZ dev server in `auth.mode: secure` with the local SSH key allowlisted,
mints scoped tokens over the `_bootstrap` SSH channel (no static token list — that
was removed), and verifies live: bootstrap endpoints stay open, the control plane
and bus require a token, and the credentials vs control scopes are enforced per
route. Secure mode is TLS-only (no plain-HTTP listener), so the requests go over
the pinned HTTPS listener. VZ-only (config mutation restarts the local dev
server); the middleware logic itself is backend-agnostic Go covered by unit
tests.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from fixtures.devcontrol import bootstrap_mint, dev_config
from fixtures.tlsclient import https_status as _status
from fixtures.tlsclient import server_cert_pem

# Secure mode is TLS-only; pin the explicitly-set https_port (the port-safety
# guard allows 18443) so requests reach the only listener facing clients.
HTTPS_PORT = 18443


@pytest.mark.vz
@pytest.mark.slow
def test_secure_mode_enforces_scoped_tokens(vz_server_dev):
    server = vz_server_dev.name
    # Allowlist the local SSH key so the bootstrap channel mints for it.
    pubkey = (Path.home() / ".ssh" / "id_ed25519.pub").read_text().strip()
    overrides = {
        "https_port": HTTPS_PORT,
        "auth": {"mode": "secure", "ssh": {"authorized_keys": [pubkey]}},
    }
    with dev_config(overrides, server):
        # Fetch the server's self-signed cert and pin it for every request
        # (the TLS-only listener presents no CA-chain trust).
        pin = server_cert_pem("localhost", HTTPS_PORT)

        # Bootstrap endpoints stay reachable without a token (shed server add).
        assert _status(HTTPS_PORT, "/api/info", pin) == 200
        assert _status(HTTPS_PORT, "/api/ssh-host-key", pin) == 200

        # Mint scoped tokens over the _bootstrap SSH channel — no static list.
        control = bootstrap_mint(server, "control")
        creds = bootstrap_mint(server, "credentials")
        assert control.startswith("shed_control_")
        assert creds.startswith("shed_credentials_")

        # Control plane requires a control token.
        assert _status(HTTPS_PORT, "/api/sheds", pin) == 401
        assert _status(HTTPS_PORT, "/api/sheds", pin, token="shed_control_bogus") == 401
        assert _status(HTTPS_PORT, "/api/sheds", pin, token=control) == 200
        assert _status(HTTPS_PORT, "/api/sheds", pin, token=creds) == 403  # wrong scope

        # The credential bus requires the credentials scope.
        assert _status(HTTPS_PORT, "/api/plugins/listeners", pin) == 401
        assert _status(HTTPS_PORT, "/api/plugins/listeners", pin, token=creds) == 200
        assert _status(HTTPS_PORT, "/api/plugins/listeners", pin, token=control) == 403
