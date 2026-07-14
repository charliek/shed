# First Run

This page walks the macOS app's first launch; the Linux Tauri client presents the same panes
(the tray is a native menu — see [Architecture](architecture.md)).

Launch `ShedDesktop.app`. It starts in the **menu bar only** (no Dock icon, no window) —
look for the shippingbox icon. Open the dashboard from its dropdown. While a window is open
the app becomes a regular app (Dock icon + ⌘-Tab + the top-left app menu); close the window
and it returns to menu-bar-only, the icon staying put.

## The dashboard

The sidebar selects a pane:

- **Sheds** — every shed across all configured hosts, grouped by host, with status, image
  (the ref/alias label plus a short `sha256:` digest, matching `shed list -vv`), backend,
  resource sizing, and uptime; per-shed start/stop/reset/delete + open terminal. The **New
  shed** sheet picks a host, a source (`owner/repo` or a local dir), an image (a dropdown of
  the server's configured aliases, defaulting to its `default_image`) + backend, and
  CPU/memory, then streams creation progress live (SSE). The list is kept current by polling
  each `shed-server`.
- **Approvals** — pending credential-approval requests from `shed-host-agent`, with
  Approve / Deny (optionally Touch ID) and "always allow". Empty unless the host agent is
  configured to delegate — see [Credential approvals](approvals.md).
- **Agents** — remote-control (Claude) sessions per shed, with state pills and "Open in
  Claude" for ready ones.
- **Activity** — the merged audit feed (host-agent credentials + the app's decisions), with
  a "Reveal log" button.
- **Egress** — per-host egress profiles (allow/deny rules, by source) plus an activity
  sub-tab filtered to the `egress` namespace.
- **System** — per-host disk usage (images / sheds / snapshots / orphans).
- **Hosts** (sidebar footer) — each configured server with a reachability dot.

## The menu bar

The status item shows the running-shed count and a pending-approval badge. Its dropdown
lists pending approvals (inline Approve/Deny) and running sheds, plus **Open dashboard**,
**Preferences…**, **Check for Updates…** (Sparkle), and **Quit**.

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
