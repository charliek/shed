# Credential delivery

When a shed is created, and when its user runs `shed provision`, shed-server asks that person's **credential source** (the shed desktop app on the machine they bound with `shed source use`) for the files and environment variables the shed should hold: an SSH key, a GitHub token, API keys, an envsecrets login, named files. The source answers, the server checks the answer, and the shed's agent writes it into the shed. The shed keeps working from those files afterwards; nothing is asked per use.

The documents and the transport are specified in [The provision contract](provision-contract.md). This page says what the server does with them.

## When delivery runs

| Moment | Delivery |
|---|---|
| `shed create` | Yes, once, as part of the create. |
| `shed provision <name>` | Yes, on a running shed, by its user. |
| `shed start` | No. A start contacts no source. |
| A snapshot restore | No request. The files are already in the snapshot. |
| `shed reset` | No request. Reset clears the delivered items and marks every step owed. |

Delivery is never done in the background and never retried by the server. A failure is a recorded outcome and a warning; the shed starts regardless.

## Who is asked

The server asks the source of the **shed's user**, and only that user can cause a delivery: they created the shed, or they run `shed provision` on it. An admin cannot provision another user's shed.

No request is made, and the state is `not_applicable` with no warning, when:

- the user has no bound machine (a person who never ran `shed source use` sees the behaviour that existed before delivery);
- the shed was created by a service client, or the create used a script key (`shed provision` with a script key, or by a service client, is refused with `403`);
- the shed was restored from a snapshot.

A bound machine that is not connected ends the delivery at once as `source_offline`. The server waits only for a connected source, up to [`delivery.timeout`](#configuration), because a source may have to run a passphrase command.

## The four steps

Create and `shed provision` run the same four tracked steps, in this order:

| Step | What it does |
|---|---|
| `stage1` | Writes the answer's stage 1 items (the home directory, and the environment file) before the clone, so a private clone can use a key or token. |
| `clone` | Clones the repository, if the shed has one. |
| `stage2` | Writes the stage 2 items, which land in the project directory (a repository's `.env`, for example). |
| `install` | Runs the repository's install hook. |

The bundle is requested once, before stage 1, held in memory until stage 2 is written or skipped, and never stored by the server.

Each step is in one of four states, saved durably after each step:

| State | Meaning |
|---|---|
| `done` | The step completed. |
| `skipped` | The step had nothing to do, with a reason. |
| `failed` | The step was tried and failed, with a reason. |
| `owed` | The step was never reached, or something it depends on is not `done` or `skipped`. |

A step is attempted only when what it depends on is `done` or `skipped`.

| Step | Depends on |
|---|---|
| `stage1` | nothing |
| `clone` | nothing (a public repository needs no credential; a private one fails quickly and is recorded `failed`) |
| `stage2` | the bundle and the clone |
| `install` | stage 1, the clone and stage 2 |

So a create whose source is offline ends with stage 1 `failed`, the clone `done` for a public repository, and stage 2 and install `owed`. The shed starts either way. The startup hook on create runs only after an install step that is done (or that found no hook), as before, so with an offline source neither the install hook nor the startup hook runs on create; both wait for `shed provision`. The startup hook still runs on every start.

!!! note "The install hook now waits for the clone"
    Before delivery, the install hook ran whatever happened to the clone. It now runs only after stage 1, the clone and stage 2 are done or skipped. A bound source that is offline therefore holds back the install hook, even for a public repository, until `shed provision` runs with the source connected.

### Skipped steps

| Reason | Skips |
|---|---|
| `not_applicable` | stage 1, stage 2 |
| `no_items` | stage 1 or stage 2: the answer had no item for it |
| `no_repo` | clone |
| `local_dir` | clone and stage 2: the project directory is a host directory (`--local-dir`) that outlives the shed, so nothing is written into it |
| `no_project_dir` | stage 2 |
| `no_hook` | install |
| `no_provision` | install: created with `--no-provision` |
| `from_snapshot` | every step |
| `adopted` | every step (a cloud shed adopted at server start-up whose history is unknown) |

`--no-provision` skips the install hook only, and records it as skipped (`no_provision`); `shed provision` retries only owed and failed steps, so it does not run that hook later. It does **not** skip delivery.

## The record

The server keeps a record per shed and returns it in the shed document as `delivery` ([HTTP API](api.md#delivery-in-the-shed-document)). It holds no value, no content and no problem text beyond what the source chose to send.

| Member | Meaning |
|---|---|
| `state` | `none`, `not_applicable`, `delivered`, `partial` or `not_delivered` |
| `steps` | the four steps, each with a state and, for `skipped` and `failed`, a reason |
| `last` | the last attempt: its time, `create` or `provision`, its outcome, the failure if any, and the machine that was asked |
| `items` | each item the shed holds or was sent: name, provider, stage, kind, target (a path or a variable name), a fingerprint, when it expires, when it was written, and its result |
| `expired` | the names of items whose `expires_at` has passed (the shed document, not the contract's record) |
| `owed` | the steps that are owed, in the order they run (the shed document) |

The states are decided in this order: `none` (no record, or an adopted cloud shed), `not_applicable` (no request was made and nothing failed), `delivered` (every step done or skipped and no problem reported), `partial` (something is in the shed as written), `not_delivered` (nothing is).

An admin viewing another user's shed sees the `state` alone, with `withheld: true`.

`shed provision <name> --status` prints the record. `shed list` shows the state in a `DELIVERY` column when any listed shed has one to show.

## Failure and the warning

An answer that is not accepted (the source offline, a timeout, a refused answer) writes nothing to the shed. A write failure can leave the items before the failed one written, and the record says which. `shed provision <name> --status` shows what the shed holds and what is owed.

A delivery that cannot finish prints a fixed warning that names the command that finishes it:

```text
Credentials were not delivered (<reason>). Run: shed provision <name>
```

| Reason | Meaning |
|---|---|
| `source_offline` | a machine is bound but not connected |
| `source_timeout` | no answer within `delivery.timeout` |
| `source_disconnected` | the source's stream ended before it answered |
| `answer_refused` | the answer broke a contract rule (the rule is recorded) |
| `write_failed` | an item could not be written (the item's name and a closed result are recorded) |
| `image_too_old` | the shed's image has an agent that cannot take items |

The create streams the warning as progress. On the remote backend, whose create answers `201 starting`, the outcome is read from the shed document.

## Re-delivery, expiry and reset

- **`shed provision`** requests a bundle with the reason `provision`, then runs the four steps in order, each only if it is `owed` or `failed` (stages 1 and 2 are written again whenever a bundle arrives). On a shed whose create finished, it is a re-delivery and nothing else. If the install hook runs and succeeds for the first time here, the startup hook runs after it. `--no-provision` recorded the install as `skipped`, so a provision does not run it.
- **A file item replaces its target.** Delivery never deletes a file: an item delivered before and absent from the new answer stays on disk and in the record, marked `not_in_last_answer`.
- **Expired items.** An item may carry an expiry. `shed start` does not fetch a new one; it prints one warning per expired item (`Warning: delivered item <name> has expired. Run: shed provision <name>`). The files stay where they are.
- **The clone under `provision` never removes anything.** An existing work tree of the repository is `done`; any other existing directory fails the step with `project_dir_exists` and is left alone.
- **One at a time.** A second provision on the same shed is refused with `409`. A stop or delete waits for the step in hand. On the remote backend a provision is refused while a job for that shed is in flight.
- **`shed reset`** wipes the shed's disk. It clears the delivered items, marks every step owed, asks no source, and its output names `shed provision`.

## Where values go

The agent writes each item with its own file mode (`0600` or `0400`), under the shed user's home or the project directory, without following a symbolic link at any step of the path. Environment values go to one file, `~/.config/environment.d/50-shed-delivery.conf` (`0600`), replaced whole at each delivery. The agent reads that directory after `/etc/environment.d` when it builds a command's environment, so the values reach commands and sessions started afterwards. A process already running (a tmux server, a roost session, a craze hub) keeps the environment it started with.

The files are plain files. They are in snapshots and on a stopped cloud shed's disk.

### What "no values" covers

The server never records, logs or returns a delivered value: not in the record, the log, the audit log, the activity feed, an API response or an error. The record's `fingerprint` is a keyed hash that lets a person tell two deliveries apart, not a hash of the value anyone can check.

It does not cover:

- **A repository's own output.** The clone's and the install hook's output (a hook that prints `$TOKEN`) is the repository's, not delivery's.
- **What the source chooses to send.** A fingerprint and a problem message come from the source; the contract bounds them but a source decides their text.
- **The shed's disk and environment.** The value is in the shed, readable by the shed user and by anything running as it, including coding agents.

## Old images

The agent's protocol number is 2. The server checks it before it sends any value. An agent below 2 ends the delivery as `image_too_old`; the shed still starts. Recreate the shed from a current image to receive delivery. The `delivery` token in `features` ([HTTP API](api.md)) says a server does delivery.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `delivery.timeout` | `30s` | How long a delivery waits for the answer of a connected source (1 s to 10 m). A bound source that is not connected is not waited for. |
| `audit.enabled` | on in `mtls` mode | The [audit log](configuration.md#audit-log) records the delivery and lifecycle events behind the activity feed. `false` turns the default off. |

A create can wait up to `delivery.timeout` when the bound source is connected and slow.
