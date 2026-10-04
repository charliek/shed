# craze in shed: the craze source, provider choice, and session settings

Status: discovery, decided 2026-10-03 by the owner in a craze design
session. Not yet planned. This page is the brief a shed planning session
starts from. The craze side of the same decisions is recorded in craze's
`discovery/session-control/` (`06-shed-lane.md`, and decisions SD-40 to SD-46
in `08-decisions.md`).

## Summary

craze becomes the way shed reaches every agent provider other than Claude.
craze drives `cursor-agent`, `grok` and `gx` over ACP, plus a native
multi-provider harness. It runs each session in a detached host, and a
per-machine **hub** lists those sessions, creates new ones and splices clients
to them. One NDJSON protocol covers all of it (craze `docs/reference/protocol.md`).

This work changes shed in four ways:

1. **Agent set.** shed's agent set becomes craze, opencode, and Claude
   through Claude's own remote control. The gx lane and shed's cursor, codex,
   grok and gx kinds are retired from shed. roost keeps every agent, and a tab
   running one of those agents directly shows in shed as a plain row.
2. **The hub is a machine-level source.** The craze hub is shed's source for
   craze sessions on a machine. One connection lists every craze session
   there, tab-hosted or headless, and pushes each change. It also says which
   providers can start, and it creates sessions. shed's lane contract splits
   into a machine-level **source** and a session-level **lane**.
3. **Create picks a provider.** The phone and desktop create sheet picks a
   provider, a directory and an optional first prompt. The model is the
   provider's default.
4. **Settings change inside a session.** In a running session, model,
   effort, fast mode, context and mode are changed from one settings sheet.
   craze already sends the data that sheet is built from.

The work runs in two phases:

- **Phase A, craze:** a small plan in craze, run first.
- **Phase B, shed and shed-mobile:** one plan run from shed.

shed holds its next release until this churn settles.

## Why

- **The phone is the key client.** Most sessions start at a desk; the phone
  watches and steers them on the go, and starting one from the phone is a
  real gain. craze was built for this:
  - **Resume:** a cursor resume with no reseed after the phone backgrounds.
  - **Asks:** sequenced and resumable.
  - **Cancel:** acknowledged.
  - **Hosts:** sessions that outlive their terminal.
  - **The hub:** one per machine, so no discovery, token or probe per
    session.
- **One abstraction over the providers.** ACP gives craze one dialect for
  cursor and grok, and the native harness covers API models. A new provider
  is craze's work, not shed's. opencode and codex may move into craze later,
  leaving shed with craze plus Claude.
- **The TUI is one client among equals.** craze's own TUI already reaches
  sessions through the same socket protocol. Anything a client needs to know
  (provider availability, settings, defaults) lives in craze's protocol, so
  shed gets it for free and cannot disagree with the TUI. craze's discovery
  calls t3code "the destination shape": one server per machine that owns the
  provider sessions, with thin clients over one protocol.
- **Churn is expected.** shed's lane work has not shipped, so its contract
  is reshaped now around the source it will live with, before a release
  fixes it.

## Decisions

| # | Decision | Rationale | craze record |
|---|---|---|---|
| D1 | shed's agents are **craze** (cursor, grok, gx, native), **opencode**, and **Claude** (its own remote control, unchanged). The gx lane is retired from shed, and so is shed's kind-specific code for cursor, codex, grok and gx. roost is untouched. On the phone a tab running one of those agents directly is a plain row. On the desktop that row opens the tab's terminal, and launching an agent's own TUI in a new tab stays a desktop option. | craze covers what gx did and more; fewer lane kinds make the contract change smaller; roost still runs any agent. | SD-40 |
| D2 | **craze is the provider abstraction, and the TUI is one client among equals.** What a client needs (provider availability, session settings, defaults and memory) is in craze's protocol, never re-derived in shed. | One implementation, so the TUI, desktop and phone cannot disagree; each new client gets it for free. | SD-41 |
| D3 | **The craze hub is a machine-level source.** shed's contract splits into a source (machine level: live session list, create options, create) and a lane (session level: transcript, send, cancel, approvals, settings), with **capabilities per session**. opencode implements both; gx is deleted rather than adapted. | craze is the flagship and is machine-scoped. Its capabilities vary by provider and model, which today's static, per-adapter `LaneCapabilities` cannot express. | SD-42 (refines SD-29) |
| D4 | **For craze sessions, status is craze's.** The hub's row is the row. A roost tab that craze owns is folded into the hub row it names: one row, with the tab's terminal actions added. roost's row stands alone only when that machine's hub feed is down. roost stays the terminal manager. | The hub's row comes from the process driving the agent and carries more (what it is doing, the head ask, last reply, model, attached clients). roost's craze report summarises the same state. | SD-43 (replaces SD-15 for craze sessions) |
| D5 | **Provider availability is a craze feature.** Each provider is `ready`, `needs_setup` (native with no key) or `unavailable` (binary missing, or cursor where the spawning login session cannot reach the macOS keychain). It comes with a reason and a fix. Clients **dim** unavailable providers instead of hiding them; gx stays hidden when missing. The TUI uses the same check, and its never-empty-picker rule ends. | One rule everywhere; a dimmed row explains itself, and doubles as the reminder that cursor-from-the-phone is unfinished. | SD-44 |
| D6 | **Create** takes a provider, a directory (recent ones offered) and an optional first prompt. The model is the provider's default, the same one a plain `craze --provider X` gets. There is no permission-mode picker (craze's default for a new session is bypass) and no effort at create. | Smallest useful create on a phone; everything else is a setting, changed in the session. | SD-45 |
| D7 | **Settings in a session:** one sheet holds the model, the current model's own options (effort first, then fast, context and the rest) and the mode. It is rendered generically from what craze sends, through a generic lane settings contract. Target: the last milestone of Phase B; if it does not fit, a roadmap row with this design. | Changing model and effort mid-session is a key feature; craze already sends the data in one generic shape, so it is client work only. | SD-45 |
| D8 | **Defaults and memory are craze's**: the default provider (the last one started), recently used models (native's ranks) and recent directories. shed stores none. | Same answers in the TUI, desktop and phone. | SD-44 |
| D9 | **Cursor on macOS, MVP:** when the hub was first started outside the GUI login session (over ssh), cursor is dimmed in the create sheet with the reason. Viewing and controlling cursor sessions started on the Mac itself works regardless. The real fix is a follow-up (see Follow-ups). | Most sessions start at the desk; other providers work fully from the phone; the fix carries its own design choices. | SD-37 (stands), SD-44 |
| D10 | **Transport:** each connection runs `craze bridge --hub` behind the device's stable local port, re-executed for each accepted connection (SD-29's pump, kept). That means one long-lived roster connection per machine plus one connection per open transcript. The desktop runs `craze bridge --hub` as a local subprocess for its own machine. | Rust never runs SSH on the phone (Dart owns it); the hub has no TCP port by design; one code path serves local and remote; the bridge starts a hub when none runs. | SD-29, SD-42 |
| D11 | **Order:** Phase A (craze) first, then Phase B (shed with shed-mobile). The release gate in craze's SD-34 ("S3 waits for shed's first release") is dropped: shed holds its release until this churn ends, then releases more often. | Doing and testing the work needs no shed release: shed-mobile pins shed by git rev, and craze installs from source. A release first would only add a bisect point. | SD-46 (supersedes SD-34's gate) |

## Where things stand (verified 2026-10-03)

shed at `a8725fa`, shed-mobile at `d9eb4c2`, craze at `4a3268a`.

### shed

**The lane contract.** `AgentLane` (`crates/shed-core/src/lane.rs:1446`)
has ten methods. Things to know:

- **Create** is only `create(cwd, text)` (`lane.rs:1472`). There is no model,
  provider, agent or permission parameter.
- **Capabilities.** `LaneCapabilities` (`lane.rs:1348`) is
  `{kind, interject, create, cancel, approvals, history_cursor}`. It is
  static for the adapter's life.
- **Session rows.** `LaneSession` (`lane.rs:332`) has no model or provider.
- **The module doc is the spec:**
  - the FRB-mirror rule (owned `String`, `Option`, `Vec` and scalars only;
    free-form payloads ride as raw JSON strings);
  - the `Reset … Ready … Down` bracket;
  - silent resume with `history_cursor`;
  - the bounded channel (`LANE_CHANNEL_CAPACITY = 1024`, `lane.rs:1100`);
  - `option_for`, which refuses an ambiguous permission kind.

**Adapters.** There are two, `crates/shed-opencode` and `crates/shed-gx`:

- **opencode** reaches its server over HTTP. The server URL comes from a
  roost tab's ownership metadata (`AgentLaneStamp`,
  `crates/shed-core/src/roost/model.rs:128`; `agent_lane()` at `:274`).
- **gx** needs discovery, a token probe over ssh, a `healthz`/`instanceId`
  pin, and an `ssh -L` forward per lane.
- **Model choice:** neither adapter exposes one. opencode's live test says
  outright: "choosing a model is the agent's configuration, not a transport
  concern" (`crates/shed-opencode/tests/live.rs:91`).

**Creating sessions.** Neither client calls `AgentLane::create`. Both start
sessions with roost's `tab.open`, given only a command and a directory. That
command is a bare binary name from `launch_argv` (`roost/model.rs:667`), with
no flags and no first prompt (shed#366).

**Kinds.** `RcKind` (`crates/shed-core/src/rc.rs:65`) has these variants:
`ClaudeRc`, `ClaudeBroker`, `Codex`, `Opencode`, `Cursor`, `Gx`, `Grok`,
`Shell` and `Other(String)`. Neither shed nor shed-mobile mentions craze
anywhere.

**The Tauri desktop.**

- **Lanes:** `desktop/tauri/src-tauri/src/lane.rs` defines
  `LANE_KINDS = ["opencode","gx"]` (`:233`) and `GxConfig`. Lanes are opened
  from a row's stamp, reached through shared `ssh -L` forwards, and run over
  the IPC ops `lane.*` (`ipc.rs:601-608`).
- **UI:** `desktop/tauri/ui/src/components/LanePanel.tsx`.
- **Shared fold:** `crates/shed-app/src/lane_view.rs`.

**Remote command contract.** `tests/machine-transport/` asserts that
remote-command strings composed in Rust (shed-core, Tauri) and in Dart
(shed-mobile) match shared goldens.

**Release state.** The lane work is under `## Unreleased` in
`CHANGELOG.md`. The latest tag is v0.8.2.

### shed-mobile

**Lane dispatch.**

- `LANE_KINDS` is `["opencode","gx"]` (`rust/src/api/lane.rs:90`).
- `build_client` (`:417-483`) is the one place an adapter is named.
- The gx-specific verbs are `gx_probe_remote_command` (`:793`) and
  `lane_refresh_credentials` (`:745`).
- The shed crates are pinned by git rev in `rust/Cargo.toml`.

**Transport.** Dart owns SSH, with one `SSHClient` per machine
(`lib/machines/machine_feed.dart:466`). Two tunnels exist:

- `RoostTunnel` (`lib/ssh/roost_tunnel.dart`) exposes a stable local port
  and runs the remote command again for each accepted connection. This is
  the shape craze needs.
- `LaneForward` (`lib/ssh/lane_forward.dart`) forwards to a remote port, for
  opencode and gx.

Both share `duplex_pump.dart`.

**Create.** There is no model or provider picker anywhere. Create is
`newSessionFromTab` (`lib/features/create/target_picker.dart:77`), then
`CreateRcScreen` (`lib/features/rc/create_rc_screen.dart`). For roost
targets, only the kind and directory are sent; `acceptsKickoff` is false.

**Session screens.**

- **List rows:** `_MachineSessionCard`
  (`lib/features/machines/machine_sessions_view.dart:186`) shows name,
  lifecycle, activity, attention, kind chip and workdir. It shows no model
  and no pending-approval count.
- **Transcript:** `LaneScreen` (`lib/features/lanes/lane_screen.dart`) has a
  composer with queue and interject, cancel, approvals and questions.
- **Controller:** `LaneController` (`lib/lanes/lane_controller.dart`) owns
  reconnection and backoff.

**Preferences.** The only stored UI preference is the theme
(`lib/theme/theme_mode_provider.dart`).

### craze

**Hub methods.** The hub (`craze bridge --hub`, which starts a hub if none
runs) serves:

- `sessions.subscribe`: the roster at a cursor, then net-change `roster`
  notifications, with an epoch to reseed on;
- `session.connect`: splices the connection to a host, after which it is the
  host protocol;
- `session.create`.

**Create parameters.** `session.create` takes `cwd`, `prompt?`,
`provider?`, `model?`, `effort?`, `fast?`, `permissionMode?` and
`requestId?`, which makes it retry-safe. The created session runs headless
in its own host.

**Roster rows.** A row carries everything a phone list needs without
attaching:

- title, workspace, provider, `providerSessionId`;
- `activity` and `pendingAsks`, the two independent signals;
- `headAsk.summary`, `doing`, `lastReply`, `since`, `lastTurn`;
- `startFailed`/`startErr`, `model`, and `attached` (the client count).

**Session settings.** Each session sends its settings: the model, the mode,
and the current model's config options, each one
`{id, name, category, type, current, selectValues[]}`. Effort-like options
are category `thought_level`, and fast and context are `model_config`.
cursor's options depend on the model: for example, claude-opus-5 offers
thinking, context, effort and fast, and composer-2.5 offers only fast. The
model catalog comes with the session info (native's with recent ranks), and
`session.set{kind: model|mode|config, id?, value}` changes any of them, with
a revisioned state delta to every attached client. Native sessions also
carry a usage section (`contextTokens`, `contextWindow`).

**What is missing for shed.** There is no wire call that says which
providers can start, or the default provider, before a session exists.
Today the TUI computes that in process:

- the provider registry (`internal/agent/provider.go:400`);
- gx hidden when missing (`internal/cli/tui.go:442`);
- cached ACP model catalogs (`internal/modelcache`).

**The roost link.** craze reports to roost under ownership
`(source "craze", session_id)`, where `session_id` is the **provider's**
session id (`internal/tui/host.go:51`). The hub's row carries the same value
as `providerSessionId`, so a roost tab links to its hub row exactly, with no
new field.

**Test doubles.** `craze-fake-host` serves fixture sessions and can list
itself in a registry (`--registry`, `--host-id`, `--session-id`). The wire
fixtures are `internal/fakehost/testdata/wire/` (01–23), and the schema is
published under `docs/reference/protocol/schema/`.

## Architecture

```text
 phone (Dart owns SSH)                      desktop (Tauri)
   stable local port per machine              local machine: subprocess
   ── exec "craze bridge --hub" ──┐           remote: the same exec tunnel
                                  ▼
                    craze hub (one per machine, per HOME)
                    sessions.subscribe · create options · session.create
                    session.connect ──► host (craze serve) ──► agent (ACP / native)
                                         ▲
                     craze TUI in a roost tab is just another client
```

### The two-level contract (indicative; Phase B's plan pins it)

**Source**, one per machine (and, for opencode, one per server):

```rust
#[async_trait]
pub trait AgentSource: Send + Sync {
    fn kind(&self) -> &str;                                    // "craze", "opencode"
    async fn create_options(&self) -> Result<CreateOptions, LaneError>;
    async fn subscribe(&self) -> Result<SourceSubscription, LaneError>; // Reset/Ready/Upsert/Remove/Down
    async fn create(&self, req: LaneCreate) -> Result<LaneSession, LaneError>;
    async fn open(&self, session_id: &str) -> Result<Arc<dyn AgentLane>, LaneError>;
}

pub struct CreateOptions {
    pub default_provider: Option<String>,
    pub providers: Vec<LaneProvider>,     // in craze's order
    pub recent_dirs: Vec<String>,
}
pub struct LaneProvider {
    pub id: String,
    pub label: String,
    pub state: ProviderState,              // Ready | NeedsSetup | Unavailable
    pub reason: Option<String>,
    pub fix: Option<String>,
}
pub struct LaneCreate {
    pub cwd: String,
    pub provider: Option<String>,
    pub prompt: Option<String>,
    pub request_id: String,               // retry-safe create
}
```

**Lane**, one per open session:

- **Per-session capabilities:** `interject`, `cancel`, `approvals`,
  `history_cursor`, `settings`, `modes` and `stop`.
- **Methods:** the existing `history`, `subscribe`, `send`, `cancel`,
  `approvals` and `answer`, plus `settings`, `set` and `stop`.
- **Events:** a `Settings` event joins `LaneEvent`.

`sessions()` and `create()` move to the source.

**`LaneSession` gains these fields,** all `Option`, per the FRB rule:

- `provider` and `model`;
- `doing`, `head_ask_summary` and `last_reply`;
- `since` and `attached`;
- `start_error`;
- a `terminal` link when a roost tab hosts the session.

**Settings** are ACP's own config-option shape, so any ACP agent fits it:

```rust
pub struct LaneSettings {
    pub model: Option<String>,
    pub models: Vec<LaneChoice>,           // craze: current first, then ranked, then the rest
    pub mode: Option<String>,
    pub modes: Vec<LaneChoice>,
    pub options: Vec<LaneSetting>,         // the current model's own
    pub usage: Option<LaneUsage>,          // contextTokens / contextWindow, native
}
pub struct LaneChoice { pub id: String, pub name: String, pub rank: Option<u32>, pub description: Option<String> }
pub struct LaneSetting { pub id: String, pub name: String, pub category: String, pub current: String, pub values: Vec<LaneChoice> }
pub enum LaneSettingChange { Model { id: String }, Mode { id: String }, Option { id: String, value: String } }
```

**How opencode maps:**

- **Source:** discovered as today, from the roost stamp's server URL.
  `subscribe` is its root-session list. `create_options` returns one implicit
  provider (or, later, its `/config/providers`). `create` is today's.
- **Lane:** today's, with `settings: false`.

### craze session rows and roost

**The rule.** For each machine whose hub feed is live, craze rows come from
the hub. A roost row whose ownership is `(craze, X)` folds into the hub row
whose `providerSessionId` is `X`: the row shows the hub's status, and adds
the tab's terminal actions. On a machine with no hub feed (craze not
installed, too old, or unreachable), roost's craze row shows as it does
today.

**What follows from it:**

- Headless sessions are listed, including ones from `craze new`, from the
  phone, and ones left running after their terminal closed.
- A machine with ssh and craze but no roost still shows its craze sessions.

### Provider availability (Phase A)

**The check.** It is computed fresh on each request. It is cheap: binary
resolution through `[agents]` then `PATH`, plus a read of native's stored
keys.

| provider | ready | otherwise |
|---|---|---|
| cursor | `cursor-agent` resolves, and the spawning process is in the GUI login session on macOS | `unavailable`: "cursor-agent not found: install it or set `[agents].cursor`", or "this hub runs outside the macOS login session; cursor needs the login keychain" |
| grok | `grok` resolves | `unavailable`, with the fix |
| gx | `gx` resolves | omitted (a personal fork) |
| native | at least one provider key, a ChatGPT plan sign-in included | `needs_setup`: the TUI's pick opens `/connect`; a remote client shows "set up a key on the machine: `craze auth login`" |

**How it is served:**

- The TUI uses it in process: the startup picker and the session list's
  `/provider`.
- Clients get it over the hub's create-options call, with the default
  provider and recent directories.
- On macOS the cursor rule uses the spawner's login session. That is the
  hub's for a create, and the TUI's own for its spawns. A TUI started over
  ssh on a Mac has the same limit.

### Create flow

**Phone and desktop:**

1. Pick the machine, then craze.
2. **Provider:** ready providers are selectable, unavailable ones dimmed
   with the reason, and the default is preselected.
3. **Directory:** pick a recent directory or type one.
4. **First prompt:** optional.
5. Create.

**What create does.** The source sends `session.create`:

- `provider`, `cwd` and `prompt`;
- no model, which means the provider's default;
- craze's default permission mode;
- a fresh `requestId`, so a retry after a dropped connection does not make
  a second session.

**The result.** The roster row arrives, the session runs headless, and the
sheet opens its transcript. A refused first prompt or a failed start is
shown from the create result (`prompt`, `promptError`, or the start
failure's `data.cause`).

### Settings sheet (Phase B's last milestone)

**Where it lives.** The transcript header shows a compact chip, for example
`Opus 5 · high · fast`. Tapping it opens the sheet. The sheet is hidden
where the session's `settings` capability is false.

**The model row.** It lists the current model first, then the recently
used ones by rank, then the rest. This is the order craze's protocol
prescribes.

**The current model's options,** in this order:

1. effort-like (`thought_level`: effort, reasoning, thinking);
2. fast;
3. context;
4. anything else, in the provider's order.

Up to four values show as a segmented control; more show as a list.

**Mode:** agent, plan or ask, where the provider has modes.

**Behaviour:**

- **A model change redraws the options:** cursor answers with the new
  model's catalog, and the sheet re-renders from the settings event.
- **Changes apply immediately.** A refusal shows inline on its row.
- **Clients stay in sync:** a TUI attached to the same session shows the
  change, and the reverse.
- **Native sessions** add a small context meter.

### Transport and the remote command

- **Connections:** one roster connection per machine with craze, and one
  connection per open transcript (craze serves one attached session per
  connection, so there is no multiplexing). Each is an exec channel on the
  machine's existing SSH connection.
- **Resolving `craze` under ssh:** an ssh exec has no login `PATH` (craze
  SD-27). The remote command needs a resolver chain like roost's, composed in
  Rust and Dart alike and added to `tests/machine-transport/` scenarios and
  goldens. Candidate locations, in order: `PATH`, `~/.local/bin`,
  `/opt/homebrew/bin`, `/usr/local/bin`, `~/go/bin`.
- **craze not installed:** when nothing is found, the machine has no craze
  source. That is a quiet state, not an error.
- **Version:** the hub's `hello` carries the protocol and capabilities. A
  hub without the create-options capability (a craze from before Phase A)
  still lists and controls sessions, and create is hidden there with
  "update craze on this machine".
- **The desktop's own machine:** the desktop runs `craze bridge --hub` as a
  subprocess, which starts a hub when none runs. On a Mac, that hub is born
  in the GUI login session, so cursor creates work from the phone too while
  it lives.
- **Lingering connections:** a dropped bridge is reaped within about a
  second once the hub has seen the client close (craze Plan 035). One
  residual: a client that half-closes and then drops over Tailscale SSH
  lingers until the host's next write (craze SF-139). The reconnect loop
  should expect it.

## Retiring gx and the direct-agent kinds (Phase B, first milestone)

**What goes:**

- `crates/shed-gx`, and the Tauri `GxConfig` and gx lane path.
- shed-mobile's gx probe, credentials and refresh verbs.
- The `gx` entries in both `LANE_KINDS`.
- shed's kind-specific code for `Cursor`, `Codex`, `Grok` and `Gx`: status
  mapping, auth hints, features and launch entries.

**What stays:**

- **Plain rows.** A roost tab running one of those agents directly shows
  through the generic path: a plain row with roost's activity and
  directory. On the desktop the row opens the tab's terminal.
- **Desktop launch.** Launching an agent's own TUI in a new tab stays a
  generic "run in a tab" option there, not a kind integration.
- **roost** is untouched.

**For the plan.** `RcKind` appears in shed's RC wire. The plan decides
whether the retired variants fold into `Other(String)` at display only, or
leave the enum.

## Work breakdown

### Phase A: craze (its own plan, in craze, first)

1. **The availability check** in craze, with the three states, reasons and
   fixes. The TUI's startup picker and `/provider` render the states (dim,
   reason, `/connect` for native). The never-empty-picker rule is removed.
2. **The hub's create-options call:** providers with state, the default
   provider and recent directories, behind a connection capability. It adds
   schema, a wire fixture (24) and fake-host support, documented in
   `protocol.md`.
3. **A `craze providers` command** (human and `--json`), for checking a
   machine over ssh. Optional.
4. **A hermetic recipe for shed's tests:** the real `craze` hub listing
   `craze-fake-host` entries, and creates spawning `craze-fake-agent`
   through `[agents]`. craze's plan verifies it and documents it.

Nothing is needed for settings: the wire already carries them.

### Phase B: shed and shed-mobile (one plan, run from shed, one PR per repo)

1. **Retire** gx and the direct-agent kinds (above).
2. **Split the contract:** `AgentSource` and the session-level `AgentLane`
   with per-session capabilities, with opencode adapted.
3. **`crates/shed-craze`:**
   - the NDJSON client over the exec transport;
   - the roster subscription (epoch reseed);
   - create;
   - the session lane: attach and resume by cursor, the bounded history
     page, send and interject, cancel, asks and answer, stop;
   - the fold into append-only `RcFeedMessage` rows (SD-29), segmented as
     shed-gx did;
   - error mapping.

   Hermetic tests run against craze's fake host and Phase A's recipe.
4. **The Tauri desktop:** a craze source per machine (local subprocess,
   remote exec), the row merge, the create sheet, the transcript, and "open
   in terminal" (a roost tab running `craze attach --session <id>`).
5. **shed-mobile:** the pin bump, FRB DTOs, the craze source over a
   `RoostTunnel`-style exec tunnel, rows (doing, needs you, model), the
   create sheet, and `LaneScreen` for craze.
6. **The settings sheet** in both clients, with the generic settings
   contract. Last, and cuttable to a follow-up with this design.

The size is roughly the 3k lines of Rust estimated in craze's `06`, plus the
contract split, two UIs and the gx deletion (a net reduction).

## Acceptance

All of these hold from the Tauri desktop and from the phone (with the
Flutter desktop build for the hermetic cells):

- **List:** every craze session on a machine is listed, tab-hosted and
  headless, with "doing", "needs you" and the model. A craze session in a
  roost tab is one row, with terminal actions.
- **Create:** a session created with a provider, a recent directory and a
  first prompt runs headless. It appears in `craze ps` and in the TUI's
  session list. An unavailable provider is dimmed with its reason.
- **Control:** read the transcript, send, interject where the provider
  supports it, cancel, answer a permission and a question, and stop the
  session.
- **Resume:** background the phone for a minute and resume with no reseed.
- **Settings:** on a cursor session, change the model, then effort and
  fast. The options follow the model, and an attached TUI shows each change.
  This one only applies if milestone 6 lands.
- **Retirement:** gx is gone, and a codex tab started by hand shows as a
  plain row.
- **Test contracts:** the hermetic lane cells pass on fake hosts, and
  `tests/machine-transport/` covers the craze remote command.

## Follow-ups and open items

- **Cursor from the phone on macOS** (D9). The options:
  - an opt-in `craze hub install` (a LaunchAgent in the GUI domain);
  - shed starting the hub from a GUI-session process of its own;
  - relying on a GUI-born hub (a terminal craze, or the shed desktop's local
    bridge) already running.

  This needs its own decision; the MVP dims cursor.
- **Model at create.** It is deferred by D6. The create-options call can
  later carry each provider's cached catalog for a model row.
- **Resuming saved sessions from the phone.** The hub lists only running
  sessions. The TUI's session list can also reopen saved ones, so a client
  resume needs a hub call of its own.
- **"Needs you" notifications while the phone is backgrounded:** not in
  scope.
- **Remote scopes** (craze SQ11, SD-16). Any client that reaches the bridge
  has the user's full rights, and the phone's loopback port has no peer
  identity, the same accepted risk as roost's tunnel.
- **opencode and codex into craze:** later. Once they are there, shed's
  agents are craze plus Claude.
- **One roster connection per machine:** opened eagerly or only while its
  machine is visible. Phase B's plan decides.

## References

**craze:**

- `docs/reference/protocol.md`: hub roster, `session.create`, row facts,
  capabilities, `session.set`, usage.
- `discovery/session-control/06-shed-lane.md` (S3), `08-decisions.md`
  (SD-15, SD-29, SD-37, SD-40 to SD-46), `09-references.md` (t3code),
  `13-follow-ups.md` (SF-126, SF-139).

**shed:**

- `crates/shed-core/src/lane.rs` (the contract and its module doc).
- `docs/desktop/agent-lanes.md` (the shipped lane design, to update in
  Phase B).
- `epics/roost-pivot.md`.

**shed-mobile:**

- `rust/src/api/lane.rs`, `lib/ssh/roost_tunnel.dart`,
  `lib/lanes/lane_controller.dart`, `lib/features/lanes/lane_screen.dart`.
