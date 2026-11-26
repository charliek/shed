# Remote Backend

The `remote` backend runs each shed as its own machine in a cloud, instead of on the server's host. The shed-server keeps a record of every shed, reaches each machine over SSH as a dedicated login user, and talks to the in-VM agent through forwarded unix sockets. Clients use the same API, CLI and SSH entry point as for a local server.

**The server holds no cloud credential and makes no cloud call.** It asks the **account service** it is enrolled with (`auth.directory`) for every machine, over the [compute contract](compute-contract.md). The service holds the cloud credential, decides which sizes and images exist, who may have how many machines and for how long, makes every cloud call, and deletes each machine when its lease ends. What a cloud shed's machine must run so the server can drive it (the link user, the agent, the host key, the shed's name and id) is the [guest contract](guest-contract.md).

A remote server differs from a Firecracker or VZ server in four ways:

- **Lifecycle is asynchronous.** A JSON create, start, stop or delete returns as soon as the new status is saved, and a job owned by the server does the work. A shed passes through `starting` and `stopping`.
- **Every machine has a lease.** The account service deletes a machine, running or stopped, at its lease's end. A person extends it with [`shed extend`](cli.md#shed-extend). No timer in the server deletes anything.
- **Some features do not exist.** There are no host directories, server mounts, snapshots, reset, image store, egress profiles or `shed assign`. Requests for them are refused before anything is created.
- **Isolation is by network topology.** The `isolated` network profile is accepted only while the account service says no machine can reach another and nothing but the server's own address can reach them (see [Ingress and the firewall modes](#ingress-and-the-firewall-modes)). Outbound traffic is not filtered: a machine has a public address and reaches any public address, including a port the server host exposes to the internet.

## What the server keeps, what the service decides

| The server keeps | The account service decides |
|---|---|
| A record of each shed (its status, its delivery history, its pinned host key, the access option it last used) in `remote.state_dir`, and a delete marker for each delete in progress | Which sizes and images exist (the catalog), and the disk a size gets |
| The jobs that follow a create, start, stop or delete to its end, each under a request id it stores | How many machines a person and the organization may hold |
| Its own limits (`remote.limits`), when set | When each machine is deleted: the first lease, the cap, whether a person may extend, the warning time |
| The SSH key it logs in with, and the host key it pins at first contact | Which people and which service clients may have machines at all (and whether this server may manage compute: the grant) |
| Which ingress sources it expects, and the check that refuses creates and starts while they are not applied | Where a machine can be reached (the `access` options), and the host key it attests |
| Credential delivery to a shed once it runs ([Credential delivery](delivery.md)) | Every cloud call, the cloud credential, and the machine's existence |

A record is a cache of what the service says plus what only the server knows. Lose the state directory and the server rebuilds a record for each ready machine the service lists for it (adoption, below); what it cannot rebuild is the delivery history, which is recorded as unknown.

## Statuses

A shed's `status` is one of five values. `status_reason` explains a status where the table lists reasons; it is empty otherwise.

| `status` | Meaning | `status_reason` values |
|---|---|---|
| `starting` | A create or start is in progress (or kept for the next pass), or the server adopted the machine and has not yet reached its agent. | empty, `no_pin`, `unreachable`, `cloud_unreadable`, `firewall` |
| `running` | The agent answered a probe. | empty |
| `stopping` | A stop is in progress. | empty |
| `stopped` | The machine is off and its disk is kept. | empty, `capacity`, `quota`, `limit`, `external`, `unreachable`, `host_key`, `access`, `layout`, `firewall` |
| `error` | The create failed. Only a delete leaves this state; the machine, if one was made, stays until then (or until its lease ends). | `capacity`, `quota`, `create_failed`, `host_key`, `access`, `layout`, `firewall` |

| Reason | Meaning |
|---|---|
| `no_pin` | The machine has no pinned host key yet. |
| `unreachable` | The server cannot reach the machine's SSH port or its agent. While `starting`, it keeps trying (a create whose guest never answers stays `starting (unreachable)` and is retried each pass until it is deleted or its lease ends); on `stopped`, a start's machine came up but its agent did not answer (or could not be driven), so the server stopped it again. |
| `cloud_unreadable` | The server could not learn from the account service what the machine is doing: the read failed, or a reply to a create never came. It changed nothing, keeps the create or start, and looks again at every periodic pass. A delete still works after a failed read. |
| `capacity` | The cloud had no room for the machine. |
| `quota` | The cloud's own quota refused it. |
| `limit` | The server's `limits` (`max_running` or `max_sheds`) were reached. |
| `external` | The machine was stopped outside shed (or by the service). |
| `layout` | The machine's agent or guest layout is not compatible with this server. |
| `create_failed` | The create failed for another reason; the server log has the detail. |
| `host_key` | The host key the machine presented did not match the pinned key, or the account service attested another: a create ends in `error`, and a start leaves the shed `stopped` with its machine stopped again. |
| `access` | The machine offers no way in this server can use (only access kinds it does not know), or one at a host outside [`remote.access.allow`](#reaching-a-machine); the server does not dial it. |
| `firewall` | The [ingress check](#ingress-and-the-firewall-modes) was failing when the server was about to ask for the machine, so it did not. |

The server notices a machine stopped from outside shed at its next [pass over the service](#keeping-records-and-the-service-in-step), at most about a minute later, and until then lists the shed as `running`; it records it `stopped` with reason `external`.

`shed list -v` shows the reason beside the status (`stopped (external)`), and `shed list --json` and the API carry it as `status_reason`.

## Calls and statuses

`POST /api/sheds` without `Accept: text/event-stream` returns **201 with `status: "starting"`** and the create continues in the background. Before it answers, the server runs the [ingress check](#ingress-and-the-firewall-modes) when its last result is more than 60 s old (waiting at most 10 s for it), and then waits for the service to accept the new machine, for at most 15 s (the fast-fail window), so that a refusal arriving that early is answered as a 4xx or 503 and leaves no shed. Which request gets which answer:

| The account service | The server answers | Shed left |
|---|---|---|
| Refuses the create (a limit, its policy, the cloud's quota or capacity) | The refusal's [code](#codes-and-reasons) | None |
| Was never reached (connection refused, no token could be had), or refused the create before acting on it (rate limited past the window, a second 401) | `503 COMPUTE_UNAVAILABLE` | None |
| Could not be asked for the machines' ingress by the [ingress check](#ingress-and-the-firewall-modes) (unreachable, hanging, failing) | `503 COMPUTE_UNAVAILABLE` (not `FIREWALL_CHECK_FAILED`, which is for an ingress found wrong) | None |
| Received the create and its reply never came | 201 `starting`, reason `cloud_unreadable` | Kept; the pass sends the same create again under the same request id |
| Is still deleting a machine of that name | `409 COMPUTE_DELETING`, after waiting for the name up to the same 15 s | None |
| Accepts it | 201 `starting` | The shed |

So a service that is down answers differently depending on how it is down. One that **refuses connections** is `503 COMPUTE_UNAVAILABLE` at once: nothing reached it. One that **accepts the connection and hangs** may have read and acted on the create before it stopped answering, so the server cannot call it refused: the create is kept `starting (cloud_unreadable)`, the request (or its SSE stream) waits up to its bounds, and each periodic pass sends the same create again under the same request id until the service answers; it is made once. Once a pass has seen the ingress unreadable, later creates and starts are `503 COMPUTE_UNAVAILABLE` at once until the service answers again.

A create is cleaned up (its machine deleted) only when the service **refused that create itself**, or the person deleted the shed. Once a create may have been received, every later failure keeps the shed (`starting` with a reason for the next pass, or `error` when no retry changes it) until it is deleted or its lease ends. A refusal that arrives after the fast-fail window (the cloud's quota or capacity found late) leaves the shed in `error` with reason `quota` or `capacity`.

Each step has its own bound, but the reply has no single one (it can also wait behind other work on the server's records), so give the request a timeout of a minute or more. A client whose request timed out may still have created the shed: get it by name before creating it again. Poll `GET /api/sheds/{name}` until the status is `running`. With `Accept: text/event-stream` the stream stays open until the shed settles: the `complete` event carries the running shed, and an `error` event reports a create that failed. Closing the stream ends the stream only, never the create.

| Call | `starting` | `running` | `stopping` | `stopped` | `error` |
|---|---|---|---|---|---|
| start | 200, no change | 409 | 200, queued | 200 `starting` | 409 with the reason |
| stop | 200, queued | 200 `stopping` | 200, no change | 409 | 409 |
| delete | 204 (JSON) in every state; the shed disappears from lists at once | | | | |

"Queued" means the job waits for the service's operation already running, then acts. A start or a stop of a machine the service is deleting is refused `409 COMPUTE_DELETING`, and a start at or after the lease's end `409 LEASE_ENDED`.

A delete without `Accept: text/event-stream` answers as soon as the shed is hidden; the machine is deleted in the background. With `Accept: text/event-stream`, which is what `shed delete` sends, the shed is hidden just as fast but the stream stays open until the machine is gone, so the command returns only then and a create right after it never meets the old machine. A delete is final: it deletes the machine and its disk, as the lease's end does. A delete that fails is retried by the periodic pass; the shed is hidden all the same, and a streamed delete then ends with an error event.

Exec (`POST /api/sheds/{name}/exec`) and the Connect tunnel on a shed that is not running return **409 `SHED_NOT_RUNNING`**. (Firecracker and VZ answer exec the same way, but the Connect tunnel with `503 SHED_NOT_RUNNING`; see [API › Connect API](api.md#connect-api).)

The CLI waits for you: `shed start` and `shed stop` poll until the shed settles (up to 180 s), `shed delete` returns once the machine is gone, and `shed attach`, `shed console`, `shed sync` and `shed tunnels` wait for a starting or stopping shed. SSH to the server (`ssh <shed>@<server>`) starts a stopped shed and waits up to 150 s, writing progress to stderr.

The `--repo` clone of a create runs with `GIT_TERMINAL_PROMPT=0`, for at most 5 minutes an attempt and at most 3 attempts. A clone that still fails is a warning, as on the other backends: the shed runs without the repository.

## Leases

Every cloud shed's machine has a lease, which the account service enforces:

| Fact | Rule |
|---|---|
| The end | `cloud.lease.expires_at`. At that instant the service deletes the machine and its disk, **running or stopped**. There is no undelete. |
| The first lease | A person's: the service's `initial_lease_s`. A service client's: the `lease` its create names (`CreateShedRequest.lease`), else its `policy.max_age`, and never above the service's longest client lease. |
| The cap | `cloud.lease.max_expires_at`: the creation time plus the service's `max_age_s` for a person. An extend never goes past it (`409 LEASE_AT_CAP`, nothing changes). A client's cap is its lease. |
| Extending | A person extends by whole hours, 1 to 720 per request: [`shed extend`](cli.md#shed-extend), [`POST /api/sheds/{name}/extend`](api.md#post-apishedsnameextend). The shed's own user or an admin may; a service client's shed cannot be extended. |
| Stop and start | Change neither time. A start (and an extend) at or after the end is refused `LEASE_ENDED`. |
| Who deletes | Only the service. The server has no timer: a server that is down, restarted or removed deletes nothing at a lease's end, and the machine goes when the service says. The server learns of a deletion from the service's list and drops the shed's record with a log line naming the cause (`lease_ended`, `page`, `external`, `server`) and an audit event. |
| The warning | When less than `warn_s` is left (the service's setting, 2 hours when it names none, never when 0) the `LEASE` column is marked `!` and the [lease banner](cli.md#the-lease-banner) is printed once at an interactive login. |

When the service is down at a lease's end, enforcement is late: a stopped machine is deleted by the service's next sweep after it returns, and a running one at the latest at its cap.

A local admin is an admin of the server, not a person of the service, and **creates no cloud shed** (`COMPUTE_POLICY_REFUSED`, reason `person`): the service makes machines for the people it lists and for registered service clients only. On the sheds there are, it may:

| Shed | List and view | Stop, delete | Extend | Start, enter, exec |
|---|---|---|---|---|
| A person's | yes | yes | yes | no (`403`): an admin does not enter another user's shed |
| A service client's | yes | yes | no: the service refuses it (`EXTEND_NOT_ALLOWED`) | yes |

A stopped person's shed is started by that person (a `shed exec` or console starts it too).

## Codes and reasons

The account service's refusals reach a client as these API codes (the full table, with each status, is in [API › Cloud error codes](api.md#cloud-error-codes)). A `reason` is the error's `reason` member.

| Code | HTTP | Reason | When |
|---|---|---|---|
| `COMPUTE_LIMIT_REACHED` | 503 | `person`, `organization` | The service's limit on machines (every state counted) is reached. Never a 409: a client reads a 409 on create as "the shed already exists". |
| `COMPUTE_POLICY_REFUSED` | 422 | `size`, `lease`, `image`, `access`, `person`, `client` | The service's policy refused the create. `person`: a local admin, or a person the service does not list for this server. `client`: a service client the service has not registered for this server. |
| `COMPUTE_DENIED` | 403 | | The service does not let this server manage compute. |
| `COMPUTE_UNAVAILABLE` | 503 | | The service could not be reached or answered too late, or the server is [degraded](#degraded-start). |
| `COMPUTE_DELETING` | 409 | | The machine (or the name) is being deleted. |
| `EXTEND_NOT_ALLOWED`, `LEASE_AT_CAP`, `LEASE_ENDED`, `PERSON_INACTIVE` | 403, 409, 409, 403 | | An extend, start or stop the lease or the person's state does not allow. |
| `CLOUD_QUOTA_EXCEEDED` | 503 | | The cloud's own quota refused the machine. |
| `ZONE_CAPACITY_UNAVAILABLE` | 503 | | The cloud has no room for the machine. |
| `SERVER_LIMIT_REACHED` | 503 | | The server's own `max_running` or `max_sheds` is reached. |
| `FIREWALL_CHECK_FAILED` | 503 | | The [ingress check](#ingress-and-the-firewall-modes) found the ingress wrong. The message says what was expected and what was found. A check that could not read the ingress is `COMPUTE_UNAVAILABLE`. |
| `SIZE_UNAVAILABLE` | 422 | | No size of the service's catalog has the requested `cpus` and `memory_mb`. |
| `FEATURE_UNSUPPORTED` | 422, 501 | | See [Unsupported features](#unsupported-features). |

None of the 503 refusals is ever a 409. A refusal that arrives after the create was accepted leaves the shed in `error` (create) or `stopped` (start) with the matching reason. `FIREWALL_CHECK_FAILED` is answered on the request, before anything is created or started; the server asks the check again just before it sends the service a create or a start, because that can be long after the request (a create resumed after a server restart, a start queued behind a stop). If it fails then, nothing is sent and the shed is left in `error` (create) or `stopped` (start) with reason `firewall`.

A reply the server cannot trust (a machine object that breaks the contract's validation, a body that echoes the server's bearer token) is treated as `COMPUTE_UNAVAILABLE`, and nothing in it is used. Text the service sends in an error is read for its code only: its `message` is never logged, stored or shown.

## Unsupported features

| Feature | Create field or route | Answer |
|---|---|---|
| Host directories | `local_dir`, `add_dirs` (`--local-dir`, `--add-dir`) | 422 |
| Server mounts | non-empty `mounts` (`--mounts`) | 422; an omitted `mounts` stores `none`, and every shed record reports `"mounts": {"mode": "selected"}` (`shed list` shows `none`) |
| Snapshots | `from_snapshot`, `POST /api/snapshots`, `GET` and `DELETE /api/snapshots/{name}` | 422 or 501; `GET /api/snapshots` lists nothing |
| Reset | `POST /api/sheds/{name}/reset` | 501 |
| Image store | image inspect, tag, pull, push, delete and prune | 501; `GET /api/images` lists nothing (the images are the account service's, by name) |
| Egress profiles | `egress` other than `["off"]`, egress set, off and profile writes | 422 or 501 |
| Assigning a user | `POST /api/sheds/{name}/assign` (`shed assign`) | 501: a cloud shed's user is its machine's, fixed by the service at create |
| Creating as a local admin | any create by an admin of `auth.users` who is not a person of the service | 422 `COMPUTE_POLICY_REFUSED`, reason `person` |

The CLI refuses `--local-dir`, `--add-dir`, `--from-snapshot`, `--egress` and named `--mounts` before sending anything when `/api/info` reports `backend: "remote"`.

## Server information

`GET /api/info` on a remote server reports `backend: "remote"`, the account service's default image, and in `features` the tokens `async_lifecycle` and `lease`, and `network_profiles` unless the `firewall` config admits more than the server (mode `any`, or `acknowledge_any`). Beside them are the tokens every server sends (`overview`, `http_exec`, `exec_limits`, `mount_selection`, `uniform_shed_record` and `source_binding`), and `service_clients` when `auth.clients` is set (see [API › GET /api/info](api.md#get-apiinfo)). It also reports `isolation: "topology"`, and `guest_arch` when every size of the service's catalog has the same `arch` (omitted when they mix, or before the catalog is read). `isolation` names the backend's kind whether or not the current `firewall` config offers the `isolated` profile: `network_profiles` says whether it does, and each shed record's own `isolation` says what that shed has.

A remote shed's record carries what a Firecracker or VZ record does ([API › GET /api/sheds](api.md#get-apisheds)): `mounts` (always `{"mode": "selected"}`), `network_profile`, `isolation` (`topology` for an `isolated` shed, `none` for an `open` one, and `none` for an `isolated` one while the `firewall` config admits more than the server) and `arch`, the architecture of the shed's size, stored at create. `isolation` is the kind the server's configuration provides, not a live check that the network still isolates the machine. It also carries the [`cloud` object](api.md#the-cloud-object) from its first record: its size, disk, image, times and lease, with the lease absent until the service has answered for the machine. No response to a service client names the cloud project, zone or any address; a `503 FIREWALL_CHECK_FAILED` sent to an admin does name the sources it expected and the server's own address, because the admin is who must fix them, and a service client gets the code with a message that names none of them.

## Sizes and images

A remote server offers its account service's named sizes (its catalog) instead of arbitrary CPU and memory values. A create with neither `cpus` nor `memory_mb` gets the catalog's default size; otherwise it gets the smallest size that has at least both, and `422 SIZE_UNAVAILABLE` when none has. The shed's record reports that size's CPUs and memory, and a service client's caps are checked against it. The disk is the size's own default unless `remote.disk_gb` is set, which wins. `upper_size_bytes` (`--upper-size`) sets the boot disk size, rounded up to whole GB, between 10 and 500 GB.

A create names an image `default` or one of the catalog's aliases. A service client's `policy.images` is checked at create against those aliases (a name the catalog lacks is logged when the catalog is first read, and a create naming it is refused). The server reads the catalog at start and in every pass, and keeps the last one it read when a read fails. The image the service resolved a name to is `cloud.image` on the record.

The cloud image has two variants like the others: `base` (with roost-session, craze and, unlike the Firecracker and VZ `base`, Docker CE) and `full` (plus `strix`, `prox`, `parcel` and `envsecrets`), `full` the default. How an image is built is the [guest contract](guest-contract.md); `make gce-image` bakes the reference image for Compute Engine, one example adapter.

## Configuration

The config file holds no address or key. Every `*_env` field names a process environment variable that the server reads once at start-up; start-up fails, naming the variable, when one is missing. On a deb install they go in [`/etc/shed/server.env`](#the-variables-on-an-installed-server-deb). The server needs `auth.directory` (see [Users from an account service](configuration.md#users-from-an-account-service)) and enrols with the service once (`shed-server directory enroll`).

```yaml
default_backend: remote          # `detect` never picks it
remote:
  provider: service              # service (needs auth.directory) | fake (needs allow_test_drivers)
  transport: ssh                 # ssh | local (local needs allow_test_drivers)
  allow_test_drivers: false
  state_dir: <path>              # record cache and delete markers (default <state dir>/remote)
  disk_gb: 0                     # 0: the size's own default; set, it wins
  limits: {max_running: 0, max_sheds: 0}   # 0 is off: the service holds the quotas
  agent_dir: <path>              # holds shed-agent-linux-<arch> (default <state dir>/agent)
  agent_install: incompatible    # incompatible | always | never; only never is built (see below)
  ssh:
    client_key_env: SHED_REMOTE_SSH_KEY             # base64 of the OpenSSH private key
    client_key_file_env: SHED_REMOTE_SSH_KEY_FILE   # or a path to it; exactly one must be set
  firewall:                      # see "Ingress and the firewall modes"
    mode: static                 # static | auto | any
    source_ranges: []            # static: the ingress's sources, as IPv4 CIDRs, or
    source_ranges_env: SHED_REMOTE_SOURCE_RANGES   # static: a variable holding them, comma-separated
    acknowledge_any: false       # must be true for mode: any, and for a static range wider than a /24
    echo_urls: []                # auto: at least two https services, on different hosts, that answer with the caller's address
  access:
    allow: []                    # CIDRs a machine's access host must be in; empty: every unicast address outside loopback, link-local, unspecified and multicast
```

Every field has the default shown. The keys `environment`, `sizes`, `default_size`, `default_image`, `image_aliases`, the run limit and `gce` of earlier releases are refused by name, as is `provider: gce`; the [upgrade guide](../upgrades/remote-compute.md) says what replaces each. With `default_backend: remote` the server refuses to load when the config also has a `firecracker:` or `vz:` block, `env_file`, top-level `mounts`, `egress.enabled`, `network.allow_cidrs`, `extensions.enabled`, a client `policy.server_mounts`, or `firewall.mode: any` (or `firewall.acknowledge_any: true` in any mode) beside service clients or an `isolated` default profile. A `remote:` block under any other backend is refused too.

| Field | What it does |
|---|---|
| `provider` | `service`: machines come from the account service. `fake` is an in-memory double for tests. |
| `transport` | `ssh`: the server reaches a machine at its `access` host. `local` (tests only): the server runs a real agent on its own host and ignores `access`, so a test needs no cloud. |
| `limits.max_running`, `limits.max_sheds` | The server's own caps, on top of the service's. `max_running` counts `starting`, `running` and `stopping` sheds; `max_sheds` counts every state. Past either: `503 SERVER_LIMIT_REACHED`. |
| `disk_gb` | The disk a create that names none asks for (10 to 500). |
| `agent_install` | Every value behaves as `never`: the server does not copy an agent into a machine. A machine whose agent reports an incompatible protocol or guest layout fails its create (`error`, reason `layout`) or start (`stopped`, reason `layout`). Bake a new image instead. |

### Reaching a machine

The service answers a create with an ordered list of **access** options. The server takes the first kind it supports (v1 defines `ssh`: direct SSH to `host:port` as the user `shed-link`, with an attested host key) and ignores kinds and members it does not know. It dials the host only when it is an IP literal outside the refused classes (loopback, link-local, unspecified, multicast) and inside **`remote.access.allow`** when that is set:

```yaml
remote:
  access:
    allow: [203.0.113.0/24, 2001:db8::/32]
```

An operator sets `allow` to the ranges the service's machines can have, so a service that is wrong or compromised cannot point the server at an address of the operator's own network. A machine whose address is outside it is not dialled: `error` or `stopped` with reason `access`. The access list is empty until a machine runs and its key is known; a record keeps the option it last used and asks the service again only when the dial fails.

**Host key.** The service attests the first key the guest publishes and never replaces it. At first contact the server compares the attested key with the key sshd presents, pins it in the record, and refuses any later change (`host_key`). The server logs in with the key named by `remote.ssh`; its public half is given to the service at every create and fixed into the machine, so a machine made with one key stays reachable only with that key (see the [upgrade guide](../upgrades/remote-compute.md#rotating-the-link-key)).

### The variables on an installed server (deb)

The deb's systemd unit reads `/etc/shed/server.env` if it exists (`EnvironmentFile=-/etc/shed/server.env`). The package ships no such file, and a server without one starts as before. systemd reads the file and passes its variables to `shed-server`; nothing else reads it. It is not the config's `env_file`, which holds variables for sheds.

For the configuration above, the file holds one of the SSH client key pair and the firewall's source ranges. The `*_FILE` variant keeps the key in its own file:

```sh
# /etc/shed/server.env: one NAME=value per line, no "export"
SHED_REMOTE_SSH_KEY_FILE=/etc/shed/remote/ssh_client_key
SHED_REMOTE_SOURCE_RANGES=203.0.113.7/32
```

If your `server.yaml` names other variables, use its names. To use `SHED_REMOTE_SSH_KEY` instead of a file, write the base64 value on one line (`base64 -w0`), and leave `SHED_REMOTE_SSH_KEY_FILE` out: exactly one of the pair may be set. The files the `*_FILE` variables name must be readable by the user the server runs as (root, under the deb's unit).

Create the file owned by root with mode 0600, since it can hold credentials, then restart the server. The first command makes an empty file: run it once, since it would empty a file that already holds your settings.

```bash
sudo install -m 0600 -o root -g root /dev/null /etc/shed/server.env
sudoedit /etc/shed/server.env
sudo systemctl restart shed-server
sudo journalctl -u shed-server -n 50 --no-pager
```

A server that started prints `Initialized remote backend (provider=service, transport=ssh)`, then the [ingress check](#ingress-and-the-firewall-modes)'s result (`remote: firewall check passed (...)`). A variable that the config names and the file lacks stops the start, and the log names it:

```text
remote backend: neither environment variable SHED_REMOTE_SSH_KEY (remote.ssh.client_key_env) nor SHED_REMOTE_SSH_KEY_FILE (remote.ssh.client_key_file_env) is set; set exactly one
```

The unit tries again every 5 seconds; after fixing the file, run `sudo systemctl restart shed-server` to start it at once. `shed-server config-validate` does not read these variables.

The unit `shed-server install` writes (a server installed without the package) has the same line. One written by an older `shed-server install` lacks it: run `sudo shed-server install` again, which overwrites `/etc/systemd/system/shed-server.service` with its current template (your edits to it are lost), runs `systemctl daemon-reload` and enables the service, but does not start or restart it; then run `sudo systemctl restart shed-server`. Or add the line under `[Service]` by hand and run `sudo systemctl daemon-reload` and the restart. Do not run `shed-server install` on a deb install: it would replace the package's unit. A Homebrew install runs `shed-server` as a launchd service whose environment the formula sets (`PATH` only), so neither this file nor a shell's variables reach it.

## Ingress and the firewall modes

Every machine's SSH port is open to the **ingress** the account service applies for this server: a set of source ranges. The server does not touch a cloud firewall; it reads the ingress through the service (`GET /v1/compute/ingress`) and, in two modes, writes it (`PUT`). The `isolated` profile holds only while the service also says the ingress is **exclusive**: the machines of this server admit inbound traffic only on their access ports and only from exactly those sources, no machine of any server or person can reach another, and the machines share no network with anything else that admits inbound traffic. The check covers ingress only: nothing denies a machine's outbound traffic.

The check passes when:

| Clause | Reason when it fails |
|---|---|
| The service answered the ingress read | `unreadable` |
| The sources equal the set the mode expects (see the modes) | `sources` |
| In `auto` mode, the service will apply the sources the server writes | `not_writable` |
| In `auto` mode, the server knows its public address | `address_unknown` |

The check runs when the server starts, in every periodic pass (once a minute), and before a create or start whose last result is more than 60 seconds old. While it fails:

- `shed create` and `shed start` (and an SSH connection that would wake a stopped shed) are refused with `503 FIREWALL_CHECK_FAILED`. The message says what was expected and what was found. When the check failed because the service could not be asked for the ingress (reason `unreadable`), they are refused with `503 COMPUTE_UNAVAILABLE` instead.
- `stop`, `delete`, `list`, `exec` and connections to running sheds work as before.
- The server still starts, so existing sheds can be listed and deleted (with one exception: `auto` mode against a service whose ingress is not writable stops the start, below).
- Nothing is stopped or deleted because of it.
- An `isolated` create or start is refused with the same code while the service does not say the ingress is exclusive, even when the sources match.

The result is written to the server log when it changes, and again every ten minutes while it fails:

```text
remote: firewall check passed (mode static, sources 203.0.113.7/32, exclusive true)
remote: firewall check FAILED (mode static, reason sources): the machines' ingress admits the wrong sources: expected sources [203.0.113.7/32], found sources [198.51.100.9/32]. Creates and starts are refused until it passes; stop, delete, list and exec are not affected
```

### Modes

| `firewall.mode` | Expected sources | The server writes the ingress |
|---|---|---|
| `static` (default) | The configured list | Yes, when they differ and the service's ingress is writable: it writes its configured sources, at most once every 5 minutes. When the service's ingress is managed elsewhere (the catalog says `ingress.writable: false`, or a write answers `INGRESS_NOT_WRITABLE`) it only compares, and a mismatch refuses creates and starts. |
| `auto` | The server's own public address, as one `/32` | Yes, under the conditions below. Refused at start-up when the service's ingress is not writable. |
| `any` | Not compared | Never. |

**static.** Set exactly one of `source_ranges` (a list in the config file) and `source_ranges_env` (the name of an environment variable holding the list, comma-separated). The variable keeps the address out of a config file that lives in a public repository; with neither set, the variable `SHED_REMOTE_SOURCE_RANGES` is read. The server refuses to start when the list is empty or holds anything but IPv4 CIDRs. A range wider than a `/24`, `0.0.0.0/0` included, is refused unless `acknowledge_any: true`. So is a range in private, carrier-grade NAT, loopback, link-local, benchmarking (`198.18.0.0/15`), protocol-assignment (`192.0.0.0/24`) or reserved space: machines are reached at public addresses, so a rule that admits such a range can admit one machine to another's SSH port. A service may refuse a source it cannot apply (`INGRESS_TOO_WIDE`): the write fails and the check keeps failing with that reason in its detail.

```yaml
remote:
  firewall:
    mode: static
    source_ranges_env: SHED_REMOTE_SOURCE_RANGES   # e.g. 203.0.113.7/32,198.51.100.0/24
```

**auto.** For a server whose public address changes. Each check asks every `echo_urls` service for the caller's address. A service must be `https`, answer `200` without a redirect, with at most 64 bytes that are exactly one IPv4 address. The address is believed only when at least two services report the same one, and never when it is in space a static source range could not name without an acknowledgement. One service alone can never move the ingress: each entry must be on a different host name (two entries on one host are refused when the config loads, and a host name outside ASCII must be written in its punycode form), and the services should be run by different operators.

When the believed address differs from the ingress's sources, the server writes that one `/32` and reads the ingress back. It writes at most once every 5 minutes. When the services have not agreed for 15 minutes, or have never agreed since the server started, creates and starts are refused (`address_unknown`) and the ingress is left alone.

```yaml
remote:
  firewall:
    mode: auto
    echo_urls:
      - https://<first-echo-service>/
      - https://<second-echo-service>/
```

**any.** The ingress may admit any address; its sources are not compared. It needs `acknowledge_any: true`, makes the `isolated` profile unavailable, and is refused beside service clients. `acknowledge_any: true` in static mode has the same two effects.

### Isolated sheds need an exclusive ingress

`exclusive` is the service's word, not the server's, and a service says `true` only when it can prove it. A Compute Engine adapter (the one cloud adapter the project's reference image targets) can prove it only for a single `/32` IPv4 source on a simple dedicated network (no Shared VPC, no Cloud NAT, IPv4 only, no regional policy rules, every service VM on the configured network); anything else is "cannot say", and `isolated` creates are refused. **So isolated sheds need `firewall.mode: auto`, or `static` with exactly one `/32` source.** A service client's policy `network: isolated` (Nocturn's) has the same need.

### When the check fails

Read the reason in the server log or in the `503` message, then:

| Reason | What to do |
|---|---|
| `sources` (static, service not writable) | The service's ingress is managed elsewhere: apply the configured sources there (the message says which). |
| `sources` (static or auto, writable) | The write was refused or has not happened yet: the message says why (a write at most every 5 minutes; the service refused the sources, usually too wide). |
| `not_writable` | `auto` against a service that will not apply the sources. Use `static` with the sources the service applies. |
| `address_unknown` | Check that the echo services answer from the server host. The message lists what each one said. |
| `unreadable` | The service could not be asked. Creates and starts answer `503 COMPUTE_UNAVAILABLE` (not `FIREWALL_CHECK_FAILED`): see [`COMPUTE_UNAVAILABLE`](#codes-and-reasons). The message carries the cause. |

The next periodic pass, at most a minute later, picks up the fix; no restart is needed unless the configured list changed.

## Keeping records and the service in step

The server reads the service when it starts and then once a minute. Each pass reads the catalog, runs the ingress check, lists this server's machines (one list) and corrects what differs, for sheds with no job in flight:

| The server finds | It does |
|---|---|
| A delete acknowledged more than two minutes ago whose machine still exists and has no delete job | Retries the delete. The shed stays hidden. |
| A `running` shed whose machine is stopped | Records it `stopped`, with reason `external`. A machine still on its way down is recorded `stopping` first. |
| A `stopped` shed whose machine is running | Records it `starting` and probes its agent. It never stops the machine. |
| A `running` or `stopped` shed whose machine no longer exists | Removes the shed, after an exact read of that one machine answers "not found". The list's deletions say why (`lease_ended`, `page`, `external`, `server`), and the log and audit event carry the cause. |
| A machine the service is deleting | Never answered with a stop. |
| A shed left `stopping` by a failed call | Stops it again. |
| A create kept `starting` (its outcome unknown, its guest not answering in time, or the ingress check failing on a later run) | Runs it again under the request id it stored, first reading the machine an earlier send may have made. |
| A machine of this server still `creating` 15 minutes after the service made it, with no record | Deletes it. It is a create whose record was lost (the `state_dir`, or a server that died before writing it); nothing would ever list, limit or delete it. A younger one is left alone. |

Each pass also runs the [ingress check](#ingress-and-the-firewall-modes). Its result decides only whether creates and starts are accepted.

The server deletes a machine only for a delete that was acknowledged, or a create abandoned as in the last row. The service's own deletes (a lease's end, the page) are the service's. A machine the server does not know and that is not an abandoned create is left alone.

## Server restarts

A server stop leaves every machine as it is: running sheds keep running and keep costing money until their leases end. At start-up the server reads the catalog and lists its machines from the service, and reconciles them with its record cache (`state_dir`):

- A ready machine that has no record is adopted under its own shed ID, with the owner, size, image and spec the service holds for it. A running one stays `starting` until its agent answers a probe.
- A record left `starting` or `stopping` resumes its job (so does one the periodic pass picks up after a read failed). A create resumed this way has lost its delivery bundle, which was held in memory only, and reads its record only: it never asks a source or probes the agent, whatever source would answer today, and never rewrites a step already recorded. When stage 1 was not recorded before the stop, no step runs, every step stays `owed`, and `shed provision <name>` finishes the shed. A step recorded `failed` is not tried again by the resumed job. A resumed job reads its create from the record, verbatim, and sends the request id it stored, so a create the service already holds is not made twice. A resumed create whose machine is gone is sent again only when no earlier send of it was accepted and the service's list names no deletion of its machine; otherwise the machine was made and deleted (its lease ended, a delete at the service), and the record is dropped, with the cause in the log and the audit log, as the periodic pass drops any shed whose machine the service deleted.
- A record whose machine is stopped but was `running` becomes `stopped` with reason `external`.
- A record whose machine is gone is dropped, unless the shed is in `error` or its create had not been accepted.
- A machine with a local delete marker, or one the service reports as deleting, is deleted or waited for.

**Old state is refused.** A record or delete marker written by an earlier server (before the account service supplied machines) stops the server at start, naming each one and what to do; see the [upgrade guide](../upgrades/remote-compute.md#old-cloud-sheds). Records and markers carry a schema number so this is recognised before they are decoded.

### Degraded start

If the service cannot give a complete list within the 2 minute start-up wait, the server logs it and **keeps going, degraded**:

- `GET` and `list` answer from the loaded records, and sessions to running sheds keep working (a record keeps the access option it last used).
- Creates, starts, stops, deletes and extends are refused `503 COMPUTE_UNAVAILABLE`. (`shed assign` is `501 FEATURE_UNSUPPORTED` on a cloud shed whatever the service's state.)
- The reconcile loop retries the list every 5 seconds. On the first complete list the backend becomes ready and runs what was waiting (ownership stamps, user origins, the resume of jobs).

A server whose config cannot work stops instead: `firewall.mode: auto` against a service whose ingress is not writable refuses to start, naming the mode to use.

### Kept creates

A create whose outcome is unknown (the reply never came) stays `starting (cloud_unreadable)` and is run again at every pass, under the same request id, until the service answers; a create whose guest never answers stays `starting (unreachable)` and is retried until it is deleted or its lease ends. Delete the shed to give up on either; the machine, if one exists, is deleted with it.

## Users

On a server with [`auth.users`](configuration.md#users) a remote shed belongs to a user like any other (see [Security › Ownership](security.md#ownership)). Ownership is **the machine's**: the account service records the person (`user`) or service client (`client`) a machine is for at create and never changes it. The server derives the shed's user from it.

- **Creating.** A person of the account service creates sheds for themselves. A service client's create is the client's, with its lease. A local admin (an admin of the server's own `auth.users`) creates none: `422 COMPUTE_POLICY_REFUSED`, reason `person`.
- **Adoption.** A machine the server adopts at start-up takes its user from the machine. A user that is not a valid name, or a record whose user disagrees with its machine, is reported with the user `!unresolved`: only an admin can reach it, and nobody can enter it, until a read of the machine finds the two in line again.
- **Assigning.** [`shed assign`](cli.md#shed-assign) is `501 FEATURE_UNSUPPORTED` on a cloud shed.
- **Who may do what.** A shed's own user, and any admin, may stop, start (the user only), delete and extend it; a service client may do all but extend (its lease is fixed).
- **Machines follow a name.** An operator who enrols another host under an old name gives it that name's machines; the service keys machines by the server's enrolled name.

A remote server has no snapshots and refuses `env_file` at load, so neither part of Ownership applies to it. A user's [credential source](security.md#one-machine-answers-for-each-user) is asked for [delivery](delivery.md) when their shed is created or provisioned, and for nothing else (`extensions.enabled` is refused with the remote backend, so there is no bus traffic).

## Credential delivery

A cloud shed gets [credential delivery](delivery.md) like any other: the user's source (their bound machine, or for a directory user the account service) is asked at create, and `shed provision` delivers again. The create answers `201 starting`, so the outcome is read from the shed document, polled as for any other field. A job never retries a delivery step recorded `failed`, a provision is refused (`409`) while a job for that shed is in flight, and a shed adopted at start-up is recorded with every step skipped and delivery `none`. The delivered files are on the machine's boot disk, so a stopped cloud shed holds them, and the lease's end deletes them with it.

## Not supported yet

| Item | Today |
|---|---|
| Agent install | Not built; see `agent_install` above. |
| Access kinds | `ssh` only; a service that offers only another kind gives sheds with reason `access`. |
| Architectures | The catalog's `arch` per size; the reference image is `amd64` only. |
| Rotating the link key without waiting | One key is sent at each create; see the [upgrade guide](../upgrades/remote-compute.md#rotating-the-link-key). |
