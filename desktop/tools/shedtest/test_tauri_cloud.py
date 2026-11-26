"""Cloud sheds over IPC (plan 041 B1): a remote server whose sheds are machines of an
account service, each with a lease, driven through the real app against the mock's
remote fixture (`MockShedServer.serve_cloud`).

* `sheds.list` carries each cloud shed's `cloud` as the server sent it and its
  `cloud_row`, what the row shows and offers, decided in `shed-app::cloud` (the UI
  only draws it): the lease ok, soon, ended and unknown, two sizes, a service
  client's machine that is never extended. A shed of a server without cloud sheds
  has `cloud_row: null`.
* `shed.extend` moves a lease and answers the shed and its row; a lost decision (a
  503) is sent again within the call with its request id and moves the lease once;
  every send unanswered is `extend_unknown`, and the shed's next extends are refused
  here until a listing settles it; each refusal arrives as its code with its fixed
  words, never the server's text.
* `cloud.usage` answers a server's usage and its line, and a server without compute
  `FEATURE_UNSUPPORTED`.

The rendered rows (the tag, the meta line, the detail with Extend, the usage line,
Delete's confirmation) are `test_tauri_cloud_view.py`'s cells.
"""

from __future__ import annotations

import uuid
from datetime import datetime, timedelta

import pytest

from client import ShedError
from mockserver import EXTEND_REFUSAL_MESSAGE, EXTEND_REFUSALS, _parse_utc, cloud_shed


def _cloud_rows(client) -> dict[str, dict]:
    return {s["name"]: s for s in client.sheds_list()}


def _end(shed: dict) -> datetime:
    return _parse_utc(shed["cloud"]["lease"]["expires_at"])


def test_cloud_rows_carry_cloud_and_their_row(client, mock):
    sheds = _cloud_rows(client)
    assert all(s["cloud_row"] is None and "cloud" not in s for s in sheds.values()), sheds

    mock.serve_cloud()
    client.sheds_refresh()
    sheds = _cloud_rows(client)
    assert set(sheds) == {"cloud-ok", "cloud-soon", "cloud-ended", "cloud-unknown",
                          "cloud-stopped", "nocturn-cloud"}
    rows = {n: s["cloud_row"] for n, s in sheds.items()}
    assert all(r is not None and r["badge"] == "Cloud" for r in rows.values()), rows

    ok = rows["cloud-ok"]
    assert sheds["cloud-ok"]["cloud"]["size"]["name"] == "standard"
    assert ok["size_words"] == "2 vCPU · 8 GB"
    assert ok["lease_state"] == "ok"
    assert ok["lease_words"].startswith("deleted in 5h "), ok
    assert ok["run_words"].startswith("running for 3h "), ok
    assert ok["actions"] == {"start": False, "stop": True, "delete": False, "extend": True}
    assert ok["extend_presets_h"] == [6, 12, 24, 48]
    assert ok["image"] == sheds["cloud-ok"]["cloud"]["image"]

    soon = rows["cloud-soon"]
    assert soon["size_words"] == "8 vCPU · 32 GB"
    assert soon["lease_state"] == "soon", soon
    assert soon["lease_words"].startswith("deleted in 1h "), soon

    ended = rows["cloud-ended"]
    assert (ended["lease_state"], ended["lease_words"]) == ("ended", "lease ended")
    assert ended["actions"] == {"start": False, "stop": False, "delete": True, "extend": False}

    unknown = rows["cloud-unknown"]
    assert (unknown["lease_state"], unknown["lease_words"]) == ("unknown", "lease unknown")
    assert unknown["actions"]["extend"] is False

    stopped = rows["cloud-stopped"]
    assert stopped["actions"] == {"start": True, "stop": False, "delete": True, "extend": True}
    assert stopped["run_words"] is None

    client_machine = rows["nocturn-cloud"]
    assert client_machine["actions"]["extend"] is False
    assert client_machine["extend_presets_h"] == []

    # The dashboard still renders every row (the cloud UI itself is B2's).
    client.wait_until(lambda: {r["name"] for r in client.dashboard_rows()} == set(sheds),
                      timeout=10, what="cloud rows rendered")


def test_extend_moves_the_lease_once(client, mock):
    mock.serve_cloud()
    client.sheds_refresh()
    before = _end(_cloud_rows(client)["cloud-ok"])

    answer = client.call("shed.extend", {"name": "cloud-ok", "hours": 6})
    assert answer["shed"]["name"] == "cloud-ok"
    assert answer["shed"]["host"] == "mock"
    assert _end(answer["shed"]) == before + timedelta(hours=6)
    assert answer["cloud_row"]["lease_words"].startswith("deleted in 11h "), answer
    (sent,) = mock.extend_requests()
    assert sent["hours"] == 6 and uuid.UUID(sent["request_id"]), sent
    assert _end(_cloud_rows(client)["cloud-ok"]) == before + timedelta(hours=6)

    # A 503 decided nothing: the extend is sent again, with its id, and the lease
    # moves once.
    mock.refuse_extend("COMPUTE_UNAVAILABLE")
    answer = client.call("shed.extend", {"name": "cloud-ok", "hours": 12, "host": "mock"})
    assert _end(answer["shed"]) == before + timedelta(hours=18)
    first, again = mock.extend_requests()[1:]
    assert first["request_id"] == again["request_id"], (first, again)


@pytest.mark.parametrize("code", sorted(EXTEND_REFUSALS))
def test_an_extend_refusal_has_its_code_and_fixed_words(client, mock, code):
    mock.serve_cloud()
    # A 5xx is sent again: refuse every send. Then no send was answered, so whether the
    # lease moved is not known: `extend_unknown`, with its words.
    unknown = EXTEND_REFUSALS[code] >= 500
    for _ in range(3 if unknown else 1):
        mock.refuse_extend(code, "person" if code == "PERSON_INACTIVE" else "")
    with pytest.raises(ShedError) as e:
        client.call("shed.extend", {"name": "cloud-ok", "hours": 6})
    assert e.value.code == ("extend_unknown" if unknown else code)
    assert e.value.message and EXTEND_REFUSAL_MESSAGE not in e.value.message, e.value.message
    assert len(mock.extend_requests()) == (3 if unknown else 1)
    if unknown:
        assert e.value.message == "not known whether it was applied; Extend stays unavailable until that is known"
        # Until a listing settles it, every extend of the shed is refused here, unsent.
        with pytest.raises(ShedError) as e:
            client.call("shed.extend", {"name": "cloud-ok", "hours": 6})
        assert e.value.code == "extend_pending"
        assert len(mock.extend_requests()) == 3
        # A listing without the shed settles it (no later cell inherits it).
        mock.set_sheds([])
        client.call("sheds.list")
        mock.serve_cloud()
        client.call("shed.extend", {"name": "cloud-ok", "hours": 6})


def test_extend_refusals_the_mock_decides(client, mock):
    mock.serve_cloud()
    cases = [
        ("cloud-ok", 720, "LEASE_AT_CAP", "past its cap"),
        ("cloud-ended", 6, "LEASE_ENDED", "has ended"),
        ("nocturn-cloud", 6, "EXTEND_NOT_ALLOWED", "cannot be extended"),
    ]
    for name, hours, code, words in cases:
        with pytest.raises(ShedError) as e:
            client.call("shed.extend", {"name": name, "hours": hours})
        assert (e.value.code, words in e.value.message) == (code, True), (name, e.value)
    for bad in [0, 721, "6", None]:
        with pytest.raises(ShedError) as e:
            client.call("shed.extend", {"name": "cloud-ok", "hours": bad})
        assert e.value.code == "bad_request", bad
    assert len(mock.extend_requests()) == len(cases), "a bad request is never sent"


def test_cloud_usage(client, mock):
    with pytest.raises(ShedError) as e:
        client.call("cloud.usage", {"host": "mock"})
    assert e.value.code == "FEATURE_UNSUPPORTED"
    mock.serve_cloud()
    u = client.call("cloud.usage", {"host": "mock"})
    assert u["host"] == "mock"
    assert u["usage"]["person"]["machines"] == 2
    assert u["words"] == "2 of 100 machines (1 running, 1 stopped) · organization 2 of 100"
    with pytest.raises(ShedError) as e:
        client.call("cloud.usage", {"host": "elsewhere"})
    assert e.value.code == "action_failed"


def _post_extend(mock, name: str, body, caller: str | None = None) -> tuple[int, dict]:
    """POST an extend to the mock directly (no app), as `caller` (its credential)."""
    import http.client
    import json
    from urllib.parse import urlsplit

    u = urlsplit(mock.base_url)
    conn = http.client.HTTPConnection(u.hostname, u.port, timeout=10)
    headers = {"Content-Type": "application/json"}
    if caller:
        headers["Authorization"] = f"Bearer {caller}"
    try:
        conn.request("POST", f"/api/sheds/{name}/extend", json.dumps(body), headers)
        resp = conn.getresponse()
        return resp.status, json.loads(resp.read() or b"{}")
    finally:
        conn.close()


def test_the_mock_keeps_an_answer_per_caller_shed_and_id(mock):
    """The server derives the service's request id from the caller's id, the shed and
    the caller: the same id for another shed, or from another caller, is a new
    extend; the same three again is the first answer, the lease moved once."""
    mock.serve_cloud()
    end = {s["name"]: _end(s) for s in mock.snapshot()[1] if s["cloud"].get("lease")}
    rid = str(uuid.uuid4())

    status, ok = _post_extend(mock, "cloud-ok", {"hours": 6, "request_id": rid}, "ada")
    assert status == 200 and ok["name"] == "cloud-ok"
    status, stopped = _post_extend(mock, "cloud-stopped", {"hours": 6, "request_id": rid}, "ada")
    assert status == 200 and stopped["name"] == "cloud-stopped", stopped
    assert _end(stopped) == end["cloud-stopped"] + timedelta(hours=6)
    status, again = _post_extend(mock, "cloud-ok", {"hours": 6, "request_id": rid}, "ada")
    assert (status, again) == (200, ok), "a retry is the first answer"
    status, other = _post_extend(mock, "cloud-ok", {"hours": 6, "request_id": rid}, "grace")
    assert status == 200 and _end(other) == end["cloud-ok"] + timedelta(hours=12), other
    now = {s["name"]: _end(s) for s in mock.snapshot()[1] if s["cloud"].get("lease")}
    assert now["cloud-ok"] == end["cloud-ok"] + timedelta(hours=12)
    assert now["cloud-stopped"] == end["cloud-stopped"] + timedelta(hours=6)


@pytest.mark.parametrize("rid", ["invalid", ["invalid"], 7, {"id": "x"},
                                 "6f9619ff-8b86-4d01-b42d-00cf4fc964f"])
def test_the_mock_refuses_a_request_id_that_is_not_a_uuid(mock, rid):
    mock.serve_cloud()
    status, body = _post_extend(mock, "cloud-ok", {"hours": 6, "request_id": rid})
    assert status == 400 and body["error"]["code"] == "INVALID_REQUEST", (status, body)
    assert mock.extend_requests() == [], "a refused body is never an extend"
    # An empty or null id is no id (each POST a new extend), as on the server; either
    # case of hex is a UUID.
    for ok_rid in ["", None, str(uuid.uuid4()).upper()]:
        status, _ = _post_extend(mock, "cloud-ok", {"hours": 1, "request_id": ok_rid})
        assert status == 200, ok_rid


def test_the_mock_compares_request_ids_lowercased(mock):
    """The server lowercases the caller's id: the same UUID spelled in upper case is a
    retry, not a second extend."""
    mock.serve_cloud()
    end = {s["name"]: _end(s) for s in mock.snapshot()[1] if s["cloud"].get("lease")}
    rid = str(uuid.uuid4())
    status, first = _post_extend(mock, "cloud-ok", {"hours": 6, "request_id": rid}, "ada")
    assert status == 200
    status, again = _post_extend(mock, "cloud-ok", {"hours": 6, "request_id": rid.upper()}, "ada")
    assert (status, again) == (200, first)
    assert _end(mock.shed("cloud-ok")) == end["cloud-ok"] + timedelta(hours=6)


def test_the_mock_keeps_an_answer_per_shed_incarnation(mock):
    """A shed deleted and created again under its name is another shed: the same caller's
    same id extends the new one, and never answers with the old one."""
    mock.serve_cloud()
    rid = str(uuid.uuid4())
    status, old = _post_extend(mock, "cloud-ok", {"hours": 6, "request_id": rid}, "ada")
    assert status == 200
    mock.set_sheds([s for s in mock.snapshot()[1] if s["name"] != "cloud-ok"]
                   + [cloud_shed("cloud-ok", "running", left_s=5 * 3600)])
    fresh = mock.shed("cloud-ok")
    fresh_end = _end(fresh)  # before the extend: `shed()`'s copy shares its lease
    assert fresh["id"] != old["id"]
    status, new = _post_extend(mock, "cloud-ok", {"hours": 6, "request_id": rid}, "ada")
    assert status == 200 and new["id"] == fresh["id"], (new["id"], old["id"])
    assert _end(new) == fresh_end + timedelta(hours=6)
    # A name the server does not have is 404 before any answer is looked up.
    status, body = _post_extend(mock, "gone", {"hours": 6, "request_id": rid}, "ada")
    assert (status, body["error"]["code"]) == (404, "SHED_NOT_FOUND")
