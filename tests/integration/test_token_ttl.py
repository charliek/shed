"""Server-side token TTL expiry (live).

Mints a control token with a 2s `auth.token_ttl` over the `_bootstrap` channel
and verifies the server accepts it immediately, then rejects it (401) once it
expires — the authtoken store validates each token's expiry on every request, so
expiry is enforced at validate time (not only by the background sweeper). VZ-only
(config mutation restarts the dev server); the TTL logic is backend-agnostic Go
covered by internal/authtoken unit tests.
"""

from __future__ import annotations

import time
import urllib.error
import urllib.request
from pathlib import Path

import pytest

from fixtures.devcontrol import bootstrap_mint, dev_config
from fixtures.server import resolve_server_entry


def _status(port: int, path: str, token: str | None = None) -> int | None:
    req = urllib.request.Request(f"http://localhost:{port}{path}")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status
    except urllib.error.HTTPError as e:
        e.close()  # warnings-as-errors: don't leak the response body
        return e.code
    except urllib.error.URLError:
        return None


@pytest.mark.vz
@pytest.mark.slow
def test_token_ttl_expiry(vz_server_dev):
    server = vz_server_dev.name
    port = int(resolve_server_entry(server)["http_port"])
    pubkey = (Path.home() / ".ssh" / "id_ed25519.pub").read_text().strip()
    overrides = {
        "auth": {"mode": "secure", "token_ttl": "2s", "ssh": {"authorized_keys": [pubkey]}}
    }
    with dev_config(overrides, server):
        tok = bootstrap_mint(server, "control")
        # Freshly minted → accepted.
        assert _status(port, "/api/sheds", tok) == 200
        # After the 2s TTL elapses, the same token is rejected (per-request expiry).
        time.sleep(3)
        assert _status(port, "/api/sheds", tok) == 401
