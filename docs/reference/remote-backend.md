# Remote Backend

The `remote` backend runs each shed as its own virtual machine in a cloud project (Google Compute Engine today) instead of on the server's host. The shed-server keeps a record of every shed, reaches each VM over SSH as a dedicated login user, and talks to the in-VM agent through forwarded unix sockets. Clients use the same API, CLI and SSH entry point as for a local server.

A remote server differs from a Firecracker or VZ server in three ways:

- **Lifecycle is asynchronous.** A JSON create, start, stop or delete returns as soon as the new status is saved, and a job owned by the server does the cloud work. A shed passes through `starting` and `stopping`.
- **Some features do not exist.** There are no host directories, server mounts, snapshots, reset, image store or egress profiles. Requests for them are refused before anything is created.
- **Isolation is by network topology.** The `isolated` network profile is accepted because the network gives a shed no route to another shed or to the server host.

## Statuses

A shed's `status` is one of five values. `status_reason` explains a status where the table lists reasons; it is empty otherwise.

| `status` | Meaning | `status_reason` values |
|---|---|---|
| `starting` | A create or start is in progress, or the server adopted the VM and has not yet reached its agent. | empty, `no_pin`, `unreachable` |
| `running` | The agent answered a probe. | empty |
| `stopping` | A stop is in progress. | empty |
| `stopped` | The VM is off and its disk is kept. | empty, `capacity`, `quota`, `limit`, `run_limit`, `external`, `isolation_lost`, `layout` |
| `error` | The create failed. Only a delete leaves this state. | `capacity`, `quota`, `create_failed`, `host_key`, `layout` |

| Reason | Meaning |
|---|---|
| `no_pin` | The VM has no pinned host key yet. |
| `unreachable` | The server cannot reach the VM's SSH port. |
| `capacity` | The zone had no room for the machine type. |
| `quota` | The cloud project's quota was reached. |
| `limit` | The server's `limits` (`max_running` or `max_sheds`) were reached. |
| `run_limit` | The shed ran for as long as its run limit and the cloud stopped it. |
| `external` | The VM was stopped outside shed. |
| `isolation_lost` | The network no longer isolates the shed. Not reported yet. |
| `layout` | The VM's agent or guest layout is not compatible with this server. |
| `create_failed` | The create failed for another reason; the server log has the detail. |
| `host_key` | The host key the VM presented did not match the pinned key. |

The run limit is enforced by the cloud: each VM is created with a maximum run duration (the larger of `max_run_duration` and the owning client's `max_age`) after which the cloud stops it. The server notices a VM stopped by the run limit or from outside shed at its next [pass over the cloud](#keeping-records-and-the-cloud-in-step), at most about a minute later, and until then lists the shed as `running`. It records the shed as `stopped` with reason `run_limit` when the VM stopped within two minutes of its run limit, and with reason `external` otherwise. `isolation_lost` is defined but no code path sets it yet.

`shed list -v` shows the reason beside the status (`stopped (external)`), and `shed list --json` and the API carry it as `status_reason`.

## Calls and statuses

`POST /api/sheds` without `Accept: text/event-stream` returns **201 with `status: "starting"`** and the create continues in the background. The reply waits first for the cloud to accept the new VM, for at most 15 s, so that a quota or capacity refusal arriving that early is answered as a 503 and leaves no shed; measured JSON creates answered after 11 to 15 s. The image lookup before it is bounded at 10 s, so the reply always comes within 25 s. Poll `GET /api/sheds/{name}` until the status is `running`. With `Accept: text/event-stream` the stream stays open until the shed settles: the `complete` event carries the running shed, and an `error` event reports a create that failed. Closing the stream ends the stream only, never the create.

| Call | `starting` | `running` | `stopping` | `stopped` | `error` |
|---|---|---|---|---|---|
| start | 200, no change | 409 | 200, queued | 200 `starting` | 409 with the reason |
| stop | 200, queued | 200 `stopping` | 200, no change | 409 | 409 |
| delete | 204 (JSON) in every state; the shed disappears from lists at once | | | | |

"Queued" means the job waits for the cloud operation already running, then acts.

A delete without `Accept: text/event-stream` answers as soon as the shed is hidden (about 3 s measured, and about 10 s when the VM cannot be labelled at once because its create or a stop is still in flight); the VM is destroyed in the background, about 30 s measured. With `Accept: text/event-stream`, which is what `shed delete` sends, the shed is hidden just as fast but the stream stays open until the VM is gone, so the command returns only then (29 to 34 s measured) and a create right after it never meets the old VM. A delete sent while the shed's create is still asking the cloud for its VM waits for that VM to appear and then destroys it, which took about two and a half minutes when measured. A destroy that fails is retried by the periodic pass (see below).

Exec (`POST /api/sheds/{name}/exec`) and the Connect tunnel on a shed that is not running return **409 `SHED_NOT_RUNNING`**.

The CLI waits for you: `shed start` and `shed stop` poll until the shed settles (up to 180 s), `shed delete` returns once the VM is gone, and `shed attach`, `shed console`, `shed sync` and `shed tunnels` wait for a starting or stopping shed. SSH to the server (`ssh <shed>@<server>`) starts a stopped shed and waits up to 150 s, writing progress to stderr.

## Keeping records and the cloud in step

The server reads the cloud when it starts and then once a minute. Each pass lists the environment's VMs and corrects what differs, for sheds with no job in flight:

| The server finds | It does |
|---|---|
| A delete acknowledged more than two minutes ago whose VM still exists and has no delete job | Retries the destroy. The shed stays hidden. |
| A `running` shed whose VM is stopped | Records it `stopped`, with reason `run_limit` or `external`. A VM still on its way down is recorded `stopping` first. |
| A `stopped` shed whose VM is running | Records it `starting` and probes its agent. It never stops the VM. |
| A `running` or `stopped` shed whose VM no longer exists | Removes the shed, after a read of that one VM confirms it. `shed delete` on it then answers "not found". |
| A shed left `stopping` by a failed cloud call | Stops it again. |
| A create that failed | Destroys its VM, retrying until it is gone. The shed stays, in `error`, until it is deleted. |

A pass destroys a VM only for a delete that was acknowledged or a create that failed. A VM it does not know (another environment's, or one with no record) is logged and left alone.

Measured on the development server: a VM stopped with the cloud's own tools was listed `stopped (external)` 10 s after it finished stopping, and `shed start` brought it back in 42 s. A VM deleted with the cloud's tools lost its record 2 to 12 s after it was gone. A stopped shed's VM started with the cloud's tools was listed `starting` at the next pass and `running` 3 s later. A `shed delete` sent in between, for a shed whose VM was already gone, succeeded in under a second.

A VM that cannot be destroyed still runs, so two minutes after its delete it counts against `max_running` until it is gone. A delete that is merely slow (one that raced its own create, above) counts the same way for as long as it lasts beyond the two minutes. `SERVER_LIMIT_REACHED` then says how many such VMs there are; the server log has the cloud's error.

The `--repo` clone of a create runs with `GIT_TERMINAL_PROMPT=0`, for at most 5 minutes an attempt and at most 3 attempts. A clone that still fails is a warning, as on the other backends: the shed runs without the repository.

## Error codes

| Code | HTTP | When |
|---|---|---|
| `SERVER_LIMIT_REACHED` | 503 | The server's `max_running` or `max_sheds` limit is reached. |
| `CLOUD_QUOTA_EXCEEDED` | 503 | The cloud project's quota is reached. |
| `ZONE_CAPACITY_UNAVAILABLE` | 503 | The zone has no room for the machine type. |
| `FEATURE_UNSUPPORTED` | 422 | A create asks for `local_dir`, `add_dirs`, a non-empty `mounts`, `from_snapshot` or egress profiles. |
| `FEATURE_UNSUPPORTED` | 501 | A snapshot, reset, image-management or egress write route is called. |
| `INVALID_REQUEST` | 400 | No configured size has the requested `cpus` and `memory_mb`. |

None of the three 503 refusals is ever a 409, because a client reads a 409 on create as "the shed already exists". A refusal that arrives after the create was accepted leaves the shed in `error` (create) or `stopped` (start) with the matching reason.

## Unsupported features

| Feature | Create field or route | Answer |
|---|---|---|
| Host directories | `local_dir`, `add_dirs` (`--local-dir`, `--add-dir`) | 422 |
| Server mounts | non-empty `mounts` (`--mounts`) | 422; an omitted `mounts` stores `none` |
| Snapshots | `from_snapshot`, `POST /api/snapshots`, `GET` and `DELETE /api/snapshots/{name}` | 422 or 501; `GET /api/snapshots` lists nothing |
| Reset | `POST /api/sheds/{name}/reset` | 501 |
| Image store | image inspect, tag, pull, push, delete and prune | 501; `GET /api/images` lists the configured aliases |
| Egress profiles | `egress` other than `["off"]`, egress set, off and profile writes | 422 or 501 |

The CLI refuses `--local-dir`, `--add-dir`, `--from-snapshot`, `--egress` and named `--mounts` before sending anything when `/api/info` reports `backend: "remote"`.

## Server information

`GET /api/info` on a remote server reports `backend: "remote"`, the remote `default_image`, and in `features` the tokens `async_lifecycle` and `network_profiles` (with `service_clients` and `http_exec` as on any server). It also reports `isolation: "topology"`. No response names the cloud project, zone or any address.

## Sizes

A remote server offers named sizes instead of arbitrary CPU and memory values. A create with neither `cpus` nor `memory_mb` gets `default_size`; otherwise it gets the smallest size that has at least both. The shed's record reports that size's CPUs and memory, and a service client's caps are checked against it. `upper_size_bytes` (`--upper-size`) sets the boot disk size, rounded up to whole GB, between 10 and 500 GB.

## Configuration

The config file holds no project, zone, address or key. Every `*_env` field names a process environment variable that the server reads once at start-up; start-up fails, naming the variable, when one is missing.

```yaml
default_backend: remote          # `detect` never picks it
remote:
  provider: gce                  # gce | fake (fake needs allow_test_drivers)
  transport: ssh                 # ssh | local (local needs allow_test_drivers)
  allow_test_drivers: false
  environment: <env-name>        # required: 1-20 of a-z, 0-9 and -, starts with a letter
  state_dir: <path>              # record cache and delete markers (default <state dir>/remote)
  default_image: <image-name>    # required
  image_aliases:
    cloud: <image-name>
  default_size: standard
  sizes:
    standard: {machine_type: e2-standard-2, cpus: 2, memory_mb: 8192, arch: amd64}
    large:    {machine_type: e2-standard-8, cpus: 8, memory_mb: 32768, arch: amd64}
  disk_gb: 50
  limits: {max_running: 2, max_sheds: 8}
  max_run_duration: 24h          # a client's shed gets the larger of this and its max_age
  agent_dir: <path>              # holds shed-agent-linux-<arch> (default <state dir>/agent)
  agent_install: incompatible    # incompatible | always | never; only never is built (see below)
  ssh:
    client_key_env: SHED_REMOTE_SSH_KEY             # base64 of the OpenSSH private key
    client_key_file_env: SHED_REMOTE_SSH_KEY_FILE   # or a path to it; exactly one must be set
  firewall:
    mode: static                 # static | auto | any; only static is supported (see below)
    source_ranges: []            # static: the firewall rule's sources (not checked yet)
    acknowledge_any: false       # must be true for mode: any
    echo_urls: []                # auto only
  gce:
    project_env: SHED_GCE_PROJECT
    zone_env: SHED_GCE_ZONE
    network_env: SHED_GCE_NETWORK
    subnet_env: SHED_GCE_SUBNET
    credentials_env: GOOGLE_CREDENTIALS_BASE64        # base64 of the service account JSON key
    credentials_file_env: SHED_GCE_CREDENTIALS_FILE   # or a path to it; exactly one must be set
```

Only `environment` and `default_image` are required; every other field has the default shown. With `default_backend: remote` the server refuses to load when the config also has a `firecracker:` or `vz:` block, `env_file`, top-level `mounts`, `egress.enabled`, `network.allow_cidrs`, `extensions.enabled`, a client `policy.server_mounts`, a client `policy.images` entry that is not an `image_aliases` name, or `firewall.mode: any` beside service clients or an `isolated` default profile. A `remote:` block under any other backend is refused too.

Two fields are accepted by the loader but not built yet:

| Field | What happens today |
|---|---|
| `agent_install` | Every value behaves as `never`: the server does not copy an agent into a VM. A VM whose agent reports an incompatible protocol or guest layout fails its create (`error`, reason `layout`) or start (`stopped`, reason `layout`). Bake a new image instead (`make gce-image`). |
| `firewall.mode` | Use `static`. The server neither reads nor changes the firewall rule in any mode: create the rule once with `scripts/remote/gce-setup.sh` and keep its source the server's public address. `source_ranges` is not compared with the rule, `auto` does nothing, and `any` only makes the `isolated` profile unavailable. |

## Server restarts

A server stop leaves every VM as it is: running sheds keep running and keep costing money. At start-up the server lists the cloud VMs of its `environment` and reconciles them with its record cache (`state_dir`):

- A VM labelled ready that has no record is adopted under its own shed ID.
- A record left `starting` or `stopping` resumes its job.
- A record whose VM is stopped but was `running` becomes `stopped` with reason `run_limit` or `external`.
- A record whose VM is gone is dropped, unless the shed is in `error` or its create had not reached the cloud.
- Only a VM labelled `deleting`, or one with a local delete marker, is deleted. Nothing else is ever removed.

## Not supported yet

| Item | Today |
|---|---|
| Agent install | Not built; see `agent_install` above. |
| Firewall `auto` mode and the network check | Not built; see `firewall.mode` above. |
| Zones | One zone per server, from its environment variable. |
| Architectures | `amd64` images only. |
