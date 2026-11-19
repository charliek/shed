"""A server whose users come from an account service, end to end, on this host.

Each case gets a fresh world (`fixtures/directory.py`): the in-tree reference
server of the directory contract and a private `shed-server` from `bin/`, on
loopback ports of their own, the server on the remote backend's test drivers
(a "shed" is a shed-agent on this host, as the user running the tests), the
server enrolled with a code passed on standard input, one local admin
(`localadmin`) and the people a case puts in the list. Everything is stopped
and removed when the case ends.

What it shows, live, over real SSH connections and the real HTTPS API:

  - a new person in the list logs in through the unknown-key pull, over SSH
    and the API, with no restart; an admin flip and a demotion take effect
    with the same pinned certificate; a removal plus `shed users refresh`
    closes their held connection and refuses SSH and the API; the kept copy
    carries them through an outage and a restart, up to the end of the trust
    time (computed here from the last good pull), which closes their
    connection, removes the copy and leaves only the local admin, until a
    refresh after the outage restores the list (`test_end_to_end_flow`);
  - a list entry named like the local admin is skipped, the local admin
    unchanged; so is an entry with another name that lists the local
    admin's key, and the local admin keeps that key;
  - a key that moves between two people: a new channel on the held
    connection is refused, the first person's certificate is refused, a new
    connection is the second person's;
  - the unknown-key pull's rate: a second unknown key in the same minute is
    refused at once;
  - an unknown key under a name that is no shed asks for no pull; under an
    existing shed's name it is refused at once, the server pulls in the
    background, and a retry is let in;
  - a certificate that names no user is refused;
  - a session inside a shed is closed when its person is removed, and an
    open session survives a key move while a new one, and the person's CLI
    certificate, are refused.

An API refusal is always the server's own (an HTTP 401 or 403 with its
error code, or its bad_certificate alert at the TLS handshake), followed by a
request the server answers; a request that got no answer fails the case
(`fixtures.directory.api_request`).

Run it with `make test-integration-directory` (Linux only: it builds bin/
and the reference server, then runs this module); under any other target it
skips. A subset: `make test-integration-directory PYTEST_ARGS="-k rates"`.
"""

from __future__ import annotations

import time
import uuid
from datetime import datetime

import pytest

from fixtures.directory import (
    BOOTSTRAP_NOT_AUTHORIZED,
    KEY_MOVED,
    LOCAL_ADMIN,
    SKIPPED_LOCAL_ADMIN_KEY,
    SKIPPED_LOCAL_ADMIN_NAME,
    SHED_NAME_PULL,
    SKIPPED_LOG,
    STARTUP_KEPT_COPY,
    STARTUP_NO_COPY,
    TRUST_ENDED,
    TRUST_TICK,
    UNKNOWN_KEY_PULL,
    UNKNOWN_KEY_WAIT,
    World,
    directory_world,
    parse_stamp,
    skip_reason,
    trust_deadline,
)

_reason = skip_reason()
if _reason:
    pytest.skip(_reason, allow_module_level=True)

# How long a closed connection may take to end on the client's side once the
# server has published the list without its user.
CLOSE_WITHIN = 20
# The trust-time check: how long before the computed end the person must
# still be admitted, and the least trust time a slow restart may leave for the
# checks before the end (else a new trust time is started first).
BEFORE_END = 10
MIN_TRUST_LEFT = 45


@pytest.fixture
def world():
    with directory_world() as w:
        yield w


def _create(world: World, who, name: str) -> None:
    r = who.cli("create", name, "--no-provision", timeout=180)
    assert r.returncode == 0, f"{who.user}: shed create {name}: {r.stdout[-600:]!r} {r.stderr[-600:]!r}"


def _marker() -> str:
    return f"itest-{uuid.uuid4().hex[:10]}"


def _left(deadline: datetime) -> float:
    return deadline.timestamp() - time.time()


# ----------------------------------------------------------------------------
# The whole flow
# ----------------------------------------------------------------------------


@pytest.mark.slow
def test_end_to_end_flow(world):
    w = world

    # Enrolled: the state is private and the code is kept nowhere (nor any
    # private key outside the files meant to hold one).
    w.assert_state_private()
    assert w.code not in w.enrol_output
    assert w.secrets_found() == []
    st = w.status()
    assert st["source"] == "directory"
    assert st["directory"]["client_id"] == w.client_id
    assert st["directory"]["state"] == "trusted"
    assert w.user_row(LOCAL_ADMIN, st) == {"name": LOCAL_ADMIN, "admin": True, "source": "local"}
    w.assert_local_admin_works("enrolled")

    # A new person logs in over SSH through the unknown-key pull (the first
    # unknown key of this world, inside the first minute), then over the API.
    k = w.key("dana")
    w.set_user("dana", [k])
    w.publish()
    dana = w.identity("dana", k)
    before = w.server.log().count(UNKNOWN_KEY_PULL)
    dana.add_server()
    assert w.server.log().count(UNKNOWN_KEY_PULL) == before + 1, "the login did not go through the unknown-key pull"
    r = dana.cli("list")
    assert r.returncode == 0, r.stderr
    assert w.user_row("dana") == {"name": "dana", "admin": False, "source": "directory"}
    dana_cert = w.cli_cert(dana)
    w.assert_local_admin_works("a new person")

    # An admin flip and a demotion, with the same pinned certificate.
    a = dana_cert.get("/api/users/status")
    assert (a.status, a.refusal) == (403, "HTTP 403 FORBIDDEN"), a
    w.set_user("dana", [k], admin=True)
    w.publish()
    w.refresh_ok()
    a = dana_cert.get("/api/users/status")
    assert a.status == 200, a
    assert w.user_row("dana")["admin"] is True
    w.set_user("dana", [k], admin=False)
    w.publish()
    w.refresh_ok()
    a = dana_cert.get("/api/users/status")
    assert (a.status, a.refusal) == (403, "HTTP 403 FORBIDDEN"), a
    assert w.user_row("dana")["admin"] is False

    # Removed: the refresh closes their held connection; SSH and the API
    # refuse them; their sheds would stay (the session cases show it).
    held = w.held(dana)
    assert held.probe().admitted
    w.remove_user("dana")
    w.publish()
    w.refresh_ok()
    assert held.wait_closed(CLOSE_WITHIN), "the removed person's held connection is still open"
    p = w.probe(dana)
    assert p.refused, p
    w.assert_api_refused(dana_cert, "a removed person")
    assert w.user_row("dana") is None
    w.assert_local_admin_works("a removal")

    # Added again: a good pull lets them back in, the same certificate too.
    w.set_user("dana", [k])
    w.publish()
    w.refresh_ok()
    p = w.probe(dana)
    assert p.admitted, p
    a = dana_cert.get("/api/sheds")
    assert a.status == 200, a

    # The service is out: the kept copy carries them, across a restart.
    w.ref.outage(True)
    assert w.probe(dana).admitted
    w.server.restart()
    assert STARTUP_KEPT_COPY in w.server.log(), w.server.log()[-2000:]
    st = w.status()
    assert st["directory"]["state"] == "trusted", st["directory"]
    p = w.probe(dana)
    assert p.admitted, p
    a = dana_cert.get("/api/sheds")
    assert a.status == 200, a
    w.assert_local_admin_works("an outage, after a restart")

    # The trust time, computed here: the last good pull plus the configured
    # trust. A restart slow enough to leave too little of it for the checks
    # below starts a new one (a good pull, then the outage again) rather than
    # failing.
    deadline = trust_deadline(st["directory"])
    if _left(deadline) < MIN_TRUST_LEFT:
        w.ref.outage(False)
        w.refresh_ok()
        w.ref.outage(True)
        st = w.status()
        deadline = trust_deadline(st["directory"])
        assert _left(deadline) >= MIN_TRUST_LEFT, (deadline.isoformat(), st["directory"])
    assert parse_stamp(st["directory"]["trusted_until"]) == deadline, st["directory"]
    held = w.held(dana)
    assert held.probe().admitted

    # Shortly before the end the person is still admitted: an API request and
    # a new channel on the held connection, both inside the trust time.
    time.sleep(max(0.0, _left(deadline) - BEFORE_END))
    a = dana_cert.get("/api/sheds")
    p = held.probe()
    assert _left(deadline) > 0, "the checks before the end of the trust time ran past it: they prove nothing"
    assert a.status == 200, f"refused {_left(deadline):.1f} s before the end of the trust time: {a}"
    assert p.admitted, f"a new channel refused {_left(deadline):.1f} s before the end of the trust time: {p}"

    # After it: only the local admin; the held connection closes.
    assert held.wait_closed(max(0.0, _left(deadline)) + TRUST_TICK + CLOSE_WITHIN), (
        f"the held connection outlived the trust time ({deadline.isoformat()})")
    assert _left(deadline) <= 0, f"the held connection closed {_left(deadline):.1f} s before the end of the trust time"
    st = w.wait_state("expired", 30)
    assert w.user_row("dana", st) is None
    assert TRUST_ENDED in w.server.log()
    p = w.probe(dana)
    assert p.refused, p
    w.assert_api_refused(dana_cert, "the trust time ended")
    w.assert_local_admin_works("the trust time ended")
    # This server removes the kept copy when the trust time ends, so a restart
    # has none. That is its decided behaviour (an expired copy would be
    # refused at the next start anyway); the contract asks only that an
    # expired copy is not used, not that it is removed.
    assert not (w.state / "directory" / "users.json").exists()
    w.server.restart()
    assert STARTUP_NO_COPY in w.server.log(), w.server.log()[-2000:]
    assert w.status()["directory"]["state"] == "no-copy"
    assert w.probe(dana).refused
    w.assert_local_admin_works("a restart with no kept copy")

    # The service is back: the local admin's refresh restores the list.
    w.ref.outage(False)
    w.refresh_ok()
    st = w.status()
    assert st["directory"]["state"] == "trusted", st["directory"]
    assert w.user_row("dana", st) == {"name": "dana", "admin": False, "source": "directory"}
    p = w.probe(dana)
    assert p.admitted, p
    a = dana_cert.get("/api/sheds")
    assert a.status == 200, a
    w.assert_local_admin_works("the list restored")
    w.assert_state_private()
    assert w.secrets_found() == []


# ----------------------------------------------------------------------------
# The local admin
# ----------------------------------------------------------------------------


def test_a_list_entry_named_like_the_local_admin_is_skipped(world):
    w = world
    impostor_key, pat_key = w.key("impostor"), w.key("pat")
    w.set_user(LOCAL_ADMIN, [impostor_key], admin=True)
    w.set_user("pat", [pat_key])
    w.publish()
    w.refresh_ok()

    st = w.status()
    skipped = {s["name"]: s["reason"] for s in st["directory"]["skipped"]}
    assert skipped == {LOCAL_ADMIN: SKIPPED_LOCAL_ADMIN_NAME}, st["directory"]["skipped"]
    # The local entry wins whole; the rest of the list loads.
    assert w.user_row(LOCAL_ADMIN, st) == {"name": LOCAL_ADMIN, "admin": True, "source": "local"}
    assert w.user_row("pat", st) == {"name": "pat", "admin": False, "source": "directory"}
    p = w.probe(w.identity("pat", pat_key))
    assert p.admitted, p
    # No list key is added to the local admin.
    p = w.probe(w.identity(LOCAL_ADMIN, impostor_key))
    assert p.refused, p
    assert SKIPPED_LOG.format(name=LOCAL_ADMIN) in w.server.log()
    w.assert_local_admin_works("a list entry with the local admin's name")


def test_a_list_entry_holding_the_local_admins_key_is_skipped(world):
    w = world
    mallory_key, pat_key = w.key("mallory"), w.key("pat")
    # Another name, listing the local admin's key (and one of its own), as an
    # admin of the service.
    w.set_user("mallory", [w.admin_key, mallory_key], admin=True)
    w.set_user("pat", [pat_key])
    w.publish()
    w.refresh_ok()

    st = w.status()
    skipped = {s["name"]: s["reason"] for s in st["directory"]["skipped"]}
    assert skipped == {"mallory": SKIPPED_LOCAL_ADMIN_KEY}, st["directory"]["skipped"]
    assert w.user_row("mallory", st) is None
    assert w.user_row(LOCAL_ADMIN, st) == {"name": LOCAL_ADMIN, "admin": True, "source": "local"}
    assert w.user_row("pat", st) == {"name": "pat", "admin": False, "source": "directory"}
    assert SKIPPED_LOG.format(name="mallory") in w.server.log()
    p = w.probe(w.identity("pat", pat_key))
    assert p.admitted, p

    # The local admin keeps the key: a new connection with it is the local
    # admin's, over SSH and for a certificate.
    w.assert_local_admin_works("a list entry with the local admin's key")
    cert = w.enrol_cert(w.identity(LOCAL_ADMIN, w.admin_key))
    assert cert.user() == LOCAL_ADMIN, cert.subject()
    a = cert.get("/api/users/status")
    assert a.status == 200, a
    # The entry is skipped whole: its own other key opens nothing.
    p = w.probe(w.identity("mallory", mallory_key))
    assert p.refused, p


# ----------------------------------------------------------------------------
# A key that moves
# ----------------------------------------------------------------------------


def test_a_key_moved_between_two_people_with_a_held_connection(world):
    w = world
    moving, erin_other, frank_own = w.key("moving"), w.key("erin-other"), w.key("frank-own")
    w.set_user("erin", [moving, erin_other])
    w.set_user("frank", [frank_own])
    w.publish()
    w.refresh_ok()
    erin = w.identity("erin", moving)

    held = w.held(erin)
    p = held.probe()
    assert p.admitted, p
    # A new connection with the key is erin's, and so is the certificate it
    # gets, which the API answers (the controls for the checks below).
    erin_cert = w.enrol_cert(erin)
    assert erin_cert.user() == "erin"
    a = erin_cert.get("/api/sheds")
    assert a.status == 200, a

    # The key moves to frank; erin keeps another, so she stays in the list.
    w.set_user("erin", [erin_other])
    w.set_user("frank", [frank_own, moving])
    w.publish()
    w.refresh_ok()

    assert held.alive() and held.check(), "erin's connection was closed, but erin is still in the list"
    p = held.probe()
    assert not p.admitted, f"a new channel on erin's connection was admitted after her key moved: {p}"
    assert BOOTSTRAP_NOT_AUTHORIZED in p.stderr, p
    assert held.alive(), "the refused channel took the connection down"
    # Erin's certificate names erin, whose key is frank's now: refused.
    w.assert_api_refused(erin_cert, "erin's certificate after her key moved")
    # A new connection with the key is frank's.
    frank_cert = w.enrol_cert(w.identity("frank", moving))
    assert frank_cert.user() == "frank"
    a = frank_cert.get("/api/sheds")
    assert a.status == 200, a


# ----------------------------------------------------------------------------
# The unknown-key pull's rate
# ----------------------------------------------------------------------------


def test_the_unknown_key_rates(world):
    w = world
    k1 = w.key("carl")
    w.set_user("carl", [k1])
    w.publish()
    carl = w.identity("carl", k1)
    before = w.server.log().count(UNKNOWN_KEY_PULL)
    p = w.probe(carl)
    assert p.admitted, p
    assert w.server.log().count(UNKNOWN_KEY_PULL) == before + 1

    # A second new person inside the same minute: the slot is taken, the key
    # is refused at once (no pull, no wait).
    k2 = w.key("dora")
    w.set_user("dora", [k2])
    w.publish()
    dora = w.identity("dora", k2)
    p = w.probe(dora)
    assert p.refused, p
    assert p.seconds < UNKNOWN_KEY_WAIT / 2, f"the second unknown key waited for a pull: {p}"
    assert w.server.log().count(UNKNOWN_KEY_PULL) == before + 1, "the second unknown key pulled"

    # It was the rate, not the list: after the local admin's refresh she is in.
    w.refresh_ok()
    p = w.probe(dora)
    assert p.admitted, p


# How long the server's off-path check of a shed name may take (its 2 s
# deadline, internal/sshd) plus room: after it, a check has ended.
SHED_CHECK_SETTLE = 4


def _ssh_timed(w: World, who, shed: str):
    t0 = time.monotonic()
    r = w.ssh_run(who, shed, "true")
    return r, time.monotonic() - t0


def test_an_unknown_key_as_a_name_that_is_no_shed_asks_no_pull(world):
    """A smoke test over real SSH, not a proof of the rule's timing.

    The sleep does not establish that the server's check has ended (its
    deadline starts inside the check, and a backend may ignore it), and the
    log is read only up to then: it shows no pull was asked in that interval.
    `took < 4` catches a login that waits for an 8-second pull, not a shorter
    inline wait. The rule itself, its timing and its bounds are held by the
    Go tests (internal/sshd unknownshed_test.go, internal/directory
    runtime_unknown_shed_test.go).
    """
    w = world
    k = w.key("nell")
    w.set_user("nell", [k])
    w.publish()
    nell = w.identity("nell", k)
    log = w.server.log()
    first_logins, shed_names = log.count(UNKNOWN_KEY_PULL), log.count(SHED_NAME_PULL)

    # Listed on the service, unknown to the server: refused at once, under a
    # valid shed name that no shed has, and no pull is asked for, then or
    # once the server's check of the name has ended.
    r, took = _ssh_timed(w, nell, "no-such-shed")
    assert r.returncode == 255 and "Permission denied" in r.stderr, (r.returncode, r.stderr)
    assert took < UNKNOWN_KEY_WAIT / 2, f"the login waited {took:.1f} s"
    time.sleep(SHED_CHECK_SETTLE)
    log = w.server.log()
    assert log.count(SHED_NAME_PULL) == shed_names, "a name that is no shed asked for a pull"
    assert log.count(UNKNOWN_KEY_PULL) == first_logins, "a name that is no shed took the first-login slot"

    # It was the rule, not the list: after the local admin's refresh she is in.
    w.refresh_ok()
    p = w.probe(nell)
    assert p.admitted, p


def test_an_unknown_key_as_an_existing_sheds_name_is_refused_then_let_in(world):
    """A smoke test over real SSH, not a proof of the rule's timing.

    `took < 4` catches a login that waits for an 8-second pull, not a shorter
    inline wait, and says nothing about whether the two names' refusals take
    the same time; the retry loop allows 30 seconds. The rule itself, its
    timing and its bounds are held by the Go tests (internal/sshd
    unknownshed_test.go, internal/directory runtime_unknown_shed_test.go).
    """
    w = world
    _create(w, w.admin, "lab")
    k = w.key("olga")
    w.set_user("olga", [k])
    w.publish()
    olga = w.identity("olga", k)
    log = w.server.log()
    first_logins, shed_names = log.count(UNKNOWN_KEY_PULL), log.count(SHED_NAME_PULL)
    allowed = f"SSH auth allowed: user=lab fingerprint={k.fingerprint} "

    # Refused at once, as a name that is no shed is: the login never waits.
    r, took = _ssh_timed(w, olga, "lab")
    assert r.returncode == 255 and "Permission denied" in r.stderr, (r.returncode, r.stderr)
    assert took < UNKNOWN_KEY_WAIT / 2, f"the login waited {took:.1f} s"

    # The shed exists: the server pulls in the background, on the shed-name
    # slot, never the first-login one.
    deadline = time.monotonic() + 30
    while w.server.log().count(SHED_NAME_PULL) == shed_names and time.monotonic() < deadline:
        time.sleep(0.5)
    log = w.server.log()
    assert log.count(SHED_NAME_PULL) == shed_names + 1, "no background pull for an existing shed's name"
    assert log.count(UNKNOWN_KEY_PULL) == first_logins, "the shed name took the first-login slot"

    # A retry seconds later is let in (authenticated; the shed is the local
    # admin's, so the session itself may be refused, but not the login).
    deadline = time.monotonic() + 30
    while True:
        r, _ = _ssh_timed(w, olga, "lab")
        if "Permission denied" not in r.stderr:
            break
        assert time.monotonic() < deadline, f"the retry is still refused: {r.stderr!r}"
        time.sleep(1)
    assert w.server.log().count(allowed) >= 1, f"no {allowed!r} line in the server's log"


# ----------------------------------------------------------------------------
# Certificates that name no user
# ----------------------------------------------------------------------------


def test_an_unbound_certificate_is_refused(world):
    w = world
    k = w.key("jay")
    w.set_user("jay", [k])
    w.publish()
    w.refresh_ok()
    jay = w.identity("jay", k)
    jay.add_server()
    a = w.api(jay, "/api/sheds")
    assert a.status == 200, a

    # Made with the CA in the server's state directory, shaped as it issues
    # one: naming jay it is accepted (so the certificate itself is sound) ...
    bound = w.forged_cert(k, "jay")
    assert bound.user() == "jay"
    a = bound.get("/api/sheds")
    assert a.status == 200, a
    # ... and naming nobody, as before certificates named their user, refused
    # by the server ...
    unbound = w.forged_cert(k, None)
    assert unbound.user() is None, unbound.subject()
    w.assert_api_refused(unbound, "an unbound certificate")
    # ... which still answers the bound one: the refusal was of that
    # certificate, not a server gone down.
    a = bound.get("/api/sheds")
    assert a.status == 200, a


# ----------------------------------------------------------------------------
# Sessions inside a shed
# ----------------------------------------------------------------------------


def test_a_session_inside_a_shed_is_closed_when_its_person_is_removed(world):
    w = world
    k = w.key("gus")
    w.set_user("gus", [k])
    w.publish()
    w.refresh_ok()
    gus = w.identity("gus", k)
    gus.add_server()
    _create(w, gus, "gus-shed")

    session = w.session(gus, "gus-shed")
    marker = _marker()
    assert session.echo(marker), session.stderr()

    w.remove_user("gus")
    w.publish()
    w.refresh_ok()
    assert session.wait_closed(CLOSE_WITHIN), "the removed person's session inside the shed is still open"
    marker = _marker()
    r = w.ssh_run(gus, "gus-shed", f"echo {marker}")
    assert r.returncode == 255 and "Permission denied" in r.stderr and marker not in r.stdout, (r.returncode, r.stderr)
    # The shed stays theirs, visible to an admin.
    sheds = {s["name"]: s for s in w.admin.list_sheds()}
    assert sheds.get("gus-shed", {}).get("user") == "gus", sheds


def test_a_session_inside_a_shed_across_a_key_move(world):
    w = world
    moving, hana_other, ivan_own = w.key("moving"), w.key("hana-other"), w.key("ivan-own")
    w.set_user("hana", [moving, hana_other])
    w.set_user("ivan", [ivan_own])
    w.publish()
    w.refresh_ok()
    hana = w.identity("hana", moving)
    hana.add_server()
    _create(w, hana, "hana-shed")
    # Hana's CLI certificate (bound to hana, for the moving key) is answered.
    hana_cert = w.cli_cert(hana)
    a = hana_cert.get("/api/sheds")
    assert a.status == 200, a

    held = w.held(hana, "hana-shed")
    session = held.open_cat(1)
    marker = _marker()
    assert session.echo(marker), session.stderr()
    r = held.run(f"echo {marker}")
    assert r.returncode == 0 and marker in r.stdout, r.stderr

    w.set_user("hana", [hana_other])
    w.set_user("ivan", [ivan_own, moving])
    w.publish()
    w.refresh_ok()

    # The open session survives ...
    marker = _marker()
    assert session.echo(marker), f"the open session did not survive the key move: {session.stderr()}"
    # ... a new one on the same connection is refused, because the key is no
    # longer the user the connection authenticated as (not merely because its
    # new owner may not enter: that refusal reads differently, below) ...
    marker = _marker()
    r = held.run(f"echo {marker}")
    assert r.returncode != 0 and marker not in r.stdout, (r.returncode, r.stdout, r.stderr)
    assert KEY_MOVED in r.stderr, r.stderr
    assert held.alive(), "the refused session took the connection down"
    # ... her CLI certificate is refused ...
    w.assert_api_refused(hana_cert, "hana's CLI certificate after her key moved")
    # ... and a new connection with the key is ivan's, who may not enter it.
    marker = _marker()
    r = w.ssh_run(w.identity("ivan", moving), "hana-shed", f"echo {marker}")
    assert r.returncode != 0 and marker not in r.stdout, (r.returncode, r.stdout, r.stderr)
    assert "shed 'hana-shed' not found" in r.stderr, r.stderr
