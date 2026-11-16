"""Credential-bus response forgery + removed-key and removed-mode refusal (live).

Enrols a credentials certificate over the `_bootstrap` SSH channel and verifies
that a forged `/respond` — one that does not correspond to a pending request
the listener was dispatched — is dropped at the registry even with a valid
credentials certificate. The per-route scope gate is covered by
test_http_auth.py; this is the below-the-handler ownership defense
(response-injection / listener-squat). It runs against either dev server
(`shed_server_dev`), which it reads and does not change, over the pinned mtls
listener. (It replaces the bearer-token form that ran against the removed
token mode.)

The second test asserts the config shapes shed-server no longer loads — the
removed `public_exposure` / `auth.http.tokens` keys, the removed bearer-token
mode under both its spellings, `allow_insecure_exposure`, and an open server
bound beyond loopback — each refuse to start with a message naming the fix, and
never come up open. The third does the same for a user list (`auth.users`)
outside mtls or beside `auth.ssh` key sources. The ownership logic is
backend-agnostic Go covered by
internal/plugin + internal/api unit tests, and every refused shape has a Go
config-load test in internal/config.
"""

from __future__ import annotations

import subprocess

import pytest

from fixtures.devcontrol import OPEN_MODE_OVERRIDES, bootstrap_cert, run_preflight
from fixtures.server import resolve_server_entry
from fixtures.busclient import machine_headers
from fixtures.tlsclient import pinned_request


def test_cred_bus_forged_respond_dropped(shed_server_dev, tmp_path):
    server = shed_server_dev
    entry = resolve_server_entry(server.name)
    assert entry is not None, f"{server.name!r} not found in ~/.shed/config.yaml"
    creds = bootstrap_cert(server.name, "credentials", tmp_path)
    # A /respond that names no pending request is dropped (403) even with a
    # valid credentials certificate: the requestID was never dispatched to any
    # listener, so it can't be answered. Forged response-injection defense.
    body = {
        "namespace": "itest-ns",
        "type": "response",
        "in_reply_to": "forged-request-id",
        "final": True,
        "shed": {"name": "itest-shed"},
    }
    status, resp = pinned_request(
        entry.get("host", "localhost"), creds.https_port,
        "/api/plugins/listeners/itest-ns/respond", creds.pin, creds.pair,
        method="POST", body=body, headers=machine_headers("cred-bus"),
    )
    assert status == 403, f"forged respond: status {status}, body {resp!r}"


def _assert_refused(result, *needles: str) -> None:
    assert not result.timed_out, (
        f"the server started (it was still running at the timeout); want a refusal naming {needles}"
    )
    assert result.returncode != 0, f"expected a non-zero exit, got {result.returncode}"
    err = result.stderr.lower()
    for needle in needles:
        assert needle.lower() in err, f"stderr should name {needle!r}: {result.stderr[-800:]!r}"


@pytest.mark.vz
@pytest.mark.parametrize(
    "overrides, needles",
    [
        # Removed before this release; still refused, now pointing at mtls.
        ({"public_exposure": True}, ("public_exposure", "removed", "auth.mode: mtls")),
        ({"auth": {"http": {"tokens": [{"scope": "control", "token": "shed_control_x"}]}}},
         ("auth.http.tokens", "removed", "auth.mode: mtls")),
        # The bearer-token mode, under both spellings: refused at load, never
        # started open.
        ({"auth": {"mode": "token"}}, ('auth.mode: "token" was removed', "use auth.mode: mtls")),
        ({"auth": {"mode": "secure"}}, ('auth.mode: "secure" was removed', "use auth.mode: mtls")),
        # The open-exposure acknowledgment, with any value.
        ({"allow_insecure_exposure": True}, ("allow_insecure_exposure", "removed", "auth.mode: mtls")),
        ({"allow_insecure_exposure": False}, ("allow_insecure_exposure", "removed")),
        # An open server bound beyond loopback.
        ({**OPEN_MODE_OVERRIDES, "bind_address": "0.0.0.0"}, ("loopback only", "auth.mode: mtls")),
        ({**OPEN_MODE_OVERRIDES, "bind_address": "::"}, ("loopback only", "auth.mode: mtls")),
    ],
    ids=[
        "public_exposure", "auth.http.tokens", "mode-token", "mode-secure",
        "allow_insecure_exposure-true", "allow_insecure_exposure-false",
        "open-on-all-ipv4", "open-on-all-interfaces",
    ],
)
def test_removed_config_shapes_hard_fail(overrides, needles):
    """A config carrying a removed key or mode must be **rejected at
    startup** — a non-zero exit naming what to change — never silently
    ignored and never started open: a dropped `public_exposure: true` would
    un-loopback the plaintext listener, a token-mode config that fell back to
    open would leave a server the operator believes is gated wide open, and a
    wide-open bind is exactly what open mode no longer allows. Uses the
    throwaway subprocess harness (not the running dev server): the server
    exits at config load, before any backend init or bind, so no VM is
    needed."""
    _assert_refused(run_preflight(overrides), *needles)


def _throwaway_key(tmp_path) -> str:
    path = tmp_path / "k"
    subprocess.run(["ssh-keygen", "-t", "ed25519", "-N", "", "-q", "-f", str(path)], check=True, timeout=30)
    return " ".join((tmp_path / "k.pub").read_text().split()[:2])


@pytest.mark.vz
@pytest.mark.parametrize(
    "shape, needles",
    [
        # A user list needs mtls; an open server cannot have one.
        ("open", ("auth.users requires auth.mode: mtls",)),
        # The committed base keeps its auth.ssh key sources: beside a user
        # list they would belong to nobody, so the combination is refused.
        ("beside-auth-ssh", ("auth.users cannot be combined with auth.ssh.github_users",)),
    ],
)
def test_a_user_list_outside_its_rules_hard_fails(tmp_path, shape, needles):
    """A config with `auth.users` and any mode but mtls, or with `auth.users`
    beside `auth.ssh` key sources, refuses to start, naming the rule. (Every
    other user-list rule has a Go config-load test in internal/config.)"""
    users = {"itest-alice": {"admin": True, "keys": [{"key": _throwaway_key(tmp_path), "label": "itest"}]}}
    if shape == "open":
        overrides = {**OPEN_MODE_OVERRIDES, "auth": {"mode": "open", "users": users}}
    else:
        overrides = {"auth": {"users": users}}
    _assert_refused(run_preflight(overrides), *needles)
