# shed-ext-rc (RC session helper)

`shed-ext-rc` is the guest-side helper for **remote-control (RC) sessions** — the
detached `tmux` sessions (named `rc-<slug>`) that run an agent (`claude`, `codex`,
`cursor-agent`, `opencode`) or a shell inside a shed. The canonical implementation is
`internal/ext/rc` in this repo; the normative cross-repo spec is the **RC Session
Convention** doc in
[shed-remote-agent](https://github.com/charliek/shed-remote-agent/blob/main/docs/reference/rc-session-convention.md).

Two independent version numbers travel with a session, and they are **decoupled on
purpose**:

- **`SHED_RC_V` = 2** — the on-session tmux-env *metadata* schema (the `SHED_RC_*`
  keys). Unchanged by multi-agent support; session metadata is the same shape.
- **`rc_version` = 3** — the *capability/protocol* version reported by
  `capabilities` and the `list` envelope. A client learns what a shed's binary can do
  from `rc_version` + the `features` list, not from the metadata schema.

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
| `create --kind <k> --name <display> [--slug s] [--workdir d] [--created-by t/v] [--target label] [--wait] [--interactive-shell] [--prompt-stdin \| --plan-stdin [--prompt-b64 <b64>]] [--permission-mode <m> \| --skip]` | Resolve the workdir (`$SHED_WORKSPACE` default), pre-seed claude trust + onboarding for `claude-*` kinds, and `tmux new-session` with the `SHED_RC_*` env. Non-blocking by default. With `--wait`, poll to `ready`, auto-accept trust (and the bypass-mode dialog for `--skip`), and deliver the kickoff. `--permission-mode`/`--skip` set the autonomy posture — see [Permission modes](#permission-modes). Prints the [session DTO](#json-output). |
| `list` | Print `{"rc_sessions":[…],"capabilities":{…}}` — every `rc-*` session's DTO plus the embedded [capabilities](#capabilities) block (one exec feeds both). |
| `capabilities` | Print the [capabilities](#capabilities) payload standalone (kinds, per-agent install/version, features, per-kind hints). |
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
| `codex` | `codex` TUI |
| `cursor` | `cursor-agent` TUI |
| `opencode` | `opencode` TUI |
| `shell` | `bash -l` |

`claude-rc`, `codex`, `cursor`, and `opencode` accept a typed kickoff (a prompt/plan);
`claude-broker`'s input is its remote URL, and `shell` takes a command. Each kind's
per-agent permission mapping, classifier, and trust/preseed behavior live in one
registry table (`internal/ext/rc/agents.go`).

**Unknown-kind policy.** A reader that sees a `SHED_RC_KIND` it doesn't recognize
(e.g. a session created by a newer client) **preserves the raw string** and renders it
neutrally — name + state only, no kind-specific affordances and no synthetic claude URL.
It does not fall back to `claude-broker`. An unknown pane classifies as a plain shell
pane; an unknown `state` maps to `starting`.

### Permission modes

A generic tri-state — `default` | `auto` | `skip` — is accepted by **every** kind and
mapped per agent to that tool's real flags (the VM is already the sandbox). `--skip` is
shorthand for the generic `skip` mode; `--skip` and `--permission-mode` are mutually
exclusive. Omitting both passes no posture (each tool's own default).

| Generic mode | claude | codex | cursor | opencode |
|------|--------|-------|--------|----------|
| `default` | (none) | (none) | (none) | (none) |
| `auto` | `--permission-mode auto` | `--ask-for-approval on-request --sandbox workspace-write` | (none) | `--auto` |
| `skip` | `--permission-mode bypassPermissions` | `--dangerously-bypass-approvals-and-sandbox` | `--force` | `--auto` |

The **claude** kinds additionally accept claude's full historical `--permission-mode`
set — `acceptEdits`, `plan`, `dontAsk`, `bypassPermissions` — on top of the generic
tri-state. Passing one of those claude-only modes with a non-claude kind is rejected
(exit 2) with an error naming the generic set.

With `--wait` and `skip` for a claude kind, the poller auto-accepts claude's one-time
"Bypass Permissions mode" acceptance dialog so the session proceeds unattended.

### Prompts and plans (stdin)

A kickoff is passed via **stdin**, never as an argument — so a line beginning with `-` is
delivered literally, not parsed as a flag. `create` accepts at most one stdin payload:

- `--prompt-stdin` — stdin is a **prompt line**. For `claude-rc`/`codex`/`cursor`/
  `opencode` it is a prompt; for `shell` it is a command. `claude-broker` rejects it
  (its input is the remote URL).
- `--plan-stdin` — stdin is a **plan document** (UTF-8, ≤ 1 MiB). The binary writes it
  to a per-kind HOME-rooted file — claude: `~/.claude/plans/plan-<slug>.md`; other agents:
  `~/.shed-plans/plan-<slug>.md` (never the workdir, so a `--repo` clone or a
  VirtioFS-mounted host dir is never dirtied) — and composes a kickoff referencing the
  absolute path. Advertised as the `plan-stdin` feature.
- `--prompt-b64 <b64>` (only with `--plan-stdin`) — optional caller **framing** carried
  out-of-band as base64 (decoded and control-char-validated in-guest, prepended to the
  composed plan kickoff), so a single guest exec ships plan + framing without either
  colliding on stdin. Advertised as the `prompt-b64` feature.

The kickoff **may be multi-line**: a single line is typed with `send-keys -l`, and a
multi-line block is delivered as one input via a **bracketed paste** (`set-buffer` +
`paste-buffer -p`) so embedded newlines don't submit early — then one `Enter` submits the
whole thing. Newlines and tabs are allowed; other control characters (notably `ESC`) are
rejected so a paste can't break out of the bracketed paste.

```bash
echo -n 'fix the failing tests' | shed-ext-rc create --kind claude-rc --name demo --wait --prompt-stdin
echo -n 'npm test'              | shed-ext-rc prompt --slug abc123
# ship a plan (autonomous posture) and, optionally, lead with framing:
shed-ext-rc create --kind codex --name demo --wait --plan-stdin --skip < plan.md
shed-ext-rc create --kind claude-rc --name demo --wait --plan-stdin \
  --prompt-b64 "$(printf 'focus on the API layer' | base64)" < plan.md
```

## Capabilities

`capabilities` (and the block embedded in the `list` envelope) is the discovery
mechanism that replaces error-string sniffing: a client reads what a shed's binary can
do rather than probing by triggering failures. `rc_version` is the capability/protocol
version (currently **3**), decoupled from `SHED_RC_V` (metadata schema, still **2**).

```json
{
  "rc_version": 3,
  "kinds": ["claude-broker", "claude-rc", "codex", "opencode", "cursor", "shell"],
  "agents": {
    "claude": { "installed": true, "version": "2.1.206" },
    "codex":  { "installed": false }
  },
  "features": ["generic-perm", "plan-stdin", "prompt-b64"],
  "kind_features": {
    "codex": { "post_input": true, "approvals": "tui" }
  }
}
```

| Field | Meaning |
|-------|---------|
| `rc_version` | Capability/protocol version. Bumped when the capability shape or a feature contract changes; **not** tied to `SHED_RC_V`. |
| `kinds` | Every kind this binary offers (order matches the pinned wire contract). |
| `agents` | Per-tool install probe (`command -v` + `--version`, 2 s budget). `version` omitted when not installed. |
| `features` | Stable feature tokens — `generic-perm` (the `default`/`auto`/`skip` tri-state), `plan-stdin`, `prompt-b64`. A token is appended in the same change that ships its feature. |
| `kind_features` | Per-kind UI hints. `post_input` = a typed line can be delivered to the pane; `approvals` = where approvals happen (v1 agents are TUI-only → `tui`). `claude-broker` and `shell` are omitted. |

The `list` envelope embeds this block as `capabilities`. It is a pointer with
`omitempty`, so an **old** binary's bare `{"rc_sessions":[…]}` output still decodes — a
consumer tolerates the absence and simply has no capability data for that shed. Absence
of a feature token (or the whole block) is how a client detects an image that predates
multi-agent RC; new kinds / plan delivery require a recreated shed.

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
  "target_label": "shed:t1@host",
  "activity": "working",
  "activity_at": "2026-06-19T18:54:12Z",
  "last_message": "Running the test suite now."
}
```

`state` is one of `starting | ready | reconnecting | needs-trust | needs-auth | dead`,
derived live from the pane (never stored). A golden fixture of this shape
(`internal/ext/rc/testdata/rcSessionDto.golden.json`) is byte-identical to the consuming
repos' copies and asserted to decode in each — the guard against contract drift.

The `activity`, `activity_at`, and `last_message` fields are the additive **live
activity** dimension (a resident per-shed rc hub derives them). They are optional and
absent when no hub is running or the kind is unsupported:

| Field | Meaning |
|-------|---------|
| `activity` | Live work dimension, orthogonal to `state`: `working` \| `needs_input` \| `idle` \| `unknown` (the value `needs_approval` is reserved in the wire contract but not produced yet). Lifecycle trumps activity — a `needs-trust`/`needs-auth`/`dead` session reports no activity. |
| `activity_at` | RFC3339 timestamp the activity was last derived/changed. |
| `last_message` | Sanitized preview of the most recent message — ANSI/control-stripped, whitespace-collapsed, truncated to ≤200 runes. |

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
