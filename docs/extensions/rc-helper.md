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
and reads them back identically:

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
| `create --kind <k> --name <display> [--slug s] [--workdir d] [--created-by t/v] [--target label] [--wait] [--interactive-shell] [--prompt-stdin \| --plan-stdin [--prompt-b64 <b64>]] [--permission-mode <m> \| --skip]` | Resolve the workdir (`$SHED_WORKSPACE` default), pre-seed claude trust + onboarding for `claude-*` kinds, and `tmux new-session` with the `SHED_RC_*` env. Non-blocking by default. With `--wait`, poll to live, auto-accept trust (and the bypass-mode dialog for `--skip`), settle, and deliver the kickoff — see [Delivering a kickoff](#delivering-a-kickoff-liveness-settle-and-the-control-gate). `--permission-mode`/`--skip` set the autonomy posture — see [Permission modes](#permission-modes). Prints the [session DTO](#json-output). |
| `list` | Print `{"rc_sessions":[…],"capabilities":{…}}` — every `rc-*` session's DTO plus the embedded [capabilities](#capabilities) block (one exec feeds both). |
| `capabilities` | Print the [capabilities](#capabilities) payload standalone (kinds, per-agent install/version, features, per-kind hints). |
| `probe --slug <s>` | Print one session DTO (state + url). Read-only. |
| `accept-trust --slug <s>` | Re-capture the pane; if claude's workspace-trust dialog is showing, send `Enter`. |
| `prompt --slug <s> [--session-id <uuid>]` | Deliver a single line (read from **stdin**) to a live session, refusing if a trust/bypass dialog is up — see [Delivering a kickoff](#delivering-a-kickoff-liveness-settle-and-the-control-gate). `--session-id` guards against a killed-and-recreated `rc-<slug>`. |
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
per-agent permission mapping and trust/preseed behavior live in one registry table
(`internal/ext/rc/agents.go`).

**Status.** `claude-rc`, `codex`, and `cursor` no longer have a lifecycle classifier or
a derived activity signal in this binary (`charliek/shed#321`, `#322`, `#324`): every
session this binary reports carries **liveness only** (see [`state`](#json-output)
below). A **machine** session of the same kind — a native host running
`roost-session`, not a shed — gets its status from **roost** instead, over roost's own
protocol, never through `shed-ext-rc`; a shed row gets the same treatment once roost
reaches the guest (S5). The claude.ai remote-control URL is unaffected: `url` is still
lifted out of the pane for `claude-rc`/`claude-broker` because it is **control** (the
address a person opens to drive the session), not status.

**Unknown-kind policy.** A reader that sees a `SHED_RC_KIND` it doesn't recognize
(e.g. a session created by a newer client) **preserves the raw string** and renders it
neutrally — name + state only, no kind-specific affordances and no synthetic claude URL.
It does not fall back to `claude-broker`. A client decoding an unrecognized `state`
string (forward compatibility with a future value) treats it as `starting`.

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

### Delivering a kickoff: liveness, settle, and the control gate

`create --wait` and the one-shot `prompt` verb both write directly into a live
session's pane — there is no classifier standing between "the session exists" and
"type here" any more (S2, `charliek/shed#324`).

**`--wait` waits for liveness, then settles.** The poller returns as soon as a pane
capture succeeds (that is the whole of `ready` now — see [`state`](#json-output)) and,
when there is a kickoff to deliver, keeps polling for a further **settle window (5 s)**
measured from the later of the first successful capture and the last *successful*
control keystroke — a trust/bypass dialog accepted late pushes the window out, but a
keystroke that failed does not, since nothing changed on screen. One more short,
content-blind sleep (1 s) follows before the line is typed, so a kickoff lands **at
least 6 s** after liveness, bounded by the overall 20 s `--wait` timeout — a session
whose settle would cross the deadline is delivered at the deadline rather than not at
all. This is a fixed delay, not a heuristic about screen content: the honest interim
until roost's provider script owns kickoff end-to-end (S4).

**The delivery gate is control, not status.** Immediately before typing, both
`--wait` and `prompt` re-capture the pane and refuse if claude's workspace-trust
prompt, claude's one-time bypass-acceptance dialog, or codex's directory-trust prompt
is still showing — a line typed into one of those answers it by accident. The check is
**not** kind-gated (every matcher is consulted for every kind, since a look-alike
phrase costs one retry and a missed dialog costs a run) and is the **same** helper both
delivery paths call, so the two can never diverge on what counts as "still showing a
dialog."

**Named residual hazard, accepted until S4/A4.** Past that control gate, delivery is
**terminal input into a live session** — the same thing typing at the attached
terminal does. With `state` reduced to liveness, nothing here can tell a claude.ai
login screen, a codex `auth` prompt, or an agent's own approval modal from an ordinary
composer: only the three dialogs named above are checked. A kickoff or a `prompt`
delivered while one of those *other* screens is up is typed into it. This is a known,
documented trade-off, not an oversight — accepted for as long as shed derives no richer
status than liveness. S4 (the roost provider script owning kickoff) and A4 (the
opencode crate) are what eventually give this verb real state to gate on again.

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
    "claude-rc": { "post_input": true, "approvals": "tui", "feed": "none", "interrupt": false, "attach": "tmux" },
    "codex": { "post_input": true, "approvals": "tui", "feed": "none", "interrupt": false, "attach": "tmux" },
    "opencode": { "post_input": true, "approvals": "remote", "watch": true, "input": "turn", "feed": "messages", "interrupt": true, "attach": "tmux" },
    "cursor": { "post_input": true, "approvals": "tui", "feed": "none", "interrupt": false, "attach": "tmux" }
  }
}
```

| Field | Meaning |
|-------|---------|
| `rc_version` | Capability/protocol version. Bumped when the capability shape or a feature contract changes; **not** tied to `SHED_RC_V`. |
| `kinds` | Every kind this binary offers (order matches the pinned wire contract). |
| `agents` | Per-tool install probe (`command -v` + `--version`, 2 s budget). `version` omitted when not installed. |
| `features` | Stable feature tokens — `generic-perm` (the `default`/`auto`/`skip` tri-state), `plan-stdin`, `prompt-b64`, `serve` (the on-demand rc activity hub), `activity` (the live activity dimension), `messages` (the message-feed and turn/interrupt/approvals endpoints exist on this binary — per-kind availability, opencode only today, is in `kind_features`), `contract-v2` (the v2 wire contract: `lane` on every session DTO, the `feed`/`interrupt`/`attach` hints in `kind_features`, the `turn`/`interrupt`/`approvals` hub verbs — routed and fully specified, live for opencode, `409 not_supported` for every other kind — the `approval_request` feed row, and `pending_approvals` on the session). A token is appended in the same change that ships its feature; `contract-v2` is a client's **route-existence** check — a server without it may 404 the new verbs at the mux, so a client reads the token instead of interpreting a bare 404. |
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
| `approvals` | Where approvals are answered: `tui` (in the terminal — claude-rc, codex, cursor), `remote` (through the hub's `POST /approvals/{id}` verb — opencode, live since this block), or `none` (nowhere a client can reach). This binary never emits `none`; the value exists for a **non-guest producer** of the same block — shed's roost-backed machine capabilities, synthesized client-side (`shed_core::roost::roost_capabilities`), where the terminal belongs to roost and no shed client can reach the tab to answer in it, so claiming `tui` would promise an affordance that does not exist. Clients branch on `== "remote"` only, so `none` and `tui` are the same non-decision to every one of them. |
| `watch` | **Deprecated** by `feed` (superseded, not removed): retained until clients migrate. The producer holds `watch == (feed == "messages")` in lockstep, so a v1 client reading `watch` and a v2 client reading `feed` see the same thing. Absent-field fallback: a client that only knows `watch` should keep using it. |
| `input` | Feed-input posting mode, **single-valued**: `turn` (the lane takes whole turns through `POST …/turn` — opencode) or `""` (no feed input at all — every other kind; the TUI-only `post_input` path still applies). A third value, `gated`, meant "`POST …/input` accepted unless the agent is blocked on a decision"; it was retired with the codex and cursor lanes (`charliek/shed#322`) and **no kind carries it any more** — `POST …/input` answers `409 not_accepting` for every kind. Clients that decode `gated` should keep doing so (an older guest may still send it) but will not see it from this binary. |
| `feed` | What the producer can stream for the kind: `messages` (a normalized conversation feed — `GET …/messages` + `message.appended`), `activity` (the activity dimension only — no message feed), or `none` (no signal at all). Supersedes `watch`. **`feed` describes the message feed, not the activity chip** — no client gates its activity chip on this field; the chip reads the session's own `activity`. That is why the same kind gets different answers from different producers: this binary says `none` for claude-rc, codex and cursor because their activity producers were deleted in A5/A6, while shed's roost-backed machine capabilities say `activity` for those kinds, because roost reports lifecycle for them and the client folds it into a live activity dimension. The answer is about **where the session lives**, not what it is. |
| `interrupt` | The `interrupt` verb is supported. `true` for opencode only; `false` elsewhere. |
| `attach` | How a terminal reaches the session: `tmux` (attach to the rc-tmux session), `native-remote` (the agent's own remote surface), or `none`. |

Normative matrix (exhaustive — pinned by `capabilities_test.go`):

| kind | post_input | approvals | watch | input | feed | interrupt | attach |
|---|---|---|---|---|---|---|---|
| claude-rc | true | tui | false | "" | none | false | tmux |
| codex | true | tui | false | "" | none | false | tmux |
| opencode | true | remote | true | turn | messages | true | tmux |
| cursor | true | tui | false | "" | none | false | tmux |

opencode is the only **live** lane (§ [Contract-v2 verbs](#contract-v2-verbs-turn-interrupt-approvalsid) below): its TUI runs an embedded HTTP+SSE server the hub steers through, so whole turns, interrupts, and approvals all go through the hub instead of the pane.

Every other kind reads `feed: "none"` and `input: ""`. The claude transcript tail (`charliek/shed#321`), the codex rollout tail and the cursor hook-ingest lane (`charliek/shed#322`) were all retired, and S2 (`charliek/shed#324`) then deleted the pane-anchor mechanism that gave codex/cursor their `needs_approval` signal too: the hub derives **no signal at all** for those kinds now, so `none` — not `activity` — is the truthful value under this table's own definition of the two. They remain launchable, attachable TUI kinds; their status comes from roost rather than from the hub for a machine session, and from liveness alone for a shed session until S5. Both clients branch on `feed == "messages"` only, so the change is invisible to them, and the value flips back to a real one when roost becomes the guest's source.

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

`state` is one of `starting | ready | reconnecting | needs-trust | needs-auth | dead`.
Since S2 (`charliek/shed#324`) it is **liveness**, not a pane reading: an enumerated
session is `ready` unconditionally, one whose tmux session is gone is simply not
enumerated (`list`) or reported missing (`probe`), and `starting` is only the
create-time placeholder before the first successful capture. `reconnecting` /
`needs-trust` / `needs-auth` remain in the wire enum purely so a client can keep
decoding an older guest's DTO — the current guest never emits them — and `dead`
survives only as the `--wait` path's own liveness verdict (a tmux session that never
came up). A golden fixture of this shape
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
approval state to report). Populated for opencode (lane-published, pending-only) only —
every other kind has no approvals producer since A6/S2 and its `pending_approvals` is
always empty. `omitempty`, so its absence carries no meaning beyond "nothing to
report." See [`needs_approval`
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
below — including the opencode message feed those previews summarize.

## The RC activity hub (`serve`)

`shed-ext-rc serve` runs the **RC activity hub**: a small, resident, per-shed daemon
that watches each rc session and exposes a loopback HTTP API. It answers the question
the lifecycle `state` cannot — *what is a usable session doing right now?* — by
deriving a live `activity` dimension (and, for opencode only, a normalized message
feed and remotely-answerable turns/interrupts/approvals). Clients never reach it
directly; the server's rc proxy and aggregate SSE stream are the only paths in (see
[Server surfaces](#server-surfaces)).

The hub enumerates the **same** tmux sessions the one-shot subcommands see, so its
session list is byte-identical to `list`; it only *overlays* the live activity a
one-shot exec cannot observe.

> **Loopback-only — a security invariant, not a default.** The hub binds
> `127.0.0.1:1029` and **only** `127.0.0.1`. It is unauthenticated and trusts the
> loopback: it is reachable solely through the server's `DialService` proxy (or an SSH
> forward). Binding a non-loopback interface would expose an unauthenticated control
> surface on a shed's shared bridge — never widen it. **The server-side proxy is the
> authorization boundary**; the hub itself does no authz. (On native **machines**
> there is no proxy — the loopback bind plus the operator's SSH tunnel is the
> boundary; see [the machine hub](#the-machine-hub-shed-host-agent).) The proxy also strips the
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
  attached**, **10 s otherwise**. A best-effort `fsnotify` nudge seam still exists for a
  future file-backed lane, but it is dormant today — its one root (codex's rollout
  directory) went with A6 (`charliek/shed#322`), and opencode's SSE stream is its own
  arrival signal, so there is nothing left for it to watch. So an activity transition
  surfaces within a couple of seconds while someone is watching, at low idle cost
  otherwise.

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
| `POST /v1/sessions/{slug}/approvals/{id}` | body `{"decision": "allow"\|"allow_always"\|"deny"}` (≤16 KiB) | **live for opencode**, `409` elsewhere: `200 {"resolved": true, "decision": "<decision>"}` | `400` invalid decision, malformed JSON, or an `{id}` that fails the approval-id grammar (below); `404` unknown slug, or `unknown_approval` for a well-formed but unrecognized id; `409` `not_supported` (`approvals != "remote"` — every kind but opencode) or `already_resolved` for a different decision on an already-resolved id (same-decision replay is idempotent, `200`, with no second upstream POST); `413` body too large |

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
| `not_accepting` | The verb **is** supported but not right now — for `turn`/`interrupt`/`approvals` this covers the lane-specific reasons above (no lane attached yet, an unpinned opencode session, an upstream failure); `POST /input` also answers `not_accepting`, unconditionally, for every kind (§ [Input](#input-post-input) below — no kind is `gated` any more). `turn`-while-busy and `interrupt`-with-no-active-turn stay **reserved** codes for a lane whose native surface actually refuses in that state — opencode's does not (see above), so it never emits them. Retryable in principle. |

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
`_`/`-` seen in native tool-call-shaped ids (e.g. ACP/opencode request ids like
`call_01HQ8Z3K.tool:2`), capped at 128 characters. The same expression gates both the
hub handler and the server-side proxy path classifier — a malformed id 404s at the
proxy before it ever reaches the guest; a syntactically invalid id sent **directly** to
the hub (bypassing the proxy) is a `400 invalid_approval_id`, not a `404` — a `404`
here would wrongly imply the id was well-formed but unknown. `claude-rc`, `codex`, and
`cursor` have no approvals producer at all (A6/S2), so any well-formed id sent for one
of them is rejected with `409 not_supported` before any lookup — their `approvals` row
says `tui`.

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
| `activity.changed` | `{shed, slug, activity, activity_at, state, last_message?}` | a session's *displayed* activity changes to a valid non-empty value (opencode only — no other kind has an activity dimension); `last_message` is the sanitized preview at the transition |
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
| `working` | a turn or tool call is in flight (from opencode's own event stream) |
| `needs_input` | opencode's last turn boundary was idle — waiting for the operator's next prompt |
| `idle` | reserved in the wire vocabulary for a settled "nothing pending" verdict; **no current producer emits it** — opencode's fold goes straight from `working` to `needs_input` |
| `unknown` | a live opencode session whose watcher hasn't yet confirmed which agent session belongs to this pane — distinct from *absent*, which means no activity dimension at all |
| `needs_approval` | opencode is blocked on the operator's yes/no — see [`needs_approval` producers](#needs_approval-producers-per-kind) below. No other kind ever reports this (or any) activity: `claude-rc`/`codex`/`cursor` have had no approvals producer since A6/S2 |

**Precedence rule (lifecycle trumps activity).** When `state` is a blocking lifecycle
value — `needs-trust`, `needs-auth`, `dead` — the *whole* activity dimension is
suppressed: `activity`, `activity_at`, **and** `last_message` are dropped together (a
bare timestamp is meaningless without its activity, and a stale `last_message` would
present pre-death context as current). The rule is retained for wire compatibility with
an older guest; since S2 (`charliek/shed#324`) `state` is liveness, so a session the hub
enumerates at all is always `ready` — a blocking-lifecycle row never appears in
`/v1/sessions` today, and in practice this rule never fires.

**Per-kind derivation.** There is exactly **one** producer left: **opencode**'s watcher,
which subscribes to the bare `opencode` TUI's embedded HTTP+SSE server on a per-session
loopback port (recorded at `create` time) and folds its `/event` stream (plus a REST
seed) into an activity verdict and a message feed. Every other kind —
`claude-rc`, `codex`, `cursor`, `shell` — has **no activity producer at all** and no
fallback to reach for one: the pane-stability engine that used to supply a universal
`working`/`idle` baseline, and the codex JSONL tail and claude transcript tail that used
to refine it for those two kinds, are all deleted (A5 `charliek/shed#321`, A6
`charliek/shed#322`, S2 `charliek/shed#324`). Their sessions simply carry no `activity`
field. **roost is the status authority for those kinds instead** — directly, over its
own protocol, for a machine session; for a shed session, only once roost reaches the
guest (S5). Until then, a shed's `claude-rc`/`codex`/`cursor` row is liveness-only, with
nothing standing in for the activity dimension it no longer has.

**Freshness / grace.** A settled watcher verdict (`needs_input`/`needs_approval`) is
trusted indefinitely — an event-bounded state stays true until a reply or a reseed
changes it. A transitional verdict (`working`/`unknown`) is fresh for 30 s since the
last SSE event; `working` additionally gets a longer **120 s grace** so a long tool call
doesn't flap. Past whichever window applies, the verdict is simply **stale**, and a
stale verdict yields **no activity at all** — not a fallback value, not `idle` — because
there is no other engine left to hand off to: the DTO omits the field entirely. So an
opencode row whose SSE stream dies mid-turn eventually loses its `activity` field rather
than sitting at `working` forever. This merge is `mergedActivity`, run once per
reconcile tick to produce the DTO's own `activity` field; no verb re-runs it to decide
whether to accept a request any more — `POST /input` no longer gates on activity at
all (§ [Input](#input-post-input) below).

`last_message` is a sanitized one-line preview (ANSI/control-stripped,
whitespace-collapsed, ≤200 runes) extracted by opencode's watcher; every other kind
carries none, since it has no activity producer to extract one from.

### `needs_approval` producers per kind

**opencode** is the only kind with an approvals surface — from live events on its own
protocol: `permission.asked`/`question.asked` open an ask (an open permission or an
open question both count toward `needs_approval`; only permissions are addressable —
see [`pending_approvals` is legal with a question open](#pending_approvals-may-be-empty)
below), `permission.replied`/`question.replied`/`question.rejected` close it. This is
an **event-bounded** verdict: `settled()` (the freshness contract) trusts it
indefinitely while the SSE transport is healthy, exactly like `needs_input`. On a dead
stream — the SSE connection disconnected or heartbeat-stale — the watcher reports
not-fresh and the [freshness/grace rule](#activity-dimension) applies: past its window
a `needs_approval` derived from a wedged connection yields no activity at all rather
than outliving the evidence for it; it comes back the moment the stream reconnects and
reseeds.

**Every other kind never reports `needs_approval`.** `claude-rc`, `codex`, and `cursor`
used to derive an informational (never remotely resolvable) approval episode by
pattern-matching each tool's approval-dialog chrome on the visible pane frame; S2
(`charliek/shed#324`) deleted that pane-anchor mechanism along with the rest of the
classifier. `approvals` stays `"tui"` for these kinds — an operator answers in the
terminal, and neither the activity dimension nor `pending_approvals` reflects that an
approval is pending. `POST /approvals/{id}` still 409s `not_supported` for them,
unconditionally (the capability check rejects it before any id lookup).

<a id="pending_approvals-may-be-empty"></a>**`needs_approval` with an empty
`pending_approvals` is legal.** An open opencode *question* (no decision vocabulary fits
`allow`/`allow_always`/`deny`, so it is never addressable by the approvals verb — remote
question-answering is a future contract extension) drives `needs_approval` without
adding a `pending_approvals` entry. A client must not assume a non-empty
`pending_approvals` accompanies every `needs_approval` session; the correct fallback
affordance is always "open the TUI".

`pending_approvals` (the session-level snapshot) is opencode's lane-published,
addressable, pending permission asks — the only source there is, now that the
pane-derived episode (codex/cursor) is gone. Every other kind's `pending_approvals` is
always empty.

### Message feed (opencode)

The opencode watcher folds its HTTP/SSE `/event` stream (plus a REST seed) into
normalized conversation messages, drained each tick into a per-session **ring buffer**
that `GET /messages` pages. Every other kind (`claude-rc`, `codex`, `cursor`, `shell`)
has a ring that simply never fills — none of them has had a message producer since A6
(`charliek/shed#322`) retired the codex JSONL tail and the cursor hook-ingest lane, and
claude never had one — so `GET /messages` for a tracked session of one of those kinds
returns `200` with an empty page forever, never a `404`: every enumerated session is
tracked, feed or not.

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
being approved, and `approval` the machine-readable state. opencode is the only
producer of this row today — every other kind's ring never carries one:

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
| `id` | The lane-assigned approval id — the address the `approvals/{id}` hub verb resolves. Grammar: `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$` (starts alphanumeric, `.`/`:`/`_`/`-` allowed, max 128 chars — same grammar as the `approvals/{id}` route above). A `tui`-approvals kind (`claude-rc`, `codex`, `cursor`) never publishes a row at all, so there is no id to resolve — `POST /approvals/{id}` 409s `not_supported` for them regardless of what `{id}` names. |
| `status` | `pending` or `resolved`. |
| `decision` | The decision that resolved it (`allow`/`allow_always`/`deny`); empty/omitted while pending, and also omitted on a `resolved` row closed by a reseed after a reply the hub never observed live. A client must not assume every `resolved` row carries a `decision`. |
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

opencode emits real, addressable `approval_request` rows from `permission.asked`/
`permission.replied`; no other kind has an approvals producer any more (see
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
that session reports no `activity`. (In practice this rule is provable rather than
exercised today: since S2 reduced `state` to liveness, a hub-tracked session's `state`
is always `ready` — the rule stands ready for an older guest, or a future lifecycle
producer, that makes the blocking states real again.)

### Input (`POST /input`)

No kind is `gated` any more. The codex/cursor lanes `gated` depended on were retired in
A6 (`charliek/shed#322`), and S2 (`charliek/shed#324`) then deleted the acceptance
machinery itself — the per-slug delivery mutex, the pane re-verify, the
approval-anchor/watcher merge — along with the pane classifier it read. `POST
/v1/sessions/{slug}/input` keeps its route and its request validation, but answers
**409 `not_accepting`** for every kind once the body is well-formed and the slug is
tracked (pinned by the `input_codex_not_accepting` rc-parity golden). opencode steers
through the `turn` verb instead (§ [Contract-v2 verbs](#contract-v2-verbs-turn-interrupt-approvalsid)
above); every other kind carries `kind_features.input == ""` (no feed input at all —
the TUI-only `post_input` kickoff path is unaffected).

**Statuses:** `400` invalid/unsafe/empty text · `404` unknown or gone slug · `409`
`not_accepting` (unconditional, once past validation and the tracked-slug lookup) ·
`413` body over 16 KiB.

### Correlation (opencode: session → SSE)

opencode has no external session file the hub could correlate against by watching a
directory — it creates its conversation session only on the **first prompt** (not at
TUI start), so a create-time window match would routinely expire before anything
exists to match. Instead, the opencode watcher correlates asynchronously, entirely
from its own `/event` stream — the only correlation mechanism left in the hub, now
that A6 (`charliek/shed#322`) retired codex's rollout-file pin and A5
(`charliek/shed#321`) retired claude's transcript pin:

- It subscribes to the session's per-port `/event` stream first, then seeds via REST —
  so no event is lost in the gap between subscribe and seed.
- A trusted pin comes **only** from a port-local SSE event on the watcher's own stream
  (never from `GET /session`, which reads the shared opencode DB and can return other
  sessions/servers' history): the first **root** session (no parent) whose canonical
  directory matches the rc session's workdir. Once pinned, the id is back-written to
  `SHED_RC_AGENT_SESSION` so a hub restart re-correlates exactly.
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

## The machine hub (shed-host-agent)

Live activity on a **machine** (as read by the desktop and mobile clients) comes from
the **machine RC hub** — the same loopback HTTP service a shed runs, bound to
`127.0.0.1:1029`. **`shed-host-agent` hosts it**, as a supervised resident role: the
daemon binds the port at startup and keeps the hub up for as long as it runs. Opt out
with `rc_hub.enabled: false` in the agent's config.

Because the daemon is supervised (brew services / systemd), the hub does not come and
go with session activity — unlike the retired `shed-machine-rc serve`, which exited
after 15 idle minutes. If some other process already holds the port, the agent logs
it, retries with backoff, and takes over when the port frees.

### The hub's `PATH` is the hub's, not yours

The hub **spawns agent binaries** (`opencode`, `codex`, `cursor-agent`, `claude`), so
it can only launch what is on the PATH of the process that hosts it — and a supervised
daemon does not inherit your shell's. A systemd **user** unit in particular starts with
a minimal PATH: an agent installed into `~/.local/bin`, `~/.bun/bin`, or a version
manager's shim directory is invisible to it.

The symptom is quiet and easy to misread: `curl 127.0.0.1:1029/v1/health` (or
`shed-host-agent status`) reports the agent as not installed, and a kickoff for that
kind fails, with nothing pointing at PATH as the cause. The tool is plainly there in
your own shell.

Set the PATH explicitly on the unit that hosts the hub:

```bash
systemd-run --user --unit=shed-rc-hub --property=Restart=always \
  --setenv=PATH="$HOME/.local/bin:$HOME/.bun/bin:/usr/local/bin:/usr/bin:/bin" \
  shed-host-agent rc-hub
```

For a packaged unit, the equivalent is an `Environment=PATH=…` line (or a drop-in). On
macOS under brew services, the launchd job has the same property: its `PATH` is
launchd's, not your login shell's.

Confirm with `curl 127.0.0.1:1029/v1/health` and `shed-host-agent status` — every agent
you expect should report as installed with a version.

**The trust model is the machine's own.** There is no server proxy on a machine: the
hub binds loopback only and does no authorization — the loopback bind plus your SSH
tunnel (`ssh -L`) IS the boundary. Never widen the bind. Note what "local" means here:
every process of every app running under any uid that can reach loopback on the
machine — not a sandboxed VM. That is still the machine's existing trust boundary (a
local process that could POST to the hub could already drive the same tmux session
directly with `send-keys`), so the hub adds a convenience channel within local trust,
not a new boundary — but the scope of "local" is the whole machine, and it is worth
saying plainly.

**Machine-posture deltas from the guest hub** (deliberate, not drift): inside the agent
the hub is a supervised resident role — no 15-minute idle exit, no detach double-fork,
no pidfile; at zero sessions the watchers quiesce and the recurring cost is one
`tmux ls` per idle tick. The agent's bind loop retries rather than exits, so a
permanently held port shows up as `RC hub: deferred` in `shed-host-agent status`, not a
dead daemon.

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
