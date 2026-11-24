# macOS Quickstart

The opinionated, packaged path to a working shed on macOS, with the
[shed-desktop](https://charliek.github.io/shed-desktop/) menu-bar app (Tauri, the shipped
macOS client) delivering your credentials into sheds. Everything installs from Homebrew plus one DMG.

Building from source, custom images, or hacking on shed itself? See
[macOS Developer Setup](vz-setup.md) instead.

## What you get

- A local `shed-server` running VMs through Apple's Virtualization.framework (via
  `vfkit`, installed by Homebrew).
- **shed-desktop**: a menu-bar app (Tauri) that lists and creates sheds, and is your
  credential source: when a shed is created, it answers the server with the credentials you
  chose (an SSH key, a GitHub token, API keys, an envsecrets login) and the shed holds them
  as ordinary files and environment variables.

## Prerequisites

- macOS 14 (Sonoma) or newer, Apple Silicon (arm64).
- [Homebrew](https://brew.sh).
- No host Docker needed — the `full` shed image ships its own Docker daemon.

## 1. Install the CLI, server, and desktop app

```bash
brew install charliek/tap/shed             # shed CLI + shed-server (+ vfkit)
brew services start shed
```

Then install **shed-desktop**: download the latest `ShedDesktop-VERSION.dmg` from the
[releases page](https://github.com/charliek/shed/releases), open it, and drag
**ShedDesktop.app** to Applications.

Release builds are signed and notarized, so they open normally. If you built the
app yourself it is signed ad-hoc instead, and macOS quarantines it until you
clear the flag once:

```bash
xattr -dr com.apple.quarantine /Applications/ShedDesktop.app
```

The app is the only credential source; there is nothing else to install. (If you ran the
separate credential daemon from an earlier release, it has been removed from Homebrew:
uninstall it with `brew uninstall`; see
[Remove the old credential daemon](../upgrades/v0.9.0-to-v0.10.0.md#remove-the-old-credential-daemon) in the
upgrade guide for this release.)

`brew install charliek/tap/shed` installs the `shed` CLI and `shed-server`, pulls
`vfkit` (the Apple Virtualization bridge), generates a default VZ server config,
installs `shed-server` inside a signed app bundle (`ShedServer.app`, so macOS
privacy grants such as external-drive access survive upgrades), and registers a
launchd service.

## 2. Configure the server

The server config lives at `/opt/homebrew/etc/shed/server.yaml`. A minimal setup
shares a couple of host credential directories:

```yaml
# /opt/homebrew/etc/shed/server.yaml
# Full reference: https://charliek.github.io/shed/reference/configuration/
name: my-mac
http_port: 8080
ssh_port: 2222
default_backend: vz

# Local-only by default: bind_address defaults to loopback (127.0.0.1), so the
# server is reachable only on this Mac. You reach it as `shed server add localhost`.
# To reach it from another machine, see the Security Configuration guide.

# Host directories shared into every shed (VirtioFS). Add only what you need.
mounts:
  claude:
    source: ~/.claude
    target: /home/shed/.claude
    readonly: false
  gh:
    source: ~/.config/gh
    target: /home/shed/.config/gh
    readonly: false

# default_image / image_aliases are omitted, so they're synthesized from the
# server version — upgrades move to the matching images with no config edit.
```

Restart after editing: `brew services restart shed`.

## 3. Open shed-desktop and choose what a shed gets

Launch it (it lives in the menu bar, no Dock icon). It reads your
`~/.shed/config.yaml` host list and opens a delivery stream to each server. The first time it
starts, it turns on **launch at login**, so credentials keep being delivered whenever you
are logged in (they are delivered only while the app is running). The first of your
machines to connect to a server becomes your credential source there. The app only knows servers in `~/.shed/config.yaml`, so run `shed server add` from the next step first if you have not.

Open **Preferences → Provisioning** from the menu-bar dropdown. Turn on the providers you want
(`ssh-key`, `github`, `api-keys`, `envsecrets-login`, `files`) and give each an `only.servers`
rule naming the servers you trust with it. The **Preview** shows what a shed would get, with
sizes and fingerprints and never a value. Nothing is delivered until you turn a provider on.
See [Desktop provisioning](../desktop/provisioning.md) and
[Credentials in a shed](../guides/credentials-in-a-shed.md), including what it means that a
usable credential is inside the shed while it runs.

## 4. Register the server and create a shed

```bash
shed server add localhost --name my-mac   # registers the local server
shed create hello-world                   # a small shed to verify with
shed console hello-world                  # drop into a shell inside it
```

(You can also create from a repo or a local directory:
`shed create demo --repo charliek/your-repo`, or
`shed create demo --local-dir ~/projects/app`.) A create with `--repo` asks your source for
credentials before the clone, so a private repository clones with the delivered key. If the
app is not running, the create still finishes and prints the command that finishes the delivery:
`shed provision <name>`.

## 5. Verify delivery

Check what the shed holds and what is owed:

```bash
shed provision hello-world --status
shed exec hello-world -- ls -l .ssh     # the delivered key, if ssh-key is on
```

If the record shows steps owed, connect the source (open the app) and run `shed provision hello-world`.
Remove the test shed when you're done: `shed delete hello-world`.

## Next steps

- [Configuration](../reference/configuration.md) — every server-config field.
- [Credential delivery](../reference/delivery.md) — what is delivered, when, and the record.
- [shed-desktop docs](https://charliek.github.io/shed-desktop/) — the menu-bar app (Tauri) in full.
- [macOS Developer Setup](vz-setup.md) — build from source, custom images.
- Provisioning a project: [Gradle](../tutorials/gradle-provisioning.md) · [TypeScript](../tutorials/typescript-provisioning.md) · [Python](../tutorials/python-provisioning.md).
