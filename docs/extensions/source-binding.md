# Credential Source

A shed server sends a user's credential requests (the `ssh-agent`, `aws-credentials` and
`docker-credentials` namespaces) to **one** of that user's machines: the user's
**credential source** on that server. Every other machine of the same user is refused and
keeps retrying. On a server with no user list there is one implicit user, so one machine
answers for every shed on it.

This page covers how a machine identifies itself, how the source is chosen, what the other
machines show, and how to move the source. The server side of the same rules is in
[Security](../reference/security.md); the commands are in the [CLI reference](../reference/cli.md).

## Machine identity

Each machine that runs a broker (the `shed-host-agent` daemon, or the desktop app's
embedded broker) has an identity stored in one file:

| Platform | Path |
|---|---|
| macOS | `~/Library/Application Support/shed/host-agent/machine.json` |
| Linux | `$XDG_DATA_HOME/shed/host-agent/machine.json`, else `~/.local/share/shed/host-agent/machine.json` |

`SHED_HOST_AGENT_STATE_DIR` replaces the `shed` directory above. The daemon and the
desktop app read the **same** file, so a machine presents the same identity whichever of
them is brokering. A process started with a different `SHED_HOST_AGENT_STATE_DIR` is a
different machine.

```json
{"schema":1,"id":"0f3c9a2e7b1d4c6a8e5f2b9d1c7a3e60","name":"laptop","created_at":"2026-11-16T09:12:03Z"}
```

| Field | Rule |
|---|---|
| `id` | 32 lowercase hexadecimal characters, random, created once. This is what a server matches on. |
| `name` | What people see and type. Taken once, when the file is created, from the first label of the hostname (`laptop.local` → `laptop`; an IP address is kept whole); `machine-<first 8 characters of the id>` when there is no usable hostname. Renaming the host later does not rename the machine, and a name edited by hand in the file is used. 1 to 63 bytes of UTF-8, no control characters. |

The file is mode `0600` in a `0700` directory. It is loaded once when the broker starts,
before it connects anywhere; `shed-host-agent status` prints it on its `machine:` line and
the desktop app shows it in Preferences.

What the broker does with the file it finds:

| On disk | Result |
|---|---|
| Missing | Created. |
| Valid | Used as is; never rewritten. |
| A valid `id`, anything else wrong | The id is kept; the other fields are repaired and written back (a warning is logged). |
| Written by a newer version (`schema` above 1) | Never rewritten, moved or replaced. Its id is used if it can be read; otherwise the broker runs without an identity. |
| No readable `id` (not JSON, wrong type, wrong format) | Copied to `machine.json.corrupt` and replaced by a new identity. A warning is logged: servers will see a new machine, which has to be chosen again. |
| Cannot be read or created (permissions, I/O) | The broker keeps running **without** an identity and reports why (`machine_error` in status, `machine:  unavailable: <reason>` in the text report). A pinning server refuses such a broker (below). |

!!! warning "A machine restored from another machine's backup"
    The id is never derived from the hardware, so a cloned disk or a restored backup is the
    one way two machines end up with the same id. On the copy, stop the broker, delete
    `machine.json`, and start it again: it creates a new identity. Two machines sharing an
    id look like one machine to a server, and the second to connect is refused as a
    duplicate.

## How the source is chosen

Every bus subscribe and every response a broker sends carries two headers:

| Header | Value |
|---|---|
| `X-Shed-Machine-Id` | the machine's `id` |
| `X-Shed-Machine-Name` | the machine's `name`, percent-encoded UTF-8 |

The server keeps one binding per user. The **first machine to connect for a user is bound**
(the binding records `first-connect` as who bound it). After that the server answers each
subscribe as follows:

| Answer | `error.code` | When | What the broker does |
|---|---|---|---|
| `200` (stream) | — | the machine is the user's source | serves requests (`connected`) |
| `403` | `SOURCE_NOT_BOUND` | another machine is the source; the body names it in `source.machine_id` and `source.machine_name` | state `not-source`; retries on its 1–30 s backoff without enrolling a new certificate |
| `409` | `SOURCE_DUPLICATE_STREAM` | this machine's id already holds the namespace on this server | state `duplicate`; retries |
| `403` | `SOURCE_MACHINE_ID_REQUIRED` | the request has no valid machine headers (a broker older than the pin) | state `reconnecting`; retries |
| `429` | `SOURCE_RATE_LIMITED` | too many new or changed machines for this user lately (burst of 8, one more every 15 s); carries `Retry-After` | state `reconnecting`; retries after `Retry-After` (whole seconds, at most a minute), never sooner than its backoff |

Matching is by id only. The server also records each machine's name and the fingerprint of
the key its certificate was issued to, and shows both, but never matches on them. It
remembers up to 32 machines per user, refused ones included, and forgets the least recently
seen past that.

A `duplicate` has three usual causes: the desktop app's embedded broker connecting in the
moment before the daemon on the same machine lets go ([below](#the-daemon-and-the-desktop-app-on-one-machine)),
a state directory copied to another machine, or a reconnect that arrives before the server
has ended the previous stream. A stream that has gone dead is ended by the server's write
deadline (15 s), after which the next retry succeeds. No stream ever replaces another.

Responses follow the same rule: the server accepts an answer only from the user's bound
machine, and only to a request it sent that machine under the current binding. An answer
from any other machine is refused with `403 SOURCE_NOT_BOUND`, and one that matches no
request the machine was sent with `403 FORBIDDEN`.

A server with credential sources lists `source_binding` in the `features` of
`GET /api/info`.

The binding lives on the server, in `source-bindings.json` in the backend's state
directory. A server that starts without that file logs a warning that no bindings exist and
that the first machine to connect for each user becomes its source; deleting the file on
purpose is how an operator discards every binding.

### The daemon and the desktop app on one machine

The `shed-host-agent` daemon and the desktop app's embedded broker share the machine
identity (`<state>/host-agent/machine.json`), so to a server they are one machine, and only
one of them can hold each namespace. Which one is decided on the machine itself, not by
the server: **while the app's embedded broker runs, it answers, and the daemon stands by**.
The app is where the prompts are answered; a daemon that delegates approval to the app
would have nothing to send its prompts to while the app runs its own broker.

The app's embedded broker holds an exclusive lock on
`<state>/host-agent/embedded-broker.lock` (beside `machine.json`) while it runs. It takes
the lock only when it has a machine identity: without one a pinning server refuses its
subscribes, so it leaves the lock alone and the daemon, if one runs, keeps answering. The
app's broker starts once it holds the lock, or after one second without it; in that case
it keeps trying (every 2 seconds) and takes the lock as soon as it can, for example when
an earlier instance of the app exits. Until it holds the lock, the daemon does not know the
app is there.

The daemon has one watcher that looks at the lock every 2 seconds (a shared lock, which
never gets in the way of another look) and tells every namespace what it found:

- While the lock is held, the daemon subscribes nothing and answers nothing. Each
  namespace reads `yielding`, with `the desktop app on this machine is answering (its
  embedded broker is running); this daemon takes over within a few seconds of the app
  quitting`; its mark in `shed-host-agent status` is `-`. This applies at once, whatever
  the daemon was doing: connected, connecting, or waiting to retry, with one exception: a
  wait the server asked for (a `429` with `Retry-After` on a subscribe, at most 60
  seconds). The daemon has no stream during that wait; it stays `reconnecting` until the
  wait ends, then yields before it would subscribe.
- When the app starts while the daemon holds a namespace, the daemon closes its streams at
  its next look, within about 2 seconds; the app's broker, refused as a `duplicate` until
  then, connects on its next retry. The daemon also stops answering: it starts no handler
  for a request it has received but not started, and a handler that was already running
  (for example waiting for an approval) finishes but posts nothing, and its audit entry
  reads `undelivered` with the reason `result <what it came to>, but not sent: left for
  the desktop app (...)`. If an earlier send of that answer may have reached the shed (it
  got no answer in time, or a refusal that was not the shed server's own), only the retry
  was left to the app: the entry keeps its result and its reason says delivery unknown and
  that the retry was not sent, left for the desktop app.
- Work the daemon received before it yielded is never answered by it afterwards, even if
  the app has quit again by then: a request it had queued is dropped when the app starts,
  and an answer it was holding back for a retry is not sent again. Each such request stays
  pending on the server, which sends it again to the app's broker when it connects (same
  machine), and the app answers it; if the app quits without answering, the server sends
  it again on the daemon's new stream, and the daemon answers that copy. The one overlap
  left is an answer the daemon was already sending at the moment it yielded: if the app
  answers the same request too, the first answer to arrive is delivered and the other is
  refused and audited `undelivered`.
- When the app quits, however it quits (a crash included: the operating system drops the
  lock with the process), the daemon subscribes at its next look, within about 2 seconds,
  without waiting out a retry delay. The exception is the same server-requested wait as
  above: if the app started and quit while the daemon was waiting out a `429`, the
  daemon subscribes when that wait ends.

On a server older than credential sources (0.9.x), the second subscriber of a namespace
is refused with a `409` that a broker treats as final. If the app's broker subscribes
while the daemon still holds the namespace (before the daemon's next look), it keeps
retrying that `409` for 15 seconds after it started or took the lock, long enough for the
daemon to let go, and treats it as final after that, as before. The daemon and every other
broker treat it as final at once, as before.

What is not covered:

- If the daemon and the app are configured with different state directories, each has its
  own lock and its own machine id: to the server they are two machines, and the
  [binding](#how-the-source-is-chosen) decides between them as between any two machines.
- If the state directory is on a filesystem where `flock` does not work between processes,
  the daemon cannot see the lock (a look that fails is logged once and read as "the app is
  not running", so the daemon answers), and the app logs why it cannot take it; either
  process may then hold a namespace, as before this lock existed.
- Two daemons on a copied state directory take no lock from each other: one holds each
  namespace and the other stays `duplicate`, retrying on its backoff.

## What the other machines show

A machine that is not the source shows it in three places:

- `shed-host-agent status` marks the namespace `-` with `not-source`, names the source, and
  prints the command that would move it here:

    ```text
    Servers (1):
      devbox  (https://devbox.example.com:18443)
        -  ssh-agent  not-source: not the credential source for this server; the active source is desk (7d2e0c41a9b85f3e6c1d0a2b4f8e9c57)
        this machine is not the credential source here; the active source is desk
        to make this machine (laptop) the source: shed -s devbox source use -- 0f3c9a2e7b1d4c6a8e5f2b9d1c7a3e60
    ```

    The command names this machine by its full id, which cannot be ambiguous: two of a
    user's machines can share a name (the same hostname), and `shed source use` refuses a
    name that matches more than one machine. The name is shown on the same line.

- The status JSON (and the desktop's `broker.status`) carries `state: "not-source"` and an
  `active_source` object (`machine_id`, `machine_name`) on the namespace and on the server.
  See [Host Agent IPC](host-agent-ipc.md#status-socket).
- The desktop app's Preferences show the source's name on that server's row, with a
  **Use this machine** button. A server that cannot be reached when Preferences reads it
  (it is restarting, or the network is down) keeps the row it last showed, marked
  **cannot reach the server**, and Preferences reads it again a few times, a little
  further apart each time; then it waits until the window is shown or focused again. A
  server older than credential sources has no row, and nor does one that has not been
  reached since the window opened (there is no last row to keep). A server that answers
  but is busy shows **server busy, retrying** instead.

A `duplicate` is shown as `x` with a message naming the duplicate stream.

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
A machine appears in `shed source ls` only after its broker has tried to connect to that
server at least once. An admin can act on another user's source with `--user`.

From the desktop app: Preferences → Credential broker lists one row per server that supports
credential sources, and **Use this machine** makes the machine the app runs on the source
there. See [Desktop configuration](../desktop/configuration.md#credential-sources).

What a move does:

1. The server records the new binding.
2. It closes the old machine's streams and drops every request it had sent that machine.
   An approval still waiting on the old machine is lost: if it is approved there, the answer
   is refused, and the old machine's audit log records it as `undelivered`.
3. The new machine starts answering when its broker next subscribes. The desktop app's
   embedded broker is woken at once by the button; a `shed-host-agent` daemon picks the
   change up on its next retry, within about 30 seconds (while the desktop app runs its
   embedded broker on that machine, the app answers instead).

The routes behind both are `GET /api/sources` and `POST /api/sources/use` with
`{"machine_id": "<32 hex>"}` (an admin may add `"user"`). Both need a control-scope
credential:

| Answer | Code | Meaning |
|---|---|---|
| `200` | | the user's binding and machines (`use` answers the same document, after the move) |
| `400` | `INVALID_REQUEST` | a malformed body or machine id, or an admin naming a user who is not configured |
| `403` | `FORBIDDEN` | a script key, a service client, or a non-admin naming another user |
| `404` | `SOURCE_MACHINE_NOT_FOUND` | no machine with that id has presented itself for the user |
| `500` | `INTERNAL_ERROR` | the move could not be confirmed; check `shed source ls` before retrying |
| `503` | `SOURCE_BUSY` | (read only) the user's records were being changed or saved; carries `Retry-After` |

## Who may be or move a source

- A user may move their own source from any of their keys that is not a script key; an
  admin may also move another configured user's.
- A **script key** cannot be a source or move one: its certificate is refused on subscribe
  and respond, and on both source routes, with `403 FORBIDDEN`.
- A shed owned by a service client gets nothing from any source; the bus is off for it.
- The remote backend has no in-VM helpers, so its sheds send no credential requests.

## Nothing fails over by itself

The binding changes only when someone moves it. If the source is asleep, off the network or
not running a broker, the server has no listener for that user and drops each request (and
logs it); the tool in the shed fails when its own timeout passes (3 seconds for the SSH
agent and the AWS proxy). Another of the user's machines keeps reporting `not-source` and
does not take over. Move the source with `shed source use` or the desktop button.

## Upgrading

Upgrade machines **before** servers (the whole procedure is in the [v0.9.1 → v0.10.0
upgrade guide](../upgrades/v0.9.1-to-v0.10.0.md)):

- A broker that sends the headers works against an older server, which ignores them. (On a
  server older than the pin, a second listener for a namespace still gets the old terminal
  `409`.)
- A broker released before the pin sends no headers, so a pinning server refuses every
  subscribe and response from it with `403 SOURCE_MACHINE_ID_REQUIRED`, whose message names
  the versions to upgrade to. It keeps retrying, but credential forwarding for that machine
  stops until it is upgraded.

On its first start with credential sources a server has no bindings, so whichever of a
user's machines connects first becomes the source. Start the intended machine first, or
check `shed source status` afterwards and move it.

When a user list is added to a server that had none, the first start with the list hands the
implicit user's binding and remembered machines to the user if exactly one user is
configured, and drops them if several are; each user's first machine to connect is then
bound. At every start the records of users no longer configured are dropped.
