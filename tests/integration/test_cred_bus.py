"""Credential-bus registry-identity hardening + v0.7.1 removed-key rejection (live).

Puts the VZ dev server in `auth.mode: secure` (the local SSH key allowlisted),
mints a credentials token over the `_bootstrap` SSH channel, and verifies that a
forged `/respond` — one that does not correspond to a pending request the
listener was dispatched — is dropped at the registry even with a valid
credentials token. The per-route scope gate is covered by test_http_auth.py;
this is the below-the-handler ownership defense (response-injection /
listener-squat). A second test asserts the removed `public_exposure` /
`auth.http.tokens` keys hard-fail at startup.

VZ-only (config mutation restarts the local dev server); the ownership logic is
backend-agnostic Go covered by internal/plugin + internal/api unit tests.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.request
from pathlib import Path

import pytest

from fixtures.devcontrol import bootstrap_mint, dev_config, run_preflight
from fixtures.server import resolve_server_entry


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
    pubkey = (Path.home() / ".ssh" / "id_ed25519.pub").read_text().strip()
    overrides = {"auth": {"mode": "secure", "ssh": {"authorized_keys": [pubkey]}}}
    with dev_config(overrides, server):
        creds = bootstrap_mint(server, "credentials")
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
        assert _post(port, "/api/plugins/listeners/itest-ns/respond", body, creds) == 403


@pytest.mark.vz
def test_removed_auth_keys_hard_fail():
    """v0.7.1 removed `public_exposure` and `auth.http.tokens`. A config that still
    carries either must be **rejected at startup** — a non-zero exit naming the
    removed key — never silently ignored: a dropped `public_exposure: true` would
    un-loopback the plaintext listener, and a dropped token list would leave a
    server the operator believes is gated wide open. Uses the throwaway subprocess
    harness (not the running dev server); the server exits at config load, before
    any backend init, so no VM is needed."""
    pe = run_preflight({"public_exposure": True})
    assert not pe.timed_out, "a removed public_exposure key must exit, not keep running"
    assert pe.returncode != 0, f"expected a non-zero exit, got {pe.returncode}"
    assert "public_exposure" in pe.stderr.lower(), f"stderr should name the removed key: {pe.stderr!r}"
    assert "removed" in pe.stderr.lower(), f"stderr should say the key was removed: {pe.stderr!r}"

    tok = run_preflight({"auth": {"http": {"tokens": [{"scope": "control", "token": "shed_control_x"}]}}})
    assert not tok.timed_out, "a removed auth.http.tokens key must exit, not keep running"
    assert tok.returncode != 0, f"expected a non-zero exit, got {tok.returncode}"
    assert "tokens" in tok.stderr.lower(), f"stderr should name the removed key: {tok.stderr!r}"
    assert "removed" in tok.stderr.lower(), f"stderr should say the key was removed: {tok.stderr!r}"
