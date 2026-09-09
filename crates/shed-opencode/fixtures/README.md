# `shed-opencode/fixtures`

Two goldens live here. **They prove different things, and conflating them would
turn a fidelity claim into a tautology**, so read this table before reading
either file:

| golden | input | minted by | the claim it carries | regenerable offline? |
|---|---|---|---|---|
| `opencode_turn.golden.json` | `crates/fixtures/jsonl/opencode_turn.jsonl` (32 lines, opencode **1.17.15**) | the **rc hub's** fold | **FIDELITY** — the port of `OpencodeFold` into this crate changed no behavior | **No** — see below |
| `1.18.29/fold.golden.json` | `1.18.29/event-frames.jsonl` (155 frames, opencode **1.18.29**) | **this crate's** fold | **REGRESSION DETECTION** — "this is what the fold does today". Its *transcript subset* was separately proven identical to the hub's; its approval DTOs and its `session.error` row were not, because the hub has no counterpart for them | **Yes** |

Both are asserted by `tests/fold_fixtures.rs`, whose module doc repeats the
split. Neither test may ever take a `shed-broker` dependency: a port that links
the thing it ported proves nothing, and the hub's copy is scheduled for deletion
in S6. `cargo tree -p shed-opencode -e normal | grep -c shed-broker` is `0` and
must stay `0`.

---

## Regenerating: two INDEPENDENT steps

The split is the point. One costs a live opencode, network and money; the other
is offline, free and deterministic. **A version bump is: re-record → re-derive →
review the golden diff as the substantive change.**

### Step 1 — re-record the wire (LIVE server, network, money)

```bash
SHED_OPENCODE_LIVE=1 SHED_OPENCODE_RECORD=1 \
  cargo test -p shed-opencode --test live -- --nocapture
```

Spawns a real `opencode serve` in a scratch project, drives a turn through a
tool call, a permission, a question and a cancel, and writes the raw `/event`
frames to `fixtures/1.18.29/event-frames.jsonl`. It is never run in CI and skips
with a message when `SHED_OPENCODE_LIVE` is unset. **On an opencode upgrade,
record into a directory named for the NEW version — do not overwrite an existing
one**, and point the replay test at it.

**Two files per recording directory, and only one of them is written by a run:**

| file | who writes it | what it holds |
|---|---|---|
| `<version>/PROVENANCE.md` | the recording run, every time | the generated card: the opencode version, the date, the command, the scratch config and the model |
| `<version>/README.md` | a HUMAN | everything a run cannot know — the pointer back to this page, the frame count and the event-type inventory the replay test depends on, and what `fold.golden.json` is |

The run used to write its card as `README.md`, which meant a re-record deleted
the page describing the regeneration path it is part of. It writes
`PROVENANCE.md` instead and touches nothing else; `README.md` is edited by hand
(`live.rs::a_re_record_writes_a_provenance_card_and_leaves_the_readme_alone`
pins that, offline).

### Step 2 — re-derive the golden from the committed recording (OFFLINE, free)

```bash
SHED_OPENCODE_REGOLD=1 cargo test -p shed-opencode --test fold_fixtures
```

No credentials, no network, no opencode: it replays the **committed** jsonl
through the fold and rewrites `1.18.29/fold.golden.json`, then asserts against
what it wrote, so a regen that does not round-trip still fails. Regenerating an
unchanged fold reproduces the committed bytes exactly (`serde_json` orders
object keys; the file ends with one newline).

Then `git diff` the golden and **read it as the change**: a new row, a moved
verdict, a shed payload that stopped shedding. If the diff is not what the code
change intended, the code change is wrong.

### Why `opencode_turn.golden.json` is NOT in step 2

That file is a recording of a **different program's** output — the rc hub's
fold. Rewriting it from this crate would swap a claim about the hub for a claim
about ourselves, and the test would then be asserting the port against itself:
any drift would be laundered into the golden instead of failing. So
`SHED_OPENCODE_REGOLD` deliberately leaves it alone, and
`tests/fold_fixtures.rs::c2_golden_is_not_regenerable` asserts that it does.

To re-record it after a *deliberate* behavior change, do what C2 did: add a
temporary `shed-broker` dev-dependency, write a throwaway harness that runs both
folds line by line, assert their projections agree, write the hub's out, then
delete the harness and the dev-dep in the same commit. Or hand-edit the file and
say in the commit message which rule changed and why.

---

## `opencode_turn.golden.json` — the hub-vs-port PIN

**This file is a pin, not an expectation someone wrote down.** It is a recording
of what the **rc hub's** `OpencodeFold`
(`crates/shed-broker/src/rc_hub/watch_opencode.rs`) produces when it folds
`crates/fixtures/jsonl/opencode_turn.jsonl` — the mechanism that proves plan
015's port of that fold into `shed-opencode` changed no behavior.

| | |
|---|---|
| input fixture | `crates/fixtures/jsonl/opencode_turn.jsonl` (32 lines, one turn, recorded from opencode **1.17.15**) |
| recorded from | `shed-broker` at shed `2265a3b`; the hub fold last changed in `dd347a6` ("rc hub: the axum HTTP shell", plan 010 H10) |
| recorded by | a throwaway `tests/record_golden.rs` harness, deleted in the same commit (plan 015 C2) |
| asserted by | `tests/fold_fixtures.rs`, which replays the fixture through **this crate's** fold only |

### How it was recorded

A throwaway integration test took a temporary `shed-broker` dev-dependency, ran
**both** folds over the fixture line by line, asserted their projections were
byte-identical, and wrote the hub's to this file. They agreed on the first run.
The harness and the dev-dependency were then deleted: the golden file IS the
pin.

### The canonical projection

The two folds emit different row types — the hub its producer-side
`rc_hub::messages::FeedMessage`, the port `shed_core::rc::RcFeedMessage` — so
the golden records the fields they SHARE, with each side's optional spelling
reduced to the other's (`None` ⇒ `""`, an absent tool/approval block ⇒ empty
strings):

- per fixture line: `applied` (what `apply_line` returned), `activity` after it,
  and the rows drained after it;
- per row: `seq`, `ts`, `role`, `type`, `text`, `tool_name`, `tool_detail`,
  `approval_id`, `approval_status`, `approval_decision`, `approval_decisions`;
- at the end: `activity`, `last_message`, `open_approvals`, and the
  `pending_approvals` snapshot as `{id, status}`.

`seq` is 0 on every row: the fold does not assign it (the bounded ring does, in
C3). `ts` is included even though the task's field list did not name it — it is
shared, it is where `opencode_ts`/`first_non_zero` regressions would show, and
`""` on both sides means "no usable time, stamp it on append".

### What this fixture does NOT cover

It was recorded from a plain tool-using turn and contains **no**
`permission.asked`, **no** `question.asked` and no `session.error`. That seam is
what the 1.18.29 recording below closes.

---

## `1.18.29/fold.golden.json` — the port's own replay pin

| | |
|---|---|
| input | `1.18.29/event-frames.jsonl` — 155 `/event` frames from a real opencode **1.18.29** session (`1.18.29/README.md` has the provenance) |
| minted by | **this crate's** fold, offline, by `tests/fold_fixtures.rs` under `SHED_OPENCODE_REGOLD=1` |
| asserted by | `tests/fold_fixtures.rs::replays_the_1_18_29_recording` |

It is the first pin that reaches `permission.asked`/`permission.replied`,
`question.asked`/`question.replied`, `session.error` and `message.part.delta`
with **real wire** rather than hand-authored unit inputs. The projection:

- per recorded line: the input's `event` type (a label, so a diff reads as prose),
  `applied`, the `activity` verdict after it, `open_approvals`, the rows drained
  after it as the **full** `RcFeedMessage` wire shape (nothing is reduced to a
  shared subset here — this golden pins the port, not an agreement), and the
  still-open approvals as `{kind, id, status}`;
- `approvals`: for every addressable ask, the FULL `LaneApproval` DTO as it stood
  **when asked** and again **at the end** — which is what pins the payload, the
  structured questions, and `shed_payload`'s tombstone-on-resolution (`status`
  flips to `resolved`, `request_json` collapses to `{}`, `questions` empties);
- `final`: the terminal verdict, `last_message`, `open_approvals` and the
  pending snapshot.

A companion test, `the_recording_covers_the_approval_and_question_paths`, asserts
the recording still carries each of those event types and that the replay
actually reaches `needs_approval` — so a truncated re-record cannot silently
delete the coverage this fixture exists for.

### The transcript subset is ALSO fidelity — and how that was proven

Plan 015 C3b repeated C2's discipline on this recording: a throwaway harness
(`tests/record_1_18_29.rs`, deleted with its temporary `shed-broker` dev-dep in
the same commit) ran **both** folds over these 155 frames and asserted the
transcript projections byte-identical. **They agreed on the first run**, over 154
compared steps carrying 15 feed rows (8 `text`, 2 `tool_use`, 2 `tool_result`, 2
`approval_request`, 1 `status`) and all four verdicts the fold can reach
(`unknown`, `working`, `needs_approval`, `needs_input`).

The three deliberate divergences (plan §3.2) were excluded **by construction**,
keyed on the input, never on an observed disagreement:

1. **`seed_approvals` vs the hub's `shed.approval.seed` marker** — not exercised.
   The marker is a hub synthesis and never appears on opencode's wire; the
   harness asserted the recording contains none rather than assuming it.
2. **Questions as first-class `LaneApproval`s** — the hub keeps open questions
   out of `pending_approvals` entirely, so the terminal snapshot comparison
   filtered the port's list to `kind == permission`. Nothing else was excluded
   for questions: both folds emit the same display-only `awaiting answer:` status
   row, and both count an open question toward `open_approvals`, so the verdict
   was compared with no exclusion at all.
3. **`session.error` as a display-only status row** — the hub ignores the type,
   so the single step whose input envelope is `session.error` was skipped. That
   row survives only in this crate's golden.

That is the whole of the fidelity claim on this file. Everything else in it —
every `LaneApproval` DTO, the `session.error` row — is the port's own behavior,
pinned so a regression shows up as a diff.
