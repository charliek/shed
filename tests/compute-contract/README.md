# The compute contract

What an account service that supplies compute promises the shed-servers
enrolled with it, and what each side must do: the calls that create, start,
stop, extend and delete a cloud shed's machine, the lease the service
enforces, the machine object and how a server validates it, and every
status and error code with what it means. Versioned, public, and tested
from both sides. **This folder is the source of truth.** Any
implementation of either side copies it from here.

It lives in the directory contract's world: the same enrolment, the same
token endpoint (with its own scope, `compute:manage`), the same exchange
language with named additions.

```
v1/CONTRACT.md            the document (complete by itself, but for the directory contract's token)
v1/MANIFEST.sha256        every file of v1/ but itself, with its SHA-256
v1/fixtures/world.json    the compute world every exchange starts from, beside the directory contract's
v1/fixtures/exchanges/    what an implementation of the service answers
v1/fixtures/machines/     machine objects a server accepts, and one it refuses for each validation rule
v1/fixtures/replies/      replies a server must classify
v1/fixtures/mapping.json  each code's status, its meaning to a server, and shed's API answer
```

## Rules for this folder

- **Files change only by a reviewed pull request.** A changed fixture is a
  contract change: say so in the pull request and read its diff in review.
- **A new version replaces this one.** A server and a service move
  together. Growth that is not breaking is by members a reader ignores and
  by new `access` kinds, as `CONTRACT.md` says; anything else is a new
  version, written in `v1/`'s place.
- **Public.** Nothing here names a host but `*.example.test` and
  `127.0.0.1`, an address but `@example.test`, or an IP address but
  loopback, the documentation ranges (`192.0.2.0/24`, `198.51.100.0/24`,
  `203.0.113.0/24`, `2001:db8::/32`) and the refused classes the
  validation fixtures show. It names no cloud, project, zone or account.
  Every key is a throwaway test key with no private half kept.
- `MANIFEST.sha256` lists every file of `v1/` but itself, sorted by path.
  A file it does not list is not part of the contract and must not be
  there.

## How shed holds itself to it

These are Go tests, not a pytest suite:

- `go test ./internal/remote/provider/service/` checks the manifest
  against the files, accepts or refuses every machine fixture under its
  rule (and holds that every rule has one), classifies every reply fixture
  as it expects and as `mapping.json` says, holds `mapping.json` equal to
  the classifier's code table and `CONTRACT.md`'s code list, holds every
  error an exchange answers to its code's status, and checks that this
  folder names no host or address outside what the rules above allow.
- `go test ./internal/directory/refserver/` runs every exchange against
  the reference compute service (inside the reference account service,
  `make directory-ref`), holds the service's own world equal to
  `world.json`, and has the service send every reply of `replies/` over
  real HTTP, classified by shed's client code as the fixture expects. Its
  own Go tests also hold that the service answers `503` rather than send a
  list over 500 machines or 4 MiB, and that it records a host-key mismatch
  with one fixed line (`compute: host key mismatch: <shed id>`, no key in
  it).
- `UPDATE_CONTRACT=1 go test ./internal/remote/provider/service/ -run Manifest`
  rewrites the manifest. Read the diff.

The reference service's command, `shed-directory-ref`, is steered by
`compute.json` in its control directory: set-up steps in the exchange
language (each applied once, by its id), forced replies by call, and an
`auto_settle_s` that settles cloud work on its own, for the integration
suite.

## Copying it into another implementation

1. Copy `v1/` whole, from a shed commit, without changing a byte:

   ```bash
   git -C <shed checkout> rev-parse HEAD     # the commit you copy from
   rm -rf <dest>/v1
   cp -R <shed checkout>/tests/compute-contract/v1 <dest>/v1
   ```

2. Check the copy is whole: `cd <dest>/v1 && sha256sum -c MANIFEST.sha256`
   must print `OK` for every file. Make your own test suite run the same
   check, so an edited fixture fails it.
3. Record beside it the shed commit and the date, and that the files are
   not edited there, next to the directory and provision contracts' copies
   (all three from one commit).
4. Copy again whenever `v1/` changes here.

A service implementation runs every exchange against its own code, from a
fresh copy of both worlds, with the set-up steps `CONTRACT.md` names as
hooks of its test harness (its lease sweep run by `clock.advance`, its
cloud by a fake that `cloud.fail` and `cloud.settle` steer), comparing the
status, the six headers and the body as canonical JSON with the
placeholders.
