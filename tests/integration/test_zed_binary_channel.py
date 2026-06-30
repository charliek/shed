"""Binary-fidelity tests for the non-PTY SSH exec channel (issue #222).

Zed Remote-SSH runs a long-running proxy over a *non-PTY* exec channel and
uses its stdin/stdout as a raw, length-prefixed binary pipe. A regression in
the agent's stdin forwarding (`cmd/shed-agent/exec.go`) once desynchronized
that framed protocol — a 500ms `SetReadDeadline` poll could fire mid-frame,
consume bytes that `ReadMessage` then discarded, and permanently offset the
stream — so Zed connections failed with "stdin read failed: unexpected end of
file". VS Code was unaffected because its primary data channel uses
`direct-tcpip` raw `io.Copy`, not the framed agent protocol.

These tests pump binary data through `ssh <shed> -- cat` (no PTY) and assert a
byte-perfect round-trip, exercising the real wire path host framing → vsock →
agent pump → process and back.

Scope note: the **deterministic** mid-frame-desync reproducer is the Go unit
test `TestPumpClientMessages` (cmd/shed-agent/exec_test.go), which can force a
read deadline mid-frame. These live tests are the complementary fidelity guard:
they confirm the real non-PTY binary channel carries data intact and stays in
sync end-to-end. Parameterized over `["vz", "fc"]` like the rest of the suite.
"""

from __future__ import annotations

import random


def test_binary_roundtrip_fidelity(shed_server, test_shed_name):
    """4 MB of binary data piped through `cat` returns byte-identical.

    The agent reads the SSH stdin stream as framed `MsgTypeData` messages and
    writes them to the process; `cat` echoes them back. Several MB streamed
    continuously crosses ~1000 frames — if the agent's framed reader lost or
    misaligned bytes, the echo would not match.
    """
    shed_server.create(test_shed_name, image="base")

    # Deterministic (seeded) random payload so a failure is reproducible. Random
    # bytes include every value 0x00..0xFF, so control bytes and high-bit bytes
    # all traverse the 8-bit-clean channel.
    payload = random.Random(0xC0FFEE).randbytes(4 * 1024 * 1024)

    r = shed_server.ssh_exec_binary(test_shed_name, "cat", input=payload, timeout=120)
    assert r.returncode == 0, (
        f"ssh -- cat exited {r.returncode}; stderr={r.stderr[:200]!r}"
    )
    assert len(r.stdout) == len(payload), (
        f"length mismatch: got {len(r.stdout)} bytes, sent {len(payload)} — "
        f"the binary stream was truncated or desynchronized"
    )
    assert r.stdout == payload, (
        "binary round-trip corrupted: the echoed bytes differ from what was "
        "sent, indicating the agent's framed stdin reader lost or misaligned "
        "bytes (issue #222 regression)"
    )


def test_binary_roundtrip_all_byte_values(shed_server, test_shed_name):
    """A payload covering every byte value 0x00..0xFF round-trips intact.

    Explicitly includes the protocol's own control-frame type bytes (0x05
    `MsgTypeData`, 0x06 `MsgTypeStdinEOF`) and NUL as ordinary payload data, to
    prove the channel is a transparent 8-bit pipe and never reinterprets
    payload bytes as framing.
    """
    shed_server.create(test_shed_name, image="base")

    # Tile 0x00..0xFF so the payload is exhaustive over byte values and large
    # enough to span many frames.
    payload = bytes(range(256)) * 4096  # 1 MiB

    r = shed_server.ssh_exec_binary(test_shed_name, "cat", input=payload, timeout=60)
    assert r.returncode == 0, (
        f"ssh -- cat exited {r.returncode}; stderr={r.stderr[:200]!r}"
    )
    assert r.stdout == payload, (
        f"binary round-trip corrupted across the full byte range: "
        f"got {len(r.stdout)} bytes, sent {len(payload)}"
    )
