"""Thin JSON-IPC client for a running shed-desktop (Tauri) app.

The app speaks newline-delimited JSON over a Unix socket — the same contract
`shedctl` uses. The wire (connect / `_readline` / `call` / `identify` /
`wait_until`) and the core ops (sheds.list / sheds.refresh / lifecycle / create)
live on the `IPCClient` base; `TauriClient` adds the rest of the op surface,
with the `_RcOps` mixin for the Agents ops. Request ids are
string-wrapped int64 on the wire, ints here.
"""

from __future__ import annotations

import base64
import json
import os
import socket
import time

# Scale every wait from one knob so a slower CI runner buys headroom without
# editing each call site.
_TIMEOUT_SCALE = float(os.environ.get("SHED_TAURI_TEST_TIMEOUT_SCALE", "1.0"))


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
    """The newline-JSON wire + the core shed ops."""

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

    # -- core shed ops ----------------------------------------------------
    def identify(self) -> dict:
        return self.call("identify")

    def sheds_list(self, host: str | None = None) -> list[dict]:
        params = {"host": host} if host else {}
        return self.call("sheds.list", params)["sheds"]

    def sheds_host_errors(self, host: str | None = None) -> list[dict]:
        """The per-host failures `sheds.list` carries beside the rows
        (`[{server, kind, summary, detail}]`) — a host that couldn't be listed is
        surfaced, not silently dropped (shed#300)."""
        params = {"host": host} if host else {}
        return self.call("sheds.list", params).get("host_errors", [])

    def sheds_refresh(self) -> None:
        """Re-fetch + re-render the dashboard so the UI-truth op reflects it."""
        self.call("sheds.refresh")

    def dashboard_rows(self) -> list[dict]:
        """The sheds the UI currently shows (its rendered state, from
        `dashboard.dump`), normalized to `[{name, status, host}]`."""
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


class _RcOps:
    """The Agents op surface — list and inject_test.

    A shed's agent sessions are the tabs its own `roost-session` reports, so
    `rc.list` returns that and only that, and `rc.inject_test` puts a row into
    the roost snapshot. Launch, kill and the machines pane are gone: those ops
    answer `unknown_op`. `self.call` comes from the `IPCClient` base each mixes
    in with.

    The pure pane-classifier op and its `rc_classify` helper went with S2
    (charliek/shed#324): a row's `state` comes off the wire, not from a pane."""

    def rc_list(self, host: str | None = None, shed: str | None = None) -> list[dict]:
        params: dict = {}
        if host:
            params["host"] = host
        if shed:
            params["shed"] = shed
        return self.call("rc.list", params)["sessions"]

    def rc_inject_test(self, target: str, slug: str, *,
                       kind: str = "claude-rc", display_name: str | None = None,
                       workdir: str | None = None, lifecycle: str | None = None,
                       attention: bool | None = None) -> None:
        """Put a row into the shed's ROOST snapshot — test mode only.

        `target` is the shed target (`roost:<server>/<shed>`). `slug` is the
        roost tab id, so it must parse as one: a row with an invented slug is
        refused. `lifecycle` is roost's own `agent_lifecycle` word (`working`,
        `waiting`, `finished`, …)."""
        params: dict = {"target": target, "slug": slug, "kind": kind}
        for k, v in (("display_name", display_name),
                     ("workdir", workdir), ("lifecycle", lifecycle),
                     ("attention", attention)):
            if v is not None:
                params[k] = v
        self.call("rc.inject_test", params)


class TauriClient(_RcOps, IPCClient):
    """The Tauri client's op surface: the `IPCClient` core ops + the RC/Agents ops
    (`_RcOps`, B2) + the UI, source, delivery, activity and egress ops."""

    def screenshot(self, scale: int = 1) -> tuple[bytes, int, int]:
        r = self.call("app.screenshot", {"scale": scale})
        return base64.b64decode(r["png"]), r["width"], r["height"]

    def navigate(self, pane: str) -> None:
        self.call("ui.navigate", {"pane": pane})

    def show_window(self) -> None:
        self.call("ui.show_window")

    def show_preferences(self) -> None:
        """Open/focus the dedicated Preferences window (a singleton: created lazily
        on the first open, fronted if already open; closing it hides it). Does NOT
        raise the dashboard. `ui.open_preferences` is an equivalent alias op."""
        self.call("ui.show_preferences")

    def open_preferences(self) -> None:
        """The `ui.open_preferences` alias for show_preferences."""
        self.call("ui.open_preferences")

    def prefs_dump(self) -> dict:
        """The Preferences window's drivable state: `{visible, title, prefs}`.
        `visible`/`title` are Rust-side native window truth (visible False and
        prefs None before the first open); `prefs` is the window's reported
        snapshot `{sections, values, mode}`."""
        return self.call("prefs.dump")

    def prefs_close(self) -> None:
        """Close (hide) the Preferences window — the close-hides singleton contract."""
        self.call("prefs.close")

    def show_create(self) -> None:
        """Open the New-Shed dialog (raises the window + emits the event)."""
        self.call("ui.show_create")

    def agents_dump(self) -> list[dict]:
        """The sessions the Agents pane rendered — the drivable `agents.dump`
        UI truth (empty unless the UI is on the agents pane)."""
        return self.call("agents.dump")["sessions"]

    def agents_empty(self) -> dict | None:
        """The Agents pane's rendered EMPTY STATE (`{state, title, body, action}`),
        or None when it rendered rows / another pane is up.

        The other half of `agents_dump`: since S6 an empty list is a meaningful
        answer ("this shed has no roost-session"), and `sessions == []` alone
        cannot tell that from a pane that has not looked. `state` is which of the
        four blanks this is — `loading` / `failed` / `unreachable` / `empty` —
        because those wear the same screen and only the last one may offer the
        bootstrap."""
        return self.call("agents.dump").get("empty")

    def dashboard_dump(self) -> dict:
        """The Sheds pane's full UI truth: `{rows, host_errors, empty}` — the shed
        rows, the per-host failures the shell knows about, and the rendered empty
        state (`{title, body}`, null when the list rendered or another pane is up).

        `host_errors` is DATA the shell carries, not a rendered strip: the Sheds
        pane deliberately renders no error rows (the sidebar's SHED SERVERS
        section owns "is that box up" — see `sidebar_dump`). It still drives the
        sidebar tooltips and the empty state's copy."""
        return self.call("dashboard.dump")

    def sidebar_dump(self) -> dict:
        """The sidebar as rendered: its nav `entries` (`{pane, label}`, in order) and its
        status foot `servers`. Answers from any
        pane (the sidebar is always mounted), which is why an unreachable server
        is guaranteed to be visible."""
        return self.call("sidebar.dump")

    def activate(self) -> None:
        self.call("app.activate")

    # -- credential sources (plan 035 §3.4) -------------------------------
    def source_status(self) -> dict:
        """`{rows, this_machine, this_machine_error}`: one row per
        configured server — `{host, supported, source: {id, name}|None, this_machine,
        connected, error}` — and this machine's `{id, name}` (or why there is none). A
        server older than the feature is `supported: False` with no error."""
        return self.call("source.status")

    def source_use(self, host: str) -> dict:
        """Make THIS machine the credential source on `host` (one request, never
        retried) and reopen its delivery stream there; `{row, woke_source}`. Raises `ShedError` with the refusal: `bad_request`
        (unknown host), `machine_identity_unavailable`, `source_machine_not_found`,
        `forbidden`, `unsupported`, `action_failed`."""
        return self.call("source.use", {"host": host})

    def sources_list(self) -> dict:
        """`{servers, this_machine, this_machine_error}`: one row per configured server
        (by name) — `{server, kind, machine, connected, url, state, stream, busy,
        unreachable, error}`, `kind` one of `this_machine`, `other_machine`, `service`,
        `nobody`, `unsupported`, `unknown` (plan 040 §3.9)."""
        return self.call("sources.list")

    def source_unbind(self, host: str) -> dict:
        """Clear this user's credential source on `host` ("Stop answering from here");
        `{server, unbound, previous, previous_was_this_machine, forgotten_machines}`.
        Raises `ShedError`: `bad_request`, `source_is_service`, `unsupported`,
        `forbidden`, `action_failed`."""
        return self.call("source.unbind", {"host": host})

    def press_source_use(self, host: str) -> None:
        """Press the Credentials view's own "Answer from here" button for `host` (the
        `button[data-source-use="<host>"]` it renders) — test mode only. A press on a
        disabled or missing button lands nowhere, as a pointer's would."""
        self.call("ui.press_source_use", {"host": host})

    def press_source_unbind(self, host: str) -> None:
        """Press the Credentials view's own "Stop answering from here" button for `host`
        (`button[data-source-unbind="<host>"]`) — test mode only."""
        self.call("ui.press_source_unbind", {"host": host})

    def credentials_dump(self) -> dict | None:
        """What the Credentials view rendered (plan 040 §3.9): `{where, what, check,
        recent, rendered}`, read off its own nodes; None off the view."""
        return self.call("credentials.dump").get("credentials")

    def hosts_auth(self) -> list[dict]:
        """Per-server credential mode (plan 002 C3): `[{name, auth_mode, learned}]`,
        name-sorted. `auth_mode` is `token`|`mtls`; `learned` is False while the value
        is only the config entry's cached claim and True once a mint in THIS session
        produced that shape. Nothing here is persisted — the app never writes
        config.yaml."""
        return self.call("hosts.auth")["hosts"]

    def current_pane(self) -> str | None:
        """The pane the React shell currently renders (reported via ui_report)."""
        return self.call("ui.current_pane").get("pane")

    def modal(self) -> str | None:
        """Which modal (if any) the frontend has open: 'create' | 'craze' | None.
        (Preferences is a dedicated window, not a modal — see prefs_dump.)"""
        return self.call("ui.modal").get("modal")

    def focus(self) -> dict | None:
        """Where keyboard focus was after a dialog last opened or closed:
        `{dialog, active}` — the top dialog's title (None with none open) and the
        focused control `{tag, text, title, marks, shed, in_dialog}` (None on the
        page itself) — or None before any dialog opened."""
        return self.call("ui.focus").get("focus")

    def computed_style(self) -> dict | None:
        """A computed-style sample the frontend reported (body bg/color + accent +
        the active light/dark mode), so a test can confirm the WebView applied the
        theme and observe the appearance op."""
        return self.call("ui.computed_style").get("style")

    def set_appearance(self, mode: str) -> None:
        """Drive the dashboard's light/dark mode (`ui.set_appearance` → the shell's
        `set-appearance` listener), so dark screenshots are deterministic."""
        self.call("ui.set_appearance", {"mode": mode})

    def badges(self) -> dict | None:
        """The sidebar nav badge counts the shell reported ({sheds, agents, hosts,
        pending}), or None before its first report — UI truth, like agents_dump."""
        return self.call("ui.badges").get("badges")

    def report_history(self, clear: bool = False) -> dict:
        """What EVERY one of the dashboard's full reports carried, oldest first:
        `{reports: [{hosts, rows, servers}], dropped}` — the Hosts badge, the
        dashboard's row count and the sidebar's server count per report, and how
        many the app's bounded ring evicted since the last clear (nonzero means
        the list no longer starts at that clear). `clear=True` empties the ring
        after this read. Test mode only (`ui.report_history`, shed#400): a
        sampled `badges()` read can fall between two reports and miss one."""
        return self.call("ui.report_history", {"clear": clear})

    def system_df(self) -> list[dict]:
        """Per-host disk usage (`[HostDiskUsage]`); each row has host/usage/error."""
        return self.call("system.df")["usage"]

    def egress_dump(self) -> dict | None:
        """The System pane's rendered egress profiles (`egress.profiles` UI truth,
        like agents_dump): `{profiles, errors}` — or None unless the UI is on the
        System pane and it has reported."""
        return self.call("egress.profiles").get("egress")

    def egress_show(self, server: str, shed: str) -> None:
        """Open `shed`'s detail on the Sheds pane (its delivery and its egress): the
        `egress.show` op, an alias of `ui.show_shed_delivery`."""
        self.call("egress.show", {"server": server, "shed": shed})

    def shed_egress_dump(self) -> dict | None:
        """What the open shed detail's egress section rendered (`shed_egress.dump`):
        `{server, shed, created_at, profiles, blocks, note, error, rendered}`, or None
        when no detail is open."""
        return self.call("shed_egress.dump").get("detail")

    def egress_recent(self, server: str, shed: str, created_at: str,
                      blocks: bool = False) -> list[dict]:
        """A shed incarnation's kept egress decisions, newest first (`egress.recent`)."""
        return self.call("egress.recent", {"server": server, "shed": shed,
                                           "created_at": created_at, "blocks": blocks})["events"]

    def egress_status(self) -> list[dict]:
        """Each server's egress stream consumer (`egress.status`)."""
        return self.call("egress.status")["servers"]

    def egress_server_profiles(self) -> list[dict]:
        """Each server's egress profiles, or its error (`egress.server_profiles`)."""
        return self.call("egress.server_profiles")["hosts"]

    def activity_server_list(self, limit: int | None = None, cursor: str | None = None,
                             shed: str | None = None) -> dict:
        """One merged page of every server's activity (`activity.server_list`):
        `{events, next, errors}`."""
        params: dict = {}
        if limit is not None:
            params["limit"] = limit
        if cursor is not None:
            params["cursor"] = cursor
        if shed is not None:
            params["shed"] = shed
        return self.call("activity.server_list", params)

    def activity_dump(self) -> dict | None:
        """What the Activity pane rendered (`activity.dump`), or None off the pane."""
        return self.call("activity.dump").get("activity")

    def press_activity_older(self) -> None:
        """Press the Activity pane's own "Load older" (test mode only)."""
        self.call("ui.press_activity_older")

    def terminal_preview(self, shed: str | None = None, host: str | None = None,
                         session: str | None = None, preset: str | None = None,
                         template: str | None = None, machine: str | None = None) -> dict:
        """The ssh command + resolved preset/invocation that would open the shed —
        no spawn (param key `shed`).

        `machine` is the roost row's ask, and it is REFUSED server-side (plan 013:
        a machine row's attach is `native-remote`, so there is no tmux to attach)."""
        return self.call("terminal.preview",
                         self._terminal_params(shed, host, session, preset, template, machine))

    def terminal_open(self, shed: str | None = None, host: str | None = None,
                      session: str | None = None, preset: str | None = None,
                      template: str | None = None, machine: str | None = None) -> dict:
        """Spawn the terminal opener (disabled under test mode → `not_enabled`;
        a `machine` is refused in every mode — see `terminal_preview`)."""
        return self.call("terminal.open",
                         self._terminal_params(shed, host, session, preset, template, machine))

    def terminal_presets(self) -> list[dict]:
        """The offerable terminal presets + whether each is installed."""
        return self.call("terminal.presets")["presets"]

    @staticmethod
    def _terminal_params(shed, host, session, preset, template, machine=None) -> dict:
        params: dict = {}
        for k, v in (("shed", shed), ("host", host), ("session", session),
                     ("preset", preset), ("template", template), ("machine", machine)):
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
        """Whether launch-at-login is enabled (the Preferences → General toggle). The
        app turns it on once at its first launch (plan 037 §3.1.4)."""
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

    # -- Sparkle updater (the popover "Check for Updates…" row) -----------
    def updater_status(self) -> dict:
        """The updater's status: `{os, enabled, reason}` (enabled == reason=='ok').
        Platform-truthful: Linux ⇒ reason 'linux_apt'; mac under the harness ⇒
        'test_mode' (the plugin is never registered in test mode); mac unbundled ⇒
        'no_bundle'; a real bundle ⇒ 'ok'."""
        return self.call("updater.status")

    def updater_check(self) -> None:
        """A user-invoked update check. On the enabled path presents Sparkle; when
        disabled raises ShedError('updater_disabled') whose message carries the
        reason ('updater_disabled:<reason>') — never crashes the app."""
        self.call("updater.check")
