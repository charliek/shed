# Agent sessions: multi-agent RC, status, and the non-TUI view

Status: draft design (2026-07-09). Companion research (transient, not committed):
`docs/research/{claude_rc,codex,opencode,cursor}.md`.

## Goals

1. **Multi-agent TUI sessions, fast.** `shed attach --kind codex|opencode|cursor`
   and the shed-mobile "new session" screen can start any of the four agents in a
   shed, with the existing tmux-attach TUI experience. Codex/opencode/cursor get
   the same create/list/probe/kill lifecycle claude has today.
2. **RC metadata over HTTP** (spirit of #242): a phone renders the cross-host
   sessions view from one HTTPS call per host — no per-shed SSH fan-out — plus a
   per-host **overview endpoint** so the landing screen is one call too.
3. **Normalized agent status**: every session reports an activity state
   (`working | needs_input | needs_approval | idle`) using the vocabulary the
   ecosystem converged on, updated live, exposed over HTTP/SSE. Push
   notifications and desktop surfacing build on this later (designed-for, not
   built now).
4. **Non-TUI session view** (codex first): watch a session as a message feed on
   mobile, post input when the agent is waiting, and be told "open the TUI" when
   the interaction needs the terminal (approvals, auth).
5. **Capability discovery**: clients learn per shed which agents are installed
   and which features (watch / post-input / approvals) each kind supports —
   replacing today's error-sniffing (`isOldBinaryPermModeErr`).
6. **Machines later**: everything lands in the shared `internal/ext/{rc,clirc}`
   core so `shed-machine-rc` (native hosts) inherits it.

Non-goals (now): push-notification delivery, desktop-app status UI, remote
approval buttons for agents whose surfaces don't support it, replacing the TUI.

## Decisions already made

- **tmux-TUI-first.** Every session — including non-TUI-viewable ones — is a
  normal agent TUI in a `rc-<slug>` tmux session. Structured observation happens
  *beside* the TUI (rollout/transcript tailing, hooks, pane analysis), never by
  replacing it. One session model; the TUI is always attachable. (Mirrors
  Omnara/Happy; avoids codex app-server multi-subscriber bugs and keeps
  claude's Max-subscription-safe interactive path.)
- **rc hub daemon, on demand only.** A resident per-shed process is required for
  tailing/hooks/SSE, but it must not run in every shed: it starts lazily on the
  first `shed-ext-rc create` (or first status request that finds rc sessions)
  and exits when idle with no sessions.
- **#242 in spirit**: server-side RC enrichment of the session routes, plus an
  aggregate overview endpoint carrying feature flags for endpoint discovery.
- **Auth fallback = classification, not plumbing**: per-agent login-prompt
  detection yields `needs-auth`; the user logs in via the TUI. Credential-mount
  conventions for ~/.codex etc. are a separate follow-on.

## Architecture

```
 mobile / CLI / desktop
   │  HTTPS (pinned TLS + control token)          SSH (attach TUI, fallback)
   ▼                                                ▼
 shed-server ──────────────────────────────► guest VM
   • GET /api/overview            agent exec   • shed-ext-rc  (one-shot CLI, as today)
   • GET /api/sessions?rc=1       (vsock)      • rc hub = `shed-ext-rc serve`
   • GET /api/sheds/{n}/rc/*  ────────────►      - session registry (tmux env, as today)
   • GET /api/rc/events (SSE)     tcp/vsock      - per-agent watchers → normalized status
                                  proxy          - message feed store (ring buffer)
                                                 - localhost HTTP+SSE, POST input
                                                 • agent TUIs in tmux rc-<slug>
```

Three planes, delivered in order:

1. **Control plane (exists, generalize):** one-shot `shed-ext-rc` over
   SSH/agent-exec — create/list/probe/kill. Gains new kinds + `capabilities`.
2. **Read plane (new, server-side):** shed-server enriches session listings by
   exec'ing `shed-ext-rc list` in-guest and exposes overview/aggregate routes.
3. **Live plane (new, hub):** the on-demand hub daemon derives activity status
   and message feeds, exposed through shed-server proxy + SSE.

---

## Part 1 — Generalize kinds (all agents, TUI-level)

### Agent registry

Refactor `internal/ext/rc` so everything agent-specific lives in one table
instead of switch statements scattered across `rc.go`/`ops.go`/`trust.go`:

```go
type AgentSpec struct {
    Tool         string            // "claude", "codex", "opencode", "cursor"
    Kinds        []Kind            // kinds this tool provides
    InnerCommand func(k Kind, o CreateOpts) string
    Classify     func(pane string) (State, url string, id string)
    Preseed      func(env Env, workdir string) error   // trust/onboarding, or nil
    PermMap      map[GenericMode][]string               // generic → agent flags
    Probe        func(getenv func(string) string) AgentInfo // installed? version?
}
```

### New kinds

| Kind | Inner command (v1) | Notes |
|---|---|---|
| `codex` | `codex` | plain TUI; rollout JSONL is the structured feed |
| `opencode` | `opencode` | monolith TUI v1; `serve`+`attach` split is a later upgrade |
| `cursor` | `cursor-agent` | TUI; hooks are the later structured feed |
| (existing) `claude-broker`, `claude-rc`, `shell` | unchanged | |

Bare tool name = plain TUI, keeping `<tool>-<mode>` open for future modes
(`codex-hub`, …). Mobile's `kindColor()` already prefix-matches these strings.

### Generic permission modes

Claude's full `--permission-mode` set stays claude-only. Other kinds accept a
generic tri-state mapped per agent (the VM is already the sandbox):

| Generic | claude | codex | opencode | cursor |
|---|---|---|---|---|
| `default` | (none) | (none) | (none) | (none) |
| `auto` | `--permission-mode auto` | `--full-auto` | `--auto`* | (none)† |
| `skip` | `--permission-mode bypassPermissions` | `--dangerously-bypass-approvals-and-sandbox` | `--auto` | `--force` |

\* opencode's `--auto` approves everything not denied — closest to both.
† cursor has no mid-tier; `auto` = default until one exists.
Open question flagged below; exact mapping verified live during implementation.

### Per-agent classification (`needs-auth` and friends)

Extend `ClassifyPane` with per-agent regex sets (login prompts, trust prompts,
ready prompts, dead pane). States stay the existing lifecycle enum
(`starting/ready/reconnecting/needs-trust/needs-auth/dead`). `url`/`id` remain
claude-remote-control-specific; nil for other agents (the DTO already omits
empties). Golden pane fixtures per agent per state, captured from live sheds
(`tmux capture-pane` of real login/trust/ready screens) drive table tests.

### Capabilities (replaces error-sniffing)

New subcommand, shared by both binaries:

```
$ shed-ext-rc capabilities
{ "rc_version": 3,
  "kinds": ["claude-broker","claude-rc","codex","opencode","cursor","shell"],
  "agents": { "claude":  {"installed": true,  "version": "2.1.206"},
              "codex":   {"installed": true,  "version": "0.143.0"},
              "cursor":  {"installed": false} },
  "features": { "serve": true, "prompt_stdin": true, "generic_perm_modes": true },
  "kind_features": { "codex": {"watch": true, "post_input": true, "approvals": "tui"},
                     "claude-rc": {"watch": false, "post_input": true, "approvals": "tui"} } }
```

`shed-ext-rc list` gains the same block in its envelope
(`{"rc_sessions": [...], "capabilities": {...}}`) so one guest exec feeds both;
old binaries' output simply lacks it (tolerated). Clients (CLI, mobile, server)
use `kinds`+`agents` to enable create chips and `kind_features` to shape the UI.
Unknown kinds remain readable (existing `MinManagedVersion` seam; `SHED_RC_V`
stays 2 — all changes are additive).

### Getting the CLIs into guests

`full` image already installs claude, opencode, codex. Add cursor-agent
(`curl https://cursor.com/install`) to `vz/Dockerfile` + `firecracker/Dockerfile`.
Existing sheds keep whatever they have — `capabilities.agents` reports reality,
so clients degrade gracefully (chip disabled with "not installed in this shed").

### Plan delivery moves into the rc core

Today plan shipping is CLI-side and claude-only (`cmd/shed/rc.go`:
`streamPlanToShed` → `~/.claude/plans/plan-<slug>.md` + kickoff composition).
Move it into the shared `create` verb: `create --plan-stdin` reads the plan
from stdin, writes it to the agent-appropriate location (claude:
`~/.claude/plans/plan-<slug>.md`; other kinds: `~/.shed-plans/plan-<slug>.md`
— home-rooted on purpose: a workdir file would dirty `--repo` clones and
write through VirtioFS onto real host dirs with `--local-dir`), and composes
the per-kind kickoff prompt referencing it. Optional user framing travels as
`--prompt-b64` alongside the plan (stdin carries only the plan). Every orchestrator —
`shed attach --plan`, mobile, and `shed-machine-rc` on native machines — gets
cross-agent plan runs from one implementation.

### `shed plan` porcelain + the shed-plan skill

New CLI subcommand collapsing the skill's multi-step workflow into one command:

```
shed plan ./plan.md --shed plan-topic [-s server] [--repo owner/repo] [--kind codex] [-p "framing"] [-d]
```

Creates the shed if missing (when `--repo` given), ships the plan via
`create --plan-stdin`, waits for `ready`, prints session + watch/steer info
(and the claude.ai URL for claude kinds). The **shed-plan skill** then reduces
to: author the plan → run `shed plan` → report — with far less orchestration.
Skill stays **claude by default**; the user can say "send to codex/cursor/
opencode" and the skill passes `--kind`. (Machine targets via
`shed-machine-rc create --plan` fall out of the shared core; pointing the
skill at machines is a follow-on.)

### Client updates in this part

- **CLI**: `shed attach --kind` accepts new kinds; validation moves from the
  hardcoded `validRCKinds` mirror to capabilities (with the old list as
  fallback). `--plan` generalizes per the above; kickoff `--prompt` works for
  all kinds that accept typed input. New `shed plan` porcelain.
- **shed-plan / shed skills**: rewrite shed-plan around `shed plan`; claude
  default, `--kind` override on user request.
- **mobile**: extend `RcKind`, flip the `codex-rc · soon` chip to real chips
  gated on capabilities, pass generic permission mode for non-claude kinds.

---

## Part 2 — RC over HTTP (#242 spirit + overview)

### Server-side enrichment

Per the ticket: `handleListAllSessions` / `handleListSessions` run
`shed-ext-rc list` per running shed **over the agent exec channel the server
already owns**, merge by tmux name into `Session.RC` (wire type already
exists), with concurrency cap + per-shed timeout + silent degradation.
Enrichment is on by default with `?rc=0` opt-out (cheap path preserved).
The canonical DTO stays `internal/ext/rc` (pure Go); the merge logic becomes
shared and the CLI's private DTO copy is retired. The CLI's SSH enrichment
path is **deleted** (no old-server fallback, per the compatibility posture) —
against a not-yet-upgraded server, `shed sessions` shows plain rows until the
fleet upgrades.

Delta from the ticket (the "spirit" part): the same exec also returns the
capabilities block, which the server caches per shed (invalidated on shed
restart) and surfaces in the responses below.

### Overview endpoint

```
GET /api/overview →
{ "server": { "version": "...", "features": ["rc-enrich","overview","rc-hub-proxy"] },
  "df": { ... },                       // same shape as /api/system/df
  "sheds": [ { ...shed, "sessions": [ { ...session, "rc": {...} } ],
               "rc_capabilities": {...} } ] }
```

One HTTPS call renders the mobile landing page *and* the sessions tab for a
host. `server.features` (also added to `/api/info`) is the endpoint-discovery
mechanism: clients feature-detect instead of version-sniffing. Mobile's
`hostSessionsProvider` SSH fan-out and the landing `sheds+df` pair collapse to
this call; SSH remains only for terminal attach and token mint.

---

## Part 3 — Live status (rc hub)

### Hub lifecycle

`shed-ext-rc serve` — same binary, runs as the shed user in the guest.

- **Start**: `shed-ext-rc create` ensures the hub is up (spawn detached +
  pidfile/socket check, à la the dev-server pattern); the server's rc proxy
  can also request a start via one guest exec when a client asks for live
  status and rc sessions exist.
- **Stop**: hub self-exits after N minutes with zero rc sessions and zero
  subscribers. No systemd unit, nothing resident in idle sheds.
- **Listen**: `127.0.0.1:<fixed port>` in the guest (unix socket would block
  the server's existing TCP `connect/{port}` tunnel primitive; localhost TCP
  keeps both proxy options open).

### Normalized activity status

Two orthogonal fields on the session DTO (additive):

- `state` — lifecycle, unchanged (`starting/ready/needs-auth/...`).
- `activity` — `working | needs_input | needs_approval | idle | unknown`,
  plus `activity_at` (last transition) and `last_message` (truncated).

Per-kind derivation, layered (best signal wins, pane stability is the
universal tiebreaker):

| Kind | Primary signal | Secondary |
|---|---|---|
| `codex` | rollout JSONL tail (`~/.codex/sessions/**`): `task_started`/`task_complete`, pending `function_call` | `notify` hook → hub HTTP; pane stability |
| `claude-*` | transcript JSONL tail (`~/.claude/projects/**`) | hooks (`Stop`/`Notification` → hub HTTP) later; pane stability |
| `cursor` | pane stability v1; `stop`/`beforeShellExecution` hooks → hub | transcript tail |
| `opencode` | pane stability v1; `serve`+SSE mode later | plugin `event` hook later |
| `shell` | pane stability only (`working`/`idle`) | — |

Pane stability = AgentAPI-style `capture-pane` snapshot diffing with a quiet
period — cheap, agent-agnostic, and we already own the capture plumbing. It is
the **status MVP for every kind**: all sessions get `working/idle` (plus the
lifecycle classifier's `needs_*`) the day the hub ships, before any per-agent
adapter exists.

The codex and claude adapters share one implementation (fsnotify JSONL tail +
tolerant parser + activity mapper), so **claude status ships in the same phase
as codex** — only the schema tables differ. Cursor and opencode start on pane
stability and upgrade to their structured surfaces (hooks / serve-split) as
follow-ons, easiest-first.

### Hub API (guest-local)

```
GET  /v1/sessions                    rich DTOs incl. activity
GET  /v1/events                      SSE: session.updated / activity.changed / message.appended
GET  /v1/sessions/{slug}/messages?since=   normalized message feed (see Part 4)
POST /v1/sessions/{slug}/input       {"text": ...} → tmux bracketed paste; 409 unless activity==needs_input
```

### Server exposure

- `GET /api/sheds/{name}/rc/*` → reverse proxy to the guest hub (over the same
  `DialService` path as `connect/{port}`), starting the hub if needed. The hub
  binds loopback only; `DialService` reaches it through the guest agent's vsock
  TCP proxy on **both** VZ and Firecracker. (Firecracker parity — routing
  `DialService` through the tcpproxy instead of dialing the bridge IP — was
  delivered on this PR; previously FC could not reach the loopback hub and
  degraded to `503 RC_HUB_UNAVAILABLE`. RC_HUB_UNAVAILABLE now means the hub is
  genuinely down or the image predates it, on either backend.)
- `GET /api/rc/events` → server-aggregated SSE across sheds (server maintains
  one upstream SSE per shed-with-hub, fans out to HTTP clients). This is the
  future push-notification seam: a notifier subscribes here; nothing else
  changes.
- AuthZ: existing control-token middleware; the hub itself trusts localhost
  (only reachable via server proxy or SSH forward).

Also: enrichment (Part 2) prefers the hub when running — `shed-ext-rc list`
consults it for `activity` — so listings get live status for free without the
server exec'ing anything new.

---

## Part 4 — Non-TUI view (codex first)

### Normalized message feed

The hub translates per-agent transcripts into one wire shape:

```json
{ "seq": 412, "ts": "...", "role": "assistant|user|tool",
  "type": "text|tool_use|tool_result|reasoning|status",
  "text": "...", "tool": {"name": "bash", "detail": "..."} }
```

Codex rollout JSONL maps cleanly (`response_item/message`,
`function_call`↔`function_call_output` by `call_id`). Parsers are tolerant
(unknown fields/types preserved as `status` rows) because both codex and claude
mark these formats unstable.

### Mobile UX

- Session card → "watch" opens a chat-style view: message feed
  (`GET .../messages` + SSE), status pill from `activity`.
- Input box enabled when `activity == needs_input`
  (`POST .../input` → hub → `tmux send-keys` bracketed paste — same delivery
  path `--prompt` uses today, so fidelity is known).
- `needs_approval` / `needs-auth` / anything unsupported → banner with one tap
  into the existing `TerminalScreen` (the TUI is always there; that's the
  tmux-first payoff).
- `kind_features` gates all of it: kinds with `watch:false` simply don't show
  the option (claude v1, shell).

Order of structured adapters after codex: claude for **status** lands with
codex in Phase C (shared tail machinery); claude *messages* for the non-TUI
view follow (transcript schema is well-understood but unstable) → cursor
(hooks) → opencode (serve split — best long-term surface, biggest
session-model change).

---

## Machines

`shed-machine-rc` shares `clirc`, so kinds, capabilities, and `serve` come along
automatically. Transport differs: no shed-server in front of a machine, so v1
reach is SSH (run `shed-machine-rc list`, forward the hub port). If/when the
host-agent (Phase 3 Rust port) grows a machine-status role, the hub's HTTP
surface is what it would broker. Explicitly out of scope now beyond keeping the
core shared.

## Compatibility posture

The RC convention is young: **breaking changes are allowed** and preferred over
carrying legacy seams. All first-party consumers move in lockstep when the
wire/CLI contract changes:

| Consumer | RC surface |
|---|---|
| shed CLI / server (this repo) | `cmd/shed/rc.go`, `internal/ext/{rc,clirc}` |
| Rust core → desktop Swift + Tauri | `crates/shed-core/src/rc.rs`, `shed-app/src/rc.rs`, `desktop/Sources/ShedKit/RC/`, `desktop/tauri` |
| shed-mobile (Flutter) | `lib/rc/{rc_models,rc_service}.dart` |
| shed-remote-agent (TS, best-effort) | `apps/api/src/lib/{rcBinClient,machineRc}.ts`, `packages/shared/src/schemas/rc.ts` |

Consequences taken in this design:

- The CLI's error-sniffing (`isOldBinaryPermModeErr`) is **deleted**, not
  wrapped: capabilities is the only version probe, and its requirement is
  scoped to **non-claude kinds and new features** — claude/shell creates keep
  working against old baked-in binaries; only a new-feature request on an old
  shed yields the one clear "this shed's image predates multi-agent RC —
  recreate it" error.
- **Unknown-kind policy** (all clients): preserve the raw kind string, render
  neutrally, attach no kind-specific affordances — replacing today's
  default-to-claude-broker (which would give a codex session a synthetic
  claude.ai URL in old readers).
- Stay additive where it's free (list envelope gains `capabilities`; DTO gains
  `activity`), break where it buys simplicity (permission-flag semantics,
  kickoff/plan composition). `SHED_RC_V` bumps to 3 only if the tmux-env
  metadata schema itself changes.
- The one durable compat concern is **existing sheds with an old baked-in
  `shed-ext-rc`**: they stay listable (the existing `MinManagedVersion` seam),
  new features simply require a recreated shed.
- CLI-vs-server skew within the fleet is handled by releasing both in one tag
  (single version line), not by fallback paths.

## Delivery plan

| Phase | Ships | Depends on |
|---|---|---|
| **A. Kinds + capabilities** | agent registry refactor, `codex/opencode/cursor` kinds, per-agent classifiers + golden fixtures, `capabilities`, generic perm modes, plan delivery in core (`--plan-stdin`), `shed plan` porcelain, Dockerfile cursor install, CLI `--kind`, shed-plan skill rewrite, mobile chips | — |
| **B. HTTP read plane** | server-side enrichment (+`?rc=0`), shared `internal/rcmeta`, `GET /api/overview`, `features` in `/api/info`, CLI prefers server RC, mobile drops SSH fan-out | A (capabilities in list envelope) |
| **C. Hub + status** _(shipped — server/CLI)_ | `serve` mode, on-demand lifecycle, pane-stability engine (baseline status for **all** kinds), codex rollout tail + claude transcript tail (shared machinery), `activity` fields, server rc proxy + aggregate SSE, **Firecracker `DialService` hub parity** (routes through the guest tcpproxy like VZ, so FC reaches the loopback hub) | A; B for exposure |
| **D. Codex non-TUI** _(shed-side shipped; mobile pending)_ | message feed + input POST in hub _(shipped)_, `kind_features` gating _(shipped)_, mobile chat view _(pending — shed-mobile)_ | C |
| **E. Follow-ons** | claude messages (non-TUI view), cursor hooks adapter, opencode serve-split, machine transport, push notifier, credential-mount conventions | D |

Phase A alone delivers "kick off codex/opencode/cursor from the app or skill";
A+B deliver the #242 efficiency win; C+D deliver codex "fully working".

Client fan-out per phase (one PR per repo): **shed** carries the core in every
phase; **crates/desktop** (same repo) update alongside when the DTO/kind
contract moves (A: kinds in the launch sheet + Rust DTO; B: overview; C/D:
optional); **shed-mobile** updates in A (chips), B (drop SSH fan-out), D (chat
view); **shed-remote-agent** best-effort in A (kinds) and later as desired.

### Testing per phase

- A: table tests on registry/classifiers (golden panes from live sheds);
  round-trip DTO tests; live check: create each kind in a real shed via
  `shed attach --kind ...`, verify `needs-auth` before login and `ready` after.
- B: fake-agent handler tests (populated/nil/degraded per the ticket's
  acceptance criteria); `make test-integration-dev` against the parallel dev
  server (server-side change ⇒ dev-server validation per CLAUDE.md); mobile
  drive-skill run against the new endpoint.
- C/D: hub unit tests with recorded rollout fixtures; live codex session
  end-to-end (create → watch feed → post input) using the drive skill for the
  mobile side.

## Open questions

1. **Generic perm-mode mapping** — verify each flag against installed CLI
   versions during Phase A (they churn); `capabilities.features` advertises the
   mapping version.
2. **Hub port** — fixed guest port (simple, one per shed) vs. registered
   dynamically in a file the one-shot CLI reads. Leaning fixed.
3. **Codex `notify`/hooks config** — writing `~/.codex/config.toml` from the
   hub is convenient but mutates user config; v1 ships tail-only, hooks opt-in.
4. **Enrichment default** — on-by-default with `?rc=0`, or opt-in `?rc=1` as
   the ticket sketches; measure the per-shed exec cost first.
