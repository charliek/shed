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


def test_binary_stdout_survives_stderr(shed_server, test_shed_name):
    """Binary stdout stays byte-perfect when the command also writes to stderr.

    This is the Zed Remote-SSH failure shape: the remote process speaks a binary
    protocol on stdout (Zed's length-prefixed protobuf) and also emits diagnostic
    text on stderr (the zed-remote-server's JSON logs). The agent used to FOLD
    stderr into stdout, injecting those log bytes into the binary stream and
    desyncing Zed's framing (readiness timed out). With stderr on its own
    MsgTypeStderr frame, stdout must be byte-identical to the input and the stderr
    text must arrive on the separate stderr channel. (The command writes stderr
    before and after the `cat`, not strictly interleaved with stdout, which is
    still sufficient to catch the fold — any folded byte corrupts stdout.)

    Like test_exec_stderr.py, this asserts the NEW baked agent: run against a
    rootfs rebuilt with the stderr-separation change, not a bare
    `make test-integration-dev` (which restarts only the dev server).
    """
    shed_server.create(test_shed_name, image="base")

    # Every byte value (incl. '{' 0x7b, the byte Zed misreads as a length) so a
    # single folded stderr byte would both corrupt and lengthen stdout.
    payload = bytes(range(256)) * 4096  # 1 MiB
    # Emit stderr diagnostics around the binary echo; the old folding agent would
    # have spliced these bytes into stdout.
    cmd = "printf 'log-before\\n' >&2; cat; printf 'log-after\\n' >&2"

    r = shed_server.ssh_exec_binary(test_shed_name, cmd, input=payload, timeout=60)
    assert r.returncode == 0, f"exit={r.returncode} stderr={r.stderr[:200]!r}"
    assert r.stdout == payload, (
        f"binary stdout corrupted by concurrent stderr (fold regression): "
        f"got {len(r.stdout)} bytes, sent {len(payload)}"
    )
    assert b"log-before" in r.stderr and b"log-after" in r.stderr, (
        f"stderr diagnostics missing from the stderr channel: {r.stderr[:200]!r}"
    )
    # (No need to also assert the log markers are absent from stdout: the exact
    # `stdout == payload` check above already proves stdout is byte-identical to
    # the monotonic-mod-256 input, which cannot contain those substrings.)
