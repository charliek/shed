# Configuration

There are three places configuration lives, with deliberately little overlap:

1. **The Preferences window** — the handful of user-facing settings, persisted to a
   `prefs.json` in the app's config directory.
2. **`~/.shed/config.yaml`** — the host list, owned by the `shed` CLI and read by the app.
   The app reads `servers:` and `default_server` only. A `machines:` block, if one is still
   in the file, is left alone: the CLI preserves it as an opaque YAML subtree on save
   (formatting and indentation may be normalized on re-encode), and the desktop does not
   list those hosts. Interactive sessions on a machine that is not a shed belong in the
   `roost` app.
3. **Environment variables** — overrides for testing, packaging, and release. Not something
   a normal user sets.

The app intentionally exposes a *small* settings surface; most behavior is derived from the
shed config and the delivery settings (`delivery.toml`) rather than configured here.

## Preferences (GUI)

Open with the tray menu → **Preferences…** (or IPC `ui.show_preferences`). The settings
persist to `prefs.json` in the app config directory — `~/Library/Application
Support/ai.stridelabs.ShedDesktop/` on macOS, `${XDG_CONFIG_HOME:-~/.config}/ai.stridelabs.shed-desktop/`
on Linux. A missing or unreadable file falls back to the defaults; it never blocks startup.

| Section | Setting | Stored as | What it does |
|---|---|---|---|
| **General** | Launch at login | the OS login item (a LaunchAgent on macOS, an XDG autostart entry on Linux), *not* `prefs.json`; `login_item_defaulted` records only that the app turned it on once | Starts the app when you sign in. On by default: the app turns it on once, at its first launch, so credentials are delivered whenever you are logged in. Switch it off and it stays off. |
| **Terminal** | Open sheds in: Ghostty / Roost / Custom | `terminal_preset`, `terminal_template` | Which terminal **Open in Terminal** launches. Custom takes a command template where `{cmd}` is the ssh command and `{shed}` the shed name, e.g. `ghostty -e {cmd}`; an empty template opens the platform's default terminal. |

Preferences holds General and Terminal only. Credentials are set up in the **Credentials** view of the sidebar (see [Credentials](credentials.md)). Server hosts are managed with the `shed` CLI (no Preferences UI).

## Credential sources

A shed server asks one of your machines only for your sheds' credentials: your
[credential source](../extensions/source-binding.md) on that server, or, on a server where the
account service answers, the service instead. The sidebar's **Credentials** view shows who
answers on each server and offers **Answer from here** and **Stop answering from here**; see
[Credentials → Where](credentials.md#where).

The machine's name comes from the `machine.json` the app keeps. If the machine has no identity,
the view says why and offers no move. **Stop answering from here** records the server in
`paused-servers.json` beside `machine.json` (mode `0600`), so the stop survives restarts; an
unreadable file pauses every server. **Answer from here** takes the server out of the file.
Moving the source here fails any delivery request still waiting on the other machine. If the
server has never seen this machine (its delivery stream has not reached it yet), the app says so
and does not retry. The same reads and moves are available over IPC as
[`sources.list`, `source.use` and `source.unbind`](ipc.md#source-delivery-and-egress-ops).

## Host list (`~/.shed/config.yaml`)

The app reads the **same file the `shed` CLI writes** to discover shed servers. It does not
write the file. Shape (the server fields the app reads):

```yaml
servers:
  buildbox:
    host: buildbox
    http_port: 8080      # default 8080
    ssh_port: 2222       # default 22
    user: pat            # written by the shed CLI; the user this entry is held to
    identity_file: /home/pat/.ssh/id_work   # optional; the SSH key this entry uses
default_server: buildbox
```

A missing file degrades to an empty host list (no crash).

### The user an entry is held to

On a server with a user list, every client certificate names the user it was issued to. The
app (its control requests, and the delivery stream's own certificate) uses a certificate only
when it names the entry's `user`, or the implicit user of a server with no user list. The `shed`
CLI writes `user` when it enrols the entry; the app only reads it.

- **A certificate for another user is refused.** When `ssh` can offer keys of two users and the
  server issues a certificate for the other one, the app does not use it: it keeps the
  certificate it holds and writes nothing. When the server then refuses the request, the host
  error says why, and so does the delivery stream's last error for that server (`last_error` in [`delivery.status`](ipc.md); the app log has it too):

  ```text
  server "buildbox": the server issued a certificate for user "admin", but this entry is held
  to "pat"; it was not used. Fix it with the shed CLI (shed server update buildbox
  --identity-file /path/to/key, or --refetch --allow-user-change), then restart the app.
  ```

  While the server keeps issuing the other user's certificate, the app asks it for one again
  only after a wait: 1 second after the first refusal, then 2, 4, 8 and 16 seconds, then
  every 30 seconds. The app's control requests and the delivery stream each keep their own
  wait on this schedule; the stream's holds for every request it answers and every
  reconnect it makes, however many arrive meanwhile. While the app still holds an earlier
  certificate for the entry's user that has not expired, it keeps using it. When it holds no
  usable certificate for that user (none, or only one that has since expired), a request is
  refused: it fails at once with the same message and is not sent. The entry stays held to
  its user all the same. When the server issues a certificate for the entry's user again
  (for example, the user's key is back in its list), each uses it at its first attempt once its
  wait has passed, with no restart: the stream at its next request or reconnect,
  the control path when the dashboard next polls (every 5 seconds while the window is shown),
  so the gap seen in the dashboard is about 35 seconds at most.

  The wait starts again from 1 second when a certificate for the entry's user is used, or
  when the app restarts. A change to `config.yaml` resets only the delivery stream's wait,
  and only for a server whose entry changed in a field it reads (such as its host,
  ports, URL, TLS pin, `user` or `identity_file`): the stream then starts that server's
  source afresh. The app's control requests keep their wait, and their clients, until the
  app restarts. After fixing the entry, restart the app; without a restart, the control
  path's next attempt comes within its wait.

- **A certificate that names no user is refused.** Only a shed-server older than 0.10.0 issues
  one:

  ```text
  server "buildbox": the server issued a certificate that names no user (a shed-server older
  than 0.10.0); it was not used. Upgrade the server, then restart the app.
  ```

- **Restart the app after changing an entry's user** (`shed server update <name> --refetch`,
  with or without `--allow-user-change`). The app builds its control clients once, at start.
  The delivery stream picks up a changed `user` by itself.
- **Record the user once with the CLI.** For an entry with no `user`, the app holds the first
  named user it is issued, in memory only, so after a restart it takes the first named user
  again. The delivery stream keeps its user across restarts through the certificate it stores. Run
  `shed server update <name> --refetch` once, even for a server you use only from the app, so
  `config.yaml` records the user and both paths hold it.
- A stored certificate that names no user, names another user than the entry's `user`,
  or cannot be read is not presented: the app logs why and enrols again.
- For an entry with no `user`, the app uses the certificate that sets the first user only
  once it has stored it, so a restart cannot forget that user. If its state directory is not
  writable, it refuses that certificate with a message that says it could not be stored.
- A `user` that is not a plain user name (a list or a map, a number, a value with a space)
  matches no certificate for a named user: every one is refused, as for another user. It is
  never read as "no user".
- An unquoted value is read as its text, by the app as by the `shed` CLI: `user: true` is the
  user name `true`. Quote the value (`user: "pat"`) if you mean a name that YAML would read as
  something else.
- When an entry's name cannot be pasted into a shell command as one word (it starts with `-`,
  or holds a line break, another control character or any non-ASCII character), the refusal
  gives the same advice in words instead of a command line. The `shed` CLI follows the same
  rule.

### The SSH key an entry uses

An entry the `shed` CLI added or updated with `--identity-file` names the SSH key it uses
(`identity_file`; see the CLI's
[One SSH key per entry](../reference/cli.md#one-ssh-key-per-entry)). The app and its delivery
stream read it with the CLI's value rule:

- **Every `ssh` they run for the entry passes the key**: `-i <path> -o IdentitiesOnly=yes -o
  ControlMaster=no -o ControlPath=none`, byte for byte as the CLI does (the enrolment
  over `_bootstrap`, the terminal command). A roost connection (a shed's session list, the
  bootstrap that installs roost, the craze hub) reads a generated ssh config whose stanza for
  the host carries the same four lines and comes before your own config; it keeps its own
  private connection, which its command line names.
- **The file is checked each time the app builds an `ssh` command for the entry,
  and each time it opens a tunnel or a command runner for it**, as the CLI checks it: it must
  still be a regular file. One that is missing, or a value the rule refuses, is a refusal that
  names the entry, and that `ssh` does not run: `entry "buildbox": identity_file
  "/home/pat/.ssh/id_work" cannot be used: the file does not exist`. This is an early, clear
  error instead of an `ssh` that quietly tries your other keys; it is not checked again for a
  connection a tunnel that is already open makes later, for the local commands that only close
  a shared connection, or when a terminal runs a command the app built for it earlier.
- **A stored token is never used for the entry.** A `control_token` in `config.yaml` is not
  sent (`entry "buildbox" cannot be used: it holds a control token …; the token was not sent.
  Enrol it again: shed server update buildbox --refetch`), and neither is a
  `credentials_token`.
- **An entry the app cannot read exactly is never read as "no key".** The app reads a server
  entry only when each line in it is a plain `key: value` (a plain or quoted key and a plain or
  quoted value, on one line). An entry with any other line (an explicit `? key`, the fields as a
  `{…}` mapping, a tag or an anchor such as `!!str …` or `&a …`, a merge key `<<`, a `|` or `>`
  block, a value over more than one line, a repeated key, a list) is still listed, and every use
  of it by the app is refused: `entry "buildbox" cannot be used: shed reads a server entry only
  when each line in it is a plain key: value …; write its fields one per line, as key: value`.
  The app's own requests (the dashboard and its control requests) send nothing to its server.
  So is an entry whose `host` is null (`host: null`, `host: ~` or a bare `host:`); an entry
  with no `host` at all still uses its name as the host. The delivery stream reads entries
  with another reader, and still serves some of these: see the limits under
  [How the app reads the file](#how-the-app-reads-the-file).

What the app does not do: they do not check that a certificate was made for the
entry's key (the CLI does). If `ssh` authenticates with another key that your own ssh config
names for the host (a `Host` or `Match` stanza, or `Host *`, can add its `IdentityFile` lines
after the entry's), the app accepts the certificate when it names the entry's `user`, and
refuses it when it names anyone else ([the user an entry is held to](#the-user-an-entry-is-held-to)).
Keep no other `IdentityFile` for that host if the entry must use one key. Restart the app after
changing an entry's `identity_file`: it builds its control clients once, at start (the delivery
stream picks the change up by itself).

### How the app reads the file

**The app reads `config.yaml` as the shed CLI writes it.** Every entry the CLI's own save path
writes with ordinary values is read with the same host, ports, user and `identity_file` the
CLI reads. An entry, or the whole file, written in a form the app does not read exactly is
**refused by name, with a reason**; it is never read with missing fields filled by defaults. A
file edited by hand into YAML forms the CLI never writes may be refused by the app while the
CLI accepts it: that difference is by design. (`config.yaml` is your own file; the app's job is
never to misread what the CLI wrote, not to defend against a hostile file.)

- **Values** are read as YAML reads them: a `#` is a comment only after a space or a tab
  (`/home/pat/.ssh/id#work` is that path), a quoted value keeps its `#`, double-quoted escapes
  such as `\t` are decoded, `''` in a single-quoted value is one `'`, and a plain `null` or `~`
  is no value (`user: null` is no user).
- **Keys**: a key ends at the first `:` followed by a space or the end of the line, so an entry
  named `prod:blue` or `10.0.0.5:8443` is read under that name. A name the CLI writes in YAML's
  explicit form (a name longer than 128 bytes, written as `? <name>` and `: …`) is not readable
  by the app: that entry is listed under its name and refused. So is a name that begins with a
  character YAML gives a meaning there, such as `-prod`.
- **The file**: a byte-order mark at the start is skipped, and only the first YAML document is
  read (a `---` or `...` line after content ends it, as for the CLI).
- **A file the app does not read at all**, so that no server or machine is listed and the app's
  log says why: one that holds a carriage return that does not end a CRLF line, a next-line
  character (U+0085), a line separator (U+2028) or a paragraph separator (U+2029) anywhere (the
  escaped forms inside a double-quoted value are text); one with the key `servers:` twice; one
  whose `servers:` is written in flow form (`servers: {…}`), or with a tag or an anchor
  (`!!str servers:`, `servers: &s`), or whose first line under it is indented with a tab; and
  one whose first `---` has content on its line.
- **An entry the app does not read** (listed, and refused at every use the app makes of it, as
  above): any line in
  it that is not a plain `key: value`, a line in it indented with a tab, or a null `host`.

Limits, each refused rather than misread:

- A value the CLI writes as a multi-line block (a path holding a line break, written `|-`)
  makes the app refuse that entry. A line break is not an ordinary value for a host, a user or
  a path.
- A value the CLI writes with a `!!binary` tag (a string that is not valid UTF-8) makes the app
  refuse that entry.
- A `machines:` block is not a host list the app reads. The CLI preserves it on save.
  Hosts named there are not shown; use the `roost` app for an interactive session on one.
- An entry whose `host` is an empty string (`host: ""`) is read as an entry with an empty
  host, as before.
- **The refusals above apply to the delivery stream too.** An entry the app refuses sends
  nothing from the app itself: not the dashboard's requests, not its control requests, and not
  a delivery stream, because the source runtime finds its servers through the same
  `shed-core` reader. (A source skips an entry with no SSH port rather than guess one, so
  nothing is minted for it.)

## Environment variables

Normal use needs none of these. They exist for the test harness, packaging, and release.

### Runtime (read by the app / `shedctl`)

| Var | Purpose |
|---|---|
| `SHED_TAURI_SOCKET` | Override the IPC socket path (default `$XDG_RUNTIME_DIR/shed-tauri.sock`, else `/tmp/shed-tauri-<uid>/shed-tauri.sock`). `shedctl` honors it too. |
| `SHED_TAURI_LOG_FILE` | Move the app's log file. |
| `SHED_TAURI_LOG` | `=debug` raises the shed crates' lines to DEBUG and adds roost-ipc's. |
| `SHED_TAURI_TEST_MODE` | `=1` → hermetic test mode: all HTTP clients hit the mock, `terminal.open` + Sparkle are disabled, the test-mode-only IPC ops are enabled, and `identify` echoes the flag. |
| `SHED_TAURI_MOCK_BASE_URL` | Redirects every shed-server client at one in-process mock (test mode); echoed by `identify`. |
| `SHED_TAURI_SHED_CONFIG` | Override the `~/.shed/config.yaml` path. In test mode an unset value loads an empty config. |
| `SHED_BROKER_STATE_DIR` | Replace the app's state directory (default: macOS `~/Library/Application Support/shed`, Linux `$XDG_DATA_HOME/shed` else `~/.local/share/shed`), which holds `broker/machine.json` and the app's certificates. |
| `SHED_TAURI_TEST_TIMEOUT_SCALE` | Multiply the harness's `wait_until` budgets (slow CI). Read by the harness, not the app. |

The log file's location and rotation are in [Architecture](architecture.md#state-storage).
The file is private (`0600`, under your own directories) and holds host names, paths and
diagnostic text; `SHED_TAURI_LOG=debug` adds roost's ssh diagnostics, which can include ssh's
own stderr. The remaining test seams (`SHED_TAURI_MOCK_UNREACHABLE_HOSTS`,
`SHED_TAURI_ROOST_SOCKETS`, `SHED_TAURI_SSH_BIN`) are described in
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
