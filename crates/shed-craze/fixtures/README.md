# `shed-craze/fixtures`

Two kinds of fixture live here, and they carry different claims.

| artifact | what it is | the claim it carries | regenerable offline? |
|---|---|---|---|
| `wire/*.ndjson`, `wire/README.md` | craze's own wire fixtures (`internal/fakehost/testdata/wire/`), vendored verbatim at the sha `wire.PIN` names | **craze's** — what protocol 1 is; CI diffs the tree against craze's | **No.** Re-vendor at a new pin |
| `0.1.0+gx/lane.ndjson` | one lane connection's raw lines from a live gx session through a real craze 0.1.0 hub | **EVIDENCE** — this is what craze actually sends a lane | **No.** A real hub, a real gx, real model quota |
| `0.1.0+gx/fold.golden.json` | what **this crate's** fold makes of that recording | **REGRESSION DETECTION ONLY** — "this is what the fold does today" | **Yes.** `SHED_CRAZE_REGOLD=1`, free |
| `0.1.0+gx/PROVENANCE.md` | a card the recording run writes | when, with what, and what the run saw | written by every re-record |

## The golden is NOT a fidelity proof

The fold is a **port** — of craze's own transcript rules (its wordings, its
`noteTodos`, its restore) onto shed's append-only rows, with shed-gx's
segmenter — but nothing else folds craze's wire into *these* rows, so nobody
but this crate could have minted `fold.golden.json`. A diff in it is a
behaviour change for a human to read, never evidence of agreement with
anything. Read a diff in it as the change: a new row, a moved activity, a
segment split differently — if the diff is not what the code change intended,
the code change is wrong.

## What the recording is FOR — what a hand-written fixture would not think of

`tests/live.rs::the_recording_carries_what_it_is_for` asserts each of these, so
a truncated or tidied re-record cannot silently delete the coverage:

- **A real seed**: an attach answered with a snapshot whose `main` is empty and
  whose `settings` carry gx's real config catalog — a `model`-category option
  (which the settings read excludes: the model row is the model list) beside a
  `thought_level` one — and the info document's catalogs.
- **A real model streaming**: `thought` chunks one word each, then `text` —
  the segmenter's everyday input, which no fixture streams this finely.
- **A real tool call**: five `tool` events on one id, the first with no status
  and a bare `toolName` title, the next renamed, two `in_progress`, one
  `completed` with its output as `contentText` — one `tool_use` and one
  `tool_result` is what the fold must make of them.
- **`meta` deltas** mid-turn (the agent's slash commands, the session title):
  no rows, settings untouched where the section is not one shed reads.
- **The turn brackets** (`turn` started/ended, `done`) and the reply barrier on
  the wire (each `session.prompt` reply after its own `started` event).
- **A stop**: the receipt, then `reset{session_closed}` — on an idle session no
  closing records come between (WIRE/14's shape; the recipe's
  `stop_ends_the_lane_after_its_closing_records` covers the records).

## What is NOT here, and is covered elsewhere

- **Asks** (permission, question, plan). The scratch session runs gx in
  craze's default permission mode, `bypass`, so the tool ran unasked. The ask
  family is covered against craze's real hub through `craze-fake-host`'s ops
  (`tests/recipe_lane.rs`) and cell by cell in `src/fold/tests.rs`.
- **A reconnect** — the silent resume, a refused cursor, a reseed. Timing needs
  a host you can break on purpose: `tests/recipe_lane.rs` (the real hub) and
  `tests/lane.rs`/`tests/overflow.rs` (a scripted one).
- **The flush clock.** The replay folds the recording with no clock, so an open
  streak is flushed only at its end; live, the watcher also flushes after 2 s
  without growth (`tests/lane.rs::a_paused_streak_is_flushed_by_the_clock`).

## The recording is scrubbed BY REFUSAL, not by rewriting

`tests/live.rs::guard` fails the run — writing nothing at all — if the captured
lines carry:

- a run of **32 or more hex digits** (a craze resume token's shape, 128 bits; a
  gx token's is 64),
- a credential marker (`api_key`, `Authorization`, `Bearer `, `"sk-`, …),
- a **well-known token prefix** starting a word with a token's length behind it
  (`ghp_`/`gho_`/`ghu_`/`ghs_`/`ghr_`, `github_pat_`, `sk-`/`sk-ant-`,
  `xoxa-`/`xoxb-`/`xoxp-`/`xoxr-`, `glpat-`, `AIza`), or an AWS key id
  (`AKIA`/`ASIA` and 16 upper-case letters or digits),
- a **JWT** (three base64url segments, the first starting `eyJ`),
- a **high-entropy run** of 40 or more base64 or base64url characters (upper,
  lower and digits; at least 4.3 bits a character; a character class that
  changes at least every third character — what random base64 does and a path
  or a camel-cased name does not), or
- the developer's **home directory** anywhere.

A refusal names the shape and the byte offset, never the value.

Refusing rather than scrubbing is the design: a scrubber that quietly rewrote
its input would turn "the fixture is clean" into a claim about the scrubber,
and the first secret shape it failed to recognise would be committed looking
fine. `the_recorder_refuses_a_secret_or_a_home_path` and
`the_recorder_refuses_every_common_credential_shape` run offline on every
`cargo test` (and re-check the committed recording), so the gate is never
exercised for the first time on the day someone re-records.

The recorder takes the lane's connection **from its `session.attach` on**: the
host `hello` before it carries the host's resume token, so it is never recorded
at all. **What the recording legitimately contains**: the scratch workspace path
(`/tmp/shcz-<pid>-0/work`), the session's UUIDv7 ids, the tool call's id, and the
output of the one command the turn ran (`echo shed-craze-live`). None of that is
a credential.

---

## Regenerating: two INDEPENDENT steps

One costs a real hub, a real gx and model quota; the other is offline, free and
deterministic. **A craze upgrade is: re-record → re-derive → review the golden
diff as the substantive change.**

### Step 1 — re-record the wire (LIVE: a real hub, gx, model quota)

```bash
make craze-binaries                     # prints SHED_CRAZE_BIN_DIR=…
SHED_CRAZE_LIVE=1 SHED_CRAZE_RECORD=1 SHED_CRAZE_BIN_DIR=… \
  SHED_CRAZE_GX=<path to gx> SHED_CRAZE_GX_CONFIG=<a cheap gx config.toml> \
  cargo test -p shed-craze --features test-support --test live record_a_gx_session -- --nocapture
```

It runs the pin's `craze` under the recipe's scratch `HOME`/`CRAZE_HOME`/
`CRAZE_RUNTIME_DIR` (under `/tmp`, 0700; never a developer's own `~/.craze`),
with `[agents].gx` naming the gx binary and the scratch `HOME/.grok/config.toml`
copied from `SHED_CRAZE_GX_CONFIG` (a model whose provider key is in that file —
gx's `xai.api_key` method is satisfied by a placeholder `XAI_API_KEY`, never a
key). It creates a gx session through the source, opens a lane, drives two
cheap turns (a one-word reply, a shell command) and a stop, prints `LEDGER`
lines (its dirs, processes, the session), and tears everything down — the gx
leader gx detaches under the scratch `HOME` included. Without `SHED_CRAZE_LIVE`
it says why it skips and passes, so CI and an ordinary `cargo test` never reach
it.

**On a craze upgrade, record into a directory named for the NEW version — do not
overwrite this one** — and point `tests/live.rs`'s `RECORDING` at it. Only
`lane.ndjson` and `PROVENANCE.md` are written by a run; this README is a
human's.

### Step 2 — re-derive the golden from the committed recording (OFFLINE, free)

```bash
SHED_CRAZE_REGOLD=1 cargo test -p shed-craze --features test-support --test live the_recording_replays_into_its_golden
```

No hub, no gx, no network: it replays the **committed** recording through the
fold and rewrites `fold.golden.json`, then asserts against what it wrote.
Regenerating an unchanged fold reproduces the committed bytes exactly
(`serde_json` orders object keys; the file ends with one newline).

## Reading `fold.golden.json`

| key | what it pins |
|---|---|
| `attach` | the snapshot's cut (`incarnation`, `seq`) and whether its window was cut |
| `capabilities` | the session's `LaneCapabilities` (§3.9) from the attach reply's info document |
| `rows` | every row, numbered by a ring, rows with no time of their own stamped at the epoch |
| `approvals` | every approval the book holds at the end (none in this recording) |
| `settings` | the session's `LaneSettings` at the end: craze's model order, the options less the model row |
| `activity` | the fold's verdict after each event, consecutive repeats collapsed |
| `events`, `end` | how many events were folded, and the reset that ended the stream |
