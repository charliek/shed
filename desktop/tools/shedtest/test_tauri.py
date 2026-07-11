"""tauri-only E2E: behavior specific to the Tauri client's runtime that has no
mac analog — the single-instance hand-off, and the A0a UI ops (navigate /
show_window / activate) that aren't in the cross-target shared suite. Gated on
`SHED_TEST_TARGET`, so the whole module is skipped unless `--target tauri`.
"""

from __future__ import annotations

import os
import platform
import subprocess

import pytest

import ui
from client import ShedError, scaled_timeout

PNG_MAGIC = b"\x89PNG\r\n\x1a\n"

pytestmark = pytest.mark.skipif(
    os.environ.get("SHED_TEST_TARGET", "mac") != "tauri",
    reason="tauri-only: single-instance hand-off + A0a UI ops",
)


def test_ui_ops_ack(tauri):
    # show_window + activate raise the window and ack (no frontend needed). Once the
    # frontend is ready, ui.navigate acks too. A raised error (non-`{}` envelope)
    # fails the call.
    tauri.show_window()
    tauri.activate()
    tauri.wait_until(lambda: tauri.current_pane() is not None, timeout=15, what="frontend ready")
    tauri.navigate("system")
    tauri.navigate("sheds")


def test_tray_dump(tauri):
    # B1a: the menu-bar/tray is drivable over IPC (the North Star). Its actionable
    # menu ids are always reported; the tray *installs* on macOS (a status-bar host
    # is always present), while a headless / no-SNI Linux box may be window-only.
    dump = tauri.call("tray.dump")
    # B1: the menu opens the dashboard, its Approvals/Preferences panes, or quits.
    assert dump["items"] == ["open", "approvals", "preferences", "quit"]
    if platform.system() == "Darwin":
        assert dump["present"] is True


def test_tray_popover_drivable(tauri):
    # B1b: the mac menu-bar popover is drivable + observable over IPC — OS tray
    # clicks aren't hermetic, so tray.show/tray.dump ARE the drivability AC (a real
    # screenshot is the maintainer's manual native-feel check). The popover is a 2nd
    # webview mirroring the Swift MenuBarContentView; it reports its OWN compact rows
    # under the `popover` window key. macOS-only — Linux emits no tray click events
    # and creates no popover window.
    if platform.system() != "Darwin":
        pytest.skip("the tray popover is macOS-only (Linux tray has no click events)")

    # The popover reports its rows on mount (even hidden), so tray.dump's popover
    # block carries the host-agent + running-sheds state regardless of visibility.
    tauri.wait_until(lambda: tauri.tray_dump().get("popover") is not None,
                     timeout=20, what="popover reported its rows")
    pop = tauri.tray_dump()["popover"]
    assert set(pop) >= {"connected", "running_sheds", "pending_approvals"}
    assert isinstance(pop["running_sheds"], list) and isinstance(pop["pending_approvals"], list)

    # The window-keyed report did NOT clobber the dashboard's `main` snapshot:
    # current_pane still reflects the shell (B1b.1's per-window keying).
    tauri.wait_until(lambda: tauri.current_pane() is not None, timeout=15, what="frontend ready")
    assert tauri.current_pane() in {"sheds", "approvals", "agents", "activity", "egress", "system"}

    # Drive the show path (the tray-icon-click analog) → the popover becomes visible;
    # hide → invisible. This is the hermetic stand-in for the (non-drivable) OS click.
    tauri.tray_show()
    tauri.wait_until(lambda: tauri.tray_dump()["popover_visible"] is True,
                     timeout=15, what="popover visible after tray.show")

    # M2: the popover CONTENT-SIZES to hug its rows (Swift NSPopover parity, no dead
    # space). It's built at MAX height (640) and shrunk by the resize_popover protocol;
    # a silently-ignored set_size (the borderless-window regression this de-risks) would
    # leave it stuck at 640. popover_height is logical px (display-independent), so a
    # content-sized popover reads well under 640 for the mock's small fixture.
    tauri.wait_until(lambda: (tauri.tray_dump().get("popover_height") or 999) < 600,
                     timeout=15, what="popover content-sized (not stuck at MAX height)")
    h = tauri.tray_dump()["popover_height"]
    assert 120 <= h < 600, f"popover_height {h} not content-sized within [120, 600)"

    tauri.tray_hide()
    tauri.wait_until(lambda: tauri.tray_dump()["popover_visible"] is False,
                     timeout=15, what="popover hidden after tray.hide")


def test_navigate_rejects_unknown_pane(tauri):
    # An unknown pane is a bad_request, not blindly emitted — a bogus pane would
    # otherwise blank the UI (PANES[pane] undefined).
    tauri.wait_until(lambda: tauri.current_pane() is not None, timeout=15, what="frontend ready")
    with pytest.raises(ShedError) as e:
        tauri.navigate("bogus")
    assert e.value.code == "bad_request"


def test_navigate_reports_rendered_pane(tauri):
    # A0b round-trip: ui.navigate emits `navigate` → React switches the pane and
    # reports it back (ui_report) → ui.current_pane reflects the RENDERED pane (the
    # dashboard.dump-is-UI-truth pattern). Proves the WebView is running the app.
    # current_pane becomes non-null only once the navigate listener is registered,
    # so this wait also rules out the listener-attach race.
    tauri.wait_until(lambda: tauri.current_pane() is not None, timeout=15, what="frontend ready")
    tauri.navigate("system")
    tauri.wait_until(lambda: tauri.current_pane() == "system", timeout=15, what="pane=system")
    tauri.navigate("agents")
    tauri.wait_until(lambda: tauri.current_pane() == "agents", timeout=15, what="pane=agents")


def _norm(color: str) -> str:
    """Whitespace/case-insensitive color compare (hex token vs computed rgb)."""
    return "".join(color.split()).lower()


def test_computed_style_probe_confirms_theme(tauri):
    # The machine-checkable half of the WebKitGTK render gate: the WebView actually
    # applied the Plex CSS, so the body background resolves to a real (non-
    # transparent) color and the accent token is the brand orange #F2541B. If
    # color-mix failed to parse, the var-backed bg would fall back to transparent
    # (rgba(0, 0, 0, 0)); a missing token would blank the accent.
    tauri.wait_until(lambda: (tauri.computed_style() or {}).get("bg"), timeout=15, what="computed style reported")
    style = tauri.computed_style()
    assert style["bg"], f"no body background reported: {style}"
    assert style["bg"] != "rgba(0, 0, 0, 0)", f"Plex theme not applied (bg transparent): {style}"
    # The accent token is the hard gate now, not just non-transparency: #F2541B.
    assert _norm(style["accent"]) in {"#f2541b", "rgb(242,84,27)"}, \
        f"accent token is not the brand orange #F2541B: {style}"


def test_set_appearance_drives_dark_and_light(tauri):
    # ui.set_appearance drives the dashboard light/dark mode deterministically (so
    # the harness can capture dark screenshots without the header toggle): dark
    # flips the reported mode + the body background; light restores both. An unknown
    # mode is a bad_request. Restore to light after (the app is session-scoped).
    tauri.wait_until(lambda: (tauri.computed_style() or {}).get("bg"), timeout=15, what="computed style reported")
    light_bg = tauri.computed_style()["bg"]
    try:
        tauri.set_appearance("dark")
        tauri.wait_until(lambda: (tauri.computed_style() or {}).get("mode") == "dark",
                         timeout=15, what="mode=dark")
        dark = tauri.computed_style()
        assert dark["mode"] == "dark"
        assert dark["bg"] != light_bg, f"dark bg did not change: {dark}"
        assert _norm(dark["accent"]) in {"#f2541b", "rgb(242,84,27)"}  # accent constant across modes
        with pytest.raises(ShedError) as e:
            tauri.set_appearance("sepia")
        assert e.value.code == "bad_request"
    finally:
        tauri.set_appearance("light")
        tauri.wait_until(lambda: (tauri.computed_style() or {}).get("mode") == "light",
                         timeout=15, what="mode=light")
    assert tauri.computed_style()["bg"] == light_bg


def test_sidebar_badges_match_fixture(tauri):
    # The sidebar nav badge counts are UI truth, reported over IPC (ui.badges), so
    # they're asserted as logical content, not pixels. The default fixture has 2
    # sheds on 1 configured host, no RC sessions, no pending approvals. `hosts`
    # counts CONFIGURED hosts (from system_df, an async fetch), so wait on the full
    # snapshot rather than `sheds` alone — the host count lands independently.
    want = {"sheds": 2, "agents": 0, "hosts": 1, "pending": 0}
    tauri.wait_until(lambda: (tauri.badges() or {}) == want,
                     timeout=15, what="sidebar badges reported")
    b = tauri.badges()
    assert b == want, f"unexpected badges: {b}"


def test_second_launch_hands_off(tauri):
    # A second shed-desktop-tauri against the same runtime must detect the running
    # instance (the single-instance plugin), hand off by raising it, and exit —
    # never bind a second socket or leave a process behind.
    proc = subprocess.Popen(
        [str(ui.TAURI_BIN)], env=ui.launch_env("tauri"),
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    try:
        rc = proc.wait(timeout=scaled_timeout(10))
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait()
        pytest.fail("second shed-desktop-tauri did not exit within 10s — single-instance hand-off hung")
    assert rc == 0, f"second instance exited {rc}, want 0 (a clean single-instance hand-off)"

    # The first instance is untouched: its socket still answers identify...
    assert tauri.identify()["platform"] == "tauri"
    # ...and app.activate (the op the hand-off invoked) still succeeds.
    tauri.call("app.activate")


_GiB = 1024 ** 3


def test_system_df_returns_per_host_usage(tauri):
    # A1c: the System pane's per-host disk usage on the shared shed-app Backend
    # (the same `system.df` the mac app exposes). The row shape + values match the
    # mock df fixture.
    usage = tauri.system_df()
    assert usage, "expected at least one host"
    row = usage[0]
    assert row["host"] == "mock"
    totals = row["usage"]["totals"]
    assert totals["sheds"]["physical_bytes"] == _GiB
    assert totals["all"]["physical_bytes"] == _GiB + _GiB // 2


def test_system_pane_renders(tauri):
    # A1c parity with the mac test_system_pane_renders + the A1b dashboard render:
    # the live SystemPane paints per-host df without crashing (the data itself is
    # pinned by test_system_df_returns_per_host_usage above). Screenshot is the
    # smoke; TCC-gated on macOS, so Linux/Xvfb is the real gate.
    if platform.system() == "Darwin":
        pytest.skip("tauri screenshot on macOS is Screen-Recording-TCC-gated; Linux/Xvfb is the gate")
    tauri.wait_until(lambda: tauri.current_pane() is not None, timeout=15, what="frontend ready")
    tauri.show_window()
    tauri.navigate("system")
    tauri.wait_until(lambda: tauri.current_pane() == "system", timeout=15, what="pane=system")
    png, w, h = tauri.screenshot(scale=1)
    assert png[:8] == PNG_MAGIC and w > 0 and h > 0


def test_terminal_preview_builds_ssh_command(tauri):
    # A1c-2a: the shared ssh command (mac+tauri parity), built from the fixture's
    # ssh endpoint (127.0.0.1:2222) with strict host-key pinning.
    # No spawn — terminal.open (the preset launch) is A1c-2b.
    r = tauri.terminal_preview("hello-world")
    argv = r["argv"]
    assert argv[0] == "ssh" and "-t" in argv
    assert "StrictHostKeyChecking=yes" in argv
    assert "hello-world@127.0.0.1" in argv  # shed name is the ssh user
    assert argv[argv.index("-p") + 1] == "2222"
    # a tmux session attaches
    r2 = tauri.terminal_preview("hello-world", session="main")
    assert r2["argv"][-4:] == ["tmux", "attach", "-t", "main"]


def test_terminal_presets_and_open_gate(tauri):
    # A1c-2b: the offerable presets (Ghostty/Roost/Custom; custom always installed)
    # and terminal.open disabled in test mode (spawning a terminal isn't hermetic).
    presets = {p["id"]: p for p in tauri.terminal_presets()}
    assert set(presets) == {"ghostty", "roost", "custom"}
    assert presets["custom"]["available"] is True
    with pytest.raises(ShedError) as e:
        tauri.terminal_open("hello-world")
    assert e.value.code == "not_enabled"
    # an explicit but unrecognized preset (e.g. a mac-only one) is a bad_request,
    # not a silent coercion to Custom.
    with pytest.raises(ShedError) as e2:
        tauri.terminal_preview("hello-world", preset="iterm2")
    assert e2.value.code == "bad_request"


def test_terminal_preview_resolves_custom_invocation(tauri):
    # A1c-2b: a custom preset resolves to `/bin/sh -c <template>` with {cmd}/{shed}
    # substituted — the deterministic cross-platform launch (script presets need the
    # bundled openers, so they fall back in the unbundled harness).
    r = tauri.terminal_preview("hello-world", preset="custom", template="kitty -e {cmd} # {shed}")
    assert r["preset"] == "custom"
    inv = r["invocation"]
    assert inv["executable"] == "/bin/sh"
    assert inv["arguments"][0] == "-c"
    assert r["command"] in inv["arguments"][1]  # {cmd} substituted
    assert "# hello-world" in inv["arguments"][1]  # {shed} substituted


def test_terminal_pref_persists_and_drives_preview(tauri):
    # A1c-2c: prefs.set_terminal persists the preset (+ template) and prefs.get
    # reflects it; terminal.preview WITHOUT an explicit preset falls back to the
    # persisted pref (so the shed-card button opens the user's chosen terminal).
    tauri.prefs_set_terminal("custom", template="myterm -e {cmd}")
    got = tauri.prefs_get()
    assert got["terminal_preset"] == "custom"
    assert got["terminal_template"] == "myterm -e {cmd}"
    # preview with no preset uses the persisted pref → the custom invocation
    r = tauri.terminal_preview("hello-world")
    assert r["preset"] == "custom"
    assert r["command"] in r["invocation"]["arguments"][1]
    # switching the preset persists (across the store's write-through)
    tauri.prefs_set_terminal("ghostty")
    assert tauri.prefs_get()["terminal_preset"] == "ghostty"


def test_ssh_prefs_round_trip_and_partial_update(tauri):
    # B4: the full {method, policy, ttl} is drivable + observable. ui.set_ssh_approval
    # applies it; ui.ssh_prefs reads back exactly what the coordinator holds — and a
    # partial update (one field) composes with the rest, the property the modal relies
    # on when it sends only the changed control. Restore the prior prefs after (the app
    # + coordinator are session-scoped, so a left-over policy would leak to later tests).
    before = tauri.ssh_prefs_get()
    try:
        tauri.set_ssh_approval(method="prompt", policy="time-based-allow", ttl="4h")
        got = tauri.ssh_prefs_get()
        assert got["method"] == "prompt"
        assert got["policy"] == "time-based-allow"
        assert got["ttl"] == "4h"
        # a policy-only update leaves method + ttl untouched (partial-update compose)
        tauri.set_ssh_approval(policy="always-allow")
        got = tauri.ssh_prefs_get()
        assert got["policy"] == "always-allow"
        assert got["method"] == "prompt"
        assert got["ttl"] == "4h"
    finally:
        tauri.set_ssh_approval(
            method=before["method"], policy=before["policy"], ttl=before["ttl"]
        )


def test_loginitem_probe(tauri):
    # B4: launch-at-login is drivable + observable (the Swift PreferencesView
    # "Launch at login" toggle parity). On Linux (the shipped target) `auto-launch`
    # writes a .desktop under the throwaway HOME/XDG → a REAL hermetic round-trip;
    # on macOS a real write hits a LaunchAgent/TCC, so test mode round-trips through
    # an in-memory cell instead — either way the IPC + status path is exercised.
    # Restore the initial state after (the app is session-scoped, so a left-over
    # login item would leak into later tests / the dev's environment).
    before = tauri.login_item_status()
    assert before is False  # default off at a hermetic launch
    try:
        tauri.login_item_set(True)
        assert tauri.login_item_status() is True
        tauri.login_item_set(False)
        assert tauri.login_item_status() is False
    finally:
        tauri.login_item_set(before)


def test_preferences_modal_opens(tauri):
    # A1c-2c(2): ui.show_preferences → the frontend opens the in-app Preferences modal
    # and reports modal=="prefs", so the harness verifies it ACTUALLY rendered (round-trip:
    # op → event → React → modal → report), not just that the op acked. The pref LOGIC
    # is covered by test_terminal_pref_persists above.
    tauri.wait_until(lambda: tauri.current_pane() is not None, timeout=15, what="frontend ready")
    tauri.show_preferences()
    tauri.wait_until(lambda: tauri.modal() == "prefs", timeout=15, what="preferences modal open")
    # ...and on the real WebView it paints (the screenshot is TCC-gated on macOS).
    if platform.system() == "Darwin":
        return
    tauri.show_window()
    png, w, h = tauri.screenshot(scale=1)
    assert png[:8] == PNG_MAGIC and w > 0 and h > 0


def test_new_shed_dialog_opens(tauri):
    # The New-Shed dialog opens (ui.show_create → the frontend reports modal=="create")
    # and paints on the real WebView. The create LOGIC is covered by
    # test_shared.py::test_create_streams_to_complete — the same shed-core create path
    # the dialog's create_start command drives.
    tauri.wait_until(lambda: tauri.current_pane() is not None, timeout=15, what="frontend ready")
    tauri.show_create()
    tauri.wait_until(lambda: tauri.modal() == "create", timeout=15, what="new-shed dialog open")
    if platform.system() == "Darwin":
        return
    tauri.show_window()
    png, w, h = tauri.screenshot(scale=1)
    assert png[:8] == PNG_MAGIC and w > 0 and h > 0


def test_navigate_egress_pane(tauri):
    # The Egress pane (mac parity) is navigable end-to-end: the ipc.rs allowlist,
    # the bridge PANES set, and the NAV entry all know it — and an unknown pane is
    # still rejected (the allowlist didn't just open up).
    tauri.wait_until(lambda: tauri.current_pane() is not None, timeout=15, what="frontend ready")
    tauri.navigate("egress")
    tauri.wait_until(lambda: tauri.current_pane() == "egress", timeout=15, what="pane=egress")
    with pytest.raises(ShedError) as e:
        tauri.navigate("bogus")
    assert e.value.code == "bad_request"


def test_egress_profiles_render(tauri):
    # The Profiles sub-tab renders the mock's fixture as UI truth (egress.profiles
    # reads the pane's REPORTED rows, like agents.dump): both profiles appear with
    # host + source, the auto-selected first profile's detail shows its allow/deny
    # rules, and selection is drivable by name (egress.show).
    #
    # NOTE: this module's shared session instance points every configured server at
    # the single in-process mock, so the down-host error row is exercised e2e in the
    # DEDICATED module test_tauri_downhost.py (its own instance + the down-host
    # fixture + the SHED_TAURI_MOCK_UNREACHABLE_HOSTS override); the shed-app unit
    # level pins it too (backend.rs::egress_profiles_keeps_error_row_for_down_host).
    tauri.wait_until(lambda: tauri.current_pane() is not None, timeout=15, what="frontend ready")
    tauri.navigate("egress")
    tauri.wait_until(lambda: tauri.current_pane() == "egress", timeout=15, what="pane=egress")
    tauri.egress_show(tab="profiles")
    tauri.wait_until(lambda: (tauri.egress_dump() or {}).get("tab") == "profiles",
                     timeout=15, what="profiles sub-tab shown")
    tauri.wait_until(lambda: len((tauri.egress_dump() or {}).get("profiles") or []) == 2,
                     timeout=15, what="egress profile rows rendered")
    d = tauri.egress_dump()
    rows = {(p["host"], p["name"], p["source"]) for p in d["profiles"]}
    assert rows == {("mock", "default", "config"), ("mock", "custom", "user")}, f"unexpected rows: {d}"
    assert d["errors"] == []
    # list→detail: the first profile auto-selects; its rule lists render.
    sel = d["selected"]
    assert sel["name"] == "default" and sel["source"] == "config"
    assert sel["allow"] == ["*.github.com"]
    assert sel["deny"] == ["evil.example.com"]
    assert sel["mode"] == "audit"
    # selection is drivable: pick the user profile, the detail follows.
    tauri.egress_show(profile="custom")
    tauri.wait_until(lambda: ((tauri.egress_dump() or {}).get("selected") or {}).get("name") == "custom",
                     timeout=15, what="selected profile=custom")
    sel = tauri.egress_dump()["selected"]
    assert sel["source"] == "user"
    assert sel["allow"] == ["api.example.com"]
    assert sel["deny"] == []
    # host-qualified selection: (host, name) resolves deterministically even when a
    # name would collide across hosts. Single-host fixture, so `mock` must match and
    # a wrong host must NOT (the selection stays put on the non-matching host).
    tauri.egress_show(profile="default", host="mock")
    tauri.wait_until(lambda: ((tauri.egress_dump() or {}).get("selected") or {}).get("name") == "default",
                     timeout=15, what="host-qualified select host=mock profile=default")
    assert tauri.egress_dump()["selected"]["host"] == "mock"
    tauri.egress_show(profile="custom", host="nonexistent-host")
    # No matching (host, name) row → selection is unchanged (still `default`).
    assert tauri.egress_dump()["selected"]["name"] == "default"
    # an unknown sub-tab is rejected, not blindly emitted.
    with pytest.raises(ShedError) as e:
        tauri.egress_show(tab="bogus")
    assert e.value.code == "bad_request"
    # off-pane the dump blanks (UI truth, like agents.dump off the agents pane).
    tauri.navigate("sheds")
    tauri.wait_until(lambda: tauri.current_pane() == "sheds", timeout=15, what="pane=sheds")
    assert tauri.egress_dump() is None


def test_egress_activity_filters_ns(tauri, fake):
    # Mixed-ns injection through the fake host-agent's audit event stream: the
    # Egress pane's Activity sub-tab renders ONLY the ns=="egress" rows, while the
    # merged feed (activity.list — the same data the main Activity pane renders)
    # carries both namespaces.
    tauri.wait_until(lambda: tauri.current_pane() is not None, timeout=15, what="frontend ready")
    fake.emit_event("ssh-agent", "sign", "egress-mix-shed", result="ok")
    fake.emit_event("egress", "connect", "egress-mix-shed", result="denied",
                    detail="evil.example.com:443")
    tauri.wait_until(
        lambda: len([e for e in tauri.activity_list() if e["shed"] == "egress-mix-shed"]) == 2,
        timeout=15, what="both events in the merged feed")
    egress_total = len([e for e in tauri.activity_list() if e.get("ns") == "egress"])
    assert egress_total >= 1
    # ...and strictly fewer than the whole feed (the ssh-agent row exists beside it).
    assert egress_total < len(tauri.activity_list())
    tauri.navigate("egress")
    tauri.wait_until(lambda: (tauri.egress_dump() or {}).get("tab") == "activity",
                     timeout=15, what="egress activity sub-tab reported")
    tauri.wait_until(lambda: (tauri.egress_dump() or {}).get("activity_count") == egress_total,
                     timeout=15, what="only the egress rows rendered")


def test_egress_pane_renders(tauri):
    # Screenshot smoke (the test_system_pane_renders pattern): the Egress pane's
    # Profiles list→detail paints on the real WebView without crashing (the data
    # itself is pinned by test_egress_profiles_render above). TCC-gated on macOS,
    # so Linux/Xvfb is the real gate.
    if platform.system() == "Darwin":
        pytest.skip("tauri screenshot on macOS is Screen-Recording-TCC-gated; Linux/Xvfb is the gate")
    tauri.wait_until(lambda: tauri.current_pane() is not None, timeout=15, what="frontend ready")
    tauri.show_window()
    tauri.navigate("egress")
    tauri.wait_until(lambda: tauri.current_pane() == "egress", timeout=15, what="pane=egress")
    tauri.egress_show(tab="profiles")
    tauri.wait_until(lambda: (tauri.egress_dump() or {}).get("tab") == "profiles",
                     timeout=15, what="profiles sub-tab shown")
    png, w, h = tauri.screenshot(scale=1)
    assert png[:8] == PNG_MAGIC and w > 0 and h > 0


def test_launch_dialog_opens(tauri):
    # The New-session (launch agent) dialog opens (ui.show_launch → the frontend
    # reports modal=="launch") and paints on the real WebView. The launch LOGIC is
    # covered by the shared test_agents suite — the same rc.launch path the dialog's
    # rcLaunch command drives. Mirrors test_new_shed_dialog_opens.
    tauri.wait_until(lambda: tauri.current_pane() is not None, timeout=15, what="frontend ready")
    tauri.show_launch()
    tauri.wait_until(lambda: tauri.modal() == "launch", timeout=15, what="launch dialog open")
    if platform.system() == "Darwin":
        return
    tauri.show_window()
    png, w, h = tauri.screenshot(scale=1)
    assert png[:8] == PNG_MAGIC and w > 0 and h > 0
