# RC activity hub — Go→Rust port map (plan 010)

The working reference for porting the RC activity hub (`serve`) from
`internal/ext/rc/` into `shed-broker`'s `rc_hub` module, hosted by the
`shed-host-agent` daemon. The **normative wire** is
[rc-helper.md § The RC activity hub](../extensions/rc-helper.md#the-rc-activity-hub-serve)
— this page maps source to destination and inventories the tests each file must
bring with it, so the port (and its acceptance criteria) are auditable from the
repo alone. The one-shot engine is already Rust (plan 009:
`shed_app::rc_engine`, graduating into its own `crates/shed-rc-engine` at plan
010 H2; kernel in `shed_core::rc_agents`).

Sheds keep the Go guest hub (`shed-ext-rc`, baked into images) — the Go code
here is NOT deleted by the port; it remains the differential oracle
(`tests/rc-parity`, hub family).

## File → module map

| Go source (internal/ext/rc/) | LOC | Rust destination (shed-broker/src/rc_hub/) | Go tests |
|---|---|---|---|
| hub.go — lifecycle, HTTP shell, input gate (`inputAccepted`, 7 arms) | 1024 | `hub.rs` | hub_test.go (28) + hub_input_test.go (17) |
| hub_reconcile.go — the heartbeat; trackedSession; pane-anchor debounce | 651 | `reconcile.rs` | hub_pane_approvals_test.go (13) |
| hub_verbs.go — turn/interrupt/approvals, claim FSM, error envelope | 563 | `verbs.rs` | hub_verbs_test.go (14) |
| hub_messages.go — feed ring + wire vocabulary + sanitize | 350 | `messages.rs` | ring/wire tables (14) |
| hub_events.go — SSE fan-out, subscriber queues, heartbeat | 244 | `events.rs` | in hub_test.go (httptest SSE reads) |
| hub_ingest.go — cursor hook ingest, pre-watcher queues | 214 | `ingest.rs` | hub_ingest_test.go (9) |
| stability.go — pane-stability engine + normalizers | 199 | `stability.rs` | stability tables (4) |
| watch.go — watcher contracts, freshness, `mergedActivity`, correlation, fsnotify | 564 | `watch.rs` | correlation tables (7) |
| watch_tail.go — JSONL line tailer (inode/truncation/rewrite) | 323 | `tail.rs` | tailer tables (8) |
| watch_claude.go — claude fold + transcript correlation | 298 | `watch_claude.rs` | fold tables (fixture-driven) |
| watch_codex.go — codex rollout fold + correlation | 377 | `watch_codex.rs` | fold tables (fixture-driven) |
| watch_opencode.go — opencode pure fold (approval seed/reopen/tombstones) | 941 | `watch_opencode.rs` | fold tables (fixture-driven) |
| watch_opencode_transport.go — SSE/REST transport + verb lane | 1559 | `watch_opencode_transport.rs` | watch_opencode_transport_test.go (47, incl. the fakeOpencode double) |
| watch_cursor.go — push-fed watcher + transcript backfill | 841 | `watch_cursor.rs` | cursor tables (15) + cursor_approval_test.go (13) |

Totals: ~8.1k production LOC, ~9.8k test LOC (239 tests) in scope. The four
folds are driven from the SHARED fixtures `internal/ext/rc/testdata/jsonl/*`
and `testdata/panes/*` — Rust mirrors consume byte-parity-swept copies under
`crates/fixtures/` (the `golden_parity_test.go` sweep mechanism from plan 009).

## Load-bearing invariants (port these as structure, not as comments)

- **Reconcile is single-threaded and the sole writer of tracked state.** Lock
  order `trackMu → ingestMu → watcher.mu`, never reversed; `subMu` independent
  so broadcast can never deadlock reconcile; per-slug input mutexes pruned on
  disappearance. The lock dance (release for heavy work, re-acquire to commit)
  needs a Rust ownership redesign that re-derives the republish and
  input-lock-survives-replacement guarantees.
- **`mergedActivity` is the single most-mirrored function**: watcher verdict
  overrides stability while fresh (settled forever; transitional 30 s;
  `working` 120 s grace then conditional), opencode demoted on transport
  unhealth, pane-episode override, then `DisplayActivity` suppression. The
  input handler re-runs the exact same merge.
- **opencode transport fold-mutation invariants**
  (watch_opencode_transport.go:32-55): one background thread owns read-side
  HTTP I/O and never mutates the fold; `refresh(now)` on the reconcile thread
  is the only fold mutator; the ONE exception is `markApprovalResolved` (HTTP
  handler goroutine, same mutex) closing the same-decision replay window.
  Generation counters gate stale seed/overflow markers.
- **Verb precedence is contract**: 413 body size → 400 body validation → 404
  tracked lookup → 409 capability → 409 lane — with one stream-order nuance the
  goldens pin in both directions: the 16 KiB cap is enforced by the BODY READER
  (`MaxBytesReader` wrapped by the JSON decoder), not a Content-Length
  pre-check, so oversized JUNK earns the 400 (the parse fails before the cap
  trips) while oversized VALID JSON reads past the cap and earns the 413. A
  Rust hub that rejects on Content-Length up front diverges on the junk cell.
  `GET /v1/sessions` lists from tmux one-shot; `/messages` and the verbs use
  the reconcile-built tracked map (the harness's `wait_tracked` polls for the
  activity overlay for exactly this reason).
- **Pane anchors are reconcile-only** and match the VISIBLE frame (no
  scrollback), debounced 2 ticks both ways, one monotonic `pane-<n>` id per
  episode, silent abandon on blocking states. Cursor's `ComposerUnderModal` +
  anchor exhaustiveness test are auto-approval-safety-critical.
- **Bind-as-lock + health identity**: binding the addr IS the lock; EADDRINUSE
  → `GET /v1/health` and only `app == "shed-rc-hub"` (byte-frozen) counts as a
  hub.

## Kernel gaps closed by the port (flagged in-source pre-010)

`captureVisiblePane` (+`Checked` variants), `capturePaneChecked`,
`listSessionNamesChecked`, tmux `set_environment` (the
`SHED_RC_AGENT_SESSION` back-write), `agentSessionEnv`/`opencodePortEnv`
readers, `ApprovalAnchor` + `ComposerUnderModal` (with the exhaustiveness
test), and ~10 regexes (spinner glyphs, volatile lines/tokens, `ApprovalIDRe`,
`cursorHookEventRe`, `ocSessionIDRe`, `cursorSessionIDRe`, both approval
anchors) under the GO_SPACE/[0-9] discipline.

## Oracle seams (sanctioned, env-gated, inert unless set)

`SHED_RC_HUB_ADDR` (loopback-only, concrete port 1–65535) and
`SHED_RC_HUB_{ACTIVE,IDLE,QUIET,IDLE_EXIT,HEARTBEAT,WRITE_TIMEOUT}_MS`, wired
in `clirc.hubConfig()` — the same protocol as `SHED_RC_NO_HUB`. The Go side
cannot express "never" for idle-exit (`resolve()` maps ≤0 to the 15 m
default), so the harness pins a large finite value on both legs.

## Deliberate machine-posture deltas (not parity debt)

Inside the host-agent the hub is a supervised resident role: no 15-minute
idle-exit, no setsid double-fork, no pidfile; watchers quiesce at zero
sessions. Lifecycle is unit-tested per side, never diffed. On machines the
loopback bind + SSH tunnel IS the authorization boundary (no server proxy) —
see the machine-hub docs.

## Harness (tests/rc-parity, hub family)

Resident daemons on distinct ephemeral `SHED_RC_HUB_ADDR`s with identical
fast-tick tuning; `hub_differential` is Go-only (goldens = the frozen wire)
until the Rust hub lights up, then equality-then-pin like the one-shot cells.
Snapshot cells first (health/sessions/messages/4xx-409 matrix/bare-mux status);
SSE, side-effect, lane (shared fake-opencode with pinGuard) and ingest cells
follow. Fold semantics stay unit-level.
