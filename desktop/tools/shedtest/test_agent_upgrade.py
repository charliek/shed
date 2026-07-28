"""mac-only agent-upgrade E2E: an OLD shed-host-agent + an mtls server surfaces
as a REMEDY-FIRST failure end to end — no generated-enum wrapper in the banner,
and an empty state that names the upgrade instead of blaming ~/.shed/config.yaml
(plan 006 D5/D6, shed#297 + shed#300).

The whole chain runs for real: the fake host-agent's `hello_ack` advertises no
`credential.get` (an old agent), the config entry is cached as `auth_mode: mtls`,
and the mock answers 401 — the shape a secure shed-server presents to a client
that could not mint anything. So the Swift minter throws the typed
`AgentUpgradeRequired`, the Rust core's `refused_because` preserves it through
the refusal, and `AppModel` maps it to a typed `HostFailure` the banner and the
empty state both read.

Two harness overrides make that expressible hermetically:
  * `SHED_DESKTOP_MOCK_CREDENTIAL_HOSTS` — the named host keeps its REAL
    credential wiring against the mock (host agent + the config's auth_mode)
    instead of the tokenless open-mode shortcut every other mock host takes;
  * `MockShedServer(require_auth=True)` — a private mock instance that 401s an
    unauthenticated /api request.

Only ONE mac app can exist at a time (fixed control socket + single-instance
lock under ~/Library/Caches/ShedDesktop), so this module OWNS the app for its
duration — it quits the session instance, runs against its own, and relaunches
the session one at teardown. The session `fake` host-agent is reused (the module
never runs concurrently with the session app, so there is no connection to
steal).
"""

from __future__ import annotations

import shutil
import tempfile
import types
from pathlib import Path

import pytest

import ui
from _marks import mac_only
from client import ShedDesktop
from mockserver import MockShedServer

pytestmark = mac_only

FIXTURES = Path(__file__).resolve().parent / "fixtures"
UPGRADE_CONFIG = FIXTURES / "config-agent-upgrade.yaml"
SESSION_CONFIG = FIXTURES / "config.yaml"
HOST = "mtlshost"

# Substrings that mean a generated FFI enum reached a rendered string — the
# shed#300 leak. None of them may appear in anything the user reads.
ENUM_WRAPPERS = ("Config(message:", "Transport(message:", "BadStatus(", "ShedError.",
                 "AgentUpgradeRequired(")


@pytest.fixture(scope="module")
def upgrade(mock, fake):
    """The app, hermetic, against a 401-ing mock + an old host-agent + an
    mtls-cached host. Restores the session instance (shared `mock` + the standard
    fixture config) on the way out."""
    probe = ui.make_client("mac")
    try:
        core = probe.identify().get("core")
    finally:
        probe.close()
    if core != "rust":
        # The legacy URLSession path cannot present a certificate at all — it
        # refuses an mtls entry up front (§7 P6), a different failure with a
        # different remedy. Nothing here applies to it.
        pytest.skip("the control-credential path only exists on the Rust core leg")
    secure = MockShedServer(require_auth=True)
    secure.start()
    token_baseline = len(fake.token_requests)
    state_dir = Path(tempfile.mkdtemp(prefix="shed-e2e-upgrade-"))
    ui.quit("mac")
    ui.launch("mac", mock_base_url=secure.base_url, config_path=UPGRADE_CONFIG,
              state_dir=state_dir, host_agent_socket=fake.socket_path,
              credential_hosts=(HOST,))
    client = ShedDesktop(ui.socket_path("mac"))
    try:
        assert fake.wait_connected(timeout=15), "the app never handshook with the fake agent"
        # The capability is learned per connection; until the ack lands the mint
        # refuses with the (correct, transient) "still connecting" sentence. Drive
        # refreshes until the settled, typed failure appears.
        client.wait_until(_typed_failure(client), timeout=30,
                          what="the agent-upgrade host failure")
        yield types.SimpleNamespace(client=client, fake=fake, token_baseline=token_baseline)
    finally:
        client.close()
        ui.quit("mac")
        shutil.rmtree(state_dir, ignore_errors=True)
        secure.stop()
        # Hand the session instance back exactly as `_app_session` launched it.
        ui.launch("mac", mock_base_url=mock.base_url, config_path=SESSION_CONFIG,
                  state_dir=Path(tempfile.mkdtemp(prefix="shed-e2e-mac-")),
                  host_agent_socket=fake.socket_path)


def _typed_failure(client: ShedDesktop):
    def ready() -> bool:
        client.refresh()
        host = client.host_list()[0]
        return (host.get("failure") or {}).get("kind") == "agent_upgrade_required"
    return ready


def _host(client: ShedDesktop) -> dict:
    hosts = client.host_list()
    assert len(hosts) == 1, f"expected the single fixture host, got {hosts}"
    return hosts[0]


def test_host_failure_is_typed_and_names_the_remedy_first(upgrade):
    host = _host(upgrade.client)
    assert host["reachable"] is False
    failure = host["failure"]
    assert failure["kind"] == "agent_upgrade_required", failure
    assert failure["server"] == HOST
    # REMEDY FIRST: the banner is one line and truncates from the end, so the
    # action must lead.
    assert failure["summary"].startswith("Upgrade shed-host-agent"), failure["summary"]
    assert HOST in failure["summary"]
    # The cause is demoted to the detail (the tooltip / DiagnosticLog body).
    assert "does not support `credential.get`" in failure["detail"], failure["detail"]
    assert "requires auth.mode: mtls" in failure["detail"], failure["detail"]
    # `last_error` keeps carrying the summary for the string-only consumers.
    assert host["last_error"] == failure["summary"]


def test_banner_leads_with_the_upgrade_and_leaks_no_enum_wrapper(upgrade):
    state = upgrade.client.ui_state()
    banner = state["last_error"]
    assert banner.startswith("Upgrade shed-host-agent"), banner
    for wrapper in ENUM_WRAPPERS:
        assert wrapper not in banner, f"the banner leaked {wrapper!r}: {banner}"


def test_empty_state_names_the_upgrade_not_config_yaml(upgrade):
    state = upgrade.client.ui_state()
    assert state["sheds"] == [], "the 401-ing host can list nothing"
    empty = state["sheds_empty_state"]
    assert empty.startswith("Upgrade shed-host-agent"), empty
    assert "config.yaml" not in empty, f"a known cause must not blame config: {empty}"
    for wrapper in ENUM_WRAPPERS:
        assert wrapper not in empty, f"the empty state leaked {wrapper!r}: {empty}"


def test_the_old_agent_is_never_asked_for_a_token(upgrade):
    # A bearer token cannot authenticate to an mtls server; asking for one would
    # trade this actionable failure for an opaque 401 (plan 002 §7 P5). The fake
    # records every frame it receives, so "no token.get since this app launched"
    # is directly observable.
    assert len(upgrade.fake.token_requests) == upgrade.token_baseline, \
        f"a token.get went out to an mtls server: {upgrade.fake.token_requests}"
