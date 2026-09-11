# roost IPC vectors (vendored)

Byte-for-byte copies of roost's own golden wire vectors, taken from

    github.com/charliek/roost @ c67ac27b6a85dbee0871f32d49c1566cc068d1c8
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
3. run `cargo test -p shed-core` — the fake and the decoders read these, so a
   shape change surfaces as a test failure rather than as silence.

**Generation-suffixed names.** roost keeps one `session.identify` reply per
`SESSION_PROTOCOL_VERSION` generation — `session.identify.response.v2.json`,
`.v3.json`, `.v4.json`. shed vendors **only the current generation**: a client
that gates on the number has nothing to do with an older shape, and a protocol-2
daemon is exercised through the fake's `set_session_protocol` control rather than
through a vector it would then have to keep in step. So a generation bump renames
the vendored file, and the `include_str!` paths move with it.

## What is here, and who reads it

| file | read by |
|---|---|
| `session.identify.response.v4.json` | the fake's `session.identify` template (the protocol gate's input, and the `features` list it preserves); the fence tests' `daemon_session_id` / `started_at` |
| `identify.response.json` | the fake's `identify` template |
| `tab.list.session.response.json` | the fake's initial project/tab set — the **session** variant, i.e. the one that carries `revision` (42); the fence replay's snapshot |
| `tab.open.response.json` | the fake's template for a tab it opens |
| `response.error.json` | the fake's error-envelope shape (`unknown-op`) |
| `events.batch.json` | the fence replay's batch shape (`{revision, events: […]}`), its revision-42 replay, and the envelope the fake commits every mutation as |
| `tab.opened.event.json` | the fence fold's unowned-tab-opens case; the batch the fake pushes for a `tab.open` |
| `tab.state_changed.event.json` | the fence fold's ignored-derived-projection case |
| `agent_report.changed.event.json` | the fence fold's claim/release case (a `claude` adapter taking tab 5); the batch the fake pushes for `set_tab_axes` |
| `session.stopping.event.json` | the fence fold's "an envelope that is not a workspace fact" case; the terminal envelope the fake pushes from `stop()` |
| `session.driver_changed.event.json` | the non-terminal envelope the fake pushes to every stream on a takeover |
| `tabs.reordered.event.json` | the batch the fake pushes for `reorder_tabs` — the event that costs the watcher a re-list, since shed models no ordering |
| `projects.reordered.event.json` | the same for `reorder_projects` |
| `events.subscribe.response.json` | the fake's `events.subscribe` ack (the fence `{revision}`) |
| `session.connect.request.json` | the unlabeled `session.connect` shape `Conn::session_connect(_, None)` sends |
| `session.connect.labeled.request.json` | the same request with `client_label` — the omit-when-unset key |
| `session.connect.response.json` | the fake's `session.connect` reply (`{lease, revision}`) |
| `tab.write.request.json` | the `tab.write` shape, whose `lease` is omit-when-unset |
| `session.set_agent_hooks.request.json` | the shape `Conn::session_set_agent_hooks` sends (plan 019 S5) — `deny_unknown_fields` on roost's side, so this is the contract |
| `session.set_agent_hooks.response.json` | the fake's `session.set_agent_hooks` reply template (`wired`/`refreshed`/`removed`/`skipped`/`errors`) |

## Shed-recorded vectors

`shed.tab.list.opencode.finished.json`, `shed.tab.list.opencode.over-ssh.json`
and `shed.tab.dump.opencode.json` were **recorded by shed** from a real
`roost-session` (release build of rev `61d8713…`, protocol 2) on 2026-09-07;
never edited; they pin the row mapping against real adapter output. They keep
their protocol-2 provenance because the `Tab` / `Project` / `tab.dump` shapes are
**byte-identical** between `61d8713` and `c67ac27` — the R1 re-cut moved the lease
semantics, not the workspace shapes.

`shed.session.identify.json` was **re-recorded** on 2026-09-07 from the
protocol-4 daemon (release build of rev `c67ac27…`, `roost-session` 0.0.19),
because that reply embeds the generation integer and a protocol-2 recording would
no longer be what shed's gate sees.

`shed.tab.closed.event.json` and `shed.tab.notification.event.json` are recorded
too, and they exist because roost publishes **no** vector for either envelope
(its event vectors are `agent_report.changed`, `events.batch`,
`projects.reordered`, `session.driver_changed`, `session.stopping`, `tab.effect`,
`tab.opened`, `tab.state_changed`, `tabs.reordered`). Both were captured from the
same protocol-4 daemon with a **leaseless observer stream** open — the close by
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

## shed's own two-language goldens

Three files here are **not** roost vectors and are **not** copies of anything:
they are shed's own, written by shed, and they may be edited. The
no-semantic-edits rule above governs the vendored vectors, not these.

| file | what it pins | asserted by |
|---|---|---|
| `bootstrap/exec-chain-command.txt` | roost's candidate-ladder remote command, `roost_ipc::bootstrap::exec_chain_command(false)` | Rust (`shed-core/tests/roost_provider_vectors.rs`, against the LIVE function) and Go (`internal/roostprovider`'s `ExecChainCommand` constant) |
| `agent-table.json` | kind → binary → title for the six agents the roost provider can start | Rust (`launch_argv` + `roost_capabilities().kinds`) and Go (`internal/roostprovider`'s `agentTable`) |
| `stderr-classes.json` | how a failed `ssh` exec classifies (`roost_ipc::ssh::classify_ssh_failure`), plus shed's own class → provider-row mapping | Rust (the live classifier, `classes` only) and Go (`ClassifySSHFailure` + `ProviderRow`) |

They exist because one behaviour is implemented on both sides of a language
boundary — the Go `shed roost-provider` subcommand and the Rust client core —
and a golden asserted from both is the only thing that makes the two provably
the same rather than the same today.

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
