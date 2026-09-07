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
| `session.identify.response.json` | the fake's `session.identify` template (the protocol gate's input) |
| `identify.response.json` | the fake's `identify` template |
| `tab.list.session.response.json` | the fake's initial project/tab set — the **session** variant, i.e. the one that carries `revision` |
| `tab.open.response.json` | the fake's template for a tab it opens |
| `response.error.json` | the fake's error-envelope shape (`unknown-op`) |

Shed-recorded vectors from the opencode spikes (`shed.tab.list.opencode.*.json`)
land here in C2, alongside these.
