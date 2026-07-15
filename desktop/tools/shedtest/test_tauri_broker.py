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
the cells are fully isolated from each other and from the session instance (which
runs in EXTERNAL mode against the session fake host-agent — the existing cells).
The embedded broker resolves its server list from `~/.shed/config.yaml` under the
throwaway HOME (`shed_broker::DEFAULT_DISCOVERY_SOURCE`), which we point at the
private mock; because that URL is plain `http://`, `ServerTarget::is_secure` is
false, the bus attaches a static (empty) token and NEVER mints over SSH — so no
cell shells real `ssh` (hermetic token handling, §C4.2).

Target-scoped: gated on `--target tauri` (skipped otherwise), like `test_tauri.py`.
The mac target never starts an in-process broker, so none of this — nor the mock's
new bus routes — is exercised on the mac suite.
"""

from __future__ import annotations

import base64
import json
import os
import shutil
import subprocess
import tempfile
from contextlib import contextmanager
from pathlib import Path
from urllib.parse import urlsplit

import pytest

import ui
from client import TauriClient
from fake_host_agent import FakeHostAgent
from mockserver import MockShedServer

pytestmark = pytest.mark.skipif(
    os.environ.get("SHED_TEST_TARGET", "mac") != "tauri",
    reason="tauri-only: embedded credential-broker e2e",
)

FIXTURES = Path(__file__).resolve().parent / "fixtures"
CONFIG = FIXTURES / "config.yaml"


def _shed_config_for(mock_base_url: str) -> str:
    """A `~/.shed/config.yaml` pointing a single OPEN (http) server at the private
    mock — the embedded broker's discovery source. http:// ⇒ not secure ⇒ no minting
    ⇒ no live SSH (hermetic)."""
    port = urlsplit(mock_base_url).port
    return (
        "servers:\n"
        "    mock:\n"
        "        host: 127.0.0.1\n"
        f"        http_port: {port}\n"
        "        ssh_port: 2222\n"
        "default_server: mock\n"
        "sheds: {}\n"
    )


def _local_keys_extensions(shed_config_abs: str, audit_abs: str) -> str:
    """An `extensions.yaml` delegating ssh to the app gate (`shed-desktop`) + brokering
    the private mock. `ssh.mode: local-keys` pins the backend to the generated
    `~/.ssh/id_ed25519` (never the real ssh-agent); the durable audit lands at
    `audit_abs` so the cell can read the daemon-shaped approval outcome."""
    return (
        "ssh:\n"
        "  mode: local-keys\n"
        "  approval:\n"
        "    policy: shed-desktop\n"
        "discovery:\n"
        f"  source: {shed_config_abs}\n"
        '  watch: "off"\n'
        "logging:\n"
        "  enabled: true\n"
        f"  path: {audit_abs}\n"
    )


@contextmanager
def _broker_app(
    *,
    host_agent_socket: str | None = None,
    extensions_yaml: str | None = None,
    discovery_mock: bool = False,
    gen_ssh_key: bool = False,
    local_keys_ssh: bool = False,
    conflict_namespaces: tuple[str, ...] = (),
    clear_ssh_auth_sock: bool = True,
):
    """Launch a self-managed tauri instance with a PRIVATE mock shed-server, yield
    `(client, mock, info)`, and tear both down. `info` carries `ssh_blob`, `audit_path`,
    and `runtime_dir`.

    * `host_agent_socket` — the app's configured Surface-A socket; the three-way probe
      keys off it (`shed_app::probe_sockets_at`). `None` ⇒ neither daemon socket exists
      under the throwaway HOME ⇒ auto-detect resolves to **embedded**.
    * `extensions_yaml` — written to `~/.config/shed/extensions.yaml` (the daemon's
      default path) + pointed at via `SHED_TAURI_EXTENSIONS_CONFIG`; `None` ⇒ absent ⇒
      the bridge synthesizes the fresh-install default.
    * `local_keys_ssh` — write the local-keys delegating `extensions.yaml` (implies
      `discovery_mock` + `gen_ssh_key`); mutually exclusive with `extensions_yaml`.
    * `discovery_mock` — write `~/.shed/config.yaml` pointing the broker at the mock.
    * `gen_ssh_key` — generate `~/.ssh/id_ed25519`; `info["ssh_blob"]` is the wire pubkey.
    * `conflict_namespaces` — pre-register these on the mock so their subscribe 409s.
    * `clear_ssh_auth_sock` — drop `$SSH_AUTH_SOCK` from the child env (default) so
      `ssh.mode: ""` auto-detect can never reach the developer's real ssh-agent.
    """
    cfg = ui._SUBPROC["tauri"]
    if not cfg.binary.exists():
        raise RuntimeError(
            f"tauri binary not found at {cfg.binary}; build it first (make tauri-build).")
    if local_keys_ssh:
        discovery_mock = True
        gen_ssh_key = True

    mock = MockShedServer()
    mock.start()
    for ns in conflict_namespaces:
        mock.pre_register_conflict(ns)

    info: dict = {"ssh_blob": None, "audit_path": None, "runtime_dir": None}
    runtime_dir = Path(tempfile.mkdtemp(prefix="shed-brk-"))
    info["runtime_dir"] = runtime_dir
    try:
        if discovery_mock:
            shed_dir = runtime_dir / ".shed"
            shed_dir.mkdir(parents=True, exist_ok=True)
            (shed_dir / "config.yaml").write_text(_shed_config_for(mock.base_url))

        ext_path: Path | None = None
        if local_keys_ssh:
            audit_abs = str(runtime_dir / "broker-audit.jsonl")
            info["audit_path"] = audit_abs
            extensions_yaml = _local_keys_extensions(
                str(runtime_dir / ".shed" / "config.yaml"), audit_abs)
        if extensions_yaml is not None:
            ext_dir = runtime_dir / ".config" / "shed"
            ext_dir.mkdir(parents=True, exist_ok=True)
            ext_path = ext_dir / "extensions.yaml"
            ext_path.write_text(extensions_yaml)

        if gen_ssh_key:
            ssh_dir = runtime_dir / ".ssh"
            ssh_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
            key = ssh_dir / "id_ed25519"
            subprocess.run(
                ["ssh-keygen", "-t", "ed25519", "-f", str(key), "-N", "", "-q"],
                check=True,
            )
            # `ssh-ed25519 <base64 wire pubkey> <comment>` — the middle field is the
            # SSH-wire pubkey blob the sign request carries + the backend matches on.
            info["ssh_blob"] = (ssh_dir / "id_ed25519.pub").read_text().split()[1]

        env = ui.subproc_env(
            cfg, runtime_dir=runtime_dir, mock_base_url=mock.base_url,
            config_path=CONFIG, host_agent_socket=host_agent_socket,
        )
        # subproc_env only SETS the host-agent socket when one is given; clear an
        # inherited value so `None` truly means "no configured desktop socket".
        if not host_agent_socket:
            env.pop("SHED_TAURI_HOST_AGENT_SOCKET", None)
        if ext_path is not None:
            env["SHED_TAURI_EXTENSIONS_CONFIG"] = str(ext_path)
        else:
            env.pop("SHED_TAURI_EXTENSIONS_CONFIG", None)
        # Developer-machine leakage guard: `env.rs` gives `SHED_HOST_AGENT_SOCKET_DIR`
        # priority over the HOME-derived socket path, so an inherited value from the
        # dev's shell would silently redirect discovery off the throwaway HOME.
        # Unconditional (unlike SSH_AUTH_SOCK below) so every cell is covered.
        env.pop("SHED_HOST_AGENT_SOCKET_DIR", None)
        if clear_ssh_auth_sock:
            env.pop("SSH_AUTH_SOCK", None)

        sock = runtime_dir / cfg.sock_rel
        log = runtime_dir / "broker-ui.log"
        log_fh = open(log, "wb")
        proc = subprocess.Popen(
            [str(cfg.binary)], env=env, stdout=log_fh, stderr=subprocess.STDOUT)
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
    wire pubkey blob, `data` = base64 challenge bytes."""
    return {
        "id": rid,
        "namespace": "ssh-agent",
        "type": "request",
        "final": True,
        "timestamp": "t",
        "payload": {
            "operation": "sign",
            "public_key": ssh_blob,
            "data": base64.b64encode(b"shed-embedded-e2e").decode(),
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
           "gate image; the round-trip runs on the mac e2e-tauri gate + in-crate tests)",
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
    """A namespace already owned by a competing listener answers 409
    NAMESPACE_ALREADY_REGISTERED — terminal `rejected` (no retry) — while the others
    connect. The startup-race split state is safe (no double prompts) and surfaced."""
    with _broker_app(discovery_mock=True, conflict_namespaces=("ssh-agent",)) as (client, mock, _info):
        client.wait_until(
            lambda: _server_ns_states(client.broker_status()).get("ssh-agent") == "rejected"
            and _server_ns_states(client.broker_status()).get("docker-credentials") == "connected",
            timeout=20, what="ssh-agent rejected while docker-credentials connects")
        states = _server_ns_states(client.broker_status())
        assert states["ssh-agent"] == "rejected", states
        assert states["docker-credentials"] == "connected", states
        # 409 NAMESPACE_ALREADY_REGISTERED is terminal client-side: the bus must not
        # hot-retry a rejected namespace — exactly one subscribe attempt.
        assert mock.subscribe_count("ssh-agent") == 1, mock.subscribe_count("ssh-agent")


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
