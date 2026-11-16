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

import re
import time

import pytest

from normalize import (
    assert_machine_identity,
    canonical,
    mask_audit_entry,
    mask_bus_response,
    mask_live_status,
    mask_status_text,
)
from synthetic_bus import NOT_SOURCE_ACTIVE, SyntheticBus

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


# The respond deadline (`bus.rs` RESPOND_RETRY_BUDGET): from the first send, nothing of a
# respond — no retry, no wait — outlives it.
_RESPOND_DEADLINE_S = 5.0


def _shell_word(s: str) -> str:
    """The Rust renderer's `shell_word`: bare only when made of ASCII letters, digits,
    `.`, `_` and `-`, else single-quoted."""
    if s and re.fullmatch(r"[A-Za-z0-9._-]+", s):
        return s
    return "'" + s.replace("'", "'\\''") + "'"


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
                # hint names the active machine and this machine's exact command.
                r = d.status()
                assert r.returncode == 0, r.stderr
                text = r.stdout.replace(bus.url, "<bus>")
                command = f"    to make this machine the source: shed source use -- {_shell_word(machine['name'])}"
                assert command in text.splitlines(), text
                text = mask_status_text(text.replace(command, "    to make this machine the source: shed source use -- <machine_name>"), d.socket_dir, d.config_path)
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
