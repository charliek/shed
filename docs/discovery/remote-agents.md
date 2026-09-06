# Remote agents: the two-lane session model, machine targets, and the Rust porcelain

Status: draft discovery (2026-08-15). Supersedes the Phase E follow-ons of
[agent-sessions.md](agent-sessions.md) (whose Phases A–D are shipped and remain
the substrate this design builds on). Companion research: protocol-maturity
findings summarized inline (cursor ACP, codex app-server, claude auth policy,
opencode server API — verified August 2026; re-verify before each phase since
all four surfaces are moving).

## Motivation

Remote maintenance of agent sessions is the point of this work: kick off a
session anywhere (this machine, an SSH machine, a shed), then watch, steer,
approve, and attach to it from anywhere — phone first. Today that story is
strong for sheds (rc hub + server proxy + mobile watch screens) and weak
everywhere else: machines are reachable only from shed-remote-agent (TS),
cursor sessions have no structured signal, approvals always require the TUI,
and there is no one-command local kickoff.

**Mobile is design-critical even where it is not built first.** Every contract
decision below is validated against the phone use-case: the acceptance bar for
the structured lane is "approve a tool call and steer a turn from the phone";
the TUI lane's known ceiling (activity + gated input + one tap into the
terminal) is acceptable only because the capability model tells the client
exactly which affordances to draw.

Backwards compatibility is explicitly **not** a constraint (experimental
feature); first-party consumers move in lockstep, per the posture already
established in agent-sessions.md.

## Decisions

1. **Two-lane session model.** The rc-tmux session stays the universal
   substrate — every kind, always attachable, survives disconnects, Max-safe
   for claude. Beside it, an agent can offer a **structured lane** speaking its
   native protocol (codex app-server, cursor ACP, opencode server API). One
   session model covers both; clients never branch on lane, only on
   capabilities.
2. **Contract before lanes.** The hub wire contract is extended to a
   lane-agnostic verb + capability shape (below) so we can build either lane —
   or both per agent — without recontracting clients.
3. **Cursor first, in the TUI lane.** Cursor is the preferred TUI; its
   structured lane (ACP) is not ready (session resume broken server-side, see
   research notes). Cursor-first means hooks + transcript hardening of the TUI
   lane. The structured-lane *prototype* is opencode or codex app-server,
   chosen at build time (both are viable; codex's protocol is the most mature,
   opencode's primitives are the richest but self-labeled experimental).
4. **Rust porcelain, new crate.** The local/remote kickoff CLI is a new Rust
   binary in `crates/` on `shed-core`/`shed-app` (the `RcRunner` seam was built
   as exactly this boundary). Thin v1: it *drives* the existing Go engine
   binaries. Structured-lane protocol clients are written as Rust crates so the
   porcelain, Tauri, Swift (FFI), and Flutter (FRB) all reuse them.
   **Shipped as `sx` in R2** — with one deliberate escalation on "thin": the
   one-shot engine is *ported* to Rust for local targets rather than shelled out
   to, with the Go binary retained as a differential oracle (see
   [The Rust porcelain](#the-rust-porcelain)). The hub is not ported.
5. **Shed-first, machines over time.** Machine reach starts client-side
   (SSH exec + SSH port-forward of the hub), inheriting into mobile/Tauri via
   shed-core. Longer-term the machine-side hub/adapters can fold into
   `shed-host-agent` (Rust, already resident on machines). **`shed-machine-rc`
   (Go) is deletable** once absorbed — the RC convention/wire is the invariant
   that must survive, not any binary. **R2 pinned the "once":** the hub was
   deliberately left out of the Rust port, so `shed-machine-rc` stays — machine
   hub provider and parity oracle — until the `shed-host-agent` hub lands (R5).
6. **t3code: learn, don't adopt.** (MIT, `~/projects/t3code`.) Its
   orchestration verbs (`thread.turn.start/interrupt`, `thread.approval.respond`,
   `thread.user-input.respond` — "callers name a thread, not an agent"), its
   connection-layer remote model (stable environment id + advertised endpoints
   + pairing codes; "remoteness never splits the runtime"), and its per-agent
   protocol adapters are the reference. Embedding its Node/Effect server was
   evaluated and rejected (runtime weight, contract coupling).
7. **Happy (MIT, `~/projects/happy`) is the closest product to the goal** and
   contributes a third pattern: *one session, two runners, switched over
   time*. `happy claude` runs the real interactive CLI locally; phone takeover
   stops the TUI and resumes the SAME session via the Agent SDK with a
   `canCallTool` callback (approvals on the phone); a keypress flips back to
   local. Continuity rides the agent's own `--resume` persistence; a
   per-machine daemon spawns sessions on request from the phone; an
   E2E-encrypted, self-hostable relay carries messages + push notifications.
   Implication adopted below: the contract must not preclude **lane
   transitions** (lane is session *state*, not identity) even though v1 fixes
   the lane at create. Caveat: Happy's remote mode draws Max's metered SDK
   credits — reinforcing remote-control as our claude default.
8. **License hygiene.** opencode, t3code, happy: MIT (study + borrow).
   ACP protocol: Apache-2.0. herdr: AGPL — concept inspiration only, never
   code. cmux: GPL-3 — read only.

## The contract (v2 of the hub wire)

> **Status: SHIPPED (R0, 2026-08-16).** This section is the original design
> sketch, kept as rationale; the **normative as-built contract lives in
> [`docs/extensions/rc-helper.md`](../extensions/rc-helper.md)**. As-built
> deltas from the sketch (panel-review outcomes): `post_input` is retained
> (nothing supersedes pane-typed input — only `watch` is deprecated, and it is
> now *derived* from `feed`); sessions additionally gained a
> `pending_approvals` snapshot so an actionable approval survives feed-ring
> eviction; the verbs reject with a single 409 vocabulary
> (`not_supported` = never per capabilities, `not_accepting` = not now — no
> 501s) and their **success shapes are pinned now** (202 `{turn_id}` /
> 202 `{interrupting}` / 200 `{resolved, decision}`) so R3 cannot recontract
> them; approval ids have a contract grammar
> (`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`).

The existing hub wire (sessions / activity / messages / gated input / SSE) is
already agent-neutral; it grows into a verb + capability contract. All changes
are additive to the DTO but the semantics may break freely (experimental).

### Session model

```text
Session (existing DTO) gains:
  lane: "tui" | "structured"        # how the session is executed/observed
```

**Decided (R0): a kind is lane-homogeneous.** All sessions of one kind share
one lane, so kind-keyed `kind_features` stays a complete description;
structured lanes arrive as **distinct kinds** (e.g. a serve-backed opencode
kind beside the TUI one). `lane` is still current-*state*, not identity — the
Happy-style takeover/handoff (one session switched between runners over the
agent's native resume) stays contract-compatible via a future session-level
capability overlay, deferred until a phase builds it. Both lanes register
identically: slug, kind, lifecycle state, activity, feed. Structured-lane
sessions may have no tmux pane; TUI-lane sessions may have no turn semantics.
The capability matrix is what tells clients the difference.

### Per-kind (× lane) capabilities

`kind_features` grows from `{post_input, approvals, watch, input}` to:

| Field | Values | Meaning |
|---|---|---|
| `feed` | `none` \| `activity` \| `messages` | what the hub can stream for this kind |
| `input` | `none` \| `gated` \| `turn` | `gated` = composer-anchored send-keys (today); `turn` = real turn semantics |
| `approvals` | `tui` \| `remote` | `remote` ⇒ approval entries appear in the feed and `approval.respond` works |
| `interrupt` | bool | `turn.interrupt` supported |
| `attach` | `tmux` \| `native-remote` \| `none` | how a terminal reaches it (`native-remote` = e.g. `opencode attach`) |

### Verbs

Existing: `create / list / probe / kill / input (gated) / accept-trust`.
New (structured lane implements; TUI lane 409s where unsupported):

```text
POST /v1/sessions/{slug}/turn          start a turn (prompt + options)
POST /v1/sessions/{slug}/interrupt     interrupt the active turn
POST /v1/sessions/{slug}/approvals/{id}  respond {decision: allow|allow_always|deny}
```

Feed messages gain typed approval entries (`type: "approval_request"`, with a
stable `id`, tool name, and detail) alongside the existing
`text|tool_use|tool_result|reasoning|status` rows. `activity` may now emit the
already-reserved `needs_approval`.

### Where adapters live

Lane adapters run **where the sessions run** — in the hub (guest-side
`shed-ext-rc serve` for sheds; machine-side hub for machines) — normalizing to
this one wire. Clients stay thin. This mirrors t3code's "complexity belongs at
the adapter boundary" and avoids N clients × M sessions protocol connections.
(Client-side Rust protocol crates still exist — the porcelain uses them for
*local* structured sessions where no hub hop is needed, and mobile may use
them later if a direct path ever wins.)

## Per-agent lane plan (research-grounded, Aug 2026)

| Agent | TUI lane (today → hardened) | Structured lane | Notes / risks |
|---|---|---|---|
| **claude** | classifier + transcript-tail activity (shipped); stays primary | **None planned.** Official remote-control is the remote surface (Max-safe: watch/steer/approve from claude.ai + mobile apps) | Anthropic blocks Pro/Max OAuth in third-party SDK/headless use (enforced 2026-04); subscriptions instead carry a **metered Agent SDK credit pool** (2026-06). A claude structured lane is therefore *possible but metered* — cost decision, deferred |
| **cursor** | **Harden first**: hooks (`~/.cursor/hooks.json` — `beforeShellExecution`/`preToolUse` fire in CLI and can veto) → hub activity + `needs_approval`; transcript tail (`~/.cursor/projects/<p>/agent-transcripts/*.jsonl`, claude-style JSONL, tool *outputs excluded*) → partial message feed | `cursor-acp` later: permissions + tool streaming work over ACP, but **`session/load` is broken server-side** (confirmed by Cursor staff 2026-03, unresolved), no fork, turn-completion hooks contested in headless. **t3code ships cursor-ACP in production anyway** by keeping a long-lived ACP child per session — `session/load` is attempted only on restart, guarded by their own timeout + replay-idle-gap — so the lane is viable under "process lifetime = session lifetime, resume best-effort" (exactly the invariant a hub-resident lane gives us) | Re-test `session/load` before building the ACP lane; hook coverage claims vs. staff statements conflict — verify live |
| **codex** | rollout-JSONL feed + gated input (shipped) | `codex app-server` (stdio/unix-socket): thread/turn/item, `turn/steer\|interrupt`, real approval RPCs, `thread/resume|fork` — **most mature protocol of the four** | `codex remote-control` pairs only with OpenAI's closed relay (their apps) — not our transport. ChatGPT-subscription auth for automation: permissive in practice, formally undocumented — offer as opt-in, document the ambiguity |
| **opencode** | SSE watcher via per-session `--port` (shipped — already half a serve-split) | serve-backed sessions via its HTTP/SSE API: prompt w/ `steer\|queue` delivery, `/interrupt`, permission + question reply endpoints; remote TUI via `opencode attach` | The needed session/permission/question routes are opencode's self-labeled **experimental v2** — but **t3code ships on exactly that surface** (`@opencode-ai/sdk/v2`, spawned server per session torn down with the session scope, consuming `permission.asked/replied` + `question.asked` events), so "experimental" is load-bearing in a 100k-user product. `attach` has two maintainer-declined bugs (password attach broken; infinite hang on server death) — front sessions with our hub, don't depend on raw `attach`; pin the SDK/API version |
| **shell** | as shipped | n/a | |

ACP is real and maturing (cross-vendor org, Apache-2.0, v0.13.x, JetBrains +
Zed native) but unifies only cursor + opencode among our agents — hence
per-agent native lanes behind one contract, not a single-protocol bet.

## The Rust porcelain

> **Status: SHIPPED (R2, plan 009, Aug 2026)** — the binary is **`sx`**
> (`crates/sx`), documented end-user-style in
> [`docs/extensions/sx.md`](../extensions/sx.md). What the block settled, beyond the
> sketch below:
>
> - **Name: `sx`.** Short enough for a skill (and a human) to type constantly, no
>   collision with `shed`/`shed-server`/`shed-agent`/`shed-ext-*`/`shedctl`/
>   `shed-host-agent` or any brew formula in the tap. It ships in **no release
>   component** yet — built from `crates/`, installed by hand.
> - **The engine is ported, not driven.** The sketch's "thin v1 drives the Go
>   binaries" became a real one-shot engine port: `shed-core::rc_agents` (registry,
>   classifiers, env/DTO shapes) + `shed-app::rc_engine` (tmux, ops, plan, preseeds,
>   capabilities) behind `sx rc <subcommand>`, wire-compatible with
>   `shed-machine-rc <subcommand>`. Remote targets still exec the far side's RC
>   binary over SSH — that IS the wire.
> - **The `serve` hub stays Go — settled, not deferred.** *(Superseded by plan
>   010: the hub is now ported into `shed-broker::rc_hub` and hosted by
>   `shed-host-agent` — exactly the "future home" named here.)* Its future home is
>   `shed-host-agent` (the v2 brokered direction below), so it was deliberately left
>   out of the port. `sx` best-effort spawns `shed-machine-rc serve --detach` on
>   create and reads that hub over loopback (or an `ssh -L` tunnel for a machine).
> - **Consequence: `shed-machine-rc` is NOT deleted.** *(Superseded by plan 010
>   H15: with the host-agent hub live and e2e-proven, the component IS retired —
>   the oracle main relocated to `tests/rc-parity/oracle/`, the binary/selector
>   deleted, `sx --on machine:` defaults to the remote `sx rc` argv.)* It survives
>   as the machine hub provider *and* as the parity oracle. Its VERSION manifest
>   and release component are untouched. Retirement stays parked behind the
>   host-agent hub (R5).
> - **The mixed-fleet guarantee is a harness, not a promise.** `tests/rc-parity/`
>   (51 cells, the fourth pytest suite) runs each scenario against both binaries,
>   requires the normalized results to agree, and pins the agreed value to a golden —
>   structural canonical JSON for DTO stdout, **raw bytes** for preseed artifacts
>   (`~/.claude.json`, `~/.cursor/hooks.json`, the hook script, plan files), which a
>   mixed fleet rewrites in place. Cross-impl interop cells create with one
>   implementation and probe/list/prompt/kill with the other, both directions.
> - **Consumer audit (informational, keep-by-default).** `claude-broker` *creation*
>   is live in exactly two places — `shed attach --kind claude-broker` and
>   shed-remote-agent's New Shed page; everywhere else it survives as a
>   display/classification kind. Standalone `accept-trust` is fully consumer-dead
>   (trust is handled by the create-time preseed and the `--wait` poller's inline
>   Enter). Both were ported unchanged; dropping either is a wire-contract change for
>   a later block.
> - **Follow-ups recorded:** wire the host-agent token minter into `sx` (an mTLS-only
>   server today needs the `shed:<name>@<server>` form); add a cursor
>   workspace-trust anchor + auto-accept as a lockstep Go+Rust change (current
>   `cursor-agent` builds open a trust dialog neither implementation classifies, so
>   both read `starting`); `sx` steering verbs off the hub/proxy wire; `sx ls`
>   fan-out concurrency (`--fast`); engine-crate graduation once it has two real
>   consumers; `sx` auto-install from skills and a release component for it.

> **Refined direction (plan 008, Aug 2026).** The porcelain is a **Rust tool called
> from shed skills** — the entry point is a Claude Code (or other agent) skill kicking
> off a session from a local machine into a shed or a remote machine, doing the
> environment setup a raw `tmux`/`ssh` invocation can't (auth checks, workdir
> resolution, posture flags), with eventual auto-install so a skill never assumes the
> tool is already on the caller's machine. The **engine logic is ported to Rust
> first**, with `shed-machine-rc` (Go) kept running as the **reference oracle** during
> the port — the same golden-harness pattern `tests/host-agent-diff` established for
> the Go→Rust host-agent migration (record goldens from the last agreeing Go↔Rust run,
> assert the new Rust engine's wire-visible output against them under a defined
> canonicalization) is the template. `shed-machine-rc` is **deleted once the Rust
> engine reaches parity**, not before. **Crate-split rule**: split only on
> dependency/consumer boundaries, never speculatively — `shed-core` stays the pure
> kernel (no I/O-heavy or lane-specific logic), the rc engine starts life under
> `shed-app`'s `rc` feature (one crate, feature-gated) and graduates to its own crate
> only once it has **two** real consumers (the porcelain CLI and, e.g., the desktop
> app), and lane protocol clients (opencode's SSE/REST client, a future codex
> app-server client, a future cursor ACP client) are separate **leaf crates** from day
> one, since they are reused by both the CLI and any future direct-connect mobile
> path.

New crate in `crates/` — **`crates/sx`, binary `sx`** — on `shed-core` + `shed-app`.
As built (the sketch's `--shed` spelling became the one uniform `--on`):

```bash
sx agent codex                           # local machine, auto posture; prints watch/attach info
sx agent claude --on machine:mini2       # over SSH (engine binary on the machine)
sx agent opencode --on shed:mytopic      # in a shed (SSH to the guest helper)
sx ls                                    # unified sessions: local + machines + sheds
sx watch <slug> / attach <slug> / plan <file> --on ... / kill <slug>
```

- **v1 as built**: the one-shot engine is ported to Rust and runs in-process for
  `local`; `machine:`/`shed:` targets exec the far side's Go RC binary over SSH
  (`shed-machine-rc` / `shed-ext-rc`), which is the wire the desktop app uses too.
  tmux choreography stays where the sessions are. The `claude` convenience verb is
  absorbed by `sx agent <tool>` rather than ported.
- Target model: `local | machine:<name> | shed:<name>[@<server>]` resolved from
  `~/.shed/config.yaml` (+ the `machines:` section it grew; the Go CLI carries it
  as a schema-agnostic passthrough so a config rewrite stops deleting it).
- Scope question (still open): agent porcelain only, or eventually *the* shed client
  CLI absorbing the Go CLI's client half. Started narrow; nothing forecloses the
  larger scope.
- Agent workflows (`shed attach --kind`, `shed plan`) migrate here over time;
  the Go `shed` CLI keeps shed/VM lifecycle.

## Machines

- **v1 (client-side)**: machine targets in shed-core — SSH exec for one-shots,
  SSH port-forward of the hub (127.0.0.1:1029) for feed/SSE. Mobile
  (dartssh2 forwarding exists) and Tauri inherit via shed-core; the porcelain
  uses the same code.
- **v2 (brokered)**: `shed-host-agent` grows the machine hub role — Rust lane
  adapters shared with the client crates, one resident daemon per machine, a
  natural aggregation + notification point (the seam agent-sessions.md
  reserved). At that point `shed-machine-rc` (binary + release component:
  VERSION manifest, brew + apt) can be retired.
- Sheds keep the Go `shed-ext-rc` hub (baked into images). Wire contract is
  the invariant across all hub implementations.
- Phone connectivity stays Tailscale-direct for now; Happy's E2E-encrypted
  relay + push (`happy-server`, MIT, self-hostable) is the studied pattern if
  a relay/push path is ever wanted — its per-machine daemon (registers the
  machine, spawns sessions on phone request) is also the closest prior art for
  the shed-host-agent machine-hub role.

## Roadmap

Each phase names its mobile checkpoint — the phone-facing behavior that proves
the phase (even when the mobile UI itself lands a phase later).

> **R0 status: SHIPPED** — PR #308 (`feature/plan-007-rc-contract-v2`,
> 2026-08-16). The landed contract is `docs/extensions/rc-helper.md` (the
> panel-corrected version — `409 not_supported`/`not_accepting`, no `501`s —
> is what the `409` in the R0 row refers to). R1 handoff items recorded in
> plan 007 §9: watcher freshness for `needs_approval` (`watch.go` treats only
> `working` specially), the input-acceptance gate interaction, and in-guest
> verification via the rootfs-rebuild loop, all of which plan 008 (below) resolved
> as part of shipping R1 itself.
>
> **R1 status: SHIPPED** — plan 008 (`feature/plan-008-observatory`). The scope grew
> beyond the R1 row's original "cursor TUI hardening" framing into the full
> observatory block (see the row and [Spike findings](#spike-findings-aug-2026)
> below); the as-built contract is `docs/extensions/rc-helper.md`.
> (`.claude/skills/testing-vm-agent-changes`).

| Phase | Ships | Mobile checkpoint |
|---|---|---|
| **R0 — Contract v2** ✅ | `lane` field, extended `kind_features`, turn/interrupt/approval verbs (409 where unimplemented), typed approval feed entries + `pending_approvals`, `needs_approval` activity; Go hub + server proxy + Rust/Swift/TS mirrors + byte-parity-guarded fixtures in lockstep | Mobile decodes v2 envelope; existing watch screens render unchanged off capabilities |
| **R1 — Observatory (opencode dual-control, cursor/codex signals, kickoff hardening)** ✅ | **Shipped, plan 008** — grew beyond the original "cursor TUI hardening" scope into the full block: opencode's turn/interrupt/approvals verbs live (dual control — the same session stays tmux-attachable while the hub steers it), `needs_approval` + informational approval feed rows for codex and cursor (pane-anchor, debounced), cursor hooks→hub ingestion (activity, turn boundaries, message feed, gated input), and `shed attach`/`shed plan` kickoff hardening (installed-agent gate, plan permission posture, `--workdir`, opencode needs-auth classification). See `docs/extensions/rc-helper.md` for the as-built contract | Cursor session on a shed shows live activity + hook-derived feed; a codex/cursor approval prompt reads `needs_approval` + "open the TUI"; an opencode session is steered (turn/interrupt/approve) through the hub while still tmux-attachable |
| **R2 — Rust porcelain v1** ✅ | **Shipped, plan 009** — `crates/sx`, binary `sx`: `agent`/`plan`/`ls`/`watch`/`attach`/`kill` across `local\|machine:<m>\|shed:<s>[@<server>]`, plus the engine-compat `sx rc <subcommand>`. The one-shot RC engine is **ported** (not merely driven): `shed-core::rc_agents` + `shed-app::rc_engine`, with `shed-machine-rc` kept alive as the machine hub provider and the **parity oracle** — `tests/rc-parity/` (51 cells) diffs both implementations per scenario and pins the agreement to goldens. The hub (`serve`) stayed Go at R2 by decision; **plan 010 then ported it into `shed-host-agent` and retired `shed-machine-rc` entirely** (see R2.5 below). `machines:` config section in shed-core, round-trip-preserved by the Go CLI. Its own release component since R2.6 (brew + apt), still dev-first: packaged, not surface-frozen. See [`docs/extensions/sx.md`](../extensions/sx.md) | n/a directly (CLI), but exercises the same shed-core target model mobile will use |
| **R3 — Structured lane prototype** | a lane adapter in the guest hub behind the v2 verbs, as a **new distinct kind**. Spike is now three-way: **opencode** (front-runner — the hub already speaks its SSE/REST via the `--port` watcher, so the adapter is incremental Go; t3code ships on the same v2 API), codex app-server (most mature protocol, needs a Go JSON-RPC client), cursor-ACP (viable per t3code's long-lived-child pattern, needs a Go ACP client). **Primary design problem: the structured-session registry** — a session with no tmux pane breaks "tmux is the source of truth", so the hub needs its own registry (in-memory + agent-side persistence for resume). **Note (post-R1):** R1 already wired opencode's turn/interrupt/approvals verbs onto the *existing* TUI-lane kind (dual control — no new kind, the session stays tmux-attachable) — see `docs/extensions/rc-helper.md`; R3's "new distinct kind" is for a lane with **no** tmux pane at all (a headless structured session), a different and larger step than R1's dual-control shape | **Approve a tool call and steer a turn from the phone** — the bar for the whole design |
| **R2.5 — Machine hub in the agent + machine-rc retirement** ✅ | **Shipped, plan 010** (PR #312) — the RC activity hub ported Go→Rust into `shed-broker::rc_hub` and hosted by the **`shed-host-agent`** daemon as a supervised resident role (bind-as-lock, `rc_hub.enabled` knob, `rc_hub` LiveStatus field, `rc-hub` foreground diagnostic). Wire parity is mechanical: `tests/rc-parity` grew a HUB family that runs BOTH hub daemons on ephemeral ports and diffs `/v1` (snapshot/SSE/side-effect/opencode-lane/cursor-ingest), 100 cells total. Live e2e on this Mac + mini3. **`shed-machine-rc` is retired**: binary + goreleaser config + selector deleted, component stripped from the release model, oracle main relocated test-only to `tests/rc-parity/oracle/`. The Go engine lives on as `shed-ext-rc` (guest) and as the differential oracle. **Net effect for machines: the hub no longer idle-exits** — it is up whenever the agent is | Machine hub is a stable, always-on, known-address endpoint — the precondition R4 needs |
| **R2.6 — Ship `sx`** ✅ | **Shipped, plan 011** — the retirement's loose end closed: `sx` is its own release component with selector `crates/sx/VERSION` and its own `.goreleaser.sx.yaml`, publishing **brew + apt** (the distribution slot machine-rc vacated) via the same `builder: rust`/`cargo zigbuild` path host-agent uses. `sx version` now reports the release tag (`SX_VERSION` injection) instead of the desktop-tracking `CARGO_PKG_VERSION`. The recommender grew a `NEVER_SHIPPED` first-ship bootstrap — the one genuinely new problem a brand-new component posed. Helper-only-tag shape means an sx tag publishes **no** rootfs images and leaves server/desktop pinned. **Dev-first**: the wiring landed without cutting a tag. **Merged 2026-08-20** (shed `d2c406c`, plus `charliek/apt-charliek` and `charliek/homebrew-tap` housekeeping) and deliberately **UNTAGGED** — see standing decision 3 | Unblocks every machine-target story, including R4's |
| **R4 — Machines in clients** ✅ | **Shipped, plan 012** — machine targets in shed-core surfaced in **Tauri** and **shed-mobile** over an SSH-forwarded hub, with a unified sessions view in both. The transport is a **per-client seam** (`shed_app::machine::MachineForward`), not a shared Rust SSH client: sx/Tauri run the `ssh` binary as a child, mobile uses dartssh2, and the load-bearing invariant is that the local port is STABLE across a re-establish — which turns "tunnel died" into "socket refused" and lets reconnect be written once. Three transports is a standing drift risk, so `tests/machine-transport` was built EARLY, not retrofitted: one committed scenario table + goldens asserted by a Rust leg, a LIVE leg through a throwaway sshd that checks the argv the far side actually received, and a Dart leg in shed-mobile. **The hub-home graduation landed with it**: `rc_hub_role.rs` moved bin-local → `shed_broker::rc_hub::role`, so the Tauri app hosts the hub in-process (a standalone `RcHubHost`, NOT folded into the embedded-broker path — this Mac's daemon is 0.8.1 and predates the hub entirely). `make test-rc-parity` stayed 100/100 across the move. **Four bugs the hermetic suites could not see** were found by running it live, each now pinned: the SSE decoder dropped EVERY machine event (it required a non-empty `shed`, and a machine hub sends `""`); the phone's capability probe ran the guest binary's name on a machine, so every machine session was silently observe-only; `SshForward::ensure()` orphaned its ssh child; and the reconnect backoff never reset on the common path, ratcheting to its 30s ceiling forever. **AC6 passed 2026-08-22**: a prompt typed on a phone reached opencode on mini3 over Tailscale SSH and the model answered it | Machine sessions listed + watchable + steerable next to shed sessions on the phone |
| **R6 — Desktop graduation + `shed-host-agent` disposition** | **Direction set 2026-08-21; a desktop-track block, listed here because it owns the hub-home decision R4 depends on.** The **Swift** app is removed outright and the **Tauri** app graduates (pending more testing). The Tauri crate already links `shed-app { features = ["rc", "broker"] }`, so it embeds `shed-broker` in-process — `broker_bridge.rs` is a drop-in for the `HostAgentClient` UDS path, presenting the same Coordinator surface (approvals, audit fan-in, `TokenMinter`, mtls-capable). So the app already replaces host-agent's **credential-brokering** feature set; it does **not** replace the **hub** (see R4). **`shed-host-agent` then becomes a headless-only survivor** — kept for machines/servers where the user wants no desktop app (mini2/mini3), not deprecated outright. Two consequences to plan for: (a) **its brew-only posture inverts** — it ships brew + a GH linux tarball today, justified as "brew/macOS is the only wired install path", which is backwards for a headless-Linux audience; it wants an **apt deb**, and `sx` has now proven that exact machinery (Rust binary → minimal one-binary nfpm deb, no unit/config/scripts). (b) **the hub's always-on guarantee weakens** on desktops — a launchd daemon with `keep_alive true` is a stronger liveness contract than a user-quittable app; lose it deliberately, not by accident | A desktop user installs one app and no daemon; a headless box installs `sx` + `shed-host-agent` from apt and nothing else |
| **R5 — Second lane + notifier** | second structured lane (from the R3 spike's runners-up); desktop notifier off aggregate SSE. *(Its third item — "host-agent hub spike (start of shed-machine-rc retirement)" — was not spiked but completed outright by plan 010; see R2.5.)* | `needs_input`/`needs_approval` reaches the phone while the app is open (SSE); push is a separate decision |

R0 was deliberately small and unblocks everything; R1–R2 are independent of
the lane bet; R3 is where the two-lane model proves out or gets revised.

**Current direction (Aug 2026).** Two standing decisions that outrank the
original row text where they conflict:

1. **No backwards compatibility for retired binaries.** Published
   `shed-machine-rc` artifacts are not withdrawn, but nothing is designed
   around machines still running them — pick what is best for the current
   tree and let stale installs fail loudly. (The agent's bind-retry stays,
   on its own merits: an agent restart or a dev-alongside-brew agent races
   the same port.)
2. **Development is the target audience, not end users.** Shipping
   machinery (R2.6) exists to make the dev loop fast across this Mac +
   mini2/mini3; a release is cut when development needs one, not on a
   user-facing cadence.
3. **Release trigger (2026-08-21): not until the machine story works
   end-to-end, mobile included.** R2.6's wiring is merged but deliberately
   UNTAGGED. `sx` is buildable from source and that is enough for
   development, so the next tag waits until the story proves out through
   R4 — machine sessions listed and watchable from shed-mobile — rather
   than being cut just because the machinery exists. Corollary: when it
   IS cut, prefer an **`sx`-only tag**. The component model makes that
   exact (a tag ships a component iff its manifest equals the tag), and
   `sx` is the component whose shape is settled — `host-agent` and
   `desktop` are both mid-transition (decision 4) and should graduate on
   their own tags.
4. **The desktop app is graduating and `shed-host-agent` is becoming a
   headless-only daemon** (2026-08-21) — see **R6**. This is the standing
   context for every host-agent decision from here: do not add desktop-
   shaped surface to it, and do not assume it is installed on a machine
   that runs the app.

Sequencing from here: **R4 is done** (plan 012) — the payoff landed, and
with it the hub-home graduation R6 was going to have to make anyway. What
R4 also settled is the RELEASE trigger: the standing rule was "no release
until the machine story works e2e, mobile included", and it now does. The
natural next tag is **sx-only**, with three things riding it (flip
apt-charliek's `sx` to `optional: false`, prune `sx` from the recommender's
`NEVER_SHIPPED`, and SPLIT the `## Unreleased` changelog entry, which still
claims `**Ships:** host-agent, sx`).

**That trigger is HELD, deliberately** (2026-08-22): the e2e bar being met is
necessary, not sufficient. The owner wants to drive the machine story locally
and polish the UX before anything ships — which proved to be the right call,
because the polish pass turned up more than polish:

- a machine session was reachable and steerable but had no way to READ it. The
  rich per-session view existed only for sheds, so the phone offered blind
  Steer/Interrupt buttons on a list card. Nobody steers an agent without seeing
  its output. The view is now transport-agnostic and serves both;
- **shed sessions reported no activity at all** in the desktop app while the
  machine beside them said `needs input`. Activity is a hub-layer overlay the
  one-shot listing never sets, and Tauri had never mounted the watcher built for
  exactly that. Same list, two amounts of truth, decided by how each row was
  fetched;
- the phone's capability probe had never worked against a real machine, so every
  machine session was silently observe-only.

None of those were visible to a green test suite. **The general lesson for this
roadmap: a client-facing block is not done when its acceptance criteria pass —
it is done when someone has driven it.** Budget for the pass after the pass.

A platform split also settled here, and is worth carrying forward: the DESKTOP
leads with the terminal (it has a real one) and a rendered feed view is only
warranted for a kind that cannot be attached — of which there are none today.
The PHONE leads with the rendered view, because its terminal is an in-app
xterm. Parity is per-client, between machines and sheds; it is not cross-client.

**R3** is independent and can slot in whenever the lane bet is due — though
R4 sharpened its case: a phone can now steer an opencode turn, and the one
thing standing between that and "approve a tool call from the phone" is R3's
lane work, not transport. **R6** runs on its own track (a desktop concern);
the decision it owned for R4 — where the hub is hosted once the app, not a
daemon, is the desktop's broker — was made and implemented in plan 012, so
R6 inherits a working answer rather than an open question.

**One operational finding worth carrying forward**: the hub spawns agent
binaries, and it runs as a systemd **user** unit whose PATH is minimal. An
agent the user installed into `~/.bun/bin` or `~/.local/bin` is therefore
invisible to it — `capabilities` reports `installed: false` and a kickoff
fails — with nothing to suggest PATH is the cause. Provisioning must set the
unit's PATH explicitly (plan 012 did, on mini3), and the hub arguably ought
to search the invoking user's login PATH rather than inherit the manager's.

## Spike findings (Aug 2026)

Live/source spikes run for R1 (plan 008), superseding or sharpening the per-agent
table above:

- **cursor hooks fire fully, but only in TUI mode.** `stop`, `afterAgentResponse`, and
  `beforeSubmitPrompt` — the turn-boundary signals — are **TUI-only**; they are absent
  entirely in `-p` (print/headless) mode. `session_id` in every hook payload equals the
  transcript directory/filename, so correlation is exact once a hook fires; there is
  **no approval-pending hook event** at all (confirmed live) — a session blocked on the
  allowlist prompt is indistinguishable from a long-running tool call by hooks alone,
  which is why cursor's `needs_approval` is a **pane anchor**, not a hook derivation
  (see `docs/extensions/rc-helper.md`).
- **opencode's v1 (unprefixed) API is the real control surface; v2 is not usable for
  turn-start.** `POST /session/{id}/prompt_async` and `POST /session/{id}/abort` on v1
  both work as expected (verified live, including mid-turn steering and an idle abort);
  the newer `/api/session/{id}/prompt` (v2) route **admits a turn but never promotes it
  on an idle session** — a dead end for turn-start regardless of how appealing "v2" as a
  name sounds. The **global-store hazard is confirmed and worse than assumed**: one
  TUI's embedded server lists sessions from every directory on the machine, and the
  global permission-reply route can answer another project's ask — this is exactly what
  the [session-scoping invariant](../extensions/rc-helper.md#session-scoping-invariant-hub-initiated-mutations)
  in the as-built contract exists to structurally prevent for the hub's own adapters
  (it does not, and cannot, fix the hazard for anything else talking to that same
  embedded server).
- **codex approvals are provably absent from the rollout JSONL.** The persistence
  policy (`rollout/src/policy.rs` in the codex source) filters every
  approval-request-shaped record before it is ever written to disk, and the
  associated tool-call record is written *before* the approval gate runs — so a
  session blocked on an approval overlay is byte-identical in the log to a
  long-running tool call. A census over hundreds of local rollout files / over a
  thousand turns found **zero** approval-shaped records. This is not a gap that a
  better JSONL parser could close; it is why codex's `needs_approval`, like cursor's,
  is a pane anchor keyed on the approval overlay's stable option-row chrome rather than
  any log-derived signal.

## Open questions

1. ~~**Kind-vs-lane modeling**~~ **RESOLVED in R0**: kinds are
   lane-homogeneous; structured lanes arrive as distinct kinds; `lane` is
   current-state (not identity) so the Happy-style takeover/handoff stays
   contract-compatible via a future session-level capability overlay.
2. **Prototype agent for R3** — now three-way (see the R3 roadmap row):
   opencode (front-runner: incremental Go on the existing `--port` transport;
   t3code ships on the same v2 API) vs codex app-server (most mature protocol,
   auth-policy ambiguity, new Go JSON-RPC client) vs cursor-ACP (t3code-proven
   long-lived-child pattern, new Go ACP client). Decide with a short spike.
3. ~~**Porcelain name**~~ **RESOLVED in R2**: the binary is `sx` (`crates/sx`).
   **Scope stays open** — agent-only today vs eventually absorbing the Go CLI's
   client half; `shed attach --kind` / `shed plan` have not migrated.
4. **Mobile push** — SSE-while-open is the R5 bar; true push (FCM/ntfy/relay)
   is unscoped. Decide when R4 lands.
4b. **How `sx` gets an mtls credential once host-agent leaves desktops (R6).**
   `sx` has TWO host-agent touchpoints, not one — the hub (`sx watch`) and,
   less obviously, the **control-token minter**: `crates/sx/src/backend.rs`
   wires `HostAgentTokenMinter` "exactly as the desktop's external mode does",
   gated on the agent answering. So `sx` inherited the **desktop** client's
   credential strategy rather than the Go CLI's — the Go CLI instead reads a
   credential STORE (`internal/config/clientcreds.go` → `~/.shed/creds`, material
   the server issues over the SSH bootstrap channel; no daemon). Scope today is
   narrow: only the HTTP fan-out (`sx ls`, unqualified `shed:<name>`) wires the
   minter; the qualified `--on shed:<name>@<server>` form is pure config, no HTTP.
   **Under R6, a desktop `sx` with no host-agent loses the minter** and
   unqualified `shed:<name>` against an mtls server fails closed. Escape hatch
   already exists and should be evaluated first: `shed-core`'s config supports
   `client_cert_file` / `client_key_file`, which is exactly the
   `~/.shed/creds/secure/client.{crt,key}` layout the Go CLI uses — i.e. teach
   `sx` the CLI's daemon-free store path rather than inventing one.
5. **Structured-session registry** — promoted to **R3's primary design
   problem** (see the roadmap row): a session with no pane breaks "tmux is the
   source of truth"; the hub needs its own registry (in-memory + agent-side
   persistence like codex `thread/resume` / opencode server sessions), plus a
   decision on how such sessions appear in `shed sessions`.
6. **Auth posture per lane** — codex ChatGPT-auth opt-in wording; claude SDK
   credits (if a claude lane is ever wanted); opencode server password
   handling when the hub fronts it.
