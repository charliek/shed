"""The signing check (plan 035 §3.5): what the broker signs for a shed, and what it
declines BEFORE any approval prompt.

The broker signs only an exact SSH login request for a username on `ssh.allow_users`
(default `[git]`) or an exact SSHSIG blob with namespace `git`
(`crates/shed-broker/src/sign_check.rs`). Every other request is declined before the
approval gate: the app (here the harness's control client) is shown NO approval
request, the backend is never asked, the shed gets the ordinary failure shape
(`{sign operation failed, SIGN_FAILED}` — it cannot tell which rule declined it), and
the broker writes one audit line: `result: "refused"`, the audit-only code
`SIGN_REFUSED`, `reason` the refusal class, `detail` the check's bounded, escaped
detail (never the request), `approval: "none"`. The line also reaches the app as an
audit `event`.

Each cell runs the gated configuration of `test_bus_sign_gated.py` (local keys, the
committed ed25519 key, `ssh.approval.policy: shed-desktop`, a subscribed control
client), so a request that DID reach the gate would prompt the app; the golden pins
how many approval requests the control client was shown (none for a refusal, one for
a request that is signed), and each cell asserts in code that a prompt it was shown is
the request's.

Not covered here: a name beginning with `_` that is LISTED in `ssh.allow_users`.
Config load refuses such a list (exit 1, `test_config_validate.py`), and a config that
fails validation yields a policy that signs nothing, so the run-time rule ("never sign
a `_` name, whatever the list says") cannot be reached through the broker's config. It
is covered by the unit tests instead (`bus.rs`
`every_refusal_class_skips_the_gate_and_the_backend_and_is_audited`, `sign_check.rs`
`a_listed_underscore_name_is_still_reserved`); the `_bootstrap` cell below covers the
reserved rule with the default list.
"""

from __future__ import annotations

import base64
import hashlib
from pathlib import Path

import pytest
from cryptography.hazmat.primitives.serialization import load_ssh_public_key

from control_client import ControlClient
from normalize import canonical, mask_audit_entry, mask_bus_response
from sign_payloads import b64, login_request, sshsig
from synthetic_bus import SyntheticBus

FIXTURES_DIR = Path(__file__).resolve().parent / "fixtures"
ED25519_PUB_B64 = (FIXTURES_DIR / "test_ed25519.pub").read_text().split()[1]
RSA_PUB_B64 = (FIXTURES_DIR / "test_rsa.pub").read_text().split()[1]

# `ssh-keygen -lf fixtures/test_ed25519.pub`.
ED25519_FINGERPRINT = "SHA256:QSbP+oRmKHQkEp2CsJn72x/5Qxyq5lsK6sib0fK1mr0"

SHED = {"name": "web", "backend": "vz", "server": "mini2"}

# What the shed is told for every refusal, whatever its class.
SIGN_FAILED = {"error": "sign operation failed", "code": "SIGN_FAILED"}

# The old cells' arbitrary challenge: neither a login request nor an SSHSIG blob.
OLD_CHALLENGE = b"shed-host-agent gated-sign differential challenge v1"


def _login(user: bytes) -> bytes:
    return login_request(user, ED25519_PUB_B64, b"ssh-ed25519")


def _sign_request(data: bytes, *, pub_b64: str = ED25519_PUB_B64, flags: int = 0) -> dict:
    return {
        "id": "sign-1",
        "namespace": "ssh-agent",
        "type": "request",
        "final": True,
        "timestamp": "2026-07-10T00:00:00Z",
        "payload": {"operation": "sign", "public_key": pub_b64, "data": b64(data), "flags": flags},
        "shed": SHED,
    }


def _run(daemon, config: str, request: dict, *, approve: bool):
    """Return `scenario(impl)`: push `request` with a subscribed control client; when
    `approve`, answer the one approval prompt it raises. Returns the masked response,
    the durable audit line, and how many approval requests the control client was shown
    in all (each asserted to be this request's prompt)."""

    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, config.replace("{server}", bus.url), install_ssh_key=True) as d:
                with ControlClient(d.control_sock) as app:
                    bus.wait_for_subscribe("ssh-agent", timeout=10.0)

                    bus.push_request("ssh-agent", request)
                    if approve:
                        ar = app.await_frame("approval_request", timeout=10.0)
                        app.decide(ar["id"], "approve")

                    response = bus.await_response("ssh-agent", timeout=10.0)
                    # The audit line fans out to the app after the response; once it is
                    # here, any prompt this request caused would have arrived first (the
                    # bridge sends both on one ordered channel).
                    event = app.await_frame("event", timeout=10.0)
                    audit = d.read_audit_jsonl(expect=1, timeout=10.0)[0]
                    for key in ("shed", "ns", "op", "result", "approval", "code", "reason"):
                        assert event.get(key) == audit.get(key), (key, event, audit)
                    prompts = [f for f in app.frames() if f.get("type") == "approval_request"]
                    for prompt in prompts:
                        assert prompt["namespace"] == "ssh-agent", prompt
                        assert prompt["op"] == "sign", prompt
                        assert prompt["shed"] == SHED["name"], prompt
                        assert "server" not in prompt, prompt
                    return {
                        "response": canonical(mask_bus_response(response)),
                        "audit": canonical(mask_audit_entry(audit)),
                        "approval_requests": len(prompts),
                    }

    return scenario


def _assert_refused(result: dict, reason: str, detail: str) -> None:
    resp = result["response"]
    assert resp["in_reply_to"] == "sign-1"
    assert resp["shed"] == SHED
    assert resp["payload"] == SIGN_FAILED
    assert result["approval_requests"] == 0, "a refusal must never prompt"
    want = {
        "ts": "<ts>",
        "shed": "web",
        "ns": "ssh-agent",
        "op": "sign",
        "result": "refused",
        "detail": detail,
        "code": "SIGN_REFUSED",
        "reason": reason,
        "approval": "none",
    }
    assert result["audit"] == want


# --- declined before any prompt ------------------------------------------------------


# case -> (data, refusal class, the check's detail)
REFUSED = {
    "another-username": (_login(b"deploy"), "username_not_allowed", "login user=deploy"),
    "reserved-username": (_login(b"_bootstrap"), "reserved_username", "login user=_bootstrap"),
    "old-arbitrary-challenge": (OLD_CHALLENGE, "unparseable_data", "login: truncated"),
    "trailing-byte": (_login(b"git") + b"\x00", "unparseable_data", "login: trailing bytes"),
    "sshsig-file-namespace": (sshsig(b"file"), "wrong_namespace", "sshsig namespace=file"),
}


@pytest.mark.differential
@pytest.mark.parametrize("case", sorted(REFUSED))
def test_sign_refused_before_any_prompt(daemon, sign_config, differential, case):
    data, reason, detail = REFUSED[case]
    result = differential(_run(daemon, sign_config("{server}"), _sign_request(data), approve=False))
    _assert_refused(result, reason, detail)


# case -> (the algorithm the login names, the request's flags)
RSA_FLAG_MISMATCH = {
    # The payload names rsa-sha2-512 but the flags ask for rsa-sha2-256.
    "sha512-payload-flags-2": (b"rsa-sha2-512", 2),
    # Both hash bits: no payload agrees (this used to sign as rsa-sha2-256).
    "sha256-payload-flags-6": (b"rsa-sha2-256", 6),
}


@pytest.mark.differential
@pytest.mark.parametrize("case", sorted(RSA_FLAG_MISMATCH))
def test_sign_rsa_flags_disagreeing_with_the_payload_refused(
    daemon, sign_config, differential, case
):
    algorithm, flags = RSA_FLAG_MISMATCH[case]
    data = login_request(b"git", RSA_PUB_B64, algorithm)
    request = _sign_request(data, pub_b64=RSA_PUB_B64, flags=flags)
    result = differential(_run(daemon, sign_config("{server}"), request, approve=False))
    _assert_refused(result, "unparseable_data", "login: RSA flags do not name the algorithm's hash")


# --- signed after the prompt ---------------------------------------------------------


def _assert_signed(result: dict, data: bytes) -> None:
    assert result["approval_requests"] == 1
    payload = result["response"]["payload"]
    assert payload["format"] == "ssh-ed25519"
    assert payload["rest"] == ""
    blob = base64.b64decode(payload["blob"])
    pub = load_ssh_public_key((FIXTURES_DIR / "test_ed25519.pub").read_bytes())
    pub.verify(blob, data)  # raises if the signature is not over exactly `data`
    # Today's ok audit, unchanged by the check.
    assert result["audit"] == {
        "ts": "<ts>",
        "shed": "web",
        "ns": "ssh-agent",
        "op": "sign",
        "result": "ok",
        "detail": "ssh-ed25519",
        "approval": "shed-desktop",
        "decided_by": "user",
    }


@pytest.mark.differential
def test_sign_git_login_signed(daemon, sign_config, differential):
    data = _login(b"git")
    result = differential(_run(daemon, sign_config("{server}"), _sign_request(data), approve=True))
    _assert_signed(result, data)


@pytest.mark.differential
def test_sign_git_sshsig_signed(daemon, sign_config, differential):
    data = sshsig(b"git")
    result = differential(_run(daemon, sign_config("{server}"), _sign_request(data), approve=True))
    _assert_signed(result, data)


# A machine that adds `deploy` to `ssh.allow_users` (which requires `ssh.offer_keys`)
# signs a login as `deploy` with an offered key — the config reaches the check.
DEPLOY_CONFIG = """\
server: {server}
ssh:
  mode: local-keys
  allow_users: [git, deploy]
  offer_keys:
    - "%s"
  approval:
    policy: shed-desktop
logging:
  enabled: true
  path: {audit_log}
""" % ED25519_FINGERPRINT


@pytest.mark.differential
def test_sign_listed_username_with_offered_key_signed(daemon, differential):
    data = _login(b"deploy")
    result = differential(_run(daemon, DEPLOY_CONFIG, _sign_request(data), approve=True))
    _assert_signed(result, data)


def test_fingerprint_constant_is_the_committed_key():
    raw = base64.b64decode(ED25519_PUB_B64)
    want = "SHA256:" + base64.b64encode(hashlib.sha256(raw).digest()).decode().rstrip("=")
    assert ED25519_FINGERPRINT == want
