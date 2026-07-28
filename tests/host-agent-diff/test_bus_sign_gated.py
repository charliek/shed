"""The gated cross-surface (A+B) **sign** flow — the capstone scenario.

A single bus `sign` request (surface B) drives a delegated `approval_request` to the
connected desktop consumer (surface A); on **approve** the daemon signs the challenge
with the committed ed25519 key and returns an `SSHSignResponse` on the bus PLUS an
audit `event` to the desktop and a durable JSONL line; on **deny** it returns
`{approval denied, SIGN_FAILED}` and a `denied` audit. All of that wire-visible
output is golden-pinned under the harness's canonicalization (`differential`).

Ties BOTH surfaces together: the bus response (B), the approval_request + audit
event (A), and the durable audit line all travel one flow, wired in single-server
mode (no `discovery:`) with `ssh.mode: local-keys` + `ssh.approval.policy:
shed-desktop`.

**Deterministic ed25519 → the signature blob is pinned UNMASKED.** ed25519 signing
is deterministic (RFC 8032: the nonce is derived from key+message, not randomness),
so the `ed25519-dalek` backend, loading the committed `~/.ssh/id_ed25519` and signing
the fixed challenge, always produces the SAME 64 bytes. The `mask_bus_response` mask
leaves `payload` (hence `blob`) unmasked, so the golden carries the signature
byte-for-byte — and we additionally pin it inline (`EXPECTED_SIGN_BLOB_B64`) as an
absolute constant.

Scoped by construction: the daemon in single-server mode also GETs
`/api/egress/stream` (501 → 5m backoff) and subscribes to `docker-credentials`. The
synthetic bus tolerates those and only the ssh-agent flow is pinned here. See the
harness README.
"""

from __future__ import annotations

import base64
from pathlib import Path

import pytest

from desktop_client import DesktopClient, wait_for_consumer
from normalize import (
    canonical,
    mask_approval_request,
    mask_audit_entry,
    mask_bus_response,
    mask_event,
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

# A FIXED challenge → base64 for the sign request's `data`. Fixed so the deterministic
# ed25519 signature (and thus the pinned blob) is stable.
CHALLENGE = b"shed-host-agent gated-sign differential challenge v1"
CHALLENGE_B64 = base64.b64encode(CHALLENGE).decode("ascii")

# The deterministic ed25519 signature (RFC 8032) of CHALLENGE by the committed key —
# base64(raw 64-byte sig). Byte-identical across Go `x/crypto/ssh` and Rust
# `ed25519-dalek` (the whole point). Pinned so the approve test is an absolute golden,
# not just a Go-agrees-with-Rust check; regenerate ONLY if the key or challenge change.
EXPECTED_SIGN_BLOB_B64 = (
    "r5+/n2R3osYEEdZqWq+BBYbMDKAHG7ISLrEb5t7AirSoQwaE9s2t2p3/SICRw2+hEUWwf+9"
    "Lnt+asgFR21PYDQ=="
)

# The request Envelope shed-server would deliver to the ssh-agent listener: a `sign`
# op carrying the committed pubkey blob + the fixed challenge, with a fixed id + shed
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
        "data": CHALLENGE_B64,
        "flags": 0,
    },
    "shed": {"name": "web", "backend": "vz", "server": "mini2"},
}

# The `shed` block the response/approval_request/event/audit must all carry (echoed
# from the request).
EXPECTED_SHED = {"name": "web", "backend": "vz", "server": "mini2"}


def _sign_scenario(daemon, sign_config, decision: str):
    """Build a `scenario(impl)` for the `differential` fixture: run the full gated
    sign flow against `impl`'s daemon and return the canonical, masked A+B artifacts.

    `decision` is the desktop app's verdict (`"approve"` or `"deny"`). The consumer is
    connected + PROMOTED (deadline poll on `consumer_connected`) before the sign is
    injected — else the gate has no consumer and fails closed."""

    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, sign_config(bus.url), install_ssh_key=True) as d:
                with DesktopClient(str(d.desktop_sock)) as app:
                    app.send_hello()
                    # Promotion first (the gate routes to the active consumer); then the
                    # bus subscription (the SSE drainer must be attached before pushing).
                    wait_for_consumer(d, connected=True, timeout=10.0)
                    bus.wait_for_subscribe("ssh-agent", timeout=10.0)

                    bus.push_request("ssh-agent", SIGN_REQUEST)

                    # Surface A: the delegated approval prompt, then the app's verdict.
                    ar = app.await_frame("approval_request", timeout=10.0)
                    app.send_approval_response(ar["id"], decision)

                    # Surface B: the bus response; then surface A: the audit `event`
                    # fan-out; then the durable JSONL line (written before the event on
                    # written before the fan-out, so it is present once the event has arrived).
                    response = bus.await_response("ssh-agent", timeout=10.0)
                    event = app.await_frame("event", timeout=10.0)
                    audit = d.read_audit_jsonl(expect=1, timeout=10.0)[0]

                    return {
                        "approval_request": canonical(mask_approval_request(ar)),
                        "response": canonical(mask_bus_response(response)),
                        "event": canonical(mask_event(event)),
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

    # --- surface A: the approval_request the app was prompted with (masked shape) ---
    ar = result["approval_request"]
    assert ar["type"] == "approval_request"
    assert ar["v"] == 2
    assert ar["namespace"] == "ssh-agent"
    assert ar["op"] == "sign"
    assert ar["shed"] == "web"
    # The reason shown to the app is the fixed "SSH sign request" — the gate runs
    # FIRST, before the key type is parsed, so the detail is NOT the key type.
    assert ar["detail"] == "SSH sign request"
    assert "server" not in ar, "single-server mode omits approval_request.server"
    # Volatile fields masked (shape-asserted inside mask_approval_request).
    assert ar["id"] == "<id>"
    assert ar["ts"] == "<ts>"
    assert ar["expires_at"] == "<expires_at>"

    # --- surface B: the bus sign response; the signature blob compared UNMASKED ---
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

    # --- surface A: the audit `event` fan-out (full shape pins the omitempty set) ---
    assert result["event"] == {
        "v": 2,
        "type": "event",
        "id": "<id>",
        "ts": "<ts>",
        "kind": "audit",
        "shed": "web",
        "ns": "ssh-agent",
        "op": "sign",
        "result": "ok",
        "detail": "ssh-ed25519",
        "approval": "shed-desktop",
        "decided_by": "user",
    }

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

    # The approval_request the app saw is identical to the approve path (the gate
    # prompts the same way; only the app's verdict differs).
    ar = result["approval_request"]
    assert ar["namespace"] == "ssh-agent"
    assert ar["op"] == "sign"
    assert ar["shed"] == "web"
    assert ar["detail"] == "SSH sign request"
    assert "server" not in ar

    # --- surface B: the error response, NOT a signature ---
    resp = result["response"]
    assert resp["type"] == "response"
    assert resp["final"] is True
    assert resp["in_reply_to"] == "sign-1"
    assert resp["namespace"] == "ssh-agent"
    assert resp["shed"] == EXPECTED_SHED
    # The guest gets {approval denied, SIGN_FAILED}; the challenge is never signed.
    assert resp["payload"] == {"error": "approval denied", "code": "SIGN_FAILED"}
    assert resp["id"] == "<id>"
    assert resp["timestamp"] == "<ts>"

    # --- surface A: the denied audit event — NO detail, NO code (unlike Docker) ---
    assert result["event"] == {
        "v": 2,
        "type": "event",
        "id": "<id>",
        "ts": "<ts>",
        "kind": "audit",
        "shed": "web",
        "ns": "ssh-agent",
        "op": "sign",
        "result": "denied",
        "approval": "shed-desktop",
        "decided_by": "user",
    }

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


# The fail-closed audit both A3 drives produce: a denied `sign`, `approval:"shed-desktop"`,
# and — unlike the consumer-deny path above (`decided_by:"user"`) — NO `decided_by`. The
# no-consumer / timeout gate returns the ERROR path (`desktopGate.Approve` →
# `ApprovalOutcome{}` + err on Go; `denied_no_decision()` on Rust), so the empty
# `DecidedBy`/`Scope`/`TTL` are all omitted (skipped when empty).
FAIL_CLOSED_AUDIT = {
    "ts": "<ts>",
    "shed": "web",
    "ns": "ssh-agent",
    "op": "sign",
    "result": "denied",
    "approval": "shed-desktop",
}


@pytest.mark.differential
def test_bus_sign_no_consumer_fails_closed(daemon, sign_config, differential):
    """A3 · no-consumer fail-closed. With NO desktop consumer connected, a gated `sign`
    denies immediately (Go `errNoConsumer`, Rust `denied_no_decision()` — no wait): the
    guest gets `{approval denied, SIGN_FAILED}` and the durable line is a denied audit
    with no `decided_by`. The gated-sign capstone minus the consumer connect."""

    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, sign_config(bus.url), install_ssh_key=True) as d:
                bus.wait_for_subscribe("ssh-agent", timeout=10.0)
                bus.push_request("ssh-agent", SIGN_REQUEST)
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
    assert resp["id"] == "<id>"
    assert resp["timestamp"] == "<ts>"
    assert result["audit"] == FAIL_CLOSED_AUDIT


@pytest.mark.differential
def test_bus_sign_timeout_fails_closed(daemon, sign_config, differential):
    """A3 · timeout fail-closed. A consumer connects and receives the `approval_request`
    but NEVER replies; with a SHORT `approval_timeout` (1s) the daemon times out and
    deny — Go on `time.After(s.timeout)`, Rust on the `tokio::time::sleep` arm of the
    `select!` — returning `{approval denied, SIGN_FAILED}` + the same no-`decided_by`
    denied audit. This exercises the EXISTING timeout arm; per D7 (ACCEPT, no divergence)
    the Rust `select!` has NO shutdown arm (adding one would make Rust faster than Go)."""

    def scenario(impl):
        with SyntheticBus() as bus:
            # Append a short top-level approval_timeout so the wait is bounded to ~1s.
            cfg = sign_config(bus.url) + "approval_timeout: 1s\n"
            with daemon(impl, cfg, install_ssh_key=True) as d:
                with DesktopClient(str(d.desktop_sock)) as app:
                    app.send_hello()
                    # The consumer must be PROMOTED before the sign, else it's the
                    # no-consumer path, not the timeout path.
                    wait_for_consumer(d, connected=True, timeout=10.0)
                    bus.wait_for_subscribe("ssh-agent", timeout=10.0)

                    bus.push_request("ssh-agent", SIGN_REQUEST)
                    # The prompt reaches the app (which never answers) → timeout deny.
                    app.await_frame("approval_request", timeout=10.0)

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
