# Credential Source

A shed server asks **one** of a user's machines for what that user's sheds should hold: the
user's **credential source** on that server. Every other machine of the same user is refused
and keeps retrying. The same pin decides which machine may answer for the shed [message
bus](../reference/extensions.md), if you run an extension on it. On a server with no user
list there is one implicit user, so one machine answers for every shed on it.

What the source is asked for is [credential delivery](../reference/delivery.md): the files
and environment variables a new shed holds (an SSH key, a GitHub token, API keys, an
envsecrets login), chosen by the [providers](../guides/credentials-in-a-shed.md) you turn
on. The source answers without a prompt. The pin is therefore the line between a machine
that can draw those items and one that cannot: **the pin is which machine the server asks
for a shed user's delivery.**

This page covers how a machine identifies itself, how the pin is set, what the other
machines show, and how to move it. The server side of the same rules is in
[Security](../reference/security.md); the commands are in the [CLI reference](../reference/cli.md).

## Users the account service answers for

On a server that takes its users from an [account service](../guides/user-directory.md), a user the service lists has **no binding**: the account service is their credential source, and no machine answers for them there. Local admins, named in the server's own config, keep a pin as described on this page.

- At start, a directory server removes every binding whose user is not a local admin, and logs each removal. Nothing recreates one: a first connection from such a user's machine is not bound, and every route that would bind, move or clear a pin answers `409 SOURCE_IS_SERVICE`. That covers `shed source use` and `unbind`, the bus's listener routes and the delivery stream and answer routes.
- The source routes judge the **target** user. An admin's `?user=` names the user they act on, so an admin asking about a directory user gets the service document, not their own.
- `GET /api/sources` answers `machines: []` and a service entry in the delivery slot; `shed source status` says the account service answers. See [the API](../reference/api.md#get-apisources).
- A name that moves into the local-admin set later never meets a pin made while it was a directory user, because none was made.

See [Credentials from the account service](../guides/credentials-from-the-account-service.md).

## Machine identity

Each machine that runs the shed desktop app keeps an identity in one file. One machine is one
app, so one identity; a machine without the app has no identity and cannot be a source:

| Platform | Path |
|---|---|
| macOS | `~/Library/Application Support/shed/broker/machine.json` |
| Linux | `$XDG_DATA_HOME/shed/broker/machine.json`, else `~/.local/share/shed/broker/machine.json` |

The directory is still named `broker` so that an identity and certificate store made by an
earlier version are used as they are. `SHED_BROKER_STATE_DIR` replaces the `shed` directory
above. An app started with a different `SHED_BROKER_STATE_DIR` is a different machine.

```json
{"schema":1,"id":"0f3c9a2e7b1d4c6a8e5f2b9d1c7a3e60","name":"laptop","created_at":"2026-11-16T09:12:03Z"}
```

| Field | Rule |
|---|---|
| `id` | 32 lowercase hexadecimal characters, random, created once. This is what a server matches on. |
| `name` | What people see and type. Taken once, when the file is created, from the first label of the hostname (`laptop.local` becomes `laptop`; an IP address is kept whole); `machine-<first 8 characters of the id>` when there is no usable hostname. Renaming the host later does not rename the machine, and a name edited by hand in the file is used. 1 to 63 bytes of UTF-8, no control characters. |

The file is mode `0600` in a `0700` directory. It is loaded once when the app starts, before
it connects anywhere.

What the app does with the file it finds:

| On disk | Result |
|---|---|
| Missing | Created. |
| Valid | Used as is; never rewritten. |
| A valid `id`, anything else wrong | The id is kept; the other fields are repaired and written back (a warning is logged). |
| Written by a newer version (`schema` above 1) | Never rewritten, moved or replaced. Its id is used if it can be read; otherwise the app runs without an identity. |
| No readable `id` (not JSON, wrong type, wrong format) | Copied to `machine.json.corrupt` and replaced by a new identity. A warning is logged: servers will see a new machine, which has to be chosen again. |
| Cannot be read or created (permissions, I/O) | The app keeps running **without** an identity and reports why. A pinning server refuses such a machine (below). |

!!! warning "A machine restored from another machine's backup"
    The id is never derived from the hardware, so a cloned disk or a restored backup is the
    one way two machines end up with the same id. On the copy, quit the app, delete
    `machine.json`, and start it again: it creates a new identity. Two machines sharing an
    id look like one machine to a server, and the second to connect is refused as a
    duplicate.

## How the pin is set

Every delivery stream the app opens, and every answer it sends, carries two headers:

| Header | Value |
|---|---|
| `X-Shed-Machine-Id` | the machine's `id` |
| `X-Shed-Machine-Name` | the machine's `name`, percent-encoded UTF-8 |

The server keeps one pin per user. It is set in one of two ways:

- **Automatically, by the first connecting desktop app.** The first machine to connect for a
  user is bound (the pin records `first-connect` as who bound it). The app opens a stream to
  **every server in your shed config**, so you do not choose a server for it to ask: the
  first of your machines to sign in to a server becomes your source there.
- **On purpose, by moving it.** **Use this machine** in the app's
  [Credentials view](../desktop/credentials.md), or `shed source use` (the app's IPC op is `source.use`).

A pin can also be **cleared** with `shed source unbind` (see [Clearing the
source](#clearing-the-source)); the next machine to connect is then bound, as the first one
was. An operator can discard every user's pin at once by deleting the server's
`source-bindings.json` (below).

After that the server answers each connection as follows:

| Answer | `error.code` | When | What the app does |
|---|---|---|---|
| `200` (stream) | none | the machine is the user's source | serves requests (`connected`) |
| `403` | `SOURCE_NOT_BOUND` | another machine is the source; the body names it in `source.machine_id` and `source.machine_name` | state `not_bound`; retries on its 1 to 30 s backoff without enrolling a new certificate |
| `409` | `SOURCE_DUPLICATE_STREAM` | this machine's id already holds a stream on this server | state `duplicate`; retries |
| `403` | `SOURCE_MACHINE_ID_REQUIRED` | the request has no valid machine headers | retries |
| `429` | `SOURCE_RATE_LIMITED` | too many new or changed machines for this user lately (burst of 8, one more every 15 s); carries `Retry-After` | retries after `Retry-After` (whole seconds, at most a minute), never sooner than its backoff |

Matching is by id only. The server also records each machine's name and the fingerprint of
the key its certificate was issued to, and shows both, but never matches on them. It
remembers up to 32 machines per user, refused ones included, and forgets the least recently
seen past that.

A `duplicate` has two usual causes: a state directory copied to another machine, or a
reconnect that arrives before the server has ended the previous stream. A stream that has
gone dead is ended by the server's write deadline (15 s), after which the next retry
succeeds. No stream ever replaces another.

A delivery answer follows the same rule: the server accepts an answer only from the user's
bound machine, on the stream it asked, and only for a request it sent under the current pin.
An answer from any other machine or connection is refused with `403`.

A server with credential sources lists `source_binding` in the `features` of
`GET /api/info`.

The pin lives on the server, in `source-bindings.json` in the backend's state directory. A
server that starts without that file logs a warning that no bindings exist and that the
first machine to connect for each user becomes its source.

## What the other machines show

A machine that is not the source shows it in these places:

- The app's [Credentials view](../desktop/credentials.md) shows the source's name for each
  server, with an **Answer from here** button. A server older than credential sources has a
  row that says so, with no action.
- `delivery.status` (IPC) reports that server's stream as `not_bound`, or `duplicate` for a
  duplicate.
- From a terminal, `shed source status` names the source and says whether it is connected.

To move the source to the machine you are on, press **Use this machine** or run this
command, which names the machine by its full id:

```text
shed -s devbox source use -- 0f3c9a2e7b1d4c6a8e5f2b9d1c7a3e60
```

The full id cannot be ambiguous: two of a user's machines can share a name (the same
hostname), and `shed source use` refuses a name that matches more than one machine.

## Moving the source

From any machine with the `shed` CLI:

```bash
shed source status                 # which machine is the source, and is it connected
shed source ls                     # every machine that has presented itself for you
shed source use laptop             # make "laptop" the source
shed -s devbox source use -- 0f3c9a2e7b1d4c6a8e5f2b9d1c7a3e60
```

`shed source use` takes a machine's full id, a prefix of its id of at least 8 hex digits, or
its exact name, as `shed source ls` lists them; put `--` before a name that begins with `-`.
A machine appears in `shed source ls` only after its app has opened a stream to that server
at least once. An admin can act on another user's source with `--user`.

From the desktop app: the Credentials view lists each server that supports
credential sources, and **Use this machine** makes the machine the app runs on the source
there. See [Credentials](../desktop/credentials.md).

What a move does:

1. The server records the new pin.
2. It closes the old machine's streams and fails every delivery request it had sent that
   machine. A create that was waiting on the old machine ends as a failed delivery, with the
   fixed warning naming `shed provision`.
3. The new machine starts answering when its app next connects. The desktop app is woken at
   once by the button; `shed source use` from a terminal is picked up on the app's next
   retry, within about 30 seconds.

The routes behind both are `GET /api/sources` and `POST /api/sources/use` with
`{"machine_id": "<32 hex>"}` (an admin may add `"user"`). Both need a control-scope
credential:

| Answer | Code | Meaning |
|---|---|---|
| `200` | | the user's pin and machines (`use` answers the same document, after the move) |
| `400` | `INVALID_REQUEST` | a malformed body or machine id, or an admin naming a user who is not configured |
| `403` | `FORBIDDEN` | a script key, a service client, or a non-admin naming another user |
| `404` | `SOURCE_MACHINE_NOT_FOUND` | no machine with that id has presented itself for the user |
| `500` | `INTERNAL_ERROR` | the move could not be confirmed; check `shed source ls` before retrying |
| `503` | `SOURCE_BUSY` | (read only) the user's records were being changed or saved; carries `Retry-After` |

## Clearing the source

```bash
shed source unbind                 # clear your source on the default server
shed -s devbox source unbind --user bob   # an admin clears another user's
```

What an unbind does:

1. The server forgets your pin **and every machine it remembers for you** (the store keeps no
   machines for a user without a pin), so `shed source ls` is empty until a machine connects
   again.
2. It closes your machines' streams and fails every delivery request it had sent, as a move
   does.
3. Until a machine is bound again, you have no source: a create asks nothing and warns about
   nothing, and its install hook runs as it would with no delivery (an offline pin, by
   contrast, holds the hook back until `shed provision`).
4. **The first of your machines to connect next becomes the source** (`first-connect`). A
   desktop app that is still running reconnects within about 30 seconds, so its machine is
   bound again. To leave no source, quit the app on your machines first; to choose another
   machine, use `shed source use` instead.

With no pin, nothing changes and the command says so. The route is `POST /api/sources/unbind`
with `{}` (an admin may add `"user"`); it answers `200` with `unbound` (false when there was
nothing to clear), the `previous` machine and `forgotten_machines`. It needs a control-scope
credential, refuses a script key and a service client with `403`, and answers `500
INTERNAL_ERROR` when the change could not be saved (check `shed source status` before
retrying). A server older than v0.11 does not have it.

The body may also name `"machine_id"`: the pin is then cleared only while it still names that
machine, and one naming another machine is `409 SOURCE_MOVED` with nothing changed. The
desktop app's **Stop answering from here** sends its own machine's id, so on a server of this
version (v0.12 or newer) it never clears a machine that took over meanwhile; `shed source
unbind` sends none. A v0.11 server refuses the field, and the app falls back to the plain
unbind there, which clears whichever machine is bound by then.

## Who may be or move a source

- A user may move or clear their own source from any of their keys that is not a script
  key; an admin may also move or clear another configured user's.
- A **script key** cannot be a source, move one or clear one: its certificate is refused on a
  stream and on every source route, with `403 FORBIDDEN`.
- A shed owned by a service client gets nothing from any source: no delivery request is made
  for it, and the bus is off.
- With no bound machine, a create asks nothing and warns about nothing.

## Nothing fails over by itself

The pin changes only when someone moves or clears it. If the source is asleep, off the network or not
running the app, the server has no stream for that user: a create ends with the delivery
failed as `source_offline` and the fixed warning, and the shed starts regardless. Another of
the user's machines keeps reporting `not_bound` and does not take over. Move the source with
`shed source use` or the desktop button, then run `shed provision <name>`.

## Upgrading

On its first start with credential sources a server has no pins, so whichever of a user's
machines connects first becomes the source. Start the intended machine first, or check
`shed source status` afterwards and move it.

When a user list is added to a server that had none, the first start with the list hands the
implicit user's pin and remembered machines to the user if exactly one user is configured,
and drops them if several are; each user's first machine to connect is then bound. At every
start the records of users no longer configured are dropped.
