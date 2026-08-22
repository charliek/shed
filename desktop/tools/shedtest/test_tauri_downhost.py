"""tauri-only down-host E2E: a per-host UNREACHABLE server surfaces as an error
row end-to-end (the Egress pane's error rows, the System pane's per-host df, and
— shed#300 — the Sheds pane's host-error strip + its failure-aware empty state),
while the reachable `mock` host stays healthy.

This is the e2e closure of the gap `test_tauri.py::test_egress_profiles_render`
documents (the shared harness blanket-redirects every configured server to the
one in-process mock, so the error row was only unit-tested). It launches its OWN
hermetic app instance — a distinct throwaway HOME/XDG_RUNTIME_DIR → a distinct
socket + single-instance lock (keyed to the socket dir), so it coexists with the
session instance without touching the session fixture — pointed at the dedicated
down-host fixture (`config-downhost.yaml`: servers `mock` + `down`) with `down`
named in the `SHED_TAURI_MOCK_UNREACHABLE_HOSTS` override (the backend redirects
it to a closed port → deterministic ECONNREFUSED). A second fixture
(`config-alldown.yaml`, BOTH servers overridden) covers the every-host-down case:
zero sheds + two failures, where the empty state must defer to the failures.

Gated on `--target tauri` (skipped otherwise), like `test_tauri.py`.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
from contextlib import contextmanager
from pathlib import Path

import pytest

import ui
from client import TauriClient
from fake_host_agent import FakeHostAgent

pytestmark = pytest.mark.skipif(
    os.environ.get("SHED_TEST_TARGET", "mac") != "tauri",
    reason="tauri-only: hermetic down-host per-host error row",
)

FIXTURES = Path(__file__).resolve().parent / "fixtures"
DOWNHOST_CONFIG = FIXTURES / "config-downhost.yaml"
ALLDOWN_CONFIG = FIXTURES / "config-alldown.yaml"


@contextmanager
def _downhost_app(mock_base_url: str, *, config: Path = DOWNHOST_CONFIG,
                  unreachable: tuple[str, ...] = ("down",)):
    """Launch a SECOND, independent tauri instance (its own throwaway HOME +
    XDG_RUNTIME_DIR → its own socket/lock) with a down-host fixture + the matching
    unreachable override, yield a ready client, and tear it down. Reuses the shared
    `ui` subprocess helpers (env / hermetic-wait / terminate) but passes its OWN
    socket/proc, so it never touches ui._state (the session instance's
    bookkeeping).

    It also owns a PRIVATE FakeHostAgent (started/stopped here, mirroring the
    session `fake` fixture): the throwaway app connects to its own socket, so this
    instance can never steal the session fake's single tracked connection and
    misroute its emit_request/emit_event frames."""
    cfg = ui._SUBPROC["tauri"]
    if not cfg.binary.exists():
        raise RuntimeError(
            f"tauri binary not found at {cfg.binary}; build it first (make tauri-build).")
    agent = FakeHostAgent()
    agent.start()
    try:
        # Short prefix: the socket lives under this dir (itself under a long macOS
        # TMPDIR), and a Unix socket path must stay under SUN_LEN (~104).
        runtime_dir = Path(tempfile.mkdtemp(prefix="shed-dh-"))
        sock = runtime_dir / cfg.sock_rel
        log = runtime_dir / "downhost-ui.log"
        env = ui.subproc_env(cfg, runtime_dir=runtime_dir, mock_base_url=mock_base_url,
                             config_path=config, host_agent_socket=agent.socket_path,
                             unreachable_hosts=unreachable)
        log_fh = open(log, "wb")
        proc = subprocess.Popen([str(cfg.binary)], env=env, stdout=log_fh, stderr=subprocess.STDOUT)
        try:
            ui.await_hermetic("tauri", sock=sock, mock_base_url=mock_base_url,
                              proc=proc, log=log)
            client = TauriClient(sock)
            try:
                client.wait_until(lambda: client.current_pane() is not None,
                                  timeout=30, what="down-host frontend ready")
                yield client
            finally:
                client.close()
        finally:
            ui.terminate(proc)
            log_fh.close()
            shutil.rmtree(runtime_dir, ignore_errors=True)
    finally:
        agent.stop()


@pytest.fixture
def downhost(mock):
    """A ready client for a self-managed down-host tauri instance. `mock` is the
    session-scoped shared mock server (reused, not re-created); the instance itself
    — and its private fake host-agent — is throwaway. The session `fake` fixture is
    deliberately NOT requested, so this module never perturbs it."""
    with _downhost_app(mock.base_url) as client:
        yield client


@pytest.fixture
def alldown(mock):
    """A ready client for an instance where EVERY configured host is unreachable
    (`config-alldown.yaml`, both servers overridden) — zero listable sheds plus two
    per-host failures, the empty-state-deference + two-failed-hosts shape."""
    with _downhost_app(mock.base_url, config=ALLDOWN_CONFIG,
                       unreachable=("down1", "down2")) as client:
        yield client


def test_egress_error_row_for_down_host(downhost):
    # The Egress pane renders the reachable mock's two profiles AND exactly one
    # error row for the unreachable `down` host — the e2e closure of the gap
    # test_tauri.py::test_egress_profiles_render documents.
    downhost.navigate("egress")
    downhost.wait_until(lambda: downhost.current_pane() == "egress", timeout=15, what="pane=egress")
    downhost.egress_show(tab="profiles")
    downhost.wait_until(lambda: (downhost.egress_dump() or {}).get("tab") == "profiles",
                        timeout=15, what="profiles sub-tab shown")
    downhost.wait_until(lambda: len((downhost.egress_dump() or {}).get("errors") or []) == 1,
                        timeout=15, what="down-host error row rendered")
    d = downhost.egress_dump()
    # mock's two profiles are intact (unaffected by the down host).
    rows = {(p["host"], p["name"], p["source"]) for p in d["profiles"]}
    assert rows == {("mock", "default", "config"), ("mock", "custom", "user")}, f"unexpected rows: {d}"
    # exactly one error row, for `down`, with a non-empty error message.
    assert len(d["errors"]) == 1, f"unexpected errors: {d['errors']}"
    err = d["errors"][0]
    assert err["host"] == "down"
    assert err["error"], f"down-host error row has an empty message: {err}"


def test_system_df_error_row_for_down_host(downhost):
    # The System pane's per-host df keeps the `down` host as an error row (usage
    # absent, error present) while `mock` stays healthy — never a dropped host.
    usage = downhost.system_df()
    by_host = {r["host"]: r for r in usage}
    assert set(by_host) == {"mock", "down"}, f"unexpected hosts: {by_host}"
    down = by_host["down"]
    assert down.get("usage") is None, f"down host unexpectedly has usage: {down}"
    assert down.get("error"), f"down host has no error: {down}"
    assert by_host["mock"].get("usage") is not None, f"mock host has no usage: {by_host['mock']}"


def test_badges_count_both_configured_hosts(downhost):
    # `hosts` counts CONFIGURED hosts (derived from system.df, which keeps the down
    # host as a row) → 2 in this instance, even though one is unreachable.
    downhost.wait_until(lambda: (downhost.badges() or {}).get("hosts") == 2,
                        timeout=15, what="badges report 2 configured hosts")
    assert downhost.badges()["hosts"] == 2


def test_a_failed_host_is_still_carried_and_surfaced_in_the_sidebar(downhost):
    # shed#300: the failed host used to be silently filter_mapped away — the Sheds
    # pane showed the reachable host's sheds and NOTHING about `down`. It is still
    # carried end-to-end; what changed is WHERE a person reads it. The Sheds pane
    # renders no error strip (an unreachable server is a status, and status lives
    # in the sidebar's SHED SERVERS section + the System pane); the payload keeps
    # the failure, which drives the sidebar and the empty state.
    # The mock's closed-port simulation is a transport failure → kind `other`
    # (the AgentUpgradeRequired kind is pinned at the Rust unit level —
    # ipc.rs::sheds_payload_carries_host_errors_for_two_failed_hosts).
    errs = downhost.sheds_host_errors()
    assert [e["server"] for e in errs] == ["down"], f"unexpected host_errors: {errs}"
    assert errs[0]["kind"] == "other"
    assert errs[0]["summary"].startswith("down: "), f"summary must name the host: {errs[0]}"
    assert errs[0]["detail"], f"the failure has no hover detail: {errs[0]}"
    # …and the rows the reachable host serves are untouched by the failure.
    assert downhost.sheds_list(), "the reachable mock host's sheds vanished"

    # UI truth: the shell holds the failure alongside a non-empty list (so no
    # empty state is showing) …
    downhost.wait_until(lambda: len(downhost.dashboard_dump()["host_errors"]) == 1,
                        timeout=15, what="the host failure to reach the shell")
    d = downhost.dashboard_dump()
    assert d["rows"], f"the reachable host's rows are missing: {d}"
    assert d["empty"] is None, f"a non-empty list must render no empty state: {d}"
    assert d["host_errors"][0]["summary"] == errs[0]["summary"]

    # … and the SIDEBAR is where it is actually visible. Two claims, both needed:
    # the row exists and is marked unreachable (which `system.df` alone could
    # produce), AND the row carries the FAILURE'S OWN text as its hover detail —
    # which only `hostErrors` can supply. Without the second assertion, dropping
    # the reason from the row (leaving a bare dot with no way to find out why)
    # would still pass, and the whole point of moving the strip here is that the
    # reason survives the move.
    downhost.wait_until(
        lambda: any(sv["name"] == "down" for sv in downhost.sidebar_dump()["servers"]),
        timeout=15, what="the failed host to appear in the sidebar",
    )
    servers = {sv["name"]: sv for sv in downhost.sidebar_dump()["servers"]}
    assert servers["down"]["reachable"] is False, f"sidebar hid the failure: {servers}"
    assert servers["down"]["detail"] == errs[0]["summary"], (
        f"the sidebar row lost the reason: {servers['down']}"
    )
    # A healthy host carries no reason — the detail is not a decoration.
    healthy = [sv for sv in servers.values() if sv["reachable"]]
    assert healthy, "the reachable host vanished from the sidebar"
    assert all(sv["detail"] == "" for sv in healthy), f"spurious detail: {healthy}"


def test_empty_state_defers_to_host_failures(alldown):
    # Every host down ⇒ zero sheds. The generic "No sheds yet" copy would tell a
    # user everything is fine and they simply haven't created one — the exact
    # config-blaming failure mode shed#300 is about. With TWO failures the empty
    # state leads with the unreachability and lists both summaries.
    alldown.wait_until(lambda: len(alldown.dashboard_dump()["host_errors"]) == 2,
                       timeout=20, what="both host failures rendered")
    d = alldown.dashboard_dump()
    assert d["rows"] == [], f"no host is reachable, so no rows: {d}"
    assert {e["server"] for e in d["host_errors"]} == {"down1", "down2"}, f"unexpected: {d}"
    empty = d["empty"]
    assert empty is not None, "the Sheds pane rendered no empty state"
    assert empty["title"] == "Some shed servers are unreachable", f"unexpected empty state: {empty}"
    assert "No sheds yet" not in empty["title"]
    # It NAMES the servers and points at the sidebar, but carries no raw transport
    # error — the reason is one place (the sidebar row's hover), not reprinted here.
    for e in d["host_errors"]:
        assert e["server"] in empty["body"], f"{e['server']!r} missing from {empty['body']!r}"
        assert e["summary"] not in empty["body"], f"raw error text leaked into {empty['body']!r}"
    assert "SHED SERVERS" in empty["body"], f"the empty state points nowhere: {empty}"


def test_sheds_refresh_returns_the_same_payload_as_list(alldown):
    # `sheds.refresh` (drive the frontend, wait for the echo) answers with the SAME
    # {sheds, host_errors} shape as `sheds.list`, so a refreshing caller can't be
    # handed a payload that omits the failures. The values come from the snapshot
    # the frontend just committed — ONE listing serves both the render and this
    # reply (no second backend fetch stacked on the round-trip).
    r = alldown.call("sheds.refresh")
    assert r["sheds"] == []
    assert {e["server"] for e in r["host_errors"]} == {"down1", "down2"}, f"unexpected: {r}"


def test_identify_stays_hermetic(downhost, mock):
    # The down-host override doesn't perturb the hermeticity contract: identify
    # still echoes the mock base URL + the tauri platform (the handshake untouched).
    info = downhost.identify()
    assert info["mock_base_url"] == mock.base_url
    assert info["platform"] == "tauri"
    assert info["test_mode"] is True
