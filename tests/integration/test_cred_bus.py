"""Phase 6: credential-bus registry-identity hardening (live).

Configures the VZ dev server with http-auth enforce + a credentials token (via
the Phase 0.5 harness) and verifies that a forged `/respond` — one that does not
correspond to a pending request the listener was dispatched — is dropped at the
registry even when presented with a valid credentials token. The per-route
scope gate is covered by test_http_auth.py / test_tls.py; this is the
below-the-handler ownership defense (response-injection / listener-squat).

VZ-only (config mutation restarts the local dev server); the ownership logic is
backend-agnostic Go covered by internal/plugin + internal/api unit tests.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.request

import pytest

from fixtures.devcontrol import dev_config, run_preflight
from fixtures.server import resolve_server_entry

CREDS_TOKEN = "shed_credentials_itestcb"


def _post(port: int, path: str, body: dict, token: str | None) -> int | None:
    req = urllib.request.Request(
        f"http://localhost:{port}{path}", data=json.dumps(body).encode(), method="POST"
    )
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status
    except urllib.error.HTTPError as e:
        e.close()  # warnings-as-errors: don't leak the response body
        return e.code


@pytest.mark.vz
@pytest.mark.slow
def test_cred_bus_forged_respond_dropped(vz_server_dev):
    server = vz_server_dev.name
    port = int(resolve_server_entry(server)["http_port"])
    overrides = {
        "auth": {
            "http": {
                "mode": "enforce",
                "tokens": [{"scope": "credentials", "token": CREDS_TOKEN}],
            }
        }
    }
    with dev_config(overrides, server):
        # A /respond that names no pending request is dropped (403) even with a
        # valid credentials token: the requestID was never dispatched to any
        # listener, so it can't be answered. Forged response-injection defense.
        body = {
            "namespace": "itest-ns",
            "type": "response",
            "in_reply_to": "forged-request-id",
            "final": True,
            "shed": {"name": "itest-shed"},
        }
        assert _post(port, "/api/plugins/listeners/itest-ns/respond", body, CREDS_TOKEN) == 403


@pytest.mark.vz
def test_public_exposure_preflight_refuses_incomplete_bundle():
    """public_exposure: true with an incomplete bundle must refuse to start —
    a non-zero exit with the gap named, and nothing bound. Uses the throwaway
    subprocess harness (not the running dev server). The dev base config has no
    auth/TLS/internal listener, so the bundle is incomplete."""
    result = run_preflight({"public_exposure": True})
    assert not result.timed_out, "an incomplete public_exposure bundle must exit, not keep running"
    assert result.returncode != 0, f"expected a non-zero exit, got {result.returncode}"
    assert "public_exposure" in result.stderr.lower(), f"stderr should name the preflight: {result.stderr!r}"
