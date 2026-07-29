# Configuration

There are three places configuration lives, with deliberately little overlap:

1. **The Preferences window** — the handful of user-facing settings, persisted to
   `UserDefaults`.
2. **`~/.shed/config.yaml`** — the host list, owned by the `shed` CLI and read **read-only**
   by the app.
3. **Environment variables** — overrides for testing, packaging, and release. Not something
   a normal user sets.

The app intentionally exposes a *small* settings surface; most behavior is derived from the
shed config and the host-agent rather than configured here.

## Preferences (GUI)

Open with the menu-bar dropdown → **Preferences…** (or IPC `ui.open_preferences`). Exactly
three values persist, under the standard suite `ai.stridelabs.ShedDesktop`:

The approval sections appear only for the providers the host agent delegates to shed-desktop
(`hello_ack.gate_namespaces`).

| Section | Setting | Stored as | What it does |
|---|---|---|---|
| **General** | Launch at login | OS login-item registration (`SMAppService`), *not* UserDefaults | Registers/unregisters the app as a login item. |
| **Terminal** | Command template (`{cmd}` placeholder) | `terminalTemplate` | Template for the terminal launch command; empty ⇒ Terminal.app. E.g. `ghostty -e {cmd}`. |
| **SSH approvals** | Method / Default decision / Duration | `sshMethod`, `sshScope`, `sshTTL` | Method: `biometrics-or-password` (default), `biometrics`, `prompt` (no Touch ID). Default decision (Per Shed Allow / **Time Based Allow** (default) / Always Ask) + duration (default `2h`, shown for Time Based) pre-fill the approval card. (Always Allow / Always Deny are per-decision card actions that persist a rule — not available as a default.) Changing any of these clears live SSH grants. |
| **AWS / Docker credentials** | Allow / Deny | `mode.aws-credentials`, `mode.docker-credentials` | A live Allow/Deny toggle (default Deny). No prompt; takes effect immediately. |
| **Per-shed overrides** | list of `(server, shed)` allow/deny rows | `policyRules` (JSON) | Per-`(server, shed)` *always-allow* / *always-deny* rules (added by **Always allow / Always deny** on an approval card); remove with the ✕. |

The persisted keys are `terminalTemplate`, the SSH defaults (`sshMethod`/`sshScope`/`sshTTL`),
the AWS/Docker `mode.<namespace>` toggles, and `policyRules` (the per-shed rules). The full
policy model is in [Credential approvals](approvals.md). Registries (Docker) and roles (AWS)
are **not** configurable here — they live in the host agent's `extensions.yaml`. Hosts are
managed with the `shed` CLI (no Preferences UI).

## Host list (`~/.shed/config.yaml`)

The app reads the **same file the `shed` CLI writes** to discover hosts. It is parsed
read-only — the app never modifies it. Override the path with `SHED_DESKTOP_SHED_CONFIG`.
Shape (the fields the app reads):

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

Whether the credential-approval gate is active is configured on the **host agent**, not in
this app — see [Credential approvals](approvals.md). The app auto-connects to the agent's
socket (default `~/Library/Application Support/shed/host-agent.sock`) when it's serving one.

## Environment variables

Normal use needs none of these. They exist for the test harness, packaging, and release.

### Runtime (read by the app / `shedctl`)

| Var | Purpose |
|---|---|
| `SHED_DESKTOP_TEST_MODE` | `=1` → hermetic test mode: all HTTP clients hit the mock, `terminal.open` + Sparkle are disabled, `policy.set` is enabled, and `identify` echoes the flag. |
| `SHED_DESKTOP_MOCK_BASE_URL` | Redirects every shed-server client at one in-process mock (test mode); echoed by `identify`. |
| `SHED_DESKTOP_SHED_CONFIG` | Override the `~/.shed/config.yaml` path. |
| `SHED_DESKTOP_HOST_AGENT_SOCKET` | Override the host-agent UDS path the approval client dials (a fake agent, in tests). |
| `SHED_DESKTOP_STATE_DIR` | Redirect the app's state dir (the `audit.jsonl`). Must be absolute; does **not** move the control socket/lock. |
| `SHED_DESKTOP_DEFAULTS_SUITE` | Scope `UserDefaults` to a throwaway suite (tests). |
| `SHED_DESKTOP_SOCKET` | Override the IPC socket path `shedctl` connects to. |
| `SHED_DESKTOP_TEST_TIMEOUT_SCALE` | Multiply the harness's `wait_until` budgets (slow CI). |

The Tauri (Linux) client mirrors these with a `SHED_TAURI_*` prefix
(`SHED_TAURI_TEST_MODE`, `SHED_TAURI_MOCK_BASE_URL`, `SHED_TAURI_SHED_CONFIG`,
`SHED_TAURI_SOCKET`); its default IPC socket is `$XDG_RUNTIME_DIR/shed-tauri.sock`.

### Build / release (read by `scripts/` + the release workflow)

| Var | Purpose |
|---|---|
| `SHED_DESKTOP_VERSION` | Override the version (else the top-level `desktop/VERSION` file). |
| `SHED_DESKTOP_DEVELOPER_ID_IDENTITY` | Developer-ID signing identity; absent ⇒ ad-hoc sign (notarization path stays inert). |
| `SHED_DESKTOP_ALLOW_UNSIGNED` | `=1` lets bundling continue when `codesign` is missing/failing. |
| `SHED_DESKTOP_DMG_FANCY` | `=1` uses `create-dmg` for a styled DMG (off on headless runners). |
| `SHED_DESKTOP_TAG` / `_APPCAST` / `_SIGN_FILE` / `_REPO` / `_MIN_MACOS` | Inputs to `scripts/update-appcast.py` (tag, appcast path, Sparkle signature file, `owner/repo`, min macOS). |

`scripts/live-verify.sh` builds the real Rust host-agent from the in-repo
`crates/shed-host-agent` (the credential broker lives in this monorepo); no
external checkout is needed.

See [RELEASING.md](https://github.com/charliek/shed/blob/main/desktop/RELEASING.md) for the
release flow and the Sparkle/secret/ruleset setup.
