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
**Open Dashboard**, **Preferences** and **Quit** (Tauri emits no tray click
events on Linux, so there is no popover).

## The dashboard

The sidebar has five entries, **Sheds**, **Agents**, **Credentials**, **Activity** and **System**, and a **Shed servers** section that lists each configured server with a reachability dot:

- **Sheds** — every shed across all configured hosts, grouped by host, with status, image
  (the ref/alias label plus a short `sha256:` digest, matching `shed list -vv`), backend,
  resource sizing, and uptime; per-shed start/stop/reset/delete + open terminal. The **New
  shed** sheet picks a host, a source (`owner/repo` or a local dir), an image (a dropdown of
  the server's configured aliases, defaulting to its `default_image`) + backend, and
  CPU/memory, then streams creation progress live (SSE). The list is kept current by polling
  each `shed-server`.
- **Agents** — agent sessions on each shed: read-only roost rows with activity,
  and craze sessions with a live transcript, approvals and **New craze session**.
  See [Agent lanes](agent-lanes.md).
- **Credentials** — who answers for you on each server, the five providers and their scope, a check of what a shed would get, and the answers this machine gave. See [Credentials](credentials.md).
- **Activity** — each configured server's own activity feed (`GET /api/activity`), merged
  newest first across servers, with a **Load older** button. You see only your own sheds'
  events, including for a deleted shed whose name someone else has since reused.
- **System** — per-host disk usage (images / sheds / snapshots / orphans) and each server's
  egress profiles (allow/deny rules, by source).

A shed's own detail shows its delivery state and **Deliver again**, and its egress profile
with its recent blocks (the last 50 per shed, kept in memory), keyed by the shed's creation
time so a reused name starts empty.

## The menu bar

**macOS.** The status item shows the running-shed count. Its
popover lists running sheds, plus **Open
dashboard**, **Preferences…**, **Check for Updates…** (Sparkle), and **Quit**.

**Linux.** The tray icon opens a native menu: **Open Dashboard**,
**Preferences** and **Quit**. There is no popover and no Sparkle item; the `.deb` updates
through apt.

## Preferences

**Preferences…** (in the menu) covers launch-at-login and the terminal. Credentials (which machine answers on
each server, the five providers and their `only` rules, and a check of what a shed would get)
have their own sidebar entry; see [Credentials](credentials.md). The full set of
settings — and the environment-variable overrides — is in
[Configuration](configuration.md).

## Credentials need the app running

The app is your credential source: it delivers credentials into your sheds, at create and
on `shed provision`, only while it is running. So at its first launch it turns on **Launch at login**,
once, and runs whenever you are logged in. Switch it off in **Preferences › General** if you
do not want that; the app never turns it on again.

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
