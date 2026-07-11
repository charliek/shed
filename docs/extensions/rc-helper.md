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
version (currently **3**), decoupled from `SHED_RC_V` (metadata schema, still **2**).

```json
{
  "rc_version": 3,
  "kinds": ["claude-broker", "claude-rc", "codex", "opencode", "cursor", "shell"],
  "agents": {
    "claude": { "installed": true, "version": "2.1.206" },
    "codex":  { "installed": false }
  },
  "features": ["generic-perm", "plan-stdin", "prompt-b64", "serve", "activity", "messages"],
  "kind_features": {
    "codex": { "post_input": true, "approvals": "tui", "watch": true, "input": "gated" }
  }
}
```

| Field | Meaning |
|-------|---------|
| `rc_version` | Capability/protocol version. Bumped when the capability shape or a feature contract changes; **not** tied to `SHED_RC_V`. |
| `kinds` | Every kind this binary offers (order matches the pinned wire contract). |
| `agents` | Per-tool install probe (`command -v` + `--version`, 2 s budget). `version` omitted when not installed. |
| `features` | Stable feature tokens — `generic-perm` (the `default`/`auto`/`skip` tri-state), `plan-stdin`, `prompt-b64`, `serve` (the on-demand rc activity hub), `activity` (the live activity dimension), `messages` (the codex message feed + gated input endpoints). A token is appended in the same change that ships its feature. |
| `kind_features` | Per-kind UI hints. `post_input` = a typed line can be delivered to the pane; `approvals` = where approvals happen (v1 agents are TUI-only → `tui`); `watch` = the hub produces a live message feed for the kind (`GET …/messages` + the `message.appended` event); `input` = feed-input posting mode (`gated` = `POST …/input` accepted only while the session is waiting, absent = no feed input). `watch`/`input` are codex-only in this phase. `claude-broker` and `shell` are omitted. |

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

These fields are **derived and served by the RC activity hub**, documented in full
below — including the codex message feed those previews summarize.

## The RC activity hub (`serve`)

`shed-ext-rc serve` runs the **RC activity hub**: a small, resident, per-shed daemon
that tails each rc session and exposes a loopback HTTP API. It answers the question the
lifecycle `state` cannot — *what is a usable session doing right now?* — by deriving a
live `activity` dimension (and, for codex, a normalized message feed and gated input).
Clients never reach it directly; the server's rc proxy and aggregate SSE stream are the
only paths in (see [Server surfaces](#server-surfaces)).

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

Errors carry a JSON envelope `{"error":"<code>","message":"…"}`. A hub-down condition is
surfaced by the proxy, not the hub — see [FC degrade](#firecracker-degrade).

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
| `activity.changed` | `{shed, slug, activity, activity_at, state}` | a session's *displayed* activity changes to a valid non-empty value |
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
  *activity* only in this phase — messages are deferred). A correlated watcher's verdict
  **overrides** stability while it is *fresh*.

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

### Message feed (codex)

The codex rollout watcher folds the JSONL turn stream into normalized conversation
messages, drained each tick into a per-session **ring buffer** that `GET /messages`
pages. claude sessions have a ring that simply never fills (messages deferred).

Message shape: `{seq, ts, role, type, text, tool{name, detail}}` where `role ∈
{user, assistant, tool, system}` and `type ∈ {text, tool_use, tool_result, reasoning,
status}` (unknown native events map to a `status` row rather than being dropped).

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

Gated feed input is **codex-only** in this phase (`kind_features.input == "gated"`; other
kinds keep TUI-only `post_input`). Delivery reuses the shared prompt path (validation +
bracketed paste), never a duplicate tmux path.

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

### Server surfaces

The hub is exposed to clients by two server endpoints (advertised as the `rc-proxy` and
`rc-events` feature tokens on `GET /api/info` and `GET /api/overview`):

- **`/api/sheds/{name}/rc/*`** — a reverse proxy into the shed's hub over
  `backend.DialService(shed, 1029)`, with a **strict method/path allowlist** (only `GET`
  sessions/events/messages and `POST` input; the `{slug}` is pattern-validated so no
  traversal reaches the proxied path), SSE flushing, hop-by-hop header stripping, bounded
  response bodies, and control-scope auth. It **ensure-starts** the hub at most once per
  shed (singleflight) behind a **circuit breaker** (3 failed starts in 5 min → 503 for
  the window, no exec storm).
- **`GET /api/rc/events`** — a **demand-driven** aggregate SSE stream across every shed:
  zero connected clients ⇒ zero upstream hub connections; the first client opens one
  upstream per shed that is running and has rc sessions. An upstream drop yields a
  synthetic `hub.unavailable` + exponential backoff (max 30 s); a stopped/deleted shed
  yields `shed.stopped`. Per-client buffered channel with drop-on-overflow, GET-only,
  control scope.

Session **listings** are enriched cheaply too: `shed-ext-rc list` consults an
*already-running* hub with a ~200 ms deadline for activity (it never *starts* one), with
instant fallback to today's hub-less behavior.

### Firecracker degrade

The hub binds loopback only, and on Firecracker `DialService` reaches the VM's **bridge
IP**, not loopback — so the hub is unreachable there (binding `0.0.0.0` is ruled out by
the security invariant). On FC the proxy returns **503 `RC_HUB_UNAVAILABLE`**, listings
carry no activity fields, and clients hide watch/activity affordances (a clean
feature-degrade). Clients key feature-degrade off the `RC_HUB_UNAVAILABLE` code. FC
parity (a vsock TCP-proxy like VZ) is a tracked follow-up.

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
