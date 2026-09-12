"""The Python fake roost-session, checked on its own wire.

`fake_roost.FakeRoost` is the Python twin of
`shed_core::roost::testing::FakeRoost` (`crates/shed-core/src/roost/testing.rs`),
and the two drifting apart is the failure mode this file exists to prevent.

**Why it cannot be left to `test_tauri_machines.py`.** That suite drives the fake
through the real app, and the app is an *observer*: it identifies, subscribes,
lists, and folds batches. It never asks for a filtered subscribe and never
drives two connections against `session.set_agent_hooks` concurrently — the
"open to every connection, last writer wins" behaviour that generation 5
actually changed about that op, and the one the Rust fake's own unit tests pin
at length. So a Python port that got that shape subtly wrong would pass every
other test in the harness.

So these cells speak the wire directly: raw newline-delimited JSON frames on the
fake's socket, no app, no Tauri, no mock server. Each one has a named counterpart
in `testing.rs`'s `mod tests`; when you change one fake, change the other and
update BOTH sets.
"""

from __future__ import annotations

import base64
import json
import socket
import time

import pytest

from fake_roost import (
    FRAME_CAPACITY,
    STREAM_WRITE_DEADLINE,
    FakeRoost,
    RoostWireError,
    roost_call,
)


def _b64(text: str) -> str:
    return base64.b64encode(text.encode()).decode()


class _Wire:
    """One connection that STAYS OPEN, unlike `roost_call`'s dial-per-request.

    Needed by anything that drives more than one request on the same socket —
    a subscribe followed by reads off the stream, or two connections whose
    ordering with respect to each other matters.
    """

    def __init__(self, path, timeout: float = 10.0):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(timeout)
        self.sock.connect(str(path))
        self._buf = b""
        self._seq = 0

    def __enter__(self) -> "_Wire":
        return self

    def __exit__(self, *_exc) -> None:
        self.close()

    def close(self) -> None:
        self.sock.close()

    def send(self, op: str, params: dict | None = None) -> str:
        self._seq += 1
        request_id = str(self._seq)
        body = {"id": request_id, "op": op, "params": params or {}}
        self.sock.sendall((json.dumps(body) + "\n").encode())
        return request_id

    def readline(self) -> dict | None:
        """The next frame, or None at EOF."""
        while b"\n" not in self._buf:
            chunk = self.sock.recv(1 << 16)
            if not chunk:
                return None
            self._buf += chunk
        line, self._buf = self._buf.split(b"\n", 1)
        return json.loads(line.decode())

    def call(self, op: str, params: dict | None = None) -> dict:
        request_id = self.send(op, params)
        reply = self.readline()
        if reply is None:
            raise RoostWireError("disconnected", "socket closed mid-response")
        assert reply.get("id") == request_id, f"a reply for someone else: {reply}"
        if not reply.get("ok"):
            err = reply.get("error") or {}
            raise RoostWireError(err.get("code", "unknown"), err.get("message", ""))
        return reply.get("result") or {}

    def subscribe(self) -> dict:
        """`events.subscribe` exactly as the real client sends it — the string
        `"0"` filter and nothing else; there is no lease key at generation 5."""
        return self.call("events.subscribe", {"tab_id_filter": "0"})


@pytest.fixture
def roost():
    """A fake roost-session with one agent-owned tab, torn down after the test.

    **NOT named `fake`.** `conftest.py` owns a SESSION-scoped `fake` fixture (the
    fake host-agent) that the autouse `_app_session` requests by name, so a
    module-level `fake` here shadows it and the whole session dies with a
    `ScopeMismatch` — but only when this module happens to be collected first,
    which makes it an intermittent landmine rather than an obvious break.

    `shutdown()`, not `stop()`: `stop()` is roost's `session.stopping` control on
    both fakes, and the teardown is a separate thing.
    """
    f = FakeRoost().start()
    f.add_tab(5, cwd="/home/shed/work", title="OC", source="opencode",
              session_id="ses_selftest", lifecycle="working", detail="session_status")
    try:
        yield f
    finally:
        f.shutdown()


# ---------------------------------------------------------------------------
# the vectors this fake is built from
# ---------------------------------------------------------------------------


def test_the_vendored_vectors_carry_what_the_fake_reads(roost):
    """The vectors are the fake's whole claim to fidelity — if one stops parsing,
    or loses the key the fake reads out of it, that has to fail here rather than
    as a confusing decode error three layers up. (The Rust twin is
    `the_vendored_vectors_carry_what_the_fake_reads`.)

    The `session.identify` filename is generation-suffixed upstream and shed
    vendors only the current one, so a rename lands as an import error in
    `fake_roost` — which is exactly what this module's import catches.
    """
    identified = roost_call(roost.socket_path, "session.identify")
    assert identified["session_protocol"] == 5, identified
    assert identified["session_id"], identified
    assert "features" not in identified, "`features` retired at generation 5"

    listed = roost_call(roost.socket_path, "tab.list")
    assert isinstance(listed["revision"], int)
    assert [t["id"] for p in listed["projects"] for t in p["tabs"]] == ["5"]


# ---------------------------------------------------------------------------
# the event stream
# ---------------------------------------------------------------------------


def test_shed_subscribes_and_takes_nothing(roost):
    """`events.subscribe` REGISTERS, it does not classify — the driver/observer
    split retired with the lease, so a subscriber is just a subscriber.

    Mirrors `testing.rs`'s stream-registration cells; there is nothing left on
    this wire for a client to hold beyond the stream itself."""
    with _Wire(roost.socket_path) as w:
        ack = w.subscribe()
        assert ack["revision"] == roost.revision, ack
        assert roost.stream_count() == 1


def test_the_subscribe_ack_echoes_the_session_id_and_follows_a_restart(roost):
    """The ack names the incarnation answering, and it **tracks a restart** —
    which is the whole reason roost made the field required at generation 5: a
    client that identifies on one connection and subscribes on another has to be
    able to tell that the two dials landed on different processes.

    Mirrors `testing.rs::the_subscribe_ack_echoes_the_session_id_and_follows_a_restart`.
    """
    identified = roost_call(roost.socket_path, "session.identify")
    with _Wire(roost.socket_path) as w:
        ack = w.subscribe()
    assert ack["session_id"] == identified["session_id"], ack

    roost.restart()
    identified_after = roost_call(roost.socket_path, "session.identify")
    assert identified_after["session_id"] != identified["session_id"], (
        "a restart is a new incarnation and identify has to say so"
    )
    with _Wire(roost.socket_path) as w:
        ack_after = w.subscribe()
    assert ack_after["session_id"] == identified_after["session_id"] == roost.session_id


def test_a_filtered_subscribe_is_refused_rather_than_served_unfiltered(roost):
    """The real client always sends `"0"`, so this is only reachable by hand —
    which is exactly what a client written against a roost that implements the
    filter would do, and what must not be silently served an unfiltered stream
    instead."""
    with _Wire(roost.socket_path) as w:
        with pytest.raises(RoostWireError) as refused:
            w.call("events.subscribe", {"tab_id_filter": "5"})
        assert refused.value.code == "invalid-param", refused.value
        assert roost.stream_count() == 0, "a refused subscribe registered a stream"

        # And the connection is still usable: a refusal is not a hang-up.
        assert w.subscribe()["revision"] == roost.revision


def test_every_mutation_commits_exactly_one_batch_from_the_vendored_envelopes(roost):
    """One commit, one batch, at `ack + 1` — empty ones included, because that is
    what makes a skipped revision mean loss and nothing else.

    Mirrors `testing.rs::every_mutation_commits_exactly_one_batch`.
    """
    with _Wire(roost.socket_path) as w:
        acked = w.subscribe()["revision"]

        def names() -> list[str]:
            batch = w.readline()
            assert batch is not None, "the stream closed"
            return [e["event"] for e in batch["events"]]

        roost.bump_revision()
        batch = w.readline()
        assert batch["revision"] == acked + 1, batch
        assert batch["events"] == [], "an empty commit is still a batch"

        roost.add_tab(9, cwd="/home/shed/other", title="zsh")
        assert names() == ["tab.opened"]

        roost.set_axes(9, lifecycle="waiting", has_notification=True)
        assert names() == ["agent_report.changed", "tab.notification"], (
            "a notification flip rides the same commit"
        )
        # The sticky bit did not move this time, so there is no second envelope.
        roost.set_axes(9, lifecycle="working")
        assert names() == ["agent_report.changed"]

        # On a SECOND connection: `w` is a stream now, and roost reads a
        # subscribed connection only to notice a peer that went away — anything
        # written on it is discarded, never dispatched.
        roost_call(roost.socket_path, "tab.close", {"tab_id": "9"})
        assert names() == ["tab.closed"]


def test_a_skipped_revision_leaves_a_hole_in_the_sequence(roost):
    """`skip_revision` is the only way to manufacture the loss a resync exists
    for; roost itself never does it (it closes the stream instead)."""
    with _Wire(roost.socket_path) as w:
        acked = w.subscribe()["revision"]
        roost.skip_revision()
        roost.bump_revision()
        batch = w.readline()
        assert batch["revision"] == acked + 2, f"expected a hole at {acked + 1}: {batch}"


def test_a_lagging_subscriber_is_closed_rather_than_thinned(roost):
    """**The server closes rather than thins.** A subscriber that falls behind the
    fan-out is dropped, and the bare EOF is the client's resync signal — never a
    sequence with holes punched in it, which a client cannot tell from loss."""
    with _Wire(roost.socket_path) as w:
        w.subscribe()
        # Overrun the queue without reading a single frame. Generously past the
        # capacity so the drop cannot depend on how many the push thread drained.
        for _ in range(FRAME_CAPACITY * 3):
            roost.bump_revision()
        seen = 0
        while True:
            frame = w.readline()
            if frame is None:
                break
            seen += 1
            assert seen <= FRAME_CAPACITY + 2, "a lagging subscriber was thinned, not closed"


def test_a_stalled_write_is_bounded_by_the_deadline(roost):
    """**A peer that stops READING is closed too**, not just one that overruns
    the queue.

    The queue-overflow cell above would still pass with the write deadline
    deleted: those frames never reach a `send`. This one does the other thing —
    one frame far larger than any socket buffer, and a peer that never reads a
    byte of it — so the `send` genuinely blocks. Without the deadline the push
    thread parks forever holding the stream registered, and a later `stop()`
    would be unhonourable; with it, the subscriber is simply gone.
    """
    with _Wire(roost.socket_path) as w:
        w.subscribe()
        assert roost.stream_count() == 1

        # Bigger than any default AF_UNIX buffer (~200 KB), so the write cannot
        # complete into the kernel and be forgotten about.
        started = time.monotonic()
        roost.set_axes(5, detail="x" * 4_000_000)

        deadline = time.monotonic() + STREAM_WRITE_DEADLINE + 30
        while roost.stream_count() and time.monotonic() < deadline:
            time.sleep(0.02)
        elapsed = time.monotonic() - started
        assert roost.stream_count() == 0, (
            "a peer that stopped reading held the stream past the write deadline"
        )
        assert elapsed >= STREAM_WRITE_DEADLINE / 2, (
            f"closed after {elapsed:.2f}s — too fast to have been the deadline"
        )


def test_stop_says_why_and_then_serves_nothing_until_restart(roost):
    """`stop()` is terminal AND latching: the stream is told why, and nothing
    answers afterwards until a restart. A real stopped daemon accepts nothing,
    and a fake that kept answering would make "further attempts fail"
    untestable."""
    with _Wire(roost.socket_path) as w:
        w.subscribe()
        roost.stop()

        told = w.readline()
        assert told["event"] == "session.stopping", told
        assert told["data"]["reason"] == "stop", told
        assert w.readline() is None, "the stream ends after the goodbye"

    with pytest.raises((RoostWireError, OSError)):
        roost_call(roost.socket_path, "session.identify")

    roost.restart()
    identified = roost_call(roost.socket_path, "session.identify")
    assert identified["session_id"], identified
    assert roost.revision == 1, "the revision counter is in-process"
    assert roost.tab_ids() == [5], "tab ids persist across a restart"


def test_the_tab_list_hook_commits_between_the_ack_and_the_reply(roost):
    """The subscribe-then-list prologue's race, in one seam.

    A mutation that commits here lands between the ack a client fenced on and the
    snapshot it is about to take — exactly the interleaving a busy daemon
    produces and the one a naive client turns into a spurious gap. The hook also
    reads the stream registry from INSIDE the `tab.list` lock, which is how the
    ORDERING (subscribe first, then list) is proved rather than only the
    arithmetic.
    """
    with _Wire(roost.socket_path) as w:
        acked = w.subscribe()["revision"]

        seen: dict = {}

        def race(hook) -> None:
            seen["streams"] = hook.stream_count()
            hook.bump_revision()

        roost.before_tab_list(race)
        listed = roost_call(roost.socket_path, "tab.list")
        assert listed["revision"] == acked + 1, (
            "the snapshot must already be past the batch the hook pushed"
        )
        assert seen["streams"] == 1, "the client subscribed BEFORE it listed"
        # Once: the next list is not raced again.
        assert roost_call(roost.socket_path, "tab.list")["revision"] == acked + 1
        assert roost.tab_list_calls == 2


# ---------------------------------------------------------------------------
# session.set_agent_hooks: open to every connection at generation 5
# ---------------------------------------------------------------------------


def test_two_connections_both_wire_hooks_and_the_last_one_is_recorded(roost):
    """`session.set_agent_hooks` is **open to every connection** at generation 5
    and the last writer wins.

    This is the one behaviour the bump actually changed about this op — roost
    deleted `AgentHooksAuthority` and `AgentHooksError::Unauthorized` outright —
    and it is the thing the deleted lease-registry tests were standing in front
    of. Two connections, no coordination, both served.

    Mirrors `testing.rs::two_connections_both_wire_hooks_and_the_last_one_is_recorded`.
    """
    with _Wire(roost.socket_path) as desktop, _Wire(roost.socket_path) as phone:
        desktop.call(
            "session.set_agent_hooks", {"mode": "auto", "skip": [], "client": "shed-desktop"}
        )
        phone.call(
            "session.set_agent_hooks", {"mode": "auto", "skip": [], "client": "shed-mobile"}
        )

        calls = roost.agent_hooks_calls
        assert len(calls) == 2, calls
        assert calls[0]["client"] == "shed-desktop"
        assert calls[1]["client"] == "shed-mobile", "`client` is the record of who wrote last"
        for call in calls:
            assert "lease" not in call, f"no authority travels with this op any more: {call}"


def test_the_hooks_op_sent_twice_yields_an_identical_wired_set(roost):
    """**Semantic idempotence, asserted rather than argued.** Sending the op
    twice against the fake yields the same recorded call twice — which is the
    whole of decision D1's safety argument: an unconditional re-send on every
    watcher cycle is safe exactly because the second call says the same thing as
    the first.

    Mirrors `testing.rs::the_hooks_op_sent_twice_yields_an_identical_wired_set`.
    """
    with _Wire(roost.socket_path) as w:
        params = {"mode": "auto", "skip": [], "client": "shed-desktop"}
        first = w.call("session.set_agent_hooks", params)
        second = w.call("session.set_agent_hooks", params)
        assert first == second

        calls = roost.agent_hooks_calls
        assert len(calls) == 2
        assert calls[0] == calls[1], "the same request, byte for byte"


# ---------------------------------------------------------------------------
# the UI socket
# ---------------------------------------------------------------------------


def test_a_ui_socket_publishes_no_fence_and_serves_no_stream(roost):
    """roost's OTHER socket. It answers `unknown-op` to `session.identify` and
    `session.set_agent_hooks` (which is the only way a client can tell the two
    apart), serves no event stream, and omits `revision` from `tab.list`
    ENTIRELY rather than sending a number nothing could be fenced against.
    """
    roost.ui_socket = True

    for op in ("session.identify", "session.set_agent_hooks"):
        with pytest.raises(RoostWireError) as unknown:
            roost_call(roost.socket_path, op)
        assert unknown.value.code == "unknown-op", (op, unknown.value)

    with _Wire(roost.socket_path) as w:
        with pytest.raises(RoostWireError) as unimplemented:
            w.subscribe()
        assert unimplemented.value.code == "not-implemented", unimplemented.value

    listed = roost_call(roost.socket_path, "tab.list")
    assert "revision" not in listed, f"a UI socket published a fence: {listed}"
    assert listed["projects"], listed

    roost_call(roost.socket_path, "tab.write", {"tab_id": "5", "data": _b64("hi")})
    assert roost.written(5) == b"hi"


def test_a_protocol_mismatch_is_a_control_not_a_vector(roost):
    """shed vendors only the CURRENT `session.identify` generation, so an older
    daemon is a control on this fake rather than a second vector to keep in step
    (`crates/CLAUDE.md`, "The one git dependency").

    Mirrors the Rust fake's `set_session_protocol` control, exercised by
    `conn.rs`'s `a_protocol_mismatch_is_refused_by_name` family — including the
    protocol-4 case, which is now the interesting one: a session plan 019's
    desktop installed everywhere is exactly what this build must refuse by name.
    """
    roost.session_protocol = 2
    assert roost_call(roost.socket_path, "session.identify")["session_protocol"] == 2

    roost.session_protocol = 4
    identified = roost_call(roost.socket_path, "session.identify")
    assert identified["session_protocol"] == 4
