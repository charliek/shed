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
  newline is malformed (400), not an unseen machine (404);
* `503 SOURCE_BUSY` carries `Retry-After`.
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


def _respond(mock, machine_id: str) -> tuple[int, dict]:
    c = _conn(mock)
    body = json.dumps({"id": "r1", "namespace": NS, "type": "response", "final": True,
                       "shed": {"name": "web"}})
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
    assert _respond(mock, DESK)[0] == 204, "the source answers"
    mock.know_machine(LAPTOP, "laptop")
    assert _use(mock, LAPTOP)[0] == 200
    status, body = _respond(mock, DESK)
    assert status == 403, body
    assert body["error"]["code"] == "SOURCE_NOT_BOUND"
    assert body["source"]["machine_id"] == LAPTOP
    assert _respond(mock, LAPTOP)[0] == 204
    assert len(mock.respond_identities(NS)) == 3, "refused ones are still recorded"


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
