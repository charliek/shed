# shed-ext-rc (RC session helper)

`shed-ext-rc` is the guest-side helper for **remote-control (RC) sessions** — the
detached `tmux` sessions (named `rc-<slug>`) that run `claude` or a shell inside a
shed. It is the canonical implementation of the **RC Session Convention v2** (the
normative spec lives in
[shed-remote-agent](https://github.com/charliek/shed-remote-agent/blob/main/docs/reference/rc-session-convention.md)).

Orchestrators — shed-remote-agent, shed-desktop, the `shed` CLI — invoke it over SSH
instead of hand-building tmux commands, so every tool creates byte-compatible sessions
and classifies them identically:

```bash
ssh <shed>@<host> shed-ext-rc <command> [flags]
```

It is a one-shot CLI (no daemon). All tmux work happens locally inside the shed. The
interactive terminal **attach** is *not* routed through it (it stays a direct
`ssh … tmux attach`).

## Commands

| Command | Behaviour |
|---------|-----------|
| `create --kind <k> --name <display> [--slug s] [--workdir d] [--created-by t/v] [--target label] [--wait] [--interactive-shell] [--prompt-stdin] [--permission-mode <m> \| --skip]` | Resolve the workdir (`$SHED_WORKSPACE` default), pre-seed claude trust + onboarding for `claude-*` kinds, and `tmux new-session` with the `SHED_RC_*` env. Non-blocking by default. With `--wait`, poll to `ready`, auto-accept trust (and the bypass-mode dialog for `--skip`), and deliver a prompt. `--permission-mode`/`--skip` set the autonomy posture (claude kinds only) — see [Permission modes](#permission-modes). Prints the [session DTO](#json-output). |
| `list` | Print `{"rc_sessions":[…]}` — every `rc-*` session's DTO. |
| `probe --slug <s>` | Print one session DTO (state + url). Read-only. |
| `accept-trust --slug <s>` | Re-capture the pane; if claude's workspace-trust dialog is showing, send `Enter`. |
| `prompt --slug <s> [--session-id <uuid>]` | Deliver a single line (read from **stdin**) to a `ready` session. `--session-id` guards against a killed-and-recreated `rc-<slug>`. |
| `kill --slug <s>` | Kill the session (idempotent). |
| `version` | Print version. |

### Kinds

| Kind | Inner command |
|------|---------------|
| `claude-rc` | `claude --name <display> /rc` (interactive REPL; the create-time default). With `--permission-mode <m>`, uses `claude --remote-control --name <display> --permission-mode <m>` instead so the posture carries into the live session. |
| `claude-broker` | `claude remote-control --name <display> [--permission-mode <m>] --spawn same-dir` |
| `shell` | `bash -l` |

### Permission modes

For `claude-*` kinds only, `--permission-mode <mode>` sets claude's autonomy posture so a
session can run unattended (no effect on `shell`). `--skip` is shorthand for
`--permission-mode bypassPermissions`; the two are mutually exclusive. Omitting both keeps
claude's own default and the original inner-command forms, so existing callers are
unaffected.

| Mode | Posture |
|------|---------|
| `default` | claude's default (prompts on most tool use) |
| `acceptEdits` | auto-accept file edits; still gates other tools |
| `plan` | read-only planning |
| `auto` | autonomous with background safety checks |
| `dontAsk` | only tools allowed by config rules; deny the rest |
| `bypassPermissions` | no permission gates (full bypass) |

With `--wait` and `bypassPermissions`/`--skip`, the poller auto-accepts claude's one-time
"Bypass Permissions mode" acceptance dialog so the session proceeds unattended.

### Prompts (stdin)

A kickoff prompt is passed via **stdin** (`create --prompt-stdin`, or `prompt`), never as
an argument — so a line beginning with `-` is delivered literally, not parsed as a flag.
For `claude-rc` it is a prompt; for `shell` it is a command. `claude-broker` rejects a
prompt (its input is the remote URL).

The prompt **may be multi-line**: a single line is typed with `send-keys -l`, and a
multi-line block is delivered as one input via a **bracketed paste** (`set-buffer` +
`paste-buffer -p`) so embedded newlines don't submit early — then one `Enter` submits the
whole thing. Newlines and tabs are allowed; other control characters (notably `ESC`) are
rejected so a paste can't break out of the bracketed paste. (Prefer a short prompt + a
plan file for large/multi-step work.)

```bash
echo -n 'fix the failing tests' | shed-ext-rc create --kind claude-rc --name demo --wait --prompt-stdin
echo -n 'npm test'              | shed-ext-rc prompt --slug abc123
# autonomous: run the kickoff prompt without permission prompts
echo -n 'run the plan in PLAN.md' | shed-ext-rc create --kind claude-rc --name demo --wait --prompt-stdin --skip
```

## JSON output

The binary runs *inside* the shed, so it reports only what it can observe — it does
**not** know the orchestrator's host alias, shed name, or routing target. Each tool
adapts this neutral DTO into its own wire model. Optional fields are omitted (absent,
not `null`) when unknown; `managed` is always present.

```json
{
  "slug": "abc123",
  "tmux_session": "rc-abc123",
  "kind": "claude-rc",
  "state": "ready",
  "managed": true,
  "display_name": "demo",
  "workdir": "/home/shed",
  "url": "https://claude.ai/code/session_…",
  "id": "…uuid…",
  "created_by": "shed-remote-agent/0.1.0",
  "created_at": "2026-06-19T18:53:00Z",
  "target_label": "shed:t1@host"
}
```

`state` is one of `starting | ready | reconnecting | needs-trust | needs-auth | dead`,
derived live from the pane (never stored). A golden fixture of this shape
(`internal/ext/rc/testdata/rcSessionDto.golden.json`) is byte-identical to the consuming
repos' copies and asserted to decode in each — the guard against contract drift.

## Exit codes

The binary reports domain outcomes it observes locally; SSH-transport classification
(auth/unreachable) is the orchestrator's job.

| Code | Meaning |
|------|---------|
| `0` | success |
| `2` | invalid arguments / validation (e.g. a prompt for `claude-broker`, control chars, bad kind/slug) |
| `3` | duplicate slug (orchestrator maps to `409 RC_SLUG_TAKEN`) |
| `4` | session not found (`probe`/`prompt`; `kill` stays idempotent → `0`) |
| `1` | generic failure |

## Workspace trust and onboarding

For `claude-*` kinds, `create` pre-seeds `${CLAUDE_CONFIG_DIR:-$HOME}/.claude.json` so a
fresh shed reaches `ready` unattended without the workspace-trust or first-run dialogs:

- `projects["<workdir>"].hasTrustDialogAccepted` — marks the workspace trusted
- `hasCompletedOnboarding` — clears the first-run onboarding gate (theme picker)
- `theme` — set to a default only when absent (never clobbered)

It also suppresses first-run interstitials that could pop a modal over an unattended
session: it raises `fullscreenUpsellSeenCount` past the fullscreen-renderer upsell
threshold (never lowering an existing value) and sets `hasSeenAutoModeEntryWarning`.

Writes use merge-never-clobber semantics (unknown OAuth/MCP keys preserved), an atomic
write, and a file lock across concurrent creates. The `accept-trust` send-keys path is the
fallback for the trust dialog; for `bypassPermissions`/`--skip` sessions the `--wait`
poller also auto-accepts the one-time "Bypass Permissions mode" dialog. `create` does
**not** log claude in — authentication is provisioned separately. See the convention spec
for the full rules.
