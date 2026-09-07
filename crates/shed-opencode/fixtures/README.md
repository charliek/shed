# `shed-opencode/fixtures`

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
pin, and `tests/fold_fixtures.rs` must never depend on `shed-broker` — the whole
point of the port is that this crate does not link the hub.

To re-record after a *deliberate* behavior change, re-create that harness (see
the git history of this commit), or hand-edit the file and say in the commit
message which rule changed and why.

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
`permission.asked`, **no** `question.asked` and no `session.error`. Those paths —
plus `note_gap` and `reset` — are covered by hand-authored **unit inputs** in
`src/fold.rs`'s test module, which assert rows AND verdicts. They are inputs, not
recordings; nothing here claims a live opencode ever emitted them verbatim.
