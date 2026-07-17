# shed-desktop

A native macOS menu-bar application that ties the [shed](https://github.com/charliek/shed)
toolchain into one resident control surface: list and create sheds across hosts, launch
Claude remote-control agents, approve credential requests from the shed host agent, and
watch a live activity feed. A **Tauri** cross-platform client on the same shared Rust core
is the shipped **Linux** client (`apt install shed-desktop`).

It is a coordinator — it runs no sheds and holds no credentials. It observes and drives
components that already exist on the developer's Mac and on shed hosts (see
[Architecture](architecture.md) for the full picture):

- **shed lifecycle** — HTTP to one or more `shed-server` instances, discovered from
  `~/.shed/config.yaml`; live create-progress over SSE.
- **credentials / approvals** — a Unix-domain-socket channel to `shed-host-agent`
  (the headline feature; see [Credential approvals](approvals.md)).
- **terminals + remote control** — SSH into a shed and drive `tmux`, launching the user's
  terminal app for interactive attach.

The macOS app and the Linux Tauri app are two thin shells over one shared **Rust core**
(`shed-core` + `shed-app`); see [Rust core](rust-core.md).

## Status

Shipping. The dashboard, lifecycle/create, RC agent launcher, the credential-approval gate,
the System (disk) pane, and Sparkle auto-update are all implemented on macOS, with the Tauri
Linux client at full feature parity (the Egress pane included). Since the [monorepo
consolidation](https://github.com/charliek/shed) the app moved onto shed's shared `vX.Y.Z`
release line — a `git tag` cuts the macOS DMG and the Linux `.deb` together when the desktop
component ships.

| Area | What | State |
|---|---|---|
| Dashboard + IPC spine | Read-only dashboard across hosts; the drivability socket + screenshots | ✅ |
| Lifecycle + create | start/stop/reset/delete, create with live SSE progress, terminal launch | ✅ |
| Agents | Remote-control launcher (RC classifier), Agents pane | ✅ |
| Approval gate | Multi-server SSH approval over UDS, policy engine, notifications, merged audit feed | ✅ |
| System | Per-host disk usage (`/api/system/df`) | ✅ |
| Packaging | Launch-at-login, preferences, DMG + Sparkle EdDSA auto-update (mac); nfpm `.deb` (Linux) | ✅ |

## Design principles

- **Native + small.** SwiftUI menu-bar app on macOS; launches instantly; no Dock icon by
  default. A Tauri/WebKitGTK shell on Linux — both over one Rust core.
- **Drivable + testable.** The app exposes a JSON IPC control socket and an in-process
  screenshot op, so every change is verified by a no-sleep functional harness — not by a
  human clicking. See [Test automation](test-automation.md).
- **Fail-closed security.** The app never holds secrets; a missing or unresponsive app
  results in credential denial, matching the host agent's unanswered-prompt behavior.

## Roadmap & directions

Directions we may take, not a schedule. Today's app is a complete control surface on both
platforms — dashboard + lifecycle, the remote-control launcher, the SSH-credential approval
gate, the System (disk) pane, plus notarized Sparkle auto-update on macOS and an
`apt`-installable `.deb` on Linux.

**Shared Rust core & multi-client (delivered).** The shed-server protocol layer was extracted
into a shared **Rust core** (`shed-core`) so the same logic backs every client instead of
being re-implemented per language. It is the macOS **default** backend (behind
`SHED_DESKTOP_RUST_CORE`, on by default) and the base for the **Tauri** cross-platform
client, which is now the **shipped Linux client** (WebKitGTK). An earlier GTK MVP proved the
architecture and has since been **retired** in favor of Tauri. The Swift app still requires
`shed-host-agent` as a **separate** process; the Tauri client can broker credentials
**in-process** instead (default) or against a separate daemon — see [Installation →
Credential broker](installation.md#credential-broker) and [Architecture → The embedded
credential broker](architecture.md#the-embedded-credential-broker-tauri). See [Rust
core](rust-core.md).

**Credentials.**

- **Gate AWS + Docker, not just SSH.** The host agent already streams an all-namespace audit
  feed and the approval protocol is namespace-agnostic; only `ssh-agent` is *gated* today.
  Extending the gate to `aws-credentials` and `docker-credentials` is mostly agent-side
  wiring, behind a clean policy story so frequent STS refreshes don't become prompt fatigue.
  See [Credential approvals](approvals.md).
- **Auto-approve with constraints** — e.g. docker limited to a registry allowlist.
- **Approvals on Linux — done.** The Mac approval spine was ported into the shared Rust core,
  so the Tauri client shows + gates SSH approvals on Linux (polkit) and macOS.

**Broader control surface.** The shed-server HTTP API exposes more than the app surfaces
today. Natural additions, each independently useful: a **global sessions** view
(`/api/sessions` + the RC list, merged), **snapshot** management (`/api/snapshots`), **image**
management (`/api/images`), **system prune** (`/api/system/prune`), and a **port-forwarding**
UI on top of `/api/sheds/{name}/connect/{port}`.

**Distribution.** A Developer-ID-signed, notarized DMG with an EdDSA-signed Sparkle appcast
on macOS; the Linux client ships as the `shed-desktop` nfpm `.deb` (built from the Tauri
client — `tauri/src-tauri`, bin `shed-desktop-tauri` → `/usr/bin/shed-desktop`) per-arch
(amd64 + arm64) via `charliek/apt-charliek`, so end users `apt install shed-desktop`. See
[Installation](installation.md).

**Larger bets.** A **mobile client** (Android-first) on the same core; an **embedded
terminal** (revisited only if delegating to the user's terminal app proves insufficient);
**in-app host management** (writing `~/.shed/config.yaml` instead of read-only reflection);
and, further out, retiring the standalone `shed-host-agent` formula/daemon for desktop
users now that the Tauri client can broker credentials on its own (the headless daemon
stays for server/no-desktop use). Have an idea? Open an issue.
