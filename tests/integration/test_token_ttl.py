"""Server-side token TTL expiry (live).

Mints a control token with a 2s `auth.token_ttl` over the `_bootstrap` channel
and verifies the server accepts it immediately, then rejects it (401) once it
expires — the authtoken store validates each token's expiry on every request, so
expiry is enforced at validate time (not only by the background sweeper). Secure
mode is TLS-only (no plain-HTTP listener), so the requests go over the pinned
HTTPS listener. VZ-only (config mutation restarts the dev server); the TTL logic
is backend-agnostic Go covered by internal/authtoken unit tests.
"""

from __future__ import annotations

import time
from pathlib import Path

import pytest

from fixtures.devcontrol import (
    bootstrap_mint,
    dev_config,
    skip_mtls_token_semantics,
    skip_needs_open_mode_dev_server,
)
from fixtures.tlsclient import https_status as _status
from fixtures.tlsclient import server_cert_pem

# Secure mode is TLS-only; pin the explicitly-set https_port (the port-safety
# guard allows 18443) so requests reach the only listener facing clients.
HTTPS_PORT = 18443


@skip_needs_open_mode_dev_server
@skip_mtls_token_semantics
@pytest.mark.vz
@pytest.mark.slow
def test_token_ttl_expiry(vz_server_dev):
    server = vz_server_dev.name
    pubkey = (Path.home() / ".ssh" / "id_ed25519.pub").read_text().strip()
    overrides = {
        "https_port": HTTPS_PORT,
        "auth": {"mode": "secure", "token_ttl": "2s", "ssh": {"authorized_keys": [pubkey]}},
    }
    with dev_config(overrides, server):
        pin = server_cert_pem("localhost", HTTPS_PORT)
        tok = bootstrap_mint(server, "control")
        # Freshly minted → accepted.
        assert _status(HTTPS_PORT, "/api/sheds", pin, tok) == 200
        # After the 2s TTL elapses, the same token is rejected (per-request expiry).
        time.sleep(3)
        assert _status(HTTPS_PORT, "/api/sheds", pin, tok) == 401
