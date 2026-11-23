# shed-desktop

A native desktop application that ties the [shed](https://github.com/charliek/shed)
toolchain into one resident control surface: list and create sheds across hosts, see and
open the agent sessions running in them (read-only roost rows, with craze lanes), approve
credential requests, and watch a live activity feed. It is a **Tauri** app on a shared Rust
core, shipped for **macOS** (DMG + Sparkle auto-update) and **Linux**
(`apt install shed-desktop`).

It is a coordinator — it runs no sheds. The credential
broker runs inside the app, so credentials are answered only while the app is running. It observes and drives
components that already exist on the developer's Mac and on shed hosts (see
[Architecture](architecture.md) for the full picture):

- **shed lifecycle** — HTTP to one or more `shed-server` instances, discovered from
  `~/.shed/config.yaml`; live create-progress over SSE.
- **credentials / approvals** — the credential broker, which runs in the app (the headline
  feature; see [Credential approvals](approvals.md)).
- **provisioning** — the app is a person's credential source: it answers a shed's delivery request from `~/.config/shed/delivery.toml` and shows each shed's delivery state (see [Provisioning](provisioning.md)).
- **agents + terminals** — each host's [roost](../extensions/roost-session-hosts.md)
  sessions are rows in the Agents pane, with live [craze lanes](agent-lanes.md); the user's terminal app is launched for interactive attach.

The app on macOS and on Linux is one thin Tauri shell over a shared **Rust core**
(`shed-core`, `shed-lane`, `shed-roost`, `shed-app`, `shed-desktop-core`); see [Rust core](rust-core.md).

## Status

Shipping. The dashboard, lifecycle/create, the Agents pane (read-only roost rows and craze
lanes), the credential-approval gate, the System (disk) pane, and Sparkle auto-update are all
implemented on both macOS and Linux (the Egress pane included). Since the [monorepo
consolidation](https://github.com/charliek/shed) the app moved onto shed's shared `vX.Y.Z`
release line — a `git tag` cuts the macOS DMG and the Linux `.deb` together when the desktop
component ships.

| Area | What | State |
|---|---|---|
| Dashboard + IPC spine | Read-only dashboard across hosts; the drivability socket + screenshots | ✅ |
| Lifecycle + create | start/stop/reset/delete, create with live SSE progress, terminal launch | ✅ |
| Agents | Agents pane: read-only roost rows grouped by shed, craze lanes, New craze session | ✅ |
| Provisioning | Preferences → Provisioning (five providers, `delivery.toml`, preview); delivery state and **Deliver again** on each shed | ✅ |
| Approval gate | Multi-server SSH approval over UDS, policy engine, notifications, merged audit feed | ✅ |
| System | Per-host disk usage (`/api/system/df`) | ✅ |
| Packaging | Launch-at-login, preferences, DMG + Sparkle EdDSA auto-update (mac); nfpm `.deb` (Linux) | ✅ |

## Design principles

- **Native + small.** A Tauri shell on both macOS and Linux, over one shared Rust core;
  launches instantly; on macOS a menu-bar app with no Dock icon until a window is open.
- **Drivable + testable.** The app exposes a JSON IPC control socket that reports what the UI
  rendered, plus a screenshot op, so every change is verified by a no-sleep functional
  harness — not by a human clicking. See [Test automation](test-automation.md).
- **Fail-closed security.** Credential approval fails closed: a missing or unresponsive
  app results in denial, matching an unanswered local prompt. The broker holds the
  secrets in-process (see [Architecture](architecture.md#the-credential-broker)).

## Roadmap & directions

Directions we may take, not a schedule. Today's app is a complete control surface on both
platforms — dashboard + lifecycle, the Agents pane, the SSH-credential approval
gate, the System (disk) pane, plus notarized Sparkle auto-update on macOS and an
`apt`-installable `.deb` on Linux.

**Shared Rust core & multi-client (delivered).** The shed-server protocol layer was extracted
into a shared **Rust core** (`shed-core`) so the same logic backs every client instead of
being re-implemented per language. The **Tauri** desktop client on it ships on both macOS
(WKWebView) and Linux (WebKitGTK), and brokers credentials **in-process** — see [Installation → Credential
broker](installation.md#credential-broker) and [Architecture → The credential
broker](architecture.md#the-credential-broker). See [Rust core](rust-core.md).

**Credentials.**

- **Gate AWS + Docker, not just SSH.** The broker already streams an all-namespace audit
  feed and the approval protocol is namespace-agnostic; only `ssh-agent` is *gated* by default.
  Extending the gate to `aws-credentials` and `docker-credentials` is mostly broker-side
  wiring, behind a clean policy story so frequent STS refreshes don't become prompt fatigue.
  See [Credential approvals](approvals.md).
- **Auto-approve with constraints** — e.g. docker limited to a registry allowlist.
- **Approvals on Linux — done.** The approval spine lives in the shared Rust core, so the
  app shows + gates SSH approvals on Linux (polkit) and macOS (Touch ID).

**Broader control surface.** The shed-server HTTP API exposes more than the app surfaces
today. Natural additions, each independently useful: a **global sessions** view
(`/api/sessions`, merged across hosts), **snapshot** management (`/api/snapshots`), **image**
management (`/api/images`), **system prune** (`/api/system/prune`), and a **port-forwarding**
UI on top of `/api/sheds/{name}/connect/{port}`.

**Distribution.** A Developer-ID-signed, notarized DMG with an EdDSA-signed Sparkle appcast
on macOS; the Linux client ships as the `shed-desktop` nfpm `.deb` (built from the Tauri
client — `tauri/src-tauri`, bin `shed-desktop-tauri` → `/usr/bin/shed-desktop`) per-arch
(amd64 + arm64) via `charliek/apt-charliek`, so end users `apt install shed-desktop`. See
[Installation](installation.md).

**Larger bets.** A **mobile client** (Android-first) on the same core; an **embedded
terminal** (revisited only if delegating to the user's terminal app proves insufficient);
**in-app host management** (writing `~/.shed/config.yaml` instead of read-only reflection). Have an idea? Open an issue.
