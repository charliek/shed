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
| **Credential broker** | This machine, and one row per server: its credential source, with **Use this machine** | not stored (read from each server) | Which of your machines answers credential requests on each server. See [Credential sources](#credential-sources) below. |
| **SSH approvals** | Approval policy / Duration / Method | `ssh_policy`, `ssh_ttl`, `ssh_method` | Policy: Always Allow, Per Shed Allow, **Time Based Allow** (default), Always Ask, Always Deny. Duration (default `2h`) applies to Time Based Allow. Method, for the policies that prompt: Authenticate (`biometrics-or-password`, the default) or Prompt only (`prompt`). Changing any of these clears live SSH grants. |
| **AWS / Docker credentials** | Allow / Deny | `provider_modes` | A live Allow/Deny toggle (default Deny). No prompt; takes effect immediately. |
| **Per-shed overrides** | list of `(server, shed)` allow/deny rows | `extra_rules` | Per-`(server, shed)` *always-allow* / *always-deny* rules; remove one with the ✕. Shown only when there are any. |

The full policy model is in [Credential approvals](approvals.md). Registries (Docker) and
roles (AWS) are **not** configurable here — they live in `extensions.yaml`. Server hosts are
managed with the `shed` CLI (no Preferences UI); machines are added from the Machines pane.

## Credential sources

A shed server sends your sheds' credential requests to one of your machines only: your
[credential source](../extensions/source-binding.md) on that server. Below the broker mode,
Preferences shows:

- **This machine:** the name of the machine the app runs on, from the `machine.json` the
  app and `shed-host-agent` share. If the machine has no identity, the reason is shown and
  every **Use this machine** button is disabled.
- One row per configured server that supports credential sources (a server older than that
  shows no row): the server's name, `Source: <machine>` (with `· not connected` when that
  machine holds no subscription), or `No source yet — the first machine to connect becomes
  it.`. A server that answered `503 SOURCE_BUSY` on every retry shows the last source it
  knew, or `Checking the source — the server is busy, retrying…`. A server that could not be
  reached (it is restarting, or the network is down) keeps the row it last showed, with
  `· cannot reach the server`; the window reads it again after 2, 4, 8 and 16 seconds, then
  waits until it is shown, focused or reloaded; a server that answers busy after that shows
  `· server busy, retrying`, no longer `· cannot reach the server`. A server that answers a
  plain `404` (older than credential sources) has no row, and nor does one that has not been
  reached since the window opened (there is no last row to keep). The window makes one
  read at a time: a read asked for while one is in flight (a focus, a reload, a move's
  read-back) is made once, right after it.
- On the row of a server where this machine is the source, `this machine`; on any other row,
  **Use this machine**.

**Use this machine** makes this machine the source on that server: one click, no
confirmation, one request that is not retried. It always sends this machine's own id. When it
succeeds the row shows the server's answer; when the server refuses it the row returns to the
last confirmed source and shows why. Moving the source here drops any approval still waiting on
the other machine. If the server has never seen this machine (its broker has not reached that
server yet), the app says so and does not retry.

With the embedded broker, the app wakes the broker at once, so it connects without waiting for
its next retry. With an external `shed-host-agent`, the daemon picks the change up on its next
retry, within about 30 seconds. Nothing fails over by itself: if the source is off, nothing
answers until you choose another.

The window reads the sources when it opens and again whenever it is shown or focused, at most
once every five seconds. After a move whose read-back shows this machine `· not connected`
(its broker had not connected yet), it reads once more a few seconds later. The same reads and the move are available over IPC as
[`source.status` and `source.use`](ipc.md#approval-ops).

## Host list (`~/.shed/config.yaml`)

The app reads the **same file the `shed` CLI writes** to discover hosts. It is parsed
read-only, apart from the Machines pane's **Add machine**, which appends a `machines:` entry
after backing the file up. Shape (the server fields the app reads):

```yaml
servers:
  buildbox:
    host: buildbox
    http_port: 8080      # default 8080
    ssh_port: 2222       # default 22
    user: pat            # written by the shed CLI; the user this entry is held to
default_server: buildbox
```

A missing file degrades to an empty host list (no crash).

### The user an entry is held to

On a server with a user list, every client certificate names the user it was issued to. The
app (its control requests, and the credential broker's own certificate) uses a certificate only
when it names the entry's `user`, or the implicit user of a server with no user list. The `shed`
CLI writes `user` when it enrols the entry; the app only reads it.

- **A certificate for another user is refused.** When `ssh` can offer keys of two users and the
  server issues a certificate for the other one, the app does not use it: it keeps the
  certificate it holds and writes nothing. When the server then refuses the request, the host
  error says why, and so does the credential broker's last error for that server (a
  namespace's `last_error` in [`broker.status`](ipc.md); the broker log has it too):

  ```text
  server "buildbox": the server issued a certificate for user "admin", but this entry is held
  to "pat"; it was not used. Fix it with the shed CLI (shed server update buildbox
  --identity-file <key>, or --refetch --allow-user-change), then restart the app.
  ```

- **A certificate that names no user is refused.** Only a shed-server older than 0.10.0 issues
  one:

  ```text
  server "buildbox": the server issued a certificate that names no user (a shed-server older
  than 0.10.0); it was not used. Upgrade the server, then restart the app.
  ```

- **Restart the app after changing an entry's user** (`shed server update <name> --refetch`,
  with or without `--allow-user-change`). The app builds its control clients once, at start.
  The embedded broker picks up a changed `user` by itself.
- **Record the user once with the CLI.** For an entry with no `user`, the app holds the first
  named user it is issued, in memory only, so after a restart it takes the first named user
  again. The broker keeps its user across restarts through the certificate it stores. Run
  `shed server update <name> --refetch` once, even for a server you use only from the app, so
  `config.yaml` records the user and both paths hold it.
- A stored broker certificate that names no user, names another user than the entry's `user`,
  or cannot be read is not presented: the broker logs why and enrols again.
- For an entry with no `user`, the broker uses the certificate that sets the first user only
  once it has stored it, so a restart cannot forget that user. If its state directory is not
  writable, it refuses that certificate with a message that says it could not be stored.
- A `user` that is not a plain user name (a list or a map, a number, a value with a space)
  matches no certificate for a named user: every one is refused, as for another user. It is
  never read as "no user".
- An unquoted value is read as its text, by the app as by the `shed` CLI: `user: true` is the
  user name `true`. Quote the value (`user: "pat"`) if you mean a name that YAML would read as
  something else.
- When an entry's name cannot be pasted into a shell command as one word (it starts with `-`,
  or holds a line break or another non-printing character), the refusal gives the same advice
  in words instead of a command line.

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
