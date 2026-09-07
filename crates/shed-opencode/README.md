# shed-opencode

The **opencode adapter** for `shed_core::lane` — the agent-lane contract. One
adapter per agent; this is opencode's, speaking its server's local HTTP API over
loopback (the server the TUI already runs, whose URL the roost plugin reports).

Plan 015 lands it in two commits:

| commit | what |
|---|---|
| **C2** | `src/fold.rs` — the pure fold (no I/O), plus `src/helpers.rs`, the decode + text-hygiene helpers it needs |
| **C3** | `client.rs` (two reqwest clients, directory routing), `ring.rs` (owns `seq`), `watcher.rs` (generations, subscribe-before-seed, `Reset` … `Ready`), the `AgentLane` impl, `testing::FakeOpencode` |

## The fold is a port, and it is pinned as one

`OpencodeFold` is a port of the rc hub's fold
(`crates/shed-broker/src/rc_hub/watch_opencode.rs`), which is itself a port of
the Go guest's `internal/ext/rc/watch_opencode.go`. Two things keep the port
honest:

- **`fixtures/opencode_turn.golden.json`** — a recording of what the HUB's fold
  produces on `crates/fixtures/jsonl/opencode_turn.jsonl`, reduced to the
  projection the two folds share. `tests/fold_fixtures.rs` replays the fixture
  through THIS fold and asserts it still equals that. See the fixture README for
  how it was recorded and why the test does not depend on `shed-broker`.
- **Hand-authored unit inputs** (`src/fold.rs`'s test module) for everything the
  fixture cannot reach — it was recorded from opencode 1.17.15 and contains no
  `permission.asked` and no `question.asked`.

The helpers in `src/helpers.rs` are **copied** from `shed-broker::rc_hub`, not
linked: `rc_hub::watch` imports `shed_rc_engine::tmux::Tmux`, so linking would
drag the RC engine and the hub into a crate that only speaks HTTP. The
duplication is deliberate and temporary — the hub's copy goes away with the rest
of its opencode watcher.

Three differences from the hub are deliberate (plan 015 §3.2), and documented at
each site in `src/fold.rs`:

1. the hub's synthesized `shed.approval.seed` envelope is the method
   `seed_approvals(permissions, questions)`;
2. questions materialize as `LaneApproval { kind: Question }` carrying
   opencode's `QuestionInfo`, where the hub could only count them;
3. `session.error` — ignored by the hub — emits a display-only `status` row.

The hub's **correlation** machinery (`not_before`, `created_late_enough`,
`dir_match_canon`, `ClaimHolder`, `rest_find_candidate`, `root_pin_from_created`)
is deliberately absent. It existed because the hub had to FIND its session in a
shared store; in the roost model the session id arrives by construction, so the
lane never searches — it scopes.

## Build / test

```bash
cd crates
cargo test -p shed-opencode
cargo clippy -p shed-opencode --all-targets -- -D warnings
```
