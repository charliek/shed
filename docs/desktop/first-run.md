# First Run

This page walks the first launch of the Tauri app, which is the client on both macOS and
Linux and presents the same panes (on Linux the tray is a native menu — see
[Architecture](architecture.md)).

**macOS:** launch `ShedDesktop.app`. It starts in the **menu bar only** (no Dock icon, no
window) — look for the shippingbox icon. Open the dashboard from its popover. While a window
is open the app becomes a regular app (Dock icon + ⌘-Tab + the top-left app menu); close the
window and it returns to menu-bar-only, the icon staying put.

**Linux:** launch `shed-desktop` (installed at `/usr/bin/shed-desktop`) from your launcher
or a terminal. It lives in the system tray; clicking the tray icon opens a native menu with
**Open Dashboard**, **Approvals**, **Preferences** and **Quit** (Tauri emits no tray click
events on Linux, so there is no popover).

## The dashboard

The sidebar selects a pane:

- **Sheds** — every shed across all configured hosts, grouped by host, with status, image
  (the ref/alias label plus a short `sha256:` digest, matching `shed list -vv`), backend,
  resource sizing, and uptime; per-shed start/stop/reset/delete + open terminal. The **New
  shed** sheet picks a host, a source (`owner/repo` or a local dir), an image (a dropdown of
  the server's configured aliases, defaulting to its `default_image`) + backend, and
  CPU/memory, then streams creation progress live (SSE). The list is kept current by polling
  each `shed-server`.
- **Approvals** — pending credential-approval requests, with Approve / Deny (optionally
  Touch ID on macOS) and "always allow". They come from the app's own embedded broker by
  default, or from a separate `shed-host-agent` configured to delegate — see
  [Credential approvals](approvals.md).
- **Agents** — the agent sessions on each shed and machine, as [roost](../extensions/roost-session-hosts.md)
  rows with activity pills, plus craze and opencode sessions with a live transcript,
  approvals and **New craze session**. See [Agent lanes](agent-lanes.md).
- **Activity** — the merged audit feed (host-agent credentials + the app's decisions), with
  a "Reveal log" button.
- **Egress** — per-host egress profiles (allow/deny rules, by source) plus an activity
  sub-tab filtered to the `egress` namespace.
- **System** — per-host disk usage (images / sheds / snapshots / orphans).
- **Hosts** (sidebar footer) — each configured server with a reachability dot.

## The menu bar

**macOS.** The status item shows the running-shed count and a pending-approval badge. Its
popover lists pending approvals (inline Approve/Deny) and running sheds, plus **Open
dashboard**, **Preferences…**, **Check for Updates…** (Sparkle), and **Quit**.

**Linux.** The tray icon opens a native menu: **Open Dashboard**, **Approvals**,
**Preferences** and **Quit**. There is no popover and no Sparkle item; the `.deb` updates
through apt.

## Preferences

**Preferences…** (in the menu) covers launch-at-login, the terminal command template, and
the approval policy (default mode + per-namespace / per-shed overrides). The full set of
settings — and the environment-variable overrides — is in
[Configuration](configuration.md).

## Configuration

The host list comes from `~/.shed/config.yaml` (read-only — manage hosts with the `shed`
CLI):

```yaml
servers:
    my-server:
        host: localhost
        http_port: 8080
        ssh_port: 2222
default_server: my-server
```
