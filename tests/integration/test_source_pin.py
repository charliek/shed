"""The pin: one machine per user answers credential requests (live).

A server sends each user's credential-bus requests to one machine of that
user, its credential source: the first machine to connect is bound, any
other machine of the user is refused naming the bound one, and the user (or
an admin) moves the binding with `shed source use`. These cases drive one dev
server with the itest user list (`fixtures/identity.py`) through synthetic
machines (`fixtures/busclient.py: FakeMachine`): an identity's credentials
certificate plus a machine id and name, subscribing and answering exactly as
a broker does. No real broker runs here (the broker's side is
tests/broker-wire); requests start inside a shed with a POST to the
shed-agent's `127.0.0.1:498/v1/publish`, which every image serves.

What it shows, live:

  - alice's machine A is the source; her machine B is refused with 403
    SOURCE_NOT_BOUND naming A (never 401, never a bare 409), and the refusal
    changes nothing;
  - a subscribe with no (or malformed) machine headers is 403
    SOURCE_MACHINE_ID_REQUIRED, and so is the source's own answer to a real
    pending request, which the same machine's proper answer then completes;
  - A subscribing twice to one request type is 409 SOURCE_DUPLICATE_STREAM
    and its first stream keeps working;
  - a request from alice's shed reaches A only: B is refused naming A, bob's
    machine (subscribed to the same request type) receives nothing, and both
    their answers are refused;
  - `shed source ls`, `status` and `use` as alice, in text and `--json`, `use`
    by name after `--`, by id prefix and by full id;
  - a move to B ends A's stream at once, refuses A's next subscribe naming B,
    and B then receives the next request; a request pending on A when the
    move happens is never delivered to B, A's late answer to it is refused,
    and nothing fails back on its own: with B disconnected, A is still
    refused and the binding is unchanged, and B, reconnected, answers;
  - the admin reads and moves alice's binding with `--user` and can choose
    only among alice's machines; bob naming alice gets one fixed 403 (the
    same as for a user that does not exist); a script key can neither read
    nor move a binding, and a credentials certificate (what a machine holds)
    cannot reach the routes at all, while alice's own key moves it;
  - `GET /api/info` lists the feature `source_binding`.

Every refusal sits beside its positive control: the same identity doing the
allowed thing on the same server.

**State on the server.** Bindings are kept in the server's state directory
and survive a restart, so a second run (or a run after `test_users.py`) meets
bindings already there: every case sets up what it needs by moving the
binding (`busclient.ensure_source`), never by assuming a fresh server, and
runs in any order. Only the throwaway itest identities are ever bound or
moved by these cases, never the developer's own user (whose real desktop
app may be the source on this server). When the module ends each itest user's
source is again their DEFAULT machine (alice: A; bob: his), each user put
back on their own as soon as their certificate exists, so a set-up that
fails part-way still repairs what it can; the teardown fails loudly when it
cannot. `test_users.py`'s bus case does not rely on it: it sets each user's
source to the machine it subscribes as itself (`ensure_source`), so the two
modules run in either order from any state, a run killed mid-move included.
On [vz], `users_env` restarts the Mac dev server with the user list and
back (`devcontrol.dev_config`), which keeps the developer's own bindings
aside for the detour and puts them back: the bindings and machine records
the itest users get there last only as long as the module. Elsewhere the
machines the cases introduce stay in the server's list (there is no route
to forget one): `itest-itest-alice`, `itest-itest-alice-b` and
`itest-itest-bob`, three in all, so the per-user admission rate limit (8,
then one per 15 s) is never reached. A correct server may answer `GET
/api/sources` with 503 SOURCE_BUSY for a moment, and a set-up subscribe
with 429 SOURCE_RATE_LIMITED: the helpers wait those out, a bounded number
of times, honouring Retry-After (`busclient`).

**The server with no user list.** There the only user is the developer's,
whose real broker may be the source, so nothing is bound or moved there:
`test_the_developers_own_source_is_readable` only reads it.

Backends: `users_env` (conftest), [vz] and [fc]; the whole module skips on
[remote] (`local_vm_only`), where sheds carry no helper requests. One shed
(alice's) is created per backend.

Run only these cases:

    cd tests/integration && uv run pytest -v -rs test_source_pin.py

with the environment the README's "The pin" section lists.
"""

from __future__ import annotations

import json
import re
import shutil
import subprocess
import threading
import time
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional
from urllib.parse import urlparse

import pytest
import yaml

from fixtures.busclient import (
    MACHINE_ID_HEADER,
    SOURCE_DUPLICATE_STREAM,
    SOURCE_MACHINE_ID_REQUIRED,
    SOURCE_MACHINE_NOT_FOUND,
    SOURCE_NOT_BOUND,
    BusListener,
    FakeMachine,
    bound_id,
    cli_retrying_busy,
    describe_sources,
    ensure_known,
    ensure_source,
    known_ids,
    payload_has,
    publish_argv,
    sources,
)
from fixtures.identity import ADMIN, ALICE, BOB, DEV_USER, Identity, UsersEnv, started_with_users
from fixtures.tlsclient import Pinned, error_code, error_message, pinned_request
from test_users import _delete, _ensure_running, _shed_fixture

pytestmark = pytest.mark.local_vm_only("the remote backend's sheds carry no helper requests")

FEATURE = "source_binding"

# The server's own words the refusals are matched against (checked against
# the source by test_fixture_units.py).
NOT_ADMIN = "only an admin can see or move another user's credential source"
SCRIPT_KEY = "a script key cannot see or move a credential source"
UNMATCHED = "response does not match a pending request for this namespace"
CONTROL_SCOPE = "control scope required"
DUPLICATE = "already holds"
# What the refusal of a request with no machine headers tells the person to do.
INSTALL_APP = "install the current shed desktop app"

# Timing. The guest gives up on a request publishTimeout after it sends it
# (cmd/shed-agent/http.go: 30 s), so every step of one request, from starting
# it in the shed to the source's answer, fits inside that with margin, as in
# test_users.py. The clock starts before `shed exec` runs, earlier than the
# guest's.
GUEST_REQUEST_LIFETIME = 30
DELIVERY_BUDGET = 14  # started in the shed -> the source's stream has it
REFUSAL_TIMEOUT = 4  # each other machine's answer (sent side by side)
ANSWER_BY = 22  # the source's answer is sent by then ...
ANSWER_TIMEOUT = 5  # ... and accepted within this: 27 s, inside 30 s
# The server writes a keepalive on every subscription every 25 s
# (internal/api/plugin_handlers.go: sseKeepaliveInterval).
LIVENESS_TIMEOUT = 40
# A move closes the old machine's streams under the user's gate, before the
# move's answer is written; this is only the time for that to reach the
# client.
STREAM_END_TIMEOUT = 10
# A shed's message channel comes up shortly after a create or start.
ROUTE_TIMEOUT = 60


# ----------------------------------------------------------------------------
# The machines and the module's state
# ----------------------------------------------------------------------------


@dataclass
class PinEnv:
    """The users of one run (`users`) and their synthetic machines: alice's
    A (her default machine, the one test_users.py's bus case uses) and B,
    and bob's default machine. `certs` are the credentials certificates the
    machines present (alice's for A and B, bob's for his)."""

    users: UsersEnv
    alice_a: FakeMachine
    alice_b: FakeMachine
    bob_m: FakeMachine
    certs: dict = field(default_factory=dict)

    @property
    def machines(self) -> tuple[FakeMachine, FakeMachine, FakeMachine]:
        return self.alice_a, self.alice_b, self.bob_m

    def alice_api(self) -> Pinned:
        return self.users.alice.api()

    def bob_api(self) -> Pinned:
        return self.users.bob.api()

    def state(self) -> str:
        """alice's and bob's sources in one line each, for a failure message
        (never raises)."""
        out = []
        for who in (self.users.alice, self.users.bob):
            try:
                out.append(describe_sources(sources(who.api())))
            except Exception as e:  # noqa: BLE001 - a diagnostic only
                out.append(f"{who.user}: sources unreadable ({type(e).__name__}: {e})")
        return "; ".join(out)

    def close_streams(self) -> None:
        for m in self.machines:
            m.close()


def _ns() -> str:
    """A request type (namespace) of its own for one subscribe: a namespace a
    closed stream may still hold for a moment is never reused."""
    return f"itest-pin-{uuid.uuid4().hex[:8]}"


@dataclass
class _Restorable:
    """One itest user whose source the module puts back on their default
    machine at the end, and every machine of theirs it made."""

    who: Identity
    default: FakeMachine
    machines: tuple


def _restore_defaults(server_name: str, users: list[_Restorable]) -> None:
    """Leave each user's source on their default machine (what
    test_users.py's bus case subscribes as), checked: every user is tried,
    each on their own, and every failure is reported together."""
    failures = []
    for r in users:
        try:
            for m in r.machines:
                m.close()
            ensure_source(r.who.api(), r.default, _ns())
        except Exception as e:  # noqa: BLE001 - reported below, with the rest
            failures.append(f"{r.who.user}: {type(e).__name__}: {e}")
    if failures:
        raise AssertionError(
            f"CLEAN-UP FAILED on server {server_name!r}: an itest user's credential source is not back on "
            f"their default machine. test_users.py's bus case sets its own and does not depend on it; run "
            f"test_source_pin.py again to put it back:\n" + "\n".join(failures)
        )


@pytest.fixture(scope="module")
def pin_env(users_env, tmp_path_factory):
    """The module's machines, with credentials certificates enrolled over
    `_bootstrap` as alice and bob. At the end: every stream closed, each
    user's source back on their default machine (checked), the certificates
    removed (they are live credentials until they expire).

    Each user is put back on their own as soon as their certificate exists,
    so a set-up that fails part-way (bob's enrolment, say) still repairs
    alice, whose source a run killed mid-move may have left elsewhere."""
    env = users_env
    _require_shed_source()
    certs_dir = tmp_path_factory.mktemp("pin-certs")
    certs_dir.chmod(0o700)
    restorable: list[_Restorable] = []
    try:
        alice_cert = env.alice.enrol("credentials", certs_dir / "alice")
        alice_a = FakeMachine(ALICE, env.alice.host, alice_cert)
        alice_b = FakeMachine(ALICE, env.alice.host, alice_cert, tag="b")
        restorable.append(_Restorable(env.alice, alice_a, (alice_a, alice_b)))
        bob_cert = env.bob.enrol("credentials", certs_dir / "bob")
        bob_m = FakeMachine(BOB, env.bob.host, bob_cert)
        restorable.append(_Restorable(env.bob, bob_m, (bob_m,)))
        p = PinEnv(users=env, alice_a=alice_a, alice_b=alice_b, bob_m=bob_m,
                   certs={ALICE: alice_cert, BOB: bob_cert})
    except BaseException as e:
        try:
            _restore_defaults(env.server.name, restorable)
        except AssertionError as cleanup:
            e.add_note(str(cleanup))
        finally:
            shutil.rmtree(certs_dir, ignore_errors=True)
        raise
    try:
        yield p
    finally:
        try:
            _restore_defaults(env.server.name, restorable)
        finally:
            shutil.rmtree(certs_dir, ignore_errors=True)


@pytest.fixture
def enrolled(tmp_path):
    """Where a case enrols a certificate over `_bootstrap`; removed when the
    case ends."""
    d = tmp_path / "enrolled"
    d.mkdir(mode=0o700)
    yield d
    shutil.rmtree(d)


@pytest.fixture
def pin(pin_env):
    """The module's machines for one case; every stream the case opened is
    closed when it ends, pass or fail, so the next case starts with none."""
    try:
        yield pin_env
    finally:
        pin_env.close_streams()


def _a_source_with_b_known(p: PinEnv) -> str:
    """alice's source is A and the server knows B (each case's starting
    point, from whatever state the server is in). Returns how A became the
    source."""
    api = p.alice_api()
    how = ensure_source(api, p.alice_a, _ns())
    ensure_known(api, p.alice_b, _ns())
    return how


def _machine(view: dict, m: FakeMachine) -> dict:
    got = [x for x in view.get("machines") or [] if x.get("id") == m.id]
    assert len(got) == 1, f"{m!r} is listed {len(got)} times: {describe_sources(view)}"
    return got[0]


def _not_bound_naming(sub_or_body, m: FakeMachine, what: str) -> None:
    """A 403 SOURCE_NOT_BOUND whose `source` names machine `m`. Takes a
    Subscription or a `(status, body)` pair."""
    if isinstance(sub_or_body, tuple):
        status, body = sub_or_body
    else:
        status, body = sub_or_body.status, sub_or_body.body
    assert (status, error_code(body)) == (403, SOURCE_NOT_BOUND), (
        f"{what}: {status} {body!r}; want 403 {SOURCE_NOT_BOUND} (never 401, which makes a broker re-mint its "
        f"certificate, nor 409, which a released broker treats as final)"
    )
    assert (body or {}).get("source") == {"machine_id": m.id, "machine_name": m.name}, \
        f"{what}: the refusal does not name {m!r}: {body!r}"


# ----------------------------------------------------------------------------
# The CLI as an identity
# ----------------------------------------------------------------------------


def _cli(who: Identity, *args: str) -> subprocess.CompletedProcess:
    """`shed <args>` as `who`, asked again while the server answers 503
    SOURCE_BUSY (a correct server may, for a moment)."""
    return cli_retrying_busy(lambda: who.cli(*args, timeout=60), f"{who.user}: shed {' '.join(args)}")


def _ok(who: Identity, *args: str) -> str:
    r = _cli(who, *args)
    assert r.returncode == 0, f"{who.user}: shed {' '.join(args)} exited {r.returncode}: {r.stdout!r} {r.stderr!r}"
    return r.stdout


def _json(who: Identity, *args: str):
    out = _ok(who, "--json", *args)
    try:
        return json.loads(out)
    except json.JSONDecodeError as e:
        raise AssertionError(f"{who.user}: shed --json {' '.join(args)} printed no JSON: {out!r}") from e


def _refused(who: Identity, *args: str) -> str:
    r = _cli(who, *args)
    assert r.returncode != 0, f"{who.user}: shed {' '.join(args)} succeeded: {r.stdout!r}"
    return r.stdout + r.stderr


def _label(m: FakeMachine) -> str:
    """How the CLI names a machine in a sentence (cmd/shed/source.go:
    sourceMachineLabel)."""
    return f'"{m.name}" ({m.id[:8]})'


_CLI_CHECKED: list[bool] = []


def _require_shed_source() -> None:
    """The cases drive `shed source`, which the CLI has from this release:
    an older `shed` first on PATH would fail every case for the wrong reason,
    so that fails once, saying so. Called only once a dev server has been
    found (a run with none skips without asking)."""
    if _CLI_CHECKED:
        return
    try:
        r = subprocess.run(["shed", "source", "--help"], capture_output=True, text=True, timeout=30)
    except FileNotFoundError:
        pytest.fail("no `shed` on PATH; put the repository's bin/ first (make build)")
    if r.returncode != 0 or "shed source use" not in r.stdout:
        pytest.fail(f"the `shed` first on PATH has no `shed source` command (exit {r.returncode}): put the "
                    f"repository's bin/ first on PATH (make build)")
    _CLI_CHECKED.append(True)


# ----------------------------------------------------------------------------
# Requests from alice's shed
# ----------------------------------------------------------------------------


@pytest.fixture(scope="module")
def alice_shed(pin_env):
    """alice's shed for the request cases, with its message channel up."""
    env = pin_env.users
    name = _shed_fixture(env, env.alice, "itest-pin")
    try:
        _wait_routed(pin_env, name)
    except BaseException as e:
        try:
            _delete(env, env.alice, name)
        except AssertionError as cleanup:
            e.add_note(str(cleanup))
        raise
    yield name
    _delete(env, env.alice, name)


def _wait_routed(p: PinEnv, shed: str) -> None:
    """Wait until the server routes `shed`'s requests to alice (its message
    channel is up), as alice's credentials certificate sees it."""
    cert = p.certs[ALICE]
    deadline = time.monotonic() + ROUTE_TIMEOUT
    while True:
        status, body = pinned_request(p.users.alice.host, cert.https_port, "/api/plugins/sheds", cert.pin, cert.pair)
        assert status == 200, f"GET /api/plugins/sheds as alice: {status} {body!r}"
        routed = {s.get("name"): s.get("user") for s in (body or {}).get("sheds") or []}
        if routed.get(shed) == ALICE:
            return
        if time.monotonic() > deadline:
            raise AssertionError(f"{shed}'s message channel did not come up within {ROUTE_TIMEOUT} s: {routed}")
        time.sleep(2)


@dataclass
class _Request:
    """One request started in a shed (`shed exec ... curl`), and the envelope
    the machine it was delivered to received."""

    proc: subprocess.Popen
    env: dict
    token: str
    started: float

    def finish(self, timeout: float = 90) -> tuple[str, str]:
        return self.proc.communicate(timeout=timeout)

    def kill(self) -> None:
        if self.proc.poll() is None:
            self.proc.kill()
            self.proc.communicate()


def _send(p: PinEnv, shed: str, ns: str, to: BusListener) -> _Request:
    """Start one request from alice's `shed` on `ns` and wait until `to` (the
    source's stream) has it, inside DELIVERY_BUDGET. Retries a start the shed
    answered NO_CONNECTION (its message channel not up yet); anything else is
    a failure that names the stream's state and alice's sources."""
    token = uuid.uuid4().hex
    for _attempt in range(4):
        started = time.monotonic()
        proc = p.users.alice.popen_cli("exec", shed, "--", *publish_argv(ns, {"itest_probe": token}))
        got = None
        while got is None and time.monotonic() - started < DELIVERY_BUDGET:
            got = to.wait_for(lambda e: payload_has(e, token), 0.5)
            if got is None and proc.poll() is not None:
                break  # the request ended in the shed without reaching anyone
        if got is not None:
            return _Request(proc, got, token, started)
        if proc.poll() is None:
            proc.kill()
            out, err = proc.communicate()
            raise AssertionError(
                f"a request from {shed} did not reach {to.name}'s stream within {DELIVERY_BUDGET} s "
                f"(stream error: {to.error}); alice's sources: {p.state()}; in the shed so far: {out!r} {err!r}"
            )
        out, err = proc.communicate(timeout=90)
        assert "NO_CONNECTION" in out + err, (
            f"a request from {shed} never reached {to.name}'s stream (stream error: {to.error}); "
            f"in the shed: {out!r} {err!r}; sources: {p.state()}"
        )
        time.sleep(5)
    raise AssertionError(f"{shed}'s bus connection never came up")


def _answer_reaches_the_shed(p: PinEnv, machine: FakeMachine, req: _Request, shed: str) -> None:
    """`machine` answers `req` inside the guest's request lifetime; the
    answer is accepted (204) and is what the request in the shed prints."""
    elapsed = time.monotonic() - req.started
    assert elapsed < ANSWER_BY, (
        f"{elapsed:.1f} s into the exchange before the source could answer; the guest gives up at "
        f"{GUEST_REQUEST_LIFETIME} s, so the answer could not be shown to arrive"
    )
    answer = uuid.uuid4().hex
    status, body = machine.answer(req.env, {"itest_answer": answer}, timeout=ANSWER_TIMEOUT)
    assert status == 204, f"{machine!r}, the source, could not answer: {status} {body!r}; {p.state()}"
    out, err = req.finish()
    assert answer in out, f"the source's answer did not reach {shed}: {out!r} {err!r}"


# ----------------------------------------------------------------------------
# Admission
# ----------------------------------------------------------------------------


def test_the_server_advertises_the_pin(pin):
    status, body = pin.alice_api().request("GET", "/api/info")
    assert status == 200, body
    assert FEATURE in (body or {}).get("features", []), f"{FEATURE} is not among the features: {body!r}"


def test_one_machine_is_the_source_and_another_is_refused_by_name(pin):
    p = pin
    api = p.alice_api()
    how = ensure_source(api, p.alice_a, _ns())
    if how == "first-connect":
        # A server that had no binding for alice: A, the first to connect, is bound.
        assert sources(api)["binding"]["bound_by"] == "first-connect", describe_sources(sources(api))
    ns = _ns()

    # Positive control: the source subscribes.
    a = p.alice_a.subscribe(ns)
    assert a.status == 200, f"{a}; {p.state()}"
    view = sources(api)
    got = _machine(view, p.alice_a)
    assert got["connected"] and ns in got["namespaces"], describe_sources(view)
    assert got["name"] == p.alice_a.name

    # Another machine of the same user, same certificate, same request type:
    # refused, naming the source.
    b = p.alice_b.subscribe(ns)
    _not_bound_naming(b, p.alice_a, "alice's machine B subscribing")
    assert p.alice_a.name in b.message, b.message
    assert b.listener is None

    # The refusal changed nothing: still A at the same generation, B is
    # remembered and not connected, and A's stream is up.
    after = sources(api)
    assert bound_id(after) == p.alice_a.id
    assert after["binding"]["generation"] == view["binding"]["generation"], (describe_sources(view), describe_sources(after))
    assert not _machine(after, p.alice_b)["connected"]
    assert a.listener.error is None, a.listener.error
    # B stays refused on another request type too (nothing fails over).
    _not_bound_naming(p.alice_b.subscribe(_ns()), p.alice_a, "alice's machine B on another request type")


def test_a_subscribe_or_answer_without_a_machine_id_is_refused(pin, alice_shed):
    p = pin
    _a_source_with_b_known(p)
    _ensure_running(p.users, p.users.alice, alice_shed)
    ns = _ns()
    a = p.alice_a
    bad = {
        "no machine headers (a released broker)": {},
        "no name header": {MACHINE_ID_HEADER: a.id},
        "an id that is not lowercase hex": {**a.headers, MACHINE_ID_HEADER: a.id.upper()},
        "an id one digit short": {**a.headers, MACHINE_ID_HEADER: a.id[:-1]},
    }
    for what, headers in bad.items():
        sub = a.subscribe(ns, headers=headers)
        assert (sub.status, sub.code) == (403, SOURCE_MACHINE_ID_REQUIRED), f"{what}: {sub}"
        assert INSTALL_APP in sub.message, f"{what}: the refusal does not say to install the desktop app: {sub}"
        assert not re.search(r"\d+\.\d+\.\d+", sub.message), f"{what}: the refusal names a version: {sub}"

    # Positive control, same certificate, same request type: with the
    # headers the subscribe is admitted.
    sub = a.subscribe(ns)
    assert sub.status == 200, f"{sub}; {p.state()}"

    # A real request pending on A: the source's own answer without the
    # headers (or with a malformed id) is refused, sent side by side ...
    req = _send(p, alice_shed, ns, sub.listener)
    try:
        forged = {"no machine headers": {}, "an id that is not lowercase hex": bad["an id that is not lowercase hex"]}
        got: dict = {}

        def answer(what: str) -> None:
            try:
                got[what] = a.answer(req.env, {"itest_answer": f"headerless-{what}"}, headers=forged[what],
                                     timeout=REFUSAL_TIMEOUT)
            except Exception as e:  # noqa: BLE001 - reported by the assertions below
                got[what] = e

        threads = [threading.Thread(target=answer, args=(w,), daemon=True) for w in forged]
        for t in threads:
            t.start()
        for t in threads:
            t.join(REFUSAL_TIMEOUT + 2)
        for what in forged:
            assert isinstance(got.get(what), tuple), f"{what}: {got.get(what)!r}"
            status, body = got[what]
            assert (status, error_code(body)) == (403, SOURCE_MACHINE_ID_REQUIRED), f"{what}: {status} {body!r}"

        # ... and did not use the request up: the same machine's proper
        # answer to it is accepted and is what the shed prints.
        _answer_reaches_the_shed(p, a, req, alice_shed)
    finally:
        req.kill()


def test_the_source_subscribing_twice_is_refused_as_a_duplicate(pin, alice_shed):
    p = pin
    _a_source_with_b_known(p)
    _ensure_running(p.users, p.users.alice, alice_shed)
    ns = _ns()
    first = p.alice_a.subscribe(ns)
    assert first.status == 200, f"{first}; {p.state()}"

    second = p.alice_a.subscribe(ns)
    assert (second.status, second.code) == (409, SOURCE_DUPLICATE_STREAM), \
        f"{second}; want 409 {SOURCE_DUPLICATE_STREAM} (a broker that sends a machine id retries it)"
    assert DUPLICATE in second.message and second.source is None, second.body

    # Nothing was replaced: the first stream is the one registered, once ...
    assert first.listener.error is None, first.listener.error
    held = _machine(sources(p.alice_api()), p.alice_a)
    assert held["connected"] and held["namespaces"].count(ns) == 1, held
    # ... and it still delivers: a request reaches it and its answer lands.
    req = _send(p, alice_shed, ns, first.listener)
    try:
        _answer_reaches_the_shed(p, p.alice_a, req, alice_shed)
    finally:
        req.kill()


# ----------------------------------------------------------------------------
# Routing: a request reaches the source only
# ----------------------------------------------------------------------------


def test_a_request_reaches_only_the_source(pin, alice_shed):
    p = pin
    _a_source_with_b_known(p)
    ensure_source(p.bob_api(), p.bob_m, _ns())
    _ensure_running(p.users, p.users.alice, alice_shed)
    ns = _ns()

    a = p.alice_a.subscribe(ns)
    assert a.status == 200, f"{a}; {p.state()}"
    _not_bound_naming(p.alice_b.subscribe(ns), p.alice_a, "alice's machine B subscribing")
    # bob's machine is his source and holds the same request type.
    bob = p.bob_m.subscribe(ns)
    assert bob.status == 200, f"{bob}; {p.state()}"

    req = _send(p, alice_shed, ns, a.listener)
    try:
        assert (req.env.get("shed") or {}).get("name") == alice_shed, req.env

        # B and bob answer it at once, side by side; each is refused.
        got: dict = {}

        def answer(m: FakeMachine) -> None:
            try:
                got[m.name] = m.answer(req.env, {"itest_answer": f"forged-by-{m.name}"}, timeout=REFUSAL_TIMEOUT)
            except Exception as e:  # noqa: BLE001 - reported by the assertions below
                got[m.name] = e

        threads = [threading.Thread(target=answer, args=(m,), daemon=True) for m in (p.alice_b, p.bob_m)]
        for t in threads:
            t.start()
        for t in threads:
            t.join(REFUSAL_TIMEOUT + 2)
        # alice's other machine: not the source, named as such.
        assert isinstance(got.get(p.alice_b.name), tuple), got
        _not_bound_naming(got[p.alice_b.name], p.alice_a, "alice's machine B answering her request")
        # bob's machine (his own source): not bob's request.
        assert isinstance(got.get(p.bob_m.name), tuple), got
        status, body = got[p.bob_m.name]
        assert (status, error_code(body)) == (403, "FORBIDDEN") and UNMATCHED in error_message(body), \
            f"bob answered alice's request: {status} {body!r}"

        # The source's answer is the one the shed gets.
        _answer_reaches_the_shed(p, p.alice_a, req, alice_shed)
    finally:
        req.kill()

    # bob's stream shows it still delivers after the exchange, so its
    # silence is evidence: it never received alice's request.
    done = time.monotonic()
    assert bob.listener.alive_after(done, LIVENESS_TIMEOUT), (
        f"bob's stream showed no sign of life within {LIVENESS_TIMEOUT} s ({bob.listener.error or 'no keepalive'}), "
        f"so its silence proves nothing"
    )
    assert not bob.listener.matching(lambda e: payload_has(e, req.token)), "alice's request reached bob's machine"


# ----------------------------------------------------------------------------
# Moving the source
# ----------------------------------------------------------------------------


def test_moving_the_source_ends_the_old_stream_and_drops_its_requests(pin, alice_shed):
    p = pin
    alice = p.users.alice
    api = p.alice_api()
    _a_source_with_b_known(p)
    _ensure_running(p.users, alice, alice_shed)
    ns = _ns()
    a = p.alice_a.subscribe(ns)
    assert a.status == 200, f"{a}; {p.state()}"

    pending: Optional[_Request] = None
    nxt: Optional[_Request] = None
    again: Optional[_Request] = None
    try:
        # A request is pending on A (delivered, not answered) ...
        pending = _send(p, alice_shed, ns, a.listener)

        # ... when alice moves the source to B, by name after `--`.
        out = _ok(alice, "source", "use", "--", p.alice_b.name)
        assert f"Machine {_label(p.alice_b)} is now the credential source for you on {alice.entry}." in out, out
        assert f"Machine {_label(p.alice_a)} no longer answers." in out, out
        moved = sources(api)
        assert bound_id(moved) == p.alice_b.id, describe_sources(moved)
        moved_gen = moved["binding"]["generation"]

        # A's stream ends at once ...
        assert a.listener.wait_ended(STREAM_END_TIMEOUT), \
            f"A's stream did not end within {STREAM_END_TIMEOUT} s of the move; {p.state()}"
        # ... its late answer to the pending request is refused, naming B ...
        late = uuid.uuid4().hex
        _not_bound_naming(p.alice_a.answer(pending.env, {"itest_answer": late}, timeout=REFUSAL_TIMEOUT),
                          p.alice_b, "A answering after the move")
        # ... and it does not come back on its own: its next subscribe is
        # refused naming B.
        _not_bound_naming(p.alice_a.subscribe(ns), p.alice_b, "A subscribing after the move")

        # B is the source, not connected until it subscribes.
        st = _json(alice, "source", "status")
        assert st["source"]["id"] == p.alice_b.id and st["source"]["connected"] is False, st
        b = p.alice_b.subscribe(ns)
        assert b.status == 200, f"{b}; {p.state()}"

        # B receives the next request, and never the one pending on A: a
        # request kept for re-delivery would have been queued ahead of it.
        nxt = _send(p, alice_shed, ns, b.listener)
        assert not b.listener.matching(lambda e: payload_has(e, pending.token)), \
            "the request pending on A was delivered to B"
        _answer_reaches_the_shed(p, p.alice_b, nxt, alice_shed)

        # The pending request was answered by nobody: the guest gave up on it.
        out1, err1 = pending.finish(timeout=GUEST_REQUEST_LIFETIME + 60)
        assert late not in out1, f"A's refused answer reached the shed: {out1!r}"
        assert "TIMEOUT" in out1, f"the request pending on A got something other than the guest's timeout: {out1!r} {err1!r}"
        # B's stream shows it still delivers after the guest gave up, so
        # anything the server queued for B before that has been read: only
        # then does B's silence about the pending request count.
        gave_up = time.monotonic()
        assert b.listener.alive_after(gave_up, LIVENESS_TIMEOUT), (
            f"B's stream showed no sign of life within {LIVENESS_TIMEOUT} s after the guest gave up "
            f"({b.listener.error or 'no keepalive'}), so its silence proves nothing"
        )
        assert not b.listener.matching(lambda e: payload_has(e, pending.token)), \
            "the request pending on A was delivered to B"

        # Nothing fails back on its own: with B gone, A is still refused
        # naming B, and the binding is where the move put it.
        p.alice_b.close()
        deadline = time.monotonic() + LIVENESS_TIMEOUT
        while _machine(sources(api), p.alice_b)["connected"]:
            assert time.monotonic() < deadline, \
                f"the server still shows B connected {LIVENESS_TIMEOUT} s after it went: {p.state()}"
            time.sleep(1)
        _not_bound_naming(p.alice_a.subscribe(_ns()), p.alice_b, "A subscribing while B is disconnected")
        after = sources(api)
        assert bound_id(after) == p.alice_b.id and after["binding"]["generation"] == moved_gen, \
            (moved_gen, describe_sources(after))

        # B reconnects and answers again.
        ns2 = _ns()
        b2 = p.alice_b.subscribe(ns2)
        assert b2.status == 200, f"B reconnecting: {b2}; {p.state()}"
        again = _send(p, alice_shed, ns2, b2.listener)
        _answer_reaches_the_shed(p, p.alice_b, again, alice_shed)
    finally:
        for r in (pending, nxt, again):
            if r is not None:
                r.kill()


# ----------------------------------------------------------------------------
# shed source ls | status | use
# ----------------------------------------------------------------------------


def test_shed_source_as_the_user(pin):
    p = pin
    alice = p.users.alice
    entry = alice.entry
    api = p.alice_api()
    _a_source_with_b_known(p)

    # ls --json: alice's machines only, A bound.
    view = _json(alice, "source", "ls")
    assert view["user"] == ALICE, view
    assert bound_id(view) == p.alice_a.id, describe_sources(view)
    assert {p.alice_a.id, p.alice_b.id} <= known_ids(view), describe_sources(view)
    assert p.bob_m.id not in known_ids(view), "bob's machine is listed among alice's"
    a_row, b_row = _machine(view, p.alice_a), _machine(view, p.alice_b)
    assert (a_row["name"], b_row["name"]) == (p.alice_a.name, p.alice_b.name)
    # Both presented alice's one certificate: the same key, shown.
    assert a_row["key_fingerprint"] and a_row["key_fingerprint"] == b_row["key_fingerprint"], (a_row, b_row)

    # ls, text: one row per machine, A the bound one.
    out = _ok(alice, "source", "ls")
    lines = out.splitlines()
    assert lines and lines[0].split()[:5] == ["MACHINE", "ID", "KEY", "BOUND", "CONNECTED"], out
    rows = {cols[0]: cols for cols in (line.split() for line in lines[1:]) if cols}
    assert rows[p.alice_a.name][1:4:2] == [p.alice_a.id[:8], "yes"], out
    assert rows[p.alice_b.name][1:4:2] == [p.alice_b.id[:8], "no"], out

    # status, with A connected.
    ns = _ns()
    a = p.alice_a.subscribe(ns)
    assert a.status == 200, f"{a}; {p.state()}"
    out = _ok(alice, "source", "status")
    assert f"Credential source for you on {entry}: {_label(p.alice_a)}, connected (" in out and ns in out, out
    st = _json(alice, "source", "status")
    assert (st["server"], st["user"]) == (entry, ALICE), st
    assert st["source"]["id"] == p.alice_a.id and st["source"]["name"] == p.alice_a.name, st
    assert st["source"]["connected"] is True and ns in st["source"]["namespaces"], st

    # use, by name after `--`: B; A's stream ends.
    gen = sources(api)["binding"]["generation"]
    out = _ok(alice, "source", "use", "--", p.alice_b.name)
    assert f"Machine {_label(p.alice_b)} is now the credential source for you on {entry}." in out, out
    assert a.listener.wait_ended(STREAM_END_TIMEOUT), "A's stream did not end when the source moved"
    view = sources(api)
    assert bound_id(view) == p.alice_b.id and view["binding"]["generation"] > gen, describe_sources(view)
    assert view["binding"]["bound_by"].startswith(f"user {ALICE}"), view["binding"]

    # use, by id prefix (8 digits): back to A.
    out = _ok(alice, "source", "use", p.alice_a.id[:8])
    assert f"Machine {_label(p.alice_a)} is now the credential source" in out, out
    view = sources(api)
    assert bound_id(view) == p.alice_a.id, describe_sources(view)
    # The machine already bound: nothing changes, and it says so.
    gen = view["binding"]["generation"]
    out = _ok(alice, "source", "use", p.alice_a.id[:12])
    assert f"Machine {_label(p.alice_a)} is already the credential source for you on {entry}." in out, out
    view = sources(api)
    assert bound_id(view) == p.alice_a.id and view["binding"]["generation"] == gen, describe_sources(view)

    # --json, by full id: a real move, to B. It prints the sources as the
    # move left them, and the server, read on its own, agrees.
    printed = _json(alice, "source", "use", p.alice_b.id)
    assert bound_id(printed) == p.alice_b.id and printed["binding"]["generation"] > gen, describe_sources(printed)
    view = sources(api)
    assert bound_id(view) == p.alice_b.id, describe_sources(view)
    assert view["binding"]["generation"] == printed["binding"]["generation"], \
        (describe_sources(printed), describe_sources(view))
    gen = view["binding"]["generation"]

    # A machine alice never presented (bob's) is not hers to choose; nothing
    # changes.
    out = _refused(alice, "source", "use", p.bob_m.id)
    assert f"nothing changed on {entry}" in out, out
    view = sources(api)
    assert bound_id(view) == p.alice_b.id and view["binding"]["generation"] == gen, describe_sources(view)


# ----------------------------------------------------------------------------
# Who may read and move a binding
# ----------------------------------------------------------------------------


def test_an_admin_reads_and_moves_another_users_source(pin):
    p = pin
    admin, entry = p.users.admin, p.users.admin.entry
    _a_source_with_b_known(p)
    ensure_source(p.bob_api(), p.bob_m, _ns())
    alice_api, admin_api = p.alice_api(), admin.api()

    # Reads alice's: her machines only.
    view = _json(admin, "source", "ls", "--user", ALICE)
    assert view["user"] == ALICE and bound_id(view) == p.alice_a.id, describe_sources(view)
    assert {p.alice_a.id, p.alice_b.id} <= known_ids(view) and p.bob_m.id not in known_ids(view), describe_sources(view)
    out = _ok(admin, "source", "status", "--user", ALICE)
    assert f"Credential source for user {ALICE} on {entry}: {_label(p.alice_a)}" in out, out
    # The admin's own view is the admin's, with none of alice's machines.
    own = sources(admin_api)
    assert own["user"] == ADMIN and not ({p.alice_a.id, p.alice_b.id} & known_ids(own)), describe_sources(own)

    # Moves alice's to B; the binding records who moved it.
    out = _ok(admin, "source", "use", "--user", ALICE, "--", p.alice_b.name)
    assert f"Machine {_label(p.alice_b)} is now the credential source for user {ALICE} on {entry}." in out, out
    view = sources(alice_api)
    assert bound_id(view) == p.alice_b.id, describe_sources(view)
    assert view["binding"]["bound_by"].startswith(f"admin {ADMIN}"), view["binding"]
    gen = view["binding"]["generation"]

    # Only alice's machines: bob's machine is not one of them.
    status, body = admin_api.request("POST", "/api/sources/use", {"machine_id": p.bob_m.id, "user": ALICE})
    assert (status, error_code(body)) == (404, SOURCE_MACHINE_NOT_FOUND), (status, body)
    out = _refused(admin, "source", "use", "--user", ALICE, p.bob_m.id)
    assert f"nothing changed on {entry}" in out, out
    view = sources(alice_api)
    assert bound_id(view) == p.alice_b.id and view["binding"]["generation"] == gen, describe_sources(view)
    # Control: the same request for bob, whose machine it is, is accepted
    # (it is bob's source already, so nothing moves).
    status, body = admin_api.request("POST", "/api/sources/use", {"machine_id": p.bob_m.id, "user": BOB})
    assert status == 200 and bound_id(body) == p.bob_m.id, (status, body)

    # A user that is not configured is a bad request for an admin.
    status, body = admin_api.request("GET", "/api/sources?user=itest-nobody")
    assert (status, error_code(body)) == (400, "INVALID_REQUEST"), (status, body)

    # And back to A, by id prefix.
    out = _ok(admin, "source", "use", "--user", ALICE, p.alice_a.id[:8])
    assert f"Machine {_label(p.alice_a)} is now the credential source for user {ALICE}" in out, out
    assert bound_id(sources(alice_api)) == p.alice_a.id


def test_another_user_a_script_key_and_a_machine_cannot_read_or_move_a_source(pin, enrolled):
    p = pin
    env = p.users
    _a_source_with_b_known(p)
    alice_api, bob_api = p.alice_api(), p.bob_api()
    before = sources(alice_api)

    # bob: his own, yes (the control) ...
    assert sources(bob_api)["user"] == BOB
    # ... alice's, no: one fixed 403, the same as for a user that does not
    # exist, for the read and the move.
    refusals = [
        bob_api.request("GET", f"/api/sources?user={ALICE}"),
        bob_api.request("GET", "/api/sources?user=itest-nobody"),
        bob_api.request("POST", "/api/sources/use", {"machine_id": p.alice_b.id, "user": ALICE}),
        bob_api.request("POST", "/api/sources/use", {"machine_id": p.alice_b.id, "user": "itest-nobody"}),
    ]
    for status, body in refusals:
        assert (status, error_code(body)) == (403, "FORBIDDEN") and error_message(body) == NOT_ADMIN, (status, body)
    assert len({json.dumps(b, sort_keys=True) for _, b in refusals}) == 1, f"the refusal differs: {refusals}"
    out = _refused(env.bob, "source", "ls", "--user", ALICE)
    assert NOT_ADMIN in out, out

    # A script key of alice's: refused both routes, while her own key reads
    # (the `before` above) ...
    ctl = env.alice_script.enrol("control", enrolled / "script-control")
    script = env.alice_script.pinned(ctl)
    for method, path, body in (("GET", "/api/sources", None),
                               ("POST", "/api/sources/use", {"machine_id": p.alice_b.id})):
        status, resp = script.request(method, path, body)
        assert (status, error_code(resp)) == (403, "FORBIDDEN") and error_message(resp) == SCRIPT_KEY, \
            (method, status, resp)
    # ... and the same certificate is alice's otherwise (a control route).
    status, resp = script.request("GET", "/api/sheds")
    assert status == 200, (status, resp)

    # A credentials certificate (what a machine holds) does not reach the
    # routes at all: they need the control scope.
    cred = p.certs[ALICE]
    for method, path, body in (("GET", "/api/sources", None),
                               ("POST", "/api/sources/use", {"machine_id": p.alice_b.id})):
        status, resp = pinned_request(env.alice.host, cred.https_port, path, cred.pin, cred.pair,
                                      method=method, body=body)
        assert (status, error_code(resp)) == (403, "FORBIDDEN") and error_message(resp) == CONTROL_SCOPE, \
            (method, path, status, resp)

    # Nothing moved.
    after = sources(alice_api)
    assert bound_id(after) == p.alice_a.id, describe_sources(after)
    assert after["binding"]["generation"] == before["binding"]["generation"], (describe_sources(before), describe_sources(after))

    # Control: the same move, by alice with her own key, moves (to B and
    # back), each step read back with a new generation (the counter is the
    # server's, shared by every user, so it only ever grows).
    gen = after["binding"]["generation"]
    for target in (p.alice_b, p.alice_a):
        status, body = alice_api.request("POST", "/api/sources/use", {"machine_id": target.id})
        assert status == 200 and bound_id(body) == target.id, (target, status, body)
        view = sources(alice_api)
        assert bound_id(view) == target.id and view["binding"]["generation"] > gen, \
            (target, gen, describe_sources(view))
        assert view["binding"]["generation"] == body["binding"]["generation"], (body, describe_sources(view))
        assert view["binding"]["bound_by"].startswith(f"user {ALICE}"), view["binding"]
        gen = view["binding"]["generation"]


# ----------------------------------------------------------------------------
# A server with no user list: read only
# ----------------------------------------------------------------------------


def _developer_api(name: str):
    """`(Pinned client, entry)` for the developer's own entry `name` in
    `~/.shed/config.yaml`, with its client certificate; skips when the entry
    has no certificate (not an mtls entry)."""
    path = Path.home() / ".shed" / "config.yaml"
    data = yaml.safe_load(path.read_text(encoding="utf-8")) or {}
    e = (data.get("servers") or {}).get(name)
    if not isinstance(e, dict) or not e.get("client_cert_file") or not e.get("tls_cert_fingerprint"):
        pytest.skip(f"{name!r} has no mtls client certificate in {path}")
    url = urlparse(e["api_url"])
    return Pinned(url.hostname, url.port, e["tls_cert_fingerprint"], e["client_cert_file"], e["client_key_file"]), e


def test_the_developers_own_source_is_readable(shed_server_dev):
    """On the developer's own entry, whatever the server's user list: the
    feature is advertised and `shed source status` reads (text and JSON).
    Nothing is bound or moved: on a server with no user list the only user
    is the developer's, whose real desktop app may be the source."""
    server = shed_server_dev
    _require_shed_source()
    # The entry's certificate names the user its key had when the CLI last
    # enrolled; the [vz] users cases restart the Mac dev server with the list
    # and back, so let the CLI re-enrol under the current one before the raw
    # client below reads the certificate file.
    r = cli_retrying_busy(lambda: server.cli("list"), "shed list")
    assert r.returncode == 0, f"shed list: {r.returncode} {r.stdout!r} {r.stderr!r}"
    api, _ = _developer_api(server.name)
    status, info = api.request("GET", "/api/info")
    assert status == 200 and FEATURE in (info or {}).get("features", []), (status, info)

    view = sources(api)
    backend = "fc" if server.backend == "firecracker" else server.backend
    # The implicit user ("") of a server with no user list, or `dev` (the
    # developer, an admin) where the server was started with the itest list.
    # (The [vz] users cases restart the Mac dev server with the list for the
    # length of a module, so either can be met there.)
    want = {DEV_USER} if backend == "fc" and started_with_users("fc") else {"", DEV_USER}
    assert view["user"] in want, view

    r = cli_retrying_busy(lambda: server.cli("source", "status"), "shed source status")
    assert r.returncode == 0, f"shed source status: {r.returncode} {r.stdout!r} {r.stderr!r}"
    assert (f"Credential source for you on {server.name}: " in r.stdout
            or f"No credential source for you on {server.name} yet" in r.stdout), r.stdout
    r = cli_retrying_busy(lambda: server.cli("--json", "source", "status"), "shed --json source status")
    assert r.returncode == 0, r.stderr
    st = json.loads(r.stdout)
    assert st["server"] == server.name and st["user"] == view["user"], st
    if bound_id(view) is not None:
        # A binding stays until it is moved, and nothing here moves it. (With
        # none, the developer's broker may bind between the two reads.)
        assert (st["source"] or {}).get("id") == bound_id(view), (st, describe_sources(view))
