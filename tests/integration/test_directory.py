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
    certificate, are refused;
  - an entry is held to its user: with keys of two users in the agent and
    the list out of force, a command through the everyday entry is refused
    and nothing the CLI keeps changes, and the entry works again as its user
    once the list is back; a refetch makes the entry another user only with
    --allow-user-change; an entry whose `user` line was removed by hand is
    refused before anything reaches the server, and a refetch restores it;
    one host name, two entries with their own SSH keys, two users;
  - every token request the server makes for its pulls names the
    directory scope and nothing else;
  - on the remote backend, the shed record carries `mounts`, `isolation` and
    `arch`, and /api/info its exec limits, `guest_arch` and the tokens.

An API refusal is always the server's own (an HTTP 401 or 403 with its
error code, or its bad_certificate alert at the TLS handshake), followed by a
request the server answers; a request that got no answer fails the case
(`fixtures.directory.api_request`).

Run it with `make test-integration-directory` (Linux only: it builds bin/
and the reference server, then runs this module); under any other target it
skips. A subset: `make test-integration-directory PYTEST_ARGS="-k rates"`.
"""

from __future__ import annotations

import json
import time
import uuid
from dataclasses import replace
from datetime import datetime

import pytest
import yaml

from fixtures.directory import (
    BOOTSTRAP_NOT_AUTHORIZED,
    ENTRY,
    ENTRY_USER_CHANGED_LINE,
    ENTRY_USER_LINE,
    EXEC_LIMIT_MEMBERS,
    FEATURE_EXEC_LIMITS,
    FEATURE_UNIFORM_SHED_RECORD,
    HOST,
    KEY_MOVED,
    LOCAL_ADMIN,
    NO_REQUEST_SENT,
    STORED_UNRECORDED_REFUSAL,
    TOKEN_REQUEST_DIRECTORY_SCOPE,
    TRUST_SECONDS,
    USER_CHANGE_REFUSAL,
    USERS_STATUS_FORBIDDEN,
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
    drop_entry_field,
    has_option,
    parse_stamp,
    shed_tree,
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

    # Every token request the server made for its pulls named the directory
    # scope and nothing else (the reference server serves this world's
    # server only, so each line it logged is this server's).
    asked = w.ref.token_requests()
    assert asked, "the reference server logged no token request"
    assert all(line == TOKEN_REQUEST_DIRECTORY_SCOPE for line in asked), asked


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


# ----------------------------------------------------------------------------
# A CLI entry is held to its user
# ----------------------------------------------------------------------------


def _listed(who, *entries: str) -> tuple[dict, str]:
    """`shed --json server list` as `who`: the rows of `entries` by name, and
    what it wrote to stderr."""
    r = who.shed("--json", "server", "list", timeout=180)
    assert r.returncode == 0, f"shed server list: exit {r.returncode}: {r.stderr!r}"
    rows = {s["name"]: s for s in json.loads(r.stdout)}
    assert set(entries) <= set(rows), (entries, sorted(rows))
    return {e: rows[e] for e in entries}, r.stderr


def _pat_with_two_users_keys(w: World):
    """`pat` in the list, with an agent offering pat's key first and then the
    local admin's, enrolled as pat through the entry `ENTRY`."""
    k = w.key("pat")
    w.set_user("pat", [k])
    w.publish()
    w.refresh_ok()
    pat = w.agent_identity("pat", w.agent([k, w.admin_key]))
    pat.add_server()
    assert pat.entry_config().get("user") == "pat", pat.entry_config()
    assert w.cli_cert(pat).user() == "pat"
    r = pat.cli("list")
    assert r.returncode == 0, r.stderr
    return pat


@pytest.mark.slow
def test_an_entry_offering_two_users_keys_is_refused_while_the_list_is_out_of_force(world):
    w = world
    pat = _pat_with_two_users_keys(w)
    kept = shed_tree(pat.home)

    # The list goes out of force: the service is out and the trust time ends
    # (only the local admin can log in). pat's certificate is refused, so the
    # CLI enrols again: ssh offers pat's key, which the server no longer
    # knows, then the local admin's, which it lets in.
    w.ref.outage(True)
    w.wait_state("expired", TRUST_SECONDS + TRUST_TICK + 60)
    w.assert_api_refused(w.cli_cert(pat), "pat's certificate with the list out of force")
    refusal = USER_CHANGE_REFUSAL.format(cert=LOCAL_ADMIN, entry=ENTRY, held="pat")
    r = pat.cli("list")
    assert r.returncode == 1, f"the everyday entry was not refused: exit {r.returncode}: {r.stdout!r} {r.stderr!r}"
    assert refusal in r.stderr, r.stderr
    assert shed_tree(pat.home) == kept, "a refused enrolment changed what the CLI keeps"
    # The listing shows it refused, still pat's, and says why once.
    rows, err = _listed(pat, ENTRY)
    assert (rows[ENTRY]["status"], rows[ENTRY].get("user")) == ("refused", "pat"), rows
    assert err.count(refusal) == 1, err
    assert shed_tree(pat.home) == kept, "listing the refused entry changed what the CLI keeps"
    w.assert_local_admin_works("the list out of force")

    # The list is back in force: the same entry works again as pat, with the
    # certificate it holds, and no step on the entry.
    w.ref.outage(False)
    w.refresh_ok()
    r = pat.cli("list")
    assert r.returncode == 0, f"the everyday entry did not work again: {r.stderr!r}"
    assert shed_tree(pat.home) == kept, "the entry was enrolled again"
    assert pat.entry_config().get("user") == "pat"
    assert w.cli_cert(pat).user() == "pat"


def test_a_refetch_makes_the_entry_another_user_only_when_asked(world):
    w = world
    pat = _pat_with_two_users_keys(w)
    kept = shed_tree(pat.home)
    # pat leaves the list: ssh's first key is refused, the local admin's is
    # let in, and the server issues a certificate for the local admin.
    w.remove_user("pat")
    w.publish()
    w.refresh_ok()

    r = pat.shed("server", "update", ENTRY, "--refetch", timeout=180)
    assert r.returncode == 1, f"the refetch was not refused: exit {r.returncode}: {r.stdout!r} {r.stderr!r}"
    assert USER_CHANGE_REFUSAL.format(cert=LOCAL_ADMIN, entry=ENTRY, held="pat") in r.stderr, r.stderr
    assert "--allow-user-change" in r.stderr, r.stderr
    assert shed_tree(pat.home) == kept, "a refused refetch changed what the CLI keeps"

    r = pat.shed("server", "update", ENTRY, "--refetch", "--allow-user-change", timeout=180)
    assert r.returncode == 0, f"the refetch with --allow-user-change failed: {r.stdout!r} {r.stderr!r}"
    assert ENTRY_USER_CHANGED_LINE.format(was="pat", now=LOCAL_ADMIN) in r.stdout, r.stdout
    assert pat.entry_config().get("user") == LOCAL_ADMIN, pat.entry_config()
    assert w.cli_cert(pat).user() == LOCAL_ADMIN
    rows, _ = _listed(pat, ENTRY)
    assert (rows[ENTRY]["status"], rows[ENTRY].get("user")) == ("online", LOCAL_ADMIN), rows
    # The entry is the local admin's now: an admin's request is answered.
    r = pat.cli("users", "status", timeout=60)
    assert r.returncode == 0, r.stderr


def test_an_entry_whose_user_line_was_removed_is_refused_before_any_request(world):
    w = world
    k = w.key("pat")
    w.set_user("pat", [k])
    w.publish()
    w.refresh_ok()
    pat = w.identity("pat", k)
    # The entry reaches the server only through two relays of the test, one
    # in front of its SSH port and one in front of its HTTPS port, so every
    # connection the CLI makes for it is an accept the test counts.
    ssh_relay, https_relay = w.relay(w.ssh_port), w.relay(w.https_port)
    r = pat.shed("server", "add", HOST, "--ssh-port", str(ssh_relay.port), "--https-port", str(https_relay.port),
                 "--name", ENTRY, "--trust-on-first-use", timeout=180)
    assert r.returncode == 0, f"shed server add through the relays: exit {r.returncode}: {r.stdout!r} {r.stderr!r}"
    e = pat.entry_config()
    assert (e["ssh_port"], e["api_url"]) == (ssh_relay.port, f"https://{HOST}:{https_relay.port}"), e
    # The relays see the entry's connections (the controls for the check
    # below): the enrolment's ssh, and a command's request.
    assert ssh_relay.settle() > 0, "the enrolment's ssh did not go through its relay"
    before = https_relay.settle()
    r = pat.cli("list")
    assert r.returncode == 0, r.stderr
    assert https_relay.settle() > before, "the entry's request did not go through its relay"

    cfg = pat.home / ".shed" / "config.yaml"
    cfg.write_text(drop_entry_field(cfg.read_text(encoding="utf-8"), ENTRY, "user"), encoding="utf-8")
    assert "user" not in pat.entry_config(), pat.entry_config()
    kept = shed_tree(pat.home)
    accepts = ssh_relay.settle(), https_relay.settle()

    # Refused before anything is sent: a command, and the listing.
    refusal = STORED_UNRECORDED_REFUSAL.format(entry=ENTRY, cert="pat")
    r = pat.cli("list")
    assert r.returncode == 1, f"the entry was not refused: exit {r.returncode}: {r.stdout!r} {r.stderr!r}"
    assert refusal in r.stderr and NO_REQUEST_SENT in r.stderr, r.stderr
    rows, err = _listed(pat, ENTRY)
    assert rows[ENTRY]["status"] == "refused" and "user" not in rows[ENTRY], rows
    assert err.count(refusal) == 1, err
    assert shed_tree(pat.home) == kept, "a refused entry changed what the CLI keeps"
    assert (ssh_relay.settle(), https_relay.settle()) == accepts, "a connection was made for the refused entry"

    # The fix the refusal names: a refetch records the user again (its ssh
    # goes through the SSH relay), and the entry works.
    r = pat.shed("server", "update", ENTRY, "--refetch", timeout=180)
    assert r.returncode == 0, f"the refetch failed: {r.stdout!r} {r.stderr!r}"
    assert ENTRY_USER_LINE.format(user="pat") in r.stdout, r.stdout
    assert pat.entry_config().get("user") == "pat", pat.entry_config()
    assert ssh_relay.settle() > accepts[0], "the refetch's ssh did not go through its relay"
    r = pat.cli("list")
    assert r.returncode == 0, r.stderr


def test_one_host_name_two_entries_two_users(world):
    """The guide's backup-admin recipe (docs/guides/user-directory.md,
    "Keeping the local admin on a backup key"): one host name, an entry that
    names the backup key (--identity-file) and is the local admin, and an
    everyday entry that is pat, while the agent offers pat's key to any ssh
    that does not name its key. The everyday entry here names no key, so the
    ssh the CLI runs for each entry shows that it narrows the offer, and
    keeps the connection to itself, only for the entry that names one."""
    w = world
    k = w.key("pat")
    w.set_user("pat", [k])
    w.publish()
    w.refresh_ok()
    pat = w.agent_identity("pat", w.agent([k]))
    admin_entry = f"{ENTRY}-admin"
    admin_key = str(w.admin_key.path)
    for entry, identity, user in ((admin_entry, ["--identity-file", admin_key], LOCAL_ADMIN), (ENTRY, [], "pat")):
        pat.forget_ssh_runs()
        r = pat.shed("server", "add", HOST, "--ssh-port", str(w.ssh_port), "--name", entry, *identity,
                     "--trust-on-first-use", timeout=180)
        assert r.returncode == 0, f"shed server add {entry}: exit {r.returncode}: {r.stdout!r} {r.stderr!r}"
        assert ENTRY_USER_LINE.format(user=user) in r.stdout, r.stdout
        # What the CLI asked ssh for: the enrolment, which names the entry's
        # key alone and opens no shared connection when the entry has a key,
        # and names no key when it has none.
        runs = pat.ssh_runs()
        assert runs and all(has_option(a, "-l", "_bootstrap") for a in runs), runs
        for argv in runs:
            narrowed = [has_option(argv, "-i", admin_key), has_option(argv, "-o", "IdentitiesOnly=yes"),
                        has_option(argv, "-o", "ControlMaster=no"), has_option(argv, "-o", "ControlPath=none")]
            if identity:
                assert all(narrowed), (entry, argv)
            else:
                assert "-i" not in argv and not narrowed[1], (entry, argv)

    servers = yaml.safe_load((pat.home / ".shed" / "config.yaml").read_text(encoding="utf-8"))["servers"]
    assert {(servers[e]["host"], servers[e]["ssh_port"]) for e in (admin_entry, ENTRY)} == {(HOST, w.ssh_port)}
    assert (servers[admin_entry]["user"], servers[admin_entry]["identity_file"]) == (LOCAL_ADMIN, admin_key)
    assert (servers[ENTRY]["user"], servers[ENTRY].get("identity_file")) == ("pat", None)
    rows, _ = _listed(pat, admin_entry, ENTRY)
    assert {e: (rows[e]["status"], rows[e].get("user")) for e in rows} == {
        admin_entry: ("online", LOCAL_ADMIN), ENTRY: ("online", "pat")}, rows

    admin = replace(pat, entry=admin_entry)
    assert w.cli_cert(admin).user() == LOCAL_ADMIN
    assert w.cli_cert(pat).user() == "pat"
    # Each entry acts as its user: the admin's request is answered, pat's is
    # refused (pat is no admin), and pat's own list is answered.
    r = admin.cli("users", "status", timeout=60)
    assert r.returncode == 0, r.stderr
    r = pat.cli("users", "status", timeout=60)
    assert r.returncode != 0, f"pat's entry read the users' status: {r.stdout!r}"
    assert USERS_STATUS_FORBIDDEN.format(entry=ENTRY) in r.stderr, r.stderr
    a = w.api(pat, "/api/users/status")
    assert (a.status, a.refusal) == (403, "HTTP 403 FORBIDDEN"), a
    r = pat.cli("list")
    assert r.returncode == 0, r.stderr


# ----------------------------------------------------------------------------
# The shed record and /api/info on the remote backend
# ----------------------------------------------------------------------------


def test_the_shed_record_and_the_server_info_on_the_remote_backend(world):
    w = world
    r = w.admin.cli("create", "iso", "--no-provision", "--network", "isolated", timeout=180)
    assert r.returncode == 0, f"shed create iso --network isolated: {r.stdout[-600:]!r} {r.stderr[-600:]!r}"
    _create(w, w.admin, "plain")

    info = w.api(w.admin, "/api/info")
    assert info.status == 200, info
    limits = info.body.get("exec")
    assert isinstance(limits, dict) and set(limits) == EXEC_LIMIT_MEMBERS, limits
    assert all(isinstance(v, int) and v > 0 for v in limits.values()), limits
    assert {FEATURE_EXEC_LIMITS, FEATURE_UNIFORM_SHED_RECORD} <= set(info.body.get("features") or []), info
    arch = info.body.get("guest_arch")
    assert arch in ("amd64", "arm64"), info

    want = {
        "iso": {"mounts": {"mode": "selected"}, "network_profile": "isolated", "isolation": "topology", "arch": arch},
        "plain": {"mounts": {"mode": "selected"}, "network_profile": "open", "isolation": "none", "arch": arch},
    }
    listed = w.api(w.admin, "/api/sheds")
    assert listed.status == 200, listed
    rows = {s["name"]: s for s in listed.body["sheds"]}
    for name, members in want.items():
        got = w.api(w.admin, f"/api/sheds/{name}")
        assert got.status == 200, got
        for record in (got.body, rows[name]):
            assert {k: record.get(k) for k in members} == members, record
