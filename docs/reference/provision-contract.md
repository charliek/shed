# The provision contract

The provision contract is what shed-server and a person's credential source promise each other: the request the server sends, the answer a source returns, the rules every item must meet, the transport, and the record the server keeps. It is a versioned folder in this repository, `tests/provision-contract/v2/`, and that folder is the **source of truth**. This page is a pointer and a summary, not a copy.

| File | Holds |
|---|---|
| `CONTRACT.md` | the conventions, the documents, the rules, the transport and the record |
| `MANIFEST.sha256` | a checksum of every file in the folder |
| fixtures | answers (accept and refuse), requests, records, stream transcripts, and the hosted source's exchanges and replies |

## Rules for changing it

- A file changes only by a reviewed pull request. A changed fixture is a contract change.
- A breaking change is a new version, which replaces the old one: version 2 replaced version 1. The contract has no optional growth: an unknown member refuses a document.
- `UPDATE_CONTRACT=1 go test ./internal/delivery/ -run Manifest` rewrites the manifest after an intended change.

## A second implementation

A source or a server written elsewhere copies `v2/` whole from a shed commit and holds itself to every fixture. The Go tests in `internal/delivery` and the Rust test in `shed-provision` read the same folder, which is how the two sides stay in step. `make provision-ref` builds the in-tree reference source into `bin/test/` (in no release): it connects as a machine, holds the request stream and answers from canned items, and can stay silent, answer late or twice, or send any refuse fixture.

## Version 2 at a glance

Every request and answer carries `"version": 2`. Version 2 adds the hosted source, the reasons `create`, `provision`, `start` and `refresh` (shed-server sends the first two), the problem codes `timed_out` and `expired`, and a `kind` in the record's `last.source`. The contract's text is the authority; this table is the part a reader usually wants, the failure reasons a record may hold and what each means on the two transports:

| Reason | Machine source | Hosted source |
|---|---|---|
| `source_offline` | the bound machine was not connected | the service could not be reached, or answered `429` or `5xx` |
| `source_timeout` | no answer by the deadline | the same, and `410 DEADLINE_PASSED` |
| `source_disconnected` | the stream ended before an answer | the reply ended before it was complete |
| `source_refused` | never | the service refused the server or its request (section 3b) |
| `answer_refused` | the answer broke a rule | the same; also a `200` whose body is not an answer, is over 6 MiB (`body_too_large`), or contains the token or assertion the server presented (rule `echoes_credential`) |

`last.source` is `{kind, machine_id, machine_name}`. `kind` is `machine` or `service` and is always present; the two machine members are present exactly when `kind` is `machine`. A record may name a `service` source on `source_refused`; `not_applicable` also covers a service with no source for this person on this server, and a service that does not let this server ask. A version 2 server reads a record written by v0.11 (no `kind`) as a machine source.

## The two transports

| | Machine source | Hosted source |
|---|---|---|
| What it is | a program on the person's machine | the account service |
| How the server reaches it | the source holds a stream open to the server | the server makes one HTTPS call per request |
| Section | 3a | 3b |
| Credentials | the machine's certificate and id | a bearer token for the scope `deliveries:request`, obtained as the [directory contract](directory-contract.md) describes, by a separate token request |
| Call | `GET /api/delivery/requests`, `POST /api/delivery/requests/{id}` | `POST <issuer>/v1/deliveries` |
| Deadline | the request's `deadline` | the same, covering the token request, one retry after a `401`, the call and reading the reply |

The server compares only the `code` of a hosted error. The text of an error, a token error description, a redirect target or an unparseable body is classified and dropped: it reaches no log, record, audit row or API response.

## Transport summary

A source is a **machine source**, a program on one of the person's machines that holds a stream open to the server (below), or a **hosted source**, the account service, which the server calls once per request (the contract's section 3b).

| Route | Purpose |
|---|---|
| `GET /api/delivery/requests` | The source's server-sent event stream. The `200` carries `X-Shed-Delivery-Stream`. Each `request` event is one request. A keepalive comment arrives every 15 s. |
| `POST /api/delivery/requests/{id}` | The answer, with the stream's id in `X-Shed-Delivery-Stream`. `204` when accepted. |

These two routes are the machine source's. Both need the credentials scope and the machine headers `X-Shed-Machine-Id` and `X-Shed-Machine-Name`, and both are refused for service clients and script keys. Only the user's bound machine is served. A request is bound to the user, the binding, the machine, the certificate, the one stream and the shed's id; an answer from anything else is `not_stream_owner`.

The contract's own error codes are lower case: `unknown_request`, `already_answered`, `deadline_passed`, `not_stream_owner` and `answer_refused` (with the broken `rule`). When a stream ends, its pending requests fail at once with `source_disconnected` and are not replayed.

See [Credential delivery](delivery.md) for what the server does with an accepted answer, and [Security](security.md) for the trade this makes.
