"""The synthetic bus's own contract, on its wire (no daemon).

The daemon cells drive `SyntheticBus` through the real broker, which answers every
request at once and only ever as one machine, so the parts of the server's contract
about WHICH answers it accepts would pass there however wrong the bus got them. These
cells speak HTTP to the bus directly, against what the Go server does
(`internal/api/plugin_handlers.go`, `internal/plugin/bridge.go`, `pin.go`):

* an answer must name a request delivered on a stream (`in_reply_to`, same namespace
  and shed); anything else — never sent, already answered finally — is 403 FORBIDDEN;
* a move away (`refuse(ns, "not-source")`) ends the namespace's live stream, drops the
  requests pending on it and refuses answers 403 SOURCE_NOT_BOUND; after the move back
  (`accept(ns)`) the old answer matches nothing: 403 FORBIDDEN, never accepted;
* `restart()` (a server restart) ends every stream and forgets every pending request,
  so an answer to one delivered before is 403 FORBIDDEN; `drop_streams()` (a lost
  connection, the server up) ends every stream and keeps them, so the same machine's
  answer still matches;
* a new stream of the same machine (same `X-Shed-Machine-Id`), at the same binding
  generation, is first re-delivered every request still pending on its namespace
  (`registry.go`, `pendingForLocked`); another machine's stream is not;
* a stream that has ended never delivers a frame it takes after that: the frame goes back
  on the queue, and nothing becomes pending;
* a re-delivered request answered finally after the new stream copied it (the old
  stream's answer) is not written on the new stream and does not become pending again
  (`Listener.Take` checks the message is still live);
* a body over the limit is 413 REQUEST_TOO_LARGE (the server's code);
* with `single_holder`, a second stream for a held namespace is 409
  SOURCE_DUPLICATE_STREAM, `drop_streams()` frees every namespace, and a holder that
  goes away frees its namespace.
"""

from __future__ import annotations

import http.client
import json
from urllib.parse import urlsplit

from synthetic_bus import (
    DELIVER_ENDED,
    DELIVER_SKIP,
    MACHINE_ID_HEADER,
    MACHINE_NAME_HEADER,
    SyntheticBus,
)

NS = "ssh-agent"
MID = "00112233445566778899aabbccddeeff"


def _conn(bus) -> http.client.HTTPConnection:
    u = urlsplit(bus.url)
    return http.client.HTTPConnection(u.hostname, u.port, timeout=5)


def _headers(name: str = "box", mid: str = MID) -> dict:
    return {MACHINE_ID_HEADER: mid, MACHINE_NAME_HEADER: name}


def _request(rid: str) -> dict:
    return {"id": rid, "namespace": NS, "type": "request", "final": True, "timestamp": "t",
            "payload": {"operation": "ping"},
            "shed": {"name": "web", "backend": "vz", "server": "s"}}


def _deliver(bus, rid: str) -> None:
    """Open a stream, have the bus write request `rid` on it, read it, close it."""
    c = _conn(bus)
    c.request("GET", f"/api/plugins/listeners/{NS}/messages", headers=_headers())
    r = c.getresponse()
    assert r.status == 200
    bus.push_request(NS, _request(rid))
    while True:
        line = r.fp.readline()
        assert line, "the stream ended before the request"
        if line.startswith(b"data: "):
            assert json.loads(line[len(b"data: "):])["id"] == rid
            break
    r.close()
    c.close()
    # Its handler has noticed: a later request goes to the next stream, not this one.
    bus.wait_for(lambda b: b.live_streams() == 0, "the stream's handler ended", timeout=5.0)


def _respond(bus, in_reply_to: str, shed: str = "web") -> tuple[int, dict]:
    c = _conn(bus)
    body = json.dumps({"id": "a1", "namespace": NS, "type": "response", "final": True,
                       "in_reply_to": in_reply_to, "shed": {"name": shed},
                       "payload": {"status": "ok"}})
    c.request("POST", f"/api/plugins/listeners/{NS}/respond", body=body,
              headers=dict(_headers(), **{"Content-Type": "application/json"}))
    r = c.getresponse()
    raw = r.read()
    c.close()
    return r.status, (json.loads(raw) if raw else {})


def _code(answer) -> str:
    return answer[1]["error"]["code"]


def test_an_answer_must_match_a_delivered_request():
    with SyntheticBus() as bus:
        assert _code(_respond(bus, "never-sent")) == "FORBIDDEN"
        _deliver(bus, "r1")
        assert bus.pending() == [(NS, "r1")]
        assert _code(_respond(bus, "r1", shed="other")) == "FORBIDDEN", "another shed's"
        assert _respond(bus, "r1")[0] == 204
        assert bus.pending() == []
        assert _code(_respond(bus, "r1")) == "FORBIDDEN", "consumed by the final answer"


def test_a_move_away_and_back_drops_the_old_request():
    with SyntheticBus() as bus:
        _deliver(bus, "r-old")
        bus.refuse(NS, "not-source")  # the source moves to another machine
        status, body = _respond(bus, "r-old")
        assert (status, body["error"]["code"]) == (403, "SOURCE_NOT_BOUND"), body
        assert bus.pending() == [], "the move dropped it"
        bus.accept(NS)  # and back to this one
        status, body = _respond(bus, "r-old")
        assert (status, body["error"]["code"]) == (403, "FORBIDDEN"), body
        _deliver(bus, "r-new")
        assert _respond(bus, "r-new")[0] == 204


def test_413_is_the_servers_request_too_large():
    with SyntheticBus() as bus:
        _deliver(bus, "r1")
        bus.respond_script(NS, [(413, None)])
        status, body = _respond(bus, "r1")
        assert (status, body["error"]["code"]) == (413, "REQUEST_TOO_LARGE"), body


def test_single_holder_refuses_a_second_stream_until_dropped():
    with SyntheticBus(single_holder=True) as bus:
        c1 = _conn(bus)
        c1.request("GET", f"/api/plugins/listeners/{NS}/messages", headers=_headers("one"))
        r1 = c1.getresponse()
        assert r1.status == 200
        bus.wait_for(lambda b: b.holders() == {NS: "one"}, "one holds", timeout=5.0)
        c2 = _conn(bus)
        c2.request("GET", f"/api/plugins/listeners/{NS}/messages", headers=_headers("two"))
        r2 = c2.getresponse()
        assert r2.status == 409
        assert json.loads(r2.read())["error"]["code"] == "SOURCE_DUPLICATE_STREAM"
        c2.close()
        bus.drop_streams()
        assert r1.read() == b"", "the dropped stream ends"
        c1.close()
        assert bus.holders() == {}
        assert bus.attempts(NS) == [("one", True), ("two", False)]
        # A holder that goes away frees its namespace too.
        c3 = _conn(bus)
        c3.request("GET", f"/api/plugins/listeners/{NS}/messages", headers=_headers("three"))
        r3 = c3.getresponse()
        assert r3.status == 200
        bus.wait_for(lambda b: b.holders() == {NS: "three"}, "three holds", timeout=5.0)
        r3.close()
        c3.close()
        bus.wait_for(lambda b: b.holders() == {}, "freed when its holder left", timeout=5.0)


def _open_stream(bus, name: str = "box", mid: str = MID):
    c = _conn(bus)
    c.request("GET", f"/api/plugins/listeners/{NS}/messages", headers=_headers(name, mid))
    r = c.getresponse()
    assert r.status == 200
    bus.wait_for(lambda b: b.live_streams() == 1, "the stream is live", timeout=5.0)
    return c, r


def _next_id(r) -> str:
    """The id of the next request written on stream `r`."""
    while True:
        line = r.fp.readline()
        assert line, "the stream ended before a request"
        if line.startswith(b"data: "):
            assert r.fp.readline() == b"\n", "the event's closing blank line"
            return json.loads(line[len(b"data: "):])["id"]


def test_a_move_away_ends_the_live_stream():
    with SyntheticBus() as bus:
        c, r = _open_stream(bus)
        bus.refuse(NS, "not-source")
        assert r.read() == b"", "the move closed the old machine's stream"
        c.close()
        bus.wait_for(lambda b: b.live_streams() == 0, "its handler ended", timeout=5.0)


def test_a_restart_forgets_pending_requests_and_a_dropped_stream_keeps_them():
    with SyntheticBus() as bus:
        _deliver(bus, "r-before-restart")
        c, r = _open_stream(bus)
        assert _next_id(r) == "r-before-restart", "re-delivered to the same machine"
        assert bus.restart() > 0
        assert r.read() == b"", "the restart ended the stream"
        c.close()
        assert bus.pending() == [], "the restart forgot it"
        status, body = _respond(bus, "r-before-restart")
        assert (status, body["error"]["code"]) == (403, "FORBIDDEN"), body

        _deliver(bus, "r-before-drop")
        c, r = _open_stream(bus)
        assert _next_id(r) == "r-before-drop"
        bus.drop_streams()
        assert r.read() == b"", "the drop ended the stream"
        c.close()
        assert bus.pending() == [(NS, "r-before-drop")], "a lost connection keeps it"
        assert _respond(bus, "r-before-drop")[0] == 204


def test_a_new_stream_of_the_same_machine_is_re_delivered_what_is_pending():
    with SyntheticBus() as bus:
        _deliver(bus, "r1")
        # Another machine's stream is not handed it: the next request it sees is new.
        c, r = _open_stream(bus, name="other", mid="ffeeddccbbaa99887766554433221100")
        bus.push_request(NS, _request("r2"))
        assert _next_id(r) == "r2"
        r.close()
        c.close()
        bus.wait_for(lambda b: b.live_streams() == 0, "its handler ended", timeout=5.0)
        # The same machine's next stream is: first, with nothing pushed, in order.
        c, r = _open_stream(bus)
        assert _next_id(r) == "r1"
        r.close()
        c.close()
        bus.wait_for(lambda b: b.live_streams() == 0, "its handler ended", timeout=5.0)
        assert _respond(bus, "r1")[0] == 204
        # Answered finally: not re-delivered again.
        c, r = _open_stream(bus)
        bus.push_request(NS, _request("r3"))
        assert _next_id(r) == "r3"
        r.close()
        c.close()


def test_an_ended_stream_never_delivers_a_frame_it_takes_after():
    """The review's interleaving, through the bus's own methods: a stream waiting for a
    frame is ended (a restart, a move, a drop), then takes one. It must not write it:
    the frame goes back on the queue and nothing becomes pending."""
    with SyntheticBus() as bus:
        token, _end, redeliver = bus._claim(NS, "box", MID)
        assert redeliver == []
        bus.drop_streams()
        q = bus._queue_for(NS)
        frame = json.dumps(_request("late"))
        assert bus._deliver(NS, token, frame, q) == DELIVER_ENDED, "an ended stream delivers nothing"
        assert q.get_nowait() == frame, "back on the queue for the next stream"
        assert bus.pending() == []


def test_a_redelivered_request_answered_meanwhile_is_not_resurrected():
    """The review's sequence: a request is delivered; its stream drops; the same machine's
    new stream copies it into its backlog; the old stream's final answer consumes it; the
    backlog then comes to write it. It must not be written or become pending again, so a
    second final answer is refused, as the server refuses it."""
    with SyntheticBus() as bus:
        _deliver(bus, "r1")
        token, _end, redeliver = bus._claim(NS, "box", MID)
        assert [json.loads(f)["id"] for f in redeliver] == ["r1"]
        assert _respond(bus, "r1")[0] == 204, "the old stream's final answer"
        assert bus.pending() == []
        assert bus._deliver(NS, token, redeliver[0], None) == DELIVER_SKIP
        assert bus.pending() == [], "not pending again"
        status, body = _respond(bus, "r1")
        assert (status, body["error"]["code"]) == (403, "FORBIDDEN"), body
        bus._release(NS, token)
