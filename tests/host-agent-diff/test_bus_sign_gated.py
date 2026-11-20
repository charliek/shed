"""The gated **sign** flow — the capstone scenario.

A single bus `sign` request drives a delegated approval request through the app's
broker to its approval gate, which the harness's control socket stands in for (the
app's Coordinator in the app); on **approve** the broker signs the request's data with
the committed ed25519 key and returns an `SSHSignResponse` on the bus and writes a
durable JSONL audit line (also fanned out to the app as an `event`); on **deny** it
returns `{approval denied, SIGN_FAILED}` and a `denied` audit. The bus response and the
durable audit line are golden-pinned under the harness's canonicalization
(`differential`); the approval request the control client is shown is asserted in code
(namespace, op, shed, detail, no server).

Wired in single-server mode (no `discovery:`) with `ssh.mode: local-keys` +
`ssh.approval.policy: shed-desktop`.

**Deterministic ed25519 → the signature blob is pinned UNMASKED.** ed25519 signing
is deterministic (RFC 8032: the nonce is derived from key+message, not randomness),
so the `ed25519-dalek` backend, loading the committed `~/.ssh/id_ed25519` and signing
the fixed data, always produces the SAME 64 bytes.

**The data is a login request for `git`** (plan 035 §3.5): the daemon signs only an SSH
login as an allowed username (default `[git]`) or a `git` SSHSIG, and declines anything
else before any prompt (`test_bus_sign_refused.py`). The data here is an exact login for
`git` with the committed key and a fixed session id (`sign_payloads.py`), so it passes
the check and reaches the approval gate, and its signature stays deterministic. The `mask_bus_response` mask
leaves `payload` (hence `blob`) unmasked, so the golden carries the signature
byte-for-byte — and we additionally pin it inline (`EXPECTED_SIGN_BLOB_B64`) as an
absolute constant.

Scoped by construction: the broker in single-server mode also GETs
`/api/egress/stream` (501 → 5m backoff) and subscribes to `docker-credentials`. The
synthetic bus tolerates those and only the ssh-agent flow is pinned here. See the
harness README.
"""

from __future__ import annotations

import base64
from pathlib import Path

import pytest

from control_client import ControlClient
from sign_payloads import b64, login_request
from normalize import (
    assert_rfc3339,
    canonical,
    mask_audit_entry,
    mask_bus_response,
)
from synthetic_bus import SyntheticBus

# NOTE: `fixtures/test_ed25519{,.pub}` is a THROWAWAY, non-secret, passphrase-less
# ed25519 keypair generated once solely for these cells (comment `hadiff-test`).
# It is intentionally committed so every run loads an identical key; it guards
# nothing and must never be reused anywhere real. (Not `.gitignore`-blocked — the repo
# ignores `*.key`/`*.pem`, not extension-less OpenSSH keys.)
FIXTURES_DIR = Path(__file__).resolve().parent / "fixtures"

# The committed test key's SSH-wire public-key blob, derived at collection time from
# the 2nd whitespace field of `test_ed25519.pub` — exactly `base64(string("ssh-ed25519")
# || string(pubkey_bytes))`, i.e. the Rust
# `marshaled_pub`. Deriving it from the committed `.pub` guarantees it never drifts
# from the private key the `daemon` fixture installs into `<HOME>/.ssh/id_ed25519`.
TEST_ED25519_PUB_BLOB = (FIXTURES_DIR / "test_ed25519.pub").read_text().split()[1]

# The same value, pinned for documentation + a loud trip-wire if the committed key is
# ever regenerated (see `test_committed_key_pubkey_blob_is_pinned`).
PINNED_PUB_BLOB = "AAAAC3NzaC1lZDI1NTE5AAAAILMdrPP0NLsfgrc8JIa6OWX1qhgyfW/UwJTSXdRuUsJJ"

# The FIXED data for the sign request: an exact `publickey` login request for `git`
# with the committed key (fixed session id), so the signing check passes it and the
# deterministic ed25519 signature (and thus the pinned blob) is stable.
LOGIN = login_request(b"git", TEST_ED25519_PUB_BLOB, b"ssh-ed25519")
LOGIN_B64 = b64(LOGIN)

# The deterministic ed25519 signature (RFC 8032) of LOGIN by the committed key —
# base64(raw 64-byte sig), as Python's `cryptography` computes it from the committed
# private key. Pinned so the approve test is an absolute golden; regenerate ONLY if the
# key or the login data change.
EXPECTED_SIGN_BLOB_B64 = (
    "6lRG1fs7BQ0Aw0o0eq8T5QYDCjUI9n0LjOSHpCd9VjavlQGEoc3NTaL5zolLah5mQJTtBYf5wRNG"
    "a1W7XN71Aw=="
)

# The request Envelope shed-server would deliver to the ssh-agent listener: a `sign`
# op carrying the committed pubkey blob + the fixed login, with a fixed id + shed
# so the correlation (`in_reply_to`), the echoed `shed`, and the audit/approval `shed`
# are pinned constants the flow must reproduce.
SIGN_REQUEST = {
    "id": "sign-1",
    "namespace": "ssh-agent",
    "type": "request",
    "final": True,
    "timestamp": "2026-07-10T00:00:00Z",
    "payload": {
        "operation": "sign",
        "public_key": TEST_ED25519_PUB_BLOB,
        "data": LOGIN_B64,
        "flags": 0,
    },
    "shed": {"name": "web", "backend": "vz", "server": "mini2"},
}

# The `shed` block the response/approval_request/event/audit must all carry (echoed
# from the request).
EXPECTED_SHED = {"name": "web", "backend": "vz", "server": "mini2"}


def assert_sign_prompt(ar: dict) -> None:
    """The approval request the app is shown for SIGN_REQUEST: the ssh-agent `sign` for
    shed `web`, the fixed reason (not the key type), no `server` in single-server mode,
    and a deadline."""
    assert ar["type"] == "approval_request", ar
    assert ar["namespace"] == "ssh-agent", ar
    assert ar["op"] == "sign", ar
    assert ar["shed"] == "web", ar
    assert ar["detail"] == "SSH sign request", ar
    assert "server" not in ar, "single-server mode omits the request's server"
    assert isinstance(ar["id"], str) and ar["id"], ar
    assert_rfc3339(ar["ts"], "approval_request.ts")
    assert_rfc3339(ar["expires_at"], "approval_request.expires_at")


def assert_event_matches_audit(event: dict, audit: dict) -> None:
    """The audit `event` the app is sent carries the durable line's request fields."""
    for key in ("shed", "ns", "op", "result", "approval"):
        assert event.get(key) == audit.get(key), (key, event, audit)


def _sign_scenario(daemon, sign_config, decision: str):
    """Build a `scenario(impl)` for the `differential` fixture: run the full gated
    sign flow against the broker and return the canonical, masked bus response and
    durable audit line.

    `decision` is the app's verdict (`"approve"` or `"deny"`), given through the control
    socket for the approval request it was shown."""

    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, sign_config(bus.url), install_ssh_key=True) as d:
                with ControlClient(d.control_sock) as app:
                    # The SSE drainer must be attached before pushing.
                    bus.wait_for_subscribe("ssh-agent", timeout=10.0)

                    bus.push_request("ssh-agent", SIGN_REQUEST)

                    # The delegated approval request, then the app's verdict.
                    ar = app.await_frame("approval_request", timeout=10.0)
                    assert_sign_prompt(ar)
                    app.decide(ar["id"], decision)

                    # The bus response; then the audit `event` fan-out; then the durable
                    # JSONL line (written before the fan-out, so it is present once the
                    # event has arrived).
                    response = bus.await_response("ssh-agent", timeout=10.0)
                    event = app.await_frame("event", timeout=10.0)
                    audit = d.read_audit_jsonl(expect=1, timeout=10.0)[0]
                    assert_event_matches_audit(event, audit)
                    prompts = [f for f in app.frames() if f.get("type") == "approval_request"]
                    assert len(prompts) == 1, prompts

                    return {
                        "response": canonical(mask_bus_response(response)),
                        "audit": canonical(mask_audit_entry(audit)),
                    }

    return scenario


def test_committed_key_pubkey_blob_is_pinned():
    """Trip-wire: the committed `test_ed25519.pub` blob equals the pinned constant, so
    a regenerated key (which would also change the pinned signature) fails loudly here
    instead of silently signing a different pubkey downstream."""
    assert TEST_ED25519_PUB_BLOB == PINNED_PUB_BLOB, (
        "fixtures/test_ed25519.pub changed; if the key was regenerated on purpose, "
        "re-pin PINNED_PUB_BLOB + EXPECTED_SIGN_BLOB_B64"
    )
    # It really is an ssh-ed25519 wire key (11-byte algo string + 32-byte pubkey).
    raw = base64.b64decode(TEST_ED25519_PUB_BLOB)
    assert raw[:15] == b"\x00\x00\x00\x0bssh-ed25519", "pubkey blob is not ssh-ed25519 wire form"
    assert len(raw) == 51, f"ssh-ed25519 wire pubkey should be 51 bytes, got {len(raw)}"


@pytest.mark.differential
def test_bus_sign_gated_approve(daemon, sign_config, differential):
    result = differential(_sign_scenario(daemon, sign_config, "approve"))
    # (The approval request the app was shown is asserted inside the scenario.)

    # --- the bus sign response; the signature blob compared UNMASKED ---
    resp = result["response"]
    assert resp["type"] == "response"
    assert resp["final"] is True
    assert resp["in_reply_to"] == "sign-1"  # correlation echoes the request id
    assert resp["namespace"] == "ssh-agent"
    assert resp["shed"] == EXPECTED_SHED  # copied verbatim so shed-server can route
    payload = resp["payload"]
    assert payload["format"] == "ssh-ed25519"
    assert payload["rest"] == ""
    blob = payload["blob"]
    # A real raw ed25519 signature: 64 bytes, deterministic → byte-identical here.
    assert len(base64.b64decode(blob)) == 64, "sign blob is not a 64-byte ed25519 sig"
    assert blob == EXPECTED_SIGN_BLOB_B64, f"unexpected sign blob (unmasked): {blob!r}"
    # Volatile envelope fields masked.
    assert resp["id"] == "<id>"
    assert resp["timestamp"] == "<ts>"

    # --- durable audit JSONL (channel 2) — the masked record, golden-pinned ---
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
def test_bus_sign_gated_deny(daemon, sign_config, differential):
    result = differential(_sign_scenario(daemon, sign_config, "deny"))
    # The approval request the app saw is the approve path's (asserted inside the
    # scenario): the gate prompts the same way; only the app's verdict differs.

    # --- the error response, NOT a signature ---
    resp = result["response"]
    assert resp["type"] == "response"
    assert resp["final"] is True
    assert resp["in_reply_to"] == "sign-1"
    assert resp["namespace"] == "ssh-agent"
    assert resp["shed"] == EXPECTED_SHED
    # The guest gets {approval denied, SIGN_FAILED}; the data is never signed.
    assert resp["payload"] == {"error": "approval denied", "code": "SIGN_FAILED"}
    assert resp["id"] == "<id>"
    assert resp["timestamp"] == "<ts>"

    # --- durable audit JSONL: the denied record (no detail/code/scope/ttl) ---
    assert result["audit"] == {
        "ts": "<ts>",
        "shed": "web",
        "ns": "ssh-agent",
        "op": "sign",
        "result": "denied",
        "approval": "shed-desktop",
        "decided_by": "user",
    }


# The fail-closed audit of the timeout: a denied `sign`, `approval:"shed-desktop"`, and
# — unlike the person's deny above (`decided_by:"user"`) — NO `decided_by`. The gate's
# timeout returns `denied_no_decision()`, so the empty `DecidedBy`/`Scope`/`TTL` are all
# omitted (skipped when empty). (The retired daemon's no-consumer cell went with its
# desktop socket: the app's broker always has its consumer, the app itself.)
FAIL_CLOSED_AUDIT = {
    "ts": "<ts>",
    "shed": "web",
    "ns": "ssh-agent",
    "op": "sign",
    "result": "denied",
    "approval": "shed-desktop",
}


@pytest.mark.differential
def test_bus_sign_timeout_fails_closed(daemon, sign_config, differential):
    """Timeout fail-closed. The control client is shown the approval request but NEVER
    decides; with a SHORT `approval_timeout` (1s) the bridge's approval gate times out
    and denies (the `tokio::time::sleep` arm of its `select!`), returning
    `{approval denied, SIGN_FAILED}` + the no-`decided_by` denied audit."""

    def scenario(impl):
        with SyntheticBus() as bus:
            # Append a short top-level approval_timeout so the wait is bounded to ~1s.
            cfg = sign_config(bus.url) + "approval_timeout: 1s\n"
            with daemon(impl, cfg, install_ssh_key=True) as d:
                with ControlClient(d.control_sock) as app:
                    bus.wait_for_subscribe("ssh-agent", timeout=10.0)

                    bus.push_request("ssh-agent", SIGN_REQUEST)
                    # The prompt reaches the app (which never answers) → timeout deny.
                    assert_sign_prompt(app.await_frame("approval_request", timeout=10.0))

                    response = bus.await_response("ssh-agent", timeout=10.0)
                    audit = d.read_audit_jsonl(expect=1, timeout=10.0)[0]
                    return {
                        "response": canonical(mask_bus_response(response)),
                        "audit": canonical(mask_audit_entry(audit)),
                    }

    result = differential(scenario)
    resp = result["response"]
    assert resp["type"] == "response"
    assert resp["final"] is True
    assert resp["in_reply_to"] == "sign-1"
    assert resp["namespace"] == "ssh-agent"
    assert resp["shed"] == EXPECTED_SHED
    assert resp["payload"] == {"error": "approval denied", "code": "SIGN_FAILED"}
    assert result["audit"] == FAIL_CLOSED_AUDIT
