# roost IPC vectors (vendored)

Byte-for-byte copies of roost's own golden wire vectors, taken from

    github.com/charliek/roost @ ee71e44a1de3c0de4c59ac0267c0a5e0c993d88a
    tests/ipc-vectors/<same filename>

which is the **same rev** `crates/Cargo.toml` pins `roost-ipc` to. They travel with
this repo so shed's roost client and its in-process fake (`shed_core::roost::testing`)
are built against the shapes roost itself publishes, not against shapes remembered
from a doc.

## The rule (roost's, inherited verbatim)

**A vector here is never semantically edited.** It is the compatibility contract:
an existing file is not rewritten to bless a wire change, and an additive change
adds a new file. Whitespace-only reformatting is not a semantic edit but is
pointless — these are copies, so keep them diffable against the source.

The full policy — what "additive" means and the four-direction compatibility
matrix — is roost's `docs/reference/ipc-compatibility.md`.

## Refreshing on a `roost-ipc` bump

When the pinned `rev` in `crates/Cargo.toml` moves:

1. re-copy these files from the new rev's `tests/ipc-vectors/`,
2. update the sha in this README,
3. run `cargo test -p shed-core` **and** `go test ./internal/roostprovider/...`
   **and** the shedtest suite — the two fakes and the decoders read these in
   three languages, so a shape change surfaces as a test failure rather than as
   silence, but only in the language that reads the file that moved.

**Generation-suffixed names.** roost keeps one `session.identify` reply per
`SESSION_PROTOCOL_VERSION` generation — `session.identify.response.v2.json`,
`.v3.json`, `.v4.json`, `.v5.json`, `.v6.json`. shed vendors **only the current
generation** (`.v6.json` today, plan 021): a client that gates on the number has
nothing to do with an older shape, and a protocol-2 daemon is exercised through
the fakes' `set_session_protocol` control rather than through a vector they would
then have to keep in step.

So a generation bump **renames the vendored file, and every reader moves with
it**. The table below is the whole list — three languages, more than "both
fakes", and the trap this paragraph exists for:

| reader | how it names the file |
|---|---|
| `crates/shed-core/src/roost/testing.rs` | `include_str!` (the Rust fake's template) |
| `crates/shed-core/src/roost/fence.rs` | its own separate `include_str!` — not a fake, easy to miss |
| `crates/shed-core/tests/roost_provider_vectors.rs` | reads it by name at runtime |
| `internal/roostprovider/goldens_test.go` | by name (`SpokenProtocol`'s pin) |
| `internal/roostprovider/wire_test.go` | by name, twice (decode + string-id) |
| `internal/roostprovider/fakessh_test.go` | by name (the fake ssh session's reply) |
| `desktop/tools/shedtest/fake_roost.py` | by name (the Python fake's template) |

Go fails loudly on a missing path, so a rename that misses one of its three is a
red `go test ./...` rather than a stale read.

## What is here, and who reads it

| file | read by |
|---|---|
| `session.identify.response.v6.json` | both fakes' `session.identify` template (the protocol gate's input); the fence tests' `daemon_session_id` / `started_at`; Go's `SpokenProtocol` pin |
| `identify.response.json` | the fake's `identify` template |
| `tab.list.session.response.json` | the fake's initial project/tab set — the **session** variant, i.e. the one that carries `revision` (42); the fence replay's snapshot |
| `tab.open.response.json` | the fake's template for a tab it opens |
| `response.error.json` | the fake's error-envelope shape (`unknown-op`) |
| `events.batch.json` | the fence replay's batch shape (`{revision, events: […]}`), its revision-42 replay, and the envelope the fake commits every mutation as |
| `tab.opened.event.json` | the fence fold's unowned-tab-opens case; the batch the fake pushes for a `tab.open` |
| `tab.state_changed.event.json` | the fence fold's ignored-derived-projection case |
| `agent_report.changed.event.json` | the fence fold's claim/release case (a `claude` adapter taking tab 5); the batch the fake pushes for `set_tab_axes` |
| `session.stopping.event.json` | the fence fold's "an envelope that is not a workspace fact" case; the terminal envelope the fake pushes from `stop()` |
| `tabs.reordered.event.json` | the batch the fake pushes for `reorder_tabs` — the event that costs the watcher a re-list, since shed models no ordering |
| `projects.reordered.event.json` | the same for `reorder_projects` |
| `events.subscribe.request.json` | the fresh-subscribe shape `Conn::subscribe` sends — the op shed calls on every watcher cycle, and the only thing that proves the lease key is gone from it |
| `events.subscribe.response.json` | the fake's `events.subscribe` ack (the fence `{revision}` plus the required `session_id`) |
| `tab.dump.request.json` | the `tab.dump` shape `Conn::tab_dump` sends — `scrollback` is omit-when-zero, so this is what proves shed's viewport read emits no `scrollback` key at all |
| `tab.write.request.json` | the `tab.write` shape — lease-free since generation 5 |
| `session.set_agent_hooks.request.json` | the shape `Conn::session_set_agent_hooks` sends — the generation-6 raise `{agents, client}`; `deny_unknown_fields` on roost's side, so this is the contract. **Its `agents` array is roost's own two-name example**, not shed's five: the request test compares every key except `agents` against this file and `agents` against `ROOST_WIRED_AGENTS`, so the copy stays byte-for-byte roost's |
| `session.set_agent_hooks.response.json` | both fakes' `session.set_agent_hooks` reply template — an `AgentHooksOutcome` (`wired`/`refreshed`/`removed`/`skipped`/`errors`), with the two skip reasons a client can see (`"not allowed"`, `"not installed"`) |
| `stream.ended.event.json` | the terminal envelope `FakeRoost::end_stream` pushes — a UI-socket frame (`reason: "backend-switch"`) a session socket never writes, which is why a test is its only coverage |

## Shed-recorded vectors

`shed.tab.list.opencode.finished.json`, `shed.tab.list.opencode.over-ssh.json`
and `shed.tab.dump.opencode.json` were **recorded by shed** from a real
`roost-session` (release build of rev `61d8713…`, protocol 2) on 2026-09-07;
never edited; they pin the row mapping against real adapter output. They keep
their protocol-2 provenance because the `Tab` / `Project` / `tab.dump` shapes are
**byte-identical** between `61d8713` and `c1bfe88` — the R1 re-cut moved the lease
semantics and the protocol-5 bump retired it, and neither touched the workspace
shapes.

`shed.session.identify.json` is **re-recorded on every generation bump**, because
that reply embeds the generation integer and a recording from an older generation
is no longer what shed's gate sees. The current one was taken on 2026-09-19 from a
live **protocol-6** daemon (release build of rev `ee71e44…`, `roost-session`
0.0.19) that shed's own desktop bootstrap installed and started on a scratch shed —
so it is a recording of the path a user actually walks, not of a daemon started by
hand for the capture.

**Re-recorded, never hand-edited**, and each bump has justified that: the 4 → 5
re-record revealed that a protocol-5 daemon answers `payload_kinds` as
`["ghostty-snapshot", "vt"]` where the protocol-4 recording carried only
`["ghostty-snapshot"]`; the 5 → 6 one added `ops` (27 entries — the op list that
daemon would actually dispatch), which no integer edit would have produced. A
hand-edited generation number would have left both quietly wrong.

**It lags the pin by one commit, on purpose, and a tripwire bounds that.** A re-pin
is a source edit, but re-recording needs a live daemon of the new generation — a
live leg, which lands later. `crates/shed-core/src/roost/model.rs`'s test module
carries `RECORDED_GENERATION` for what this file holds, asserted against it at
runtime, plus a compile-time assertion that the recording may trail
`SESSION_PROTOCOL_VERSION` by at most one generation. Refresh the recording and the
runtime assertion goes red until the constant moves; skip a second bump and the
build fails. Neither direction can pass by drifting.

Earlier recordings, for the record: 2026-09-12 from the protocol-5 daemon at
`c1bfe88…`, and 2026-09-07 from the protocol-4 daemon at the rev pinned then.

`shed.tab.closed.event.json` and `shed.tab.notification.event.json` are recorded
too, and they exist because roost publishes **no** vector for either envelope
(its event vectors are `agent_report.changed`, `events.batch`,
`projects.reordered`, `session.stopping`, `tab.effect`, `tab.opened`,
`tab.state_changed`, `tabs.reordered`). Both were captured from the
protocol-4 daemon of the day with an observer stream open — the close by
`roostctl tab open` followed by `roostctl tab close`, the notification envelope by
the same stream's `tab.notification` frame — so the fakes build a close and a
notification flip from roost's own bytes rather than from a shape remembered off
a doc page. The recorded `tab_id` / `has_pending` are overwritten by the fakes
per push, exactly as they overwrite fields in the other envelope templates.

`shed.tab.list.opencode.finished.json` is the local spike and is the shape the
row mapping is really about: a **plain shell tab** (id 3, unowned) sits beside
the opencode tab (id 4, owned, `finished`, `detail: "session_idle"`, with the
adapter's `agent`/`model`/`version` metadata). One of those two is a session row;
the other is somebody's terminal. `shed.tab.list.opencode.over-ssh.json` is the
same pair read from inside a shed VM over roost's SSH client-bridge.

## shed's own multi-language goldens

Three files here are **not** roost vectors and are **not** copies of anything:
they are shed's own, written by shed, and they may be edited. The
no-semantic-edits rule above governs the vendored vectors, not these.

| file | what it pins | asserted by |
|---|---|---|
| `bootstrap/exec-chain-command.txt` | roost's candidate-ladder remote command, `roost_ipc::bootstrap::exec_chain_command(false)` | Rust (`shed-core/tests/roost_provider_vectors.rs`, against the LIVE function), Go (`internal/roostprovider`'s `ExecChainCommand` constant) and Dart (shed-mobile's `integration_test/roost_goldens_test.dart`, against `roostRemoteCommand()` — the string its roost tunnel hands `execute` verbatim) |
| `agent-table.json` | kind → binary → title for the six agents the roost provider can start | Rust (`launch_argv` + `roost_capabilities().kinds`), Go (`internal/roostprovider`'s `agentTable`) and Dart (the kind SET a machine's create form offers) |
| `stderr-classes.json` | how a failed `ssh` exec classifies (`roost_ipc::ssh::classify_ssh_failure`), plus shed's own class → provider-row and class → `ReachKind` mappings | Rust (the live classifier, `classes`; and `shed_app::roost::ReachError`, `reach_kinds`), Go (`ClassifySSHFailure` + `ProviderRow`) and Dart (`classes` **and** `reach_kinds`, against shed-mobile's own port in `lib/ssh/roost_reach.dart`) |

They exist because one behaviour is implemented on both sides of a language
boundary — the Go `shed roost-provider` subcommand and the Rust client core, and
since plan 020 the Dart transport in shed-mobile as well — and a golden asserted
from every side is the only thing that makes the implementations provably the
same rather than the same today.

**The Dart leg reads these files out of a shed CHECKOUT**, not a vendored copy:
shed-mobile is a separate repository, and its
`make test-integration-linux` refuses to run without one (`SHED_CHECKOUT`,
defaulting to a sibling `../shed`; CI checks out the pinned rev). It owns a port
of `classify_ssh_failure` because it owns its own transport — the phone execs
over dartssh2, so nothing in Rust can see the far end's `exit 127` — and this
golden is what keeps that third port honest.

### The bootstrap script goldens (`bootstrap/*`, Rust-only)

Everything else under `bootstrap/` is a **tripwire on roost's own script
builders**, written by plan 019 C4 and read only by Rust
(`shed-core/tests/roost_bootstrap_goldens.rs`):

| file | the builder it pins |
|---|---|
| `discovery-script.sh` | `discovery_script(false)` |
| `path-check-command.txt` | `path_check_command()` |
| `identity-script.sh` | `identity_script([dest, /usr/bin/roost-session])` |
| `prepare-script.sh` | `prepare_script()` |
| `stream-command.txt` | `stream_command(tmp)` |
| `verify-staged-script.sh` | `verify_staged_script(tmp)` |
| `commit-script.sh` | `commit_script(tmp, dest, backup)` |
| `post-commit-identity-script.sh` | `identity_script([dest])` |
| `discard-backup-script.sh` | `discard_backup_script(backup)` |
| `rollback-script.sh` | `rollback_script(dest, backup)` |
| `cleanup-script.sh` | `cleanup_script(tmp)` |
| `start-script.txt` | `start_script(path)` |

All in **shipped** form (`jail_fs_root: false`) with one fixed path triple, and
all **byte-exact** — no trailing newline is added or trimmed, unlike
`exec-chain-command.txt`, whose +1 convention exists because Go reads that one
too. `shed_core::roost::bootstrap` composes these builders rather than writing
its own, which means a `roost-ipc` rev bump can change what a bootstrap sends
without a line of shed changing; these files are what makes that a failing test
instead of a surprise on somebody's host. They are **not** a specification of
what the scripts should say — read the diff, decide whether the choreography in
`bootstrap/machines.rs` still matches, then regenerate:

```
SHED_UPDATE_ROOST_GOLDEN=1 cargo test -p shed-core --test roost_bootstrap_goldens
```

**`bootstrap/exec-chain-command.txt` is GENERATED, never hand-written.** It was
produced by calling `exec_chain_command(false)` and writing the result; the Rust
test then asserts the file still equals that call, so a `roost-ipc` bump that
changes the ladder fails loudly instead of leaving the hand-copied Go constant
quietly wrong. To refresh it after a bump, re-run the generator described in that
test's comment and re-copy the string into Go.

The file carries a trailing newline that the command itself does not; both tests
trim exactly one.
