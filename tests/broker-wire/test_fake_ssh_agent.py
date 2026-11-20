"""Fake-seam self-test (slice discipline): drive `fake_ssh_agent.py` with a tiny
in-process client so a fake bug is never misread as a daemon diff.

Asserts the fake serves the three committed identities, real-signs the ed25519
identity (verifiable against its pubkey), serves canned blobs for rsa/ecdsa with the
flag-selected format, FAILs an unknown key, and records a transcript.
"""

from __future__ import annotations

import base64
from pathlib import Path

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives.serialization import load_ssh_public_key

from fake_ssh_agent import (
    CANNED_ECDSA_BLOB,
    CANNED_RSA_BLOB,
    FakeSshAgent,
    client_request_identities,
    client_sign,
)

FIXTURES_DIR = Path(__file__).resolve().parent / "fixtures"


def _pub_blob(stem: str) -> bytes:
    return base64.b64decode((FIXTURES_DIR / f"{stem}.pub").read_text().split()[1])


def test_fake_lists_three_identities():
    with FakeSshAgent() as agent:
        ids = client_request_identities(agent.path)
    assert [c for _, c in ids] == ["id_ed25519", "id_rsa", "id_ecdsa"]
    assert [b for b, _ in ids] == [
        _pub_blob("test_ed25519"),
        _pub_blob("test_rsa"),
        _pub_blob("test_ecdsa"),
    ]


def test_fake_ed25519_sign_verifies():
    ed_blob = _pub_blob("test_ed25519")
    data = b"self-test challenge for the fake agent"
    with FakeSshAgent() as agent:
        fmt, blob = client_sign(agent.path, ed_blob, data, flags=0)
    assert fmt == "ssh-ed25519"
    assert len(blob) == 64
    pub = load_ssh_public_key((FIXTURES_DIR / "test_ed25519.pub").read_bytes())
    pub.verify(blob, data)  # raises on a bad signature


def test_fake_rsa_canned_format_by_flags():
    rsa_blob = _pub_blob("test_rsa")
    with FakeSshAgent() as agent:
        assert client_sign(agent.path, rsa_blob, b"d", flags=0) == ("ssh-rsa", CANNED_RSA_BLOB)
        assert client_sign(agent.path, rsa_blob, b"d", flags=2) == ("rsa-sha2-256", CANNED_RSA_BLOB)
        assert client_sign(agent.path, rsa_blob, b"d", flags=4) == ("rsa-sha2-512", CANNED_RSA_BLOB)
        # bit-2 priority: flags=6 → sha256 (the same rule the local-keys backend uses).
        assert client_sign(agent.path, rsa_blob, b"d", flags=6) == ("rsa-sha2-256", CANNED_RSA_BLOB)


def test_fake_ecdsa_canned():
    ec_blob = _pub_blob("test_ecdsa")
    with FakeSshAgent() as agent:
        fmt, blob = client_sign(agent.path, ec_blob, b"d", flags=0)
    assert fmt == "ecdsa-sha2-nistp256"
    assert blob == CANNED_ECDSA_BLOB


def test_fake_unknown_key_fails():
    with FakeSshAgent() as agent:
        assert client_sign(agent.path, b"\x00\x00\x00\x04junk", b"d", flags=0) is None


def test_fake_records_transcript():
    ed_blob = _pub_blob("test_ed25519")
    with FakeSshAgent() as agent:
        client_request_identities(agent.path)
        client_sign(agent.path, ed_blob, b"challenge", flags=2)
        transcript = agent.transcript()
    assert transcript[0] == {"type": 11}
    sign = transcript[1]
    assert sign["type"] == 13
    assert base64.b64decode(sign["key_b64"]) == ed_blob
    assert base64.b64decode(sign["data_b64"]) == b"challenge"
    assert sign["flags"] == 2


def test_fake_ed25519_wrong_data_fails_verify():
    """Negative control: a signature over different data must NOT verify (so a passing
    verify in the other tests is meaningful, not vacuous)."""
    ed_blob = _pub_blob("test_ed25519")
    with FakeSshAgent() as agent:
        _, blob = client_sign(agent.path, ed_blob, b"data-A", flags=0)
    pub = load_ssh_public_key((FIXTURES_DIR / "test_ed25519.pub").read_bytes())
    try:
        pub.verify(blob, b"data-B")
        raise AssertionError("signature over data-A must not verify against data-B")
    except InvalidSignature:
        pass
