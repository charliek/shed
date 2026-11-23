# The provision contract, version 1

When a shed is created, or a person runs `shed provision` on it, the
**server** (shed-server) asks that person's **source** for the files and
environment variables the shed should hold: an SSH key, a token, an API
key. The source answers with **items**; the server checks them against the
rules below and writes them into the shed. This document is what a server
and a source may rely on, and what anyone implementing either side must do.
It is complete by itself: it refers to nothing outside `v1/`.

**Contract number: 1.** Every request and every answer carries
`"version": 1`. A breaking change is `v2/`, beside this one. Unlike some
contracts, this one has no optional growth: an unknown member refuses a
document (Conventions), so a new member is a new version.

A source is, in version 1, a **machine source**: a program on one of the
person's machines (the shed desktop app, or the reference source in this
repository) that holds a stream open to the server and answers on it
(section 3). The documents (sections 1 and 2) do not depend on the
transport; a later version may add another transport for the same
documents.

The server never asks anyone but the shed's own user's source, and only
when that user created the shed or ran `shed provision` on it. Who that is,
and when no request is made at all, is the server's business; what it
records about it is section 4.

## Conventions

These apply to every document in this contract: a request, an answer, a
record.

- **A document is one JSON object, as UTF-8.** It is refused as `not_json`
  unless: it is valid UTF-8; no string in it escapes an unpaired UTF-16
  surrogate (a `\ud800` to `\udbff` escape not followed at once by a
  `\udc00` to `\udfff` escape, or a `\udc00` to `\udfff` escape that does
  not follow one), while an escaped pair is text like any other; it is one
  JSON value, an object, with nothing after it but whitespace; and it nests
  no deeper than 16 levels (each object and each array is a level, the
  top-level object level 1). Invalid UTF-8 and unpaired surrogates are
  refused, never repaired.
- **Member names** are compared after JSON unescaping: `"name"` names
  the member `name`.
- **A duplicate member, a `null`, or an unknown member refuses the
  document**, wherever it is: a member named twice in one object
  (`duplicate_member`), a `null` as any value, a member's or an array's
  (`null_value`; a `null` is never the same as an absent member), and a
  member this document does not define for that object (`unknown_member`).
- **Every member is required** unless this document calls it optional. An
  optional member is either present with a valid value or absent.
- **Numbers are compared as written.** `version` is the number written `1`;
  `1.0` and `1e0` are not it.
- **Times** are RFC 3339 instants in UTC, written with `Z` (upper case) and
  whole seconds: `2026-11-23T10:00:00Z`, never `+00:00`, never a fraction,
  and naming a real instant (no 30 February).
- **A file's `mode`** is a string of four octal digits. **`content`** is
  base64 with the standard alphabet and padding (RFC 4648, section 4), no
  line break, and zero padding bits (`YQ==`, never `YR==`).
- **Sizes are bytes.** Every limit on a string is on its UTF-8 bytes, and
  every limit on content is on its decoded bytes.
- **A control character** is U+0000 to U+001F and U+007F to U+009F.
- **A printable character** is a letter, mark, number, punctuation or
  symbol (Unicode general categories L, M, N, P and S), or U+0020 SPACE:
  what Go's `unicode.IsPrint` accepts. Control characters, format
  characters (U+200B ZERO WIDTH SPACE, U+202E RIGHT-TO-LEFT OVERRIDE),
  private-use and surrogate code points, and every white space but U+0020
  (U+00A0, U+2028, U+3000 among them) are not printable. A code point not
  assigned in the reader's Unicode version is not printable either; the
  fixtures use none whose category changed in recent versions.
- **Errors.** An error answer on the transport (section 3) is the API's
  usual error object, `{"error":{"code":"<code>","message":"<text>"}}`;
  an `answer_refused` error also carries the rule broken, beside `error`:
  `{"error":{"code":"answer_refused","message":"<text>"},"rule":"nested_target"}`.
  `code` and `rule` are stable; `message` is for people and may change: a
  source never compares it.
- **This contract's error codes are lower case on purpose.** The rest of
  shed-server's API uses UPPER_SNAKE codes (`FORBIDDEN`,
  `SOURCE_NOT_BOUND`); the five codes of section 3 (`unknown_request`,
  `already_answered`, `deadline_passed`, `not_stream_owner`,
  `answer_refused`) are this contract's own, and lower case marks them so.
  Where the transport refuses a caller the way the rest of the API does,
  it uses the API's codes unchanged.
- **Every response** on the two routes of section 3 carries
  `Cache-Control: no-store`, the stream and a `204` included.
- **A body over 6 MiB** is refused (`413`, rule `body_too_large`) before
  it is read. **A body that is not a document** (the first check above) is
  `400`, rule `not_json`. Every other broken rule is `422`.
- **Public.** Nothing in `v1/` names a host but `*.example.test` and
  `127.0.0.1`, or an address but `@example.test`. Every key, token and
  value in the fixtures is made up for them and is not a credential.

## 1. The request

The server sends one request per delivery: one shed, created or
provisioned.

```json
{
  "version": 1,
  "id": "0123456789abcdef0123456789abcdef",
  "reason": "create",
  "deadline": "2026-11-23T10:00:30Z",
  "user": "ada",
  "shed": {
    "name": "widgets",
    "id": "q6c3g4ymb5m2dkxwe7kyz5hhzu",
    "backend": "firecracker",
    "repo": {"url": "git@git.example.test:example/widgets.git", "name": "example/widgets"},
    "project_dir": "/home/shed/widgets"
  }
}
```

| Member | Rule |
|---|---|
| `version` | `1` |
| `id` | 32 lowercase hex digits; the server makes it at random, one per request |
| `reason` | `create` or `provision` |
| `deadline` | a time: the server takes no answer after it |
| `user` | the shed's user, 1 to 64 bytes with no control character; or the empty string for the implicit user of a server with no user list |
| `shed.name` | the shed's name, 1 to 64 bytes with no control character |
| `shed.id` | the shed's permanent id, 1 to 64 lowercase letters and digits. A name can be reused after a delete; an id never is |
| `shed.backend` | `firecracker`, `vz` or `remote` |
| `shed.repo` | optional: the shed's repository, `{url, name}` |
| `shed.repo.url` | the URL it is cloned from, 1 to 2,048 bytes with no control character |
| `shed.repo.name` | optional: the `owner/name` the URL parses to (two non-empty parts, one slash), at most 255 bytes, with no control character; absent when the URL does not parse to one |
| `shed.local_dir` | optional: the base name of the host directory mounted as the project directory, 1 to 255 bytes with no slash and no control character, not `.` or `..` |
| `shed.project_dir` | optional: the absolute, canonical path of the project directory in the shed (`/`, or segments after one leading slash with no empty, `.` or `..` segment, no trailing slash, no backslash), at most 1,024 bytes with no control character; absent when the shed has none. The answer's `base: project` items land under it (section 2, "Where a file lands") |

The server's own name is not in the request: the source knows which server
it is connected to. A source does not answer a request it cannot parse
under these rules; the request ends at its deadline.

On the stream, a request is written as compact JSON on one line, members
in the order above, optional members absent rather than empty.

## 2. The answer

The source answers a request once, with the items it has for that shed.

```json
{
  "version": 1,
  "id": "0123456789abcdef0123456789abcdef",
  "items": [
    {"name": "ssh-key", "provider": "ssh-key", "kind": "file", "stage": 1, "base": "home",
     "path": ".ssh/id_ed25519", "mode": "0600", "content": "LS0tLS1C...", "fingerprint": "k3y-fp_01"},
    {"name": "example-api-key", "provider": "api-keys", "kind": "env",
     "variable": "EXAMPLE_API_KEY", "value": "fixture-value-not-a-key", "fingerprint": "api-fp-1"}
  ],
  "problems": [
    {"provider": "github", "code": "signed_out", "message": "gh is not signed in"}
  ]
}
```

| Member | Rule |
|---|---|
| `version` | `1` |
| `id` | the id of the request it answers |
| `items` | a list of at most 64 items, possibly empty |
| `problems` | a list of at most 16 problems, possibly empty |

**An item** is a file or an env variable. Each kind takes exactly its own
members, beside the members every item has:

| Member | Kind | Rule |
|---|---|---|
| `name` | both | `[a-z0-9][a-z0-9._-]{0,63}`, unique in the answer |
| `provider` | both | the same alphabet: the provider on the source that made the item |
| `kind` | both | `file` or `env` |
| `stage` | file | `1` (written before the clone) or `2` (written after it) |
| `base` | file | `home` (the shed user's home directory), or `project` (the project directory) with `stage: 2` only, since the project directory exists only after the clone |
| `path` | file | relative to the base, by the path rules below |
| `mode` | file | `0400`, `0600` or `0644` |
| `content` | file | base64; at most 1 MiB decoded, and all of the answer's files at most 4 MiB together |
| `variable` | env | `[A-Z_][A-Z0-9_]{0,127}`, and not refused (below) |
| `value` | env | 1 to 8,192 bytes of printable characters, neither starting nor ending with a space (U+0020). It is used byte for byte: there is no quoting and no expansion |
| `fingerprint` | both | chosen by the source, 1 to 64 of `[A-Za-z0-9_-]`: the same content gives the same fingerprint, so a person can see what changed without the server seeing the content |
| `expires_at` | both | optional: a time after which the item is stale |

An env item is written at stage 1 always and has no `stage` member. A file
item carrying `variable` or `value`, or an env item carrying `stage`,
`base`, `path`, `mode` or `content`, breaks `wrong_kind_member`.

Why the value rule is what it is: the shed reads its environment from
`KEY=VALUE` lines with no quoting, trimming each line's ends of white
space. A value with a control character could end its line and start
another; a value with white space at its ends would lose it; and a value
with an invisible or reordering character (U+200B, U+202E) could differ
from the one a person sees. Printable characters and no space at the ends
make every value survive that file exactly, and look like what it is.

**A problem** says what a provider could not deliver: `{provider, code,
message}`. `provider` has the item name alphabet; `code` is one of the
problem codes below; `message` is at most 256 bytes of printable
characters, for people (it may be empty). A problem is recorded and shown;
a valid problem never refuses the items beside it.

### Problem codes

| Code | Meaning |
|---|---|
| `missing_input` | the provider needs something the person has not set (a file, a setting) |
| `unreadable` | an input exists but could not be read |
| `signed_out` | the provider's tool is not signed in |
| `passphrase_protected` | a key has a passphrase, and keys with one are not delivered |
| `conflict` | two providers produced the same target, and neither was delivered |
| `command_failed` | a command the provider runs failed |

### Path rules

A `path` is a relative, canonical path:

- not empty, at most 255 bytes;
- every character printable (so no control and no format character), and
  no backslash;
- no leading slash and no trailing slash;
- segments separated by single slashes: no empty segment (`a//b`), no `.`
  segment, no `..` segment.

A path is compared as written, byte for byte: these rules make a path's
written form its only form, so `.ssh/./authorized_keys` cannot stand for
`.ssh/authorized_keys` (it breaks `path_dot_segment`).

### Refused targets and variables

No file item may land (below) on these paths under the shed user's home:

- `.ssh/authorized_keys`, `.ssh/authorized_keys2` or `.ssh/known_hosts`
  (who may log in to the shed, and which hosts it trusts, are not the
  source's to set);
- `.config/environment.d`, or anything under it (the shed's own user
  environment directory, which the server writes from the env items).

This holds for both bases: a `base: project` item is refused when it lands
there (a repository named `.ssh` is cloned into `~/.ssh`, so its project
item `authorized_keys` lands on `~/.ssh/authorized_keys`), and it is not
refused when it lands elsewhere (`.ssh/authorized_keys` in a project at
`~/widgets` is `~/widgets/.ssh/authorized_keys`).

No env item may set `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `IFS`,
`ENV`, `BASH_ENV`, `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` or `ALL_PROXY`,
or any variable whose name starts `LD_` or `SHED_`. In an answer, a
variable is upper case by its alphabet, so `path`, `ld_preload` and
`http_proxy` break `bad_variable` before they could break
`refused_variable`. A reader of variables that are not in an answer (the
shed reading a file a person wrote, say) compares them with this list after
upper-casing ASCII letters.

### Where a file lands

The shed user's home is `/home/shed`. An answer is judged against the
request it answers, and each file item **lands** at an absolute path:

- a `base: home` item at `/home/shed/<path>`;
- a `base: project` item at `<project_dir>/<path>`, with the request's
  `shed.project_dir`, wherever that is: under home (the usual
  `/home/shed/<repo>`), outside it (its items then cannot land in home),
  or above it (`/home`: an item `shed/x` lands on `/home/shed/x`, in
  home);
- a `base: project` item of a request with **no** `project_dir` lands
  nowhere: it is compared only with the other project items, by path, and
  is never written (the server skips stage 2 for a shed with no project
  directory).

The refused targets above apply to every file that lands under
`/home/shed`, by its path there.

**Targets.** A file item's target is where it lands; an env item's is its
variable. Two items may not have the same target (`duplicate_target`: two
files at stage 1 and stage 2 on one path, or `home: widgets/.env` and
`project: .env` with the project at `/home/shed/widgets`, are one target).
No file may land at a directory prefix of where another lands
(`nested_target`: `a` and `a/b`, in either order, or `home:
widgets/secrets` and `project: secrets/key` with that project), since one
would have to be a file and a directory at once; `a` and `ab/c` are not
nested. The same path under `home` and under a project at
`/home/shed/widgets` is two targets.

### One answer, one judgement

- **One bad item refuses the whole answer.** Nothing of a refused answer is
  written.
- **A refused answer is consumed:** the request is over, as if it had been
  answered (a repeated post is `already_answered`).
- **An answer with no items is valid**, with or without problems.

### Rule names

The server checks an answer in this order and refuses it under the first
rule it breaks; the rule's name travels in the refusal. The checks run:
the whole document; the top level (members, then each member's type, then
values); each item in order; the rules across items; each problem in
order. Within an item: that it is an object, unknown members, `kind`
(present, a string, a kind), members of the other kind, missing members,
each member's type, then the value rules in the order of this table.
Within a problem: that it is an object, unknown and missing members, types,
then `provider`, `code`, `message`. `duplicate_member` and `null_value` are
judged on the whole document at once, the first in the text deciding.

| Rule | Status | Broken when |
|---|---|---|
| `body_too_large` | 413 | the body is over 6 MiB |
| `not_json` | 400 | the body is not a document (Conventions) |
| `duplicate_member` | 422 | an object names a member twice |
| `null_value` | 422 | a value is `null` |
| `unknown_member` | 422 | an object has a member this document does not define for it |
| `missing_member` | 422 | a required member is absent |
| `wrong_type` | 422 | a member, an item or a problem is not of its JSON type (a string, a number, a list, an object) |
| `unsupported_version` | 422 | `version` is not `1` |
| `bad_id` | 422 | `id` is not 32 lowercase hex digits |
| `id_mismatch` | 422 | `id` is not the id of the request posted to |
| `too_many_items` | 422 | more than 64 items |
| `too_many_problems` | 422 | more than 16 problems |
| `bad_kind` | 422 | `kind` is not `file` or `env` |
| `wrong_kind_member` | 422 | an item carries a member of the other kind |
| `bad_name` | 422 | `name` is outside its alphabet or length |
| `bad_provider` | 422 | an item's or a problem's `provider` is outside its alphabet or length |
| `bad_stage` | 422 | `stage` is not `1` or `2` |
| `bad_base` | 422 | `base` is not `home` or `project` |
| `project_base_stage` | 422 | `base` is `project` with `stage: 1` |
| `path_empty` | 422 | `path` is empty |
| `path_too_long` | 422 | `path` is over 255 bytes |
| `path_not_printable` | 422 | `path` holds a character that is not printable |
| `path_backslash` | 422 | `path` holds a backslash |
| `path_absolute` | 422 | `path` starts with a slash |
| `path_trailing_slash` | 422 | `path` ends with a slash |
| `path_empty_segment` | 422 | `path` has an empty segment |
| `path_dot_segment` | 422 | `path` has a `.` segment |
| `path_dot_dot_segment` | 422 | `path` has a `..` segment |
| `refused_target` | 422 | a file lands on a refused target |
| `bad_mode` | 422 | `mode` is not `0400`, `0600` or `0644` |
| `bad_base64` | 422 | `content` is not base64 as the Conventions define it |
| `file_too_large` | 422 | one file's content is over 1 MiB |
| `bad_variable` | 422 | `variable` is outside its alphabet or length |
| `refused_variable` | 422 | `variable` is refused |
| `value_empty` | 422 | `value` is empty |
| `value_too_long` | 422 | `value` is over 8,192 bytes |
| `value_not_printable` | 422 | `value` holds a character that is not printable |
| `value_edge_space` | 422 | `value` starts or ends with a space (U+0020) |
| `bad_fingerprint` | 422 | `fingerprint` is outside its alphabet or length |
| `bad_time` | 422 | `expires_at` is not a time as the Conventions define it |
| `duplicate_name` | 422 | two items have one name |
| `duplicate_target` | 422 | two items have one target |
| `nested_target` | 422 | one file lands at a directory prefix of where another lands |
| `total_too_large` | 422 | the files' contents are over 4 MiB together |
| `env_too_large` | 422 | the env items, written one `VARIABLE=value` line each with a line feed after it, are over 256 KiB together |
| `bad_problem_code` | 422 | a problem's `code` is not one of the problem codes |
| `bad_problem_message` | 422 | a problem's `message` is over 256 bytes or holds a character that is not printable |

A refusal names the rule and, in its message, where (`items[3].path`),
never a value: the server logs refusals, and an answer's values are
secrets. A member name the contract does not define is written `*` there.

## 3. The transport for a machine source

Two routes on shed-server. A machine source authenticates as its person
(a client certificate on an `mtls` server; nothing on a server in open
mode, which listens on loopback only) and names its machine on both routes
with two headers:

- `X-Shed-Machine-Id`: the machine's id, 32 lowercase hex digits, the same
  on every call;
- `X-Shed-Machine-Name`: its display name, percent-encoded UTF-8, 1 to 63
  bytes decoded, with no control character.

The server takes delivery requests for a user from one machine only, the
user's **bound** machine (the same binding that `shed source use` moves).
A machine becomes bound when it opens the stream and the user has none.

### The stream

```
GET /api/delivery/requests
Accept: text/event-stream
X-Shed-Machine-Id: 5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b
X-Shed-Machine-Name: ada-laptop
```

The answer is `200` with `Content-Type: text/event-stream`,
`Cache-Control: no-store` and `X-Shed-Delivery-Stream: <stream id>`, 32
lowercase hex digits made at random for this stream, then server-sent
events (the HTML standard's `text/event-stream`):

- a **request** is one event of type `request` whose data is one request
  (section 1) on one line:

  ```
  event: request
  data: {"version":1,"id":"0123456789abcdef0123456789abcdef",...}

  ```

- a **comment** line `: keepalive` (and the blank line after it) is
  written every 15 seconds, so a stream that hears nothing for 45 seconds
  can be taken as dead.

The server writes no `id:` and no `retry:` field and ignores
`Last-Event-ID`: a stream never replays. A source parses the stream by the
HTML standard (lines ended by LF, CRLF or CR; comment lines ignored; an
event of any other type ignored).

**One stream per user and machine.** A second stream from the machine
that holds one is refused (below); nothing is replaced.

### The answer

```
POST /api/delivery/requests/<request id>
Content-Type: application/json
X-Shed-Machine-Id: 5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b
X-Shed-Machine-Name: ada-laptop
X-Shed-Delivery-Stream: <the id of the stream the request came on>

<the answer>
```

`204` with no body when the answer is accepted.

### Error codes

The five codes of this contract:

| Code | Status | When |
|---|---|---|
| `unknown_request` | 404 | no request with this id is waiting for this user: never made, made for another user, or ended without an answer (its stream ended, the binding moved, the shed was deleted), or forgotten |
| `already_answered` | 409 | the request was answered already, accepted or refused |
| `deadline_passed` | 410 | the request's deadline passed before the answer came |
| `not_stream_owner` | 403 | the answer came from another certificate, machine or stream than the request went out on, or named no stream |
| `answer_refused` | 413, 400 or 422 | the answer broke a rule; `rule` names it (section 2) |

The server keeps an answered or expired request's outcome for at least
five minutes, so a repeated post is told `already_answered` or
`deadline_passed`; after that it may answer `unknown_request`.

### Every refusal, in order

The server judges a call in this order and answers the first that applies:

| Case | Route | Status and code |
|---|---|---|
| The caller is not a person (a service client), or uses a script key | both | `403 FORBIDDEN` |
| The machine headers are missing or invalid | both | `403 SOURCE_MACHINE_ID_REQUIRED` |
| Admitting a new or changed machine would pass the user's rate limit | stream | `429 SOURCE_RATE_LIMITED`, with `Retry-After` in seconds |
| The machine is not the user's bound machine | both | `403 SOURCE_NOT_BOUND`, naming the bound machine in `source` beside `error` |
| The machine already holds the stream | stream | `409 SOURCE_DUPLICATE_STREAM` |
| No such request for this user | answer | `404 unknown_request` |
| Another certificate, machine or stream | answer | `403 not_stream_owner` |
| Answered already | answer | `409 already_answered` |
| Past the deadline | answer | `410 deadline_passed` |
| The body is over 6 MiB | answer | `413 answer_refused`, rule `body_too_large`, before the body is read |
| The body is not a document | answer | `400 answer_refused`, rule `not_json` |
| The answer breaks a rule | answer | `422 answer_refused`, with the rule |

The first five are the API's own refusals, with its codes; they are the
same answers its credential stream gives today. The checks up to and
including `deadline_passed` read no body, so a caller that does not own a
request cannot consume it. From `body_too_large` on, the request is
consumed: answered, and over.

### Binding

A request is bound, when it is sent, to: the user; the binding's
generation (it changes whenever the bound machine changes); the machine;
the certificate, by its serial number (in open mode, the machine alone,
on loopback only); the one stream it was sent on, by the stream's id; and
the shed's id. An answer that does not match the certificate, the machine
and the stream is `not_stream_owner`. An accepted answer for a shed whose
id has changed since (deleted, and another created under its name) is
dropped by the server, never written.

### The stream's protections

- On a server whose users can change while it runs (users from an account
  service), the caller is checked again before each request is sent and
  at each answer: a user who has left, a key that moved to another user,
  or a certificate past its dates, ends the stream (or is refused).
- A write that the source does not take within a bounded time ends the
  stream, and a connection with too much unacknowledged data is closed.
- **When a stream ends, its pending requests fail at once** (the shed's
  delivery records `source_disconnected`) **and are not replayed** on the
  next stream. An answer to one is `unknown_request`.
- **When the binding moves** to another machine, every pending request of
  that user fails, and the old machine's stream is closed.
- **A source that does not see its `204`** (a lost response) may post the
  same answer again and is told `409 already_answered`: the server has
  acted on the first.

### What a source does

- Holds one stream per server, and opens a new one when it ends, waiting
  first: 1 second, doubled after each stream that failed or held less
  than 60 seconds, at most 30 seconds, and back to 1 second after a stream
  that held 60 seconds; never less than a `Retry-After`.
- Answers each request at most once, before its deadline, on the stream it
  came on, sending that stream's id.
- Drops, unanswered, every request of a stream that has ended.
- Sends only items it would write into the shed itself, for the user the
  request names.

## 4. The record

The server keeps, per shed, a record of its delivery. It holds no content
and no value. The shed document the API returns carries it (a person sees
their own shed's whole record; an admin looking at another user's shed
sees its `state` alone).

```json
{
  "state": "partial",
  "steps": {
    "stage1": {"state": "failed", "reason": "write_failed"},
    "clone": {"state": "done"},
    "stage2": {"state": "owed"},
    "install": {"state": "owed"}
  },
  "last": {
    "at": "2026-11-23T10:00:00Z",
    "reason": "create",
    "outcome": "accepted",
    "failure": {"reason": "write_failed", "item": "github", "result": "is_directory"},
    "source": {"machine_id": "5f0c6e2a9b8d4c1e7a3f2b6d8e9c0a1b", "machine_name": "ada-laptop"}
  },
  "items": [
    {"name": "ssh-key", "provider": "ssh-key", "stage": 1, "kind": "file", "base": "home",
     "target": ".ssh/id_ed25519", "fingerprint": "k3y-fp_01",
     "written_at": "2026-11-23T10:00:02Z", "result": "ok"}
  ]
}
```

| Member | Rule |
|---|---|
| `state` | a delivery state (below), which must be the one the rest of the record gives |
| `steps` | optional: absent only for a shed with no record (state `none`); otherwise the four steps, each `{state, reason}` |
| `steps.<step>.reason` | present exactly when the step is `skipped` or `failed`, one of the reasons that step may carry |
| `last` | optional: the last delivery attempt |
| `last.at` | a time |
| `last.reason` | `create` or `provision` |
| `last.outcome` | an outcome (below) |
| `last.failure` | optional: `{reason, rule, item, result}`: `reason` a failure reason; `rule` (a rule name) exactly when the reason is `answer_refused`; `item` (an item name) and `result` (a writer result but `ok`) exactly when it is `write_failed` |
| `last.source` | optional: `{machine_id, machine_name}`, the machine asked; present when the outcome is `accepted` or `refused` |
| `last.problems` | optional: the answer's problems (at most 16, each `{provider, code, message}` under the answer's problem rules), only with the outcome `accepted`, and never an empty list (absent means none) |
| `items` | optional: the items the shed holds or was sent, at most one per target, each `{name, provider, stage, kind, base, target, fingerprint, expires_at, written_at, result}`; never an empty list (absent means none), and present only with `steps` |
| `items[].stage` | `1` or `2`; an env item's is `1` |
| `items[].base` | present for a file item only |
| `items[].target` | a file item's path, or an env item's variable, under the answer's rules |
| `items[].expires_at` | optional: a time (an empty string is not one) |
| `items[].written_at` | present exactly when `result` is `ok` or `not_in_last_answer`: when it was written |

A shed created before delivery existed has no record and is reported as
`{"state": "none"}`.

Every member not marked optional is required. A record follows the
Conventions (a duplicate member, a `null`, an unknown member, or a member
name in another case refuses it), and a reader holds it to the rules of
this section on the document as written, so an absent member and an empty
one are never confused: `"reason": ""` on an `owed` step is a member that
must not be there.

### Record rule names

A record is refused under the document rules and these answer rules where
they fit (`unknown_member`, `missing_member`, `wrong_type`, `bad_time`,
`bad_name`, `bad_provider`, `bad_kind`, `bad_stage`, `bad_base`,
`project_base_stage`, the path rules, `refused_target`, `bad_variable`,
`refused_variable`, `bad_fingerprint`, `too_many_problems`,
`bad_problem_code`, `bad_problem_message`, `duplicate_target`), and under
these of its own. Record rules do not travel: a record is the server's,
and these say why a document is not one.

| Rule | Broken when |
|---|---|
| `unexpected_member` | a conditional member is present when it must not be (a reason on a `done` or `owed` step, `base` on an env item, `written_at` on an item not written, `rule`, `item` or `result` on a failure that takes none, problems without an accepted answer, `last` or `items` without `steps`) |
| `empty_list` | `items` or `last.problems` is an empty list |
| `bad_state` | `state` is not a delivery state |
| `bad_step_state` | a step's `state` is not a step state |
| `bad_reason` | a step's reason is not one that step takes in that state |
| `bad_last_reason` | `last.reason` is not `create` or `provision` |
| `bad_outcome` | `last.outcome` is not an outcome |
| `bad_failure` | `last.failure.reason` is not a failure that outcome takes |
| `bad_rule` | `last.failure.rule` is not a rule name |
| `bad_machine` | `last.source`'s machine id or name is malformed |
| `bad_result` | an item's `result` is not an item result, or a failure's is not a writer result but `ok` |
| `state_mismatch` | `state` is not the one the rest of the record gives |

### Delivery states

| State | Meaning |
|---|---|
| `none` | no record (the shed predates delivery), or a cloud shed adopted at start-up whose history the server does not know (every step `skipped`, reason `adopted`) |
| `not_applicable` | the last delivery made no request and did not fail: the user has no bound machine, the shed was created by a service client or with a script key, or restored from a snapshot |
| `delivered` | every step is `done` or `skipped`, and the last answer reported no problem |
| `partial` | not the above, and some item is in the shed as written (result `ok`) |
| `not_delivered` | none of the above: nothing is in the shed |

They are decided in that order.

### Steps and step states

Create and `shed provision` run four tracked steps, in this order:
`stage1` (stage 1 items), `clone` (the repository), `stage2` (stage 2
items), `install` (the repository's install hook).

| Step state | Meaning |
|---|---|
| `done` | the step completed |
| `skipped` | the step had nothing to do, for a reason |
| `failed` | the step was tried and failed, for a reason |
| `owed` | the step was never reached, or what it depends on is not `done` or `skipped` |

### Reasons

| Reason | Skips | Fails |
|---|---|---|
| `not_applicable` | `stage1`, `stage2` | |
| `from_snapshot` | every step | |
| `adopted` | every step | |
| `no_items` | `stage1`, `stage2`: the answer had no item for the stage | |
| `no_repo` | `clone` | |
| `local_dir` | `clone`, `stage2`: the project directory is a host directory | |
| `no_project_dir` | `stage2`: the shed has no project directory | |
| `no_hook` | `install` | |
| `no_provision` | `install`: created with `--no-provision` | |
| `source_offline` | | `stage1`: the bound machine was not connected |
| `source_timeout` | | `stage1`: no answer by the deadline |
| `source_disconnected` | | `stage1`: the stream ended before an answer |
| `answer_refused` | | `stage1`: the answer broke a rule |
| `write_failed` | | `stage1`, `stage2`: an item could not be written |
| `image_too_old` | | `stage1`, `stage2`: the shed's image cannot take items |
| `clone_failed` | | `clone` |
| `project_dir_exists` | | `clone`: the project directory exists and is not the repository's work tree |
| `hook_failed` | | `install` |

The six from `source_offline` to `image_too_old` are also the **failure
reasons** of `last.failure`, as the warning a person sees names them.

### Outcomes

| Outcome | Meaning | Failure |
|---|---|---|
| `accepted` | an answer was accepted | none, `write_failed` or `image_too_old` |
| `refused` | the answer broke a rule | `answer_refused` |
| `no_answer` | no answer came | `source_offline`, `source_timeout` or `source_disconnected` |
| `not_requested` | no request was made | none, or `image_too_old` |

### Item results

The first seven are what the writer reports for each item it tries; the
last two are the record's own.

| Result | Meaning |
|---|---|
| `ok` | written |
| `is_directory` | a directory stands at the target |
| `symlink` | a symbolic link stands at the target or on its way |
| `not_directory` | something that is not a directory stands where a directory is needed |
| `permission` | refused by the file system's permissions |
| `other_filesystem` | the target, or a directory on its way, is on another file system than the base's home (a host directory mounted into the shed): a delivered file must not land outside the shed |
| `io` | any other failure, or a writer reply that could not be read |
| `not_written` | in the accepted answer, but not tried: its stage stopped at an earlier failure, or was never reached |
| `not_in_last_answer` | delivered before and absent from the last answer: it stays on disk (a delivery never deletes a file) |

## Limits

| What | Limit |
|---|---|
| Answer body | 6 MiB, refused before it is read |
| Items per answer | 64 |
| Problems per answer | 16 |
| One file's content | 1 MiB, decoded |
| All files' content | 4 MiB, decoded |
| All env items, as `VARIABLE=value` lines with a line feed each | 256 KiB |
| Path | 255 bytes |
| Variable | 128 characters |
| Value | 1 to 8,192 bytes |
| Name, provider, fingerprint | 64 characters |
| Problem message | 256 bytes |
| Nesting | 16 levels |
| Request: user, shed name, shed id | 64 bytes |
| Request: repository URL | 2,048 bytes |
| Request: repository owner/name, local directory | 255 bytes |
| Request: project directory | 1,024 bytes |
| Keep-alive comment | every 15 seconds |
| An ended request's outcome kept | at least 5 minutes |

## The fixtures

`fixtures/` holds what both sides test against. Every file in `v1/` but the
manifest is listed with its SHA-256 in `MANIFEST.sha256` (`<hex>  <path>`
lines, sorted by path), so a copy can be checked whole.

### Answers

`answers/accept/*.json` are answers a server accepts; `answers/refuse/*.json`
are answers it refuses, each under one rule (a refuse fixture breaks exactly
one). Every rule but `body_too_large` has at least one; an oversize body is
generated by a test, not stored. Each file is:

```json
{
  "name": "<file name without .json>",
  "description": "<what it shows>",
  "request": "create-repo",
  "answer": { },
  "fill": [ {"item": 0, "bytes": 1048577} ],
  "expect": {"result": "refused", "status": 422, "rule": "<rule>"}
}
```

- `request` names the request fixture (`requests/<request>.json`) the
  answer is judged against; its id is the one every fixture answers
  (below), and its `shed.project_dir` decides where the `base: project`
  items land. A harness that sends a fixture to a live server uses a shed
  whose request has the same `project_dir`.
- The answer is exactly one of: `answer`, a JSON value, sent as JSON;
  `answer_text`, sent byte for byte as its UTF-8, for a body that is not
  one JSON object or that names a member twice, or whose exact text is the
  point (a number written `1.0`, an escape such as `\ud800`), which a
  harness reading `answer` as a JSON value might rewrite; or
  `answer_base64`, the body's exact bytes in padded standard base64, for a
  body a JSON string cannot carry (bytes that are not UTF-8).
- `fill`, optional and only with `answer`: before sending, set
  `items[item].content` to the base64 of `bytes` bytes, each `0x61`
  (`a`). Contents of a mebibyte and more are filled in, not stored.
- Every fixture answers the request `0123456789abcdef0123456789abcdef`. A
  harness that sends a fixture to a live server replaces that string,
  wherever it appears in the body, with the live request's id; the
  fixtures about ids (`bad-id-*`, `id-mismatch`) do not contain it.
- `expect.result` is `accepted` (and nothing else is given) or `refused`,
  with `status` (413, 400 or 422) and `rule`.
- No other member is part of this language, and a harness refuses a
  fixture that uses one.

### Requests

`requests/*.json` are valid requests: `{"name", "description", "request"}`.
`request` is the document; written compactly, it is exactly the data line
a server sends for it.

### Records

`records/accept/*.json` are valid records, one for each main outcome;
`records/refuse/*.json` are documents that are not records, each breaking
one rule. Each file is `{"name", "description", "record", "expect"}`:
`record` is the record as a JSON value, or `record_text` its exact text
(for a member named twice); `expect` is `{"result": "accepted"}` or
`{"result": "refused", "rule": "<rule>"}`. An accepted record's `state` is
the one its steps, last delivery and items give. Every record rule has a
refuse fixture.

### Stream transcripts

`stream/*.json` are streams as a server writes them, for a source to be
tested against:

```json
{
  "name": "<file name without .json>",
  "description": "<what it shows>",
  "response": {"status": 200, "headers": {"Content-Type": "text/event-stream", "Cache-Control": "no-store", "X-Shed-Delivery-Stream": "<id>"}},
  "chunks": [ {"at_ms": 0, "text": "event: request\ndata: {...}\n\n"} ],
  "expect": {"requests": ["<request id>"]}
}
```

The response's status and headers come first; then each chunk's `text`,
written `at_ms` milliseconds after the response (a harness may scale the
times down); then the server ends the stream. `expect.requests` are the
requests a source reads from it, in order. After the end, a source answers
none of them that it has not answered already.
