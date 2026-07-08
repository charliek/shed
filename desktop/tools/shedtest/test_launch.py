"""Issue #4: the dashboard is reachable beyond the menu-bar icon.

The hermetic harness can't exercise the real launch-context detection: the
launch-time auto-open (in applicationDidFinishLaunching, keyed off the
kAEOpenApplication Apple Event) is gated off under test mode so the suite keeps
its hidden-start / accessory policy. So this file pins what IS observable in
test mode: the `ui.window_state` op and the invariant that showing the window
(the path both an active launch and a reopen funnel through) puts the
dashboard on screen without flipping the hermetic app to a Dock app.

The genuine user-launch-vs-login-launch detection is covered by the
LaunchClassifier unit test and, on the real (non-test) launch path, by
scripts/smoke-launch-window.sh.
"""

from __future__ import annotations

from _marks import mac_only

pytestmark = mac_only


def test_window_state_shape(shed):
    st = shed.window_state()
    assert isinstance(st["visible"], bool)
    assert st["activation_policy"] in ("accessory", "regular")


def test_show_then_hide_window(shed):
    # showWindow() is the shared target of both the active-launch auto-open and
    # the applicationShouldHandleReopen escape hatch — after it the dashboard
    # is on screen; hideWindow() (a user closing the window) takes it back off.
    shed.show_window()
    st = shed.window_state()
    assert st["visible"] is True
    # Under the harness showWindow deliberately does NOT raise the app to a
    # regular (Dock) app, so the hermetic instance stays an accessory.
    assert st["activation_policy"] == "accessory"

    shed.hide_window()
    assert shed.window_state()["visible"] is False

    # And it can be reopened afterwards (the window object survives the close).
    shed.show_window()
    assert shed.window_state()["visible"] is True
