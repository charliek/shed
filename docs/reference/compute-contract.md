# Compute Contract

The **compute contract** is what a shed-server running the [remote backend](remote-backend.md) and an **account service** that supplies its compute promise each other. The service holds the cloud credential, decides which sizes and images exist, who may have how many machines and for how long, makes every cloud call and deletes each machine when its lease ends. The server asks for a machine for each cloud shed, follows the work to its end, and reaches the machine over SSH where the service says. The contract is versioned and public, tested from both sides, and any implementation of the service side works with shed, whatever cloud (or Kubernetes cluster, or hypervisor) is behind it.

The contract lives in this repository, in [`tests/compute-contract/`](https://github.com/charliek/shed/tree/main/tests/compute-contract), and **that folder is the source of truth**. This page is a reader's guide: it says what the contract is for and how to read it, and restates none of it.

| Path | What it is |
|------|------------|
| [`v1/CONTRACT.md`](https://github.com/charliek/shed/blob/main/tests/compute-contract/v1/CONTRACT.md) | Version 1. Normative and complete but for the token. |
| `v1/MANIFEST.sha256` | Every file of `v1/` but itself, with its SHA-256. A changed fixture is a contract change. |
| `v1/fixtures/exchanges/` | What a service answers, call by call, from a recorded starting world. |
| `v1/fixtures/machines/` | Valid machine objects, and a refused object for each validation rule. |
| `v1/fixtures/replies/` | What a server must classify: every row of the status matrix, an unlisted code, a truncated body, silence past the deadline, an incomplete list, an echoed bearer. |
| `v1/fixtures/mapping.json` | For each code: its status, what it means to a server, and the shed API code a client sees. |

The folder changes only by a reviewed pull request, and a breaking change replaces `v1/` in place (the server and the service move together); growth that is not breaking is a member either side ignores, or an access kind a server skips.

## What it is for

A company or an individual writes an integration for their own environment (a custom Kubernetes cluster, DigitalOcean, Google Cloud, AWS) with their own rules, and exposes it as an account service that also supplies the server's users and credentials. shed stays open and does not need to know where compute comes from. The server never sees a cloud credential, a project, a zone, a network or an address other than the machine's own access host, and nothing the service sends names more.

The service is the **same service** as the [directory contract](directory-contract.md)'s: the server enrols once, signs in with the same client credentials, and asks for a second token with the scope `compute:manage`. A service lets a server manage compute only when its operator has granted it (off by default).

## The calls

| Call | What it does |
|---|---|
| `GET /v1/compute/catalog` | The sizes (CPUs, memory, architecture, default disk), the default size, the image names, the access kinds, the lease and limit policy, and whether the ingress is writable. Read at start and every pass; cached. |
| `PUT /v1/compute/machines/{shed_id}` | Create a machine for a person or a client. Judgement refusals (limits, size, image, lease, person, client) answer here before any cloud call. `202` with an operation, or `200` when the same create already exists. |
| `POST …/{shed_id}/start`, `…/stop` | Start or stop. `202` with an operation, `200` when already there. |
| `DELETE …/{shed_id}` | Delete the machine and its disk. `202` with an operation, `204` when gone. |
| `GET /v1/compute/operations/{op}` | Follow an operation: not done, done with the machine, done with a deleted machine, or done with an error code. |
| `GET …/{shed_id}` | One machine, or `404`. |
| `GET /v1/compute/machines` | Every live machine of the calling server (at most 500) and its deletions of the last 24 hours. Never partial: a service that cannot read everything answers `503`. |
| `POST …/{shed_id}/labels` | The server's working labels, compare-and-set in one atomic write. |
| `POST …/{shed_id}/extend` | Move a lease later by whole hours, never past its cap. |
| `GET /v1/compute/usage[?user=]` | A person's and the organization's machine counts against their limits. |
| `GET` and `PUT /v1/compute/ingress` | The sources that may reach the machines' SSH port, whether that is exclusive, and (when writable) set them. |

Every call carries `Authorization: Bearer` from a `compute:manage` token. Requests are at most 64 KiB and replies at most 64 KiB (the list, 4 MiB). A service answers within **10 seconds**, with its cloud work for the call bounded at 8 seconds in total; a server waits 12.

## Ownership

Every machine and operation belongs to the **server's enrolled name**. A server never sees another name's machines: a read is `404`, a delete `204`. A machine belongs to one person (`user`) or one registered client (`client`), fixed at create; the service's list of people is the directory's. The same name enrolled again (how a server's key rotates) has the same machines.

## Idempotency

Every call that changes something carries a `request_id` (a UUID) which the server stores with its job. A repeated `request_id` returns the first answer, kept 24 hours; a refusal is not kept. A create is also idempotent by `shed_id`: the same body returns the existing machine and its operation (also while it is still creating), a different body is `409 SPEC_MISMATCH`. This is what lets a server answer "the outcome is unknown" correctly: a create whose reply was lost is sent again with the same ids and makes one machine. Operations are the server's own and kept 24 hours after they finish; `404` on one means "unknown or expired: read the machine".

## Operations

A call that needs cloud work returns `202` and an operation, and the server polls it at 2 seconds, then 5. A service that cannot finish its cloud work inside its deadline accepts the request and reports the outcome through the operation: `done: true` with the machine (or `null`, for a delete), or with an error code. The server follows each operation to its end and survives its own restart in the middle: the job's record holds the operation and the request id, so a restarted server reads the machine first and then waits for or replays the call.

## Leases

A person's `expires_at` is the creation time plus the service's `initial_lease_s`, and its cap `max_expires_at` is the creation time plus `max_age_s`. A client's lease is its own, and its cap. An extend adds `by_s` (whole hours) and is refused past the cap. **At `expires_at` the service deletes the machine, running or stopped**; stop and start change neither time, and a start or extend at or after the end is refused (`LEASE_ENDED`). The server runs no timer: it learns of the deletion from the list's `deleted` entries (a cause: `lease_ended`, `page`, `external` or `server`). The catalog's `warn_s` says how long before the end a person is warned. See [Remote backend › Leases](remote-backend.md#leases).

## Refusals

A refusal is a `4xx` or a `409`/`422` with a body `{"error":{"code":"<CODE>"}}`; the server reads the code only and drops the message (never logged, stored or shown). The contract lists each code and its status, and `mapping.json` gives what a client sees: for example `QUOTA_PERSON` is `503 COMPUTE_LIMIT_REACHED` with reason `person`, `IMAGE_UNKNOWN` is `422 COMPUTE_POLICY_REFUSED` with reason `image`, `DELETING` is `409 COMPUTE_DELETING`. A `5xx`, a timeout or a body cut short is "outcome unknown" for a call that changes something, and "unavailable" for a read. A reply that breaks the contract's validation, or that echoes the bearer token or the client assertion, is refused whole.

## Machine access and the host key

A machine's `access` is an ordered list of options. v1 defines `ssh`: direct SSH to `host:port` as the user `shed-link`, with the attested `host_key`. The list is empty until the machine runs and its key is known. The service stores the **first** key the guest publishes and never replaces it. A server takes the first kind it supports and ignores kinds it does not know, so a later version adds a jump host or a proxy without changing `ssh`. No member names a project, zone, network or account.

## How to write a service

1. **Read `v1/CONTRACT.md` start to finish.** It is complete: the status and error matrix, the codes and their statuses, the validation rules, the lease rules, the `exclusive` rule and the limits.
2. **Implement the directory side first** (enrolment, the token, the user list): [Directory Contract](directory-contract.md). Add the `compute:manage` scope and the per-server grant.
3. **Implement the calls against `fixtures/exchanges/`.** Each exchange is a request, the state it starts from and the answer. A conformant service answers every exchange; shed's own tests run them against its reference service.
4. **Make an image the guest contract allows.** The machine must run what [Guest Contract](guest-contract.md) names (the link user, the agent, the layout file, a host key made on first boot). How the shed's name, id and keys reach the guest, and how its host key is read back, is each adapter's own business.
5. **Answer `exclusive` honestly.** `true` only when you read every rule you need to say so; an incomplete read is a `503`, never `true`.
6. **Run shed's reference service for the other side.** The in-tree reference service (`make directory-ref` builds it into `bin/test/`; `make dev-compute-ref-up` runs it) is a worked, in-memory implementation of every rule. See [Remote Backend Development](../development/remote-backend.md).

If you copy `v1/` into another repository, copy it whole, with its manifest, and treat a changed file as a contract change.
