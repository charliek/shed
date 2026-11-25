"""The mock shed-server's delivery routes (plan 039 A6), checked on their own wire.

`test_tauri_delivery.py` drives the app against these routes; here the mock speaks
HTTP to a hand-rolled source (no app), so a mock that drifted from the server's
contract (`tests/provision-contract/v2/CONTRACT.md` §3-4, `internal/api/
delivery_handlers.go`) fails here and not as a confusing app cell:

* the stream answers `200` with `text/event-stream`, `Cache-Control: no-store` and a
  32-hex `X-Shed-Delivery-Stream`, and writes each request as an `event: request`;
* missing machine headers are `403 SOURCE_MACHINE_ID_REQUIRED`; a second stream from
  the machine that holds one is `409 SOURCE_DUPLICATE_STREAM`;
* an answer is judged in the contract's order: `404 unknown_request`, `403
  not_stream_owner`, `409 already_answered`, `400 answer_refused` (`not_json`);
* when a stream ends its pending requests fail (`404`) and a new stream never
  carries them;
* the record served in the shed document is the contract's own fixture, and the
  provision and activity routes answer the Go handlers' shapes.
"""

from __future__ import annotations

import http.client
import json
import re
from pathlib import Path
from urllib.parse import urlsplit

import pytest

from mockserver import (DELIVERED_RECORD, DELIVERY_STREAM_HEADER, MACHINE_ID_HEADER,
                        MACHINE_NAME_HEADER, NONE_RECORD, MockShedServer)

MACHINE = "5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b"
OTHER = "fedcba9876543210fedcba9876543210"
CONTRACT = Path(__file__).resolve().parents[3] / "tests" / "provision-contract" / "v2"


@pytest.fixture
def dmock():
    """A private mock (not the session's `mock`)."""
    m = MockShedServer()
    m.start()
    try:
        yield m
    finally:
        m.stop()


def _conn(mock: MockShedServer) -> http.client.HTTPConnection:
    u = urlsplit(mock.base_url)
    return http.client.HTTPConnection(u.hostname, u.port, timeout=10)


def _headers(machine: str | None = MACHINE, stream: str | None = None) -> dict:
    h = {}
    if machine:
        h[MACHINE_ID_HEADER] = machine
        h[MACHINE_NAME_HEADER] = "ada%20laptop"
    if stream:
        h[DELIVERY_STREAM_HEADER] = stream
    return h


class Stream:
    """One open stream, read line by line."""

    def __init__(self, mock: MockShedServer, machine: str = MACHINE):
        self.conn = _conn(mock)
        self.conn.request("GET", "/api/delivery/requests", headers={
            **_headers(machine), "Accept": "text/event-stream"})
        self.resp = self.conn.getresponse()

    def next_request(self) -> dict:
        kind = None
        while True:
            line = self.resp.readline().decode().rstrip("\n")
            if line.startswith("event: "):
                kind = line[len("event: "):]
            elif line.startswith("data: ") and kind == "request":
                return json.loads(line[len("data: "):])

    def close(self):
        self.conn.close()


def _answer(mock, rid: str, body: bytes, machine: str = MACHINE, stream: str | None = None):
    c = _conn(mock)
    try:
        c.request("POST", f"/api/delivery/requests/{rid}", body=body,
                  headers={**_headers(machine, stream), "Content-Type": "application/json"})
        r = c.getresponse()
        raw = r.read()
        return r.status, (json.loads(raw) if raw else None), r.getheader("Cache-Control")
    finally:
        c.close()


def test_the_stream_and_an_answer_follow_the_contract(dmock):
    mock = dmock
    s = Stream(mock)
    try:
        assert s.resp.status == 200
        assert s.resp.getheader("Content-Type") == "text/event-stream"
        assert s.resp.getheader("Cache-Control") == "no-store"
        sid = s.resp.getheader(DELIVERY_STREAM_HEADER)
        assert re.fullmatch(r"[0-9a-f]{32}", sid)
        sent = mock.push_delivery_request(repo="example/widgets", backend="firecracker")
        got = s.next_request()
        assert got == sent and got["version"] == 2 and got["shed"]["repo"]["name"] == "example/widgets"
        rid = got["id"]
        body = json.dumps({"version": 2, "id": rid, "items": [], "problems": []}).encode()

        # In the contract's order: another stream, then the right one, then a repeat.
        assert _answer(mock, rid, body, stream="0" * 32)[0:2] == (
            403, {"error": {"code": "not_stream_owner",
                            "message": "the answer did not come on the request's stream"}})
        assert _answer(mock, rid, body, machine=OTHER, stream=sid)[0] == 403
        status, _, cache = _answer(mock, rid, body, stream=sid)
        assert (status, cache) == (204, "no-store")
        assert _answer(mock, rid, body, stream=sid)[1]["error"]["code"] == "already_answered"
        assert _answer(mock, "1" * 32, body, stream=sid)[1]["error"]["code"] == "unknown_request"
        posted = mock.await_delivery_answer(rid)
        assert posted["stream"] == sid and posted["machine_name"] == "ada laptop"
        assert posted["body"]["id"] == rid

        rid2 = mock.push_delivery_request()["id"]
        s.next_request()
        status, err, _ = _answer(mock, rid2, b"{not json", stream=sid)
        assert (status, err["error"]["code"], err["rule"]) == (400, "answer_refused", "not_json")
    finally:
        s.close()


def test_machine_headers_and_one_stream_per_machine(dmock):
    mock = dmock
    c = _conn(mock)
    c.request("GET", "/api/delivery/requests", headers={"Accept": "text/event-stream"})
    r = c.getresponse()
    assert (r.status, json.loads(r.read())["error"]["code"]) == (403, "SOURCE_MACHINE_ID_REQUIRED")
    c.close()
    s = Stream(mock)
    try:
        mock.live_delivery_stream()
        dup = Stream(mock)
        assert dup.resp.status == 409
        assert json.loads(dup.resp.read())["error"]["code"] == "SOURCE_DUPLICATE_STREAM"
        dup.close()
    finally:
        s.close()


def test_a_request_dies_with_its_stream_and_is_not_replayed(dmock):
    mock = dmock
    s = Stream(mock)
    sid = s.resp.getheader(DELIVERY_STREAM_HEADER)
    rid = mock.push_delivery_request()["id"]
    s.next_request()
    mock.end_delivery_stream()
    s.resp.read()  # the server closed it
    s.close()
    s2 = Stream(mock)
    try:
        sid2 = s2.resp.getheader(DELIVERY_STREAM_HEADER)
        assert sid2 != sid
        body = json.dumps({"version": 2, "id": rid, "items": [], "problems": []}).encode()
        for stream in (sid, sid2):
            assert _answer(mock, rid, body, stream=stream)[1]["error"]["code"] == "unknown_request"
        rid2 = mock.push_delivery_request()["id"]
        assert s2.next_request()["id"] == rid2, "the ended stream's request is not sent again"
    finally:
        s2.close()


def test_a_scripted_refusal_comes_first(dmock):
    mock = dmock
    mock.refuse_delivery_stream(403, "SOURCE_NOT_BOUND")
    s = Stream(mock)
    assert s.resp.status == 403
    assert json.loads(s.resp.read())["error"]["code"] == "SOURCE_NOT_BOUND"
    s.close()


def test_provision_activity_and_the_document(dmock):
    mock = dmock
    c = _conn(mock)
    c.request("GET", "/api/sheds/hello-world")
    doc = json.loads(c.getresponse().read())
    assert doc["delivery"] == DELIVERED_RECORD
    c.request("POST", "/api/sheds/hello-world/provision", headers={"Accept": "text/event-stream"})
    r = c.getresponse()
    text = r.read().decode()
    assert r.status == 200 and "event: progress" in text and "event: complete" in text
    c.request("POST", "/api/sheds/callbell/provision")
    r = c.getresponse()
    assert (r.status, json.loads(r.read())["error"]["code"]) == (409, "SHED_NOT_RUNNING")
    c.request("GET", "/api/activity?limit=2")
    page = json.loads(c.getresponse().read())
    assert [e["id"] for e in page["events"]] == ["c5", "c4"] and page["next"] == "c4"
    c.request("GET", "/api/activity?since=c4&limit=10&shed=hello-world")
    page = json.loads(c.getresponse().read())
    assert [e["id"] for e in page["events"]] == ["c3", "c1"] and page["next"] == ""
    c.close()


def test_an_activity_page_can_end_empty_with_a_cursor(dmock):
    """The server scans at most a budget of events per page; events this user may not
    see are scanned but not sent, so a page can be EMPTY and still carry a cursor."""
    dmock.activity_events = [{"id": f"e{n}", "at": f"2026-11-23T10:{n:02d}:00Z",
                              "kind": "shed.start", "shed": {"name": "w"},
                              "actor": {"kind": "user"}} for n in range(9, -1, -1)]
    dmock.activity_hidden = {"e9", "e8", "e7"}
    dmock.activity_scan_budget = 3
    c = _conn(dmock)
    c.request("GET", "/api/activity?limit=50")
    page = json.loads(c.getresponse().read())
    assert page == {"events": [], "next": "e7"}, page
    c.request("GET", "/api/activity?limit=50&since=e7")
    page = json.loads(c.getresponse().read())
    assert [e["id"] for e in page["events"]] == ["e6", "e5", "e4"] and page["next"] == "e4", page
    c.close()


@pytest.mark.skipif(not CONTRACT.is_dir(), reason="the provision contract is not in this tree")
def test_shapes_are_the_contracts_own():
    records = CONTRACT / "fixtures" / "records" / "accept"
    assert json.loads((records / "delivered.json").read_text())["record"] == DELIVERED_RECORD
    assert json.loads((records / "none.json").read_text())["record"] == NONE_RECORD
    for f in sorted((CONTRACT / "fixtures" / "stream").glob("*.json")):
        resp = json.loads(f.read_text())["response"]
        assert resp["headers"]["Content-Type"] == "text/event-stream", f.name
        assert resp["headers"]["Cache-Control"] == "no-store", f.name
        assert DELIVERY_STREAM_HEADER in resp["headers"], f.name
