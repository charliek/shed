"""The Python fake roost-session, checked on its own wire.

`fake_roost.FakeRoost` is the Python twin of
`shed_core::roost::testing::FakeRoost` (`crates/shed-core/src/roost/testing.rs`),
and the two drifting apart is the failure mode this file exists to prevent.

**Why it cannot be left to `test_tauri_machines.py`.** That suite drives the fake
through the real app, and the app is an *observer*: it identifies, subscribes
with an empty lease, lists, and folds batches. It never calls `session.connect`,
never presents a lease on a `tab.write`, never asks for a filtered subscribe, and
never holds two connections whose fates a takeover decides. So the entire lease
half of protocol 4 — the half the Rust fake's own unit tests pin at length —
would sit here unexercised, and a Python port that got roost's takeover table
subtly wrong would pass every other test in the harness.

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
    _normalize_label,
    roost_call,
)

#: A well-formed lease this fake never minted (the mint is counter-seeded, so a
#: run of `f`s cannot collide with one).
UNKNOWN_LEASE = "f" * 32


def _b64(text: str) -> str:
    return base64.b64encode(text.encode()).decode()


class _Wire:
    """One connection that STAYS OPEN, unlike `roost_call`'s dial-per-request.

    The lease protocol is per-connection in two places `roost_call` cannot reach:
    `session.connect` refuses a second connect on the connection that already
    holds the lease, and a takeover closes the deposed holder's connection. Both
    need the same socket for two requests.
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

    def subscribe(self, lease: str = "") -> dict:
        """`events.subscribe` exactly as the real client sends it — a `lease`
        (empty for an observer) and the string `"0"` filter."""
        return self.call("events.subscribe", {"lease": lease, "tab_id_filter": "0"})


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
    assert identified["session_protocol"] == 4, identified
    assert identified["session_id"], identified
    assert identified["features"] == ["put_file"], "the .v4 vector's features survive"

    listed = roost_call(roost.socket_path, "tab.list")
    assert isinstance(listed["revision"], int)
    assert [t["id"] for p in listed["projects"] for t in p["tabs"]] == ["5"]


def test_a_client_label_is_normalized_the_way_roost_normalizes_it():
    """Mirrors `testing.rs::a_client_label_is_normalized_the_way_roost_normalizes_it`.

    The label is echoed back as `session.driver_changed.taken_by`, so the two
    fakes agreeing on the normalization is the difference between a client being
    tested against roost's string and against ours.
    """
    assert _normalize_label("  workbox  ") == "workbox"
    assert _normalize_label("work\u0007box") == "workbox", "an ASCII control"
    assert _normalize_label("   ") is None
    assert len(_normalize_label("x" * 200)) == 128

    # **The half "is a control character" does not cover.** Unicode classifies
    # none of these as controls, so a control-only filter passes them into the
    # takeover banner — the separators break it onto a second line and the
    # overrides reorder everything after them. An ASCII-only test would certify a
    # parity with roost that does not exist.
    assert _normalize_label("work\u202ebox") == "workbox", "a right-to-left override"
    assert _normalize_label("work\u2066box\u2069") == "workbox", "the isolate family"
    assert _normalize_label("work\u2028box") == "workbox", "a line separator"
    assert _normalize_label("work\u2029box") == "workbox", "a paragraph separator"
    assert _normalize_label("\u202e\u2028") is None, "hostile-only"


# ---------------------------------------------------------------------------
# the lease
# ---------------------------------------------------------------------------


def test_tab_write_on_a_session_socket_is_lease_gated(roost):
    """The four answers roost gives a `tab.write`'s `lease` key, in order:
    absent, unknown, current, tombstoned.

    Absent and unknown are deliberately the SAME answer (`connect-required`) —
    roost does not tell a caller whether a token it invented ever existed.
    """
    with _Wire(roost.socket_path) as w:
        with pytest.raises(RoostWireError) as absent:
            w.call("tab.write", {"tab_id": "5", "data": _b64("no lease")})
        assert absent.value.code == "connect-required", absent.value

        with pytest.raises(RoostWireError) as unknown:
            w.call("tab.write", {"tab_id": "5", "data": _b64("x"), "lease": UNKNOWN_LEASE})
        assert unknown.value.code == "connect-required", unknown.value
        assert roost.written(5) == b"", "a refused write must not land"

        lease = w.call("session.connect", {"takeover": False, "client_label": "self-test"})["lease"]
        assert len(lease) == 32, lease
        assert all(c in "0123456789abcdef" for c in lease), lease
        assert roost.lease == lease
        assert roost.lease_label == "self-test"

        w.call("tab.write", {"tab_id": "5", "data": _b64("hi"), "lease": lease})
        assert roost.written(5) == b"hi"

    # The tombstone is answered on a FRESH connection, because the takeover just
    # closed the one that held the displaced lease (see the takeover cell below).
    roost.take_over("workbox")
    with _Wire(roost.socket_path) as after:
        with pytest.raises(RoostWireError) as displaced:
            after.call("tab.write", {"tab_id": "5", "data": _b64("x"), "lease": lease})
        assert displaced.value.code == "taken-over", displaced.value


def test_exactly_one_tombstone_survives_two_takeovers(roost):
    """roost remembers only the MOST RECENTLY displaced lease, so a lease
    displaced twice falls back to `connect-required` — it has already been told.

    Mirrors `testing.rs::the_lease_follows_roosts_takeover_table_with_exactly_one_tombstone`.
    A fake that kept every tombstone would let a client be written against a
    `taken-over` roost stops sending.
    """
    with _Wire(roost.socket_path) as holder:
        connected = holder.call("session.connect", {"takeover": False, "client_label": "first"})
        first = connected["lease"]

    roost.take_over("second")
    with _Wire(roost.socket_path) as writer:
        with pytest.raises(RoostWireError) as displaced:
            writer.call("tab.write", {"tab_id": "5", "data": _b64("x"), "lease": first})
        assert displaced.value.code == "taken-over", displaced.value

        roost.take_over("third")
        with pytest.raises(RoostWireError) as forgotten:
            writer.call("tab.write", {"tab_id": "5", "data": _b64("x"), "lease": first})
        assert forgotten.value.code == "connect-required", forgotten.value


def test_a_write_with_the_live_lease_registers_that_connection_too(roost):
    """**A lease-bearing write registers its connection under the lease**, not
    just `session.connect` does.

    roost's `present()` is what every lease-carrying op runs, and on the live
    lease it pushes the connection onto the holder's list — which is the list a
    takeover closes. A fake that registered only the connecting one would let a
    second connection keep writing straight through a takeover, which is
    precisely the authority the lease exists to move.

    Mirrors `testing.rs::a_write_with_the_live_lease_registers_that_connection_too`.
    """
    owner = _Wire(roost.socket_path)
    lease = owner.call("session.connect", {"takeover": False, "client_label": "owner"})["lease"]

    # A SECOND connection that never connected, only wrote.
    writer = _Wire(roost.socket_path)
    writer.call("tab.write", {"tab_id": "5", "data": _b64("ok"), "lease": lease})
    assert roost.written(5) == b"ok"

    with _Wire(roost.socket_path) as observer:
        observer.subscribe("")

        roost.take_over("usurper")

        with pytest.raises((RoostWireError, OSError)):
            owner.call("tab.list")
        with pytest.raises((RoostWireError, OSError)):
            writer.call("tab.list")
        owner.close()
        writer.close()

        # …and the stream is spared, and still delivering.
        told = observer.readline()
        assert told["event"] == "session.driver_changed", told
        roost.bump_revision()
        assert observer.readline()["revision"] == roost.revision


def test_the_write_gate_runs_before_the_tab_lookup(roost):
    """**The lease is checked before the tab.** roost decodes, runs
    `require_lease`, and only then writes — so an unknown tab presented without
    authority answers `connect-required` and never confirms whether that tab
    exists. With the checks the other way round, an unauthorized caller could
    enumerate a session's tabs by the error code alone."""
    with _Wire(roost.socket_path) as w:
        with pytest.raises(RoostWireError) as blind:
            w.call("tab.write", {"tab_id": "4242", "data": _b64("x")})
        assert blind.value.code == "connect-required", blind.value

        lease = w.call("session.connect", {"takeover": False})["lease"]
        with pytest.raises(RoostWireError) as missing:
            w.call("tab.write", {"tab_id": "4242", "data": _b64("x"), "lease": lease})
        assert missing.value.code == "not-found", missing.value


def test_a_connect_on_a_held_session_is_already_connected_even_from_its_holder(roost):
    """roost's table, whole: held by ANYONE — the caller's own connection
    included — without `takeover` is `already-connected`.

    A client that lost track of its own lease is the one that has to
    re-establish it deliberately, and `takeover: true` from that same connection
    is how: it mints, and roost spares the requester's connection.
    """
    with _Wire(roost.socket_path) as w:
        w.call("session.connect", {"takeover": False})
        with pytest.raises(RoostWireError) as held:
            w.call("session.connect", {"takeover": False})
        assert held.value.code == "already-connected", held.value

        mine = w.call("session.connect", {"takeover": True, "client_label": "mine"})["lease"]
        # The requester keeps its connection — this is the proof, since a closed
        # one could not answer at all.
        w.call("tab.write", {"tab_id": "5", "data": _b64("ok"), "lease": mine})
        assert roost.written(5) == b"ok"


def test_a_takeover_closes_the_deposed_holders_control_connection_but_not_streams(roost):
    """The asymmetry R1 introduced, in one cell: a takeover **closes** the
    deposed holder's control connection and **spares** every event stream,
    telling them once with a non-terminal `session.driver_changed`.

    Mirrors `testing.rs::a_takeover_closes_the_deposed_holders_control_connection`
    plus `a_takeover_tells_every_stream_and_closes_none`.
    """
    holder = _Wire(roost.socket_path)
    holder.call("session.connect", {"takeover": False, "client_label": "deposed"})
    with _Wire(roost.socket_path) as watcher:
        watcher.subscribe("")

        roost.take_over("usurper")

        with pytest.raises((RoostWireError, OSError)):
            holder.call("tab.list")
        holder.close()

        told = watcher.readline()
        assert told["event"] == "session.driver_changed", told
        assert told["data"]["taken_by"] == "usurper", told

        # Not terminal: the next commit still arrives on the same stream.
        roost.bump_revision()
        batch = watcher.readline()
        assert batch["revision"] == roost.revision, batch
        assert batch["events"] == [], batch


# ---------------------------------------------------------------------------
# the event stream
# ---------------------------------------------------------------------------


def test_an_empty_lease_is_an_observer_and_a_current_one_is_a_driver(roost):
    """`events.subscribe` CLASSIFIES on the lease, it does not gate on one — which
    is what makes shed's watcher able to read somebody's machine without taking
    the driver seat from the roost UI they are looking at.

    The driver half is what gives "shed subscribes as an observer" any content:
    without it, an observer count of one would also pass against a fake that
    called every stream an observer.
    """
    with _Wire(roost.socket_path) as observer:
        ack = observer.subscribe("")
        assert ack["revision"] == roost.revision, ack
        assert roost.observer_count() == 1
        assert roost.driver_count() == 0

        control = _Wire(roost.socket_path)
        lease = control.call("session.connect", {"takeover": False})["lease"]
        with _Wire(roost.socket_path) as driver:
            driver.subscribe(lease)
            assert roost.driver_count() == 1
            assert roost.observer_count() == 1

            # A takeover reclassifies it IN PLACE rather than closing it.
            roost.take_over("someone else")
            assert roost.driver_count() == 0
            assert roost.observer_count() == 2
            told = driver.readline()
            assert told["event"] == "session.driver_changed", told
        control.close()


def test_a_filtered_subscribe_is_refused_rather_than_served_unfiltered(roost):
    """The real client always sends `"0"`, so this is only reachable by hand —
    which is exactly what a client written against a roost that implements the
    filter would do, and what must not be silently served an unfiltered stream
    instead."""
    with _Wire(roost.socket_path) as w:
        with pytest.raises(RoostWireError) as refused:
            w.call("events.subscribe", {"lease": "", "tab_id_filter": "5"})
        assert refused.value.code == "invalid-param", refused.value
        assert roost.observer_count() == 0, "a refused subscribe registered a stream"

        # And the connection is still usable: a refusal is not a hang-up.
        assert w.subscribe("")["revision"] == roost.revision


def test_every_mutation_commits_exactly_one_batch_from_the_vendored_envelopes(roost):
    """One commit, one batch, at `ack + 1` — empty ones included, because that is
    what makes a skipped revision mean loss and nothing else.

    Mirrors `testing.rs::every_mutation_commits_exactly_one_batch`.
    """
    with _Wire(roost.socket_path) as w:
        acked = w.subscribe("")["revision"]

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
        acked = w.subscribe("")["revision"]
        roost.skip_revision()
        roost.bump_revision()
        batch = w.readline()
        assert batch["revision"] == acked + 2, f"expected a hole at {acked + 1}: {batch}"


def test_a_lagging_subscriber_is_closed_rather_than_thinned(roost):
    """**The server closes rather than thins.** A subscriber that falls behind the
    fan-out is dropped, and the bare EOF is the client's resync signal — never a
    sequence with holes punched in it, which a client cannot tell from loss."""
    with _Wire(roost.socket_path) as w:
        w.subscribe("")
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
        w.subscribe("")
        assert roost.observer_count() == 1

        # Bigger than any default AF_UNIX buffer (~200 KB), so the write cannot
        # complete into the kernel and be forgotten about.
        started = time.monotonic()
        roost.set_axes(5, detail="x" * 4_000_000)

        deadline = time.monotonic() + STREAM_WRITE_DEADLINE + 30
        while roost.observer_count() and time.monotonic() < deadline:
            time.sleep(0.02)
        elapsed = time.monotonic() - started
        assert roost.observer_count() == 0, (
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
        w.subscribe("")
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
        acked = w.subscribe("")["revision"]

        seen: dict = {}

        def race(hook) -> None:
            seen["observers"] = hook.observer_count()
            hook.bump_revision()

        roost.before_tab_list(race)
        listed = roost_call(roost.socket_path, "tab.list")
        assert listed["revision"] == acked + 1, (
            "the snapshot must already be past the batch the hook pushed"
        )
        assert seen["observers"] == 1, "the client subscribed BEFORE it listed"
        # Once: the next list is not raced again.
        assert roost_call(roost.socket_path, "tab.list")["revision"] == acked + 1
        assert roost.tab_list_calls == 2


# ---------------------------------------------------------------------------
# the UI socket
# ---------------------------------------------------------------------------


def test_a_ui_socket_mints_no_lease_and_publishes_no_fence(roost):
    """roost's OTHER socket. It answers `unknown-op` to the session ops (which is
    the only way a client can tell the two apart), serves no event stream, omits
    `revision` from `tab.list` ENTIRELY rather than sending a number nothing
    could be fenced against — and **accepts and ignores** a `tab.write` lease,
    because it mints none and refusing would make one client unable to talk to
    both kinds of socket.
    """
    roost.ui_socket = True

    for op in ("session.identify", "session.connect"):
        with pytest.raises(RoostWireError) as unknown:
            roost_call(roost.socket_path, op)
        assert unknown.value.code == "unknown-op", (op, unknown.value)

    with _Wire(roost.socket_path) as w:
        with pytest.raises(RoostWireError) as unimplemented:
            w.subscribe("")
        assert unimplemented.value.code == "not-implemented", unimplemented.value

    listed = roost_call(roost.socket_path, "tab.list")
    assert "revision" not in listed, f"a UI socket published a fence: {listed}"
    assert listed["projects"], listed

    roost_call(roost.socket_path, "tab.write", {"tab_id": "5", "data": _b64("hi")})
    roost_call(roost.socket_path, "tab.write",
               {"tab_id": "5", "data": _b64("!"), "lease": UNKNOWN_LEASE})
    assert roost.written(5) == b"hi!", "a UI socket ignores the lease key, both ways"


def test_a_protocol_mismatch_and_a_featureless_reply_are_controls_not_vectors(roost):
    """shed vendors only the CURRENT `session.identify` generation, so an older
    daemon is a control on this fake rather than a second vector to keep in step
    (`crates/CLAUDE.md`, "The one git dependency").

    Both controls exist on the Rust fake (`set_session_protocol`,
    `serve_without_features`); this pins the Python ones answer the same way.
    """
    roost.session_protocol = 2
    assert roost_call(roost.socket_path, "session.identify")["session_protocol"] == 2

    roost.session_protocol = 4
    roost.strip_features = True
    identified = roost_call(roost.socket_path, "session.identify")
    assert "features" not in identified, identified
    assert identified["session_protocol"] == 4
