"""The SSH backend differential — surface-B `list`/`sign`/`status` in BOTH backend
modes, asserted equal across the Go `cmd/shed-host-agent` and the Rust
`crates/shed-host-agent`.

Two modes:

* **local-keys** (`ssh.mode: local-keys`, `SSH_AUTH_SOCK` stripped, the committed
  `id_ed25519`+`id_rsa`+`id_ecdsa` fixtures installed in each `$HOME/.ssh`): the
  daemon signs with the on-disk keys. `list` is masked-canonical-equal + its durable
  audit line diffed; `sign` verifies the produced RSA/ECDSA blob per-impl against the
  fixture pubkey (verify-not-bytes, per the harness plan) and diffs the `format`;
  `status` is canonical-equal.
* **agent-forward** (auto-detect: no `ssh.mode`, `SSH_AUTH_SOCK` → a per-impl
  `fake_ssh_agent.py`): the daemon proxies to the fake host agent. `list` is
  canonical-equal (3 identities); `sign` ed25519 → the fake's REAL deterministic
  signature, so the blob is compared UNMASKED; `sign` rsa flags=2 → the fake's canned
  blob byte-equal + the recorded `flags==2` (wire passthrough); `status` is
  canonical-equal. The **fake's transcript** (msg type / keyblob / data / flags) is
  compared Go-vs-Rust so the two daemons are proven to issue the same wire requests.

**Gate policy — `approve-all`, deliberately.** These cells prove the *backend signing
and wire behavior*, not the approval gate — the gated (shed-desktop delegate →
approve/deny) `sign` path is already differentially enforced by `test_bus_sign_gated.py`
(with a deterministic ed25519 blob compared unmasked). Using `approve-all` here avoids
a scripted desktop consumer per cell (and the flakiness that adds) while still driving
the full decode → gate → backend → response + audit flow. The `list`/`status` ops are
ungated regardless.
"""

from __future__ import annotations

import base64
import struct
from pathlib import Path

import pytest
from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.asymmetric import ec, padding, utils
from cryptography.hazmat.primitives.serialization import load_ssh_public_key

from fake_ssh_agent import CANNED_RSA_BLOB, FakeSshAgent
from normalize import canonical, mask_audit_entry, mask_bus_response
from synthetic_bus import SyntheticBus

FIXTURES_DIR = Path(__file__).resolve().parent / "fixtures"

# The three committed fixtures, installed as `id_<algo>` by the `daemon` fixture and
# loaded by the local-keys backend in this exact order (STANDARD_KEY_FILES).
THREE_KEYS = ("test_ed25519", "test_rsa", "test_ecdsa")

# Per-fixture SSH-wire marshaled public-key blob (2nd field of the `.pub`) — what a
# `sign` request carries as `public_key`, and what the backend matches against.
ED25519_PUB_B64 = (FIXTURES_DIR / "test_ed25519.pub").read_text().split()[1]
RSA_PUB_B64 = (FIXTURES_DIR / "test_rsa.pub").read_text().split()[1]
ECDSA_PUB_B64 = (FIXTURES_DIR / "test_ecdsa.pub").read_text().split()[1]

# A fixed challenge (fixed so the deterministic ed25519 sig is stable).
CHALLENGE = b"shed-host-agent ssh-backend differential challenge v1"
CHALLENGE_B64 = base64.b64encode(CHALLENGE).decode("ascii")

# The `shed` block every request carries; echoed onto the response/audit.
SHED = {"name": "web", "backend": "vz", "server": "mini2"}
EXPECTED_SHED = dict(SHED)

# The list response's keys, in load order (used by both modes).
EXPECTED_COMMENTS = ["id_ed25519", "id_rsa", "id_ecdsa"]
EXPECTED_FORMATS = ["ssh-ed25519", "ssh-rsa", "ecdsa-sha2-nistp256"]


# --- config templates (block style; `{server}` filled, `{audit_log}` survives) -------

LOCAL_KEYS_CONFIG = """\
server: {server}
ssh:
  mode: local-keys
  approval:
    policy: approve-all
logging:
  enabled: true
  path: {audit_log}
"""

AGENT_FORWARD_CONFIG = """\
server: {server}
ssh:
  approval:
    policy: approve-all
logging:
  enabled: true
  path: {audit_log}
"""


def _local_config(bus_url: str) -> str:
    return LOCAL_KEYS_CONFIG.replace("{server}", bus_url)


def _agent_config(bus_url: str) -> str:
    return AGENT_FORWARD_CONFIG.replace("{server}", bus_url)


# --- request builders ----------------------------------------------------------------


def _req(req_id: str, payload: dict) -> dict:
    return {
        "id": req_id,
        "namespace": "ssh-agent",
        "type": "request",
        "final": True,
        "timestamp": "2026-07-10T00:00:00Z",
        "payload": payload,
        "shed": SHED,
    }


def _sign_req(req_id: str, pub_b64: str, flags: int) -> dict:
    return _req(
        req_id,
        {"operation": "sign", "public_key": pub_b64, "data": CHALLENGE_B64, "flags": flags},
    )


# --- signature verification helpers (verify-not-bytes) -------------------------------


def _rsa_hash_for_flags(flags: int):
    if flags & 2:
        return hashes.SHA256()
    if flags & 4:
        return hashes.SHA512()
    return hashes.SHA1()


def _verify_rsa(pub_line_stem: str, sig: bytes, flags: int) -> None:
    pub = load_ssh_public_key((FIXTURES_DIR / f"{pub_line_stem}.pub").read_bytes())
    pub.verify(sig, CHALLENGE, padding.PKCS1v15(), _rsa_hash_for_flags(flags))


def _ssh_read_string(buf: bytes, pos: int) -> tuple[bytes, int]:
    (n,) = struct.unpack_from(">I", buf, pos)
    pos += 4
    return buf[pos : pos + n], pos + n


def _verify_ecdsa(sig_blob: bytes) -> None:
    # The ecdsa ssh signature blob is mpint(r) ‖ mpint(s); decode + DER-encode for
    # cryptography's ECDSA verify (P-256 → SHA-256).
    r_bytes, pos = _ssh_read_string(sig_blob, 0)
    s_bytes, _ = _ssh_read_string(sig_blob, pos)
    r = int.from_bytes(r_bytes, "big")
    s = int.from_bytes(s_bytes, "big")
    der = utils.encode_dss_signature(r, s)
    pub = load_ssh_public_key((FIXTURES_DIR / "test_ecdsa.pub").read_bytes())
    pub.verify(der, CHALLENGE, ec.ECDSA(hashes.SHA256()))


# =====================================================================================
# local-keys mode
# =====================================================================================


@pytest.mark.differential
def test_ssh_local_list(daemon, differential):
    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _local_config(bus.url), install_ssh_keys=THREE_KEYS) as d:
                bus.wait_for_subscribe("ssh-agent", timeout=10.0)
                bus.push_request("ssh-agent", _req("list-1", {"operation": "list"}))
                resp = bus.await_response("ssh-agent", timeout=10.0)
                audit = d.read_audit_jsonl(expect=1, timeout=10.0)[0]
                return {
                    "response": canonical(mask_bus_response(resp)),
                    "audit": canonical(mask_audit_entry(audit)),
                }

    result = differential(scenario)
    resp = result["response"]
    assert resp["in_reply_to"] == "list-1"
    assert resp["shed"] == EXPECTED_SHED
    keys = resp["payload"]["keys"]
    assert [k["comment"] for k in keys] == EXPECTED_COMMENTS
    assert [k["format"] for k in keys] == EXPECTED_FORMATS
    # Each blob is the fixture's marshaled pubkey (b64), byte-equal across impls.
    assert keys[0]["blob"] == ED25519_PUB_B64
    assert keys[1]["blob"] == RSA_PUB_B64
    assert keys[2]["blob"] == ECDSA_PUB_B64
    # The durable non-gated list audit (positional Log form: approval "none", no outcome).
    assert result["audit"] == {
        "ts": "<ts>",
        "shed": "web",
        "ns": "ssh-agent",
        "op": "list",
        "result": "ok",
        "detail": "3 keys",
        "approval": "none",
    }


@pytest.mark.differential
@pytest.mark.parametrize(
    "flags,fmt",
    [(0, "ssh-rsa"), (2, "rsa-sha2-256"), (4, "rsa-sha2-512"), (6, "rsa-sha2-256")],
)
def test_ssh_local_sign_rsa(daemon, differential, flags, fmt):
    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _local_config(bus.url), install_ssh_keys=THREE_KEYS) as d:
                bus.wait_for_subscribe("ssh-agent", timeout=10.0)
                bus.push_request("ssh-agent", _sign_req("rsa-1", RSA_PUB_B64, flags))
                payload = mask_bus_response(bus.await_response("ssh-agent", timeout=10.0))["payload"]
                assert payload["format"] == fmt, f"{impl}: {payload}"
                assert payload["rest"] == ""
                # verify-not-bytes: the blob cryptographically verifies per-impl.
                _verify_rsa("test_rsa", base64.b64decode(payload["blob"]), flags)
                return {"format": payload["format"]}

    result = differential(scenario)
    assert result == {"format": fmt}


@pytest.mark.differential
def test_ssh_local_sign_ecdsa(daemon, differential):
    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _local_config(bus.url), install_ssh_keys=THREE_KEYS) as d:
                bus.wait_for_subscribe("ssh-agent", timeout=10.0)
                bus.push_request("ssh-agent", _sign_req("ec-1", ECDSA_PUB_B64, 0))
                payload = mask_bus_response(bus.await_response("ssh-agent", timeout=10.0))["payload"]
                assert payload["format"] == "ecdsa-sha2-nistp256", f"{impl}: {payload}"
                _verify_ecdsa(base64.b64decode(payload["blob"]))
                return {"format": payload["format"]}

    result = differential(scenario)
    assert result == {"format": "ecdsa-sha2-nistp256"}


@pytest.mark.differential
def test_ssh_local_status(daemon, differential):
    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _local_config(bus.url), install_ssh_keys=THREE_KEYS) as d:
                bus.wait_for_subscribe("ssh-agent", timeout=10.0)
                bus.push_request("ssh-agent", _req("st-1", {"operation": "status"}))
                resp = bus.await_response("ssh-agent", timeout=10.0)
                return canonical(mask_bus_response(resp)["payload"])

    result = differential(scenario)
    assert result == {"connected": True, "mode": "local-keys", "key_count": 3}


# =====================================================================================
# agent-forward mode (auto-detect via a per-impl fake ssh-agent)
# =====================================================================================


@pytest.mark.differential
def test_ssh_agent_list(daemon, differential):
    def scenario(impl):
        with FakeSshAgent() as agent:
            with SyntheticBus() as bus:
                with daemon(impl, _agent_config(bus.url), ssh_auth_sock=agent.path) as d:
                    bus.wait_for_subscribe("ssh-agent", timeout=10.0)
                    bus.push_request("ssh-agent", _req("list-1", {"operation": "list"}))
                    resp = bus.await_response("ssh-agent", timeout=10.0)
                    audit = d.read_audit_jsonl(expect=1, timeout=10.0)[0]
                    return {
                        "response": canonical(mask_bus_response(resp)),
                        "audit": canonical(mask_audit_entry(audit)),
                        "transcript": agent.transcript(),
                    }

    result = differential(scenario)
    keys = result["response"]["payload"]["keys"]
    assert [k["comment"] for k in keys] == EXPECTED_COMMENTS
    assert [k["format"] for k in keys] == EXPECTED_FORMATS
    assert keys[0]["blob"] == ED25519_PUB_B64
    assert result["audit"] == {
        "ts": "<ts>",
        "shed": "web",
        "ns": "ssh-agent",
        "op": "list",
        "result": "ok",
        "detail": "3 keys",
        "approval": "none",
    }
    # One wire request: REQUEST_IDENTITIES (the auto-detect probe sends no request).
    assert result["transcript"] == [{"type": 11}]


@pytest.mark.differential
def test_ssh_agent_sign_ed25519(daemon, differential):
    def scenario(impl):
        with FakeSshAgent() as agent:
            with SyntheticBus() as bus:
                with daemon(impl, _agent_config(bus.url), ssh_auth_sock=agent.path) as d:
                    bus.wait_for_subscribe("ssh-agent", timeout=10.0)
                    bus.push_request("ssh-agent", _sign_req("ed-1", ED25519_PUB_B64, 0))
                    resp = bus.await_response("ssh-agent", timeout=10.0)
                    return {
                        "response": canonical(mask_bus_response(resp)),
                        "transcript": agent.transcript(),
                    }

    result = differential(scenario)
    payload = result["response"]["payload"]
    # The fake real-signs ed25519 deterministically → the blob is byte-identical across
    # impls (compared UNMASKED via `differential`) and verifies against the pubkey.
    assert payload["format"] == "ssh-ed25519"
    assert payload["rest"] == ""
    blob = base64.b64decode(payload["blob"])
    assert len(blob) == 64
    pub = load_ssh_public_key((FIXTURES_DIR / "test_ed25519.pub").read_bytes())
    pub.verify(blob, CHALLENGE)
    # The wire request carried the keyblob/data verbatim, flags 0.
    assert result["transcript"] == [
        {
            "type": 13,
            "key_b64": base64.b64encode(base64.b64decode(ED25519_PUB_B64)).decode("ascii"),
            "data_b64": CHALLENGE_B64,
            "flags": 0,
        }
    ]


@pytest.mark.differential
def test_ssh_agent_sign_rsa_flags2(daemon, differential):
    def scenario(impl):
        with FakeSshAgent() as agent:
            with SyntheticBus() as bus:
                with daemon(impl, _agent_config(bus.url), ssh_auth_sock=agent.path) as d:
                    bus.wait_for_subscribe("ssh-agent", timeout=10.0)
                    bus.push_request("ssh-agent", _sign_req("rsa-1", RSA_PUB_B64, 2))
                    resp = bus.await_response("ssh-agent", timeout=10.0)
                    return {
                        "response": canonical(mask_bus_response(resp)),
                        "transcript": agent.transcript(),
                    }

    result = differential(scenario)
    payload = result["response"]["payload"]
    # The fake returns a canned blob (byte-equal both impls — proves passthrough, not a
    # real signature) with the flag-selected format.
    assert payload["format"] == "rsa-sha2-256"
    assert base64.b64decode(payload["blob"]) == CANNED_RSA_BLOB
    # flags==2 reached the wire verbatim.
    assert result["transcript"][0]["type"] == 13
    assert result["transcript"][0]["flags"] == 2


@pytest.mark.differential
def test_ssh_agent_status(daemon, differential):
    def scenario(impl):
        with FakeSshAgent() as agent:
            with SyntheticBus() as bus:
                with daemon(impl, _agent_config(bus.url), ssh_auth_sock=agent.path) as d:
                    bus.wait_for_subscribe("ssh-agent", timeout=10.0)
                    bus.push_request("ssh-agent", _req("st-1", {"operation": "status"}))
                    resp = bus.await_response("ssh-agent", timeout=10.0)
                    return {
                        "payload": canonical(mask_bus_response(resp)["payload"]),
                        "transcript": agent.transcript(),
                    }

    result = differential(scenario)
    assert result["payload"] == {"connected": True, "mode": "agent-forward", "key_count": 3}
    # status → the backend List → the extra REQUEST_IDENTITIES on the wire.
    assert result["transcript"] == [{"type": 11}]
