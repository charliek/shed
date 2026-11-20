# macOS Quickstart

The opinionated, packaged path to a working shed on macOS, with the
[shed-desktop](https://charliek.github.io/shed-desktop/) menu-bar app (Tauri, the shipped
macOS client) handling credential approvals. Everything installs from Homebrew plus one DMG.

Building from source, custom images, or hacking on shed itself? See
[macOS Developer Setup](vz-setup.md) instead.

## What you get

- A local `shed-server` running VMs through Apple's Virtualization.framework (via
  `vfkit`, installed by Homebrew).
- **shed-desktop**: a menu-bar app (Tauri) that lists/creates sheds, shows a Touch ID
  approval prompt whenever a shed asks to use a credential, and is the credential broker:
  it brokers your real credentials (SSH agent, Docker registry auth, optionally AWS) into
  sheds — keys never leave your Mac.

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

The app is the only credential broker; there is nothing else to install. (If you ran the
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
shares a couple of host credential directories and enables the credential
extensions:

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

# Credential brokering by the shed-desktop app (configured in step 3).
extensions:
  enabled:
    - ssh-agent
    - docker-credentials
    # - aws-credentials

# default_image / image_aliases are omitted, so they're synthesized from the
# server version — upgrades move to the matching images with no config edit.
```

Restart after editing: `brew services restart shed`.

## 3. Configure credential brokering

The credential broker runs inside shed-desktop. It holds your real keys and decides each
request; its config is `~/.config/shed/extensions.yaml`. To route approvals to the app's
approval prompt, set each provider's policy to `shed-desktop` (there is nothing to
enable). With no file at all, the app uses a default that routes SSH approvals to the app.

```yaml
# ~/.config/shed/extensions.yaml
# Full reference: https://charliek.github.io/shed/extensions/configuration/
discovery:
  servers: all            # broker for every server in ~/.shed/config.yaml
  watch: fsnotify         # pick up `shed server add/remove` live

# Optional: how long to wait for a shed-desktop decision before failing closed (default 25s)
# approval_timeout: 25s

ssh:
  approval:
    policy: shed-desktop  # SSH sign approvals decided in shed-desktop
                          # no-GUI alternative: biometrics-or-password (native Touch ID)

docker:
  approval:
    policy: shed-desktop  # live Allow/Deny toggle in shed-desktop
  registries:             # registries the broker brokers creds for
    - ghcr.io
    - docker.io
    # - registry.example.com   # add your private/org registries here
  allow_all: false        # set true to broker for ANY registry (ignores the list)

# AWS credential vending (inactive until you set a role)
aws:
  approval:
    policy: shed-desktop  # or deny-all until a role is configured
  source_profile: default
  default_role: ""        # set an IAM role ARN to enable AWS vending
  session_duration: 1h
  cache_refresh_before: 5m
  # Per-shed role overrides:
  # sheds:
  #   my-shed:
  #     role: arn:aws:iam::123456789012:role/ShedRole

logging:
  enabled: true
  path: ~/.local/share/shed/extensions-audit.log
```

Restart shed-desktop to apply the file, then verify in the app: **Preferences → Credential
broker** shows that the broker started and lists each server with its credential source
(`shedctl raw broker.status` prints the same details, including the config it loaded, as
JSON). Policies, roles, and registries are off by default — see the
[full reference](../extensions/configuration.md).

## 4. Open shed-desktop

Launch it (it lives in the menu bar, no Dock icon). It reads your
`~/.shed/config.yaml` host list and starts its credential broker. The first time it starts, it
turns on **launch at login**, so credentials keep being answered whenever you are logged in
(they are answered only while the app is running). Open
**Preferences** from the menu-bar dropdown to set approval behavior:

| Section | What |
|---|---|
| **SSH approvals** | Method (Touch ID or password / Touch ID / Prompt), default decision (Time Based Allow, etc.), and duration. |
| **AWS / Docker credentials** | A live **Allow / Deny** toggle (default Deny). |
| **Per-shed overrides** | Always-allow / always-deny rules keyed by `(server, shed)`. |

Credentials **fail closed**: when shed-desktop isn't running, delegated requests
are denied. See the shed-desktop
[installation](https://charliek.github.io/shed-desktop/getting-started/installation/)
and [approvals](https://charliek.github.io/shed-desktop/reference/approvals/)
docs for the full policy model.

## 5. Register the server and create a shed

```bash
shed server add localhost --name my-mac   # registers the local server
shed create hello-world                   # a small shed to verify with
shed console hello-world                  # drop into a shell inside it
```

(You can also create from a repo or a local directory:
`shed create demo --repo charliek/your-repo`, or
`shed create demo --local-dir ~/projects/app`.)

## 6. Verify credential brokering

Run these **inside the shed** — the `shed console hello-world` shell from step 5.
The first SSH or Docker credential use raises a Touch ID approval in shed-desktop;
approve it (pick a policy like *Time Based Allow* so you aren't asked again for a
while).

```bash
# SSH — signs with your host key via the forwarded agent (-> approval in shed-desktop).
# Success prints: Hi <you>! You've successfully authenticated...
ssh -T git@github.com

# Docker — pull a small public image (anonymous pulls work with credsStore=shed):
docker pull postgres:16-alpine
# ...and a private registry to exercise credential brokering (-> approval):
# docker pull ghcr.io/your-org/your-image:tag

# AWS — only if you set default_role + aws.approval.policy in step 3:
aws sts get-caller-identity
```

Back on the **host**, check each shed's extension health and connection state:

```bash
shed list -vv
```

Remove the test shed when you're done: `shed delete hello-world`.

## Next steps

- [Configuration](../reference/configuration.md) — every server-config field.
- [Extensions](../reference/extensions.md) · [Extensions Architecture](../extensions/architecture.md) — the credential bus in full.
- [shed-desktop docs](https://charliek.github.io/shed-desktop/) — the menu-bar app (Tauri) in full.
- [macOS Developer Setup](vz-setup.md) — build from source, custom images.
- Provisioning a project: [Gradle](../tutorials/gradle-provisioning.md) · [TypeScript](../tutorials/typescript-provisioning.md) · [Python](../tutorials/python-provisioning.md).
