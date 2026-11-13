# Remote Backend

The `remote` backend runs each shed as its own virtual machine in a cloud project (Google Compute Engine today) instead of on the server's host. The shed-server keeps a record of every shed, reaches each VM over SSH as a dedicated login user, and talks to the in-VM agent through forwarded unix sockets. Clients use the same API, CLI and SSH entry point as for a local server.

A remote server differs from a Firecracker or VZ server in three ways:

- **Lifecycle is asynchronous.** Create, start, stop and delete return as soon as the new status is saved, and a job owned by the server does the cloud work. A shed passes through `starting` and `stopping`.
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
| `error` | The create failed. Only a delete leaves this state. | `capacity`, `quota`, `create_failed`, `host_key` |

| Reason | Meaning |
|---|---|
| `no_pin` | The VM has no pinned host key yet. |
| `unreachable` | The server cannot reach the VM's SSH port. |
| `capacity` | The zone had no room for the machine type. |
| `quota` | The cloud project's quota was reached. |
| `limit` | The server's `limits` (`max_running` or `max_sheds`) were reached. |
| `run_limit` | The shed ran for longer than the run limit and was stopped. |
| `external` | The VM was stopped outside shed. |
| `isolation_lost` | The network no longer isolates the shed. |
| `layout` | The VM's agent or guest layout is not compatible with this server. |
| `create_failed` | The create failed for another reason; the server log has the detail. |
| `host_key` | The host key the VM presented did not match the pinned key. |

## Calls and statuses

`POST /api/sheds` without `Accept: text/event-stream` returns **201 with `status: "starting"`** and the create continues in the background. Poll `GET /api/sheds/{name}` until the status is `running`. With `Accept: text/event-stream` the stream stays open until the shed settles: the `complete` event carries the running shed, and an `error` event reports a create that failed. Closing the stream ends the stream only, never the create.

| Call | `starting` | `running` | `stopping` | `stopped` | `error` |
|---|---|---|---|---|---|
| start | 200, no change | 409 | 200, queued | 200 `starting` | 409 with the reason |
| stop | 200, queued | 200 `stopping` | 200, no change | 409 | 409 |
| delete | 204 in every state; the shed disappears from lists at once | | | | |

"Queued" means the job waits for the cloud operation already running, then acts. A delete with `Accept: text/event-stream` stays open until the VM is destroyed.

Exec (`POST /api/sheds/{name}/exec`) and the Connect tunnel on a shed that is not running return **409 `SHED_NOT_RUNNING`**.

The CLI waits for you: `shed start` and `shed stop` poll until the shed settles (up to 180 s), and `shed attach`, `shed console`, `shed sync` and `shed tunnels` wait for a starting or stopping shed. SSH to the server (`ssh <shed>@<server>`) starts a stopped shed and waits up to 150 s, writing progress to stderr.

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
  agent_install: incompatible    # incompatible | always | never
  ssh:
    client_key_env: SHED_REMOTE_SSH_KEY             # base64 of the OpenSSH private key
    client_key_file_env: SHED_REMOTE_SSH_KEY_FILE   # or a path to it; exactly one must be set
  firewall:
    mode: static                 # static | auto | any
    source_ranges: []            # static: the firewall rule's sources
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
