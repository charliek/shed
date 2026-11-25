# Credentials from the account service

On a server that takes its users from an [account service](user-directory.md), the service can also be where those users' credentials come from. When one of them creates a shed, or runs `shed provision`, the server asks the account service, not a machine of theirs, for the files and environment variables the shed should hold. The person needs no desktop app running and no machine bound.

This page is for the admin of such a server and for the people on it. The wire format is the hosted source in [The provision contract](../reference/provision-contract.md) (section 3b); what the server does with the answer is the same as for any source, in [Credential delivery](../reference/delivery.md).

## What the server does

| Server | The shed's user | Who answers |
|---|---|---|
| not a directory server | anyone | the user's pinned machine |
| a directory server | a local admin (named in the server's config) | the user's pinned machine |
| a directory server | any other user | the account service, and nothing else |

For a directory user:

1. The server signs in to the service for the scope `deliveries:request`, with the key it enrolled with (a separate token request from the one that pulls the user list).
2. It makes one call, `POST <issuer>/v1/deliveries`, with the request document: the shed's name, backend and repository, the person's name, and a deadline.
3. The service answers with the items for that person, or says it has none. The server checks the answer with the same rules as any source and writes the items into the shed.

There is no fallback. If the service has no source for the person here, delivery is `not_applicable`. If it is unreachable, or refuses, the delivery fails with a reason ([below](#when-the-service-does-not-deliver)). A pinned machine is never tried for a directory user, and none can be pinned.

## What an admin sets up

Nothing new in the server's configuration. A server with `auth.directory` set and enrolled as in the [User Directory](user-directory.md) guide asks the service for deliveries automatically. There is no setting to turn it on or off and no second enrolment.

Two things to know:

- **Whether the service delivers is the service's choice.** It decides which servers may ask and for which people. A server the service does not allow to ask is treated as `not_applicable`; the server remembers that for five minutes (`shed users refresh` and a new enrolment clear it) and `shed source status` says so.
- **Upgrading changes who is asked.** From the first start of a server with this behaviour, a pinned machine no longer delivers to directory users, and their bindings on that server are removed (the server logs each). Put the values into the account service first, then upgrade the servers and the desktop apps together.

## What a person does

At the account service, in whatever way it offers, a person stores what their sheds should hold (an SSH key, a GitHub token, API keys, an envsecrets login, named files) and may limit it, for example to certain servers. Shed takes no part in that and has no command for it.

Then, on the server, nothing different: `shed create` and `shed provision <name>` deliver as before. `shed provision <name> --status` shows who answered:

```text
Last:      2026-11-25T10:00:00Z (create) accepted from the account service
```

`shed source status` shows that the service is the source, and `shed source use` and `shed source unbind` refuse, since there is no pin to move:

```text
$ shed source status
Credential source for you on my-server: the account service (https://accounts.example.test); no machine answers here.
```

Use the account service for what to store. Everything stored there is usable by a shed, so store a key made for sheds and a fine-grained, expiring token, not a personal one. See [Security](../reference/security.md#a-hosted-credential-source).

## When the service does not deliver

A delivery that cannot finish prints the usual warning and records a reason; the shed starts regardless.

| What you see | Meaning | What to do |
|---|---|---|
| `delivery: not_applicable`, no warning | the service has no source for you on this server, does not offer delivery, or does not let this server ask | store values at the service, or ask its admin to allow the server; then `shed provision <name>` (a refusal of the server is remembered for five minutes) |
| `source_offline` | the service could not be reached, or was busy | `shed provision <name>` when it is back |
| `source_timeout` | no answer within `delivery.timeout` | `shed provision <name>` |
| `source_disconnected` | the reply ended before it was complete | `shed provision <name>` |
| `source_refused` | the service refused this server's sign-in or its request | the server's enrolment may be disabled or stale; see [Enrolling again](user-directory.md#enrolling-again) |
| `answer_refused` | the reply broke a contract rule, or held the token the server presented | the service is misbehaving; tell its admin |

The server never shows the service's own error text; the reason is all it keeps. See [Credential delivery](../reference/delivery.md#failure-and-the-warning).

## A name that moves between the sets

A shed is owned by a name. If the name moves between the server's local admins and the account service, it is a different person. Each shed records at create which it was, and a delivery after the move is refused with `SHED_ORIGIN_CHANGED` and nothing is requested. Create a new shed. Sheds that predate this behaviour are stamped once, from the server's first start with it, in `<state_dir>/delivery/origin-migration.json`; do not edit that file. See [Credential delivery](../reference/delivery.md#where-a-sheds-user-came-from).

## Trying it

The repository's [reference server](user-directory.md#try-it-with-the-reference-server) speaks the hosted source for tests: a `deliveries.json` control gives each user canned items, no source, or a fault. It is not a service to run for real.
