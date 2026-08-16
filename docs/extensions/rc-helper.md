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
    "opencode": { "post_input": true, "approvals": "tui", "watch": true, "input": "gated", "feed": "messages", "interrupt": false, "attach": "tmux" },
    "cursor": { "post_input": true, "approvals": "tui", "feed": "activity", "interrupt": false, "attach": "tmux" }
  }
}
```

| Field | Meaning |
|-------|---------|
| `rc_version` | Capability/protocol version. Bumped when the capability shape or a feature contract changes; **not** tied to `SHED_RC_V`. |
| `kinds` | Every kind this binary offers (order matches the pinned wire contract). |
| `agents` | Per-tool install probe (`command -v` + `--version`, 2 s budget). `version` omitted when not installed. |
| `features` | Stable feature tokens — `generic-perm` (the `default`/`auto`/`skip` tri-state), `plan-stdin`, `prompt-b64`, `serve` (the on-demand rc activity hub), `activity` (the live activity dimension), `messages` (the codex/opencode message feed + gated input endpoints), `contract-v2` (the v2 wire contract: `lane` on every session DTO, the `feed`/`interrupt`/`attach` hints in `kind_features`, the `turn`/`interrupt`/`approvals` hub verbs — routed and fully specified, but answering `409 not_supported` until a lane implements them — the `approval_request` feed row, and `pending_approvals` on the session). A token is appended in the same change that ships its feature; `contract-v2` is a client's **route-existence** check — a server without it may 404 the new verbs at the mux, so a client reads the token instead of interpreting a bare 404. |
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
| `post_input` | A typed line can be delivered to the session's pane (the prompt/attach kickoff path). **Not deprecated** — nothing in contract v2 supersedes it. |
| `approvals` | Where approvals are answered: `tui` (in the terminal) for every kind in this phase. Documented future value `remote` — the predicate for the `approvals` hub verb. |
| `watch` | **Deprecated** by `feed` (superseded, not removed): retained until clients migrate. The producer holds `watch == (feed == "messages")` in lockstep, so a v1 client reading `watch` and a v2 client reading `feed` see the same thing. Absent-field fallback: a client that only knows `watch` should keep using it. |
| `input` | Feed-input posting mode: `gated` (`POST …/input` accepted only while the session is waiting) or `""` (no feed input — the TUI-only `post_input` path still applies). Documented future value `turn`, denoting a lane that takes whole turns via the `turn` verb. |
| `feed` | What the hub can stream for the kind: `messages` (a normalized conversation feed — `GET …/messages` + `message.appended`), `activity` (the activity dimension only — no message feed), or `none` (no hub signal at all). Supersedes `watch`. |
| `interrupt` | The `turn/interrupt` verb is supported. `false` for every kind in this phase. |
| `attach` | How a terminal reaches the session: `tmux` (attach to the rc-tmux session), `native-remote` (the agent's own remote surface), or `none`. |

Normative R0 matrix (exhaustive — pinned by `capabilities_test.go`):

| kind | post_input | approvals | watch | input | feed | interrupt | attach |
|---|---|---|---|---|---|---|---|
| claude-rc | true | tui | false | "" | activity | false | tmux |
| codex | true | tui | true | gated | messages | false | tmux |
| opencode | true | tui | true | gated | messages | false | tmux |
| cursor | true | tui | false | "" | activity | false | tmux |

(`feed:"activity"` for claude-rc/cursor because the hub's stability/transcript engines
already derive the activity dimension for them; `"none"` is reserved for kinds with no
hub signal at all.)

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
approval state to report), and it is **always empty in this phase** — nothing produces
approvals yet. `omitempty`, so its absence carries no meaning beyond "nothing to
report."

The `activity`, `activity_at`, and `last_message` fields are the additive **live
activity** dimension (a resident per-shed rc hub derives them). They are optional and
absent when no hub is running or the kind is unsupported:

| Field | Meaning |
|-------|---------|
| `activity` | Live work dimension, orthogonal to `state`: `working` \| `needs_input` \| `idle` \| `unknown` (the value `needs_approval` is reserved in the wire contract but not produced yet). Lifecycle trumps activity — a `needs-trust`/`needs-auth`/`dead` session reports no activity. |
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
> authorization boundary**; the hub itself does no authz. The proxy also strips the
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
  advisory/debug only — the port bind decides ownership.
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
| `POST /v1/sessions/{slug}/turn` | body `{"text": string, "options": object?}` (≤16 KiB) | *(reserved)* `202 {"turn_id": "<opaque>"}` | `400` empty/whitespace text or malformed JSON; `404` unknown slug; `409` `not_supported` (every kind, this phase) or `not_accepting` (busy — reserved); `413` body too large |
| `POST /v1/sessions/{slug}/interrupt` | body ignored (still size-capped) | *(reserved)* `202 {"interrupting": true}` | `404` unknown slug; `409` `not_supported` (every kind, this phase) or `not_accepting` (no active turn — reserved); `413` body too large |
| `POST /v1/sessions/{slug}/approvals/{id}` | body `{"decision": "allow"\|"allow_always"\|"deny"}` (≤16 KiB) | *(reserved)* `200 {"resolved": true, "decision": "<decision>"}` | `400` invalid decision, malformed JSON, or an `{id}` that fails the approval-id grammar (below); `404` unknown slug, or `unknown_approval` for a well-formed but unrecognized id (reserved); `409` `not_supported` (every kind, this phase) or `already_resolved` for a different decision on an already-resolved id (reserved — same-decision replay is idempotent, `200`); `413` body too large |

Errors carry a JSON envelope `{"error":"<code>","message":"…"}`. A hub-down condition is
surfaced by the proxy, not the hub — see [Hub-down degrade](#hub-down-degrade).

#### Contract-v2 verbs: `turn` / `interrupt` / `approvals/{id}`

These three routes exist **now** so clients (mobile above all) can be written against a
stable surface, but **no lane implements them in this phase**: every handler validates
fully and then rejects with `409 not_supported`, because no kind's `kind_features` row
advertises the verb (`input == "turn"` / `interrupt == true` / `approvals == "remote"`).
The success shapes above are pinned by the contract now — schema-tested, unemitted —
so a later lane implementation cannot recontract clients that already coded against
them.

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
| `not_supported` | This session's kind/lane **never** supports the verb — capabilities said so, and retrying or waiting changes nothing. Every kind returns this for all three verbs today. |
| `not_accepting` | The verb **is** supported but not right now (the existing `/input` vocabulary: wrong activity, recreated identity; and, once a lane lands, a `turn` while a turn is running, or an `interrupt` with no active turn). Retryable in principle. |

There are deliberately no `501`s — one envelope, one vocabulary, for every rejection. A
client that must distinguish "this server is too old to have the route at all" reads
the `contract-v2` capability feature token rather than interpreting the mux's bare
`404`.

**Success semantics (pinned now, unemitted in R0)**:

- `turn` → `202 {"turn_id": "<opaque>"}` (lane-assigned; clients must not parse it).
  Turn-while-busy → `409 not_accepting`.
- `interrupt` → `202 {"interrupting": true}` (acknowledges the interrupt was
  *delivered*, not that the turn has stopped — the stop itself surfaces on the
  feed/activity stream). No active turn → `409 not_accepting`.
- `approvals/{id}` → `200 {"resolved": true, "decision": "<decision>"}`. A replay of
  the **same** decision on an already-resolved id is idempotent → `200`. A
  **different** decision on an already-resolved id → `409 already_resolved`. An
  unknown (but well-formed) id → `404 unknown_approval`.

**Approval-id grammar** (a contract decision, not an inherited regex):
`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$` — starts alphanumeric (so `.`/`..`/`...` can
never match; path traversal is excluded by the grammar itself), allows the `.`/`:`/
`_`/`-` seen in native ids (codex call ids, ACP/opencode request ids), capped at 128
characters. The same expression gates both the hub handler and the server-side proxy
path classifier — a malformed id 404s at the proxy before it ever reaches the guest; a
syntactically invalid id sent **directly** to the hub (bypassing the proxy) is a `400
invalid_approval_id`, not a `404` — a `404` here would wrongly imply the id was
well-formed but unknown.

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
| `needs_approval` | **reserved** in the wire contract; no derivation produces it in this phase and no client keys off it (approval handling stays "open the TUI") |

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
  merely-quiet file is — see [Message feed](#message-feed-codex-opencode)).

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

### Message feed (codex, opencode)

The codex rollout watcher folds the JSONL turn stream, and the opencode watcher folds
its HTTP/SSE `/event` stream, into normalized conversation messages, drained each tick
into a per-session **ring buffer** that `GET /messages` pages. claude sessions have a
ring that simply never fills (messages deferred).

opencode's fold additionally turns a pending `permission.asked`/`question.asked` event
into a display-only `status` feed row (role `system`) — e.g. `awaiting approval: bash —
<patterns>` — **without** changing the `activity` dimension (it stays
`working`/`needs_input`; the wire-reserved `needs_approval` value is still not
produced — see [Activity dimension](#activity-dimension)). There is no retraction event;
the row is simply superseded by whatever the feed emits next.

Message shape: `{seq, ts, role, type, text, tool{name, detail}, approval}` where `role ∈
{user, assistant, tool, system}` and `type ∈ {text, tool_use, tool_result, reasoning,
status, approval_request}` (unknown native events map to a `status` row rather than
being dropped).

**`approval_request` (contract v2).** An approval row: an agent asked for permission to
do something. It rides `role: "tool"` with `text` carrying a sanitized human-readable
summary, `tool{name, detail}` the call being approved, and `approval` the
machine-readable state:

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
| `id` | The lane-assigned approval id — the address the `approvals/{id}` hub verb resolves. Grammar: `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$` (starts alphanumeric, `.`/`:`/`_`/`-` allowed, max 128 chars — same grammar as the `approvals/{id}` route above). |
| `status` | `pending` or `resolved`. |
| `decision` | The decision that resolved it (`allow`/`allow_always`/`deny`); empty/omitted while pending. |
| `decisions` | The decisions this request accepts, advertised per request so a client renders exactly the buttons the lane will honor (a subset of `allow`/`allow_always`/`deny`). |

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

Nothing produces `approval_request` rows in this phase (no lane emits approvals yet) —
the shape is contracted now so a lane can start emitting it without a client-side
recontract. `size()` accounting for the ring's byte budget counts the approval's `id` +
`status` + `decision` + every advertised `decisions` entry, alongside `text`/`tool`, so
an approval-heavy feed still honors the ring's 1 MiB cap.

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

Gated feed input is **codex- and opencode-only** in this phase (`kind_features.input ==
"gated"`; other kinds keep TUI-only `post_input`). opencode reuses the *existing* tmux
prompt-delivery path — there is no opencode-side REST submission. Delivery reuses the
shared prompt path (validation + bracketed paste), never a duplicate tmux path.

**Gating.** Under a per-slug mutex, immediately before sending the hub **re-captures the
pane and re-derives state**, and accepts only when the session is genuinely waiting: a
fresh watcher `needs_input` is accepted outright; otherwise the **degraded-path policy**
applies — accept only if the kind's prompt anchor is visible on the *fresh* pane (this is
what keeps input possible when a JSONL tail breaks, and what closes the lookup→lock
race). A merged `working` verdict (including an expired-working turn) is always rejected.
A killed-and-recreated slug is caught by an identity guard (`id`/`created_at` must still
match).

**Statuses:** `400` invalid/unsafe/empty text · `404` unknown or gone slug · `409` not
accepting (wrong activity, recreated identity, or a non-gated kind) · `413` body over
16 KiB.

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
