"""The mock shed-server's source binding, checked on its own wire.

`MockShedServer.arm_source` stands in for a pinning shed-server (plan 035 §3.4, the
binding judged on the delivery stream since plan 039) in the cells of
`test_tauri_source.py`. Those cells drive it through the real app, which only ever
holds one machine's stream, so the parts of the contract that concern ANOTHER
machine's stream — what a move does to it — would pass there however wrong the mock
got them. These cells speak HTTP to the mock directly (no app), as two machines:

* a stream from a machine that is not the bound one is refused 403
  SOURCE_NOT_BOUND naming the bound machine, and is still recorded as seen;
* a move ends every other machine's stream (its pending requests fail with it), so
  the new source is admitted, and the old machine's answers are refused 403
  SOURCE_NOT_BOUND — one in flight across the move included;
* that happens AT the move, not when the old stream's handler gets round to closing:
  after a move away and back (A → B → A) with A's old connection still open, A's
  answer on its old stream is refused (the request no longer waits), nothing can be
  pushed onto the old stream, and A's new stream is admitted, not a duplicate;
* the source document says which machine holds a stream (`connected`);
* the bound machine opening a second stream is the retryable 409
  SOURCE_DUPLICATE_STREAM;
* a machine id must be exactly 32 lowercase hex characters — one followed by a
  newline is malformed (400 INVALID_REQUEST), not an unseen machine (404);
* `503 SOURCE_BUSY` carries `Retry-After`;
* the move's refusal codes are the server's own (an unconfirmed move is 500
  INTERNAL_ERROR);
* `pause()` / `resume()` take the server down and back on the same port, ending
  every stream (and so failing its pending requests);
* for a user the account service answers for (plan 040 §3.2) every source route —
  the stream, an answer, a move, an unbind — is `409 SOURCE_IS_SERVICE` with the
  shared fixture's body, and when it stops, the first machine to connect is bound;
* an unbind forgets the binding and the remembered machines and ends the streams, and
  the first machine to connect next is bound again (the server's first-connect rule).
"""

from __future__ import annotations

import http.client
import json
import time
from urllib.parse import urlsplit

import pytest

from client import scaled_timeout
from mockserver import (DELIVERY_STREAM_HEADER, MACHINE_ID_HEADER, MACHINE_NAME_HEADER,
                        MockShedServer)

DESK = "fedcba9876543210fedcba9876543210"
LAPTOP = "0123456789abcdef0123456789abcdef"


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


def _stream(mock, machine_id: str, name: str):
    """Open the delivery stream as `machine_id`; the connection and its response."""
    c = _conn(mock)
    c.request("GET", "/api/delivery/requests",
              headers={MACHINE_ID_HEADER: machine_id, MACHINE_NAME_HEADER: name,
                       "Accept": "text/event-stream"})
    return c, c.getresponse()


def _read_request(resp) -> dict:
    kind = None
    while True:
        line = resp.readline()
        assert line, "the stream ended before the request"
        line = line.decode().rstrip("\n")
        if line.startswith("event: "):
            kind = line[len("event: "):]
        elif line.startswith("data: ") and kind == "request":
            return json.loads(line[len("data: "):])


def _answer(mock, machine_id: str, rid: str, stream: str) -> tuple[int, dict]:
    c = _conn(mock)
    c.request("POST", f"/api/delivery/requests/{rid}", body=b'{"version":2,"items":[]}',
              headers={MACHINE_ID_HEADER: machine_id, MACHINE_NAME_HEADER: "m",
                       DELIVERY_STREAM_HEADER: stream, "Content-Type": "application/json"})
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


def _unbind(mock, body: object = None) -> tuple[int, object]:
    c = _conn(mock)
    raw = b"" if body is None else json.dumps(body).encode()
    c.request("POST", "/api/sources/unbind", body=raw,
              headers={"Content-Type": "application/json"})
    r = c.getresponse()
    data = r.read()
    c.close()
    try:
        return r.status, json.loads(data)
    except ValueError:
        return r.status, data.decode()


def _get(mock, path: str) -> dict:
    c = _conn(mock)
    c.request("GET", path)
    r = c.getresponse()
    assert r.status == 200, r.status
    raw = r.read()
    c.close()
    return json.loads(raw)


def _wait(pred, what: str, timeout: float = 5.0) -> None:
    deadline = time.monotonic() + scaled_timeout(timeout)
    while not pred():
        if time.monotonic() > deadline:
            raise AssertionError(f"timed out waiting for {what}")
        time.sleep(0.02)


def _connected(mock) -> dict[str, bool]:
    return {m["id"]: m["connected"] for m in _get(mock, "/api/sources")["machines"]}


def test_a_stream_from_another_machine_is_refused_and_a_move_admits_it(armed):
    mock = armed
    c, refused = _stream(mock, LAPTOP, "laptop")
    assert refused.status == 403
    body = json.loads(refused.read())
    assert body["error"]["code"] == "SOURCE_NOT_BOUND"
    assert body["source"] == {"machine_id": DESK, "machine_name": "desk"}
    c.close()
    assert mock.delivery_streams() == []
    # Refused, but seen: the move to it is accepted (an unseen machine is 404).
    assert _use(mock, LAPTOP)[0] == 200
    c, admitted = _stream(mock, LAPTOP, "laptop")
    assert admitted.status == 200
    _wait(lambda: _connected(mock).get(LAPTOP) is True, "laptop connected")
    c.close()


def test_a_move_ends_the_old_sources_stream_and_refuses_its_answers(armed):
    mock = armed
    old_conn, old = _stream(mock, DESK, "desk")
    assert old.status == 200
    stream = old.getheader(DELIVERY_STREAM_HEADER)
    req = mock.push_delivery_request()
    assert _read_request(old)["id"] == req["id"]
    mock.know_machine(LAPTOP, "laptop")
    assert _use(mock, LAPTOP)[0] == 200
    # desk's stream ends: its body reaches EOF, and the request it held failed.
    assert old.read().strip() == b"", "nothing more on the stream before its end"
    old_conn.close()
    _wait(lambda: not mock._dlv_pending, "the old stream's request failed")
    status, body = _answer(mock, DESK, req["id"], stream)
    assert status == 403, body
    assert body["error"]["code"] == "SOURCE_NOT_BOUND"
    assert body["source"]["machine_id"] == LAPTOP
    # The new source is admitted and served.
    new_conn, new = _stream(mock, LAPTOP, "laptop")
    assert new.status == 200
    new_conn.close()


def test_a_move_away_and_back_fails_the_old_streams_requests_at_once(armed):
    """Deterministic: the old connection stays open throughout, so nothing here
    waits on its handler closing."""
    mock = armed
    old_conn, old = _stream(mock, DESK, "desk")
    assert old.status == 200
    old_stream = old.getheader(DELIVERY_STREAM_HEADER)
    req = mock.push_delivery_request()
    assert _read_request(old)["id"] == req["id"]
    mock.know_machine(LAPTOP, "laptop")
    try:
        assert _use(mock, LAPTOP)[0] == 200
        assert _use(mock, DESK)[0] == 200
        assert mock.source_bound() == DESK
        # At once: the request is gone and the old stream is no longer live.
        assert not mock._dlv_pending, mock._dlv_pending
        with pytest.raises(TimeoutError):
            mock.live_delivery_stream(timeout=0.2, after=len(mock.delivery_streams()))
        status, body = _answer(mock, DESK, req["id"], old_stream)
        assert (status, body["error"]["code"]) == (404, "unknown_request"), body
        # A new stream for desk is admitted while the old connection is still open.
        new_conn, new = _stream(mock, DESK, "desk")
        assert new.status == 200, new.read()
        new_stream = new.getheader(DELIVERY_STREAM_HEADER)
        assert new_stream != old_stream
        # A request goes to the new stream only, and an answer on the old one is not
        # its owner's.
        req2 = mock.push_delivery_request()
        assert _read_request(new)["id"] == req2["id"]
        status, body = _answer(mock, DESK, req2["id"], old_stream)
        assert (status, body["error"]["code"]) == (403, "not_stream_owner"), body
        assert _answer(mock, DESK, req2["id"], new_stream)[0] == 204
        new_conn.close()
    finally:
        old_conn.close()


def test_the_bound_machine_holding_a_stream_is_a_duplicate(armed):
    mock = armed
    c1, first = _stream(mock, DESK, "desk")
    assert first.status == 200
    _wait(lambda: _connected(mock).get(DESK) is True, "desk connected")
    c2, second = _stream(mock, DESK, "desk")
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
    assert _get(mock, "/api/sources")["binding"] is not None


def test_pause_takes_the_server_down_and_ends_every_stream(armed):
    mock = armed
    url = mock.base_url
    c, held = _stream(mock, DESK, "desk")
    assert held.status == 200
    mock.push_delivery_request()
    mock.pause()
    with pytest.raises(OSError):
        _get(mock, "/api/sources")
    _wait(lambda: not mock._dlv_pending, "the pending request failed with its stream")
    mock.resume()
    c.close()
    assert mock.base_url == url, "the same port"
    assert _get(mock, "/api/sources")["binding"] is not None


@pytest.mark.parametrize("state", ["", "not_delivering"])
def test_a_service_source_serves_the_shared_shapes(state):
    """A directory user's `GET /api/sources` is the shared fixture (`crates/fixtures/
    sources/`, which the server's API tests and shed-core read too): the account service
    in the delivery slot, with `not_delivering` when the server says so, and no machine."""
    from mockserver import (SERVICE_NOT_DELIVERING_DOCUMENT, SERVICE_SOURCE_DOCUMENT,
                            SOURCE_IS_SERVICE_BODY)
    mock = MockShedServer()
    mock.start()
    try:
        mock.serve_service_source(state)
        want = SERVICE_NOT_DELIVERING_DOCUMENT if state else SERVICE_SOURCE_DOCUMENT
        assert _get(mock, "/api/sources") == want
        assert want["binding"]["delivery"][0]["kind"] == "service"
        assert SOURCE_IS_SERVICE_BODY["error"]["code"] == "SOURCE_IS_SERVICE"
    finally:
        mock.stop()


def test_every_source_route_refuses_a_service_user_then_first_connect_binds():
    """While the account service answers for the user, the stream, an answer, a move
    and an unbind are each `409 SOURCE_IS_SERVICE` with the shared body, and a stream
    that was live when it began ends. When it stops, no machine is bound: the first
    stream is admitted and bound."""
    from mockserver import SOURCE_IS_SERVICE_BODY
    mock = MockShedServer()
    mock.start()
    try:
        mock.arm_source(DESK, "desk")
        c_live, live = _stream(mock, DESK, "desk")
        assert live.status == 200
        mock.serve_service_source()
        _wait(lambda: not mock._dlv_live, "the live stream ended")
        c_live.close()
        for mid in (DESK, LAPTOP):
            c, refused = _stream(mock, mid, "m")
            assert refused.status == 409, refused.status
            assert json.loads(refused.read()) == SOURCE_IS_SERVICE_BODY
            c.close()
        assert _answer(mock, DESK, "ab" * 16, "cd" * 16) == (409, SOURCE_IS_SERVICE_BODY)
        assert _use(mock, DESK) == (409, SOURCE_IS_SERVICE_BODY)
        assert _unbind(mock) == (409, SOURCE_IS_SERVICE_BODY)
        assert mock.source_unbind_requests() == [{}]
        doc = _get(mock, "/api/sources")
        assert doc["binding"]["delivery"][0]["kind"] == "service", doc

        mock.stop_service_source()
        assert _get(mock, "/api/sources")["binding"] is None
        c, admitted = _stream(mock, LAPTOP, "laptop")
        assert admitted.status == 200, admitted.status
        assert mock.source_bound() == LAPTOP
        c.close()
    finally:
        mock.stop()


def test_an_unbind_forgets_the_source_and_the_next_machine_is_bound(armed):
    mock = armed
    c_live, live = _stream(mock, DESK, "desk")
    assert live.status == 200
    status, body = _unbind(mock)
    assert status == 200, body
    assert body == {"user": "mock-user", "unbound": True,
                    "previous": {"id": DESK, "name": "desk"}, "forgotten_machines": 1}
    assert mock.source_bound() is None
    _wait(lambda: not mock._dlv_live, "the user's stream ended")
    c_live.close()
    assert _get(mock, "/api/sources") == {"user": "mock-user", "binding": None, "machines": []}
    # Nothing left to clear: a success that changed nothing.
    assert _unbind(mock, {}) == (200, {"user": "mock-user", "unbound": False,
                                       "previous": None, "forgotten_machines": 0})
    # A compare-and-clear naming a machine, with nothing bound: also a no-op success.
    assert _unbind(mock, {"machine_id": DESK}) == (200, {"user": "mock-user", "unbound": False,
                                                         "previous": None, "forgotten_machines": 0})
    # First-connect: the next machine's stream is admitted and bound.
    c, admitted = _stream(mock, LAPTOP, "laptop")
    assert admitted.status == 200
    assert mock.source_bound() == LAPTOP
    c.close()
    assert len(mock.source_unbind_requests()) == 3


def test_an_unarmed_server_has_no_unbind_route():
    mock = MockShedServer()
    mock.start()
    try:
        status, body = _unbind(mock)
        assert (status, body) == (404, "404 page not found\n")
    finally:
        mock.stop()
