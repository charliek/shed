"""A fake ssh-agent on a UDS for the agent-forward cells.

Stands in for the host SSH agent ($SSH_AUTH_SOCK) that the host-agent's
**agent-forward** backend (`crates/shed-host-agent/ssh_backend_agent.rs`, the
hand-rolled client) proxies to, so the three messages it speaks are exercised against
a deterministic peer.

**Wire behavior mirrors the in-Rust unit fake** (`ssh_backend_agent.rs` tests) EXACTLY
— it serves only the three messages the backend uses:

* framing: `uint32-BE len ‖ payload`, `payload[0]` = message type;
* `SSH_AGENTC_REQUEST_IDENTITIES(11)` → `SSH_AGENT_IDENTITIES_ANSWER(12)`:
  `uint32 nkeys` then per key `string keyblob`, `string comment` — the identity set
  built from the committed fixtures (`test_ed25519`/`test_rsa`/`test_ecdsa`);
* `SSH_AGENTC_SIGN_REQUEST(13)` (`string keyblob`, `string data`, `uint32 flags`) →
  `SSH_AGENT_SIGN_RESPONSE(14)` `string sigblob` where `sigblob = string(format) ‖
  string(blob)`:
    - the **ed25519** identity → a REAL deterministic ed25519 signature (RFC 8032) over
      `data` (so the proxied 64 bytes are fixed → the blob is golden-pinned UNMASKED);
    - the **rsa** identity → a CANNED fixed blob (not a real signature — transcript-/
      byte-verified only), `format` selected from `flags` the way a real agent labels
      it: `flags&2` → `rsa-sha2-256`, else `flags&4` → `rsa-sha2-512`, else `ssh-rsa`;
    - the **ecdsa** identity → a CANNED fixed blob, `format` = `ecdsa-sha2-nistp256`;
    - an **unknown** keyblob → `SSH_AGENT_FAILURE(5)`;
* a **fresh connection per op** (the backend dials per `list`/`sign`); an empty
  connection (the auto-detect probe connects then closes without sending) is ignored.

Every served request is appended to a JSONL **transcript** (`{"type": N, "key_b64":
..., "data_b64": ..., "flags": N}` for sign, `{"type": 11}` for list) so a cell can
golden-pin the exact wire requests the daemon issued, in order (flags passthrough
included).
"""

from __future__ import annotations

import base64
import json
import socket
import struct
import tempfile
import threading
from pathlib import Path

from cryptography.hazmat.primitives.serialization import load_ssh_private_key

# ssh-agent protocol message types (draft-miller-ssh-agent §5.1).
SSH_AGENT_FAILURE = 5
SSH_AGENTC_REQUEST_IDENTITIES = 11
SSH_AGENT_IDENTITIES_ANSWER = 12
SSH_AGENTC_SIGN_REQUEST = 13
SSH_AGENT_SIGN_RESPONSE = 14

# Deterministic CANNED signature blobs for the rsa/ecdsa identities — fixed bytes the
# fake returns for every request, so the proxied blob is fixed (byte-compared).
# NOT real signatures (transcript-verified only), per the harness plan.
CANNED_RSA_BLOB = b"hadiff-canned-rsa-signature-blob-v1"
CANNED_ECDSA_BLOB = b"hadiff-canned-ecdsa-signature-blob-v1"

FIXTURES_DIR = Path(__file__).resolve().parent / "fixtures"


def _ssh_string(b: bytes) -> bytes:
    """RFC 4251 `string`: uint32-BE length prefix ‖ bytes."""
    return struct.pack(">I", len(b)) + b


class _Reader:
    """A minimal SSH-wire reader (RFC 4251 byte/uint32/string) over a bytes buffer."""

    def __init__(self, buf: bytes) -> None:
        self.buf = buf
        self.pos = 0

    def u8(self) -> int:
        b = self.buf[self.pos]
        self.pos += 1
        return b

    def u32(self) -> int:
        (v,) = struct.unpack_from(">I", self.buf, self.pos)
        self.pos += 4
        return v

    def string(self) -> bytes:
        n = self.u32()
        s = self.buf[self.pos : self.pos + n]
        self.pos += n
        return s


class _Identity:
    """One key the fake offers. `kind` selects the sign behavior; `blob` is the
    SSH-wire marshaled public key (matched byte-for-byte against a sign request's
    keyblob)."""

    def __init__(self, blob: bytes, comment: str, kind: str, ed25519_key=None) -> None:
        self.blob = blob
        self.comment = comment
        self.kind = kind  # "ed25519" | "rsa" | "ecdsa"
        self.ed25519_key = ed25519_key

    def sign(self, data: bytes, flags: int) -> tuple[str, bytes]:
        """Return `(format, blob)` for a sign of `data` under `flags`."""
        if self.kind == "ed25519":
            return "ssh-ed25519", self.ed25519_key.sign(data)
        if self.kind == "rsa":
            if flags & 2:
                fmt = "rsa-sha2-256"
            elif flags & 4:
                fmt = "rsa-sha2-512"
            else:
                fmt = "ssh-rsa"
            return fmt, CANNED_RSA_BLOB
        if self.kind == "ecdsa":
            return "ecdsa-sha2-nistp256", CANNED_ECDSA_BLOB
        raise AssertionError(f"unknown identity kind {self.kind!r}")


def default_identities() -> list[_Identity]:
    """The three committed fixtures as agent identities, in STANDARD_KEY_FILES order
    (ed25519, rsa, ecdsa). The ed25519 identity carries its private key for real
    deterministic signing; rsa/ecdsa serve canned blobs."""
    ed_blob = base64.b64decode((FIXTURES_DIR / "test_ed25519.pub").read_text().split()[1])
    rsa_blob = base64.b64decode((FIXTURES_DIR / "test_rsa.pub").read_text().split()[1])
    ec_blob = base64.b64decode((FIXTURES_DIR / "test_ecdsa.pub").read_text().split()[1])
    ed_key = load_ssh_private_key((FIXTURES_DIR / "test_ed25519").read_bytes(), password=None)
    return [
        _Identity(ed_blob, "id_ed25519", "ed25519", ed25519_key=ed_key),
        _Identity(rsa_blob, "id_rsa", "rsa"),
        _Identity(ec_blob, "id_ecdsa", "ecdsa"),
    ]


class FakeSshAgent:
    """A fake ssh-agent bound to a short UDS path. Use as a context manager; `.path`
    is the socket to point a daemon's `$SSH_AUTH_SOCK` at, `.transcript()` returns the
    parsed served-request records."""

    def __init__(self, identities: list[_Identity] | None = None) -> None:
        self.identities = identities if identities is not None else default_identities()
        # A SHORT dir: an AF_UNIX bind path caps at ~104 bytes (macOS) / ~108 (Linux).
        self._tmp = tempfile.mkdtemp(prefix="hadiff-ssha-")
        self.path = str(Path(self._tmp) / "agent.sock")
        self._transcript: list[dict] = []
        self._lock = threading.Lock()
        self._sock: socket.socket | None = None
        self._thread: threading.Thread | None = None
        self._stop = threading.Event()

    # -- lifecycle -----------------------------------------------------------

    def start(self) -> "FakeSshAgent":
        self._sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self._sock.bind(self.path)
        self._sock.listen(16)
        self._sock.settimeout(0.2)  # so the accept loop can observe _stop
        self._thread = threading.Thread(target=self._serve, name="fake-ssh-agent", daemon=True)
        self._thread.start()
        return self

    def stop(self) -> None:
        self._stop.set()
        # Nudge the accept loop out of its timeout wait.
        try:
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
                s.connect(self.path)
        except OSError:
            pass
        if self._thread is not None:
            self._thread.join(timeout=2.0)
            self._thread = None
        if self._sock is not None:
            self._sock.close()
            self._sock = None
        try:
            Path(self.path).unlink()
        except OSError:
            pass

    def __enter__(self) -> "FakeSshAgent":
        return self.start()

    def __exit__(self, *exc) -> None:
        self.stop()

    # -- test-facing ---------------------------------------------------------

    def transcript(self) -> list[dict]:
        """A snapshot of the served requests (order preserved). Empty probe
        connections (no request bytes) are not recorded."""
        with self._lock:
            return list(self._transcript)

    # -- internals -----------------------------------------------------------

    def _serve(self) -> None:
        while not self._stop.is_set():
            try:
                conn, _ = self._sock.accept()
            except socket.timeout:
                continue
            except OSError:
                break
            with conn:
                if self._stop.is_set():
                    break
                self._handle_conn(conn)

    def _handle_conn(self, conn: socket.socket) -> None:
        payload = _read_message(conn)
        if payload is None:
            return  # empty connection (auto-detect probe) — not a request.
        msg_type = payload[0]
        if msg_type == SSH_AGENTC_REQUEST_IDENTITIES:
            with self._lock:
                self._transcript.append({"type": msg_type})
            _write_message(conn, self._identities_answer())
        elif msg_type == SSH_AGENTC_SIGN_REQUEST:
            r = _Reader(payload)
            r.u8()  # type
            keyblob = r.string()
            data = r.string()
            flags = r.u32()
            with self._lock:
                self._transcript.append(
                    {
                        "type": msg_type,
                        "key_b64": base64.b64encode(keyblob).decode("ascii"),
                        "data_b64": base64.b64encode(data).decode("ascii"),
                        "flags": flags,
                    }
                )
            _write_message(conn, self._sign_response(keyblob, data, flags))
        else:
            _write_message(conn, bytes([SSH_AGENT_FAILURE]))

    def _identities_answer(self) -> bytes:
        out = bytes([SSH_AGENT_IDENTITIES_ANSWER]) + struct.pack(">I", len(self.identities))
        for idn in self.identities:
            out += _ssh_string(idn.blob) + _ssh_string(idn.comment.encode("utf-8"))
        return out

    def _sign_response(self, keyblob: bytes, data: bytes, flags: int) -> bytes:
        for idn in self.identities:
            if idn.blob == keyblob:
                fmt, blob = idn.sign(data, flags)
                sigblob = _ssh_string(fmt.encode("utf-8")) + _ssh_string(blob)
                return bytes([SSH_AGENT_SIGN_RESPONSE]) + _ssh_string(sigblob)
        return bytes([SSH_AGENT_FAILURE])  # unknown key


def _read_message(conn: socket.socket) -> bytes | None:
    """Read one framed message (uint32-BE len ‖ payload). Returns None on a clean
    empty connection (EOF before any bytes) — the auto-detect probe."""
    header = _read_exact(conn, 4)
    if header is None:
        return None
    (length,) = struct.unpack(">I", header)
    return _read_exact(conn, length)


def _read_exact(conn: socket.socket, n: int) -> bytes | None:
    buf = b""
    while len(buf) < n:
        try:
            chunk = conn.recv(n - len(buf))
        except OSError:
            return None
        if not chunk:
            return None
        buf += chunk
    return buf


def _write_message(conn: socket.socket, payload: bytes) -> None:
    try:
        conn.sendall(struct.pack(">I", len(payload)) + payload)
    except OSError:
        pass


# -- a tiny in-process client, for the fake-seam self-test --------------------


def client_request_identities(path: str) -> list[tuple[bytes, str]]:
    """Drive REQUEST_IDENTITIES against the fake and return `[(keyblob, comment)]`."""
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
        s.connect(path)
        _write_message(s, bytes([SSH_AGENTC_REQUEST_IDENTITIES]))
        reply = _read_message(s)
    assert reply is not None and reply[0] == SSH_AGENT_IDENTITIES_ANSWER
    r = _Reader(reply)
    r.u8()
    n = r.u32()
    out = []
    for _ in range(n):
        blob = r.string()
        comment = r.string().decode("utf-8")
        out.append((blob, comment))
    return out


def client_sign(path: str, keyblob: bytes, data: bytes, flags: int):
    """Drive SIGN_REQUEST against the fake. Returns `(format, blob)` on success or
    `None` if the fake replied FAILURE (unknown key)."""
    payload = (
        bytes([SSH_AGENTC_SIGN_REQUEST])
        + _ssh_string(keyblob)
        + _ssh_string(data)
        + struct.pack(">I", flags)
    )
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
        s.connect(path)
        _write_message(s, payload)
        reply = _read_message(s)
    assert reply is not None
    if reply[0] == SSH_AGENT_FAILURE:
        return None
    assert reply[0] == SSH_AGENT_SIGN_RESPONSE
    r = _Reader(reply)
    r.u8()
    sigblob = r.string()
    sr = _Reader(sigblob)
    fmt = sr.string().decode("utf-8")
    blob = sr.string()
    return fmt, blob
