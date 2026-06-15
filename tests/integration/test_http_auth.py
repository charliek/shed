"""HTTP bearer-token enforcement under v0.7.1 secure mode (live).

Puts the VZ dev server in `auth.mode: secure` with the local SSH key allowlisted,
mints scoped tokens over the `_bootstrap` SSH channel (no static token list — that
was removed), and verifies live: bootstrap endpoints stay open, the control plane
and bus require a token, and the credentials vs control scopes are enforced per
route. VZ-only (config mutation restarts the local dev server); the middleware
logic itself is backend-agnostic Go covered by unit tests.
"""

from __future__ import annotations

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
def test_secure_mode_enforces_scoped_tokens(vz_server_dev):
    server = vz_server_dev.name
    port = int(resolve_server_entry(server)["http_port"])
    # Allowlist the local SSH key so the bootstrap channel mints for it.
    pubkey = (Path.home() / ".ssh" / "id_ed25519.pub").read_text().strip()
    overrides = {"auth": {"mode": "secure", "ssh": {"authorized_keys": [pubkey]}}}
    with dev_config(overrides, server):
        # Bootstrap endpoints stay reachable without a token (shed server add).
        assert _status(port, "/api/info") == 200
        assert _status(port, "/api/ssh-host-key") == 200

        # Mint scoped tokens over the _bootstrap SSH channel — no static list.
        control = bootstrap_mint(server, "control")
        creds = bootstrap_mint(server, "credentials")
        assert control.startswith("shed_control_")
        assert creds.startswith("shed_credentials_")

        # Control plane requires a control token.
        assert _status(port, "/api/sheds") == 401
        assert _status(port, "/api/sheds", token="shed_control_bogus") == 401
        assert _status(port, "/api/sheds", token=control) == 200
        assert _status(port, "/api/sheds", token=creds) == 403  # wrong scope

        # The credential bus requires the credentials scope.
        assert _status(port, "/api/plugins/listeners") == 401
        assert _status(port, "/api/plugins/listeners", token=creds) == 200
        assert _status(port, "/api/plugins/listeners", token=control) == 403
