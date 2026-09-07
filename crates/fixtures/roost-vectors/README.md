# roost IPC vectors (vendored)

Byte-for-byte copies of roost's own golden wire vectors, taken from

    github.com/charliek/roost @ 61d8713bcd0378971ad8490fe89b8dbef4e49fac
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

## What is here, and who reads it

| file | read by |
|---|---|
| `session.identify.response.json` | the fake's `session.identify` template (the protocol gate's input); the fence tests' `daemon_session_id` / `started_at` |
| `identify.response.json` | the fake's `identify` template |
| `tab.list.session.response.json` | the fake's initial project/tab set — the **session** variant, i.e. the one that carries `revision` (42); the fence replay's snapshot |
| `tab.open.response.json` | the fake's template for a tab it opens |
| `response.error.json` | the fake's error-envelope shape (`unknown-op`) |
| `events.batch.json` | the fence replay's batch shape (`{revision, events: […]}`) and its revision-42 replay |
| `tab.opened.event.json` | the fence fold's unowned-tab-opens case |
| `tab.state_changed.event.json` | the fence fold's ignored-derived-projection case |
| `agent_report.changed.event.json` | the fence fold's claim/release case (a `claude` adapter taking tab 5) |
| `session.stopping.event.json` | the fence fold's "an envelope that is not a workspace fact" case |

## Shed-recorded vectors

`shed.tab.list.opencode.finished.json`, `shed.tab.list.opencode.over-ssh.json`,
`shed.tab.dump.opencode.json` and `shed.session.identify.json` were **recorded by
shed** from a real `roost-session` (release build of rev `61d8713…`, protocol 2)
on 2026-09-07; never edited; they pin the row mapping against real adapter
output.

`shed.tab.list.opencode.finished.json` is the local spike and is the shape the
row mapping is really about: a **plain shell tab** (id 3, unowned) sits beside
the opencode tab (id 4, owned, `finished`, `detail: "session_idle"`, with the
adapter's `agent`/`model`/`version` metadata). One of those two is a session row;
the other is somebody's terminal. `shed.tab.list.opencode.over-ssh.json` is the
same pair read from inside a shed VM over roost's SSH client-bridge.
