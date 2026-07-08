"""Thin JSON-IPC clients for a running shed-desktop UI (the mac app or the Tauri client).

Both UIs speak the same newline-delimited JSON protocol over a Unix socket — the
same contract `shedctl` uses. The wire (connect / `_readline` / `call` /
`identify` / `wait_until`) and the ops the clients share (sheds.list /
sheds.refresh / lifecycle / create) live on the `IPCClient` base, so one test
driver can drive either target. Swift-only ops stay on `ShedDesktop`; the Tauri
client adds the UI-truth op `dashboard.dump`. Request ids are string-wrapped
int64 on the wire, ints here.
"""

from __future__ import annotations

import base64
import json
import os
import socket
import time

# Scale every wait from one knob so a slower CI runner buys headroom without
# editing each call site. Honor both targets' scale knobs (mac + tauri) and take
# the largest, so a run under either CI leg gets its intended headroom.
_TIMEOUT_SCALE = max(
    float(os.environ.get("SHED_DESKTOP_TEST_TIMEOUT_SCALE", "1.0")),
    float(os.environ.get("SHED_TAURI_TEST_TIMEOUT_SCALE", "1.0")),
)


def scaled_timeout(timeout: float) -> float:
    return timeout * _TIMEOUT_SCALE


class ShedError(Exception):
    """A server error envelope (`ok: false`) or a transport failure."""

    def __init__(self, code: str, message: str):
        super().__init__(f"{code}: {message}")
        self.code = code
        self.message = message


class Timeout(ShedError):
    def __init__(self, message: str):
        super().__init__("timeout", message)


class IPCClient:
    """The shared newline-JSON wire + the ops both targets implement."""

    def __init__(self, socket_path: str):
        self.path = str(socket_path)
        self._next_id = 0
        self._buf = b""
        self._sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        # A timeout so a wedged app (accepts but never replies) surfaces as a
        # test failure instead of hanging CI forever.
        self._sock.settimeout(scaled_timeout(15.0))
        self._sock.connect(self.path)

    # -- lifecycle --------------------------------------------------------
    def close(self) -> None:
        try:
            self._sock.close()
        except OSError:
            pass

    def __enter__(self) -> "IPCClient":
        return self

    def __exit__(self, *_exc) -> None:
        self.close()

    # -- transport --------------------------------------------------------
    def call(self, op: str, params: dict | None = None) -> dict:
        self._next_id += 1
        req = {"id": str(self._next_id), "op": op, "params": params or {}}
        self._sock.sendall((json.dumps(req) + "\n").encode())
        resp = json.loads(self._readline())
        if not resp.get("ok"):
            err = resp.get("error") or {}
            raise ShedError(err.get("code", "unknown"), err.get("message", ""))
        return resp.get("result") or {}

    def _readline(self) -> str:
        while b"\n" not in self._buf:
            try:
                chunk = self._sock.recv(1 << 16)
            except socket.timeout as e:
                raise Timeout("no IPC response within socket timeout") from e
            if not chunk:
                raise ShedError("disconnected", "socket closed mid-response")
            self._buf += chunk
        line, self._buf = self._buf.split(b"\n", 1)
        return line.decode()

    # -- ops shared by both targets --------------------------------------
    def identify(self) -> dict:
        return self.call("identify")

    def sheds_list(self, host: str | None = None) -> list[dict]:
        params = {"host": host} if host else {}
        return self.call("sheds.list", params)["sheds"]

    def sheds_refresh(self) -> None:
        """Re-fetch + re-render the dashboard so the UI-truth op reflects it."""
        self.call("sheds.refresh")

    def dashboard_rows(self, target: str) -> list[dict]:
        """The sheds the UI currently shows (its rendered state), normalized to
        `[{name, status, host}]`. The truth op differs per target: the mac app
        exposes it as `ui.state.sheds`; the Tauri client as `dashboard.dump.rows`."""
        if target == "mac":
            rows = self.call("ui.state")["sheds"]
        else:
            rows = self.call("dashboard.dump")["rows"]
        return [{"name": r["name"], "status": r["status"], "host": r["host"]} for r in rows]

    def shed_action(self, action: str, name: str, host: str | None = None) -> None:
        params = {"name": name}
        if host:
            params["host"] = host
        self.call(f"shed.{action}", params)

    def shed_status(self, name: str) -> str | None:
        for s in self.sheds_list():
            if s["name"] == name:
                return s["status"]
        return None

    def create_start(self, name: str, host: str | None = None, **fields) -> str:
        params = {"name": name, **fields}
        if host:
            params["host"] = host
        return self.call("create.start", params)["create_id"]

    def create_status(self, create_id: str) -> dict:
        return self.call("create.status", {"create_id": create_id})

    def create_cancel(self, create_id: str) -> None:
        self.call("create.cancel", {"create_id": create_id})

    # -- waits (poll the op set; no sleeps in tests) ----------------------
    def wait_until(self, pred, timeout: float = 5.0, what: str = "condition") -> None:
        eff = scaled_timeout(timeout)
        deadline = time.monotonic() + eff
        while True:
            try:
                if pred():
                    return
            except ShedError:
                pass
            if time.monotonic() >= deadline:
                raise Timeout(f"timed out after {eff}s waiting for {what}")
            time.sleep(0.1)


class _ApprovalOps:
    """The credential-approval op surface (SSH-approval prefs / policy / approvals
    / activity / notifications), shared by the mac app and the Tauri client — both
    wire the same shed-core approval spine with identical op names + shapes.
    `self.call` comes from the `IPCClient` base each mixes in with."""

    def set_ssh_approval(self, method: str | None = None, policy: str | None = None,
                         ttl: str | None = None) -> None:
        """Set SSH approval prefs (any subset) and reset live SSH grants.

        `policy` is one of: always-allow | per-shed-allow | time-based-allow |
        always-ask | always-deny.
        """
        params: dict = {}
        if method is not None:
            params["method"] = method
        if policy is not None:
            params["policy"] = policy
        if ttl is not None:
            params["ttl"] = ttl
        self.call("ui.set_ssh_approval", params)

    def ssh_prefs_get(self) -> dict:
        """The coordinator's current SSH approval prefs ({method, policy, ttl}) —
        the read side of set_ssh_approval, so a test can assert what a set applied."""
        return self.call("ui.ssh_prefs")

    def approvals_list(self) -> list[dict]:
        return self.call("approvals.list")["approvals"]

    def approval_decide(self, id: str, decision: str, scope: str | None = None,
                        ttl: str | None = None, persist: bool = False) -> None:
        params: dict = {"id": id, "decision": decision, "persist": persist}
        if scope is not None:
            params["scope"] = scope
        if ttl is not None:
            params["ttl"] = ttl
        self.call("approval.decide", params)

    def activity_list(self, limit: int = 200) -> list[dict]:
        return self.call("activity.list", {"limit": limit})["entries"]

    def activity_log_path(self) -> str:
        return self.call("activity.log_path")["path"]

    def policy_set(self, rules: list[dict]) -> None:
        self.call("policy.set", {"rules": rules})

    def policy_list(self) -> list[dict]:
        return self.call("policy.list")["rules"]

    def notifications_list(self) -> list[dict]:
        return self.call("notifications.list")["notifications"]

    def notification_invoke(self, id: str, action: str) -> None:
        self.call("notification.invoke", {"id": id, "action": action})

    def notification_open(self) -> None:
        """Drive a notification-body tap → opens the dashboard on Approvals."""
        self.call("notification.open")


class _RcOps:
    """The remote-control (Agents) op surface — classify / list / launch / kill /
    inject_test — shared by the mac app and the Tauri client (both wire the same
    shed-core/shed-app RC spine with identical op names + shapes). `self.call`
    comes from the `IPCClient` base each mixes in with."""

    def rc_classify(self, kind: str, pane: str) -> dict:
        return self.call("rc.classify", {"kind": kind, "pane": pane})

    def rc_list(self, host: str | None = None, shed: str | None = None) -> list[dict]:
        params: dict = {}
        if host:
            params["host"] = host
        if shed:
            params["shed"] = shed
        return self.call("rc.list", params)["sessions"]

    def rc_launch(self, shed: str, kind: str = "claude-rc", host: str | None = None,
                  display_name: str | None = None, initial_prompt: str | None = None) -> dict:
        params: dict = {"shed": shed, "kind": kind}
        if host:
            params["host"] = host
        if display_name:
            params["display_name"] = display_name
        # Include whenever non-None (not just truthy) so negative tests can send a
        # deliberately bad value (e.g. a control char) through to the op.
        if initial_prompt is not None:
            params["initial_prompt"] = initial_prompt
        return self.call("rc.launch", params)

    def rc_kill(self, shed: str, slug: str, host: str | None = None) -> None:
        params: dict = {"shed": shed, "slug": slug}
        if host:
            params["host"] = host
        self.call("rc.kill", params)

    def rc_inject_test(self, shed: str, slug: str, *, host: str | None = None,
                       kind: str = "claude-broker", state: str = "ready", managed: bool = False,
                       display_name: str | None = None, created_by: str | None = None,
                       created_at: str | None = None, rc_id: str | None = None,
                       url: str | None = None, target_label: str | None = None) -> None:
        """Inject a session (managed or legacy) into the table — test mode only."""
        params: dict = {"shed": shed, "slug": slug, "kind": kind,
                        "state": state, "managed": managed}
        for k, v in (("host", host), ("display_name", display_name),
                     ("created_by", created_by), ("created_at", created_at),
                     ("rc_id", rc_id), ("url", url), ("target_label", target_label)):
            if v is not None:
                params[k] = v
        self.call("rc.inject_test", params)


class ShedDesktop(_ApprovalOps, _RcOps, IPCClient):
    """The macOS app's full op surface (drives the SwiftUI dashboard, the
    approval gate, remote-control agents, prefs, and notifications)."""

    # `sheds.refresh` reads more naturally as `refresh()` at the mac call sites
    # that predate the shared base; keep the alias so those stay untouched.
    def refresh(self) -> None:
        self.sheds_refresh()

    def ui_state(self) -> dict:
        return self.call("ui.state")

    def navigate(self, pane: str) -> dict:
        return self.call("ui.navigate", {"pane": pane})

    def show_window(self) -> None:
        self.call("ui.show_window")

    def hide_window(self) -> None:
        self.call("ui.hide_window")

    def show_create(self) -> None:
        self.call("ui.show_create")

    def show_launch(self) -> None:
        self.call("ui.show_launch")

    def open_menu(self, open_: bool) -> None:
        self.call("ui.open_menu", {"open": open_})

    def open_preferences(self) -> None:
        self.call("ui.open_preferences")

    def host_list(self) -> list[dict]:
        return self.call("host.list")["hosts"]

    def system_df(self) -> list[dict]:
        return self.call("system.df")["usage"]

    def images_list(self) -> list[dict]:
        """Per-host image lists (`[HostImageList]`); each has host/images/error."""
        return self.call("images.list")["images"]

    # -- M1: lifecycle, create, terminal ---------------------------------
    def terminal_preview(self, shed: str, host: str | None = None, session: str | None = None) -> dict:
        params: dict = {"shed": shed}
        if host:
            params["host"] = host
        if session:
            params["session"] = session
        return self.call("terminal.preview", params)

    def window_metrics(self) -> dict:
        return self.call("app.window_metrics")

    def window_state(self) -> dict:
        """Dashboard visibility + activation policy (issue #4 launch/reopen)."""
        return self.call("ui.window_state")

    def screenshot(self, surface: str = "window", scale: int = 1) -> tuple[bytes, int, int]:
        r = self.call("app.screenshot", {"surface": surface, "scale": scale})
        return base64.b64decode(r["png"]), r["width"], r["height"]


class _RustCoreClient(IPCClient):
    """The Rust-core subprocess client base (the Tauri client): a surface-less
    `app.screenshot` (the mac app's takes a `surface` arg instead). The
    sheds/lifecycle/create ops live on `IPCClient`, and the UI-truth
    `dashboard.dump` is issued by `dashboard_rows(target)` there."""

    def screenshot(self, scale: int = 1) -> tuple[bytes, int, int]:
        r = self.call("app.screenshot", {"scale": scale})
        return base64.b64decode(r["png"]), r["width"], r["height"]


class TauriClient(_ApprovalOps, _RcOps, _RustCoreClient):
    """The Tauri client's op surface: the shared Rust-core base + the approval ops
    (`_ApprovalOps`, B3) + the RC/Agents ops (`_RcOps`, B2) + pane `navigate` and
    `show_window`/`activate` (A0a)."""

    def navigate(self, pane: str) -> None:
        self.call("ui.navigate", {"pane": pane})

    def show_window(self) -> None:
        self.call("ui.show_window")

    def show_preferences(self) -> None:
        """Open the in-app Preferences modal (raises the window + emits the event)."""
        self.call("ui.show_preferences")

    def show_create(self) -> None:
        """Open the New-Shed dialog (raises the window + emits the event)."""
        self.call("ui.show_create")

    def agents_dump(self) -> list[dict]:
        """The RC sessions the Agents pane rendered — the drivable `agents.dump`
        UI truth (empty unless the UI is on the agents pane)."""
        return self.call("agents.dump")["sessions"]

    def activate(self) -> None:
        self.call("app.activate")

    def current_pane(self) -> str | None:
        """The pane the React shell currently renders (reported via ui_report)."""
        return self.call("ui.current_pane").get("pane")

    def modal(self) -> str | None:
        """Which modal (if any) the frontend has open: 'prefs' | 'create' | None."""
        return self.call("ui.modal").get("modal")

    def computed_style(self) -> dict | None:
        """A computed-style sample the frontend reported (body bg/color + accent),
        so a test can confirm the WebView applied the theme."""
        return self.call("ui.computed_style").get("style")

    def system_df(self) -> list[dict]:
        """Per-host disk usage (`[HostDiskUsage]`); each row has host/usage/error."""
        return self.call("system.df")["usage"]

    def terminal_preview(self, shed: str, host: str | None = None, session: str | None = None,
                         preset: str | None = None, template: str | None = None) -> dict:
        """The ssh command + resolved preset/invocation that would open the shed —
        no spawn. Same `terminal.preview` contract as the mac app (param key `shed`)."""
        return self.call("terminal.preview", self._terminal_params(shed, host, session, preset, template))

    def terminal_open(self, shed: str, host: str | None = None, session: str | None = None,
                      preset: str | None = None, template: str | None = None) -> dict:
        """Spawn the terminal opener (disabled under test mode → `not_enabled`)."""
        return self.call("terminal.open", self._terminal_params(shed, host, session, preset, template))

    def terminal_presets(self) -> list[dict]:
        """The offerable terminal presets + whether each is installed."""
        return self.call("terminal.presets")["presets"]

    @staticmethod
    def _terminal_params(shed, host, session, preset, template) -> dict:
        params: dict = {"shed": shed}
        for k, v in (("host", host), ("session", session), ("preset", preset), ("template", template)):
            if v is not None:
                params[k] = v
        return params

    def prefs_get(self) -> dict:
        """The persisted prefs (`terminal_preset` + `terminal_template`)."""
        return self.call("prefs.get")

    def prefs_set_terminal(self, preset: str, template: str | None = None) -> None:
        """Persist the terminal preset (+ optional custom template)."""
        params: dict = {"preset": preset}
        if template is not None:
            params["template"] = template
        self.call("prefs.set_terminal", params)

    # -- launch-at-login (B4) --------------------------------------------
    def login_item_status(self) -> bool:
        """Whether launch-at-login is enabled (the Preferences → General toggle)."""
        return self.call("loginitem.status")["enabled"]

    def login_item_set(self, enabled: bool) -> None:
        """Set launch-at-login. Guarded to an in-memory cell under the macOS harness
        (a real LaunchAgent/TCC write isn't hermetic); a real, hermetic `auto-launch`
        `.desktop` write on Linux (the harness redirects HOME + XDG_CONFIG_HOME)."""
        self.call("loginitem.set", {"enabled": enabled})

    # -- menu-bar popover (B1b) ------------------------------------------
    def tray_dump(self) -> dict:
        """The drivable menu-bar/tray state: `{present, items, popover,
        popover_visible}`. `popover` (the popover webview's reported rows) is null
        where there's no popover (Linux) or it hasn't reported yet."""
        return self.call("tray.dump")

    def tray_show(self) -> None:
        """Show the mac popover — the hermetic analog of a tray-icon left-click
        (OS tray clicks aren't drivable, so this runs the same Rust path)."""
        self.call("tray.show")

    def tray_toggle(self) -> None:
        """Toggle the mac popover (hide if visible, else show + anchor)."""
        self.call("tray.toggle")

    def tray_hide(self) -> None:
        """Hide the mac popover."""
        self.call("tray.hide")
