# Installation

shed-desktop ships two clients from one shared Rust core: a native macOS app (Apple Silicon)
and a Tauri/WebKitGTK Linux app.

## macOS (DMG)

The macOS app is an Apple Silicon (arm64) menu-bar app and requires macOS 14 or newer.

Grab the latest `ShedDesktop-<version>.dmg` from the
[releases page](https://github.com/charliek/shed/releases), open it, and drag
**ShedDesktop.app** to Applications.

Official release DMGs are Developer-ID-signed and notarized when the release
pipeline has signing credentials, and then launch without a Gatekeeper prompt.
(An ad-hoc-signed, non-notarized build ships a `FIRST-LAUNCH.txt` with the
one-time bypass steps.)

### Updates

The app updates through **Sparkle**, served from the appcast at
`https://charliek.github.io/shed/appcast.xml` and verified by an EdDSA signature independent
of Apple notarization. Updates are **user-invoked only** — there are no automatic background
checks. Trigger one from the menu-bar dropdown (or the tray popover) → **Check for Updates…**;
if a newer build is published, Sparkle offers it and applies it in place.

| Behavior | Value |
|---|---|
| Trigger | User-invoked (**Check for Updates…**); no automatic/scheduled checks |
| Feed | `https://charliek.github.io/shed/appcast.xml` (stable channel) |
| Verification | EdDSA signature (`SUPublicEDKey`), independent of Apple notarization |
| Channels | Stable by default; prerelease (rc) builds subscribe to a **beta** channel |

The Tauri macOS app is on a **beta rollout** — prerelease (`vX.Y.Z-rc.N`) tags publish a
beta-channel Tauri DMG while stable users keep receiving the Swift app. Both share the same
feed, EdDSA key, and bundle identity (`ai.stridelabs.ShedDesktop`), so the eventual promotion
is a seamless in-place update. See
[RELEASING.md](https://github.com/charliek/shed/blob/main/desktop/RELEASING.md).

Locally built DMGs (`make -C desktop dmg`) are **ad-hoc signed**, so Gatekeeper blocks the
first launch. Clear the quarantine once, after copying it in:

```bash
xattr -dr com.apple.quarantine /Applications/ShedDesktop.app
```

(Or double-click it, dismiss the warning, then System Settings → Privacy & Security →
"Open Anyway".)

> A Homebrew **cask** for the macOS app is planned (a later monorepo phase) but not
> available yet — install from the DMG for now.

## Linux (apt)

The Linux client is the Tauri app, distributed as the `shed-desktop` `.deb` (amd64 + arm64)
through `charliek/apt-charliek`:

```bash
apt install shed-desktop
```

The binary installs to `/usr/bin/shed-desktop`, with a headless `shedctl` and the polkit
action alongside. The `.deb` declares its WebKitGTK runtime dependencies
(`libwebkit2gtk-4.1-0`, `libgtk-3-0`, `libayatana-appindicator3-1`, `librsvg2-2`,
`libsoup-3.0-0`) and recommends `polkitd`. See the
[shed apt repo](https://github.com/charliek/apt-charliek) for the repository setup.

## Build from source

The desktop app lives under `desktop/` in the [shed monorepo](https://github.com/charliek/shed);
its shared Rust core is the sibling `crates/` workspace. Every `make` target below runs from
the monorepo root via the `desktop-` passthrough (`make desktop-<target>`) or directly with
`make -C desktop <target>`.

**macOS** — prerequisites: Xcode 16+ (Swift 6 toolchain), Rust stable ≥1.85.

```bash
git clone https://github.com/charliek/shed
cd shed
make -C desktop bundle     # builds desktop/build/ShedDesktop.app (ad-hoc signed)
open desktop/build/ShedDesktop.app
```

The bundle embeds the `shedctl` CLI at
`desktop/build/ShedDesktop.app/Contents/Resources/bin/shedctl`. `make -C desktop dmg` packages
a release bundle into `desktop/build/ShedDesktop-<version>.dmg`. To produce a notarizable
build locally, set the signing identity:

```bash
SHED_DESKTOP_DEVELOPER_ID_IDENTITY="Developer ID Application: …" ./desktop/scripts/bundle.sh release
```

**Linux** — the `.deb` is built (in Docker, to pin the WebKitGTK toolchain) with:

```bash
make -C desktop deb            # → desktop/out/shed-desktop_<version>_<arch>.deb
make -C desktop deb-validate   # build + install-validate in a clean ubuntu:24.04 container
```

## What it needs at runtime

- `~/.shed/config.yaml` — the shed-server host list (created by the `shed` CLI). The app
  reads this read-only and watches it for changes.
- A reachable `shed-server` on at least one configured host. Unreachable hosts are shown
  as a degraded state, never a hard failure.

**Nothing else.** `brew install shed` (or `apt install shed`) plus the desktop app is a
complete install — the app brokers credential approvals (SSH sign, AWS, Docker) itself.
See below for how that differs between the two clients.

## Credential broker

The app's headline feature — SSH-sign approvals, AWS/Docker credential gating, and the
Activity audit feed — needs a **credential broker** in the loop. How that broker runs
differs by client:

| Client | Broker |
|---|---|
| **Tauri** (Linux; macOS beta) | **Embedded** — runs in-process, on by default. No extra install. |
| **Swift** (macOS stable) | **Separate daemon** — a standalone `shed-host-agent` process (Homebrew formula, `brew services start shed-host-agent`). See [Credential approvals](approvals.md). |

The Tauri client can also run against a standalone daemon instead of its embedded broker
— useful if you already run `shed-host-agent` headless on a server, or want one broker
shared across tools. A **Preferences → Credential broker** setting (`Automatic` /
`In-app (embedded)` / `External daemon`) controls it, default `Automatic`:

| Mode | Behavior |
|---|---|
| `Automatic` (default) | Probes for a running `shed-host-agent` at startup: a full daemon present ⇒ **external** (dials it, unchanged from today); a *headless* daemon (status socket only, no desktop socket) ⇒ **coexist** (the app doesn't start its own broker — no namespace conflicts — and mints its own secure-server tokens, but gets no in-app approvals, since the headless daemon owns those); neither present ⇒ **embedded**. |
| `In-app (embedded)` | Always starts the in-process broker, regardless of any running daemon. If a daemon is also running, the two race for each server's credential-bus namespace — the loser gets a per-namespace `409`, surfaced (not hidden) in `broker.status`; no double approval prompts. |
| `External daemon` | Always dials the standalone daemon, never starts an in-process broker. |

The resolved mode is **fixed for the app's process lifetime** — changing the preference
takes effect on the **next launch**, not live. The active mode, the probe evidence that
produced it, and (for embedded) the resolved SSH backend and per-server connection state
are visible in Preferences and over IPC (`broker.status`, `identify.broker_mode`). Full
mechanics — `extensions.yaml` handling, the synthesized fresh-install default, and the
fail-closed behaviors — are documented in
[Architecture → The embedded credential broker](architecture.md#the-embedded-credential-broker-tauri).
