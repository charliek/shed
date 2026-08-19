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
- **`rc_version` = 4** — the *capability/protocol* version reported by
  `capabilities` and the `list` envelope. A client learns what a shed's binary can do
  from `rc_version` + the `features` list, not from the metadata schema.

Orchestrators — shed-remote-agent, shed-desktop, the `shed` CLI — invoke it over SSH
instead of hand-building tmux commands, so every tool creates byte-compatible sessions
and classifies them identically:

```bash
ssh <shed>@<host> shed-ext-rc <command> [flags]
```

Every subcommand is **one-shot** (does its tmux work locally and exits) except
`serve`, which runs the resident **RC activity hub** — a loopback HTTP daemon that
watches the shed's rc sessions and streams live activity (see [The RC activity
hub](#the-rc-activity-hub-serve)). All tmux work happens locally inside the shed. The
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
| `serve [--detach \| --foreground]` | Run the resident [RC activity hub](#the-rc-activity-hub-serve). `--detach` double-forks a background daemon and returns once its port is up; `--foreground` runs it in this process (the default when neither flag is given). Spawned on demand, self-exiting when idle. |
| `version` | Print version. |

### Kinds

| Kind | Inner command |
|------|---------------|
| `claude-rc` | `claude --name <display> /rc` (interactive REPL; the create-time default). With `--permission-mode <m>`, uses `claude --remote-control --name <display> --permission-mode <m>` instead so the posture carries into the live session. |
| `claude-broker` | `claude remote-control --name <display> [--permission-mode <m>] --spawn same-dir` |
| `codex` | `codex` TUI |
| `cursor` | `cursor-agent --trust` TUI (`--trust` skips the workspace-trust dialog — the same posture as the claude kinds' trust preseed; without it an unattended kickoff in a fresh workspace stalls at a dialog no classifier models) |
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
version (currently **4**), decoupled from `SHED_RC_V` (metadata schema, still **2**).

```json
{
  "rc_version": 4,
  "kinds": ["claude-broker", "claude-rc", "codex", "opencode", "cursor", "shell"],
  "agents": {
    "claude": { "installed": true, "version": "2.1.206" },
    "codex":  { "installed": false }
  },
  "features": ["generic-perm", "plan-stdin", "prompt-b64", "serve", "activity", "messages", "contract-v2"],
  "kind_features": {
    "claude-rc": { "post_input": true, "approvals": "tui", "feed": "activity", "interrupt": false, "attach": "tmux" },
    "codex": { "post_input": true, "approvals": "tui", "watch": true, "input": "gated", "feed": "messages", "interrupt": false, "attach": "tmux" },
    "opencode": { "post_input": true, "approvals": "remote", "watch": true, "input": "turn", "feed": "messages", "interrupt": true, "attach": "tmux" },
    "cursor": { "post_input": true, "approvals": "tui", "watch": true, "input": "gated", "feed": "messages", "interrupt": false, "attach": "tmux" }
  }
}
```

| Field | Meaning |
|-------|---------|
| `rc_version` | Capability/protocol version. Bumped when the capability shape or a feature contract changes; **not** tied to `SHED_RC_V`. |
| `kinds` | Every kind this binary offers (order matches the pinned wire contract). |
| `agents` | Per-tool install probe (`command -v` + `--version`, 2 s budget). `version` omitted when not installed. |
| `features` | Stable feature tokens — `generic-perm` (the `default`/`auto`/`skip` tri-state), `plan-stdin`, `prompt-b64`, `serve` (the on-demand rc activity hub), `activity` (the live activity dimension), `messages` (the codex/opencode/cursor message feed + gated input endpoints — per-kind availability is in `kind_features`), `contract-v2` (the v2 wire contract: `lane` on every session DTO, the `feed`/`interrupt`/`attach` hints in `kind_features`, the `turn`/`interrupt`/`approvals` hub verbs — routed and fully specified, live for opencode, `409 not_supported` for every other kind — the `approval_request` feed row, and `pending_approvals` on the session). A token is appended in the same change that ships its feature; `contract-v2` is a client's **route-existence** check — a server without it may 404 the new verbs at the mux, so a client reads the token instead of interpreting a bare 404. |
| `kind_features` | Per-kind UI hints — see [`kind_features` matrix](#kind_features-matrix) below. |

The `list` envelope embeds this block as `capabilities`. It is a pointer with
`omitempty`, so an **old** binary's bare `{"rc_sessions":[…]}` output still decodes — a
consumer tolerates the absence and simply has no capability data for that shed. Absence
of a feature token (or the whole block) is how a client detects an image that predates
multi-agent RC; new kinds / plan delivery require a recreated shed.

### `kind_features` matrix

`kind_features` is the per-kind row a client (mobile above all) renders watch/steer/
approve affordances from, without keeping its own per-kind table. A kind is
**lane-homogeneous** — every session of a kind shares one lane — so this kind-keyed row
is a complete description of every session of that kind. `claude-broker` and `shell` are
**omitted entirely**: an absent entry means no feed/input/approval affordances, exactly
today's client behavior for those two kinds.

| Field | Meaning |
|-------|---------|
| `post_input` | A typed line can be delivered to the session's pane (the prompt/attach kickoff path). **Not deprecated** — nothing in contract v2 supersedes it, opencode included (the create/prompt kickoff path still uses it for a session's first prompt). |
| `approvals` | Where approvals are answered: `tui` (in the terminal — claude-rc, codex, cursor) or `remote` (through the hub's `POST /approvals/{id}` verb — opencode, live since this block). |
| `watch` | **Deprecated** by `feed` (superseded, not removed): retained until clients migrate. The producer holds `watch == (feed == "messages")` in lockstep, so a v1 client reading `watch` and a v2 client reading `feed` see the same thing. Absent-field fallback: a client that only knows `watch` should keep using it. |
| `input` | Feed-input posting mode, **single-valued**: `gated` (`POST …/input` accepted only while the session is waiting — codex, cursor), `turn` (the lane takes whole turns through `POST …/turn`, and `POST …/input` no longer applies — opencode), or `""` (no feed input at all — claude-rc; the TUI-only `post_input` path still applies). `turn` supersedes `gated` for a kind that has it: the two are mutually exclusive spellings of "how a client steers this kind's feed", not layered capabilities. |
| `feed` | What the hub can stream for the kind: `messages` (a normalized conversation feed — `GET …/messages` + `message.appended`), `activity` (the activity dimension only — no message feed), or `none` (no hub signal at all). Supersedes `watch`. |
| `interrupt` | The `interrupt` verb is supported. `true` for opencode only; `false` elsewhere. |
| `attach` | How a terminal reaches the session: `tmux` (attach to the rc-tmux session), `native-remote` (the agent's own remote surface), or `none`. |

Normative matrix (exhaustive — pinned by `capabilities_test.go`):

| kind | post_input | approvals | watch | input | feed | interrupt | attach |
|---|---|---|---|---|---|---|---|
| claude-rc | true | tui | false | "" | activity | false | tmux |
| codex | true | tui | true | gated | messages | false | tmux |
| opencode | true | remote | true | turn | messages | true | tmux |
| cursor | true | tui | true | gated | messages | false | tmux |

opencode is the first **live** lane (§ [Contract-v2 verbs](#contract-v2-verbs-turn-interrupt-approvalsid) below): its TUI runs an embedded HTTP+SSE server the hub steers through, so whole turns, interrupts, and approvals all go through the hub instead of the pane. cursor gained a normalized `messages` feed (its own hook scripts push turn boundaries, tool calls and messages into the hub — see [Cursor hook ingestion](#cursor-hook-ingestion)) and `gated` input (its composer-anchor gate, identical in shape to codex's), but its approvals stay `tui`: cursor's hooks carry no approval-pending event, so nothing the hub receives is remotely answerable — see [`needs_approval` producers](#needs_approval-producers-per-kind) below. `"none"` is reserved for a kind with no hub signal at all (none exists yet).

`feed` and `attach` carry `omitempty` but are **never** empty in this binary's own
output (the strict golden pins them present) — the `omitempty` exists so a newer server
re-emitting an **older** guest's decoded capabilities (the overview embeds the struct
raw) emits the fields as absent rather than `""`. `interrupt` is unconditional: `false`
is its real matrix value, and an absent bool already decodes to the same default
everywhere.

**Client fallbacks for absent fields** (a v3 payload, or a re-emitted older guest's
capabilities): absent `feed` → fall back to `watch`; absent `attach` → treat as `tmux`;
absent `lane` on the session DTO → treat as `"tui"`.

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
  "lane": "tui",
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

`target_label` is **opaque metadata** echoed back verbatim from the `--target`
value the orchestrator (or session creator) supplied at `create` time — the guest
does not discover it, cannot verify it, and it carries **no routing or
authorization authority**. It is a label for the creator's own bookkeeping, not a
guest-attested route; clients must never treat it as an authoritative target.

`state` is one of `starting | ready | reconnecting | needs-trust | needs-auth | dead`,
derived live from the pane (never stored). A golden fixture of this shape
(`internal/ext/rc/testdata/rcSessionDto.golden.json`) is byte-identical to the consuming
repos' copies and asserted to decode in each — the guard against contract drift.

`lane` (contract v2) is the session's **current** lane — `"tui"` (an rc-tmux pane) or
`"structured"` (a native-protocol lane) — and is **always present** on every session:
managed, unmanaged, and unknown-kind rows alike. It is derived at DTO-build time from
the kind's registry entry, never stored in the tmux env; every kind in this phase
derives `"tui"`, including unknown kinds. It documents current state, not identity — a
future takeover/handoff feature (one session moved between an interactive and a
headless runner over the agent's own resume) would ride a session-level
effective-capabilities overlay layered on top, not a change to this field. Old
payloads (pre-v2 binaries) omit `lane`; a client reading one treats absent as `"tui"`.

`pending_approvals` (contract v2) is the session's currently-unresolved approval
requests — the snapshot that keeps a session actionable after the feed ring evicted (or
a hub restart lost) the `approval_request` rows that announced them. It is a
**hub-layer** field only: the one-shot `list` path never sets it (no hub running, no
approval state to report). Populated for opencode (lane-published, pending-only) and for
codex/cursor while a pane-anchor episode is open; empty otherwise. `omitempty`, so its
absence carries no meaning beyond "nothing to report." See [`needs_approval`
producers](#needs_approval-producers-per-kind) for the per-kind derivation and the
"empty `pending_approvals` is legal" note.

The `activity`, `activity_at`, and `last_message` fields are the additive **live
activity** dimension (a resident per-shed rc hub derives them). They are optional and
absent when no hub is running or the kind is unsupported:

| Field | Meaning |
|-------|---------|
| `activity` | Live work dimension, orthogonal to `state`: `working` \| `needs_input` \| `needs_approval` \| `idle` \| `unknown`. Lifecycle trumps activity — a `needs-trust`/`needs-auth`/`dead` session reports no activity. |
| `activity_at` | RFC3339 timestamp the activity was last derived/changed. |
| `last_message` | Sanitized preview of the most recent message — ANSI/control-stripped, whitespace-collapsed, truncated to ≤200 runes. |

These fields are **derived and served by the RC activity hub**, documented in full
below — including the codex/opencode message feed those previews summarize.

## The RC activity hub (`serve`)

`shed-ext-rc serve` runs the **RC activity hub**: a small, resident, per-shed daemon
that tails each rc session and exposes a loopback HTTP API. It answers the question the
lifecycle `state` cannot — *what is a usable session doing right now?* — by deriving a
live `activity` dimension (and, for codex and opencode, a normalized message feed and
gated input). Clients never reach it directly; the server's rc proxy and aggregate SSE
stream are the only paths in (see [Server surfaces](#server-surfaces)).

The hub drives the **same** tmux/pane machinery the one-shot subcommands use, so its
session list and classification are byte-identical to `list`; it only *overlays* the
live activity a one-shot exec cannot observe.

> **Loopback-only — a security invariant, not a default.** The hub binds
> `127.0.0.1:1029` and **only** `127.0.0.1`. It is unauthenticated and trusts the
> loopback: it is reachable solely through the server's `DialService` proxy (or an SSH
> forward). Binding a non-loopback interface would expose an unauthenticated control
> surface on a shed's shared bridge — never widen it. **The server-side proxy is the
> authorization boundary**; the hub itself does no authz. (On native **machines**
> there is no proxy — the loopback bind plus the operator's SSH tunnel is the
> boundary; see [the machine hub](sx.md#the-machine-hub).) The proxy also strips the
> client's `Authorization`/`Cookie` before forwarding, so the guest-local hub never sees
> server-API credentials.

### Lifecycle

- **On-demand start.** `create` ensures a hub (best-effort — a start failure never fails
  create), and the server proxy ensure-starts one when a client first reads it. Both go
  through `serve --detach`, which double-forks the daemon via `setsid` (so it survives
  the exec channel's `SIGHUP` when the spawning guest exec returns), redirects stdio to
  `~/.shed-rc-hub/hub.log`, and waits for a successful health probe before the parent
  exits. The exec therefore returns promptly with the hub up.
- **Bind-as-lock.** Binding `:1029` *is* the lock: a second `serve` that hits
  `EADDRINUSE` verifies the holder's identity over `GET /v1/health` (see below) and, if
  it is a hub, exits 0 (a redundant start); a *foreign* process squatting the port is
  reported as an error, never mistaken for a hub. The pidfile under `~/.shed-rc-hub` is
  advisory/debug only — the port bind decides ownership. (The pidfile is a **Go-hub**
  detail: the agent-hosted machine hub writes none — the daemon supervises the process
  and `/v1/health` carries the pid.)
- **Health identity.** `GET /v1/health` returns `{"app":"shed-rc-hub","version",
  "pid"}`. A bare open port proves only that *something* listens; the `app` token is what
  distinguishes a real hub from a squatter, and every start/probe path verifies it.
- **Idle exit.** The hub self-exits after **15 idle minutes with zero rc sessions**.
  Subscribers do **not** extend that window — an all-sessions-killed hub exits even with
  the aggregator still attached (it closes its SSE; the aggregator re-demands a start
  when sessions reappear). A last-chance re-check on the way out respawns the hub if a
  `create` raced the exit, so a new session is never left unmonitored.
- **Reconcile cadence.** The watch loop ticks every **2 s while ≥1 SSE subscriber is
  attached**, **10 s otherwise** (plus a best-effort `fsnotify` nudge that surfaces a
  transcript append sub-tick). So an activity transition surfaces within a couple of
  seconds while someone is watching, at low idle cost otherwise.

### API (`/v1`)

All endpoints are loopback-only and reached through the server proxy at
`/api/sheds/{name}/rc/…`.

| Method + path | Params | Returns | Errors |
|---|---|---|---|
| `GET /v1/health` | — | `{app, version, pid}` identity handshake | — |
| `GET /v1/sessions` | — | `{"sessions":[…]}` — the `list` DTO array with the live activity overlay | — |
| `GET /v1/events` | — | SSE stream (activity/session/message notifications) | — |
| `GET /v1/sessions/{slug}/messages` | `since=<seq>` (exclusive), `limit=<n≤200, default 100>` | `{"messages":[…],"truncated":bool}` | `400` bad `since`/`limit`; `404` unknown slug |
| `POST /v1/sessions/{slug}/input` | body `{"text":"…"}` (≤16 KiB) | `{"delivered":true}` | `400` invalid/unsafe/empty text; `404` unknown/gone slug; `409` not accepting; `413` body too large |
| `POST /v1/sessions/{slug}/turn` | body `{"text": string, "options": object?}` (≤16 KiB) | **live for opencode**, `409` elsewhere: `202 {"turn_id": "<opaque>"}` | `400` empty/whitespace text or malformed JSON; `404` unknown slug; `409` `not_supported` (non-opencode kinds) or `not_accepting` (no lane yet, unpinned session, or upstream failure — opencode never rejects for "busy") ; `413` body too large |
| `POST /v1/sessions/{slug}/interrupt` | body ignored (still size-capped) | **live for opencode**, `409` elsewhere: `202 {"interrupting": true}` | `404` unknown slug; `409` `not_supported` (non-opencode kinds) or `not_accepting` (no lane yet, unpinned session, or upstream failure — opencode passes through even an idle abort as success); `413` body too large |
| `POST /v1/sessions/{slug}/approvals/{id}` | body `{"decision": "allow"\|"allow_always"\|"deny"}` (≤16 KiB) | **live for opencode**, `409` elsewhere: `200 {"resolved": true, "decision": "<decision>"}` | `400` invalid decision, malformed JSON, or an `{id}` that fails the approval-id grammar (below); `404` unknown slug, or `unknown_approval` for a well-formed but unrecognized id; `409` `not_supported` (`approvals != "remote"` — every kind but opencode, including a pane-anchor `pane-*` id) or `already_resolved` for a different decision on an already-resolved id (same-decision replay is idempotent, `200`, with no second upstream POST); `413` body too large |

Errors carry a JSON envelope `{"error":"<code>","message":"…"}`. A hub-down condition is
surfaced by the proxy, not the hub — see [Hub-down degrade](#hub-down-degrade).

#### Contract-v2 verbs: `turn` / `interrupt` / `approvals/{id}`

These three routes were specified — and fully validated — before any lane implemented
them, so clients (mobile above all) could be written against a stable surface. The
**opencode** lane implements all three now, through its TUI's embedded HTTP+SSE server
(session-scoped v1 routes, addressed by the rc session's pinned opencode sessionID — see
[the WS-B scoping invariant](#session-scoping-invariant-hub-initiated-mutations) below).
Every other kind still validates the request fully and then rejects with `409
not_supported`, because its `kind_features` row advertises no verb.

**Verb liveness — which kind implements what, today:**

| kind | `turn` | `interrupt` | `approvals/{id}` |
|---|---|---|---|
| claude-rc | 409 `not_supported` | 409 `not_supported` | 409 `not_supported` |
| codex | 409 `not_supported` | 409 `not_supported` | 409 `not_supported` |
| **opencode** | **live** — 202 `{turn_id}` | **live** — 202 `{interrupting}` | **live** — 200 `{resolved, decision}` |
| cursor | 409 `not_supported` | 409 `not_supported` | 409 `not_supported` |

A verb whose capability check passes but whose session has no watcher built yet (a
brand-new opencode session, before the hub's first reconcile tick) falls to 409
`not_accepting` ("no lane is attached to this session") — genuinely reachable, not a
dead branch. An **unpinned** opencode session (a fresh, promptless TUI the hub has not
yet correlated to a conversation) also answers 409 `not_accepting`, with the message
"agent session not established yet — deliver the first prompt via the prompt/attach
path". An upstream failure (opencode's embedded server times out, errors, or answers a
non-2xx) maps to the same 409 `not_accepting`, with a coarse, generic message — the
detail (the upstream URL, which embeds the loopback port and the pinned opencode
session id) goes to the hub log only, never to the client.

**opencode defines no busy-409.** R0 reserved `turn`-while-busy and `interrupt`-with-
no-active-turn as *lane-defined* 409 `not_accepting` rejections — a lane whose native
surface refuses the verb in that state emits them, one whose surface accepts it simply
never does. opencode **natively queues/steers typed input while a turn is running**
(verified live: `prompt_async` on a busy session is accepted and renders in the TUI) and
**answers an abort on an idle session successfully** too (verified live: `abort` on an
idle session returns `200 true`, which the lane still maps to `202
{"interrupting":true}` — the hub does not second-guess the lane about what is running).
So the opencode lane defines **neither** reserved rejection: it forwards `turn` and
`interrupt` regardless of the session's merged activity. This explicitly supersedes the
"turn-while-busy → 409" / "no active turn → 409" sketch below — those codes stay
reserved for a *future* lane whose native surface actually refuses in that state; a
client must not treat a 409 as how it learns a session is busy. `activity` is that
signal.

**Handler precedence** (identical across the three verbs, and matching `POST /input`'s
precedent): body size (`413`, 16 KiB cap) → body validation (`400` `invalid_json` /
`empty_text` / `invalid_decision` / `invalid_approval_id`) → tracked-session lookup
(`404` `unknown_slug`) → capability check (`409` `not_supported`). `turn` with
empty/whitespace text is a `400`. `interrupt` reads no body — any body is ignored, but
still size-capped by the proxy. Unknown body fields are ignored; `Content-Type` is not
enforced (both match the existing `/input` handler). R0 handlers take no input mutex
and capture no pane — there is nothing to deliver — and use the same tracked-lookup
rule `GET /messages` does (`404` for an unknown slug, no re-derivation from tmux).

**409 vocabulary** (defined once here — mirrored by the `hub.go` doc comment in
`internal/ext/rc/hub_verbs.go`):

| Code | Meaning |
|---|---|
| `not_supported` | This session's kind/lane **never** supports the verb — capabilities said so, and retrying or waiting changes nothing. Every kind but opencode returns this for all three verbs. |
| `not_accepting` | The verb **is** supported but not right now — the existing `/input` vocabulary (wrong activity, recreated identity) plus the lane-specific reasons above (no lane attached yet, an unpinned opencode session, an upstream failure). `turn`-while-busy and `interrupt`-with-no-active-turn stay **reserved** codes for a lane whose native surface actually refuses in that state — opencode's does not (see above), so it never emits them. Retryable in principle. |

There are deliberately no `501`s — one envelope, one vocabulary, for every rejection. A
client that must distinguish "this server is too old to have the route at all" reads
the `contract-v2` capability feature token rather than interpreting the mux's bare
`404`.

**Success semantics**:

- `turn` → `202 {"turn_id": "<opaque>"}` — opencode's turn id is hub-generated
  (`oc-<uuid>`) since `prompt_async` answers with no body; clients must not parse it.
- `interrupt` → `202 {"interrupting": true}` (acknowledges the interrupt was
  *delivered*, not that the turn has stopped — the stop itself surfaces on the
  feed/activity stream). It cancels **generation**, not an approval gate the turn
  already surfaced: if the model had emitted a tool call that raised a permission
  request before the interrupt landed, the session stays `needs_approval` with that
  approval still pending after the interrupt is acknowledged — a client must resolve
  (or the operator must answer in the TUI) that approval to reach `idle`. Verified
  live against opencode 1.18.18.
- `approvals/{id}` → `200 {"resolved": true, "decision": "<decision>"}`. A replay of
  the **same** decision on an already-resolved id is idempotent → `200`, with **no
  second upstream POST** (the resolution is recorded synchronously the moment the
  first POST succeeds, closing the ~1-tick replay window before opencode's own
  `permission.replied` event comes back around the SSE stream). A **different**
  decision on an already-resolved id → `409 already_resolved`. An unknown (but
  well-formed) id → `404 unknown_approval`.

**Approval-id grammar** (a contract decision, not an inherited regex):
`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$` — starts alphanumeric (so `.`/`..`/`...` can
never match; path traversal is excluded by the grammar itself), allows the `.`/`:`/
`_`/`-` seen in native ids (codex call ids, ACP/opencode request ids, the pane-anchor
`pane-<n>` ids), capped at 128 characters. The same expression gates both the hub
handler and the server-side proxy path classifier — a malformed id 404s at the proxy
before it ever reaches the guest; a syntactically invalid id sent **directly** to the
hub (bypassing the proxy) is a `400 invalid_approval_id`, not a `404` — a `404` here
would wrongly imply the id was well-formed but unknown. A well-formed `pane-*` id (a
codex/cursor informational approval row) is never remotely resolvable — the kind's
`approvals` row says `tui`, so the capability check rejects it with `409 not_supported`
before any id lookup.

### Session-scoping invariant (hub-initiated mutations)

> **Normative.** Every hub-initiated **mutation** (a `POST`/write the hub sends to an
> agent's own embedded server or protocol endpoint — today, opencode's three verb
> lanes) addresses the rc session's **pinned** opencode sessionID via a
> **session-scoped route** — `POST /session/{pinned}/prompt_async`, `.../abort`,
> `.../permissions/{id}` — and **never** a global write route (`POST
> /permission/{id}/reply`, `POST /question/{id}/reply|reject`), and never
> "latest"/"newest". A verb on an **unpinned** session is a 409, never a guess: the
> three lane methods (`startTurn`/`interruptTurn`/`resolveApproval`) take **no session
> parameter** — they read the pin (`getPinned()`) internally, so no code path can
> enumerate sessions to address one.

**Global `GET` routes remain legal** — for discovery/seed only, always pin-filtered:
`GET /session/status`, `GET /permission`, `GET /question` (opencode has no session-
scoped variant of these; the watcher reads the global list and filters to the pinned
session id before folding anything), and the correlation-only `GET /session` used to
find a follow candidate before a session is pinned. A consumer of these routes either
filters to the pin, or — pre-pin — uses the result *only* to discover the pin, never to
address a mutation.

**Why this matters**: a spike confirmed the opencode global-store hazard is real and
worse than assumed — one TUI's embedded server lists sessions from **every directory**
on the machine (14 sessions across 3 unrelated project directories observed at
startup), `?scope=project` does **not** filter it, and the global permission-reply
route answers asks belonging to unrelated projects. Nothing in opencode's own API
enforces session isolation; **this invariant is what the hub adds**, and it governs
only the hub's own adapters — a different guest process talking to the same embedded
server directly is unaffected (documented, not solved; see
[Open items](../discovery/remote-agents.md)).

**Enforced structurally, tested adversarially.** The pin itself is validated to a
single safe path segment (`^[A-Za-z0-9_-]+$`, ≤256 chars) before it is ever used to
build a URL, and every interpolated path segment (the pin, the approval id) is
additionally `url.PathEscape`d — two independent layers, so a malformed or adversarial
value can neither smuggle a path traversal nor re-target another session's route. The
fake-opencode test double used by the unit tests grows a **second session** in its
store and **fails the test** on any POST to a global route or to a non-pinned session's
path; the suite asserts a verb only ever hits `{pinned}`-prefixed paths, a second rc
session pinned to the second opencode session is untouched by verbs on the first, an
unpinned watcher 409s without issuing any HTTP request at all, and seed `GET`s stay
pin-filtered. Guest e2e re-proves it live against a real opencode binary: two sessions
in one embedded-server store, steer + approve session A through the server proxy,
session B provably unchanged.

### SSE events (`GET /v1/events`)

Best-effort **notification**, not durable delivery. Each subscriber has a bounded queue
(256 frames); a slow client's overflowing queue **drops** frames rather than blocking the
broadcaster. There is no `Last-Event-ID` replay — on (re)connect a client **refetches
snapshots** (`/v1/sessions`, or `/messages?since=…`). A `: heartbeat` comment every 25 s
keeps idle streams warm through proxies.

Three envelope shapes (the same events the server aggregator re-broadcasts, with `shed`
filled in server-side):

| `event:` | `data:` | Fires when |
|---|---|---|
| `activity.changed` | `{shed, slug, activity, activity_at, state, last_message?}` | a session's *displayed* activity changes to a valid non-empty value; `last_message` is the sanitized preview at the transition (omitted for stability-only kinds) |
| `session.updated` | `{shed, slug, session}` (`session:null` on kill) | a session appears, is recreated, or its lifecycle `state` changes |
| `message.appended` | `{shed, slug, seq}` | a new feed message lands (notification only — the body comes from `/messages`, keeping fan-out tiny and drop-safe) |

`activity.changed` is **never** emitted for the suppressed (empty) activity dimension — a
transition *into* suppression (a session becoming `needs-trust`/`needs-auth`/`dead`)
rides on the `session.updated` that the state change already emits; the client drops the
activity badge from the new `state`, per the precedence rule below. The guest hub leaves
`shed` blank (it does not know the orchestrator's alias); the server always corrects it,
and the synthetic `hub.unavailable`/`shed.stopped` events are server-only — a guest hub
cannot spoof them.

### Activity dimension

`activity` is orthogonal to lifecycle `state`: `state` answers "is the session usable?",
`activity` answers "what is a usable session doing?".

| Value | Meaning |
|---|---|
| `working` | actively producing output (a JSONL turn is streaming, or the pane changed since the last capture) |
| `needs_input` | idle at the kind's prompt anchor, waiting for the operator to type |
| `idle` | quiescent with no prompt anchor visible (finished, or an anchorless kind sitting still) |
| `unknown` | a live session whose activity can't be determined yet (e.g. correlation to a JSONL file is still ambiguous) — distinct from *absent*, which means no activity dimension at all |
| `needs_approval` | the session is blocked on the operator's yes/no — see [`needs_approval` producers](#needs_approval-producers-per-kind) below for how each kind derives it, and how a client should render it (`remote` for opencode: render decision buttons; `tui` for codex/cursor: render "open the TUI") |

**Precedence rule (lifecycle trumps activity).** When the pane-derived `state` is a
blocking lifecycle state — `needs-trust`, `needs-auth`, `dead` — the *whole* activity
dimension is suppressed: `activity`, `activity_at`, **and** `last_message` are dropped
together (a bare timestamp is meaningless without its activity, and a stale
`last_message` would present pre-death context as current). Activity renders only for the
non-blocking states (`starting`/`ready`/`reconnecting`).

**Per-kind derivation.** The pane-stability engine is the **universal fallback** (every
kind gets baseline `working`/`idle`); structured JSONL tails refine it for the agents
that log:

- **Stability engine** (opencode/cursor/shell, and any kind's fallback): diffs
  consecutive pane captures, first **normalizing** each snapshot (stripping spinner
  glyphs, timers, and counter lines) so that spinner-only churn reads `idle`, not
  `working`. A pane that holds still for the quiet period (4 s) downgrades `working` →
  `idle`, or → `needs_input` **only** when the kind declares a prompt anchor the stable
  pane matches (an anchorless kind's stable pane is always `idle`).
- **codex** tails the rollout JSONL; **claude** tails the transcript JSONL (claude feeds
  *activity* only in this phase — messages are deferred). **opencode** has no JSONL to
  tail — the bare `opencode` TUI runs an embedded HTTP+SSE server on a per-session
  loopback port (recorded at `create` time), and the hub subscribes to its `/event`
  stream (plus a REST seed) as a second client, folding the same shape of
  activity-verdict + message feed as codex's JSONL fold. A correlated watcher's verdict
  **overrides** stability while it is *fresh*; for opencode, freshness also depends on
  the SSE connection's transport health (a disconnected stream is not trusted the way a
  merely-quiet file is — see [Message feed](#message-feed-codex-opencode-cursor)).

**Freshness / grace.** A settled watcher verdict (`needs_input`/`idle`) is trusted
indefinitely; a transitional verdict is fresh for 30 s since the last in-file event; a
`working` verdict gets a longer **120 s grace** (a long silent tool call must not flap to
idle). Past the grace, `working` is *demoted to conditional* — it yields to stability
**only** if stability holds a settled quiet verdict (`idle`/`needs_input`); if the pane
still churns, `working` is kept. This merge is `mergedActivity`; the input handler
re-runs the exact same merge so it can never be more permissive than the displayed
activity.

`last_message` is a sanitized one-line preview (ANSI/control-stripped,
whitespace-collapsed, ≤200 runes) extracted by the watcher; stability has no message
signal, so a stability-only session carries none.

### `needs_approval` producers per kind

Each kind's approvals surface reaches the wire through a different mechanism, matched
to what that agent actually exposes:

- **opencode** — from live events on its own protocol: `permission.asked`/
  `question.asked` open an ask (an open permission or an open question both count
  toward `needs_approval`; only permissions are addressable — see
  [`pending_approvals` is legal with a question open](#pending_approvals-may-be-empty)
  below), `permission.replied`/`question.replied`/`question.rejected` close it. This is
  an **event-bounded** verdict: `settled()` (the freshness contract) trusts it
  indefinitely while the SSE transport is healthy, exactly like `needs_input`. **Demoted
  to stability on a dead stream**: when the SSE connection is disconnected or
  heartbeat-stale, the watcher reports not-fresh and pane stability drives instead — a
  `needs_approval` derived from a wedged connection cannot outlive the evidence for it;
  it comes back the moment the stream reconnects and reseeds.
- **codex and cursor** — from a **visible-frame pane anchor**: neither agent's live
  signal carries an approval-pending event (codex's rollout JSONL persistence policy
  filters every approval-shaped record before it is ever written — the tool-call record
  itself is written *before* the approval gate, so a session blocked on approval is
  byte-identical in the log to a long-running tool call; cursor's hooks fire no
  approval-pending event at all, and a hook `allow` cannot bypass cursor's own allowlist
  prompt). The hub instead pattern-matches the tool's approval-dialog chrome — the
  option-row widget shape, never a headline alone (a headline is ordinary English an
  agent can quote back in its own prose, and it survives in the transcript after the
  dialog is answered; option rows exist only while the widget is mounted) — against the
  session's **visible terminal frame only** (`tmux capture-pane -p`, no scrollback):
  scrollback would let an answered or historical dialog wedge a false episode open
  forever. A match is debounced **two consecutive ticks** to open an episode and **two
  consecutive ticks** of no match to close it (roughly 4 s at the hub's active 2 s
  cadence), so a single missed/mid-redraw capture cannot flip the verdict. These rows
  are **informational only**: `approvals` stays `"tui"` for both kinds — the pane shows
  chrome, not a structured, remotely-answerable request, so `pending_approvals` entries
  for these ids carry no `decisions`, and `POST /approvals/{pane-id}` still 409s
  `not_supported` (the capability check rejects it before any lookup, same as any other
  `tui`-approvals kind). A resolved row may **omit `decision`** — the operator answered
  in the TUI and the hub has no way to know which way (see [the loosened `decision`
  field](#approval_request-contract-v2) below). **Known limitation, inherent to a
  pane-derived signal**: a verbatim on-screen reproduction of the dialog's exact chrome
  (e.g. `cat`ing a fixture file, or a pasted transcript that preserves the gutter and
  footer) reads as a real dialog and false-positives — no regex can tell "the widget is
  mounted" from "a perfect picture of the widget is on screen" from the pane alone. The
  blast radius is bounded: the anchor only ever sees the current visible frame, so a
  false episode clears the moment the text scrolls away, and the row is informational —
  nothing is ever auto-approved or auto-denied by either false direction.

<a id="pending_approvals-may-be-empty"></a>**`needs_approval` with an empty
`pending_approvals` is legal.** An open opencode *question* (no decision vocabulary fits
`allow`/`allow_always`/`deny`, so it is never addressable by the approvals verb — remote
question-answering is a future contract extension) drives `needs_approval` without
adding a `pending_approvals` entry. A pane-anchor kind's open episode is also **not**
guaranteed to appear there for a client reading an older snapshot — see the union rule
below. Either way, a client must not assume a non-empty `pending_approvals` accompanies
every `needs_approval` session; the correct fallback affordance is always "open the
TUI".

`pending_approvals` (the session-level snapshot) is the **union** of the lane-published
entries (opencode's addressable, pending permission asks) and the open pane-derived
episode (codex/cursor), when one is open. The two sources are disjoint in practice — a
kind's approvals are either lane-derived or pane-derived, never both — but they are
unioned rather than switched so a kind that someday has both keeps every open ask
visible.

### Message feed (codex, opencode, cursor)

The codex rollout watcher folds the JSONL turn stream, the opencode watcher folds its
HTTP/SSE `/event` stream, and the cursor watcher folds its hook-event stream (see
[Cursor hook ingestion](#cursor-hook-ingestion) below), into normalized conversation
messages, drained each tick into a per-session **ring buffer** that `GET /messages`
pages. claude sessions have a ring that simply never fills (messages deferred).

opencode's fold additionally turns a pending `question.asked` event (one with no
addressable permission id) into a display-only `status` feed row (role `system`) — e.g.
`awaiting answer: <header>` — without an `approval_request` row (questions are not
addressable — see above). `permission.asked`/`permission.replied` instead produce real
`approval_request` rows (below), since permissions ARE addressable through the
`approvals` verb.

Message shape: `{seq, ts, role, type, text, tool{name, detail}, approval}` where `role ∈
{user, assistant, tool, system}` and `type ∈ {text, tool_use, tool_result, reasoning,
status, approval_request}` (unknown native events map to a `status` row rather than
being dropped).

<a id="approval_request-contract-v2"></a>**`approval_request` (contract v2).** An
approval row: an agent asked for permission to do something. It rides `role: "tool"`
with `text` carrying a sanitized human-readable summary, `tool{name, detail}` the call
being approved (omitted on a pane-anchor row — the hub never learns which call the
dialog guards), and `approval` the machine-readable state:

```json
{
  "seq": 3,
  "ts": "2026-08-14T10:00:05Z",
  "role": "tool",
  "type": "approval_request",
  "text": "Allow running `rm -rf build/`?",
  "tool": { "name": "exec", "detail": "rm -rf build/" },
  "approval": {
    "id": "call_01HQ8Z3K.tool:2",
    "status": "pending",
    "decisions": ["allow", "allow_always", "deny"]
  }
}
```

| `approval` field | Meaning |
|---|---|
| `id` | The lane-assigned approval id — the address the `approvals/{id}` hub verb resolves for a `remote`-approvals kind. A `tui`-approvals kind's pane-anchor id (`pane-<n>`, monotonic per session) is **not** remotely resolvable — `POST /approvals/{id}` on it 409s `not_supported`. Grammar: `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$` (starts alphanumeric, `.`/`:`/`_`/`-` allowed, max 128 chars — same grammar as the `approvals/{id}` route above). |
| `status` | `pending` or `resolved`. |
| `decision` | The decision that resolved it (`allow`/`allow_always`/`deny`); empty/omitted while pending. **Also omitted on a `resolved` row when the resolution happened outside the hub** — a pane-anchor kind's dialog answered in the TUI (the hub sees only that the chrome cleared, never which button was pressed), or an opencode ask closed by a reseed after a reply the hub never observed live. A client must not assume every `resolved` row carries a `decision`. |
| `decisions` | The decisions this request accepts, advertised per request so a client renders exactly the buttons the lane will honor (a subset of `allow`/`allow_always`/`deny`). **Omitted entirely** on a pane-anchor (`tui`-approvals) row — there is nothing the hub can honor remotely, so a capability-driven client renders zero decision buttons ("open the TUI") rather than a set that would silently fail. |

A resolution is a **second** appended row with the same `id` and `status: "resolved"` —
never an edit of the first:

```json
{
  "seq": 4,
  "ts": "2026-08-14T10:00:11Z",
  "role": "tool",
  "type": "approval_request",
  "text": "Allow running `rm -rf build/`?",
  "approval": { "id": "call_01HQ8Z3K.tool:2", "status": "resolved", "decision": "allow" }
}
```

**Client folding rule.** Approval rows are an **id-keyed, last-write-wins stream**. A
client must not require seeing the `pending` row before the `resolved` one — ring
eviction (or a hub restart) can drop the earlier row entirely — and the session's
[`pending_approvals`](#json-output) snapshot is the authoritative answer to "what is
still open," independent of what the ring happens to retain.

opencode emits real, addressable `approval_request` rows from `permission.asked`/
`permission.replied`; codex and cursor emit **informational** ones from the pane-anchor
mechanism (no `tool`, no `decisions`, `decision` may be absent even resolved — see
[`needs_approval` producers](#needs_approval-producers-per-kind) above). `size()`
accounting for the ring's byte budget counts the approval's `id` + `status` + `decision`
+ every advertised `decisions` entry, alongside `text`/`tool`, so an approval-heavy feed
still honors the ring's 1 MiB cap.

**`seq` semantics.** `seq` is monotonic **per hub run**, starting at 1, and **restarts
from 1 on hub restart** (or a session recreate). `since` is **exclusive**. Two
cursor-misalignment cases return `"truncated": true`, both meaning *refetch from
scratch*:

- `since` predates the ring's earliest retained message (drop-oldest discarded messages
  the client never saw);
- `since` points **beyond** the current tail — the cursor came from a previous
  incarnation (restarted `seq`), so a poll-only client would otherwise sit on empty pages
  forever. A client that sees a `seq` **lower** than one it already holds does a full
  refetch on the same signal.

**Caps + sanitization.** Each message's `text` (and each `tool.detail`) is sanitized
(ANSI escapes and non-whitespace control chars stripped — but **newlines and internal
structure preserved**, unlike the one-line `last_message`) and capped at **8 KiB**
(`…[truncated]` marker appended). The per-session ring is bounded to **500 messages AND
1 MiB of text**, dropping oldest first.

**Sensitive-data / trust posture.** Treat the feed as **same-trust as the pane itself**:
`tool.detail` carries raw command lines and tool outputs. All forwarded payload fields
(slug/activity/last_message/seq/message bodies) are **guest-controlled** — clients must
treat them as untrusted.

**History-read-through-gating policy (an intended asymmetry).** Message history stays
**readable** even while a blocking lifecycle state gates the *activity* dimension and
*input* posting. The ring holds pre-gate content the operator already saw on the pane;
this is a loopback-only surface behind the server's authz boundary; and suppressing it
would only hide the context a client needs to render the "session died mid-conversation"
view. So `GET /messages` returns content for a `dead`/`needs-auth` session even though
that session reports no `activity`.

### Input (`POST /input`)

Gated feed input is **codex- and cursor-only** now (`kind_features.input == "gated"`;
claude-rc keeps TUI-only `post_input` with no feed input at all). Delivery reuses the
shared prompt path (validation + bracketed paste), never a duplicate tmux path.

**Gating.** Under a per-slug mutex, immediately before sending the hub **re-captures the
pane and re-derives state**, and accepts only when the session is genuinely waiting: a
fresh watcher `needs_input` is accepted outright; otherwise the **degraded-path policy**
applies — accept only if the kind's prompt anchor is visible on the *fresh* pane (this is
what keeps input possible when a JSONL tail breaks, and what closes the lookup→lock
race). A merged `working` verdict (including an expired-working turn) is always rejected,
as is a session an open approval (lane-derived `needs_approval`, or a matching pane
approval anchor on the *fresh* pane) currently owns — typed input must never land on an
approval dialog by accident. A killed-and-recreated slug is caught by an identity guard
(`id`/`created_at` must still match).

**Statuses:** `400` invalid/unsafe/empty text · `404` unknown or gone slug · `409` not
accepting (wrong activity, an open approval, recreated identity, or a non-gated kind) ·
`413` body over 16 KiB.

**opencode's `/input` now 409s.** `input` moved from `gated` to `turn` for opencode (§
[`kind_features` matrix](#kind_features-matrix) above): `input` is single-valued, so
`turn` **replaces** `gated` rather than layering on top of it. `POST
/v1/sessions/{slug}/input` on an opencode session now falls through to the non-gated
"this kind has no feed input" `409` — the same rejection a claude-rc session already
gets — because the lane's steering surface is the `turn` verb, not `/input`. **This is a
deliberate wire behavior break** for any hub client that was posting to `/input` for
opencode; no shipped client did (first-party consumers move in lockstep). The
create/prompt kickoff path (`post_input`) is unaffected — it still delivers a fresh
session's first prompt; the `turn` verb covers every steer after that.

### Correlation (session → JSONL)

The hub pins each watchable session to its agent's JSONL file by **cwd + a created-at
window (±60 s)**, pinned by **inode**:

- **codex** matches rollout files under `~/.codex/sessions`; **claude** derives the
  transcript dir from the cwd encoding. On a unique match the hub does a bounded
  catch-up read (so current activity is known immediately) and **back-writes**
  `SHED_RC_AGENT_SESSION=<id>` into the tmux env (an additive key; `SHED_RC_V` stays 2)
  so a hub restart re-correlates exactly.
- **Ambiguity** within the window (>1 candidate) → the newest is followed *append-only*,
  activity stays `unknown`, and the id is **not** back-written until the first in-file
  event confirms the pick (a wrong pin would otherwise become permanent).
- Watchers stop when the tmux session disappears; a file truncation / inode swap resets
  and re-reads; new dated subdirs are handled (fsnotify is non-recursive). A session
  whose file never appears stops re-scanning after a bounded retry budget.

### Correlation (opencode: session → SSE)

opencode has no JSONL file to correlate against — it creates its conversation session
only on the **first prompt** (not at TUI start), so a create-time window match would
routinely expire before anything exists to match. Instead, the opencode watcher
correlates asynchronously, entirely from its own `/event` stream:

- It subscribes to the session's per-port `/event` stream first, then seeds via REST —
  so no event is lost in the gap between subscribe and seed.
- A trusted pin comes **only** from a port-local SSE event on the watcher's own stream
  (never from `GET /session`, which reads the shared opencode DB and can return other
  sessions/servers' history): the first **root** session (no parent) whose canonical
  directory matches the rc session's workdir. Once pinned, the id is back-written to
  `SHED_RC_AGENT_SESSION` (same as the JSONL path) so a hub restart re-correlates
  exactly.
- A fresh, prompt-less opencode TUI has no session yet and stays watchable indefinitely
  — correlation does not consume a retry budget waiting for the first prompt.
- On reconnect (SSE drop, hub restart) the watcher re-subscribes, re-seeds
  (`/session/{id}/message`, `/session/status`, `/permission`, `/question`), and replays
  buffered live events; feed emission is deduped so a reseed never double-emits a
  message.

### Correlation (cursor: hooks, not a JSONL pin)

cursor-agent has no protocol the hub can subscribe to and no server-computed identity to
correlate against ahead of time — the pin instead arrives *inside* the hook payloads
themselves (a hook's `session_id`), so there is nothing to search for and no retry
budget to spend: the cursor watcher is push-fed, not pulled. The **first** hook event's
`session_id` pins the session (back-written to `SHED_RC_AGENT_SESSION`, same as the
JSONL/SSE paths); a *different* `session_id` arriving later re-pins (the operator
switched chats inside the same TUI — a status row notes the switch, since the session is
scoped to whatever conversation the TUI currently shows). `sessionStart` is not required
to establish the pin (it does not fire on `cursor-agent --resume`).

### Cursor hook ingestion

cursor-agent's own [user-configured
hooks](https://docs.cursor.com/cli/hooks) are the **only** live signal cursor produces
at all — its transcript JSONL carries user/assistant lines only (no tool results, no
ids, no timestamps, and it lags mid-turn), so tool output exists nowhere else. The hub
makes itself a hook consumer instead of tailing anything:

- **Preseed** (best-effort, like every agent's preseed — a failure costs the session its
  feed, never its create): `~/.shed-rc-hub/cursor-hook.sh` (hub-owned, 0755, rewritten on
  every create) relays one hook event's raw stdin payload to the hub; `~/.cursor/hooks.json`
  is **merged, never clobbered** (existing entries preserved; the hub's entry appended once
  per event, matched by script path) to wire it to `sessionStart`, `beforeSubmitPrompt`,
  `preToolUse`, `afterShellExecution`, `afterFileEdit`, `postToolUse`, `postToolUseFailure`,
  `afterAgentResponse`, `stop`, `sessionEnd`. **Foreign-device guard**: if `~/.cursor` sits
  on a different filesystem than `$HOME` (a VirtioFS/9P host auth mount), the `hooks.json`
  half is **skipped** (with a hub-log note) — writing hook config into a mounted-through host
  cursor setup would reference a script that does not exist there and fire on every local
  `cursor-agent` run forever. The paired guidance: mount cursor auth at **`~/.config/cursor`
  only** (see [Configuration reference](../reference/configuration.md#mounts)) and
  leave `~/.cursor` guest-local so hooks work.
- **The hook script is deliberately mute.** cursor-agent reads a hook's stdout as a
  **verdict** (permission decisions, prompt rewrites), so the script always exits `0`
  with **empty** stdout regardless of the hub's reachability — a hub that is down, slow,
  or absent must change nothing about how the agent runs (verified live: even a hook
  `allow` cannot bypass cursor's own allowlist prompt, so there was never a bypass to
  accidentally grant). `curl --connect-timeout 1 --max-time 2 --noproxy '*'` bounds the
  cost per event and refuses any `http_proxy` in the environment (the hub is loopback-only
  — honoring a proxy would exfiltrate prompts and command output off-box).
- **`POST /v1/ingest/cursor?slug=<slug>&event=<hookEvent>`** (loopback only) is the
  receiving route. It is a **guest-internal** surface — the caller is a process *inside*
  the shed (the hook script), not the server's proxy — and is deliberately **not** on the
  server proxy's allowlist (`internal/api/rchub.go`): nothing outside the shed has any
  business injecting a session's feed. A proxy test pins that `/rc/v1/ingest/…` is
  rejected before any dial. It carries its **own 256 KiB body cap** (not the 16 KiB cap
  every other hub POST shares) because `afterShellExecution.output` routinely exceeds
  16 KiB for build-style commands and is the feed's only source of tool output; the
  ring's existing per-field 8 KiB cap still applies once the event is folded, so the
  larger ingest cap buys fidelity only at the ingest hop. An oversized payload is a `413`
  and the event is simply dropped — the session is otherwise unaffected.
- **What it mutates.** A hook event, once accepted, can update three things in the same
  request: the session's **feed/activity** (folded into a normalized message + the
  activity verdict — see the fold mapping below), the tmux session's **environment** (the
  `SHED_RC_AGENT_SESSION` pin back-write on first correlation), and — for `beforeSubmitPrompt`
  when the session is otherwise idle — it can **relax the input gate** the same way a
  fresh `needs_input` watcher verdict would. None of this is a privilege escalation: a
  guest process that can fire a cursor-agent hook already has full tmux control over the
  session (it could `send-keys` directly), so ingest is a convenience channel within
  existing guest trust, not a new trust boundary.
- **Fold mapping**: `beforeSubmitPrompt` → user feed row + `working` · `preToolUse` →
  `tool_use` row + `working` · `afterShellExecution` → `tool_result` row (the command's
  actual output) · `afterFileEdit` → `tool_result` row (path + edit count) ·
  `postToolUseFailure` → `status` row · `afterAgentResponse` → assistant feed row
  (sanitized/capped) + `last_message` · `stop` → settled `needs_input`. `postToolUse` is
  wired for its **counter**, not its payload — it is what brings the open-tool-call count
  back down for a Read/Grep/Glob-class tool that fires no `afterShellExecution`/
  `afterFileEdit`. `needs_approval` for cursor is **not** derived from hooks (no
  approval-pending hook event exists) — see [`needs_approval`
  producers](#needs_approval-producers-per-kind) above.
- **Pre-watcher window.** A hook can fire before the hub's first reconcile tick builds the
  session's watcher (`shed attach --kind cursor --prompt …` delivers its kickoff prompt
  within about a second of create). The ingest handler holds a **bounded per-slug
  pre-watcher queue** (32 events / 256 KiB total) that the watcher drains on construction,
  so the kickoff prompt's `beforeSubmitPrompt` is never lost to the create→first-tick gap;
  a queue for a slug that never grows a watcher within 60 s is dropped wholesale.

### Server surfaces

The hub is exposed to clients by two server endpoints (advertised as the `rc-proxy` and
`rc-events` feature tokens on `GET /api/info` and `GET /api/overview`):

- **`/api/sheds/{name}/rc/*`** — a reverse proxy into the shed's hub over
  `backend.DialService(shed, 1029)`, with a **strict method/path allowlist** (`GET`
  sessions/events/messages; `POST` input/turn/interrupt/approvals; the `{slug}` is
  pattern-validated on every route, and the approvals route additionally validates
  `{id}` against the same approval-id grammar the hub handler re-checks, so no
  traversal reaches the proxied path on either wildcard), SSE flushing, hop-by-hop
  header stripping, bounded response bodies, and control-scope auth. It
  **ensure-starts** the hub at most once per shed (singleflight) behind a **circuit
  breaker** (3 failed starts in 5 min → 503 for the window, no exec storm).
- **`GET /api/rc/events`** — a **demand-driven** aggregate SSE stream across every shed:
  zero connected clients ⇒ zero upstream hub connections; the first client opens one
  upstream per shed that is running and has rc sessions. An upstream drop yields a
  synthetic `hub.unavailable` + exponential backoff (max 30 s); a stopped/deleted shed
  yields `shed.stopped`. Per-client buffered channel with drop-on-overflow, GET-only,
  control scope.

Session **listings** are enriched cheaply too: `shed-ext-rc list` consults an
*already-running* hub with a ~200 ms deadline for activity (it never *starts* one), with
instant fallback to today's hub-less behavior.

### Hub-down degrade

The hub binds loopback only. `DialService` routes through the guest agent's vsock TCP
proxy on **both** VZ and Firecracker (the agent dials the target on `127.0.0.1`), so the
loopback hub is reachable on both backends — there is no backend-structural degrade.
(Binding `0.0.0.0` is ruled out by the security invariant regardless.)

The proxy still returns **503 `RC_HUB_UNAVAILABLE`** when the hub genuinely isn't
answering: the hub hasn't started yet, it crashed, or the image predates the hub binary.
In that case listings carry no activity fields and clients hide watch/activity
affordances (a clean feature-degrade). Clients key feature-degrade off the
`RC_HUB_UNAVAILABLE` code.

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
