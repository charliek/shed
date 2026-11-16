"""The data a shed asks this machine to sign, built byte by byte (plan 035 §3.5).

The daemon signs only two shapes, and refuses everything else before any approval
prompt (`crates/shed-broker/src/sign_check.rs`):

* an SSH public-key **login request** (RFC 4252 §7): `string` session id, `byte` 50,
  `string` username, `string "ssh-connection"`, `string "publickey"`, `boolean` true,
  `string` algorithm, `string` key blob — for a username on `ssh.allow_users`
  (default `[git]`), naming the very key being asked to sign, nothing trailing;
* an **SSHSIG** signed-data blob (OpenSSH `PROTOCOL.sshsig`): `"SSHSIG"`, `string`
  namespace (only `git`), `string` reserved (empty), `string` hash algorithm,
  `string` H(message).

Every value here is FIXED (a constant session id, a constant message), so an ed25519
signature over it is deterministic and can be pinned byte for byte.
"""

from __future__ import annotations

import base64
import hashlib
import struct

# A fixed 32-byte (SHA-256 sized) session id, so the signed bytes never change.
SESSION_ID = bytes(range(32))

# The message an SSHSIG blob's hash covers (git signs the commit object; any fixed
# bytes do here).
SSHSIG_MESSAGE = b"shed-host-agent differential commit v1"


def ssh_string(b: bytes) -> bytes:
    """RFC 4251 `string`: a big-endian uint32 length, then the bytes."""
    return struct.pack(">I", len(b)) + b


def login_request(user: bytes, key_blob_b64: str, algorithm: bytes) -> bytes:
    """An exact `publickey` login request for `user` with the key whose SSH-wire blob is
    `key_blob_b64`, naming `algorithm` (the key's type, or for RSA the hash the agent
    request's flags must name: `ssh-rsa` = flags 0, `rsa-sha2-256` = 2,
    `rsa-sha2-512` = 4)."""
    return (
        ssh_string(SESSION_ID)
        + bytes([50])  # SSH_MSG_USERAUTH_REQUEST
        + ssh_string(user)
        + ssh_string(b"ssh-connection")
        + ssh_string(b"publickey")
        + bytes([1])  # a signature follows
        + ssh_string(algorithm)
        + ssh_string(base64.b64decode(key_blob_b64))
    )


def sshsig(namespace: bytes) -> bytes:
    """An exact SSHSIG signed-data blob for `namespace` over `SSHSIG_MESSAGE` (sha512,
    as `ssh-keygen -Y sign` and git use)."""
    return (
        b"SSHSIG"
        + ssh_string(namespace)
        + ssh_string(b"")
        + ssh_string(b"sha512")
        + ssh_string(hashlib.sha512(SSHSIG_MESSAGE).digest())
    )


def b64(data: bytes) -> str:
    return base64.b64encode(data).decode("ascii")
