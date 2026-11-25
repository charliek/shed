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

## Transport summary

A source is a **machine source**, a program on one of the person's machines that holds a stream open to the server (below), or a **hosted source**, the account service, which the server calls once per request (the contract's section 3b).

| Route | Purpose |
|---|---|
| `GET /api/delivery/requests` | The source's server-sent event stream. The `200` carries `X-Shed-Delivery-Stream`. Each `request` event is one request. A keepalive comment arrives every 15 s. |
| `POST /api/delivery/requests/{id}` | The answer, with the stream's id in `X-Shed-Delivery-Stream`. `204` when accepted. |

Both routes need the credentials scope and the machine headers `X-Shed-Machine-Id` and `X-Shed-Machine-Name`, and both are refused for service clients and script keys. Only the user's bound machine is served. A request is bound to the user, the binding, the machine, the certificate, the one stream and the shed's id; an answer from anything else is `not_stream_owner`.

The contract's own error codes are lower case: `unknown_request`, `already_answered`, `deadline_passed`, `not_stream_owner` and `answer_refused` (with the broken `rule`). When a stream ends, its pending requests fail at once with `source_disconnected` and are not replayed.

See [Credential delivery](delivery.md) for what the server does with an accepted answer, and [Security](security.md) for the trade this makes.
