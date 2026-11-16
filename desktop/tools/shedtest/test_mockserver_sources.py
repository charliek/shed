"""The mock shed-server's credential-source contract, checked on its own wire.

`MockShedServer.arm_source` stands in for a pinning shed-server (plan 035 §3.4) in
the "(d1b)" cells of `test_tauri_broker.py`. Those cells drive it through the real
app, which only ever holds one machine's streams, so the parts of the server's
contract that concern ANOTHER machine's live streams — what a move does to them —
would pass there however wrong the mock got them. These cells speak HTTP to the
mock directly (no app), as two machines:

* a move releases every slot the old machine holds and ends its streams, so the
  new source's subscribe is admitted and served (not the legacy terminal 409);
* after a move the old machine's answers are refused 403 SOURCE_NOT_BOUND, an
  answer in flight across the move included;
* a machine id must be exactly 32 lowercase hex characters — one followed by a
  newline is malformed (400 INVALID_REQUEST), not an unseen machine (404);
* `503 SOURCE_BUSY` carries `Retry-After`;
* an answer must match a request delivered to that machine at the binding's current
  generation: after a move A → B → A, A's answer to a request delivered before the
  first move is 403 FORBIDDEN (the first move dropped it), as on the server;
* the bound machine subscribing to a namespace it already holds is the retryable 409
  SOURCE_DUPLICATE_STREAM;
* the codes are the server's own: 413 REQUEST_TOO_LARGE, an unconfirmed move 500
  INTERNAL_ERROR;
* `pause()` / `resume()` take the server down and back on the same port, and, as a
  real restart, forget the pending requests;
* a stream is bound to the binding generation it was admitted at: one that stopped
  being live (a move away and back) cannot take a request, which goes to the next
  stream instead, as the server's `Listener.Take` refuses an unregistered listener;
* refusals come in the server's order: the machine headers' shape, the body's size,
  then the binding (a non-bound machine's oversized answer is 413, not 403).
"""

from __future__ import annotations

import http.client
import json
import time
from urllib.parse import urlsplit

import pytest

from client import scaled_timeout
from mockserver import MACHINE_ID_HEADER, MACHINE_NAME_HEADER, MockShedServer

DESK = "fedcba9876543210fedcba9876543210"
LAPTOP = "0123456789abcdef0123456789abcdef"
NS = "ssh-agent"


@pytest.fixture
def armed():
    mock = MockShedServer()
    mock.start()
    mock.arm_source(DESK, "desk")
    try:
        yield mock
    finally:
        mock.stop()


def _conn(mock: MockShedServer, timeout: float = 5.0) -> http.client.HTTPConnection:
    u = urlsplit(mock.base_url)
    return http.client.HTTPConnection(u.hostname, u.port, timeout=scaled_timeout(timeout))


def _subscribe(mock, machine_id: str, name: str):
    """Open `NS`'s stream as `machine_id`; the connection and its response."""
    c = _conn(mock)
    c.request("GET", f"/api/plugins/listeners/{NS}/messages",
              headers={MACHINE_ID_HEADER: machine_id, MACHINE_NAME_HEADER: name})
    return c, c.getresponse()


def _respond(mock, machine_id: str, in_reply_to: str = "req-1") -> tuple[int, dict]:
    c = _conn(mock)
    body = json.dumps({"id": "r1", "namespace": NS, "type": "response", "final": True,
                       "in_reply_to": in_reply_to, "shed": {"name": "web"}})
    c.request("POST", f"/api/plugins/listeners/{NS}/respond", body=body,
              headers={MACHINE_ID_HEADER: machine_id, MACHINE_NAME_HEADER: "m",
                       "Content-Type": "application/json"})
    r = c.getresponse()
    raw = r.read()
    c.close()
    return r.status, (json.loads(raw) if raw else {})


def _use(mock, machine_id) -> tuple[int, dict]:
    c = _conn(mock)
    c.request("POST", "/api/sources/use", body=json.dumps({"machine_id": machine_id}),
              headers={"Content-Type": "application/json"})
    r = c.getresponse()
    body = json.loads(r.read())
    c.close()
    return r.status, body


def _request(rid: str) -> dict:
    return {"id": rid, "namespace": NS, "type": "request", "final": True,
            "timestamp": "t", "payload": {"operation": "ping"},
            "shed": {"name": "web", "backend": "vz", "server": "mock"}}


def _deliver(mock, machine_id: str, name: str, rid: str) -> None:
    """Subscribe as `machine_id`, push request `rid` and read it off the stream (it is
    then pending, delivered to that machine), then end the stream."""
    c, r = _subscribe(mock, machine_id, name)
    assert r.status == 200, r.read()
    mock.push_request(NS, _request(rid))
    while True:
        line = r.fp.readline()
        assert line, "the stream ended before the request"
        if line.startswith(b"data: "):
            assert json.loads(line[len(b"data: "):])["id"] == rid
            break
    c.close()
    r.close()
    _wait(lambda: NS not in mock.bus_live(), "the stream released")


def _wait(pred, what: str, timeout: float = 5.0) -> None:
    deadline = time.monotonic() + scaled_timeout(timeout)
    while not pred():
        if time.monotonic() > deadline:
            raise AssertionError(f"timed out waiting for {what}")
        time.sleep(0.02)


def test_a_move_releases_the_old_sources_slots_and_ends_its_streams(armed):
    mock = armed
    old_conn, old = _subscribe(mock, DESK, "desk")
    assert old.status == 200
    _wait(lambda: mock.bus_live().get(NS) == DESK, "desk holding the slot")
    doc = json.loads(_conn_get(mock, "/api/sources"))
    assert {m["id"]: m["namespaces"] for m in doc["machines"]}[DESK] == [NS]

    # Not the source yet: refused, and it never reaches the slot.
    c, refused = _subscribe(mock, LAPTOP, "laptop")
    assert refused.status == 403
    assert json.loads(refused.read())["error"]["code"] == "SOURCE_NOT_BOUND"
    c.close()

    assert _use(mock, LAPTOP)[0] == 200
    assert mock.bus_live() == {}, "the move released desk's slot"
    # desk's stream ends: its body reaches EOF.
    assert old.read() == b""
    old_conn.close()

    # The new source is admitted AND served — not the legacy terminal 409.
    new_conn, new = _subscribe(mock, LAPTOP, "laptop")
    assert new.status == 200, new.read()
    _wait(lambda: mock.bus_live().get(NS) == LAPTOP, "laptop holding the slot")
    new_conn.close()


def test_after_a_move_the_old_sources_answers_are_refused(armed):
    mock = armed
    _deliver(mock, DESK, "desk", "req-1")
    assert _respond(mock, DESK, "req-1")[0] == 204, "the source answers"
    _deliver(mock, DESK, "desk", "req-2")
    mock.know_machine(LAPTOP, "laptop")
    assert _use(mock, LAPTOP)[0] == 200
    status, body = _respond(mock, DESK, "req-2")
    assert status == 403, body
    assert body["error"]["code"] == "SOURCE_NOT_BOUND"
    assert body["source"]["machine_id"] == LAPTOP
    assert mock.pending() == [], "the move dropped the request delivered to desk"
    _deliver(mock, LAPTOP, "laptop", "req-3")
    assert _respond(mock, LAPTOP, "req-3")[0] == 204
    assert len(mock.respond_identities(NS)) == 3, "refused ones are still recorded"


def test_an_answer_must_match_a_pending_request():
    """Every mode (the server tracks pending requests in every mode): an answer to
    nothing, a second answer to a request already answered, and one naming another
    shed are 403 FORBIDDEN."""
    mock = MockShedServer()
    mock.start()
    try:
        assert _respond(mock, DESK, "never-sent") == (403, {"error": {
            "code": "FORBIDDEN",
            "message": "response does not match a pending request for this namespace"}})
        _deliver(mock, DESK, "desk", "req-1")
        assert _respond(mock, DESK, "req-1")[0] == 204
        assert _respond(mock, DESK, "req-1")[0] == 403, "consumed by the final answer"
    finally:
        mock.stop()


def test_an_old_answer_after_a_move_away_and_back_is_refused(armed):
    """A → B → A: the first move dropped the request delivered to A, so A's answer to
    it — A being the source again — matches nothing: 403 FORBIDDEN, as on the server
    (never accepted because A is bound once more)."""
    mock = armed
    _deliver(mock, DESK, "desk", "req-old")
    assert mock.pending() == [(NS, "req-old")]
    mock.know_machine(LAPTOP, "laptop")
    assert _use(mock, LAPTOP)[0] == 200
    assert _use(mock, DESK)[0] == 200
    assert mock.source_bound() == DESK
    status, body = _respond(mock, DESK, "req-old")
    assert status == 403, body
    assert body["error"]["code"] == "FORBIDDEN", body
    # A request delivered after the move back is answered as usual.
    _deliver(mock, DESK, "desk", "req-new")
    assert _respond(mock, DESK, "req-new")[0] == 204


def test_the_bound_machine_holding_a_namespace_is_a_duplicate(armed):
    mock = armed
    c1, first = _subscribe(mock, DESK, "desk")
    assert first.status == 200
    _wait(lambda: mock.bus_live().get(NS) == DESK, "desk holding the slot")
    c2, second = _subscribe(mock, DESK, "desk (copy)")
    assert second.status == 409
    assert json.loads(second.read())["error"]["code"] == "SOURCE_DUPLICATE_STREAM"
    c2.close()
    c1.close()


def test_refusal_codes_are_the_servers(armed):
    mock = armed
    status, body = _use(mock, "not-hex")
    assert (status, body["error"]["code"]) == (400, "INVALID_REQUEST"), body
    mock.source_use_script([500])
    status, body = _use(mock, DESK)
    assert (status, body["error"]["code"]) == (500, "INTERNAL_ERROR"), body
    _deliver(mock, DESK, "desk", "req-1")
    mock.respond_script(NS, [(413, None)])
    status, body = _respond(mock, DESK, "req-1")
    assert (status, body["error"]["code"]) == (413, "REQUEST_TOO_LARGE"), body


def test_pause_takes_the_server_down_and_resume_brings_it_back(armed):
    mock = armed
    url = mock.base_url
    assert json.loads(_conn_get(mock, "/api/sources"))["binding"] is not None
    mock.pause()
    with pytest.raises(OSError):
        _conn_get(mock, "/api/sources")
    mock.resume()
    assert mock.base_url == url, "the same port"
    assert json.loads(_conn_get(mock, "/api/sources"))["binding"] is not None


def test_a_machine_id_with_a_trailing_newline_is_malformed(armed):
    mock = armed
    status, body = _use(mock, DESK + "\n")
    assert status == 400, body
    assert _use(mock, LAPTOP)[0] == 404, "a well-formed unseen id is still 404"
    assert mock.source_bound() == DESK


def test_a_busy_read_carries_retry_after(armed):
    mock = armed
    mock.source_busy(1, retry_after="2")
    c = _conn(mock)
    c.request("GET", "/api/sources")
    r = c.getresponse()
    assert r.status == 503
    assert r.getheader("Retry-After") == "2"
    assert json.loads(r.read())["error"]["code"] == "SOURCE_BUSY"
    c.close()
    assert json.loads(_conn_get(mock, "/api/sources"))["binding"] is not None


def _conn_get(mock, path: str) -> bytes:
    c = _conn(mock)
    c.request("GET", path)
    r = c.getresponse()
    assert r.status == 200, r.status
    raw = r.read()
    c.close()
    return raw


def test_a_stream_from_before_a_move_away_and_back_takes_no_request(armed):
    """The race a review reproduced through the mock's own methods: desk's stream has
    passed its liveness check and waits on its queue when the source moves desk → laptop
    → desk. A frame it then takes is NOT written under the new generation: it goes back
    to the head of the queue, nothing is pending, and the stream ends; desk's next
    stream (admitted at the new generation) takes it."""
    mock = armed
    claim = mock._bus_try_claim(NS, DESK)
    token, end, gen = claim
    q = mock._bus_queue_for(NS)
    mock.know_machine(LAPTOP, "laptop")
    assert _use(mock, LAPTOP)[0] == 200
    assert _use(mock, DESK)[0] == 200
    mock._bus_release(NS, token)
    frame = json.dumps(_request("req-race"))
    assert mock._bus_take(NS, frame, DESK, gen, end, q) is False
    assert mock.pending() == []
    assert q.get_nowait() == frame, "back at the head of the queue"

    token2, end2, gen2 = mock._bus_try_claim(NS, DESK)
    assert gen2 != gen
    assert mock._bus_take(NS, frame, DESK, gen2, end2, q) is True
    assert mock.pending() == [(NS, "req-race")]
    mock._bus_release(NS, token2)


def _respond_raw(mock, headers: dict, in_reply_to: str = "req-1") -> tuple[int, dict]:
    c = _conn(mock)
    body = json.dumps({"id": "r1", "namespace": NS, "type": "response", "final": True,
                       "in_reply_to": in_reply_to, "shed": {"name": "web"}})
    c.request("POST", f"/api/plugins/listeners/{NS}/respond", body=body,
              headers=dict(headers, **{"Content-Type": "application/json"}))
    r = c.getresponse()
    raw = r.read()
    c.close()
    return r.status, (json.loads(raw) if raw else {})


def test_refusals_come_in_the_servers_order(armed):
    """Header shape, then size, then binding: a non-bound machine's oversized answer is
    413 (not 403 SOURCE_NOT_BOUND); an answer with no machine id is refused for that
    first, oversized or not."""
    mock = armed
    mock.know_machine(LAPTOP, "laptop")
    mock.respond_script(NS, [(413, None)])
    status, body = _respond(mock, LAPTOP)
    assert (status, body["error"]["code"]) == (413, "REQUEST_TOO_LARGE"), body
    status, body = _respond(mock, LAPTOP)
    assert (status, body["error"]["code"]) == (403, "SOURCE_NOT_BOUND"), body
    mock.respond_script(NS, [(413, None)])
    status, body = _respond_raw(mock, {})
    assert (status, body["error"]["code"]) == (403, "SOURCE_MACHINE_ID_REQUIRED"), body


def test_a_pause_forgets_the_pending_requests(armed):
    mock = armed
    _deliver(mock, DESK, "desk", "req-1")
    assert mock.pending() == [(NS, "req-1")]
    mock.pause()
    mock.resume()
    assert mock.pending() == []
    status, body = _respond(mock, DESK, "req-1")
    assert (status, body["error"]["code"]) == (403, "FORBIDDEN"), body
