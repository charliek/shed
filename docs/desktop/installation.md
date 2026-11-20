# Installation

shed-desktop is a Tauri app on one shared Rust core, shipped for macOS (Apple Silicon, a
DMG) and for Linux (WebKitGTK, an apt `.deb`).

## macOS (DMG)

The macOS app (Tauri) is an Apple Silicon (arm64) menu-bar app and requires macOS 14 or newer.

Grab the latest `ShedDesktop-<version>.dmg` from the
[releases page](https://github.com/charliek/shed/releases), open it, and drag
**ShedDesktop.app** to Applications.

Official stable release DMGs are always Developer-ID-signed and notarized, and launch
without a Gatekeeper prompt: a stable desktop tag fails to publish without the signing
credentials. Only a prerelease (`-rc`) tag may ship an ad-hoc-signed, non-notarized DMG,
which carries a `FIRST-LAUNCH.txt` with the one-time bypass steps.

### Updates

The app updates through **Sparkle**, served from the appcast at
`https://charliek.github.io/shed/appcast.xml` and verified by an EdDSA signature independent
of Apple notarization. Updates are **user-invoked only** — there are no automatic background
checks. Trigger one from the menu-bar popover → **Check for Updates…**;
if a newer build is published, Sparkle offers it and applies it in place.

| Behavior | Value |
|---|---|
| Trigger | User-invoked (**Check for Updates…**); no automatic/scheduled checks |
| Feed | `https://charliek.github.io/shed/appcast.xml` (stable channel) |
| Verification | EdDSA signature (`SUPublicEDKey`), independent of Apple notarization |
| Channels | Stable by default; prerelease (rc) builds subscribe to a **beta** channel |

Prerelease (`vX.Y.Z-rc.N`) tags publish a beta-channel DMG and stable tags the
stable-channel DMG, both from the same job. The app shares the feed, EdDSA key, and bundle
identity (`ai.stridelabs.ShedDesktop`) of the Swift app that shipped before 0.9.0, so an
install from before 0.9.0 updates in place (see the [0.9.0 upgrade
guide](../upgrades/v0.8.2-to-v0.9.0.md#macos-desktop)). See
[RELEASING.md](https://github.com/charliek/shed/blob/main/desktop/RELEASING.md).

Locally built DMGs (`make -C desktop tauri-dmg-mac`) are **ad-hoc signed**, so Gatekeeper blocks the
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

**macOS** — prerequisites: Rust stable ≥1.97, Node 20+, and the Xcode Command Line Tools
(see [Development setup](development-setup.md) for the full list).

```bash
git clone https://github.com/charliek/shed
cd shed
npm --prefix desktop/tauri/ui ci
make -C desktop tauri-bundle-mac   # desktop/build/ShedDesktop.app (ad-hoc signed)
open desktop/build/ShedDesktop.app
```

`make -C desktop tauri-dmg-mac` packages it into
`desktop/build/ShedDesktop-<version>.dmg`. To produce a notarizable build locally, set the
signing identity:

```bash
SHED_DESKTOP_DEVELOPER_ID_IDENTITY="Developer ID Application: …" make -C desktop tauri-dmg-mac
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

**Nothing else.** `brew install shed` (or `apt install shed-server` on a Linux host) plus the
desktop app is a complete install — its credential broker handles credential approvals (SSH
sign, AWS, Docker) itself, with nothing else to install.

## Credential broker

The app's headline feature — SSH-sign approvals, AWS/Docker credential gating, and the
Activity audit feed — needs a **credential broker** in the loop. The app is that broker: it
runs it in its own process, on both macOS and Linux, with nothing extra to install and no
mode to choose. A machine without the app (a server, or a box with no desktop session) has no
broker.

Credential requests are answered only while the app is running. To keep it running, the app
turns on **launch at login** once, at its first launch (an upgraded install included), and
records that in its preferences. It never turns it on again, so if you switch it off in
Preferences it stays off. A debug build does not register itself.

If you installed the separate credential daemon from Homebrew with an earlier release, it has
been removed from the tap and does nothing now: uninstall it with `brew uninstall` (see
[Remove the old credential daemon](../upgrades/v0.9.0-to-v0.10.0.md#remove-the-old-credential-daemon) in the
upgrade guide for this release).

The active SSH backend and per-server connection state are visible in Preferences → Credential
broker and over IPC (`broker.status`). The app and the servers see one machine identity, and
Preferences shows which of your machines is the [credential source](../extensions/source-binding.md)
on each server. Full mechanics — `extensions.yaml` handling, the synthesized fresh-install
default, and the fail-closed behaviors — are in
[Architecture → The credential broker](architecture.md#the-credential-broker).
