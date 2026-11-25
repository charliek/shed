"""The account service as a credential source, live (plan 040 §6).

On a server whose users come from an account service, a directory user's
delivery is asked of the service (the provision contract's hosted source,
`POST /v1/deliveries`) and of nothing else; a local admin keeps the machine
they pinned. The account service here is the directory contract's reference
server (`bin/test/shed-directory-ref`), told what to answer through its
control directory (`fixtures/directory.py: DeliveriesControl`):
`deliveries.json` (each person's canned items, `NO_SOURCE`, or a failure
mode: silence, a status and body, a truncated or oversize body, an echo of
the bearer, any body), `no-deliveries` (the servers that may not ask) and
`outage`. It logs one `delivery call` line per call and one line per token
request, which the cases count.

**Two places, by what a case writes** (s0 ruling 4). The self-managed
harness of `test_directory.py` runs a private shed-server on the remote
backend's local drivers, whose "shed" is a real `shed-agent` on this host:
its `apply-items` would write into the home of the user running the tests.
So only the cases that write nothing run there, and the harness refuses to
serve anything the server could accept (`DeliveriesControl(local=True)`):

  2. `NO_SOURCE` (a listed entry and a person missing from the file); a
     token request refused `invalid_scope`; `403 INSUFFICIENT_SCOPE` on the
     call. Each `not_applicable`, no warning, a create like one with no
     source; the five-minute memory (no token request, no call, `shed source
     status` says "not delivering", the sources document `not_delivering`),
     cleared by `shed users refresh`.
  5. Criterion 2: a local admin (also a person in the service's list) is
     answered by their pinned machine (the reference source answering a
     refuse fixture: nothing written); moved into the directory, the
     server's next start removes the binding (logged, gone from
     `source-bindings.json`), `shed source status` names the service,
     `shed source use`/`unbind`, `POST /api/sources/use`/`unbind`, both
     delivery-stream routes and the bus's listener routes answer `409
     SOURCE_IS_SERVICE` for the user and for an admin naming them; a new
     shed goes to the service; moved back, a new shed gets no source. (A
     shed made before the move cannot outlive the restart here: the fake
     cloud provider keeps its instances in memory, and the server drops a
     record with no instance. `409 SHED_ORIGIN_CHANGED` is held by the Go
     tests and checked by hand at the checkpoint.) The choice holds for a create
     whose caller has gone (a raw API create on the remote backend's async
     job) and, at the routes, after the directory's trust time has ended.
  7. A service client's shed and another user's shed: no request to the
     service, and no provision by them; a script key's shed cannot exist on
     a directory server (a script key on a local admin fails config load);
     the owner's own create and provision ask (the control).

Run them with `make test-integration-directory` (Linux; it builds `bin/`,
the reference server and the reference source, and starts everything on
loopback). Only these: `make test-integration-directory
DIRECTORY_ITEST_FILES=test_delivery_hosted.py`.

The cases that write (1, 3, 4, 6, 8) run on real backends only, under
`SHED_ITEST_SOURCE=directory`, against a server the run started with
`render-directory`'s auth block and enrolled with a reference server the run
started, whose control directory and standard-error file are named by
`SHED_ITEST_DIRECTORY_CONTROL` and `SHED_ITEST_DIRECTORY_LOG`, on a backend
`SHED_IMAGE_HAS_DELIVERY` lists. Anywhere else they skip, saying what to set:

  1. a create for alice with the service's canned items: every file with its
     mode and content, the env values byte for byte (a new command and a new
     SSH session), the stage order around the clone, `last.source` `{"kind":
     "service"}`, one call; `shed provision --status` says "from the account
     service";
  3. each failure of criterion 4 the reference server can play (500, 503,
     429, an outage, silence past the deadline, 410, a reply closed
     mid-body, 401 twice, an unlisted status, a 404 without `NO_SOURCE`, a
     redirect, a 200 over 6 MiB, a 200 echoing the bearer): the reason, the
     warning naming `shed provision`, the step states, no request made in
     the background; after EACH failure, `shed provision` with the service
     healthy finishes the delivery (every step, the items, the bundle in the
     shed) before the next failure is played. A shed deleted during a silent reply ends its provision at
     once. (The service's port closed is not played here: the reference
     server is the run's; the Go tests and the checkpoint cover it.)
  4. every live-runnable refuse fixture of the provision contract as a 200
     body (one per rule, at least one; all with `SHED_DELIVERY_REFUSE_ALL=1`),
     rendered by the reference source itself in the fixture language
     (`answer`, `answer_text`, `answer_base64`, `fill`; raw bytes kept):
     refused with its rule, the record's items untouched, no file in the
     shed newer;
  6. the sentinel sweep: no delivered value, no text of the service's (an
     error message, a redirect target) in any kept output, the server's log
     since the module began, the audit log or the state directories; a
     problem's message only in `last.problems` of the record shown to the
     user (those fields are taken out structurally, then everything is
     searched for it too). The case sends each sentinel through the server
     itself on a local backend; on [remote] it asserts case 3 did;
  8. a shed alice made on a v0.11 server binary, listed by this one, and
     delivered to by `shed provision` (`SHED_HOSTED_V011_SHED` names it; the
     run makes it before upgrading the server; unset, the case skips).

[remote] runs cases 1, 3's create and provision, and 6 (two VMs, each
deleted).

**Not live here, on purpose.** A delivery to a shed made before its user's
name moved between the local admins and the directory (`409
SHED_ORIGIN_CHANGED`) needs a persistent real backend, a config change and a
restart: it is checked by hand at the checkpoint (§7, criterion 2 by hand),
and the Go tests hold it on all three backends. The five-minute expiry of
the not-delivering memory is held by the Go tests with a moved clock; waiting
five minutes here is not worth it (case 2 checks the memory and its clearing
by `shed users refresh`).

**Negative control, §6.** "With the choice of §3.2 forced to local, case 1
must fail": the server has no switch for the choice, and none is added for a
test; the control is the Go test of A3 that forces the choice to local
(`internal/delivery`, run with the whole of `go test ./internal/delivery/`).
With it forced so, case 1 here would record a machine source or none, and
its `last.source == {"kind": "service"}` assertion fails.
"""

from __future__ import annotations

import json
import os
import subprocess
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from types import SimpleNamespace
from typing import Any, Optional

import pytest
import yaml

from fixtures import delivery as dl
from fixtures.busclient import FakeMachine, ensure_source
from fixtures.directory import (
    ASKING_THE_SERVICE,
    BINDING_REMOVED,
    ENTRY,
    HOST,
    LOCAL_ADMIN,
    NO_SOURCE,
    TRUST_SECONDS,
    TRUST_TICK,
    DeliveriesControl,
    World,
    delivery_answer,
    delivery_body,
    delivery_status,
    directory_world,
    hosted_control,
    hosted_skip_reason,
    skip_reason,
    with_local_admin,
    with_service_client,
    without_local_admin,
)
from fixtures.identity import ALICE, Identity, UsersEnv
from fixtures.tlsclient import Pinned, error_code, pinned_request
from test_delivery import REPO, REPO_DIR, STAGE1_FILE, _bash, _check_bundle, _exec, _written_at, standard_items
from test_users import _delete, _timeout

# The server's delivery.timeout default, which is a hosted request's deadline.
DELIVERY_TIMEOUT = 30
# The config's own words (internal/config/types.go SourceIsServiceMessage).
SOURCE_IS_SERVICE_TEXT = "the account service is this user's credential source on this server: no machine answers here"
NOT_DELIVERING_TEXT = "The account service is not delivering credentials to this server"
# internal/config: a script key on a local admin beside auth.directory.
SCRIPT_KEY_REFUSED = "a local admin beside auth.directory has no script keys"
SERVICE = {"kind": "service"}
# How long a case waits to see that nothing was asked in the background.
QUIET = 6

_LOCAL = skip_reason()
local_harness = pytest.mark.skipif(_LOCAL is not None, reason=_LOCAL or "")
_REAL = hosted_skip_reason(None)
real_backend = pytest.mark.skipif(_REAL is not None, reason=_REAL or "")


def _uid() -> str:
    return uuid.uuid4().hex[:6]


def _not_applicable(rec: dict, what: str, *, repo: bool = False) -> None:
    """A record with no source: as a create with none makes it."""
    assert rec.get("state") == "not_applicable", f"{what}: {rec}"
    want = {"stage1": ("skipped", "not_applicable"), "stage2": ("skipped", "not_applicable"),
            "clone": ("done", None) if repo else ("skipped", "no_repo")}
    problems = dl.step_problems(rec, want)
    assert not problems, f"{what}: {problems}: {rec}"
    last = rec.get("last") or {}
    assert last.get("outcome") == "not_requested" and "failure" not in last and "source" not in last, f"{what}: {last}"
    assert not rec.get("items") and not rec.get("owed"), f"{what}: {rec}"


def _shape(rec: dict) -> dict:
    """A record without its times, for comparing two creates."""
    r = dl.without_times(rec)
    return {k: r.get(k) for k in ("state", "steps", "last", "items", "owed")}


# ============================================================================
# The self-managed harness: cases 2, 5 and 7 (nothing written)
# ============================================================================


@pytest.fixture
def world():
    with directory_world() as w:
        yield w


def _person(w: World, name: str):
    """A person in the service's list, with their own key and CLI entry (the
    list is pulled first, so no login waits on the unknown-key pull)."""
    k = w.key(name)
    w.set_user(name, [k])
    w.publish()
    w.refresh_ok()
    who = w.identity(name, k)
    who.add_server()
    return who


def _doc(w: World, who, name: str) -> dict:
    a = w.api(who, f"/api/sheds/{name}")
    assert a.status == 200 and isinstance(a.body, dict), f"{who.user}: GET {name}: {a}"
    return a.body


def _settled(get, name: str, timeout: float = 90) -> dict:
    """The shed's delivery record once a delivery has been recorded (a
    remote create answers before its job has run)."""
    deadline = time.monotonic() + timeout
    while True:
        doc = get()
        rec = (doc or {}).get("delivery") or {}
        if rec.get("state") not in (None, "none") and rec.get("last"):
            return rec
        if time.monotonic() > deadline:
            raise AssertionError(f"{name}: no delivery recorded within {timeout:.0f} s: {doc}")
        time.sleep(1)


def _create_local(w: World, who, name: str) -> tuple[subprocess.CompletedProcess, dict]:
    r = who.cli("create", name, "--no-sync", timeout=180)
    assert r.returncode == 0, f"{who.user}: shed create {name}: {r.stdout[-600:]!r} {r.stderr[-600:]!r}"
    return r, _settled(lambda: _doc(w, who, name), name)


def _asks(w: World, name: str) -> int:
    """The hosted requests the server has sent for shed `name` (this start's
    log)."""
    return sum(1 for ln in w.server.log().splitlines() if ASKING_THE_SERVICE in ln and f'for shed "{name}"' in ln)


def _source_status(who, *args: str) -> dict:
    r = who.shed("--json", "-s", ENTRY, "source", "status", *args, timeout=60)
    assert r.returncode == 0, f"{who.user}: shed source status {args}: {r.stderr!r}"
    return json.loads(r.stdout)


def _counts(w: World) -> tuple[int, int]:
    return w.ref.deliveries.counts()


@local_harness
def test_no_source_is_not_applicable_and_silent(world):
    """Case 2, NO_SOURCE: a person the file names `NO_SOURCE` and one it
    does not name each get a create like one with no source at all (the
    local admin's, who has no machine): no warning, `not_applicable`, no
    source recorded. One call each; one token request for the delivery
    scope in all (the token is kept)."""
    w = world
    dana, eve = _person(w, "dana"), _person(w, "eve")
    w.ref.deliveries.set({"dana": NO_SOURCE})
    _, base = _create_local(w, w.admin, f"lab-{_uid()}")
    _not_applicable(base, "the local admin's create")

    calls, toks = _counts(w)
    for who, n in ((dana, 1), (eve, 2)):
        name = f"{who.user}-{_uid()}"
        r, rec = _create_local(w, who, name)
        assert dl.delivery_warnings(r.stdout + r.stderr) == [], r.stderr
        _not_applicable(rec, name)
        assert _shape(rec) == _shape(base), f"{name}: not a create with no source:\n{rec}\n{base}"
        assert _asks(w, name) == 1, f"{name}: the server did not ask the service once"
        assert _counts(w) == (calls + n, toks + 1), (f"{name}: calls and delivery token requests {_counts(w)}, "
                                                    f"want {(calls + n, toks + 1)}")
    st = _source_status(dana)
    assert st.get("source") is None and st.get("service") == {"url": w.ref.issuer, "delivering": True}, st


def _assert_not_delivering(w: World, who) -> None:
    st = _source_status(who)
    assert st.get("service") == {"url": w.ref.issuer, "delivering": False}, st
    r = who.cli("source", "status", timeout=60)
    assert r.returncode == 0 and NOT_DELIVERING_TEXT in r.stdout, r.stdout
    for api, path in ((who.api(), "/api/sources"), (w.admin.api(), f"/api/sources?user={who.user}")):
        status, body = api.request("GET", path)
        assert status == 200, (path, status, body)
        assert body.get("machines") == [] and body["binding"]["delivery"] == [
            {"kind": "service", "url": w.ref.issuer, "state": "not_delivering"}], (path, body)


@local_harness
def test_a_server_refused_at_the_token_request_is_remembered(world):
    """Case 2, `invalid_scope`: the service does not let this server ask.
    The create is `not_applicable` with no warning and no call; for five
    minutes nothing more is asked (no token request either) and the status
    says so; `shed users refresh` clears the memory; once the server may ask
    again, a create asks."""
    w = world
    dana = _person(w, "dana")
    w.ref.deliveries.deny([w.client_id])
    calls, toks = _counts(w)
    name = f"dana-{_uid()}"
    r, rec = _create_local(w, dana, name)
    assert dl.delivery_warnings(r.stdout + r.stderr) == [], r.stderr
    _not_applicable(rec, name)
    assert _counts(w) == (calls, toks + 1), f"a refused token request: {_counts(w)}, want {(calls, toks + 1)}"
    _assert_not_delivering(w, dana)

    name = f"dana-{_uid()}"
    _, rec = _create_local(w, dana, name)
    _not_applicable(rec, name)
    assert _counts(w) == (calls, toks + 1), f"the memory asked the service: {_counts(w)}"

    w.refresh_ok()
    name = f"dana-{_uid()}"
    _, rec = _create_local(w, dana, name)
    _not_applicable(rec, name)
    assert _counts(w) == (calls, toks + 2), f"after shed users refresh the token was not asked again: {_counts(w)}"

    w.ref.deliveries.deny([])
    w.refresh_ok()
    name = f"dana-{_uid()}"
    _, rec = _create_local(w, dana, name)
    _not_applicable(rec, name)
    assert _counts(w) == (calls + 1, toks + 3), f"allowed again, the create did not ask: {_counts(w)}"
    assert _source_status(dana).get("service", {}).get("delivering") is True


@local_harness
def test_a_grant_removed_after_the_token_is_remembered(world):
    """Case 2, `403 INSUFFICIENT_SCOPE` on the call: the server holds a token
    for the delivery scope when the service stops letting it ask. The call
    is refused, the create `not_applicable` with no warning; the next create
    makes no call and no token request."""
    w = world
    dana = _person(w, "dana")
    _, rec = _create_local(w, dana, f"dana-{_uid()}")
    _not_applicable(rec, "the first create")
    calls, toks = _counts(w)
    w.ref.deliveries.deny([w.client_id])
    name = f"dana-{_uid()}"
    r, rec = _create_local(w, dana, name)
    assert dl.delivery_warnings(r.stdout + r.stderr) == [], r.stderr
    _not_applicable(rec, name)
    assert _counts(w) == (calls + 1, toks), f"the call with the kept token: {_counts(w)}, want {(calls + 1, toks)}"
    _assert_not_delivering(w, dana)
    name = f"dana-{_uid()}"
    _, rec = _create_local(w, dana, name)
    _not_applicable(rec, name)
    assert _counts(w) == (calls + 1, toks), f"the memory asked the service: {_counts(w)}"


# -- case 5 ------------------------------------------------------------------


def _bindings_text(w: World) -> str:
    files = list(w.state.rglob("source-bindings.json"))
    return "".join(p.read_text(encoding="utf-8") for p in files)


def _refuse_fixture(name: str = "path-dot-segment") -> dl.RefuseFixture:
    got = [f for f in dl.load_refuse_fixtures() if f.name == name]
    assert len(got) == 1, name
    return got[0]


def _ref_source(w: World, who, machine: FakeMachine, cred, refuse: Path) -> dl.RefSource:
    """The provision contract's reference source as `who`'s machine,
    answering every request with the refuse fixture `refuse` (refused: the
    shed's agent is never asked to write)."""
    binary = dl.ref_source_binary()
    if not (binary.is_file() and os.access(binary, os.X_OK)):
        pytest.fail(f"{binary} is missing: make test-integration-directory builds it (make provision-ref)")
    home = w.root / f"refsource-{_uid()}"
    home.mkdir(mode=0o700)
    ca = dl.write_ca(home / "server-ca.pem", HOST, cred.https_port, cred.pin)
    ident = dl.SourceIdentity(url=f"https://{HOST}:{cred.https_port}", ca_file=str(ca), cert_file=cred.pair[0],
                              key_file=cred.pair[1], machine_id=machine.id, machine_name=machine.name)
    src = dl.RefSource(dl.refsource_argv(binary, ident, refuse_fixture=str(refuse)), home, label="machine")
    try:
        src.wait_stream()
    except BaseException:
        src.stop()
        raise
    return src


def _credentials(w: World, who):
    c = w.enrol_cert(who, "credentials")
    return SimpleNamespace(https_port=c.https_port, pin=c.pin, pair=(str(c.cert), str(c.key)))


def _assert_service_refusals(w: World, who, machine: FakeMachine, cred, admin_cred) -> None:
    """Every route that would bind, move, clear, stream to or answer from a
    machine for `who` answers 409 SOURCE_IS_SERVICE: as `who`, and as the
    local admin naming them (`?user=`), on the CLI and the API."""
    q = f"?user={who.user}"
    for api, named, label in ((who.api(), {}, who.user), (w.admin.api(), {"user": who.user}, "the admin")):
        for path, body in (("/api/sources/use", {"machine_id": machine.id, **named}), ("/api/sources/unbind", named)):
            status, resp = api.request("POST", path, body)
            assert (status, error_code(resp)) == (409, "SOURCE_IS_SERVICE"), (label, path, status, resp)
    rid = uuid.uuid4().hex
    for cert, suffix, label in ((cred, "", who.user), (admin_cred, q, "the admin")):
        for method, path, body in (
            ("GET", "/api/delivery/requests", None),
            ("POST", f"/api/delivery/requests/{rid}", {"version": 2, "id": rid, "items": [], "problems": []}),
            ("GET", "/api/plugins/listeners", None),
            ("GET", "/api/plugins/listeners/itest-hosted/messages", None),
            ("POST", "/api/plugins/listeners/itest-hosted/respond", {"id": rid, "final": True}),
        ):
            status, resp = pinned_request(HOST, cert.https_port, path + suffix, cert.pin, cert.pair, method=method,
                                          body=body, headers=dict(machine.headers), timeout=15)
            assert (status, error_code(resp)) == (409, "SOURCE_IS_SERVICE"), (label, method, path, status, resp)
    for args, actor in (((), who), (("--user", who.user), w.admin)):
        for cmd in (["source", "use", *args, "--", machine.name], ["source", "unbind", *args]):
            r = actor.cli(*cmd, timeout=60)
            assert r.returncode != 0 and SOURCE_IS_SERVICE_TEXT in r.stderr, (actor.user, cmd, r.stdout, r.stderr)
        st = _source_status(actor, *args)
        assert st.get("source") is None and (st.get("service") or {}).get("url") == w.ref.issuer, (actor.user, st)


@local_harness
def test_who_is_asked_moves_with_the_name_and_a_moved_name_gets_nothing_old(world):
    """Case 5, criterion 2's first, second and fourth parts. pat is a local
    admin who is also a person in the service's list: their pinned machine
    answers (a refused answer: nothing written) and the service is not
    asked. Moved into the directory (config change and restart): the
    binding is removed and logged, every source route refuses, a new shed
    goes to the service. Moved back: a new shed gets no source, and no
    binding came back.

    Not here: `409 SHED_ORIGIN_CHANGED` for a shed made before the move. The
    harness's fake cloud provider keeps its instances in memory, so the
    restart that moves the name drops every shed's record ("has no instance
    in the cloud any more"); no shed outlives the move. The Go tests of A3
    hold the refusal on all three backends, and the checkpoint checks it by
    hand on a real backend."""
    w = world
    pk = w.key("pat")
    w.set_user("pat", [pk])
    w.publish()
    w.reconfigure(with_local_admin(w.config(), "pat", pk.pub))
    w.refresh_ok()
    assert (w.user_row("pat") or {}).get("source") == "local", w.user_row("pat")
    pat = w.identity("pat", pk)
    pat.add_server()
    cred, admin_cred = _credentials(w, pat), _credentials(w, w.admin)
    machine = FakeMachine("pat", HOST, cred, tag="hosted")
    ensure_source(pat.api(), machine, f"itest-hosted-{_uid()}")
    assert '"pat"' in _bindings_text(w), "no binding of pat in source-bindings.json"
    fx = _refuse_fixture()

    # A local admin: their machine, never the service.
    calls = w.ref.deliveries.calls()
    old = f"pat-old-{_uid()}"
    with _ref_source(w, pat, machine, cred, fx.path) as src:
        r, rec = _create_local(w, pat, old)
        reqs = src.requests(old)
    assert [q.get("reason") for q in reqs] == ["create"], reqs
    assert dl.delivery_warnings(r.stdout + r.stderr) == [dl.delivery_warning(old, "answer_refused", rule=fx.rule)]
    last = rec.get("last") or {}
    assert (last.get("outcome"), last.get("failure")) == ("refused", {"reason": "answer_refused", "rule": fx.rule})
    assert last.get("source") == {"kind": "machine", "machine_id": machine.id, "machine_name": machine.name}, last
    assert w.ref.deliveries.calls() == calls and _asks(w, old) == 0, "a local admin's delivery asked the service"

    # Into the directory.
    w.reconfigure(without_local_admin(w.config(), "pat"))
    w.refresh_ok()
    assert (w.user_row("pat") or {}).get("source") == "directory", w.user_row("pat")
    assert BINDING_REMOVED.format(user="pat") in w.server.log(), "the removal of pat's binding was not logged"
    assert '"pat"' not in _bindings_text(w), f"pat's binding is still in source-bindings.json: {_bindings_text(w)}"
    _assert_service_refusals(w, pat, machine, cred, admin_cred)
    assert w.ref.deliveries.calls() == calls, "the refused routes asked the service"
    new = f"pat-dir-{_uid()}"
    _, rec = _create_local(w, pat, new)
    _not_applicable(rec, new)
    assert w.ref.deliveries.calls() == calls + 1 and _asks(w, new) == 1, "a directory user's create did not ask"
    assert '"pat"' not in _bindings_text(w), "a binding was made for a directory user"

    # Back to the local admins.
    w.reconfigure(with_local_admin(w.config(), "pat", pk.pub))
    calls = w.ref.deliveries.calls()
    assert '"pat"' not in _bindings_text(w), "pat's binding came back"
    back = f"pat-back-{_uid()}"
    _, rec = _create_local(w, pat, back)
    _not_applicable(rec, back)
    assert w.ref.deliveries.calls() == calls and _asks(w, back) == 0, "a local admin's create asked the service"
    st = _source_status(pat)
    assert st.get("service") is None and st.get("source") is None, st


@local_harness
@pytest.mark.slow
def test_the_choice_holds_for_a_caller_that_left_and_after_the_trust_time(world):
    """Case 5, criterion 2's third part. A create made by a raw API request
    that is not followed (the remote backend's job runs on after the 201)
    asks the service (here answering 503: `source_offline`, the service
    recorded). With the service out until the trust time ends, the routes
    still answer for the person by the static rule: the sources document
    names the service and `use` is 409 SOURCE_IS_SERVICE. (A delivery itself
    cannot start then: a directory user cannot log in. The Go tests hold the
    choice for it; the checkpoint's cloud run holds the delayed create.)"""
    w = world
    dana = _person(w, "dana")
    w.ref.deliveries.set({"dana": delivery_status(503, "UNAVAILABLE")})
    calls = w.ref.deliveries.calls()
    name = f"dana-{_uid()}"
    status, body = dana.api().request("POST", "/api/sheds", {"name": name}, timeout=120)
    assert status in (200, 201, 202), (status, body)
    rec = _settled(lambda: _doc(w, dana, name), name)
    assert (rec["last"].get("outcome"), rec["last"].get("failure")) == ("no_answer", {"reason": "source_offline"})
    assert rec["last"].get("source") == SERVICE, rec["last"]
    assert w.ref.deliveries.calls() == calls + 1

    w.ref.outage(True)
    try:
        w.wait_state("expired", TRUST_SECONDS + TRUST_TICK + 90)
        admin = w.admin.api()
        status, doc = admin.request("GET", "/api/sources?user=dana")
        assert status == 200 and doc["binding"]["delivery"][0].get("kind") == "service", (status, doc)
        status, resp = admin.request("POST", "/api/sources/use", {"machine_id": "0" * 32, "user": "dana"})
        assert (status, error_code(resp)) == (409, "SOURCE_IS_SERVICE"), (status, resp)
    finally:
        w.ref.outage(False)
    w.refresh_ok()


# -- case 7 ------------------------------------------------------------------


@local_harness
def test_no_request_for_a_service_client_a_script_key_or_another_users_shed(world):
    """Case 7. Nothing asks the service for a service client's shed or a
    shed of another user's (refused: 404 to a person, 403 to an admin), and
    neither may be provisioned by them. A script key's shed cannot exist on
    a directory server: a list never marks a key script, and the server
    refuses a script key on a local admin (its config does not load). The
    control: the owner's own create and provision ask."""
    w = world
    sk = w.key("script")
    script_cfg = with_local_admin(w.config(), LOCAL_ADMIN, sk.pub, label="itest-script", script=True)
    bad = w.root / "script.yaml"
    bad.write_text(yaml.safe_dump(script_cfg, sort_keys=False), encoding="utf-8")
    r = subprocess.run([str(w.server.binary), "--config", str(bad), "config-validate"], capture_output=True,
                       text=True, timeout=60, env=w.server.env(), stdin=subprocess.DEVNULL)
    assert r.returncode != 0 and SCRIPT_KEY_REFUSED in r.stdout + r.stderr, (r.returncode, r.stdout, r.stderr)

    w.reconfigure(with_service_client(w.config(), "itest", "itest-svc-"))
    gus, dana = _person(w, "gus"), _person(w, "dana")
    w.ref.deliveries.clear()

    mine = f"gus-{_uid()}"
    calls = w.ref.deliveries.calls()
    _create_local(w, gus, mine)
    assert w.ref.deliveries.calls() == calls + 1, "the owner's create did not ask (the control)"
    calls = w.ref.deliveries.calls()

    status, body = dana.api().request("POST", f"/api/sheds/{mine}/provision", timeout=60)
    assert (status, error_code(body)) == (404, "SHED_NOT_FOUND"), (status, body)
    status, body = w.admin.api().request("POST", f"/api/sheds/{mine}/provision", timeout=60)
    assert (status, error_code(body)) == (403, "FORBIDDEN"), (status, body)

    r = w.server.run("client", "issue", "itest")
    assert r.returncode == 0, f"shed-server client issue: {r.stderr}"
    bundle = json.loads(r.stdout)
    d = w.root / "certs" / "svc"
    d.mkdir(parents=True, mode=0o700)
    (d / "client.pem").write_text(bundle["client_cert"], encoding="utf-8")
    key = d / "client.key"
    key.write_text(bundle["client_key"], encoding="utf-8")
    key.chmod(0o600)
    svc = Pinned(HOST, int(bundle["https_port"]), bundle["tls_cert_fingerprint"], str(d / "client.pem"), str(key))
    vname = f"itest-svc-{_uid()}"
    try:
        status, body = svc.request("POST", "/api/sheds", {"name": vname}, timeout=180)
        assert status in (200, 201, 202), (status, body)

        def get():
            st, doc = svc.request("GET", f"/api/sheds/{vname}")
            assert st == 200, (st, doc)
            return doc

        _not_applicable(_settled(get, vname), vname)
        status, body = svc.request("POST", f"/api/sheds/{vname}/provision", timeout=60)
        assert (status, error_code(body)) == (403, "FORBIDDEN"), (status, body)
    finally:
        svc.request("DELETE", f"/api/sheds/{vname}", timeout=120)

    assert w.ref.deliveries.calls() == calls, "a service client's or another user's shed asked the service"
    assert (_asks(w, vname), _asks(w, mine)) == (0, 1), (_asks(w, vname), _asks(w, mine))
    r = gus.cli("provision", mine, timeout=120)
    assert r.returncode == 0, r.stderr
    assert w.ref.deliveries.calls() == calls + 1, "the owner's provision did not ask (the control)"


# ============================================================================
# Real backends: cases 1, 3, 4, 6 and 8 (written into a VM)
# ============================================================================


@dataclass
class HostedEnv:
    """One run's hosted delivery against one dev server: the reference
    server's control (`ctl`), the module's sentinels, the server's files, and
    what the module kept (`kept`: every API body and CLI output; `records`:
    alice's own records, the one place a problem's message may be shown) and
    the sheds a delivery ran for."""

    users: UsersEnv
    ctl: DeliveriesControl
    value: str
    text: str
    problem: str
    files: dl.ServerFiles
    log_offset: Any
    kept: list[str] = field(default_factory=list)
    records: list[str] = field(default_factory=list)
    delivered: set[str] = field(default_factory=set)
    # What the admin was shown, where a problem's message must never be.
    admin_seen: list[str] = field(default_factory=list)
    # Which case sent each sentinel through the server (the sweep's
    # positive control that it searches for something that was there).
    planted: dict[str, str] = field(default_factory=dict)
    heals: int = 0

    @property
    def backend(self) -> str:
        return self.users.backend

    @property
    def alice(self) -> Identity:
        return self.users.alice

    def keep(self, *texts: Any) -> None:
        for t in texts:
            if t is not None:
                self.kept.append(t if isinstance(t, str) else json.dumps(t))

    def items(self) -> list[dict]:
        return standard_items(self.value)

    def problem_entry(self) -> dict:
        """One problem whose message carries the problem sentinel."""
        return {"provider": "github", "code": "signed_out", "message": f"signed out {self.problem}"}

    def serve(self, entry) -> None:
        self.ctl.set({ALICE: entry})

    def log(self) -> str:
        return self.users.server._read_log_since(self.log_offset)

    def asks(self, name: str) -> int:
        return sum(1 for ln in self.log().splitlines() if ASKING_THE_SERVICE in ln and f'for shed "{name}"' in ln)


@pytest.fixture(scope="module")
def hosted(users_env):
    env = users_env
    why = hosted_skip_reason(dl.backend_key(env.backend))
    if why:
        pytest.skip(why)
    ctl = hosted_control()
    h = HostedEnv(users=env, ctl=ctl, value=dl.new_sentinel(), text=dl.new_sentinel(), problem=dl.new_sentinel(),
                  files=dl.server_files(env.backend, ssh_host=getattr(env.server, "ssh_host", None)),
                  log_offset=env.server._log_offset())
    try:
        yield h
    finally:
        ctl.clear()
        ctl.outage(False)


def _create(h: HostedEnv, name: str, repo: Optional[str] = REPO) -> subprocess.CompletedProcess:
    args = ["create", name, "--no-sync"]
    if h.users.image:
        args += ["--image", h.users.image]
    if repo:
        args += ["--repo", repo]
    r = h.alice.cli(*args, timeout=_timeout(h.users))
    h.keep(r.stdout, r.stderr)
    assert r.returncode == 0, f"shed create {name} (exit {r.returncode}): {r.stdout[-800:]!r} {r.stderr[-800:]!r}"
    return r


def _record(h: HostedEnv, name: str) -> dict:
    status, body = h.alice.api().request("GET", f"/api/sheds/{name}")
    h.keep(body)
    h.records.append(json.dumps(body))
    assert status == 200 and isinstance(body, dict) and isinstance(body.get("delivery"), dict), (status, body)
    return body["delivery"]


def _provision_cli(h: HostedEnv, name: str) -> subprocess.CompletedProcess:
    r = h.alice.cli("provision", name, timeout=DELIVERY_TIMEOUT + 180)
    h.keep(r.stdout, r.stderr)
    assert r.returncode == 0, f"shed provision {name} (exit {r.returncode}): {r.stderr[-800:]!r}"
    return r


class MainShed:
    """alice's shed made by case 1, reused by cases 4 and 6 (local
    backends); deleted at the module's end, or by case 1 on [remote]."""

    def __init__(self, h: HostedEnv) -> None:
        self.h, self.name, self.items, self.created = h, None, [], None

    def get(self) -> str:
        if self.name is None:
            h = self.h
            name, items = f"itest-hdl-main-{_uid()}", h.items()
            h.serve(delivery_answer(items))
            h.delivered.add(name)
            try:
                self.calls = h.ctl.calls()
                self.created = _create(h, name)
                self.calls = h.ctl.calls() - self.calls
            except BaseException:
                _delete(h.users, h.alice, name)
                raise
            self.name, self.items = name, items
        return self.name

    def close(self) -> None:
        if self.name is not None:
            name, self.name = self.name, None
            _delete(self.h.users, self.h.alice, name)


@pytest.fixture(scope="module")
def main_shed(hosted):
    slot = MainShed(hosted)
    yield slot
    slot.close()


# -- case 1 ------------------------------------------------------------------


@real_backend
def test_create_delivers_the_services_items(hosted, main_shed):
    h = hosted
    alice = h.alice
    name = main_shed.get()
    items = main_shed.items
    try:
        out = main_shed.created.stdout + main_shed.created.stderr
        assert dl.delivery_warnings(out) == [], dl.delivery_warnings(out)
        assert main_shed.calls == 1 and h.asks(name) == 1, f"want one call for {name}: {main_shed.calls}"
        rec = _record(h, name)
        assert rec.get("state") == "delivered", rec
        problems = dl.step_problems(rec, {"stage1": ("done", None), "clone": ("done", None),
                                          "stage2": ("done", None), "install": ("skipped", "no_hook")})
        assert not problems, problems
        last = rec.get("last") or {}
        assert (last.get("reason"), last.get("outcome")) == ("create", "accepted"), last
        assert last.get("source") == SERVICE, f"the record's source is not the service: {last}"
        assert "failure" not in last and not last.get("problems"), last
        bad = dl.record_item_problems(rec, items)
        assert not bad, bad
        _check_bundle(None, alice, name, items, REPO_DIR)
        h.planted.setdefault("value", "case 1")

        stage1, head, stage2 = f"{dl.HOME}/{STAGE1_FILE}", f"{REPO_DIR}/.git/HEAD", f"{REPO_DIR}/.itest-delivery-project"
        r = _exec(alice, name, "find", stage1, head, stage2, "-maxdepth", "0", "-printf", "%T@\t%p\n")
        t = {p: float(s) for s, p in (ln.split("\t", 1) for ln in r.stdout.splitlines() if "\t" in ln)}
        assert set(t) == {stage1, head, stage2}, r.stdout
        assert t[stage1] < t[head] <= t[stage2], f"the stage order around the clone: {t}"

        r = alice.cli("provision", name, "--status", timeout=60)
        h.keep(r.stdout, r.stderr)
        assert r.returncode == 0 and "from the account service" in r.stdout, r.stdout
    finally:
        if h.backend == "remote":
            main_shed.close()


# -- case 3 ------------------------------------------------------------------


def _failures(h: HostedEnv) -> list[tuple[str, Any, str, Optional[str]]]:
    """(label, entry or "outage", reason, rule) for each failure the
    reference server plays. The 500's message and the redirect's target
    carry the text sentinel."""
    return [
        ("500", delivery_status(500, "INTERNAL", f"boom {h.text}"), "source_offline", None),
        ("503", delivery_status(503, "UNAVAILABLE"), "source_offline", None),
        ("429", delivery_status(429, "RATE_LIMITED", headers={"Retry-After": "30"}), "source_offline", None),
        ("outage", "outage", "source_offline", None),
        ("silent", {"mode": "silent"}, "source_timeout", None),
        ("410", delivery_status(410, "DEADLINE_PASSED"), "source_timeout", None),
        ("truncated", {"mode": "truncated", "items": h.items()}, "source_disconnected", None),
        ("401 twice", delivery_status(401, "UNAUTHORIZED"), "source_refused", None),
        ("unlisted 418", delivery_status(418, text=""), "source_refused", None),
        ("404 other code", delivery_status(404, "NOT_FOUND"), "source_refused", None),
        ("302", delivery_status(302, headers={"Location": f"https://elsewhere.example.test/{h.text}"}),
         "source_refused", None),
        ("oversize", {"mode": "oversize"}, "answer_refused", "body_too_large"),
        ("echo", {"mode": "echo"}, "answer_refused", "echoes_credential"),
    ]


def _play(h: HostedEnv, entry) -> None:
    if entry == "outage":
        h.ctl.clear()
        h.ctl.outage(True)
    else:
        h.ctl.outage(False)
        h.serve(entry)


def _check_failure(h: HostedEnv, name: str, rec: dict, reason: str, rule: Optional[str], what: str, *,
                   first: bool) -> None:
    """The record of a failed delivery: stage 1 failed with the reason, the
    failure and the service recorded. At create (`first`), nothing was ever
    delivered: not_delivered, stage 2 and the install hook owed; after a
    good delivery, stage 2 keeps what it had, and the state is not
    delivered."""
    want_steps = {"stage1": ("failed", reason)}
    if first:
        assert rec.get("state") == "not_delivered", f"{what}: {rec}"
        want_steps.update({"stage2": ("owed", None), "install": ("owed", None)})
    else:
        assert rec.get("state") in ("partial", "not_delivered"), f"{what}: {rec}"
    problems = dl.step_problems(rec, want_steps)
    assert not problems, f"{what}: {problems}"
    last = rec.get("last") or {}
    want = {"reason": reason, **({"rule": rule} if rule else {})}
    outcome = "refused" if reason == "answer_refused" else "no_answer"
    assert (last.get("outcome"), last.get("failure")) == (outcome, want), f"{what}: {last}"
    assert last.get("source") == SERVICE, f"{what}: the failure does not record the service: {last}"


def _heal(h: HostedEnv, name: str, what: str) -> None:
    """`shed provision` with the service healthy, after a failure: one call,
    no warning, every step done, the record's items, and this recovery's own
    bundle in the shed (content no earlier recovery sent, still carrying the
    value sentinel; the answer carries a problem: the sweep's problem
    sentinel)."""
    h.heals += 1
    items = standard_items(f"{h.value}h{h.heals}")
    problem = h.problem_entry()
    _play(h, delivery_answer(items, [problem]))
    asks = h.asks(name)
    r = _provision_cli(h, name)
    assert dl.delivery_warnings(r.stdout + r.stderr) == [], f"healed after {what}: {r.stderr}"
    assert h.asks(name) == asks + 1, f"healed after {what}: {h.asks(name) - asks} requests"
    rec = _record(h, name)
    assert rec.get("state") == "delivered", f"healed after {what}: {rec}"
    bad = dl.step_problems(rec, {"stage1": ("done", None), "clone": ("done", None), "stage2": ("done", None),
                                 "install": ("skipped", "no_hook")})
    assert not bad, f"healed after {what}: {bad}"
    last = rec.get("last") or {}
    assert (last.get("reason"), last.get("outcome"), last.get("source")) == ("provision", "accepted", SERVICE), (
        f"healed after {what}: {last}")
    assert [p.get("message") for p in last.get("problems") or []] == [problem["message"]], last
    assert not rec.get("owed"), f"healed after {what}: {rec}"
    bad = dl.record_item_problems(rec, items)
    assert not bad, f"healed after {what}: {bad}"
    _check_bundle(None, h.alice, name, items, REPO_DIR)
    h.planted.setdefault("value", "case 3")
    h.planted.setdefault("problem", "case 3")
    _admin_looks(h, name)


def _admin_looks(h: HostedEnv, name: str) -> None:
    """What the admin is shown of alice's shed, whose record holds a
    problem (the sweep checks it never shows the message): its document and
    the list, kept while the shed is there."""
    admin = h.users.admin
    status, body = admin.api().request("GET", f"/api/sheds/{name}")
    assert status == 200, (status, body)
    h.keep(body)
    h.admin_seen.append(json.dumps(body))
    r = admin.cli("--json", "list", timeout=60)
    assert r.returncode == 0 and name in r.stdout, r.stderr
    h.keep(r.stdout, r.stderr)
    h.admin_seen.append(r.stdout + r.stderr)


@real_backend
def test_each_failure_names_shed_provision_and_provision_finishes_it(hosted):
    """Case 3 (criterion 4): each failure, then a healthy `shed provision`
    that finishes the delivery, before the next failure. [remote]: the
    create with the 500 and its healthy provision only."""
    h = hosted
    name = f"itest-hdl-fail-{_uid()}"
    failures = _failures(h)
    if h.backend == "remote":
        failures = failures[:1]
    h.delivered.add(name)
    try:
        label, entry, reason, rule = failures[0]
        _play(h, entry)
        r = _create(h, name)
        if h.backend != "remote":
            assert dl.delivery_warnings(r.stdout + r.stderr) == [dl.delivery_warning(name, reason)], r.stderr
        rec = _record(h, name)
        _check_failure(h, name, rec, reason, rule, f"create ({label})", first=True)
        assert dl.steps_of(rec)["clone"] == ("done", None)
        h.planted.setdefault("text", f"case 3 ({label})")
        _heal(h, name, label)

        for label, entry, reason, rule in failures[1:]:
            _play(h, entry)
            asks, calls = h.asks(name), h.ctl.calls()
            r = _provision_cli(h, name)
            assert dl.delivery_warnings(r.stdout + r.stderr) == [dl.delivery_warning(name, reason, rule=rule)], (
                label, r.stdout, r.stderr)
            _check_failure(h, name, _record(h, name), reason, rule, label, first=False)
            assert h.asks(name) == asks + 1, f"{label}: {h.asks(name) - asks} requests, want 1"
            time.sleep(QUIET)
            assert h.asks(name) == asks + 1, f"{label}: a request was made in the background"
            if entry != "outage":
                n = h.ctl.calls() - calls
                assert n == (2 if label == "401 twice" else 1), f"{label}: {n} calls to the service"
            _heal(h, name, label)
    finally:
        h.ctl.outage(False)
        _delete(h.users, h.alice, name)


@real_backend
@pytest.mark.local_vm_only("a delete during a slow reply is held by the Go tests there; one VM fewer")
def test_a_shed_deleted_during_a_slow_reply_ends_its_request(hosted):
    """Criterion 4's last sentence: the service silent, a provision waiting
    on it; the shed is deleted: the provision ends well before the deadline
    and nothing more is asked."""
    h = hosted
    name = f"itest-hdl-del-{_uid()}"
    h.serve(NO_SOURCE)
    _create(h, name, repo=None)
    proc = None
    try:
        h.serve({"mode": "silent"})
        proc = h.alice.popen_cli("provision", name)
        deadline = time.monotonic() + 60
        while h.asks(name) < 2:
            assert time.monotonic() < deadline and proc.poll() is None, "the provision never asked the service"
            time.sleep(0.5)
        t0 = time.monotonic()
        r = h.alice.cli("delete", name, "-f", timeout=_timeout(h.users))
        assert r.returncode == 0, r.stderr
        out, err = proc.communicate(timeout=DELIVERY_TIMEOUT + 60)
        took = time.monotonic() - t0
        h.keep(out, err)
        assert took < DELIVERY_TIMEOUT - 5, f"the provision ended {took:.0f} s after the delete: it waited for the deadline"
        time.sleep(QUIET)
        assert h.asks(name) == 2, "a request was made after the delete"
    finally:
        if proc is not None and proc.poll() is None:
            proc.kill()
            proc.communicate()
        h.users.server.ensure_deleted(name)


# -- case 4 ------------------------------------------------------------------


@real_backend
@pytest.mark.local_vm_only("each refusal is a provision of the main shed, which [remote] does not keep")
def test_each_refuse_fixture_as_a_200_is_refused_and_writes_nothing(hosted, main_shed):
    h = hosted
    alice = h.alice
    name = main_shed.get()
    everything = os.environ.get("SHED_DELIVERY_REFUSE_ALL", "").strip() == "1"
    fixtures = dl.select_refuse_fixtures(dl.load_refuse_fixtures(), everything=everything)
    assert fixtures, f"no live-runnable refuse fixture under {dl.REFUSE_DIR}: the case would prove nothing"
    # Each body as the fixture language renders it (answer, answer_text,
    # answer_base64, fill; raw bytes kept), by the reference source itself.
    bodies = {f.name: dl.render_answer_fixture(f.path) for f in fixtures}
    before = _written_at(_record(h, name))
    marker = "/tmp/itest-hosted-refuse-marker"
    _bash(alice, name, ': > "$1" && sleep 1', marker)
    failures = []
    for f in fixtures:
        h.serve(delivery_body(bodies[f.name]))
        status, body = alice.api().request("POST", f"/api/sheds/{name}/provision", timeout=DELIVERY_TIMEOUT + 120)
        h.keep(body)
        what = f"{f.name} ({f.rule})"
        if status != 200 or not isinstance(body, dict):
            failures.append(f"{what}: provision {status} {body!r}")
            continue
        rec = body["delivery"]
        last = rec.get("last") or {}
        if (last.get("outcome"), last.get("failure"), last.get("source")) != (
                "refused", {"reason": "answer_refused", "rule": f.rule}, SERVICE):
            failures.append(f"{what}: last {last}")
        if dl.steps_of(rec)["stage1"] != ("failed", "answer_refused"):
            failures.append(f"{what}: stage1 {dl.steps_of(rec)['stage1']}")
        if _written_at(rec) != before:
            failures.append(f"{what}: the record's items changed")
    r = _bash(alice, name, 'find /home/shed -xdev -newer "$1" -not -path "/home/shed/.cache/*" -print', marker)
    assert not failures, f"{len(failures)} of {len(fixtures)} refusals were wrong:\n" + "\n".join(failures)
    assert r.stdout.strip() == "", f"files changed in the shed during the refusals:\n{r.stdout}"


# -- case 8 ------------------------------------------------------------------


@real_backend
def test_a_shed_made_on_v011_is_listed_and_delivered(hosted):
    """A shed alice made on a v0.11 server binary before the server was
    upgraded to this build (same state): it is listed, its record loads, its
    origin was stamped at the first start (a delivery to it is not refused),
    and `shed provision` delivers the service's items into it."""
    h = hosted
    name = os.environ.get("SHED_HOSTED_V011_SHED", "").strip()
    if not name:
        pytest.skip("SHED_HOSTED_V011_SHED is not set: make a shed as itest-alice on the v0.11 server binary, "
                    "upgrade the server to this build on the same state, and name the shed")
    listed = {s["name"]: s for s in h.alice.list_sheds()}
    assert name in listed, f"{name} is not in alice's list: {sorted(listed)}"
    _record(h, name)
    items = [i for i in h.items() if i.get("base") != "project"]
    h.serve(delivery_answer(items))
    h.delivered.add(name)
    r = _provision_cli(h, name)
    assert dl.delivery_warnings(r.stdout + r.stderr) == [], r.stderr
    rec = _record(h, name)
    assert rec.get("state") in ("delivered", "partial") and (rec.get("last") or {}).get("source") == SERVICE, rec
    assert not dl.step_problems(rec, {"stage1": ("done", None)}), rec
    _check_bundle(None, h.alice, name, items, None)


# -- case 6 (last: it sweeps what the module did) -----------------------------


def _plant(h: HostedEnv, name: str) -> None:
    """Send every sentinel the sweep searches for through the server, on
    `name`: the service's text in a 500's message and in a redirect's
    target (each a failed provision), then values and a problem in a good
    answer."""
    for entry, reason in ((delivery_status(500, "INTERNAL", f"boom {h.text}"), "source_offline"),
                          (delivery_status(302, headers={"Location": f"https://elsewhere.example.test/{h.text}"}),
                           "source_refused")):
        h.serve(entry)
        r = _provision_cli(h, name)
        assert dl.delivery_warnings(r.stdout + r.stderr) == [dl.delivery_warning(name, reason)], r.stderr
    h.planted["text"] = "case 6"
    items = [dl.env_item("itest-after", "ITEST_DELIVERY_AFTER", f"after-{h.value}")]
    problem = h.problem_entry()
    h.serve(delivery_answer(items, [problem]))
    _provision_cli(h, name)
    rec = _record(h, name)
    assert rec.get("state") != "not_delivered" and (rec.get("last") or {}).get("outcome") == "accepted", rec
    assert [p.get("message") for p in (rec.get("last") or {}).get("problems") or []] == [problem["message"]], rec
    h.planted["value"] = h.planted["problem"] = "case 6"


@real_backend
def test_no_value_or_service_text_leaves_the_shed(hosted, main_shed):
    """Case 6: no delivered value and no text of the service's (an error's
    message, a redirect's target) anywhere the module can see; a problem's
    message only in the `last.problems` of the record its user is shown,
    taken out structurally before everything else is searched for it. Each
    sentinel was sent through the server first: on a local backend by this
    case, on [remote] by case 3 (its create's 500 and its healthy answer),
    which it asserts."""
    h = hosted
    alice = h.alice
    if h.backend != "remote":
        name = main_shed.get()
        _plant(h, name)
        _admin_looks(h, name)
        r = alice.cli("--json", "list", timeout=60)
        h.keep(r.stdout, r.stderr)
        r = alice.cli("--json", "provision", name, "--status", timeout=60)
        h.keep(r.stdout, r.stderr)
        r = alice.cli("provision", name, "--status", timeout=60)
        h.keep(r.stdout, r.stderr)
    assert h.admin_seen, "nothing the admin was shown was kept: the admin's checks would pass vacuously"
    missing = sorted({"value", "text", "problem"} - set(h.planted))
    assert not missing, (f"the sweep searches for sentinels nothing sent through the server: {missing}; on [remote] "
                         f"case 3 sends them (run it first): planted {h.planted}")

    values = dl.sentinel_forms(h.value) + dl.sentinel_forms(h.text)
    leaks = []
    for i, t in enumerate(h.kept):
        got = dl.found_forms(t, values)
        if got:
            leaks.append(f"kept output #{i} holds {got}: {t[:200]!r}")
        if h.problem in dl.without_problem_messages(t, [h.problem_entry()]):
            leaks.append(f"kept output #{i} holds a problem's message outside last.problems: {t[:200]!r}")
    for i, t in enumerate(h.admin_seen):
        if h.problem in t:
            leaks.append(f"what the admin was shown (#{i}) holds a problem's message: {t[:200]!r}")
    assert any(n in t for t in h.kept for n in h.delivered), "the kept outputs name none of the module's sheds"
    assert any(h.problem in r for r in h.records), "the problem's message is not in alice's record (the control)"

    everything = values + [h.problem]
    log = h.log()
    assert any(ASKING_THE_SERVICE in ln for ln in log.splitlines()), (
        "the server log read since the module began holds no hosted request: the wrong log, or none")
    if dl.found_forms(log, everything):
        leaks.append(f"the server log holds {dl.found_forms(log, everything)}")
    audit = dl.read_audit_log(h.files)
    events = dl.audit_events(audit)
    assert any(e.get("event") == "delivery" and e.get("shed") in h.delivered for e in events), (
        f"the audit log {h.files.audit_log} holds no delivery event for the module's sheds: the wrong file")
    if dl.found_forms(audit, everything):
        leaks.append(f"the audit log holds {dl.found_forms(audit, everything)}")
    control = dl.scan_state_dirs(h.files, sorted(h.delivered))
    assert control, f"no file in the state directories {h.files.state_dirs} names the module's sheds: the wrong ones"
    hits = dl.scan_state_dirs(h.files, values)
    if hits:
        leaks.append(f"files in the state directories hold a value or the service's text: {hits}")
    # A shed's record on disk keeps a problem's message in last.problems (its
    # user reads it there): those fields are taken out, then the file is
    # searched again.
    for path in dl.scan_state_dirs(h.files, [h.problem]):
        if h.problem in dl.without_problem_messages(dl.read_server_file(h.files, path), [h.problem_entry()]):
            leaks.append(f"{path} holds a problem's message outside last.problems")
    assert not leaks, "a value or the service's text left where it belongs:\n" + "\n".join(leaks)
