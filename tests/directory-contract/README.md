# The directory contract

What an account service promises the shed-servers that learn their users
from it, and what each side must do: versioned, public, and tested from
both sides. **This folder is the source of truth.** Any implementation of
either side copies it from here.

```
v1/CONTRACT.md            the document (complete by itself)
v1/MANIFEST.sha256        every file of v1/ but itself, with its SHA-256
v1/fixtures/world.json    the state every exchange starts from
v1/fixtures/test-keys/    throwaway client keys and the token-signing key (public by design)
v1/fixtures/exchanges/    what an implementation of the service answers
v1/fixtures/client/       answers a client must classify
v1/fixtures/keys/         the key corpus: accept/, refuse/, sources/
```

## Rules for this folder

- **Files change only by a reviewed pull request.** A changed fixture is a
  contract change: say so in the pull request and read its diff in review.
- **`v2/` arrives beside `v1/`, never in its place.** A server or service
  that speaks version 1 keeps its fixtures.
- **Public.** Nothing here names a host but `*.example.test` and
  `127.0.0.1`, or an address but `@example.test`. Every key is a throwaway
  test key, made for these fixtures and used nowhere else; the enrolment
  codes in `world.json` are test data.
- `MANIFEST.sha256` lists every file of `v1/` but itself, sorted by path.
  A file it does not list is not part of the contract and must not be
  there.

## How shed holds itself to it

These are Go tests, not a pytest suite:

- `go test ./internal/directory/` checks the manifest against the files
  (an added, removed or changed file without its manifest line fails), runs
  every corpus line through `config.ParseUserKey`, checks the two literal
  tokens, holds every exchange and client fixture to the closed harness
  language `CONTRACT.md` documents (an undocumented member, set-up step or
  placeholder fails, so a misspelling never becomes a default), and checks
  that this folder names no host or address outside `example.test` and
  `127.0.0.1` (a JWT is checked by its decoded header and claims).
- The key corpus's shed-side values (`canonical`, `fingerprint`,
  `algorithm`, `shed_also_refuses`) are recorded from shed's parser:
  `UPDATE_CONTRACT=1 go test ./internal/directory/ -run 'Corpus|Manifest'`
  rewrites them and the manifest. Read the diff: a change means
  `ParseUserKey`, or the version of Go's SSH package shed pins, changed
  what shed accepts.

## Copying it into another implementation

1. Copy `v1/` whole, from a shed commit, without changing a byte:

   ```bash
   git -C <shed checkout> rev-parse HEAD     # the commit you copy from
   rm -rf <dest>/v1
   cp -R <shed checkout>/tests/directory-contract/v1 <dest>/v1
   ```

2. Check the copy is whole: `cd <dest>/v1 && sha256sum -c MANIFEST.sha256`
   must print `OK` for every file. Make your own test suite run the same
   check, so an edited fixture fails it.
3. Record beside it the shed commit and the date, and that the files are
   not edited there: a change is a pull request here first, then copied
   again.
4. Copy again whenever `v1/` changes here (the manifest's diff shows what
   moved).

A service implementation runs every exchange against its own code from a
fresh copy of the world, with the set-up steps `CONTRACT.md` names as hooks
of its test harness, comparing the status, the six headers and the body as
canonical JSON with the placeholders. A client implementation classifies
every client fixture, and serves the exchanges as a fake service, as
`CONTRACT.md` says.
