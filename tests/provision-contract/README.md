# The provision contract

What a shed-server and a person's credential source promise each other
when a shed is created or provisioned: the request the server sends, the
answer the source returns and the rules it must meet, the two transports
(a machine source's stream, and one HTTPS call to a hosted source: the
account service), and the per-shed record of what was delivered. Versioned,
public, and tested from both sides. **This folder is the source of truth.**
Any implementation of either side copies it from here.

```
v2/CONTRACT.md                  the document (complete by itself, but for the directory contract's token)
v2/MANIFEST.sha256              every file of v2/ but itself, with its SHA-256
v2/fixtures/answers/accept/     answers a server accepts
v2/fixtures/answers/refuse/     answers a server refuses, one rule each
v2/fixtures/requests/           requests, as a server sends them
v2/fixtures/records/accept/     per-shed records, one per main outcome and kind of source
v2/fixtures/records/refuse/     documents that are not records, one rule each
v2/fixtures/stream/             delivery streams, for a machine source to be tested against
v2/fixtures/hosted/exchanges/   what a hosted source answers (in the directory contract's world)
v2/fixtures/hosted/replies/     token and call replies a server must classify
```

## Rules for this folder

- **Files change only by a reviewed pull request.** A changed fixture is a
  contract change: say so in the pull request and read its diff in review.
- **A new version replaces the old one.** Version 2 replaced version 1
  (which is gone, with the code that spoke it): a server, the desktop app
  and the reference programs move together. The next breaking change is
  `v3/`, written in `v2/`'s place.
- **Public.** Nothing here names a host but `*.example.test` and
  `127.0.0.1`, or an address but `@example.test`. Every key, token and value
  is made up for these fixtures and is not a credential.
- `MANIFEST.sha256` lists every file of `v2/` but itself, sorted by path.
  A file it does not list is not part of the contract and must not be
  there.

## How shed holds itself to it

These are Go tests, not a pytest suite:

- `go test ./internal/delivery/` checks the manifest against the files (an
  added, removed or changed file without its manifest line fails), runs
  every answer fixture through the server's validation against the request
  it names and checks the rule each refuse fixture is refused by, decodes
  every request, record and stream fixture, refuses every record refuse
  fixture under its rule, checks that the lists `CONTRACT.md` gives (rule
  names, record rule names, problem codes, error codes, states, reasons,
  outcomes, item results) are the ones the code knows, and checks that
  this folder names no host or address outside `example.test` and
  `127.0.0.1`.
- `go test ./internal/delivery/refsource/` drives the reference source
  against a fake server built from the stream transcripts.
- `go test ./internal/directory/refserver/` runs every hosted exchange
  against the reference account service (`make directory-ref`), which can
  also be told to send every call reply of `hosted/replies/` (its control
  file `deliveries.json`).
- `cargo test -p shed-provision` (in `crates/`) holds the Rust side to the
  same fixtures, rule for rule.
- `UPDATE_CONTRACT=1 go test ./internal/delivery/ -run 'Manifest|Corpus'`
  rewrites the manifest. Read the diff.

The reference source, `shed-provision-ref` (`make provision-ref`, built into
`bin/test/`, in no release), is a machine source that answers from canned
items or sends any refuse fixture; the transport tests and the integration
suite use it as the fake machine.

## Copying it into another implementation

1. Copy `v2/` whole, from a shed commit, without changing a byte:

   ```bash
   git -C <shed checkout> rev-parse HEAD     # the commit you copy from
   rm -rf <dest>/v2
   cp -R <shed checkout>/tests/provision-contract/v2 <dest>/v2
   ```

2. Check the copy is whole: `cd <dest>/v2 && sha256sum -c MANIFEST.sha256`
   must print `OK` for every file. Make your own test suite run the same
   check, so an edited fixture fails it.
3. Record beside it the shed commit and the date, and that the files are
   not edited there: a change is a pull request here first, then copied
   again.
4. Copy again whenever `v2/` changes here (the manifest's diff shows what
   moved). A hosted source also copies the directory contract's `v1/`,
   whose world and test keys the hosted exchanges run in.

A server implementation runs every answer fixture through its validation
and compares the result and, for a refusal, the rule and the status. A
machine source reads every request fixture, reads every stream transcript
as a stream, and builds only answers the accept fixtures' rules allow. A
hosted source runs every exchange of `hosted/exchanges/`; a server
classifies every reply of `hosted/replies/`.
