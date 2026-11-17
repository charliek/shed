# Directory Contract

The **directory contract** is what a shed-server that takes its users from an
account service ([`auth.directory`](configuration.md#users-from-an-account-service))
and the service promise each other: enrolment with a one-time code, the
server's sign-in (OAuth 2.0 client credentials with a JWT client assertion),
and the pull of the user list. It is versioned and public, and it is tested
from both sides. Any implementation of the service side works with shed.

The contract lives in this repository, in
[`tests/directory-contract/`](https://github.com/charliek/shed/tree/main/tests/directory-contract),
and **that folder is the source of truth**: the document and its fixtures are
not restated on this page.

| Path | What it is |
|------|------------|
| [`README.md`](https://github.com/charliek/shed/blob/main/tests/directory-contract/README.md) | The folder's rules, how shed tests itself against it, and how another implementation copies it |
| [`v1/CONTRACT.md`](https://github.com/charliek/shed/blob/main/tests/directory-contract/v1/CONTRACT.md) | Version 1: the three steps, the numbered rules, keys, limits and the fixtures' language. Complete by itself |
| `v1/MANIFEST.sha256` | Every file of `v1/` but itself, with its SHA-256 |
| `v1/fixtures/world.json` | The state every recorded exchange starts from |
| `v1/fixtures/test-keys/` | Throwaway client keys and the token-signing key, public by design |
| `v1/fixtures/exchanges/` | What an implementation of the service answers |
| `v1/fixtures/client/` | Answers a client must classify |
| `v1/fixtures/keys/` | The key corpus: keys accepted and refused, with the values shed's parser gives |

## What version 1 covers

`CONTRACT.md` describes three steps (enrolment, a token, the directory), each
with its requests, answers and refusals, and fourteen numbered rules, each
stating what a service does and, apart, what a client does:

| Rule | Subject |
|------|---------|
| 1 | Sizes of answers |
| 2 | `Retry-After` (delta-seconds only, on 429 and 503) |
| 3 | Redirects (never sent, never followed) |
| 4 | Transport (HTTPS; plain HTTP only to loopback, when asked for) |
| 5 | The organization |
| 6 | The directory answer's top level |
| 7 | Each user decoded alone |
| 8 | A user is a name, an admin flag and keys |
| 9 | Labels |
| 10 | The client assertion's `typ` and `aud` |
| 11 | Ed25519 keys |
| 12 | Placeholders in the fixtures |
| 13 | What the client uses (the token endpoint, the opaque access token) |
| 14 | The standards cited |

A user in the list has no other member: there is no GitHub username, and a
reader ignores members it does not know. How shed applies the list (local
admins, skipped entries, the trust time) is in [Security › Users from an
account service](security.md#users-from-an-account-service).

## How shed holds itself to it

The contract is checked by Go tests, not by a pytest suite:

- `go test ./internal/directory/` checks the manifest against the files, runs
  every key of the corpus through shed's key parser, checks the two literal
  tokens, classifies every client fixture and recorded answer, and checks that
  the folder names no host but `*.example.test` and `127.0.0.1` and no address
  but `@example.test`.
- `go test ./internal/directory/refserver/` holds the [reference
  server](#the-reference-server) to every recorded exchange, and runs shed's
  client against it.

## Changing it, and copying it

- Files change only by a reviewed pull request. A changed fixture is a
  contract change, said so in the pull request.
- A breaking change is `v2/`, beside `v1/`, never in its place.
- Another implementation copies `v1/` whole from a shed commit and checks the
  copy with `sha256sum -c MANIFEST.sha256`; a change it needs is a pull
  request here first. The steps are in the folder's `README.md`.

## The reference server

`shed-directory-ref` is a small implementation of the contract's service
side, in Go (`internal/directory/refserver`), **for tests and local trials,
never for production**. It is in no release or package: build it from a
checkout.

```bash
make directory-ref        # builds bin/test/shed-directory-ref
```

```text
usage: shed-directory-ref --control <dir> [--listen 127.0.0.1:0] [--organization example]
                          [--signing-key <PKCS#8 PEM> --key-id <id>]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--control` | required | The control directory (below) |
| `--listen` | `127.0.0.1:0` | The address and port to listen on: a loopback IP literal (`127.0.0.0/8` or `::1`) only; anything else is refused |
| `--organization` | `example` | The organization the server names |
| `--signing-key` | a new key | A PKCS#8 PEM RSA key to sign access tokens with |
| `--key-id` | - | The signing key's key id (with `--signing-key`) |

It serves plain HTTP only, so a shed-server reaches it only with
`auth.directory.allow_plain_http: true`. Every start prints
`reference implementation of the directory contract, for tests; not for
production` on standard error. On standard output it prints the issuer it
serves as (`listening on http://127.0.0.1:<port>`), which is the URL for
`auth.directory.url`, then one line per enrolment (`enrolled <server name>
<client id>`).

All of its state (enrolled servers, spent codes, the replay ledger of client
assertions) is in memory: a restart is a clean start, and a shed-server
enrolled with it must enrol again. Nothing can be changed over HTTP. What the
operator controls is read from the control directory on each request:

| File | Contents |
|------|----------|
| `users.json` | The list as served: `[{"name", "admin", "keys": [{"key", "label", "expires"?}]}]`, `expires` an RFC 3339 instant. Missing, every lookup fails (never an empty list) |
| `codes` | One `<server name> <code>` per line; each code is spent once, in memory |
| `disabled` | One client id per line: those servers are refused (their sign-in, and the directory with `403 SERVER_DISABLED`). A code for a server name whose enrolment is not disabled is refused with `409 SERVER_NAME_IN_USE` |
| `outage` | Present: every lookup fails as in a storage outage (`/v1` answers `503 UNAVAILABLE`, the token endpoint `503 server_error`) |

The directory and each file must not be writable by group or others, and
none may be a symbolic link. `codes` or `disabled` missing holds nothing.
Edit `users.json` to add, change or remove people; the next pull serves it.

The [User Directory guide](../guides/user-directory.md#try-it-with-the-reference-server)
runs a trial with it.
