"""The machine pin, from the broker's side (plan 035 §3.4).

A pinning shed-server answers one machine per user; this is what the daemon does with
each of its answers. The synthetic bus answers the pinned shapes (`synthetic_bus.py`);
every cell runs one single-server daemon against it, subscribing `ssh-agent` and
`docker-credentials`.

* **not-source, then accepted** — `ssh-agent` is refused 403 `SOURCE_NOT_BOUND`
  naming another machine. The namespace reads `not-source` with the active machine
  (and so does its server), keeps retrying, and `status` prints the command that moves
  the source here. Once the bus accepts it, it connects with no restart.
* **duplicate** — 409 `SOURCE_DUPLICATE_STREAM`: `duplicate`, still retrying.
* **the desktop app on the same machine** — the daemon stands by while another process
  holds a lock beside `machine.json` (`shed_broker::presence`; the desktop app no longer
  takes it, plan 037 B1a); the cells take that lock from Python, standing in for the
  app, on the daemon's real lock file, so it is the shipped daemon's own check
  that is exercised. A daemon started while the lock is held never subscribes and reads
  `yielding`; a lock taken while it is connected makes it close every stream within a
  few seconds (a stand-in for the app's broker then holds each type); releasing the
  lock, or killing its holder outright, makes it subscribe at once. A request the daemon
  is still handling when the lock is taken is never answered by it: the bus re-delivers
  it to the stand-in's stream (same machine), the stand-in answers, and the daemon posts
  nothing and audits it `undelivered`, left for the desktop app. An answer the daemon
  holds back on a scripted `503 Retry-After` is never re-sent once the app has started,
  even though the app has quit again before the retry is due; the daemon's new stream is
  delivered the still-pending request again, and that copy it answers. Two daemons on
  copied state directories (no lock between them) settle with one holder and no tight
  loop.
* **another 403**, a released-broker 403 and **an unparseable 403** — plain
  `reconnecting` with the server's body shown, NEVER `not-source`.
* the **bare 409** stays terminal — `test_servers.py::test_servers_409_rejected_masked_equal`
  (its golden is unchanged by the pin).
* **identity on the wire** — every subscribe and respond carries this machine's id and
  name, equal to what `status --json` reports.
* **respond 503 / 429 with Retry-After** — re-sent, then delivered; **403** — not
  re-sent, and the audit entry is recorded as `undelivered`.

A secure (minting) daemon is not needed to prove "no re-mint on not-source": the bus
unit tests pin that against a counting token provider (`bus.rs`,
`not_source_retries_without_reminting_then_connects`); here a minting cell
additionally counts the shim `ssh`'s credentials mints.
"""

from __future__ import annotations

import fcntl
import json
import os
import shutil
import signal
import subprocess
import sys
import time

import pytest

from desktop_client import DesktopClient, wait_for_consumer
from normalize import (
    assert_machine_identity,
    canonical,
    mask_audit_entry,
    mask_bus_response,
    mask_live_status,
    mask_status_text,
)
from synthetic_bus import NOT_SOURCE_ACTIVE, StandInBroker, SyntheticBus

_SSH = "ssh-agent"
_DOCKER = "docker-credentials"

LIST_REQUEST = {
    "id": "list-1",
    "namespace": _SSH,
    "type": "request",
    "final": True,
    "timestamp": "2026-07-10T00:00:00Z",
    "payload": {"operation": "list"},
    "shed": {"name": "web", "backend": "vz", "server": "alpha"},
}

PING_REQUEST = dict(LIST_REQUEST, id="ping-1", payload={"operation": "ping"})


def _ns(obj: dict, ns: str) -> dict | None:
    for sv in obj.get("servers", []):
        for n in sv.get("namespaces", []):
            if n.get("namespace") == ns:
                return n
    return None


def _state(obj: dict, ns: str) -> str | None:
    n = _ns(obj, ns)
    return n.get("state") if n else None


def _servers(d, obj: dict, bus_url: str) -> list:
    """The masked `servers[]`, with the dynamic bus URL as `<bus>`."""
    masked = mask_live_status(obj, d.socket_dir, d.config_path)
    out = []
    for sv in masked["servers"]:
        sv = dict(sv)
        assert sv["url"] == bus_url, sv
        sv["url"] = "<bus>"
        out.append(sv)
    return canonical(out)


# The respond deadline (`bus.rs` RESPOND_RETRY_BUDGET: two 8 s attempts and the server's
# 1 s Retry-After): from the first send, nothing of a respond — no retry, no wait —
# outlives it.
_RESPOND_DEADLINE_S = 17.0


@pytest.mark.differential
def test_not_source_then_accepted(daemon, single_server_config, differential):
    def scenario(impl):
        with SyntheticBus(refuse={_SSH: "not-source"}) as bus:
            with daemon(impl, single_server_config(bus.url)) as d:
                refused = d.poll_status(
                    lambda o: _state(o, _SSH) == "not-source" and _state(o, _DOCKER) == "connected"
                )
                machine = refused["machine"]
                assert_machine_identity(machine["id"], machine["name"])
                # It keeps retrying (the 1 s backoff floor, then 2 s).
                bus.wait_for_subscribe_count(_SSH, 2, timeout=10.0)

                # The text report: the row reads not-source with an idle mark, and the
                # hint names the active machine and this machine's exact command, which
                # names this machine by its full id (two machines can share a name), the
                # name alongside it.
                r = d.status()
                assert r.returncode == 0, r.stderr
                text = r.stdout.replace(bus.url, "<bus>")
                command = (
                    f"    to make this machine ({machine['name']}) the source: "
                    f"shed source use -- {machine['id']}"
                )
                assert command in text.splitlines(), text
                text = mask_status_text(
                    text.replace(
                        command,
                        "    to make this machine (<machine_name>) the source: "
                        "shed source use -- <machine_id>",
                    ),
                    d.socket_dir,
                    d.config_path,
                )
                servers_block = text[text.index("Servers ("):]

                # The server stops refusing (the source moved here): the very next retry
                # connects, with no restart.
                bus.accept(_SSH)
                accepted = d.poll_status(
                    lambda o: _state(o, _SSH) == "connected", timeout=40.0
                )
                return {
                    "refused": _servers(d, refused, bus.url),
                    "refused_text": servers_block,
                    "accepted": _servers(d, accepted, bus.url),
                }

    got = differential(scenario)

    ssh = next(n for n in got["refused"][0]["namespaces"] if n["namespace"] == _SSH)
    assert ssh["state"] == "not-source"
    assert ssh["active_source"] == NOT_SOURCE_ACTIVE
    assert "the active source is desk" in ssh["last_error"]
    # The server row reads as not the source; the docker namespace is unaffected.
    assert got["refused"][0]["active_source"] == NOT_SOURCE_ACTIVE
    # Once accepted, nothing about the pin is left behind.
    after = got["accepted"][0]
    assert "active_source" not in after
    assert all(n["state"] == "connected" and "active_source" not in n for n in after["namespaces"])


@pytest.mark.differential
def test_duplicate_stream_keeps_retrying(daemon, single_server_config, differential):
    def scenario(impl):
        with SyntheticBus(refuse={_SSH: "duplicate"}) as bus:
            with daemon(impl, single_server_config(bus.url)) as d:
                obj = d.poll_status(lambda o: _state(o, _SSH) == "duplicate")
                # Not terminal: it comes back (a bare 409 would stop at one attempt).
                bus.wait_for_subscribe_count(_SSH, 2, timeout=10.0)
                obj = d.poll_status(
                    lambda o: _state(o, _SSH) == "duplicate" and _state(o, _DOCKER) == "connected"
                )
                return _servers(d, obj, bus.url)

    servers = differential(scenario)
    ssh = next(n for n in servers[0]["namespaces"] if n["namespace"] == _SSH)
    assert ssh["state"] == "duplicate"
    assert "active_source" not in ssh
    assert "active_source" not in servers[0]


@pytest.mark.differential
@pytest.mark.parametrize("kind", ["forbidden", "machine-id-required", "unparseable-403"])
def test_other_403_is_reconnecting_not_source(daemon, single_server_config, differential, kind):
    """Negative controls: only 403 + `SOURCE_NOT_BOUND` in a parseable body is
    not-source. Another code, the released-broker code and a body that does not parse
    (here a truncated JSON that names `SOURCE_NOT_BOUND`) are plain `reconnecting`, the
    server's body shown, and retried."""

    def scenario(impl):
        with SyntheticBus(refuse={_SSH: kind}) as bus:
            with daemon(impl, single_server_config(bus.url)) as d:
                bus.wait_for_subscribe_count(_SSH, 2, timeout=10.0)
                obj = d.poll_status(
                    lambda o: _state(o, _SSH) == "reconnecting"
                    and "403" in (_ns(o, _SSH) or {}).get("last_error", "")
                    and _state(o, _DOCKER) == "connected"
                )
                return _servers(d, obj, bus.url)

    servers = differential(scenario)
    ssh = next(n for n in servers[0]["namespaces"] if n["namespace"] == _SSH)
    assert ssh["state"] == "reconnecting"
    assert ssh["last_error"].startswith("unexpected status 403: ")
    assert "active_source" not in ssh
    assert "active_source" not in servers[0]


def test_identity_headers_match_status(daemon, single_server_config):
    """Every subscribe and every respond carries this machine's id and (decoded) name —
    the ones `status --json` reports. Not a golden: both are per-run values."""
    with SyntheticBus() as bus:
        with daemon("rust", single_server_config(bus.url)) as d:
            bus.wait_for_subscribe(_SSH, timeout=10.0)
            bus.wait_for_subscribe(_DOCKER, timeout=10.0)
            bus.push_request(_SSH, PING_REQUEST)
            bus.await_response(_SSH, timeout=10.0)
            machine = d.poll_status(lambda o: "machine" in o)["machine"]
            want = (machine["id"], machine["name"])
            for ns in (_SSH, _DOCKER):
                ids = bus.subscribe_identities(ns)
                assert ids and all(i == want for i in ids), (ns, ids, want)
            assert bus.respond_identities(_SSH) == [want]


@pytest.mark.differential
@pytest.mark.parametrize("status", [503, 429])
def test_respond_retried_on_retry_after(daemon, single_server_config, differential, status):
    """A respond answered 503 or 429 WITH `Retry-After` (nothing written) is re-sent and
    then delivered: two attempts, one delivered pong."""

    def scenario(impl):
        with SyntheticBus() as bus:
            bus.respond_script(_SSH, [(status, "0")])
            with daemon(impl, single_server_config(bus.url)) as d:
                bus.wait_for_subscribe(_SSH, timeout=10.0)
                bus.push_request(_SSH, PING_REQUEST)
                response = bus.await_response(_SSH, timeout=10.0)
                attempts = bus.wait_for_respond_attempts(_SSH, 2)
                return {"attempts": attempts, "response": canonical(mask_bus_response(response))}

    got = differential(scenario)
    assert got["attempts"] == 2
    assert got["response"]["in_reply_to"] == "ping-1"
    assert got["response"]["payload"] == {"status": "ok"}


@pytest.mark.differential
@pytest.mark.parametrize("status", [403, 413])
def test_respond_refusal_not_retried_and_audited_undelivered(
    daemon, single_server_config, differential, status
):
    """403 and 413 are final: one attempt, never re-sent — even when the refusal carries a
    `Retry-After` (the server's never do; it is attached here so only the status can stop
    the retry). The `list` audit says the answer was refused."""

    def scenario(impl):
        with SyntheticBus() as bus:
            bus.respond_script(_SSH, [(status, "0")])
            with daemon(impl, single_server_config(bus.url)) as d:
                bus.wait_for_subscribe(_SSH, timeout=10.0)
                bus.push_request(_SSH, LIST_REQUEST)
                bus.wait_for_respond_attempts(_SSH, 1)
                first_seen = time.monotonic()
                # The audit is written only once the respond has returned, so no retry
                # of it can follow; and the count is read only after the respond's
                # deadline (from the first send, which came before `first_seen`) has
                # passed, so a retry could not merely be late.
                entry = d.read_audit_jsonl(expect=1)[0]
                time.sleep(max(0.0, first_seen + _RESPOND_DEADLINE_S + 0.5 - time.monotonic()))
                return {
                    "attempts": len(bus.respond_identities(_SSH)),
                    "audit": canonical(mask_audit_entry(entry)),
                }

    got = differential(scenario)
    assert got["attempts"] == 1
    assert got["audit"]["result"] == "undelivered"
    assert got["audit"]["reason"].startswith(
        f"result ok, but the server refused the response (status {status} "
    )


def test_not_source_never_remints(daemon, discovery_poll_config):
    """On a SECURE (minting) server, a not-source refusal is retried with the credential
    already held: the shim `ssh` sees one credentials-scope mint, however many retries
    follow. (A 401 re-mints; `test_secure_bus.py` pins that side.) Not a golden: the
    assertion is a count."""
    from test_secure_bus import CERT_PIN, TLS_CERT, TLS_KEY, _make_secure_daemon, _secure_source

    with SyntheticBus(tls_cert=TLS_CERT, tls_key=TLS_KEY, refuse={_SSH: "not-source"}) as bus:
        with _make_secure_daemon(daemon, "rust", discovery_poll_config) as d:
            d.source_path.write_text(_secure_source(bus.url, CERT_PIN))
            # Three attempts: the first and two retries on the 1 s / 2 s backoff.
            bus.wait_for_subscribe_count(_SSH, 3, timeout=20.0)
            d.poll_status(lambda o: _state(o, _SSH) == "not-source")
            argv = d.read_ssh_argv()
    # One argv element per line; two concurrent mints (this and the egress task's control
    # mint) may interleave their lines, but each mint writes its scope word once.
    mints = [a for a in argv if a == "credentials"]
    assert len(mints) == 1, f"one credentials mint for every not-source retry, got argv={argv!r}"


# ---- the daemon and the desktop app's embedded broker on one machine ---------------

# A fixed machine identity for these cells: written into the daemon's state directory
# before it starts, so the stand-in for the app's broker presents the same id.
_MID = "00112233445566778899aabbccddeeff"
# The lock the daemon watches (`shed_broker::presence`), beside `machine.json`. The
# desktop app no longer takes it (plan 037 B1a); these cells take it in its place.
_LOCK_NAME = "embedded-broker.lock"
# The daemon's presence check interval (`bus.rs` PRESENCE_CHECK_INTERVAL), and the slack
# a cell allows on top of it for a loaded machine.
_CHECK_S = 2.0
_SLACK_S = 4.0
# What a `yielding` namespace says (`bus.rs` YIELDING_REASON).
_YIELDING_REASON = (
    "the desktop app on this machine is answering (its embedded broker is running); this "
    "daemon takes over within a few seconds of the app quitting"
)


def _write_identity(state_dir, name: str) -> None:
    path = state_dir / "host-agent" / "machine.json"
    path.parent.mkdir(parents=True, mode=0o700)
    path.write_text(
        json.dumps({"schema": 1, "id": _MID, "name": name, "created_at": "2026-11-16T00:00:00Z"})
    )
    path.chmod(0o600)


def _lock_path(state_dir):
    return state_dir / "host-agent" / _LOCK_NAME


def _take_lock(state_dir):
    """Hold the daemon's lock from this process, standing in for the app (an exclusive
    `flock`, not waiting). Close the returned file to release it."""
    f = open(_lock_path(state_dir), "a+")  # noqa: SIM115 — held across the cell
    os.chmod(f.name, 0o600)
    fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
    return f


def _lock_in_child(state_dir) -> subprocess.Popen:
    """Hold the daemon's lock from a child process, which says `locked` once it holds it and
    then waits to be killed (a minute at most)."""
    code = (
        "import fcntl, sys, time\n"
        "f = open(sys.argv[1], 'a+')\n"
        "fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)\n"
        "print('locked', flush=True)\n"
        "time.sleep(60)\n"
    )
    p = subprocess.Popen(
        [sys.executable, "-I", "-c", code, str(_lock_path(state_dir))],
        stdout=subprocess.PIPE,
        text=True,
    )
    assert p.stdout.readline().strip() == "locked"
    return p


def _yielding(o: dict, ns: str) -> bool:
    n = _ns(o, ns) or {}
    return n.get("state") == "yielding" and n.get("last_error") == _YIELDING_REASON


def test_daemon_started_while_the_app_runs_never_subscribes(
    daemon, single_server_config, tmp_path
):
    """The app's broker is running (its lock is held) when the daemon starts: the daemon
    never subscribes, reads `yielding` with the reason (and `-` in `status`), and the
    moment the lock is released it subscribes, within one check interval."""
    state_dir = tmp_path / "state"
    _write_identity(state_dir, "box")
    types = (_SSH, _DOCKER)
    lock = _take_lock(state_dir)
    try:
        with SyntheticBus() as bus:
            with daemon("rust", single_server_config(bus.url), state_dir=state_dir) as d:
                d.poll_status(lambda o: all(_yielding(o, ns) for ns in types), timeout=15.0)
                # Several checks go by: still nothing subscribed.
                time.sleep(3 * _CHECK_S)
                assert [bus.subscribe_count(ns) for ns in types] == [0, 0]
                r = d.status()
                assert r.returncode == 0, r.stderr
                rows = [l.split() for l in r.stdout.splitlines() if "yielding:" in l]
                assert sorted((row[0], row[1]) for row in rows) == [
                    ("-", _DOCKER),
                    ("-", _SSH),
                ], r.stdout

                lock.close()  # the app quits
                released = time.monotonic()
                for ns in types:
                    bus.wait_for_subscribe_count(ns, 1, timeout=_CHECK_S + _SLACK_S)
                took = time.monotonic() - released
                assert took <= _CHECK_S + _SLACK_S, took
                d.poll_status(lambda o: all(_state(o, ns) == "connected" for ns in types))
    finally:
        lock.close()


def test_daemon_lets_go_when_the_app_starts_and_takes_over_when_it_is_killed(
    daemon, single_server_config, tmp_path
):
    """The daemon holds every type; then the app starts — a child process takes the
    lock, and a stand-in for its broker (same machine id) subscribes, refused at first as
    a duplicate. Within a check interval the daemon closes every stream, the stand-in
    holds each type, and the daemon asks for nothing while the app runs. The app is then
    killed outright (no unlock, its streams cut): the kernel drops the lock and the
    daemon subscribes again within a check interval."""
    state_dir = tmp_path / "state"
    _write_identity(state_dir, "box")
    types = (_SSH, _DOCKER)
    with SyntheticBus(single_holder=True) as bus:
        with daemon("rust", single_server_config(bus.url), state_dir=state_dir) as d:
            bus.wait_for(lambda b: b.holders() == {_SSH: "box", _DOCKER: "box"},
                         "the daemon holds both types", timeout=15.0)

            holder = _lock_in_child(state_dir)
            app = None
            try:
                started = time.monotonic()
                app = StandInBroker(bus.url, types, _MID, "app")
                bus.wait_for(lambda b: b.holders() == {_SSH: "app", _DOCKER: "app"},
                             "the stand-in holds both types",
                             timeout=_CHECK_S + 1.0 + _SLACK_S)
                d.poll_status(lambda o: all(_yielding(o, ns) for ns in types), timeout=10.0)
                # Let go within an interval: the daemon's streams were the first to end.
                assert time.monotonic() - started <= _CHECK_S + 1.0 + _SLACK_S
                quiet_from = time.monotonic()
                time.sleep(3 * _CHECK_S)
                for ns in types:
                    assert [who for who, _ok in bus.attempts(ns, since=quiet_from)
                            if who == "box"] == [], ns

                holder.send_signal(signal.SIGKILL)
                holder.wait(timeout=5)
                app.stop()
                killed = time.monotonic()
                bus.wait_for(lambda b: b.holders() == {_SSH: "box", _DOCKER: "box"},
                             "the daemon holds both types again",
                             timeout=_CHECK_S + _SLACK_S)
                assert time.monotonic() - killed <= _CHECK_S + _SLACK_S
                d.poll_status(lambda o: all(_state(o, ns) == "connected" for ns in types))
            finally:
                if app is not None:
                    app.stop()
                if holder.poll() is None:
                    holder.kill()
                    holder.wait(timeout=5)
                holder.stdout.close()


def test_two_daemons_on_copied_state_directories_settle_without_a_tight_loop(
    daemon, single_server_config, tmp_path
):
    """Two daemons on copies of one state directory: one machine id, but two locks and no
    app, so neither stands by. One holds each type; the other is refused as a duplicate
    and retries on its ordinary backoff (1, 2, 4 s ...), never in a tight loop; the holder
    keeps both types."""
    first_dir, second_dir = tmp_path / "state-1", tmp_path / "state-2"
    _write_identity(first_dir, "box")
    shutil.copytree(first_dir, second_dir)
    types = (_SSH, _DOCKER)
    with SyntheticBus(single_holder=True) as bus:
        with daemon("rust", single_server_config(bus.url), state_dir=first_dir) as first:
            first.poll_status(lambda o: all(_state(o, ns) == "connected" for ns in types))
            with daemon("rust", single_server_config(bus.url), state_dir=second_dir) as second:
                second.poll_status(
                    lambda o: all(_state(o, ns) == "duplicate" for ns in types), timeout=15.0
                )
                start = time.monotonic()
                time.sleep(8.0)
                for ns in types:
                    refused = [at for at, _who, ok in bus.attempt_times(ns, since=start) if not ok]
                    assert 1 <= len(refused) <= 5, (ns, refused)
                    gaps = [b - a for a, b in zip(refused, refused[1:])]
                    assert all(g >= 0.9 for g in gaps), (ns, gaps)
                    accepted = [at for at, _who, ok in bus.attempt_times(ns, since=start) if ok]
                    assert accepted == [], (ns, "the holder kept it", accepted)
                assert all(
                    _state(first.poll_status(lambda o: True), ns) == "connected" for ns in types
                )


def test_a_request_in_flight_when_the_app_starts_is_answered_by_the_app_only(
    daemon, sign_config, tmp_path
):
    """The daemon holds `ssh-agent` and is handling a sign request (its approval is held
    back at the desktop consumer) when the app starts: a child takes the lock and a
    stand-in for the app's broker (same machine id) subscribes. The daemon lets go at its
    next look; the bus re-delivers the still-pending request to the stand-in's stream, and
    the stand-in answers it. Then the person approves at the daemon's prompt: the daemon
    signs, but posts NOTHING (no respond of this machine name ever reaches the bus) and
    audits the request `undelivered`, left for the desktop app."""
    from test_bus_sign_gated import SIGN_REQUEST

    state_dir = tmp_path / "state"
    _write_identity(state_dir, "box")
    stand_in_payload = {"format": "ssh-ed25519", "blob": "c3RhbmQtaW4=", "rest": ""}
    with SyntheticBus(single_holder=True) as bus:
        with daemon(
            "rust", sign_config(bus.url), install_ssh_key=True, state_dir=state_dir
        ) as d:
            with DesktopClient(str(d.desktop_sock)) as app:
                app.send_hello()
                wait_for_consumer(d, connected=True, timeout=10.0)
                bus.wait_for(lambda b: b.holders().get(_SSH) == "box",
                             "the daemon holds ssh-agent", timeout=15.0)
                bus.push_request(_SSH, SIGN_REQUEST)
                prompt = app.await_frame("approval_request", timeout=10.0)
                assert bus.pending() == [(_SSH, "sign-1")]

                holder = _lock_in_child(state_dir)
                stand_in = None
                try:
                    stand_in = StandInBroker(
                        bus.url, (_SSH,), _MID, "app",
                        answer=lambda env: stand_in_payload,
                    )
                    d.poll_status(lambda o: _yielding(o, _SSH),
                                  timeout=_CHECK_S + 1.0 + _SLACK_S)
                    # The re-delivered request reaches the stand-in, which answers it.
                    bus.await_response(_SSH, timeout=_CHECK_S + 1.0 + _SLACK_S)
                    assert [e.get("id") for e in stand_in.received()] == ["sign-1"]
                    assert bus.pending() == []

                    # The person approves at the daemon's prompt, after it let go.
                    app.send_approval_response(prompt["id"], "approve")
                    entry = d.read_audit_jsonl(expect=1, timeout=15.0)[0]
                    assert entry["op"] == "sign"
                    assert entry["result"] == "undelivered", entry
                    assert entry["reason"].startswith(
                        "result ok, but not sent: left for the desktop app"
                    ), entry
                    # Every respond the bus saw is the stand-in's; one answer recorded.
                    assert bus.respond_identities(_SSH) == [(_MID, "app")]
                    responses = [bus.await_response_at(_SSH, 0)]
                    assert responses[0]["in_reply_to"] == "sign-1"
                    assert responses[0]["payload"] == stand_in_payload
                    time.sleep(1.0)  # nothing from the daemon arrives late either
                    assert bus.respond_identities(_SSH) == [(_MID, "app")]
                finally:
                    if stand_in is not None:
                        stand_in.stop()
                    if holder.poll() is None:
                        holder.kill()
                        holder.wait(timeout=5)
                    holder.stdout.close()


def test_an_answer_held_back_by_a_503_is_never_resent_after_the_app_came_and_went(
    daemon, sign_config, tmp_path
):
    """The daemon answers a sign request and the bus refuses the POST `503` with
    `Retry-After: 8` (the server's own refusal: nothing delivered). During that wait the
    app starts (a child takes the lock; a stand-in for its broker, which never answers,
    subscribes as the same machine) and quits again (stand-in stopped, lock released), and
    the daemon takes over again, all before the retry is due. The old answer is never
    re-sent: the only respond the bus has seen from this machine by then is the refused
    one, and the request is audited `undelivered`, left for the desktop app. The daemon's
    new stream is delivered the still-pending request again; approving that copy answers
    it."""
    from test_bus_sign_gated import SIGN_REQUEST

    retry_after_s = 8.0  # the most a respond allows: 8 s wait + an 8 s attempt in 17 s
    state_dir = tmp_path / "state"
    _write_identity(state_dir, "box")
    with SyntheticBus(single_holder=True) as bus:
        with daemon(
            "rust", sign_config(bus.url), install_ssh_key=True, state_dir=state_dir
        ) as d:
            with DesktopClient(str(d.desktop_sock)) as app:
                app.send_hello()
                wait_for_consumer(d, connected=True, timeout=10.0)
                bus.wait_for(lambda b: b.holders().get(_SSH) == "box",
                             "the daemon holds ssh-agent", timeout=15.0)
                bus.respond_script(_SSH, [(503, str(int(retry_after_s)))])
                bus.push_request(_SSH, SIGN_REQUEST)
                prompt = app.await_frame("approval_request", timeout=10.0)
                app.send_approval_response(prompt["id"], "approve")
                bus.wait_for_respond_attempts(_SSH, 1, timeout=10.0)
                retry_due = time.monotonic() + retry_after_s

                holder = _lock_in_child(state_dir)
                stand_in = None
                try:
                    stand_in = StandInBroker(bus.url, (_SSH,), _MID, "app")
                    d.poll_status(lambda o: _yielding(o, _SSH),
                                  timeout=_CHECK_S + 1.0 + _SLACK_S)
                    bus.wait_for(lambda b: b.holders().get(_SSH) == "app",
                                 "the app's stand-in holds ssh-agent", timeout=_SLACK_S)
                    # The app quits without answering.
                    stand_in.stop()
                    holder.kill()
                    holder.wait(timeout=5)
                    d.poll_status(lambda o: _state(o, _SSH) == "connected",
                                  timeout=_CHECK_S + _SLACK_S)
                    assert time.monotonic() < retry_due, (
                        "the daemon took over only after its retry was due: this run "
                        "cannot tell a re-send from none"
                    )
                    # The copy delivered again on the daemon's new stream is handled once
                    # the old handler is done (past the retry time).
                    prompt2 = app.await_frame("approval_request", timeout=20.0)
                    assert time.monotonic() >= retry_due
                    assert bus.respond_identities(_SSH) == [(_MID, "box")], (
                        "the old answer was never re-sent"
                    )
                    entry = d.read_audit_jsonl(expect=1, timeout=5.0)[0]
                    assert entry["op"] == "sign"
                    assert entry["result"] == "undelivered", entry
                    assert entry["reason"].startswith(
                        "result ok, but not sent: left for the desktop app"
                    ), entry

                    app.send_approval_response(prompt2["id"], "approve")
                    response = bus.await_response(_SSH, timeout=10.0)
                    assert response["in_reply_to"] == "sign-1"
                    assert bus.respond_identities(_SSH) == [(_MID, "box"), (_MID, "box")]
                    assert bus.pending() == []
                finally:
                    if stand_in is not None:
                        stand_in.stop()
                    if holder.poll() is None:
                        holder.kill()
                        holder.wait(timeout=5)
                    holder.stdout.close()
