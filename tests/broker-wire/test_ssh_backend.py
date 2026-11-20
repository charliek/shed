"""The SSH backend — surface-B `list`/`sign`/`status` in BOTH backend modes,
golden-pinned.

Two modes:

* **local-keys** (`ssh.mode: local-keys`, `SSH_AUTH_SOCK` stripped, the committed
  `id_ed25519`+`id_rsa`+`id_ecdsa` fixtures installed in `$HOME/.ssh`): the daemon
  signs with the on-disk keys. `list` is masked-canonical-pinned + its durable audit
  line pinned; `sign` verifies the produced RSA/ECDSA blob against the fixture pubkey
  (verify-not-bytes, per the harness plan) and pins the `format`; `status` is pinned.
* **agent-forward** (auto-detect: no `ssh.mode`, `SSH_AUTH_SOCK` → a
  `fake_ssh_agent.py`): the broker proxies to the fake SSH agent. `list` is pinned
  (3 identities); `sign` ed25519 → the fake's REAL deterministic signature, so the blob
  is pinned UNMASKED; `sign` rsa flags=2 → the fake's canned blob byte-equal + the
  recorded `flags==2` (wire passthrough); `status` is pinned. The **fake's transcript**
  (msg type / keyblob / data / flags) is pinned too, so the exact wire requests the
  daemon issues are frozen.

**The `ssh.offer_keys` key filter** (plan 035 §3.5) has its own cells in each mode:
a filtered `list` (the ed25519 key named by fingerprint, the ECDSA key by public key
line, the RSA key left out) and a `sign` with the RSA key, which the daemon holds but
does not offer — the signing check declines it before the approval gate (audited
`refused`, `key_not_offered`), the shed gets exactly what an unknown key gets, and in
agent-forward mode the agent is never asked (an empty transcript) — and a `status` whose
guest-visible `key_count` is the offered count. The unfiltered cells above are
unchanged by the filter.

**Every `sign` sends real data** (plan 035 §3.5): an exact login request for `git` with
the key being asked, its algorithm the one the request's flags name
(`sign_payloads.py`). Arbitrary bytes are declined before any prompt now; the declines
have their own cells in `test_bus_sign_refused.py`.

**Gate policy — `approve-all`, deliberately.** These cells prove the *backend signing
and wire behavior*, not the approval gate — the gated (shed-desktop delegate →
approve/deny) `sign` path is already covered by `test_bus_sign_gated.py`
(with a deterministic ed25519 blob pinned unmasked). Using `approve-all` here avoids
a scripted desktop consumer per cell (and the flakiness that adds) while still driving
the full decode → signing check → gate → backend → response + audit flow. The
`list`/`status` ops are ungated regardless.
"""

from __future__ import annotations

import base64
import hashlib
import struct
from pathlib import Path

import pytest
from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.asymmetric import ec, padding, utils
from cryptography.hazmat.primitives.serialization import load_ssh_public_key

from fake_ssh_agent import CANNED_RSA_BLOB, FakeSshAgent
from normalize import canonical, mask_audit_entry, mask_bus_response
from sign_payloads import b64, login_request
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

# The algorithm a login names for each RSA flags value (the check binds them: flags 0
# is `ssh-rsa`, 2 is `rsa-sha2-256`, 4 is `rsa-sha2-512`).
RSA_ALGORITHM_FOR_FLAGS = {0: b"ssh-rsa", 2: b"rsa-sha2-256", 4: b"rsa-sha2-512"}


def _login(pub_b64: str, algorithm: bytes) -> bytes:
    """The fixed data every sign cell sends: an exact login for `git` with this key
    (fixed session id, so an ed25519 signature over it is deterministic)."""
    return login_request(b"git", pub_b64, algorithm)


ED25519_LOGIN = _login(ED25519_PUB_B64, b"ssh-ed25519")
ECDSA_LOGIN = _login(ECDSA_PUB_B64, b"ecdsa-sha2-nistp256")

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


# `ssh.offer_keys` (plan 035 §3.5): the ed25519 key by its fingerprint (as
# `ssh-keygen -lf test_ed25519.pub` prints it) and the ECDSA key by its public key
# line, so both entry forms are on the wire path. The RSA key is NOT offered.
ED25519_FINGERPRINT = "SHA256:QSbP+oRmKHQkEp2CsJn72x/5Qxyq5lsK6sib0fK1mr0"
ECDSA_PUB_LINE = (FIXTURES_DIR / "test_ecdsa.pub").read_text().strip()
OFFER_KEYS_BLOCK = (
    "  offer_keys:\n"
    f'    - "{ED25519_FINGERPRINT}"\n'
    f'    - "{ECDSA_PUB_LINE}"\n'
)


def _with_offer_keys(config: str) -> str:
    # Insert the filter under `ssh:` (both templates open the block with `ssh:\n`).
    return config.replace("ssh:\n", "ssh:\n" + OFFER_KEYS_BLOCK, 1)


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


def _sign_req(req_id: str, pub_b64: str, flags: int, data: bytes) -> dict:
    return _req(
        req_id,
        {"operation": "sign", "public_key": pub_b64, "data": b64(data), "flags": flags},
    )


def _rsa_sign_req(req_id: str, flags: int) -> dict:
    """A sign with the RSA key: a `git` login naming the algorithm `flags` selects."""
    return _sign_req(req_id, RSA_PUB_B64, flags, _rsa_login(flags))


def _rsa_login(flags: int) -> bytes:
    return _login(RSA_PUB_B64, RSA_ALGORITHM_FOR_FLAGS[flags])


# --- signature verification helpers (verify-not-bytes) -------------------------------


def _rsa_hash_for_flags(flags: int):
    if flags & 2:
        return hashes.SHA256()
    if flags & 4:
        return hashes.SHA512()
    return hashes.SHA1()


def _verify_rsa(pub_line_stem: str, sig: bytes, flags: int, data: bytes) -> None:
    pub = load_ssh_public_key((FIXTURES_DIR / f"{pub_line_stem}.pub").read_bytes())
    pub.verify(sig, data, padding.PKCS1v15(), _rsa_hash_for_flags(flags))


def _ssh_read_string(buf: bytes, pos: int) -> tuple[bytes, int]:
    (n,) = struct.unpack_from(">I", buf, pos)
    pos += 4
    return buf[pos : pos + n], pos + n


def _verify_ecdsa(sig_blob: bytes, data: bytes) -> None:
    # The ecdsa ssh signature blob is mpint(r) ‖ mpint(s); decode + DER-encode for
    # cryptography's ECDSA verify (P-256 → SHA-256).
    r_bytes, pos = _ssh_read_string(sig_blob, 0)
    s_bytes, _ = _ssh_read_string(sig_blob, pos)
    r = int.from_bytes(r_bytes, "big")
    s = int.from_bytes(s_bytes, "big")
    der = utils.encode_dss_signature(r, s)
    pub = load_ssh_public_key((FIXTURES_DIR / "test_ecdsa.pub").read_bytes())
    pub.verify(der, data, ec.ECDSA(hashes.SHA256()))


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
    # Each blob is the fixture's marshaled pubkey (b64), byte-pinned.
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
# Flags 6 (both hash bits) used to sign as rsa-sha2-256; no payload agrees with both
# bits, so the signing check now declines it (`test_bus_sign_refused.py`).
@pytest.mark.parametrize(
    "flags,fmt",
    [(0, "ssh-rsa"), (2, "rsa-sha2-256"), (4, "rsa-sha2-512")],
)
def test_ssh_local_sign_rsa(daemon, differential, flags, fmt):
    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _local_config(bus.url), install_ssh_keys=THREE_KEYS) as d:
                bus.wait_for_subscribe("ssh-agent", timeout=10.0)
                bus.push_request("ssh-agent", _rsa_sign_req("rsa-1", flags))
                payload = mask_bus_response(bus.await_response("ssh-agent", timeout=10.0))["payload"]
                assert payload["format"] == fmt, f"{impl}: {payload}"
                assert payload["rest"] == ""
                # verify-not-bytes: the blob cryptographically verifies.
                _verify_rsa(
                    "test_rsa", base64.b64decode(payload["blob"]), flags, _rsa_login(flags)
                )
                return {"format": payload["format"]}

    result = differential(scenario)
    assert result == {"format": fmt}


@pytest.mark.differential
def test_ssh_local_sign_ecdsa(daemon, differential):
    def scenario(impl):
        with SyntheticBus() as bus:
            with daemon(impl, _local_config(bus.url), install_ssh_keys=THREE_KEYS) as d:
                bus.wait_for_subscribe("ssh-agent", timeout=10.0)
                bus.push_request("ssh-agent", _sign_req("ec-1", ECDSA_PUB_B64, 0, ECDSA_LOGIN))
                payload = mask_bus_response(bus.await_response("ssh-agent", timeout=10.0))["payload"]
                assert payload["format"] == "ecdsa-sha2-nistp256", f"{impl}: {payload}"
                _verify_ecdsa(base64.b64decode(payload["blob"]), ECDSA_LOGIN)
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


# --- the ssh.offer_keys key filter (plan 035 §3.5) ------------------------------------

# A sign the backend cannot perform (an unknown key — or, with a filter, a key that is
# not offered: the two are indistinguishable to a shed, by design).
SIGN_FAILED = {"error": "sign operation failed", "code": "SIGN_FAILED"}

# `ssh-keygen -lf test_rsa.pub`: the key the filter leaves out, as the refusal names it.
RSA_FINGERPRINT = "SHA256:" + base64.b64encode(
    hashlib.sha256(base64.b64decode(RSA_PUB_B64)).digest()
).decode("ascii").rstrip("=")

# The audit line of a sign with a key that is not offered: declined by the signing
# check before the approval gate (plan 035 §3.5), so `approval` is `none`.
NOT_OFFERED_AUDIT = {
    "ts": "<ts>",
    "shed": "web",
    "ns": "ssh-agent",
    "op": "sign",
    "result": "refused",
    "detail": f"key {RSA_FINGERPRINT}",
    "code": "SIGN_REFUSED",
    "reason": "key_not_offered",
    "approval": "none",
}


@pytest.mark.differential
def test_ssh_local_list_filtered(daemon, differential):
    def scenario(impl):
        with SyntheticBus() as bus:
            config = _with_offer_keys(_local_config(bus.url))
            with daemon(impl, config, install_ssh_keys=THREE_KEYS) as d:
                bus.wait_for_subscribe("ssh-agent", timeout=10.0)
                bus.push_request("ssh-agent", _req("list-1", {"operation": "list"}))
                resp = bus.await_response("ssh-agent", timeout=10.0)
                audit = d.read_audit_jsonl(expect=1, timeout=10.0)[0]
                return {
                    "response": canonical(mask_bus_response(resp)),
                    "audit": canonical(mask_audit_entry(audit)),
                }

    result = differential(scenario)
    keys = result["response"]["payload"]["keys"]
    # Only the two offered keys, in load order; the RSA key is not listed.
    assert [k["comment"] for k in keys] == ["id_ed25519", "id_ecdsa"]
    assert [k["blob"] for k in keys] == [ED25519_PUB_B64, ECDSA_PUB_B64]
    assert result["audit"]["result"] == "ok"
    assert result["audit"]["detail"] == "2 keys"


@pytest.mark.differential
def test_ssh_local_sign_unoffered_key(daemon, differential):
    def scenario(impl):
        with SyntheticBus() as bus:
            config = _with_offer_keys(_local_config(bus.url))
            with daemon(impl, config, install_ssh_keys=THREE_KEYS) as d:
                bus.wait_for_subscribe("ssh-agent", timeout=10.0)
                # The RSA key is on disk and loaded, but not offered.
                bus.push_request("ssh-agent", _rsa_sign_req("rsa-1", 2))
                resp = bus.await_response("ssh-agent", timeout=10.0)
                audit = d.read_audit_jsonl(expect=1, timeout=10.0)[0]
                return {
                    "response": canonical(mask_bus_response(resp)),
                    "audit": canonical(mask_audit_entry(audit)),
                }

    result = differential(scenario)
    assert result["response"]["in_reply_to"] == "rsa-1"
    assert result["response"]["payload"] == SIGN_FAILED
    assert result["audit"] == NOT_OFFERED_AUDIT


@pytest.mark.differential
def test_ssh_local_status_filtered(daemon, differential):
    def scenario(impl):
        with SyntheticBus() as bus:
            config = _with_offer_keys(_local_config(bus.url))
            with daemon(impl, config, install_ssh_keys=THREE_KEYS) as d:
                bus.wait_for_subscribe("ssh-agent", timeout=10.0)
                bus.push_request("ssh-agent", _req("st-1", {"operation": "status"}))
                resp = bus.await_response("ssh-agent", timeout=10.0)
                return canonical(mask_bus_response(resp)["payload"])

    result = differential(scenario)
    # The guest-visible count is the offered keys, not the three on disk.
    assert result == {"connected": True, "mode": "local-keys", "key_count": 2}


# =====================================================================================
# agent-forward mode (auto-detect via a fake ssh-agent)
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
                    bus.push_request(
                        "ssh-agent", _sign_req("ed-1", ED25519_PUB_B64, 0, ED25519_LOGIN)
                    )
                    resp = bus.await_response("ssh-agent", timeout=10.0)
                    return {
                        "response": canonical(mask_bus_response(resp)),
                        "transcript": agent.transcript(),
                    }

    result = differential(scenario)
    payload = result["response"]["payload"]
    # The fake real-signs ed25519 deterministically → the blob is byte-identical across
    # runs (pinned UNMASKED in the golden) and verifies against the pubkey.
    assert payload["format"] == "ssh-ed25519"
    assert payload["rest"] == ""
    blob = base64.b64decode(payload["blob"])
    assert len(blob) == 64
    pub = load_ssh_public_key((FIXTURES_DIR / "test_ed25519.pub").read_bytes())
    pub.verify(blob, ED25519_LOGIN)
    # The wire request carried the keyblob/data verbatim (the very bytes checked), flags 0.
    assert result["transcript"] == [
        {
            "type": 13,
            "key_b64": base64.b64encode(base64.b64decode(ED25519_PUB_B64)).decode("ascii"),
            "data_b64": b64(ED25519_LOGIN),
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
                    bus.push_request("ssh-agent", _rsa_sign_req("rsa-1", 2))
                    resp = bus.await_response("ssh-agent", timeout=10.0)
                    return {
                        "response": canonical(mask_bus_response(resp)),
                        "transcript": agent.transcript(),
                    }

    result = differential(scenario)
    payload = result["response"]["payload"]
    # The fake returns a canned blob (byte-pinned — proves passthrough, not a
    # real signature) with the flag-selected format.
    assert payload["format"] == "rsa-sha2-256"
    assert base64.b64decode(payload["blob"]) == CANNED_RSA_BLOB
    # flags==2 and the checked login reached the wire verbatim.
    assert result["transcript"][0]["type"] == 13
    assert result["transcript"][0]["flags"] == 2
    assert result["transcript"][0]["data_b64"] == b64(_rsa_login(2))


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


@pytest.mark.differential
def test_ssh_agent_list_filtered(daemon, differential):
    def scenario(impl):
        with FakeSshAgent() as agent:
            with SyntheticBus() as bus:
                config = _with_offer_keys(_agent_config(bus.url))
                with daemon(impl, config, ssh_auth_sock=agent.path) as d:
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
    assert [k["comment"] for k in keys] == ["id_ed25519", "id_ecdsa"]
    assert [k["blob"] for k in keys] == [ED25519_PUB_B64, ECDSA_PUB_B64]
    assert result["audit"]["detail"] == "2 keys"
    # Still exactly one wire request: the filter asks the agent nothing at start-up
    # and lists through the same single REQUEST_IDENTITIES.
    assert result["transcript"] == [{"type": 11}]


@pytest.mark.differential
def test_ssh_agent_sign_unoffered_key(daemon, differential):
    def scenario(impl):
        with FakeSshAgent() as agent:
            with SyntheticBus() as bus:
                config = _with_offer_keys(_agent_config(bus.url))
                with daemon(impl, config, ssh_auth_sock=agent.path) as d:
                    bus.wait_for_subscribe("ssh-agent", timeout=10.0)
                    bus.push_request("ssh-agent", _rsa_sign_req("rsa-1", 2))
                    resp = bus.await_response("ssh-agent", timeout=10.0)
                    audit = d.read_audit_jsonl(expect=1, timeout=10.0)[0]
                    return {
                        "response": canonical(mask_bus_response(resp)),
                        "audit": canonical(mask_audit_entry(audit)),
                        "transcript": agent.transcript(),
                    }

    result = differential(scenario)
    assert result["response"]["payload"] == SIGN_FAILED
    assert result["audit"] == NOT_OFFERED_AUDIT
    # The agent holds the RSA key, but the request never reached it.
    assert result["transcript"] == []


@pytest.mark.differential
def test_ssh_agent_status_filtered(daemon, differential):
    def scenario(impl):
        with FakeSshAgent() as agent:
            with SyntheticBus() as bus:
                config = _with_offer_keys(_agent_config(bus.url))
                with daemon(impl, config, ssh_auth_sock=agent.path) as d:
                    bus.wait_for_subscribe("ssh-agent", timeout=10.0)
                    bus.push_request("ssh-agent", _req("st-1", {"operation": "status"}))
                    resp = bus.await_response("ssh-agent", timeout=10.0)
                    return {
                        "payload": canonical(mask_bus_response(resp)["payload"]),
                        "transcript": agent.transcript(),
                    }

    result = differential(scenario)
    # The guest-visible count is the offered keys, not the three the agent holds.
    assert result["payload"] == {"connected": True, "mode": "agent-forward", "key_count": 2}
    assert result["transcript"] == [{"type": 11}]
