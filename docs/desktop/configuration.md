# Configuration

There are three places configuration lives, with deliberately little overlap:

1. **The Preferences window** — the handful of user-facing settings, persisted to a
   `prefs.json` in the app's config directory.
2. **`~/.shed/config.yaml`** — the host list, owned by the `shed` CLI and read by the app,
   which only ever appends a `machines:` entry.
3. **Environment variables** — overrides for testing, packaging, and release. Not something
   a normal user sets.

The app intentionally exposes a *small* settings surface; most behavior is derived from the
shed config and the host-agent rather than configured here.

## Preferences (GUI)

Open with the tray menu → **Preferences…** (or IPC `ui.show_preferences`). The settings
persist to `prefs.json` in the app config directory — `~/Library/Application
Support/ai.stridelabs.ShedDesktop/` on macOS, `${XDG_CONFIG_HOME:-~/.config}/ai.stridelabs.shed-desktop/`
on Linux. A missing or unreadable file falls back to the defaults; it never blocks startup.

The approval sections appear only for the providers the broker delegates to shed-desktop.

| Section | Setting | Stored as | What it does |
|---|---|---|---|
| **General** | Launch at login | the OS login item (a LaunchAgent on macOS, an XDG autostart entry on Linux), *not* `prefs.json` | Starts the app when you sign in. |
| **Terminal** | Open sheds in: Ghostty / Roost / Custom | `terminal_preset`, `terminal_template` | Which terminal **Open in Terminal** launches. Custom takes a command template where `{cmd}` is the ssh command and `{shed}` the shed name, e.g. `ghostty -e {cmd}`; an empty template opens the platform's default terminal. |
| **Credential broker** | Mode: Automatic / In-app (embedded) / External daemon | `broker_mode` | Where the credential broker runs; applies on the next launch. See [Installation → Credential broker](installation.md#credential-broker). |
| **SSH approvals** | Approval policy / Duration / Method | `ssh_policy`, `ssh_ttl`, `ssh_method` | Policy: Always Allow, Per Shed Allow, **Time Based Allow** (default), Always Ask, Always Deny. Duration (default `2h`) applies to Time Based Allow. Method, for the policies that prompt: Authenticate (`biometrics-or-password`, the default) or Prompt only (`prompt`). Changing any of these clears live SSH grants. |
| **AWS / Docker credentials** | Allow / Deny | `provider_modes` | A live Allow/Deny toggle (default Deny). No prompt; takes effect immediately. |
| **Per-shed overrides** | list of `(server, shed)` allow/deny rows | `extra_rules` | Per-`(server, shed)` *always-allow* / *always-deny* rules; remove one with the ✕. Shown only when there are any. |

The full policy model is in [Credential approvals](approvals.md). Registries (Docker) and
roles (AWS) are **not** configurable here — they live in `extensions.yaml`. Server hosts are
managed with the `shed` CLI (no Preferences UI); machines are added from the Machines pane.

## Host list (`~/.shed/config.yaml`)

The app reads the **same file the `shed` CLI writes** to discover hosts. It is parsed
read-only, apart from the Machines pane's **Add machine**, which appends a `machines:` entry
after backing the file up. Shape (the server fields the app reads):

```yaml
servers:
  mini3:
    host: mini3
    http_port: 8080      # default 8080
    ssh_port: 2222       # default 22
default_server: mini3
```

A missing file degrades to an empty host list (no crash).

## Host-agent approval (`extensions.yaml`)

Whether the credential-approval gate is active is configured in the broker's
`extensions.yaml`, not in this app — see [Credential approvals](approvals.md). In the
external broker mode the app connects to the daemon's socket (default
`~/Library/Application Support/shed/host-agent.sock` on macOS,
`$XDG_RUNTIME_DIR/shed/host-agent.sock` on Linux) when it is serving one.

## Environment variables

Normal use needs none of these. They exist for the test harness, packaging, and release.

### Runtime (read by the app / `shedctl`)

| Var | Purpose |
|---|---|
| `SHED_TAURI_SOCKET` | Override the IPC socket path (default `$XDG_RUNTIME_DIR/shed-tauri.sock`, else `/tmp/shed-tauri-<uid>/shed-tauri.sock`). `shedctl` honors it too. |
| `SHED_TAURI_LOG_FILE` | Move the app's log file. |
| `SHED_TAURI_LOG` | `=debug` raises the shed crates' lines to DEBUG and adds roost-ipc's. |
| `SHED_HOST_AGENT_SOCKET_DIR` | Move the directory the app looks in for the `shed-host-agent` sockets. |
| `SHED_TAURI_TEST_MODE` | `=1` → hermetic test mode: all HTTP clients hit the mock, `terminal.open` + Sparkle are disabled, the test-mode-only IPC ops are enabled, and `identify` echoes the flag. |
| `SHED_TAURI_MOCK_BASE_URL` | Redirects every shed-server client at one in-process mock (test mode); echoed by `identify`. |
| `SHED_TAURI_SHED_CONFIG` | Override the `~/.shed/config.yaml` path. In test mode an unset value loads an empty config. |
| `SHED_TAURI_HOST_AGENT_SOCKET` | Override the host-agent socket the approval client dials (a fake agent, in tests). |
| `SHED_TAURI_EXTENSIONS_CONFIG` | Override the `extensions.yaml` the embedded broker loads. |
| `SHED_TAURI_TEST_TIMEOUT_SCALE` | Multiply the harness's `wait_until` budgets (slow CI). Read by the harness, not the app. |

The log file's location and rotation are in [Architecture](architecture.md#state-storage).
The file is private (`0600`, under your own directories) and holds host names, paths and
diagnostic text; `SHED_TAURI_LOG=debug` adds roost's ssh diagnostics, which can include ssh's
own stderr. The remaining test seams (`SHED_TAURI_MOCK_UNREACHABLE_HOSTS`,
`SHED_TAURI_ROOST_SOCKETS`, `SHED_TAURI_CRAZE_PATH`, `SHED_TAURI_SSH_BIN`) are described in
[IPC → Test mode](ipc.md#test-mode).

### Build / release (read by `scripts/` + the release workflow)

| Var | Purpose |
|---|---|
| `SHED_DESKTOP_VERSION` | Override the version (else the top-level `desktop/VERSION` file). |
| `SHED_DESKTOP_NOTARY_PROFILE` | The `notarytool` keychain profile `scripts/notarize.sh` submits with. |
| `SHED_DESKTOP_DEVELOPER_ID_IDENTITY` | Developer-ID signing identity; absent ⇒ ad-hoc sign (notarization path stays inert). |
| `SHED_DESKTOP_ALLOW_UNSIGNED` | `=1` lets bundling continue when `codesign` is missing/failing. |
| `SHED_DESKTOP_DMG_FANCY` | `=1` uses `create-dmg` for a styled DMG (off on headless runners). |
| `SHED_DESKTOP_TAG` / `_APPCAST` / `_SIGN_FILE` / `_REPO` / `_MIN_MACOS` | Inputs to `scripts/update-appcast.py` (tag, appcast path, Sparkle signature file, `owner/repo`, min macOS). |

See [RELEASING.md](https://github.com/charliek/shed/blob/main/desktop/RELEASING.md) for the
release flow and the Sparkle/secret/ruleset setup.
