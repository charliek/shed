"""Phase 4b: HTTP bearer-token enforcement (deny-by-default middleware).

Configures the VZ dev server with auth.http.mode=enforce + scoped tokens (via
the Phase 0.5 harness) and verifies live: bootstrap endpoints stay open, the
control plane and bus require a token, and the credentials vs control scopes
are enforced per route. VZ-only (config mutation restarts the local dev
server); the middleware logic itself is backend-agnostic Go covered by unit
tests.
"""

from __future__ import annotations

import urllib.error
import urllib.request

import pytest

from fixtures.devcontrol import dev_config
from fixtures.server import resolve_server_entry

CONTROL = "shed_control_itest0000"
CREDS = "shed_credentials_itest0"


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
def test_http_auth_enforce(vz_server_dev):
    server = vz_server_dev.name
    port = int(resolve_server_entry(server)["http_port"])
    overrides = {
        "auth": {
            "http": {
                "mode": "enforce",
                "tokens": [
                    {"scope": "control", "token": CONTROL},
                    {"scope": "credentials", "token": CREDS},
                ],
            }
        }
    }
    with dev_config(overrides, server):
        # Bootstrap endpoints stay reachable without a token (shed server add).
        assert _status(port, "/api/info") == 200
        assert _status(port, "/api/ssh-host-key") == 200

        # Control plane requires a control/admin token.
        assert _status(port, "/api/sheds") == 401
        assert _status(port, "/api/sheds", token="shed_control_bogus") == 401
        assert _status(port, "/api/sheds", token=CONTROL) == 200
        assert _status(port, "/api/sheds", token=CREDS) == 403  # wrong scope

        # The credential bus requires the credentials scope.
        assert _status(port, "/api/plugins/listeners") == 401
        assert _status(port, "/api/plugins/listeners", token=CREDS) == 200
        assert _status(port, "/api/plugins/listeners", token=CONTROL) == 403
