# `shed-gx/fixtures`

One recording of real gx wire, and one golden derived from it. **They carry
different claims, and this page exists so the two are never blurred.**

| artifact | what it is | the claim it carries | regenerable offline? |
|---|---|---|---|
| `1.0.16+gx.12/{history.json, event-frames.jsonl, approvals.jsonl}` | raw bytes from a live gx **1.0.16+gx.12** leader | **EVIDENCE** — this is what gx actually serves | **No.** A live leader and real model quota |
| `1.0.16+gx.12/fold.golden.json` | what **this crate's** fold makes of that recording | **REGRESSION DETECTION** — "this is what the fold does today" | **Yes.** `SHED_GX_REGOLD=1`, free |
| `1.0.16+gx.12/PROVENANCE.md` | a card the recording run writes | when, with what, and what the run saw | written by every re-record |

## The golden is NOT a fidelity proof, and there is nothing here that is

`shed-opencode` has two goldens because its fold is a **port**: one of them
records what the *other* implementation (the rc hub) produced, so matching it is
evidence the port changed no behaviour. **shed-gx has no counterpart.** Nothing
else folds gx's wire, so nobody but this crate could have minted
`fold.golden.json`, and a diff in it is a behaviour change for a human to read —
never evidence of agreement with anything.

Read a diff in it as the change. A new row, a moved verdict, a cursor that stopped
advancing: if the diff is not what the code change intended, the code change is wrong.

## What the recording is FOR — the things a hand-written fixture would not think of

`tests/fold_fixtures.rs::the_recording_still_carries_what_the_golden_is_for`
asserts each of these, so a truncated or tidied re-record cannot silently delete
the coverage:

- **Counters that are not monotonic in arrival order.** gx's event ids come off
  an atomic several sessions bump, so the stream really does deliver
  `…-43, …-42, …-46, …-44`. This is why the resume cursor is the fold's
  **maximum** counter and not its last-applied one, and why history is cut
  **positionally** rather than by comparing counters.
- **Id-less envelopes** (5 of them). Never deduplicated, never a cursor — and
  never replayable, which `id_less_envelopes_fold_to_nothing` turns from an
  assumption into an assertion.
- **`hook_execution` interleaved through chunk streaks** (25 of 70 frames). The
  ignored kinds have to be *transparent* to a streak, not merely skipped.
- **A real permission with five OPAQUE option ids**, two of which declare
  `allow_once` (`enable-always-approve` and `allow-once` — the first turns
  prompting off for the whole session). That is what makes
  `LaneAnswer::Permission{AllowOnce}` genuinely ambiguous on real gx, which the
  adapter answers with `BadRequest` rather than a guess. No tidy fixture would
  have invented it.
- **gx's option-less placeholder.** When an agent blocks, gx files a
  `pending_interaction` with `method` and `request` both `null` **first**, and
  sends the real request in a second `approval` frame on the same id. Both are
  in `approvals.jsonl`. This is why `LaneEvent::Approval` is id-keyed and
  last-write-wins: a client that rendered the first frame's empty option list
  and stopped would show a question with no buttons.
- **Both timestamp units and both `status` spellings**, which the fold's unit
  tests cover against synthetic input and this pins against real wire.

## What is NOT here, and is covered elsewhere

Stated so nobody reads absence as coverage:

- **`create`.** `POST /v1/sessions` mints a session, and the scratch leader this
  was recorded against is on a quota-exhausted account where a new session is
  not worth spending. It is exercised against `FakeGx` only
  (`client::tests::create_posts_cwd_and_text_then_reads_the_created_row`).
- **Question and plan approvals.** gx raises these from interactions this
  recording's turns never triggered. They are covered against synthetic
  resources in `fold::tests` (`a_question_is_keyed_by_its_text_…`,
  `a_plan_approval_gets_two_synthesized_options_…`) and, for the answer bodies,
  in `client::tests`.
- **The reconnect ladder**, the reconcile and the tombstones. Timing behaviour
  needs a server you can break on purpose; that is `tests/watcher.rs` against
  `FakeGx`, with the windows scaled through `GxTimings`.

## The recording is scrubbed BY REFUSAL, not by rewriting

`tests/live.rs::Guard` fails the run — writing nothing at all — if the captured
bytes carry either:

- a run of **64 or more lowercase hex digits**, which is a gx token's exact
  shape, or
- an **absolute path outside the scratch directories** (`$HOME/.cache/shed-plan017`
  by default; override with `SHED_GX_SCRATCH`).

Refusing rather than scrubbing is the design. A scrubber that quietly rewrote
its input would turn "the fixture is clean" into a claim about the scrubber, and
the first secret shape it failed to recognise would be committed looking fine.
`the_recorder_refuses_a_token_or_a_path_outside_the_scratch_directories` runs
offline on every `cargo test`, so the gate is never exercised for the first time
on the day someone re-records.

**What the recording legitimately does contain**, because it is a real
transcript from a real scratch project: the scratch project path
(`~/.cache/shed-plan017/project`), the scratch session's UUID, tool-call ids, and
the output of the commands the recorded turns ran — one of which is `id -un`, so
the developer's local username appears as tool output. None of that is a
credential; all of it is inside the scratch tree the guard allows.

---

## Regenerating: two INDEPENDENT steps

The split is the point. One costs a live leader and real model quota; the other
is offline, free and deterministic. **A gx upgrade is: re-record → re-derive →
review the golden diff as the substantive change.**

### Step 1 — re-record the wire (LIVE leader, model quota)

```bash
SHED_GX_LIVE=1 SHED_GX_RECORD=1 \
  GROK_HOME=~/.cache/shed-plan017/grok-home \
  SHED_GX_SESSION=<a session with a transcript> \
  cargo test -p shed-gx --features test-support --test live -- --nocapture
```

It finds the leader through `$GROK_HOME`'s own discovery record — a home, never
a port — so a developer's personal leader elsewhere is untouched. Without
`SHED_GX_LIVE` it prints why it is skipping and passes, so CI and an ordinary
`cargo test` never reach it.

The run drives **two cheap turns**: one trivial prompt, and one that asks
permission so there is an approval to record. The second one is overridable,
and it has to be:

```bash
SHED_GX_ASK="run the shell command: <something the classifier will stop>" …
```

gx's classifier decides what needs asking, and what it waves through moves with
the model **and with the session's own history** — a command it has already
approved once is not asked about again. If no approval is raised, the run says
so, leaves `approvals.jsonl` alone and records the fact in `PROVENANCE.md`
rather than committing a recording that quietly lost its approval coverage.

**On a gx upgrade, record into a directory named for the NEW version — do not
overwrite an existing one** — and point `tests/fold_fixtures.rs`'s `RECORDING`
at it.

Two files per recording directory, and only one of them is written by a run:

| file | who writes it | what it holds |
|---|---|---|
| `PROVENANCE.md` | the recording run, every time | the generated card: the gx version, the date, the command, what the run saw |
| this `README.md` | a **human** | everything a run cannot know — which claim each artifact carries, why the recording is worth having, what is deliberately absent |

`a_re_record_writes_a_provenance_card_and_leaves_the_readme_alone` pins that
split, offline. (It is not hypothetical: `shed-opencode` hit exactly this bug —
a run wrote its template as `README.md` and deleted the page describing the
regeneration path it was part of.)

### Step 2 — re-derive the golden from the committed recording (OFFLINE, free)

```bash
SHED_GX_REGOLD=1 cargo test -p shed-gx --test fold_fixtures
```

No credentials, no network, no gx: it replays the **committed** files through
the fold and rewrites `1.0.16+gx.12/fold.golden.json`, then asserts against what
it wrote — so a regeneration that does not round-trip still fails instead of
laundering itself into the file. Regenerating an unchanged fold reproduces the
committed bytes exactly (`serde_json` orders object keys; the file ends with one
newline).

## Reading `fold.golden.json`

| key | what it pins |
|---|---|
| `session` | the recorded session id, taken FROM the recording — the fold refuses an event id whose prefix is another session's, so reading it from the file is what keeps a re-record honest |
| `history` | the persisted `GET …/history` page, folded step by step: each envelope's id and kind, whether it applied, the verdict and cursor after it, and the rows it produced |
| `stream` | the same projection over the replayed SSE frames |
| `approvals` | every recorded approval resource as its full `LaneApproval` DTO — the option ids verbatim, the placeholder included |
| `resume` | `lossy_splits`: the break points gx's own by-counter replay cannot heal |

That last one deserves a sentence, because it is a **finding, not a setting**.
gx replays by counter, and its counters are not monotonic in arrival order, so a
frame minted just after a break can carry a counter *below* the cursor and is
then never replayed. `a_silent_resume_loses_and_duplicates_nothing_that_gx_can_replay`
simulates a break at **every** frame: at the ~53 splits inside gx's replay window
the transcript must come out identical row for row (the fold's `seen` set absorbs
the overlap and nothing folds twice), and at the 18 listed here it may be short —
never longer, which would mean something was folded twice. That residual is filed
against gx (plan 017 §9); shed's bounded silent resume and the reseed that follows
it are what heal it.
