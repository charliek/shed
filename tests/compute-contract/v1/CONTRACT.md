# The compute contract, version 1

An **account service** that supplies a server's users (the directory
contract) can also supply its **compute**: it holds the cloud credential,
decides which sizes and images exist, who may have how many machines and
for how long, makes every cloud call, and deletes each machine when its
lease ends. A **server** (shed-server is one) asks the service for a
machine for each cloud shed it runs, follows the work to its end, and
reaches the machine over SSH where the service says. This document is what
a server and a service may rely on, and what anyone implementing either
side must do. It is complete by itself, except that a server reaches the
service with a bearer token obtained as the directory contract's section 2
describes.

A **machine** is the service's unit of compute behind one shed. A
**person** is a user on the service's list (the directory contract's
users); a **client** is a program a server lets create machines (for
shed, one of its service clients), which the service knows by name once it
is registered for that server. **The server's name** is the name it is
enrolled under in the service.

**Contract number: 1.** A breaking change replaces this document in place,
and the server and the service move together. Growth that is not breaking
comes in two forms only: a member either side does not know is ignored
(Conventions), and an `access` option of a kind a server does not know is
skipped (section "Access").

## Conventions

- **Transport.** HTTPS, JSON, UTF-8. A server may use plain HTTP only to a
  loopback address, and only when its operator asks for it (tests). No
  answer under `/v1/compute/` is a redirect, and a server never follows one.
- **The token.** Every call carries `Authorization: Bearer <token>`, a
  token from the directory contract's token endpoint, by a separate token
  request that sends `scope=compute:manage`. Its `200` has the directory
  contract's shape with `"scope":"compute:manage"`. A service lets a server
  manage compute only when its operator has said so (the **grant**, off by
  default): without it the token request answers `400 invalid_scope` and
  the assertion is not spent. The scope is never in the service's
  metadata, and a token request with no `scope` is answered
  `directory:read` alone, as before. The grant is read on every call: a
  token issued before the grant was removed gets `403 INSUFFICIENT_SCOPE`.
- **Ownership.** Every machine and every operation belongs to the server
  that made it, by the **server's name**: a server that enrols again under
  the same name (how its key is rotated) has the same machines. A server
  never sees another name's machines: a read of one is `404 NOT_FOUND`, a
  delete is `204`, and the list does not hold it.
- **Documents.** A request body and a reply body are each one JSON object
  in valid UTF-8, with nothing after it but whitespace, no member named
  twice in any object (names compared after unescaping), no escaped
  unpaired UTF-16 surrogate, and nesting no deeper than 16 levels. JSON's
  whitespace is allowed wherever JSON allows it, before the object
  included: a value is judged by what it is, never by its first byte. Both
  sides ignore a member this document does not define, wherever it is and
  whatever its value, `null` included. Member names are compared exactly:
  `LIMITS` is not `limits`, and is an unknown member. A member is required
  unless this document says it is optional; a `null` is a value only where
  this document allows it.
- **Times** are RFC 3339 instants in UTC, written with `Z` and whole
  seconds (`2026-11-16T06:00:00Z`), naming a real instant.
- **Errors** are `{"error":{"code":"<CODE>","message":"<text>"}}`, the
  directory contract's `/v1` shape, with the status the matrix gives. A
  server reads `error.code` only. **`message` is dropped where the body is
  decoded: never logged, stored or shown.** Any text in a reply that this
  document does not give a grammar is treated the same way.
- Every answer carries `Cache-Control: no-store`.
- **Deadlines.** A service answers every call within **10 seconds**: its
  judgement, its own storage work and at most one cloud call, bounded at
  8 seconds. A server waits at most **12 seconds** for any one call (it
  then treats the call as unanswered, section "Status and error matrix").
  A service that cannot finish cloud work inside its deadline accepts it
  and reports it through an operation.

## Names and values

Every string a server may show, log or use in a command is held to a
grammar here, and a server refuses a reply that breaks one (section
"Validation").

| Value | Grammar |
|---|---|
| shed id (`shed_id`, the path's `{shed_id}`) | `^[a-z2-7]{26}$` |
| shed name (`name`) | `^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$` |
| person (`user`) and client (`client`) | `^[a-z][a-z0-9-]{0,62}$` |
| a word: `provider`, a size's `name` and `arch`, an image's `requested` and `resolved`, an alias, an access kind | `^[a-z0-9][a-z0-9._-]{0,62}$` |
| operation id | `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$` |
| `request_id` | a UUID in lower case: `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$` |
| label key | `^[a-z][a-z0-9_]{0,62}$` |
| label value | `^[a-z0-9_-]{0,63}$` |
| error code | `^[A-Z][A-Z0-9_]{0,63}$` |
| a source (ingress) | an IPv4 or IPv6 prefix in its canonical form: the network address and the length, as `203.0.113.0/24` or `2001:db8:5::/48` |

## Status and error matrix

This table is normative for every call.

| Status | Body | A server |
|---|---|---|
| 2xx | as each call says | decodes it; a body that breaks section "Validation", a 2xx the call does not answer, or a body over its limit, is an **invalid reply** |
| 401 | any | gets a new token once and sends the call once more; a second 401 is **unavailable** |
| 403 | a code: `INSUFFICIENT_SCOPE`, `SERVER_DISABLED`, `COMPUTE_DENIED` or a refusal of the list | denied (the grant is off, the server is disabled, the service refuses this server compute), or that refusal |
| 404 | `NOT_FOUND` | the machine or operation does not exist for this server |
| 409, 422 | a code of the list | a refusal: nothing was changed |
| 413 | any | its own bug (a body over 64 KiB): it does not retry |
| 429 | `RATE_LIMITED`, with `Retry-After` | waits and sends the call again (below), inside its deadline; past the deadline the call is **unavailable** |
| 5xx, no reply within 12 s, a reset, a body cut short | any | for a call that changes something, **the outcome is unknown**: it retries with the same `request_id` or reads the machine; for a read, **unavailable** |
| any other (a 3xx, 400, 405, 410…) | any | as 5xx |

- **Codes are honoured at their status only.** A code outside the list, a
  listed code at another status than the list gives it, an error body that
  is not the shape above, or one over 4 KiB (a server reads no more), is
  read as its status's default: 403 `COMPUTE_DENIED`, 404 `NOT_FOUND`, 409
  and 422 `INVALID`.
- **`Retry-After`** on a 429 is honoured as the directory contract's rule 2
  reads it (delta-seconds, given once, leading zeros allowed: `00007` is 7
  seconds), capped at 30 seconds whatever its number of digits. Absent or
  malformed, the server waits 5 seconds.
- **Credentials.** A service never puts the token or the client assertion
  a server presented in a reply. **A server refuses a reply that echoes the
  bearer token or the client assertion** it obtained that token with,
  whatever the reply's status: in the body's bytes, or in any string the
  body decodes to (a member name or a value, its escapes resolved, an error
  `message` included before it is dropped). Such a reply is an **invalid
  reply** (for a call that changes something, its outcome is unknown), and
  nothing of it is used: not its code, not its machine. Three points make
  the check whole:
  - **Each attempt against its own credentials.** A call sent again after
    a 401 presents a new token, obtained with a new assertion; the 401 is
    checked against the first pair, the retry's reply against the new one.
  - **The bytes read, even past a limit.** A body over its limit (a 2xx's,
    or an error body's 4 KiB) is checked in the bytes the server read
    before it is set aside.
  - **The whole document, and fail closed.** The scan reads every string
    of a document the strict reader takes, to its end, keeping each number
    as its text (a number too large for a float, `1e400`, is valid JSON and
    never stops the scan). A scan that fails on such a document counts as
    an echo.
- `fixtures/mapping.json` gives each code its status, what it means to a
  server (its `provider` error kind, and whether it is a refusal a client
  may retry later), and what shed's API answers when it reaches a caller.
  Its `failures` give the same for an unavailable call, an invalid reply
  and a 413.

## Codes

| Code | Status | Meaning |
|---|---|---|
| `NOT_FOUND` | 404 | no machine with this shed id, or no operation with this id, for this server; also an unknown path |
| `EXISTS` | 409 | the create's `name` is held by another live machine of this server (another shed id) |
| `SPEC_MISMATCH` | 409 | the shed id exists with a different create body |
| `BUSY` | 409 | the machine is in a transition that does not allow the call; nothing was changed. Wait for it and call again |
| `INVALID` | 422 | the request is malformed: a body that is not a document, a member missing or of the wrong type or outside its grammar or range, a path id outside its grammar, a `request_id` used for another call, a labels write that would leave more than 32 labels |
| `LABEL_TAKEN` | 409 | a labels write's `expect` does not hold |
| `RESERVED_LABEL` | 422 | a create's `labels` or a labels write names a reserved label |
| `QUOTA_PERSON` | 409 | the person holds as many machines as the policy lets them, counting every server and every state |
| `QUOTA_ORG` | 409 | the organization holds as many machines as the policy lets it, counting every server, every person, every client and every state |
| `SIZE_NOT_ALLOWED` | 403 | the policy does not let this person have this size |
| `SIZE_UNKNOWN` | 422 | the catalog has no such size |
| `IMAGE_UNKNOWN` | 422 | the image is neither `default` nor an alias of the catalog |
| `LEASE_TOO_LONG` | 422 | a client's `lease_s` is above the policy's `client.max_lease_s` |
| `EXTEND_NOT_ALLOWED` | 403 | the machine's lease cannot be extended: it is a client's, or the policy does not let people extend |
| `LEASE_AT_CAP` | 409 | the extend would take the lease past `max_expires_at`; nothing changes |
| `LEASE_ENDED` | 409 | a start or an extend at or after the machine's `expires_at` |
| `DELETING` | 409 | the machine is being deleted |
| `PERSON_INACTIVE` | 403 | a start or extend of a person's machine, and the person is no longer a live person on the list, or has opted this server out |
| `PERSON_UNKNOWN` | 403 | a create for a handle that is not a live person on the service's list, or whose person has opted this server out |
| `CLIENT_UNKNOWN` | 403 | a create for a client not registered for this server |
| `CLOUD_QUOTA` | 409 | the cloud refused for its own quota |
| `CLOUD_CAPACITY` | 409 | the cloud had no room for the machine |
| `ACCESS_UNSUPPORTED` | 422 | the create's `access_kinds` name none the service offers |
| `INGRESS_NOT_WRITABLE` | 403 | the service's ingress is managed elsewhere: a server may read it, not write it |
| `INGRESS_TOO_WIDE` | 422 | an ingress source is wider than the service allows |
| `COMPUTE_DENIED` | 403 | the service refuses this server compute by its own policy |
| `INSUFFICIENT_SCOPE` | 403 | the token lacks `compute:manage`, or the grant was removed after it was issued |
| `SERVER_DISABLED` | 403 | the server was disabled while it held a token |
| `RATE_LIMITED` | 429 | too many calls; `Retry-After` says when to try again |
| `UNAVAILABLE` | 503 | the service cannot reach its storage or the cloud's reply was lost; for a call that changes something, the outcome is unknown |

`INSUFFICIENT_SCOPE` and `SERVER_DISABLED` are the directory contract's
codes, with the same meaning. `CLOUD_QUOTA` and `CLOUD_CAPACITY` may come
from a create or start itself, or as an operation's error.

## Idempotency

- **Every call that changes something** (create, start, stop, delete, a
  labels write, extend, an ingress write) carries `request_id`, a UUID the
  server makes for that request and stores with its job, so that a retry
  after a lost reply sends the same one.
- **A request id's answer is kept 24 hours.** The service keeps the first
  2xx answer to each `request_id` of each server, and answers the same id
  again with that answer, byte for byte, whatever has happened to the
  machine since and whatever the repeated body says. A refusal is not
  kept: the same id sent again is judged again. A `request_id` the server
  used for another call (another route or another machine) is `422
  INVALID`.
- **A create is also idempotent by its shed id.** A create for a shed id
  that exists with the **same body** answers the existing machine: `202
  {machine, operation}` with the create's operation while it is still
  creating, `200 {machine}` after. The same body means: every member this
  document defines but `request_id` has an equal value (objects member by
  member, arrays element by element, strings by their characters, numbers
  by value); a member present in one and absent from the other differs;
  members neither side defines are not compared. A different body is `409
  SPEC_MISMATCH` and changes nothing. A shed id whose machine has been
  deleted may be created again: it is a new machine.

## The calls

Each call below lists its answers. A service checks a call in this order,
and answers the first check that fails: the route and its method (an
unknown path is `404 NOT_FOUND`, a wrong method `405`), the token (`401`,
as the directory contract says), its scope (`403 INSUFFICIENT_SCOPE`), its
rate limit (`429`), the server's record (gone: `401`; disabled: `403
SERVER_DISABLED`; a lookup that fails: `503 UNAVAILABLE`), the grant (`403
INSUFFICIENT_SCOPE`), the body's size (`413`), the body's media type
(`application/json`) and the path's ids (`422 INVALID`), and then the
call's own checks, in the order each call lists them.

### Catalog

`GET /v1/compute/catalog` answers `200`:

```json
{
  "sizes": [{"name": "standard", "cpus": 2, "memory_mb": 8192, "arch": "amd64", "disk_gb_default": 50}],
  "default_size": "standard",
  "images": {"default": "shed-remote-full-v1-0-0-20261101-000000", "aliases": ["base", "cloud"]},
  "access_kinds": ["ssh"],
  "policy": {
    "person": {"initial_lease_s": 86400, "max_age_s": 432000, "extend_allowed": true, "presets_h": [6, 12, 24, 48]},
    "client": {"max_lease_s": 432000},
    "warn_s": 7200
  },
  "limits": {"person_max": 4, "org_max": 10},
  "ingress": {"writable": true}
}
```

Every member is required and none is `null`. `sizes` holds 1 to 64
sizes, no name twice; `default_size` names one of them.
`images.default` is the name the image `default` resolves to now, for
display; `aliases` are the other names a create may give (0 to 64, none
twice, never `default`), ordered by name. `access_kinds` are the kinds the
service offers (1 to 8, none twice). Every duration (`initial_lease_s`,
`max_age_s`, `max_lease_s`, and `warn_s`, which may be 0) is from 1 second
to one year (31536000 seconds), with `max_age_s` no less than
`initial_lease_s`; `person_max` and `org_max` are 0 to 1000000. `policy.person` is a
person's first lease and cap (seconds), whether a person may extend, and
the extend presets in hours (1 to 720 each); `policy.client.max_lease_s`
bounds a client's lease; `warn_s` is how long before a lease's end a
server warns the person. `limits` are the machines a person and the
organization may hold. `ingress.writable` is false for a service whose
ingress is managed elsewhere. A server reads the catalog at start and on
every pass of its own, and keeps it.

### Create

`PUT /v1/compute/machines/{shed_id}` with:

```json
{
  "request_id": "00000000-0000-4000-8000-000000000001",
  "name": "widgets",
  "user": "ada",
  "size": "standard",
  "image": "default",
  "disk_gb": 50,
  "labels": {"shed_layout": "1"},
  "authorized_keys": ["ssh-ed25519 AAAA…"],
  "spec": "…",
  "access_kinds": ["ssh"]
}
```

| Member | Rule |
|---|---|
| `request_id` | as section "Idempotency" says |
| `name` | the shed's name; unique among this server's live machines |
| `user` or `client` | exactly one: a person's handle, or a registered client's name |
| `size` | a catalog size's name |
| `image` | `default` or a catalog alias |
| `disk_gb` | optional, 1 to 65536; absent, the size's `disk_gb_default` |
| `labels` | the server's working labels (section "Labels"), possibly empty |
| `authorized_keys` | 1 to 4 public key lines the guest's link user accepts, each at most 8 KiB of printable ASCII: a key type, one space, the base64 key, an optional comment |
| `spec` | at most 4 KiB, opaque to the service, returned verbatim |
| `access_kinds` | 1 to 8 access kinds the server can use, in its order of preference |
| `lease_s` | for a client's machine only, and required there: its lease in seconds, at least 60 |

The checks, in order, after those of every call:

1. The body, in this order: a document with a valid `request_id` (`422
   INVALID`); no reserved label in `labels` (`422 RESERVED_LABEL`); every
   other member under its rule, `lease_s` absent on a person's machine and
   present on a client's (`422 INVALID`).
2. The request id's kept answer, if any (section "Idempotency").
3. A machine with this shed id: the same body answers it; a different body
   is `409 SPEC_MISMATCH`.
4. Another live machine of this server with this `name`: `409 EXISTS`.
5. **The judgement**, made before any cloud work: the person is a live
   person on the list who has not opted this server out (`403
   PERSON_UNKNOWN`), or the client is registered for this server (`403
   CLIENT_UNKNOWN`); the size is in the catalog (`422 SIZE_UNKNOWN`) and
   allowed for this person (`403 SIZE_NOT_ALLOWED`; a client may have any
   catalog size); the image is `default` or an alias (`422
   IMAGE_UNKNOWN`); a client's `lease_s` is within `client.max_lease_s`
   (`422 LEASE_TOO_LONG`); one of `access_kinds` is offered (`422
   ACCESS_UNSUPPORTED`); the person holds fewer than `person_max` machines
   (`409 QUOTA_PERSON`); the organization holds fewer than `org_max` (`409
   QUOTA_ORG`). Every machine not yet deleted counts, in any state, on any
   server: stopped machines, creating machines and clients' machines
   included.
6. The machine is reserved (it counts from now on) and the cloud work
   starts. The cloud may refuse at once (`409 CLOUD_QUOTA`, `409
   CLOUD_CAPACITY`: the reservation is released) or later (the operation
   ends with that error, and the machine is gone, with no deletion
   listed).

Answers: `202 {"machine": <the machine, creating>, "operation": "<id>"}`;
`200 {"machine": <the machine>}` for an existing machine with the same
body, once its create has finished.

### Start and stop

`POST /v1/compute/machines/{shed_id}/start` and `…/stop` with
`{"request_id": "…"}`. The checks, in order: the body (`422 INVALID`);
the request id's kept answer; no such machine (`404 NOT_FOUND`); the
machine is being deleted (`409 DELETING`); then, for a start only: the
lease has ended, that is the service's clock is at or after `expires_at`
(`409 LEASE_ENDED`), and a person's machine whose person is no longer
live or has opted this server out (`403 PERSON_INACTIVE`). Then the
state:

| Call | Machine | Answer |
|---|---|---|
| start | `stopped` | `202 {"operation": "<id>"}`; the machine is `starting` |
| start | `running` | `200 {"machine": …}` |
| start | `starting` | `202 {"operation": "<id>"}`, the start in progress |
| start | `creating`, `stopping` | `409 BUSY` |
| stop | `running` | `202 {"operation": "<id>"}`; the machine is `stopping` |
| stop | `stopped` | `200 {"machine": …}` |
| stop | `stopping` | `202 {"operation": "<id>"}`, the stop in progress |
| stop | `creating`, `starting` | `409 BUSY` |

The cloud may also answer `409 BUSY` itself. A stopped machine keeps its
disk. **Stop and start change neither `expires_at` nor `max_expires_at`.**
Stop and delete are allowed whoever the machine's person is and whatever
its lease.

### Delete

`DELETE /v1/compute/machines/{shed_id}` with `{"request_id": "…"}`. The
checks, in order: the body (`422 INVALID`); the request id's kept answer;
then `204` (no body) when the server has no such machine, or `202
{"operation": "<id>"}`. A delete of a machine already being deleted
answers that delete's operation. A delete ends any create, start or stop
in progress on the machine: that operation ends with error `DELETING`. A
deleted machine's disk is deleted with it.

### Operations

`GET /v1/compute/operations/{operation}` answers `200` with one of:

| Body | Meaning |
|---|---|
| `{"done": false}` | the work is in progress |
| `{"done": true, "machine": <the machine>}` | done; the machine as it is now |
| `{"done": true, "machine": null}` | done, and the machine no longer exists: a finished delete |
| `{"done": true, "error": {"code": "<CODE>"}}` | the work failed with that code: `CLOUD_QUOTA`, `CLOUD_CAPACITY`, `DELETING` (a delete ended it), `LEASE_ENDED` (the lease's end ended it), or another code of the list. A code outside the list means the work failed for a reason the server cannot name (`UNAVAILABLE`) |

An operation belongs to the server whose call started it, and to **the
machine it was made for**: once that machine is gone, a done operation
answers `machine: null` even if the shed id has been created again since,
and never names the new machine. It is kept 24 hours after it is done (a pending one is kept until it is done); after
that, or for an id this server never had, the answer is `404 NOT_FOUND`,
which means "unknown or expired: read the machine". A server polls an
operation every 2 seconds, then every 5 seconds, until it is done.

### Get

`GET /v1/compute/machines/{shed_id}` answers `200 {"machine": …}` or `404
NOT_FOUND`.

### The list

`GET /v1/compute/machines` answers `200`:

```json
{
  "machines": [<machine>, …],
  "deleted": [{"shed_id": "…", "at": "2026-11-17T06:00:00Z", "cause": "lease_ended"}],
  "complete": true
}
```

- `machines` is every live machine of the calling server, at most 500,
  ordered by shed id, no shed id twice. A deleted machine is not in it.
- `deleted` is every machine of this server deleted in the last 24 hours,
  ordered by `at` and then by shed id, with its `cause`: `lease_ended`
  (the lease's end), `page` (deleted at the service, by a person or an
  operator), `external` (gone from the cloud without the service), or
  `server` (the server's own delete). A create the cloud refused is not a
  deletion.
- **A list is never partial.** `complete` is always `true`; a service that
  cannot read everything it needs answers `503 UNAVAILABLE`, and so does
  one that would answer more than 500 machines or a body over 4 MiB (a
  service keeps its policy within the first, and the contract's limits on
  a machine object keep 500 of them within the second only when their
  `spec`s are not mostly escaped bytes). A server
  refuses a list with `complete: false`, more than 500 machines, or a
  cause outside the four, as an invalid reply.
- **A server drops its record of a shed only when the list omits its
  machine and an exact `GET` of that shed id answers `404`.** A list that
  fails, or that a server refuses, drops nothing.

### Labels

`POST /v1/compute/machines/{shed_id}/labels` with:

```json
{"request_id": "…", "set": {"shed_state": "ready", "old": null}, "expect": {"shed_state": null}}
```

One atomic write: every key of `set` is set to its value, or removed when
the value is `null`; `expect`, optional, is a condition on the labels
before the write: a value means "holds exactly this", `null` means "is
absent". The checks, in order: the body (`422 INVALID`), with a reserved
label in `set` or `expect` first (`422 RESERVED_LABEL`); the request id's
kept answer; no such machine (`404 NOT_FOUND`); the machine is being
deleted (`409 DELETING`); `expect` does not hold (`409 LABEL_TAKEN`,
nothing written); the write would leave more than 32 labels (`422
INVALID`, nothing written). Answer: `200 {"machine": …}`.

### Extend

`POST /v1/compute/machines/{shed_id}/extend` with `{"request_id": "…",
"by_s": 21600}`: `by_s` is whole hours, 3600 to 2592000. The checks, in
order: the body (`422 INVALID`); the request id's kept answer; no such
machine (`404 NOT_FOUND`); being deleted (`409 DELETING`); at or after
`expires_at` (`409 LEASE_ENDED`); a client's machine, or a policy that does
not let people extend (`403 EXTEND_NOT_ALLOWED`); the machine's person no
longer live or opted out (`403 PERSON_INACTIVE`); `expires_at + by_s`
after `max_expires_at` (`409 LEASE_AT_CAP`: refused whole, nothing
changes). Then `expires_at` moves by `by_s` and the answer is `200
{"machine": …}`. An extend that reaches `max_expires_at` exactly is
accepted. Because the answer is kept by `request_id`, an extend sent again
after a lost reply extends once.

### Usage

`GET /v1/compute/usage[?user=<handle>]` answers `200`:

```json
{"person": {"machines": 2, "running": 1, "stopped": 1, "max": 4}, "org": {"machines": 3, "running": 2, "stopped": 1, "max": 10}}
```

`org` counts every machine of the organization on every server; `person`
counts the person's, on every server, and is `null` without `user`, and
for a handle the create judgement would refuse (not a live person on the
list, or opted out of this server). `machines` counts every machine not
yet deleted; `running` those `creating`, `starting` or `running`;
`stopped` those `stopping` or `stopped` (a machine being deleted is in
`machines` only). `max` is the policy's limit. `user` given twice is `422
INVALID`.

### Ingress

`GET /v1/compute/ingress` and `PUT /v1/compute/ingress` (with
`{"request_id": "…", "sources": ["203.0.113.0/24"]}`) answer `200`:

```json
{"sources": ["203.0.113.0/24"], "exclusive": true, "applied_at": "2026-11-16T06:00:00Z"}
```

`sources` are the prefixes this server's machines admit SSH from (0 to 64,
canonical, none twice, in a reply as in a write), as last applied; `applied_at` is when, or `null`
before any write. A write replaces the sources whole. The checks, in
order: the body (`422 INVALID`); the request id's kept answer; a service
whose ingress is managed elsewhere (`403 INGRESS_NOT_WRITABLE`: a server
then only compares what it reads); a service that cannot read its rules
(`503 UNAVAILABLE`, nothing applied); a source wider than the service
allows (`422 INGRESS_TOO_WIDE`). A read of a service that cannot read its
rules is `503 UNAVAILABLE` too.

**`exclusive` (normative).** `true` means all of these hold, and `false`
that the service cannot say so:

1. this server's machines admit inbound traffic only on their `access`
   ports, and only from exactly `sources`;
2. no machine, of any server or person, can reach another;
3. the machines are on no network the service shares with anything else
   that admits inbound traffic;
4. the service read every rule it needs to say the first three. A service
   that could not read them answers `503 UNAVAILABLE`, never `true` or
   `false`.

## The machine object

```json
{
  "shed_id": "q6c3g4ymb5m2dkxwe7kyz5hhzu", "name": "widgets", "server": "srv-a", "user": "ada", "client": null,
  "state": "running",
  "size": {"name": "standard", "cpus": 2, "memory_mb": 8192, "arch": "amd64"},
  "disk_gb": 50, "image": {"requested": "default", "resolved": "shed-remote-full-v1-0-0-20261120-120000"},
  "provider": "cloud-a",
  "created_at": "2026-11-16T06:00:00Z", "started_at": "2026-11-16T06:00:00Z", "stopped_at": null,
  "lease": {"expires_at": "2026-11-17T06:00:00Z", "max_expires_at": "2026-11-21T06:00:00Z", "extendable": true, "presets_h": [6, 12, 24, 48]},
  "access": [{"kind": "ssh", "host": "203.0.113.7", "port": 22, "user": "shed-link", "host_key": "ssh-ed25519 AAAA…"}],
  "labels": {"shed_layout": "1"},
  "spec": "…"
}
```

| Member | Rule |
|---|---|
| `shed_id`, `name` | the create's |
| `server` | the server's name |
| `user`, `client` | exactly one is a string, the other `null`: **the truth about who owns the machine**, fixed at create |
| `state` | `creating`, `running`, `stopped`, `starting`, `stopping` or `deleting` |
| `size` | the catalog size's name, cpus (1 to 1024), memory in MiB (1 to 16777216) and arch |
| `disk_gb` | 1 to 65536 |
| `image` | what the create asked for, and what it resolved to |
| `provider` | a word for display (which cloud, say); it says nothing a server acts on |
| `created_at` | when the service reserved the machine |
| `started_at` | when it last reached `running`, or `null` if it never has |
| `stopped_at` | when it last reached `stopped`, or `null` if it has not stopped since it last started |
| `lease` | `expires_at` and `max_expires_at` (section "Lease rules"), `created_at ≤ expires_at ≤ max_expires_at`; `extendable`, whether an extend by one hour would be accepted now (below); `presets_h`, 0 to 16 hours (1 to 720 each) a person may extend by (empty for a client's machine) |
| `access` | an ordered list of options (section "Access") |
| `labels` | the server's working labels, stored and returned verbatim (section "Labels") |
| `spec` | the create's, verbatim |

No member names a cloud project, a zone, a network or an account.

**`extendable` (normative)** is `true` exactly when every check of
section "Extend" but the body's own would pass now for one hour: the
machine is a person's; the policy lets people extend; the person is a live
person on the list who has not opted this server out; the machine is not
being deleted; the service's clock is before `expires_at`; and `expires_at`
plus one hour is not after `max_expires_at`. Otherwise it is `false`: a
client's machine, an inactive person's, one at its cap, one past its end,
one being deleted. It is computed when the machine is answered, so the same
machine may read differently later.

### Access

`access` is an **ordered list of options**, each an object with a `kind`.
A server takes the first option whose kind it supports; it skips every
option of a kind it does not know, whatever it holds, and ignores a member
it does not know in an option it takes. The list is empty until the
machine is `running` and its host key is known, and while it is not
running. A server that supports none of the options marks the shed
unreachable.

Version 1 defines one kind, **`ssh`**: direct SSH to `host`:`port` as
`user`, with `host_key` the attested host key.

| Member | Rule |
|---|---|
| `host` | an IPv4 or IPv6 literal in its canonical text (no name, no zone, no IPv4-mapped IPv6 form), never loopback, link-local, unspecified or multicast (a server running with test drivers allows those) |
| `port` | 1 to 65535 |
| `user` | exactly `shed-link` |
| `host_key` | one `ssh-ed25519` public key line in its canonical form: the type, one space, the base64 key, and nothing else |

A later version adds kinds (a jump host, a proxy, a guest that dials out)
without changing `ssh`.

### The host key

**The service stores the first host key the guest publishes after the
machine is created, and never replaces it; `host_key` is always that
stored key.** A key the guest publishes later that differs is not used (the
service records the mismatch for its operator). A server still pins the
key at its first contact, compares it with the key the guest's sshd
presents, and refuses a change.

### Labels

`labels` are the server's working labels: at most 32, each key and value
under the grammar. **Reserved, never written and never in a reply:**
`shed_schema`, `shed_env`, `shed_id`, `shed_name`, `shed_owner`,
`shed_user`, `shed_user_origin` (their facts are members of the machine).

## Validation

A server refuses a machine object, wherever it appears (a reply's
`machine`, a list's `machines`), when it breaks one of these rules, and
uses nothing of it. Each rule has a name; `fixtures/machines/` holds a
refused object for each.

| Rule | Refused |
|---|---|
| `not_json` | not one strict JSON object (Conventions, "Documents") |
| `missing_member` | a member this document requires is absent |
| `wrong_type` | a member of the wrong type, `null` included where it is not allowed |
| `shed_id_mismatch` | `shed_id` outside its grammar, not the path's (or the operation's machine), or listed twice |
| `server_mismatch` | `server` is not the server's own name |
| `name_invalid` | `name` outside its grammar |
| `owner_invalid` | not exactly one of `user` and `client` a string, or outside their grammar |
| `state_invalid` | `state` not one of the six |
| `word_invalid` | `provider`, a size's `name` or `arch`, or an image's `requested` or `resolved` outside the word grammar |
| `number_out_of_range` | `cpus`, `memory_mb`, `disk_gb` or a preset out of its range |
| `time_invalid` | a time not RFC 3339 UTC with `Z` in whole seconds, or not a real instant |
| `lease_invalid` | the lease's times out of order, or more than 16 presets |
| `labels_invalid` | more than 32 labels, a key or value outside its grammar, or a reserved label |
| `spec_too_long` | `spec` over 4096 bytes |
| `access_host_invalid` | an `ssh` option's `host` that is not an IP literal in its canonical text |
| `access_host_refused` | an `ssh` option's `host` that is loopback, link-local, unspecified or multicast, to a server without test drivers |
| `access_port_invalid` | `port` outside 1 to 65535 |
| `access_user_invalid` | `user` other than `shed-link` |
| `access_host_key_invalid` | `host_key` not one canonical `ssh-ed25519` line |

A server applies its own operator's rules on top (for shed,
`remote.access.allow`, the address ranges a host may be in).

Every other reply is held to its shape the same way, member by member,
with every member required, none `null` where this document does not allow
it, and member names compared exactly: the catalog (section "Catalog", its
counts and bounds), an operation (exactly one of `machine` and `error`
when done), the list, usage (every count and `max` 0 to 1000000, and
`running + stopped ≤ machines`), and ingress (0 to 64 canonical sources,
none twice). A reply that breaks any of these is an invalid reply.

## Lease rules

- A person's machine: `expires_at = created_at + initial_lease_s` and
  `max_expires_at = created_at + max_age_s`, from the policy in force at
  create. A client's: `expires_at = max_expires_at = created_at +
  lease_s`, refused `LEASE_TOO_LONG` above `client.max_lease_s`.
- An extend moves `expires_at` by `by_s`, never past `max_expires_at`, and
  never for a client's machine. Nothing else moves either time: stop and
  start do not.
- **At `expires_at` the service deletes the machine, whatever its state**,
  running or stopped, with its disk, and lists the deletion with cause
  `lease_ended`. Any work in progress on it ends with error `LEASE_ENDED`.
- Start and extend are refused at or after `expires_at`, also before the
  service has deleted the machine.
- A person who leaves the list, or opts the server out, gets no new
  machine, start or extend; their machines keep their leases.

## Limits

| What | Limit |
|---|---|
| Request body | 64 KiB (`413`) |
| Reply body | 64 KiB; the list 4 MiB. A server reads one byte past the limit, to learn that a body is larger, and no further |
| Error body, as a server reads it | 4 KiB |
| Machines in one list | 500 |
| Labels | 32 |
| `spec` | 4096 bytes |
| `authorized_keys` | 1 to 4 lines of at most 8 KiB |
| `access_kinds` in a create | 1 to 8 |
| Ingress sources | 64 |
| `by_s` | whole hours, 3600 to 2592000 |
| Usage counts, `person_max`, `org_max` | 0 to 1000000 |
| Policy durations | 1 second to one year (`warn_s` may be 0) |
| Catalog sizes, aliases, access kinds | 1 to 64, 0 to 64, 1 to 8 |
| A service's answer | 10 seconds (one cloud call within it, 8 seconds) |
| A server's wait for one call | 12 seconds |
| A kept answer, a finished operation, a listed deletion | 24 hours |
| `Retry-After`, as a server honours it | 30 seconds |

A service limits the rate of compute calls as it sees fit (polls, reads,
lists and usage together), and answers `429` with `Retry-After`.

## The fixtures

`fixtures/` holds what both sides test against. Every file in `v1/` but the
manifest is listed with its SHA-256 in `MANIFEST.sha256` (`<hex>  <path>`
lines, sorted by path), so a copy can be checked whole.

- `world.json`: the compute world (below).
- `exchanges/*.json`: what an implementation of the service answers.
- `machines/*.json`: machine objects a server accepts, and one it refuses
  for each rule of section "Validation".
- `replies/*.json`: replies a server must classify.
- `mapping.json`: each code's status and meaning (section "Status and error
  matrix").

### The world

Every exchange runs against a fresh copy of the directory contract's world
(version 1: its `fixtures/world.json` and `fixtures/test-keys/`, its
scenario time) and of `world.json` here, which gives the service's
catalog, its policy and where machines are reached:

- `provider`: the word machines carry (`ref`);
- `catalog`: the sizes `standard` (2 CPUs, 8192 MiB, amd64, 50 GB) and
  `large` (8, 32768, amd64, 100 GB), the default size `standard`, the image
  `default` and the aliases `base` and `cloud` with what each resolves to,
  and the access kind `ssh`;
- `policy`: a person's first lease 86400 s, cap 432000 s, extends allowed
  with the presets 6, 12, 24 and 48; a client's maximum lease 432000 s;
  `warn_s` 7200; 4 machines a person, 10 the organization; the sizes by
  role (a person the directory lists as `admin` may have `standard` and
  `large`, any other `standard`); ingress writable, every clause of
  `exclusive` holding and the rules readable, no source wider than an IPv4
  `/24` or an IPv6 `/48`;
- `access`: every running machine with a known host key is reached at
  `127.0.0.1` port 22.

In the world, **no server may manage compute** and no client is
registered, until a set-up step says so. The world's people are the
directory world's users (`ada`, an admin; `linus` and `turing`), all live.

### Exchanges

Each file is one scenario in the directory contract's exchange language
(its section "Exchanges": the members, `assertion`, `token`, the six
compared headers, the canonical comparison, `then`, and its set-up steps
and placeholders), with these additions and no others.

**Set-up steps** (each an object with one member, as there):

| Step | Makes true |
|---|---|
| `{"compute.allow": "<server name>"}`, `{"compute.deny": "<server name>"}` | the live server with that name may (may not) manage compute: the grant, per enrolment, so a server enrolled again under the name needs it again |
| `{"client.register": ["<server name>", "<client>"]}` | the client is registered for that server |
| `{"policy.set": {<member>: <value>, …}}` | each named member of the policy has that value: `initial_lease_s`, `max_age_s`, `extend_allowed`, `presets_h`, `client_max_lease_s`, `warn_s`, `person_max`, `org_max`, `ingress_writable` (false: ingress is managed elsewhere) |
| `{"ingress.break": "<clause>"}` | the cloud no longer meets one clause of `exclusive`, so the service answers `exclusive: false`: `extra_rule` (a rule admits traffic on another port, or from a source outside `sources`: clause 1), `machine_to_machine` (one machine can reach another: clause 2), `shared_network` (the machines share a network with something else that admits inbound traffic: clause 3); or `unreadable`, the service cannot read its rules (clause 4: a read and a write of ingress answer `503 UNAVAILABLE`) |
| `{"machine.delete": ["<server name>", "<shed id>", "page" \| "external"]}` | that server's machine is deleted outside the server: at the service (`page`) or from the cloud without the service (`external`). It is gone at once, its work in progress ends with error `DELETING`, and the list names the deletion with that cause |
| `{"quota.fill": ["person", "<handle>"]}`, `{"quota.fill": ["org"]}` | the person (the organization) holds, on other servers, running machines enough to reach its limit; they count everywhere and appear in no list of this scenario's servers |
| `{"cloud.fail": ["<call>", "<fault>"]}` | the next call of that kind meets the fault, once: `quota` or `capacity` (create, start: the cloud accepts the work, and its operation ends with error `CLOUD_QUOTA` or `CLOUD_CAPACITY` when it settles), `busy` (start, stop: `409 BUSY`), `lost_reply` (create, start, stop, delete, extend, labels: the service does the work and keeps its answer, and the server hears `503 UNAVAILABLE` instead) |
| `{"cloud.settle": ["<server name>", "<shed id>"]}` | the cloud finishes the work in progress on that server's machine: creating and starting machines run, stopping ones stop, a deleting one is gone (its deletion listed with cause `server`); work that `cloud.fail` marked fails |
| `{"guest.publish_key": ["<server name>", "<shed id>", "<ssh-ed25519 line>"]}` | that server's machine's guest publishes that host key (kept only if it is the first; a different later key is recorded as a mismatch) |
| `{"clock.advance": <seconds>}` | the service's clock moves forward that much, and the service's lease sweep then runs: every machine at or past `expires_at` is deleted |
| `{"person.optout": ["<handle>", "<server name>"]}` | the person has opted that server out |
| `{"limits.exhaust": "compute"}` | the compute limit, for the scenario's server, as the directory limit is |

A step that names a machine names it by its server and its shed id, as
the service keys machines; a pair that names no machine is an error of
the fixture, never a step that does nothing.

The directory contract's `clock.set` moves the clock **without** running
the sweep, so a scenario can stand at a lease's end before the service has
deleted the machine. Between a cloud call and its `cloud.settle`, work is
in progress and its operation is not done.

**Placeholders:**

| Placeholder | Where | Rule |
|---|---|---|
| `{{compute_token}}` | the 200 body of a token request that asked for `compute:manage`, member `access_token` | the directory contract's `{{access_token}}` rule with `scp` = `["compute:manage"]`; bound (again at each such answer) for later requests: `Authorization: Bearer {{compute_token}}` |
| `{{operation_N}}` (N = 1, 2, …) | a reply's member `operation` | an operation id; its first appearance binds it, every later one (in a reply or a request path) must be that value, and two different N are different values |

**`request.body_bytes`**, instead of `body`: the body is that many bytes,
a JSON object `{"pad":"aaa…"}` padded to the length (the provision
contract's addition).

**`response.replay_of`**, beside `body`: the step number of an earlier
step of the scenario (the first step is 0); this response's body must be,
byte for byte, that step's body (a kept answer is the first answer's bytes,
not a new encoding of it).

Everything else the service computes from its clock and the world (the
machine objects, their times and leases, usage, ingress) is exact.

**The exchanges** cover: the token and the grant (`token-compute-*`,
`compute-*`: granted, not allowed (and the assertion then accepted, as it
was not spent), a disabled server, a directory token, a
grant removed, a disabled server holding a token, no bearer, the rate
limit, a storage outage, an unknown route), the catalog; create (a person's
and a client's, disk and labels, an alias, a repeated request id, a
repeated shed id, `SPEC_MISMATCH`, `EXISTS`, each judgement refusal, the
person and organization limits with stopped and clients' machines counted
and with `quota.fill`, the cloud's quota and capacity, a lost reply, bodies
that are not valid, unknown members ignored); start and stop (every state,
`BUSY`, a start the cloud refuses, `DELETING`, `LEASE_ENDED` before the
sweep, `PERSON_INACTIVE` with the machine no longer extendable,
`NOT_FOUND`); delete (running, unknown, while creating, a lost reply);
operations (pending, done, done with an error, a finished delete, expired
at 24 hours, unknown, bound to the machine they were made for); the list
(machines, deletions with each of the four causes, kept 24 hours); labels (set, expect, remove,
`LABEL_TAKEN`, `RESERVED_LABEL`, the grammar and the 32 labels); extend
(accepted, a repeated request id answered byte for byte, an unknown null
member, the cap, a client's machine, the policy off, invalid hours, a lost
reply); usage (a person and the organization, a refused handle); ingress
(read, write, not writable, too wide, not canonical, each clause of
`exclusive` broken, rules that cannot be read); the lease's end (a running and a stopped
machine deleted, an extended machine kept to its new end); the host key
kept once; and servers (another server's machine, a server enrolled again
under its name).

### Machines

Each file in `machines/` is one machine object and how a server must
judge it:

```json
{
  "name": "<file name without .json>",
  "description": "<what it shows>",
  "context": {"shed_id": "<the shed asked for>", "server": "srv-a", "test_drivers": false},
  "machine": { },
  "expect": {"result": "refused", "rule": "<rule>"}
}
```

`machine` is the object as a JSON value, or `machine_text` its exact text
(for a member named twice, data after it, an escape). `context` is what
the server judges it against: the shed id the reply is about, its own
name, and whether it runs with test drivers. `expect` is `{"result":
"accepted", "decoded": { }}` or `{"result": "refused", "rule": "<rule>"}`;
a refused object breaks exactly that rule. `decoded` is what a server reads
of an accepted object: every member of the machine object but `access`,
as the object holds them (a size's and an image's and the lease's own
members only); `access`, the `ssh` options in the service's order, each
with exactly `host`, `port`, `user` and `host_key`; and `ignored_access`,
how many options of other kinds were skipped. No other member is part of this
language.

### Replies

Each file in `replies/` is the reply, or the replies, to one call, and
what a server makes of them:

```json
{
  "name": "<file name without .json>",
  "description": "<what it shows>",
  "call": "get",
  "replies": [{"status": 200, "headers": { }, "body": { }}],
  "expect": {"result": "failed", "kind": "ErrInvalidReply"}
}
```

- Every reply is to a server named `srv-a` without test drivers, about the
  shed `q6c3g4ymb5m2dkxwe7kyz5hhzu` (a list is about no one shed).
- `call` is one of `catalog`, `create`, `start`, `stop`, `delete`,
  `operation`, `get`, `list`, `labels`, `extend`, `usage`, `ingress_get`,
  `ingress_put`. `create`, `start`, `stop`, `delete`, `labels`, `extend`
  and `ingress_put` change something.
- `replies` are in order: a second only after a 401, to the call sent
  again with a new token. A reply is `{status, headers, <body>}` with at
  most one body: `body` (JSON), `text` (sent byte for byte), or `generate`:
  `oversize` (a machine reply of 64 KiB and one byte), `oversize_list` (a
  list of 4 MiB and one byte), `list_501` (a complete list of 501 valid
  machines), `truncated` (the `Content-Length` of a valid machine reply,
  half of it sent, then the connection closed), `echo_bearer` (a valid
  machine whose `spec` is the bearer presented), `echo_bearer_escaped` (the
  same, with the bearer's first character written as a `\u` escape, so the
  bearer is in what the body decodes to but not in its bytes),
  `oversize_error` (a 409 error body over 4 KiB naming `BUSY`),
  `oversize_error_echo_bearer` (a 409 naming `BUSY` whose message holds the
  bearer in its first bytes, padded past 4 KiB). In `body` and `text`,
  `{{bearer}}` and `{{assertion}}` stand for the bearer token that reply's
  attempt presented and the client assertion that token was obtained with
  (a retry after a 401 presents a new pair), and `{{bearer_escaped}}` and
  `{{assertion_escaped}}` for the same with the first character written as
  a `\u` escape (in a JSON string, the same text once decoded). Two replies have no status:
  `{"silent": true}` (nothing within 12 seconds) and `{"unreachable":
  true}` (the connection is refused).
- `expect.result` is `ok` (the answer), `refused` (with `code`, the code
  the server acts on, and `kind`), `unknown` (a call that changes something
  with no answer it can read, with `kind`), `failed` (a read that failed,
  or a 413, with `kind`), or `retry` (a 429, with `retry_after_s`). For an
  operation's `ok`, `operation` says what it held: `pending`, `machine`,
  `gone`, or the error code it ended with (and `kind`, that code's). `kind`
  is the `provider` error kind `mapping.json` gives.
- No other member is part of this language.

`replies/code-*` holds each code at its status.

### mapping.json

```json
{
  "codes": {"QUOTA_PERSON": {"status": 409, "provider": "ErrComputeLimit", "refusal": true,
                             "shed_api": {"status": 503, "code": "COMPUTE_LIMIT_REACHED", "reason": "person"}}},
  "status_defaults": {"403": "COMPUTE_DENIED", "404": "NOT_FOUND", "409": "INVALID", "422": "INVALID"},
  "failures": {"unavailable": { }, "invalid_reply": { }, "payload_too_large": { }}
}
```

For each code: its `status`; `provider`, the error kind of shed's remote
backend it means (`ErrNotFound`, `ErrExists`, `ErrBusy`, `ErrInvalid`,
`ErrLabelTaken`, `ErrQuota`, `ErrCapacity`, and, for this contract,
`ErrSpecMismatch`, `ErrComputeLimit`, `ErrPolicy`, `ErrLease`,
`ErrDeleting`, `ErrIngress`, `ErrDenied`, `ErrUnavailable`,
`ErrInvalidReply`); `refusal`, whether it is a refusal a client may retry
later (shed's `provider.IsRefusal`: the cloud's quota and capacity, and
the person and organization limits); and `shed_api`, what shed's API
answers when it reaches a caller (its status, code and `reason`). A
service needs only the codes and statuses; the rest is how shed holds
itself to them.
