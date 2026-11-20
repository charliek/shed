"""tauri-only embedded-broker E2E (leg 3a.2).

Drives the REAL Tauri app's IN-PROCESS credential broker (the `shed-broker`
Supervisor the C1 differential harness verifies, embedded via the C2 bridge and
wired by C3) against a hermetic mock shed-server that grows the two plugin-bus
endpoints (`GET /api/plugins/listeners/{ns}/messages` SSE + `POST
/api/plugins/listeners/{ns}/respond`). This is the §3.8 behavioral oracle for the
embedded path: structurally it IS the harness-verified broker code; behaviorally
these cells prove the app-level integration (three-way auto-detect, the bus →
AppGate → Coordinator → respond round-trip, config provenance, the 409 split).

Every cell launches its OWN hermetic app instance (a throwaway HOME/XDG_RUNTIME_DIR
→ its own socket + single-instance lock) with its own PRIVATE mock shed-server, so
the cells are fully isolated from each other and from the session instance (whose
broker serves the two session mocks). The embedded broker resolves its server list
from `~/.shed/config.yaml` under the throwaway HOME
(`shed_broker::DEFAULT_DISCOVERY_SOURCE`), or from the server file
`broker_env.write_hermetic_broker` writes, either naming only the private mock;
because that URL is plain `http://`, `ServerTarget::is_secure` is
false, the bus attaches a static (empty) token and NEVER mints over SSH — so no
cell shells real `ssh` (hermetic token handling, §C4.2).
"""

from __future__ import annotations

import base64
import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile
import time
from contextlib import contextmanager
from pathlib import Path

import pytest

import broker_env
import ui
from bus_stimulus import SSH, audit_line, emit_sign, git_login, new_approval, pending_ids, verdict
from client import ShedError, TauriClient, scaled_timeout
from mockserver import MockShedServer

# The external-mode cells below still run against the fake daemon until that
# mode goes.
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "fake-host-agent"))
from fake_host_agent import FakeHostAgent  # noqa: E402

FIXTURES = Path(__file__).resolve().parent / "fixtures"
CONFIG = FIXTURES / "config.yaml"


@contextmanager
def _broker_app(
    *,
    host_agent_socket: str | None = None,
    extensions_yaml: str | None = None,
    discovery_mock: bool = False,
    gen_ssh_key: bool = False,
    local_keys_ssh: bool = False,
    conflict_namespaces: tuple[str, ...] = (),
    refuse: dict[str, str] | None = None,
    respond_script: dict[str, list] | None = None,
    shed_config_path: Path = CONFIG,
    source: dict | None = None,
    block_state_dir: bool = False,
    approval_timeout: str | None = None,
):
    """Launch a self-managed tauri instance with a PRIVATE mock shed-server, yield
    `(client, mock, info)`, and tear both down. `info` carries `ssh_blob`, `audit_path`,
    `runtime_dir`, `proc` (the app's process, for a cell that quits it itself) and
    `broker` (the `broker_env.BrokerInfo`). Every launch goes through
    `broker_env.validate` (in `ui.subproc_env`) before the spawn.

    * `host_agent_socket` — the app's configured Surface-A socket; the three-way probe
      keys off it (`shed_app::probe_sockets_at`). `None` ⇒ neither daemon socket exists
      under the throwaway HOME ⇒ auto-detect resolves to **embedded**.
    * `extensions_yaml` — written to `~/.config/shed/extensions.yaml` (the daemon's
      default path) + pointed at via `SHED_TAURI_EXTENSIONS_CONFIG`; `None` ⇒ absent ⇒
      the bridge synthesizes the fresh-install default.
    * `local_keys_ssh` — the hermetic broker of `broker_env.write_hermetic_broker`
      (local keys, every namespace delegated to the app, its own server file naming
      the private mock as `mock`); mutually exclusive with the other file options.
    * `discovery_mock` — write `~/.shed/config.yaml` pointing the broker at the mock.
    * `gen_ssh_key` — generate `~/.ssh/id_ed25519`; `info["ssh_blob"]` is the wire pubkey.
    * `conflict_namespaces` — pre-register these on the mock so their subscribe 409s.
    * `refuse` — `{ns: kind}`: answer these subscribes with a pinned plan 035 refusal
      (`mockserver._REFUSALS`) from the broker's first attempt.
    * `respond_script` — `{ns: [(status, retry_after), ...]}`: answer these responds
      with pinned refusals first.
    * `shed_config_path` — the app's OWN `~/.shed/config.yaml` (`SHED_TAURI_SHED_CONFIG`),
      i.e. what `hosts.auth` seeds its per-server credential modes from. Distinct from
      `discovery_mock`, which writes the config the embedded BROKER discovers servers
      through.
    * `source` — arm the mock's credential-source binding (plan 035 §3.4) with these
      `MockShedServer.arm_source` kwargs (`{}` binds the default other machine, "desk").
    * `block_state_dir` — make the broker state dir (`SHED_BROKER_STATE_DIR`) a
      regular FILE, so no machine identity can be loaded or created.
    * `approval_timeout` — the broker's `approval_timeout` (with `local_keys_ssh`).
    """
    cfg = ui._SUBPROC["tauri"]
    if not cfg.binary.exists():
        raise RuntimeError(
            f"tauri binary not found at {cfg.binary}; build it first (make tauri-build).")
    mock = MockShedServer()
    mock.start()
    for ns in conflict_namespaces:
        mock.pre_register_conflict(ns)
    for ns, kind in (refuse or {}).items():
        mock.refuse(ns, kind)
    for ns, script in (respond_script or {}).items():
        mock.respond_script(ns, script)
    if source is not None:
        mock.arm_source(**source)

    info: dict = {"ssh_blob": None, "audit_path": None, "runtime_dir": None}
    runtime_dir = Path(tempfile.mkdtemp(prefix="shed-brk-"))
    info["runtime_dir"] = runtime_dir
    try:
        if local_keys_ssh:
            assert extensions_yaml is None and not (discovery_mock or gen_ssh_key)
            broker = broker_env.write_hermetic_broker(
                runtime_dir, {"mock": mock.base_url}, approval_timeout=approval_timeout)
        else:
            if discovery_mock:
                shed_dir = runtime_dir / ".shed"
                shed_dir.mkdir(parents=True, exist_ok=True)
                # The broker's default server file, naming only the private mock
                # (plain http: not secure, so nothing mints over SSH). It must be
                # exactly `render_server_file`'s, or `validate` refuses the launch.
                (shed_dir / "config.yaml").write_text(
                    broker_env.render_server_file({"mock": mock.base_url}))
            ext_path: Path | None = None
            if extensions_yaml is not None:
                ext_dir = runtime_dir / ".config" / "shed"
                ext_dir.mkdir(parents=True, exist_ok=True)
                ext_path = ext_dir / "extensions.yaml"
                ext_path.write_text(extensions_yaml)
            ssh_blob = None
            if gen_ssh_key:
                ssh_dir = runtime_dir / ".ssh"
                ssh_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
                key = ssh_dir / "id_ed25519"
                subprocess.run(
                    ["ssh-keygen", "-t", "ed25519", "-f", str(key), "-N", "", "-q"],
                    check=True,
                )
                # `ssh-ed25519 <base64 wire pubkey> <comment>` — the middle field is
                # the SSH-wire pubkey blob the sign request carries.
                ssh_blob = (ssh_dir / "id_ed25519.pub").read_text().split()[1]
            broker = broker_env.custom_broker(
                runtime_dir, {"mock": mock.base_url}, extensions_path=ext_path,
                ssh_blob=ssh_blob)
        info["broker"] = broker[1]
        info["ssh_blob"] = broker[1].ssh_blob
        info["audit_path"] = broker[1].audit_path and str(broker[1].audit_path)

        if block_state_dir:
            # `broker_env` points SHED_BROKER_STATE_DIR here.
            (runtime_dir / "broker-state").write_text("not a directory\n")

        # `subproc_env` applies the broker's env (no SSH_AUTH_SOCK, HOME and the XDG
        # dirs here, the extensions config set or cleared) and validates it.
        env = ui.subproc_env(
            cfg, runtime_dir=runtime_dir, mock_base_url=mock.base_url,
            config_path=shed_config_path, host_agent_socket=host_agent_socket,
            broker=broker,
        )

        sock = runtime_dir / cfg.sock_rel
        log = runtime_dir / "broker-ui.log"
        log_fh = open(log, "wb")
        proc = subprocess.Popen(
            [str(cfg.binary)], env=env, stdout=log_fh, stderr=subprocess.STDOUT)
        info["proc"] = proc
        try:
            ui.await_hermetic("tauri", sock=sock, mock_base_url=mock.base_url,
                              proc=proc, log=log)
            client = TauriClient(sock)
            try:
                client.wait_until(lambda: client.current_pane() is not None,
                                  timeout=30, what="broker frontend ready")
                yield client, mock, info
            finally:
                client.close()
        finally:
            ui.terminate(proc)
            log_fh.close()
    finally:
        shutil.rmtree(runtime_dir, ignore_errors=True)
        mock.stop()


def _server_ns_states(status: dict) -> dict[str, str]:
    """Flatten `broker.status.servers[].namespaces[]` to `{namespace: state}` (first
    server; single-server mode has exactly one)."""
    servers = status.get("servers") or []
    if not servers:
        return {}
    return {n["namespace"]: n["state"] for n in servers[0].get("namespaces", [])}


def _sign_envelope(rid: str, ssh_blob: str, *, server: str = "mock", shed: str = "web") -> dict:
    """A `sign` request Envelope in the bus wire shape (mirrors the in-crate httpmock
    fixture + `sdk/envelope.go`): `payload.operation == "sign"`, `public_key` = the
    wire pubkey blob, `data` = base64 of a `git` login with that key."""
    return {
        "id": rid,
        "namespace": "ssh-agent",
        "type": "request",
        "final": True,
        "timestamp": "t",
        "payload": {
            "operation": "sign",
            "public_key": ssh_blob,
            "data": base64.b64encode(git_login(ssh_blob)).decode(),
            "flags": 0,
        },
        "shed": {"name": shed, "backend": "vz", "server": server},
    }


# ---------------------------------------------------------------------------
# Cell (a) — three-way auto-detect (§3.3)
# ---------------------------------------------------------------------------

def test_autodetect_embedded_when_no_daemon_sockets():
    """No daemon sockets under the throwaway HOME ⇒ auto resolves to EMBEDDED, with the
    probe evidence surfaced (both sockets dead) and the fresh-install config
    synthesized."""
    with _broker_app(host_agent_socket=None) as (client, _mock, _info):
        assert client.identify()["broker_mode"]["effective"] == "embedded"
        st = client.broker_status()
        assert st["effective_mode"] == "embedded"
        assert st["probe"] == {"desktop_socket_live": False, "status_socket_live": False}
        assert st["config"]["source"] == "synthesized"


def test_autodetect_external_when_desktop_socket_live():
    """A live desktop (Surface-A) socket ⇒ EXTERNAL mode (today's UDS client) — and the
    external approval round-trip still works via the fake host-agent (the session
    instance's contract, proven here for a self-managed one too)."""
    fake = FakeHostAgent()
    fake.start()
    try:
        with _broker_app(host_agent_socket=fake.socket_path) as (client, _mock, _info):
            assert fake.wait_connected(), "app did not connect to the desktop socket"
            assert client.identify()["broker_mode"]["effective"] == "external"
            assert client.broker_status()["effective_mode"] == "external"
            # today's approval flow: emit over the UDS, decide, assert the reply.
            client.set_ssh_approval(policy="always-ask")
            rid = fake.emit_request("ssh-agent", "sign", "ext-shed", "ssh-ed25519")
            client.wait_until(lambda: any(a["id"] == rid for a in client.approvals_list()),
                              timeout=15, what="external approval prompt")
            client.approval_decide(rid, "approve")
            resp = fake.wait_response(rid)
            assert resp and resp["decision"] == "approve"
    finally:
        fake.stop()


def test_autodetect_headless_coexist_when_only_status_socket_live():
    """A HEADLESS daemon (status socket live, desktop socket absent) ⇒ HEADLESS-COEXIST:
    the app starts no bus (no servers watched) and takes no approvals (dark), and
    `broker.status` says exactly that."""
    status_fake = FakeHostAgent(status_only=True)
    status_fake.start()
    try:
        # The desktop socket the app is CONFIGURED with is the (absent) sibling of the
        # bound status socket, so `probe_sockets_at` finds status-only.
        desktop_sibling = os.path.join(
            os.path.dirname(status_fake.socket_path), "host-agent.sock")
        with _broker_app(host_agent_socket=desktop_sibling) as (client, _mock, _info):
            assert client.identify()["broker_mode"]["effective"] == "headless-coexist"
            st = client.broker_status()
            assert st["effective_mode"] == "headless-coexist"
            assert st["probe"] == {"desktop_socket_live": False, "status_socket_live": True}
            # Approvals dark: no bus started ⇒ no servers watched, ssh mode unresolved.
            assert st["servers"] == []
            assert st["resolved_ssh_mode"] is None
            # A real headless status socket never speaks the desktop protocol — the app
            # must never have sent it a frame (hello or otherwise).
            assert status_fake.received_frames() == [], status_fake.received_frames()
    finally:
        status_fake.stop()


# ---------------------------------------------------------------------------
# Cell (b) — embedded approval end-to-end (bus → AppGate → Coordinator → respond)
# ---------------------------------------------------------------------------

@pytest.mark.skipif(
    shutil.which("ssh-keygen") is None,
    reason="needs ssh-keygen to seed a local-keys backend (absent on the Linux render "
           "gate image; the round-trip runs on the macOS e2e-tauri gate + in-crate tests)",
)
def test_embedded_sign_round_trip_through_appgate():
    """A bus `sign` request reaches the Coordinator queue, the app approves (test-mode
    gate), the decision returns through the bridge, and the bus `respond` carries the
    signature keyed by `in_reply_to`; the activity row lands via the IPC list op and the
    broker's durable JSONL carries the daemon-shaped approval outcome."""
    with _broker_app(local_keys_ssh=True) as (client, mock, info):
        # Prompt-then-approve, gate "none" (a plain Approve, not biometric) so the
        # test-mode auto-approve records a deterministic decided_by == "user".
        client.policy_set([{"scope": "default", "action": "prompt", "gate": "none"}])
        # Raises on timeout; the open-mode return (the Authorization header) is None.
        mock.wait_for_subscribe("ssh-agent")

        rid = "sign-embedded-1"
        mock.push_request("ssh-agent", _sign_envelope(rid, info["ssh_blob"]))

        # The AppGate posts an approval-request into the Coordinator queue (a FRESH id,
        # distinct from the bus envelope id) — approve it.
        client.wait_until(lambda: len(client.approvals_list()) >= 1,
                          timeout=20, what="embedded approval prompt")
        appr = client.approvals_list()[0]
        assert appr.get("namespace") == "ssh-agent" and appr.get("op") == "sign", appr
        assert appr.get("shed") == "web", appr
        client.approval_decide(appr["id"], "approve")

        # The bus respond POST carries the SIGNATURE keyed by in_reply_to == envelope id.
        resp = mock.await_response("ssh-agent")
        assert resp["in_reply_to"] == rid, resp
        # `resp.shed = env.shed.clone()` (bus.rs) routes the request's shed back —
        # the fixture's shed name ("web", the `_sign_envelope` default).
        assert resp["shed"]["name"] == "web", resp
        payload = resp.get("payload") or {}
        assert "error" not in payload, f"sign failed (not approved?): {payload}"
        assert payload.get("blob"), f"no signature blob in respond payload: {payload}"
        # An ed25519 signature is exactly 64 raw bytes; the format tag is fixed.
        assert payload["format"] == "ssh-ed25519", payload
        assert len(base64.b64decode(payload["blob"])) == 64, payload

        # The activity row lands via the IPC list op (audit fan-in), approval delegated.
        client.wait_until(
            lambda: any(e["ns"] == "ssh-agent" and e["op"] == "sign" and e["result"] == "ok"
                        for e in client.activity_list()),
            timeout=15, what="embedded sign activity row")
        row = next(e for e in client.activity_list()
                   if e["ns"] == "ssh-agent" and e["op"] == "sign" and e["result"] == "ok")
        assert row["approval"] == "shed-desktop", row

        # The broker's durable JSONL carries the daemon-shaped approval outcome
        # (decided_by/scope/ttl live in the durable log, not the app activity row).
        audit = Path(info["audit_path"])
        client.wait_until(lambda: audit.exists() and _sign_line(audit) is not None,
                          timeout=15, what="durable broker audit line")
        line = _sign_line(audit)
        assert line["approval"] == "shed-desktop", line
        assert line["decided_by"] == "user", line
        assert line["result"] == "ok", line


def _sign_line(audit: Path) -> dict | None:
    """The first durable JSONL audit line for an ok ssh-agent sign, or None."""
    try:
        for raw in audit.read_text().splitlines():
            if not raw.strip():
                continue
            e = json.loads(raw)
            if e.get("ns") == "ssh-agent" and e.get("op") == "sign" and e.get("result") == "ok":
                return e
    except (OSError, ValueError):
        return None
    return None


def test_expiry_fails_closed():
    """A request nobody decides is denied when the broker's `approval_timeout` runs
    out (or the app's prompt expires: both carry the same deadline, and the cell
    asserts only what both produce): the bus answer is a deny, the approval leaves
    the pending list, and a late decide sends nothing — the app either refuses it
    (not-found) or no-ops, and the mock sees no second answer for the request."""
    with _broker_app(local_keys_ssh=True, approval_timeout="2s") as (client, mock, info):
        client.policy_set([{"scope": "default", "action": "prompt", "gate": "none"}])
        before = pending_ids(client)
        eid = emit_sign(mock, "expire-shed", info["broker"])
        aid = new_approval(client, "expire-shed", before)
        assert verdict(mock, SSH, eid) == "deny"  # left undecided → denied on expiry
        # Attributed to one of the two real expiry paths: the app's prompt expiry
        # ("timeout") or the broker's own gate timeout (no attribution, omitted).
        decided_by = audit_line(info["broker"], SSH, "expire-shed").get("decided_by", "")
        assert decided_by in ("timeout", ""), decided_by
        client.wait_until(lambda: aid not in pending_ids(client), timeout=15,
                          what="the expired approval left the pending list")
        try:
            client.approval_decide(aid, "approve")
        except ShedError as e:
            assert e.code in ("not-found", "not_found"), e
        time.sleep(scaled_timeout(1))
        assert len(mock.replies_to(SSH, eid)) == 1, mock.replies_to(SSH, eid)


# ---------------------------------------------------------------------------
# Cell (c) — token/status: subscribed namespace set, identify, set_mode round-trip
# ---------------------------------------------------------------------------

def test_embedded_status_and_set_mode_round_trip():
    """`broker.status` shows the mock subscribed with the expected namespace set
    (connected), `identify` carries the mode, and `broker.set_mode` is a deferred-apply
    round-trip (set embedded ⇒ restart_required; set back to auto ⇒ cleared)."""
    with _broker_app(discovery_mock=True) as (client, mock, _info):
        # The broker subscribes ssh-agent (gated) + docker-credentials (subscribes-with-
        # deny); aws is unconfigured ⇒ never subscribed (broker_bridge.rs spawn()).
        client.wait_until(
            lambda: _server_ns_states(client.broker_status()).get("ssh-agent") == "connected"
            and _server_ns_states(client.broker_status()).get("docker-credentials") == "connected",
            timeout=20, what="mock subscribed (ssh-agent + docker-credentials connected)")
        st = client.broker_status()
        assert st["effective_mode"] == "embedded"
        assert set(_server_ns_states(st)) == {"ssh-agent", "docker-credentials"}, st
        assert client.identify()["broker_mode"]["effective"] == "embedded"
        # What the embedded broker signs for a shed (plan 035 §3.5): the default allow
        # list, no key filter, the resolved mode; no counts until a shed lists keys.
        ssh = st["ssh"]
        assert ssh["allow_users"] == ["git"], ssh
        assert ssh["key_filter"] is False, ssh
        assert ssh["mode"] == st["resolved_ssh_mode"], (ssh, st["resolved_ssh_mode"])
        assert "keys_offered" not in ssh and "keys_available" not in ssh, ssh

        # set_mode round-trip. Launch pref was auto (effective embedded via the probe):
        # pinning embedded drifts the persisted pref ⇒ restart_required until relaunch.
        r = client.broker_set_mode("embedded")
        assert r["pref"] == "embedded" and r["effective"] == "embedded"
        assert r["restart_required"] is True
        assert client.broker_mode()["restart_required"] is True
        # Back to the launch value clears it (no relaunch needed).
        r = client.broker_set_mode("auto")
        assert r["pref"] == "auto" and r["restart_required"] is False
        assert client.broker_mode()["restart_required"] is False


# ---------------------------------------------------------------------------
# Cell (d) — split-namespace 409: a pre-registered namespace is Rejected, others connect
# ---------------------------------------------------------------------------

def test_split_namespace_409_surfaced_in_status():
    """A namespace already owned by a competing listener answers an older server's 409
    NAMESPACE_ALREADY_REGISTERED while the others connect: terminal `rejected` at the
    first refusal, never retried, for the embedded broker as for every bus client.
    Surfaced in status."""
    with _broker_app(discovery_mock=True, conflict_namespaces=("ssh-agent",)) as (client, mock, info):
        client.wait_until(
            lambda: _server_ns_states(client.broker_status()).get("ssh-agent") == "rejected"
            and _server_ns_states(client.broker_status()).get("docker-credentials") == "connected",
            timeout=20, what="ssh-agent rejected while docker-credentials connects")
        states = _server_ns_states(client.broker_status())
        assert states["ssh-agent"] == "rejected", states
        assert states["docker-credentials"] == "connected", states
        # Terminal client-side: exactly one subscribe attempt, and none later (the first
        # backoff steps, 1 s and 2 s, pass while the cell waits).
        assert mock.subscribe_count("ssh-agent") == 1, mock.subscribe_count("ssh-agent")
        time.sleep(3.0)
        assert mock.subscribe_count("ssh-agent") == 1, "no retry once rejected"
        # And the app's log file says which server refused it (#414).
        log = ui.app_log_path(info["runtime_dir"])
        client.wait_until(
            lambda: any("SSE subscription rejected" in line and "namespace ssh-agent server=mock " in line
                        for line in _log_lines(log)),
            timeout=10, what="the 409's line, naming its server, in the app's log file")


# ---------------------------------------------------------------------------
# Cell (d1) — the machine pin (plan 035 §3.4): identity on the wire, the three
# refusals, and the respond retries, through the EMBEDDED broker
# ---------------------------------------------------------------------------

# The broker's respond deadline (`bus.rs` RESPOND_RETRY_BUDGET: two 8 s attempts and the
# server's 1 s Retry-After): nothing of a respond outlives it.
_RESPOND_DEADLINE_S = 17.0


def _ns_row(status: dict, ns: str) -> dict:
    servers = status.get("servers") or []
    for n in (servers[0].get("namespaces", []) if servers else []):
        if n["namespace"] == ns:
            return n
    return {}


def _ping(rid: str) -> dict:
    return {
        "id": rid, "namespace": "ssh-agent", "type": "request", "final": True,
        "timestamp": "t", "payload": {"operation": "ping"},
        "shed": {"name": "web", "backend": "vz", "server": "mock"},
    }


def test_not_source_then_accepted_names_the_active_machine():
    """Refused 403 SOURCE_NOT_BOUND, the embedded broker reads `not-source` naming the
    active machine (per namespace and on the server row), keeps retrying, and connects
    with no relaunch once the server accepts it. Every subscribe carried the identity
    `broker.status` reports."""
    with _broker_app(discovery_mock=True, refuse={"ssh-agent": "not-source"}) as (client, mock, _i):
        # The two namespaces subscribe independently: wait for BOTH, never assume the
        # docker one is up because the ssh one has been refused.
        client.wait_until(
            lambda: (lambda st: _ns_row(st, "ssh-agent").get("state") == "not-source"
                     and _ns_row(st, "docker-credentials").get("state") == "connected")(
                         client.broker_status()),
            timeout=20, what="ssh-agent not-source and docker-credentials connected")
        st = client.broker_status()
        row = _ns_row(st, "ssh-agent")
        assert row["active_source"] == {
            "machine_id": "fedcba9876543210fedcba9876543210", "machine_name": "desk"}, row
        assert "the active source is desk" in row["last_error"], row
        assert st["servers"][0]["active_source"]["machine_name"] == "desk", st
        assert _ns_row(st, "docker-credentials").get("state") == "connected", st
        # The identity the embedded broker loaded is on the wire.
        machine = st["machine"]
        assert machine and len(machine["id"]) == 32, st
        assert st["machine_error"] is None, st
        client.wait_until(lambda: mock.subscribe_count("ssh-agent") >= 2,
                          timeout=10, what="a retry after not-source")
        for ident in mock.subscribe_identities("ssh-agent"):
            assert ident == (machine["id"], machine["name"]), ident
        # The server stops refusing: the next retry connects, no relaunch.
        mock.accept("ssh-agent")
        client.wait_until(
            lambda: _ns_row(client.broker_status(), "ssh-agent").get("state") == "connected",
            timeout=40, what="ssh-agent connected after accept")
        st = client.broker_status()
        assert "active_source" not in _ns_row(st, "ssh-agent"), st
        assert "active_source" not in st["servers"][0], st


def test_duplicate_stream_is_retried_not_rejected():
    """409 SOURCE_DUPLICATE_STREAM reads `duplicate` and keeps retrying — unlike the
    bare 409 cell above, which stops at one attempt."""
    with _broker_app(discovery_mock=True, refuse={"ssh-agent": "duplicate"}) as (client, mock, _i):
        client.wait_until(
            lambda: _ns_row(client.broker_status(), "ssh-agent").get("state") == "duplicate",
            timeout=20, what="ssh-agent duplicate")
        client.wait_until(lambda: mock.subscribe_count("ssh-agent") >= 2,
                          timeout=10, what="a retry after the duplicate")


@pytest.mark.parametrize("kind", ["forbidden", "unparseable-403"])
def test_other_403_is_reconnecting_never_not_source(kind):
    """Negative control: a 403 with any other code, or a body that does not parse, is
    plain `reconnecting` with the server's body — never `not-source`."""
    with _broker_app(discovery_mock=True, refuse={"ssh-agent": kind}) as (client, mock, _i):
        client.wait_until(lambda: mock.subscribe_count("ssh-agent") >= 2,
                          timeout=20, what="two attempts")
        row = _ns_row(client.broker_status(), "ssh-agent")
        assert row.get("state") == "reconnecting", row
        assert row.get("last_error", "").startswith("unexpected status 403: "), row
        assert "active_source" not in row, row


def test_respond_503_with_retry_after_is_resent():
    """A respond answered 503 + Retry-After is re-sent and delivered; a 403 is not."""
    with _broker_app(discovery_mock=True,
                     respond_script={"ssh-agent": [(503, "0")]}) as (client, mock, _i):
        mock.wait_for_subscribe("ssh-agent")
        client.wait_until(
            lambda: _ns_row(client.broker_status(), "ssh-agent").get("state") == "connected",
            timeout=20, what="ssh-agent connected")
        mock.push_request("ssh-agent", _ping("ping-503"))
        resp = mock.await_response("ssh-agent")
        assert resp["in_reply_to"] == "ping-503", resp
        assert len(mock.respond_identities("ssh-agent")) == 2, "the 503, then the delivered retry"
        # Now a 403: final, sent once, never delivered.
        mock.respond_script("ssh-agent", [(403, None)])
        mock.push_request("ssh-agent", _ping("ping-403"))
        client.wait_until(lambda: len(mock.respond_identities("ssh-agent")) >= 3,
                          timeout=10, what="the refused respond")
        # Read the count only after the respond's whole deadline (from its first send,
        # which came before this point) has passed: a retry cannot merely be late.
        time.sleep(_RESPOND_DEADLINE_S + 0.5)
        assert len(mock.respond_identities("ssh-agent")) == 3, "a 403 is never re-sent"


# ---------------------------------------------------------------------------
# Cell (d1b) — credential sources (plan 035 §3.4): the source rows, "Use this
# machine" over IPC and through the Preferences button, in embedded and external mode
# ---------------------------------------------------------------------------

# The machine the armed mock binds by default (`mockserver.NOT_SOURCE_ACTIVE`).
OTHER_MACHINE = "fedcba9876543210fedcba9876543210"
# A third machine that takes the source behind the app's back.
LAB_MACHINE = "abcdefabcdefabcdefabcdefabcdef01"

# The embedded broker's backoff after its fourth refusal (1, 2, 4, then 8 s; no
# jitter): with no wake, the next subscribe lands exactly this long after it.
_BACKOFF_AFTER_FOUR_S = 8.0
# How soon after that fourth refusal the WOKEN subscribe must come. 2 s inside the
# backoff at scale 1, and scaled with SHED_TAURI_TEST_TIMEOUT_SCALE for a loaded
# machine — but never closer than 0.5 s to the backoff itself, where a wake and the
# backoff could no longer be told apart (a larger scale cannot make the check vacuous).
_WAKE_BUDGET_S = min(scaled_timeout(6.0), _BACKOFF_AFTER_FOUR_S - 0.5)


def _source_row(client, host: str = "mock") -> dict:
    return next(r for r in client.source_status()["rows"] if r["host"] == host)


def _prefs_report(client) -> dict:
    return client.prefs_dump().get("prefs") or {}


def _prefs_source(client, host: str = "mock") -> dict | None:
    for row in (_prefs_report(client).get("values") or {}).get("sources") or []:
        if row["host"] == host:
            return row
    return None


def _open_prefs(client) -> None:
    client.show_preferences()
    client.wait_until(lambda: client.prefs_dump()["visible"] and _prefs_report(client),
                      timeout=20, what="preferences window visible + reported")


def _machine_json(info) -> Path:
    """Where the app reads this machine's identity: `ui.subproc_env` points
    SHED_BROKER_STATE_DIR at `<runtime>/broker-state`."""
    return Path(info["runtime_dir"]) / "broker-state" / "broker" / "machine.json"


def test_not_source_until_use_this_machine_then_connected_at_once():
    """Armed with another machine as the source, the embedded broker reads
    `not-source` naming it and keeps retrying on its backoff. `source.use` sends THIS
    machine's id (the one `broker.status` reports and every subscribe carried), wakes
    the broker, and both namespaces connect with no relaunch — sooner than the backoff
    the refusals had built up would have allowed."""
    with _broker_app(discovery_mock=True, source={}) as (client, mock, _i):
        client.wait_until(
            lambda: (lambda st: _ns_row(st, "ssh-agent").get("state") == "not-source"
                     and _ns_row(st, "docker-credentials").get("state") == "not-source")(
                         client.broker_status()),
            timeout=20, what="both namespaces not-source")
        st = client.broker_status()
        assert _ns_row(st, "ssh-agent")["active_source"] == {
            "machine_id": OTHER_MACHINE, "machine_name": "desk"}, st
        machine = st["machine"]
        assert machine and len(machine["id"]) == 32, st
        # It keeps retrying: four refusals, so the next backoff is 8 s.
        client.wait_until(lambda: mock.subscribe_count("ssh-agent") >= 4,
                          timeout=30, what="four not-source attempts")
        for ident in mock.subscribe_identities("ssh-agent"):
            assert ident == (machine["id"], machine["name"]), ident

        status = client.source_status()
        assert status["this_machine"] == {"id": machine["id"], "name": machine["name"]}, status
        assert status["broker_mode"] == "embedded", status
        row = _source_row(client)
        assert row == {"host": "mock", "supported": True,
                       "source": {"id": OTHER_MACHINE, "name": "desk"},
                       "this_machine": False, "connected": True, "busy": False,
                       "unreachable": False, "error": None}, row

        before = mock.subscribe_times("ssh-agent")
        used = client.source_use("mock")
        assert mock.source_use_requests() == [{"machine_id": machine["id"]}]
        assert used["woke_broker"] is True, used
        assert used["row"]["this_machine"] is True, used
        assert used["row"]["source"] == {"id": machine["id"], "name": machine["name"]}, used
        assert mock.source_bound() == machine["id"]
        client.wait_until(
            lambda: (lambda st: _ns_row(st, "ssh-agent").get("state") == "connected"
                     and _ns_row(st, "docker-credentials").get("state") == "connected")(
                         client.broker_status()),
            timeout=30, what="both namespaces connected after source.use")
        st = client.broker_status()
        assert "active_source" not in _ns_row(st, "ssh-agent"), st
        # The accepted subscribe came well inside the 8 s the broker would otherwise
        # have slept after its fourth refusal: the wake, not the backoff, brought it.
        after = mock.subscribe_times("ssh-agent")[len(before):]
        assert after, "no subscribe after the move"
        assert after[0] - before[-1] < _WAKE_BUDGET_S, (_WAKE_BUDGET_S, before, after)
        assert _source_row(client)["this_machine"] is True


def test_preferences_source_row_flips_after_use_this_machine():
    """Preferences shows one row per supporting server: the other machine's name with
    a "Use this machine" button, and a line naming this machine. Pressing the REAL
    button (its `data-source-use` element, clicked by the harness door) sends this
    machine's id; the row then reads "this machine" and the broker connects."""
    with _broker_app(discovery_mock=True, source={}) as (client, mock, _i):
        client.wait_until(
            lambda: _ns_row(client.broker_status(), "ssh-agent").get("state") == "not-source",
            timeout=20, what="ssh-agent not-source")
        machine = client.broker_status()["machine"]
        _open_prefs(client)
        client.wait_until(lambda: (_prefs_source(client) or {}).get("source_name") == "desk",
                          timeout=20, what="the source row naming desk")
        row = _prefs_source(client)
        assert row["this_machine"] is False and row["can_use"] is True, row
        assert row["busy"] is False and row["error"] is None, row
        values = _prefs_report(client)["values"]
        assert values["this_machine_name"] == machine["name"], values
        assert values["this_machine_error"] is None, values
        assert "broker" in _prefs_report(client)["sections"]

        client.press_source_use("mock")
        client.wait_until(lambda: mock.source_use_requests(),
                          timeout=15, what="the button's move request")
        assert mock.source_use_requests() == [{"machine_id": machine["id"]}]
        client.wait_until(
            lambda: (lambda r: r and r["this_machine"] and not r["busy"])(_prefs_source(client)),
            timeout=15, what="the row flipped to this machine")
        row = _prefs_source(client)
        assert row["source_name"] == machine["name"], row
        assert row["error"] is None, row
        # No button where this machine already is the source, and none reported.
        assert row["can_use"] is False, row
        client.wait_until(
            lambda: _ns_row(client.broker_status(), "ssh-agent").get("state") == "connected",
            timeout=30, what="ssh-agent connected after the button")


def test_unarmed_server_has_no_source_rows():
    """A server older than the feature (the unarmed mock: a plain 404 on
    `/api/sources`) is `supported: false` with no error, Preferences shows no source
    row and no new section, and a move is refused as unsupported."""
    with _broker_app(discovery_mock=True) as (client, mock, _i):
        mock.wait_for_subscribe("ssh-agent")
        status = client.source_status()
        assert status["rows"] == [{"host": "mock", "supported": False, "source": None,
                                   "this_machine": False, "connected": False,
                                   "busy": False, "unreachable": False,
                                   "error": None}], status
        assert status["this_machine"] is not None, status
        with pytest.raises(ShedError) as e:
            client.source_use("mock")
        assert e.value.code == "unsupported", e.value
        assert "does not support credential sources" in e.value.message
        _open_prefs(client)
        client.wait_until(lambda: "ssh" in (_prefs_report(client).get("sections") or []),
                          timeout=20, what="the gated ssh section")
        report = _prefs_report(client)
        assert report["sections"] == ["general", "terminal", "broker", "ssh"], report
        assert report["values"]["sources"] == [], report
        assert report["values"]["this_machine_name"] is None, report


def test_external_mode_rows_and_use_this_machine():
    """External mode (a shed-host-agent daemon brokers; here the fake): the app still
    shows the rows and moves the source, naming this machine by the identity file the
    daemon shares. A machine the server has never seen is refused in plain words that
    name the daemon, once; after its broker has reached the server the same move
    works."""
    fake = FakeHostAgent()
    fake.start()
    try:
        with _broker_app(host_agent_socket=fake.socket_path, source={}) as (client, mock, info):
            assert fake.wait_connected()
            assert client.broker_status()["effective_mode"] == "external"
            status = client.source_status()
            me = status["this_machine"]
            assert me and status["this_machine_error"] is None, status
            on_disk = json.loads(_machine_json(info).read_text())
            assert on_disk["id"] == me["id"] and on_disk["name"] == me["name"], on_disk
            row = _source_row(client)
            assert row["supported"] and row["source"]["name"] == "desk", row
            assert row["this_machine"] is False, row

            with pytest.raises(ShedError) as e:
                client.source_use("mock")
            assert e.value.code == "source_machine_not_found", e.value
            assert e.value.message.startswith("mock has not seen this machine yet"), e.value
            assert "separate broker daemon" in e.value.message, e.value
            assert mock.source_use_requests() == [{"machine_id": me["id"]}], "sent once"

            # What the daemon's first subscribe does on the real server.
            mock.know_machine(me["id"], me["name"])
            used = client.source_use("mock")
            assert used["woke_broker"] is False, used
            assert used["row"]["this_machine"] is True, used
            assert mock.source_use_requests()[-1] == {"machine_id": me["id"]}
            _open_prefs(client)
            client.wait_until(lambda: (_prefs_source(client) or {}).get("this_machine") is True,
                              timeout=20, what="the Preferences row reads this machine")
    finally:
        fake.stop()


def test_refused_moves_show_their_messages():
    """A move the server refuses is reported in its words: a 403 under the
    Preferences row (pressed through the real button, the row flips back), a 500 as
    "could not confirm", an unknown server as a bad request — each one request, never
    retried."""
    with _broker_app(discovery_mock=True, source={}) as (client, mock, _i):
        client.wait_until(lambda: _source_row(client)["supported"], timeout=20,
                          what="the source row")
        mock.source_use_script([403, 500])
        _open_prefs(client)
        client.wait_until(lambda: (_prefs_source(client) or {}).get("can_use") is True,
                          timeout=20, what="an enabled Use this machine button")
        client.press_source_use("mock")
        client.wait_until(lambda: (_prefs_source(client) or {}).get("error"),
                          timeout=15, what="the refusal under the row")
        row = _prefs_source(client)
        assert "refused the move: a script key cannot see or move a credential source" \
            in row["error"], row
        assert row["this_machine"] is False and row["busy"] is False, row
        assert len(mock.source_use_requests()) == 1

        with pytest.raises(ShedError) as e:
            client.source_use("mock")
        assert e.value.code == "action_failed", e.value
        assert "could not confirm the move" in e.value.message, e.value
        assert len(mock.source_use_requests()) == 2

        with pytest.raises(ShedError) as e:
            client.source_use("no-such-server")
        assert e.value.code == "bad_request", e.value
        assert len(mock.source_use_requests()) == 2
        assert mock.source_bound() == OTHER_MACHINE


def test_machine_name_with_control_and_bidi_characters_is_shown_cleaned():
    """A machine name is text from another machine: its control and Unicode format
    characters (a bell, a newline, a right-to-left override, a zero-width space) are
    gone wherever it is shown — the source row, Preferences, and the broker's own
    not-source status."""
    dirty = "desk\N{RIGHT-TO-LEFT OVERRIDE}\u0007\n\N{ZERO WIDTH SPACE}evil"
    with _broker_app(discovery_mock=True, source={"machine_name": dirty}) as (client, _m, _i):
        client.wait_until(lambda: _source_row(client)["supported"], timeout=20,
                          what="the source row")
        assert _source_row(client)["source"] == {"id": OTHER_MACHINE, "name": "deskevil"}
        client.wait_until(
            lambda: _ns_row(client.broker_status(), "ssh-agent").get("state") == "not-source",
            timeout=20, what="ssh-agent not-source")
        assert _ns_row(client.broker_status(), "ssh-agent")["active_source"]["machine_name"] \
            == "deskevil"
        _open_prefs(client)
        client.wait_until(lambda: (_prefs_source(client) or {}).get("source_name") == "deskevil",
                          timeout=20, what="the cleaned name in Preferences")


def test_no_machine_identity_disables_use_this_machine():
    """With no machine identity (the state dir cannot hold one) a move is refused
    with the reason and nothing is sent, and Preferences disables the button and says
    why."""
    with _broker_app(discovery_mock=True, source={}, block_state_dir=True) as (client, mock, _i):
        client.wait_until(lambda: _source_row(client)["supported"], timeout=20,
                          what="the source row")
        status = client.source_status()
        assert status["this_machine"] is None, status
        assert status["this_machine_error"], status
        with pytest.raises(ShedError) as e:
            client.source_use("mock")
        assert e.value.code == "machine_identity_unavailable", e.value
        assert e.value.message.startswith("this machine has no identity"), e.value
        assert mock.source_use_requests() == []
        _open_prefs(client)
        client.wait_until(lambda: _prefs_source(client) is not None,
                          timeout=20, what="the source row in Preferences")
        assert _prefs_source(client)["can_use"] is False
        values = _prefs_report(client)["values"]
        assert values["this_machine_error"] == status["this_machine_error"], values


def _close_prefs(client) -> None:
    client.prefs_close()
    client.wait_until(lambda: not client.prefs_dump()["visible"],
                      timeout=10, what="preferences window hidden")


def _holds(pred, seconds: float, what: str) -> None:
    """`pred` stays true for `seconds` (a stale answer must not land at any point)."""
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        assert pred(), f"no longer holds: {what}"
        time.sleep(0.1)


def test_busy_source_read_keeps_the_last_known_source_and_recovers():
    """`503 SOURCE_BUSY` is transient. Over IPC: two busy answers are retried away
    inside one read; busy on every ask is a `busy` row — supported, no error, the
    source unknown rather than absent. In Preferences, reopened after another machine
    took the source while the server is busy: the row keeps the source it last showed,
    marked retrying (never "no source"), and the one later read brings the new one."""
    with _broker_app(discovery_mock=True, source={}) as (client, mock, _i):
        client.wait_until(lambda: _source_row(client)["supported"], timeout=20,
                          what="the source row")
        gets = mock.source_gets()
        mock.source_busy(2, retry_after="0")
        row = _source_row(client)
        assert row["busy"] is False and row["source"]["name"] == "desk", row
        assert mock.source_gets() - gets == 3, "two busy asks, then the answer"

        mock.source_busy(3, retry_after="0")
        row = _source_row(client)
        assert row == {"host": "mock", "supported": True, "source": None,
                       "this_machine": False, "connected": False, "busy": True,
                       "unreachable": False, "error": None}, row

        _open_prefs(client)
        client.wait_until(lambda: (_prefs_source(client) or {}).get("source_name") == "desk",
                          timeout=20, what="the source row naming desk")
        _close_prefs(client)
        mock.move_source(LAB_MACHINE, "lab")
        mock.source_busy(3, retry_after="0")
        client.show_preferences()
        client.wait_until(lambda: (_prefs_source(client) or {}).get("retrying") is True,
                          timeout=20, what="the row marked busy, retrying")
        row = _prefs_source(client)
        assert row["source_name"] == "desk", f"the last known source, never none: {row}"
        assert row["error"] is None, row
        client.wait_until(
            lambda: (lambda r: r and r["source_name"] == "lab" and not r["retrying"])(
                _prefs_source(client)),
            timeout=20, what="the later read naming lab")


def test_source_row_survives_a_server_restart_and_recovers():
    """A read while the server is down (a restart) keeps the source row Preferences
    showed — the last known source, noted "cannot reach the server" — instead of
    dropping it, and the window reads again on its own: once the server is back the
    row is current again, with nothing shown, focused or reloaded meanwhile. Over IPC
    the same read is `unreachable`, never `supported: false` with nothing to show."""
    with _broker_app(discovery_mock=True, source={}) as (client, mock, _i):
        client.wait_until(lambda: _source_row(client)["supported"], timeout=20,
                          what="the source row")
        _open_prefs(client)
        client.wait_until(lambda: (_prefs_source(client) or {}).get("source_name") == "desk",
                          timeout=20, what="the source row naming desk")
        mock.pause()
        try:
            row = _source_row(client)
            assert row["unreachable"] is True and row["error"], row
            # Any prefs change reloads the window: its source read finds the server down.
            prefs = client.prefs_get()
            client.prefs_set_terminal(prefs["terminal_preset"], prefs.get("terminal_template"))
            client.wait_until(lambda: (_prefs_source(client) or {}).get("unreachable") is True,
                              timeout=20, what="the row kept, marked unreachable")
            row = _prefs_source(client)
            assert row["source_name"] == "desk", f"the last known source, never gone: {row}"
            assert row["error"] is None, row
            # Another machine takes the source while the server is down; the window must
            # learn it by itself once the server is back.
            mock.move_source(LAB_MACHINE, "lab")
            _holds(lambda: (_prefs_source(client) or {}).get("source_name") == "desk",
                   1.0, "the row stays while the server is down")
        finally:
            mock.resume()
        client.wait_until(
            lambda: (lambda r: r and r["source_name"] == "lab" and not r["unreachable"])(
                _prefs_source(client)),
            timeout=30, what="the row current again once the server is back")


def test_reopening_preferences_reads_a_source_taken_elsewhere():
    """Closing Preferences only hides it, so a reopen must read the sources again:
    this machine was the source when it closed, another machine took it meanwhile,
    and the reopened window says so and offers the button again."""
    with _broker_app(discovery_mock=True, source={}) as (client, mock, _i):
        client.wait_until(
            lambda: _ns_row(client.broker_status(), "ssh-agent").get("state") == "not-source",
            timeout=20, what="ssh-agent not-source")
        client.source_use("mock")
        _open_prefs(client)
        client.wait_until(lambda: (_prefs_source(client) or {}).get("this_machine") is True,
                          timeout=20, what="the row reading this machine")
        assert _prefs_source(client)["can_use"] is False
        _close_prefs(client)
        mock.move_source(OTHER_MACHINE, "desk")
        client.show_preferences()
        client.wait_until(
            lambda: (lambda r: r and r["source_name"] == "desk" and not r["this_machine"]
                     and r["can_use"])(_prefs_source(client)),
            timeout=20, what="the reopened window naming desk with the button back")


def test_a_read_overtaken_by_a_move_never_lands_over_it():
    """A read the window started BEFORE a move, answered after the move's own answer
    and read-back (the server took its document first), is older: it must not put
    the other machine back on the row."""
    with _broker_app(discovery_mock=True, source={}) as (client, mock, _i):
        client.wait_until(
            lambda: _ns_row(client.broker_status(), "ssh-agent").get("state") == "not-source",
            timeout=20, what="ssh-agent not-source")
        machine = client.broker_status()["machine"]
        _open_prefs(client)
        client.wait_until(lambda: (_prefs_source(client) or {}).get("can_use") is True,
                          timeout=20, what="an enabled Use this machine button")
        gets = mock.source_gets()
        mock.source_reads_late(3.0)
        # Any prefs change reloads the window: its source read is the late one.
        prefs = client.prefs_get()
        client.prefs_set_terminal(prefs["terminal_preset"], prefs.get("terminal_template"))
        client.wait_until(lambda: mock.source_gets() > gets, timeout=10,
                          what="the window's read reaching the server (desk)")
        client.press_source_use("mock")
        client.wait_until(
            lambda: (lambda r: r and r["this_machine"] and not r["busy"])(_prefs_source(client)),
            timeout=15, what="the move confirmed")
        client.wait_until(lambda: mock.source_late_answered() >= 1, timeout=15,
                          what="the overtaken read answered")
        _holds(lambda: (_prefs_source(client) or {}).get("source_name") == machine["name"],
               1.5, "the row still names this machine after the stale answer")


def test_moves_on_two_servers_at_once_and_a_second_press_lands_nowhere(tmp_path):
    """Two servers' buttons pressed back to back are both in flight at once (a move
    on one server never disables another's); a second press on a row whose move is
    in flight lands nowhere; both settle on this machine."""
    cfg = tmp_path / "config.yaml"
    cfg.write_text(CONFIG.read_text().replace(
        "default_server: mock",
        "    mock2:\n        host: 127.0.0.1\n        http_port: 8081\n"
        "        ssh_port: 2222\ndefault_server: mock"))
    with _broker_app(discovery_mock=True, source={}, shed_config_path=cfg) as (client, mock, _i):
        client.wait_until(
            lambda: _ns_row(client.broker_status(), "ssh-agent").get("state") == "not-source",
            timeout=20, what="ssh-agent not-source")
        _open_prefs(client)
        both = ("mock", "mock2")
        client.wait_until(
            lambda: all((_prefs_source(client, h) or {}).get("can_use") for h in both),
            timeout=20, what="both servers' buttons enabled")
        mock.delay_source_use(1.5)
        client.press_source_use("mock")
        client.press_source_use("mock2")
        client.wait_until(
            lambda: all((_prefs_source(client, h) or {}).get("busy") for h in both),
            timeout=5, what="both moves in flight at once")
        client.press_source_use("mock")  # the row's move is in flight: no button
        client.wait_until(
            lambda: all((lambda r: r and r["this_machine"] and not r["busy"])(
                _prefs_source(client, h)) for h in both),
            timeout=20, what="both rows reading this machine")
        assert len(mock.source_use_requests()) == 2, mock.source_use_requests()
        assert not any(_prefs_source(client, h)["can_use"] for h in both)


# ---------------------------------------------------------------------------
# Cell (d2) — the app's log file (#414): where it is, and what it records
# ---------------------------------------------------------------------------

def _log_lines(path) -> list[str]:
    try:
        return path.read_text(errors="replace").splitlines()
    except FileNotFoundError:
        return []


def test_log_file_records_startup_broker_bus_and_shutdown():
    """The app writes a log file (#414) and says where: `identify.log_path` is the
    platform default under the harness's redirected HOME / `XDG_STATE_HOME`, created
    0600 in 0700 directories the app made. It holds the startup line (version,
    platform, log path), the resolved broker mode with its pref, probe and config
    provenance, and the EMBEDDED bus's connect line naming the server it connected to
    — and, after an orderly quit, every shutdown phase and the closing line, which
    an app launched from Finder or a desktop launcher (stderr discarded) used to
    lose."""
    with _broker_app(discovery_mock=True) as (client, mock, info):
        log = ui.app_log_path(info["runtime_dir"])
        assert client.identify()["log_path"] == str(log)
        mock.wait_for_subscribe("ssh-agent")
        client.wait_until(
            lambda: any("SSE connected namespace=ssh-agent server=mock" in line
                        for line in _log_lines(log)),
            timeout=20, what="the embedded bus's connect line, naming its server, in the log")
        lines = _log_lines(log)
        starting = [line for line in lines if "shed-desktop-tauri: starting: version " in line]
        assert len(starting) == 1, lines
        assert starting[0].endswith(f"log {log}"), starting
        broker = ("shed-desktop-tauri: broker: mode=embedded pref=auto "
                  "probe{desktop_socket_live=false, status_socket_live=false} config=synthesized")
        assert any(line.endswith(broker) for line in lines), lines
        # Every line is stamped `<RFC 3339 UTC> <LEVEL> …`.
        for line in lines:
            ts, _, rest = line.partition(" ")
            assert ts.endswith("Z") and "T" in ts, line
            assert rest.lstrip().split(" ", 1)[0] in {"ERROR", "WARN", "INFO", "DEBUG"}, line
        assert stat.S_IMODE(log.stat().st_mode) == 0o600
        assert stat.S_IMODE(log.parent.stat().st_mode) == 0o700

        # An orderly quit (the tray's Quit, through the test-mode door): the
        # shutdown's phases land in the file before the process exits.
        client.call("app.quit")
        code = info["proc"].wait(timeout=scaled_timeout(20))
        assert code == 0, f"an orderly quit exits 0, not {code}"
        lines = _log_lines(log)
        for n in range(5):
            assert any(f"shed-desktop-tauri: shutdown: phase {n} (" in line for line in lines), \
                f"phase {n} missing: {[l for l in lines if 'shutdown' in l]}"
        assert any("shed-desktop-tauri: shutdown: done in " in line for line in lines), lines
        print(f"\n{log}:\n" + "\n".join(lines))


# ---------------------------------------------------------------------------
# Cell (e) — malformed extensions.yaml fails closed; the app stays up (§3.4)
# ---------------------------------------------------------------------------

def test_malformed_extensions_config_fails_closed_app_stays_up():
    """A present-but-invalid `extensions.yaml` fails the broker CLOSED (the daemon would
    exit 1): the error surfaces in `broker.status`, no bus subscribes, and the app keeps
    answering IPC."""
    bad = "ssh:\n  approval:\n    policy: not-a-real-policy\n"
    with _broker_app(discovery_mock=True, extensions_yaml=bad) as (client, mock, _info):
        # The app is up + hermetic (identify still answers).
        info = client.identify()
        assert info["test_mode"] is True and info["platform"] == "tauri"
        st = client.broker_status()
        assert st["config"]["source"] == "error", st
        assert st["config"]["message"], "expected a config error message"
        assert st["broker_error"], "expected broker_error surfaced"
        assert st["servers"] == [], st
        # No bus was ever started ⇒ nothing subscribed on the mock.
        assert mock.subscribed_namespaces() == [], mock.subscribed_namespaces()
        # One more IPC round-trip (a further broker_status read) buys real wall-clock
        # time, then re-assert: closes the deferred-subscribe window where a bus that
        # started just after the first assertion would otherwise go unnoticed.
        client.broker_status()
        assert mock.subscribed_namespaces() == [], mock.subscribed_namespaces()


# ---------------------------------------------------------------------------
# Cell (f) — fresh-install synthesized default: the exact namespace set (§3.4)
# ---------------------------------------------------------------------------

def test_synthesized_default_namespace_set():
    """No `extensions.yaml` ⇒ the synthesized fresh-install default: config source
    `synthesized`, ssh-agent GATED (policy shed-desktop), and the bus subscribes exactly
    ssh-agent + docker-credentials (docker subscribes-with-deny; aws unconfigured ⇒
    absent). Grounded in broker_bridge.rs `synthesize_default` + `spawn`."""
    with _broker_app(discovery_mock=True) as (client, mock, _info):
        # Both subscriptions land (wait_for_subscribe raises on timeout; the open-mode
        # return is the None Authorization header, so it isn't asserted on).
        mock.wait_for_subscribe("ssh-agent")
        mock.wait_for_subscribe("docker-credentials")
        st = client.broker_status()
        assert st["config"]["source"] == "synthesized", st
        # ssh-agent is the only GATED namespace (delegated to the app).
        assert st["gate_namespaces"] == ["ssh-agent"], st
        # The exact subscribed set — aws-credentials never subscribes.
        client.wait_until(
            lambda: set(_server_ns_states(client.broker_status())) == {"ssh-agent", "docker-credentials"},
            timeout=20, what="exactly ssh-agent + docker-credentials subscribed")
        assert "aws-credentials" not in mock.subscribed_namespaces(), mock.subscribed_namespaces()


# ---------------------------------------------------------------------------
# Cell (f) — per-server credential mode across all three broker modes (plan 002 C3)
# ---------------------------------------------------------------------------

MTLS_CONFIG = FIXTURES / "config-mtls.yaml"


@pytest.mark.parametrize(
    "mode,use_fake_agent",
    [("embedded", False), ("external", True), ("headless-coexist", False)],
)
def test_hosts_auth_surfaces_the_config_mode_in_every_broker_mode(mode, use_fake_agent):
    """`hosts.auth` reports each server's credential mode regardless of WHICH broker
    is doing the minting.

    The three modes reach mtls by different routes — external relays the CSR over the
    UDS, embedded and headless-coexist relay it into the in-process broker — but the
    app's view of "what shape does this server issue" is one registry seeded from
    config and updated by the core's CredentialObserver. This is the cell that would
    fail if a mode were wired without it.

    `learned` must be False everywhere: the hermetic mock is tokenless (no minter is
    installed in test mode), so nothing has been proven, and a surface that echoed the
    config into `learned` would be lying about the one thing it exists to report.
    """
    fake = None
    sock = None
    try:
        if use_fake_agent:
            fake = FakeHostAgent()
            fake.start()
            sock = fake.socket_path
        elif mode == "headless-coexist":
            # Only the STATUS socket is live ⇒ a headless daemon owns brokering;
            # the app is configured with the (absent) desktop sibling.
            fake = FakeHostAgent(status_only=True)
            fake.start()
            sock = os.path.join(os.path.dirname(fake.socket_path), "host-agent.sock")
        with _broker_app(host_agent_socket=sock, shed_config_path=MTLS_CONFIG) as (
            client, _mock, _info,
        ):
            assert client.broker_status()["effective_mode"] == mode
            hosts = {h["name"]: h for h in client.hosts_auth()}
            assert set(hosts) == {"legacy", "mock"}
            assert hosts["mock"]["auth_mode"] == "mtls"
            # ABSENT MEANS TOKEN — an entry written before certificates existed.
            assert hosts["legacy"]["auth_mode"] == "token"
            assert all(h["learned"] is False for h in hosts.values())
    finally:
        if fake:
            fake.stop()
