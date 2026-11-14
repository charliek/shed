# Remote Backend

The `remote` backend runs each shed as its own virtual machine in a cloud project (Google Compute Engine today) instead of on the server's host. The shed-server keeps a record of every shed, reaches each VM over SSH as a dedicated login user, and talks to the in-VM agent through forwarded unix sockets. Clients use the same API, CLI and SSH entry point as for a local server.

A remote server differs from a Firecracker or VZ server in three ways:

- **Lifecycle is asynchronous.** A JSON create, start, stop or delete returns as soon as the new status is saved, and a job owned by the server does the cloud work. A shed passes through `starting` and `stopping`.
- **Some features do not exist.** There are no host directories, server mounts, snapshots, reset, image store or egress profiles. Requests for them are refused before anything is created.
- **Isolation is by network topology.** The `isolated` network profile is accepted because the network gives a shed no route to another shed and no private route to the server host. Outbound traffic is not filtered: a shed has a public address and reaches any public address, including a port the server host exposes to the internet.

## Statuses

A shed's `status` is one of five values. `status_reason` explains a status where the table lists reasons; it is empty otherwise.

| `status` | Meaning | `status_reason` values |
|---|---|---|
| `starting` | A create or start is in progress, or the server adopted the VM and has not yet reached its agent. | empty, `no_pin`, `unreachable` |
| `running` | The agent answered a probe. | empty |
| `stopping` | A stop is in progress. | empty |
| `stopped` | The VM is off and its disk is kept. | empty, `capacity`, `quota`, `limit`, `run_limit`, `external`, `isolation_lost`, `layout`, `firewall` |
| `error` | The create failed. Only a delete leaves this state. | `capacity`, `quota`, `create_failed`, `host_key`, `layout`, `firewall` |

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
| `firewall` | The [firewall check](#the-firewall-check) was failing when the server was about to ask the cloud for the VM, so it did not. |

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

A delete without `Accept: text/event-stream` answers as soon as the shed is hidden (about 3 s measured, and about 10 s when the VM cannot be labelled at once because its create or a stop is still in flight); the VM is destroyed in the background, about 30 s measured. With `Accept: text/event-stream`, which is what `shed delete` sends, the shed is hidden just as fast but the stream stays open until the VM is gone, so the command returns only then (29 to 34 s measured) and a create right after it never meets the old VM. A delete sent while the shed's create is still asking the cloud for its VM waits for that VM to appear and then destroys it, which took about two and a half minutes when measured. A destroy that fails is retried by the periodic pass (see below). The shed is hidden all the same; a streamed delete then ends with an error event (`shed delete` prints `the delete is recorded but the VM could not be destroyed yet; the server keeps trying` and exits 1), and a JSON delete has already answered 204.

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
| A VM of this environment still labelled `creating` 15 minutes after the cloud made it, with no record | Destroys it. It is a create whose record was lost (the `state_dir`, or a server that died before writing it); nothing would ever list, limit or delete it. A younger one is left alone. |

Each pass also runs the [firewall check](#the-firewall-check). Its result decides only whether creates and starts are accepted.

A pass destroys a VM only for a delete that was acknowledged, a create that failed, or a create abandoned as in the last row. Any other VM it does not know (another environment's, one with labels of a schema it cannot read, or a finished one with no record) is left alone.

Measured on the development server: a VM stopped with the cloud's own tools was listed `stopped (external)` 10 s after it finished stopping, and `shed start` brought it back in 42 s. A VM deleted with the cloud's tools lost its record 2 to 12 s after it was gone. A stopped shed's VM started with the cloud's tools was listed `starting` at the next pass and `running` 3 s later. A `shed delete` sent in between, for a shed whose VM was already gone, succeeded in under a second.

A VM that cannot be destroyed still runs, so two minutes after its delete it counts against `max_running` until it is gone. A delete that is merely slow (one that raced its own create, above) counts the same way for as long as it lasts beyond the two minutes. `SERVER_LIMIT_REACHED` then says how many such VMs there are; the server log has the cloud's error.

Measured with a VM put under the cloud's deletion protection: `shed delete` returned after 4 s with the error above, the VM kept running with its delete marker, a create 2 min 5 s later was refused at `max_running`, and each pass from then on retried the destroy. The VM was gone 80 s after the protection was cleared.

Measured for a create that fails late, from a stock image with no shed agent: the create ended in `error` (`create_failed`) after 324 s, which is the 5 minute wait for the agent, and its VM and disk were gone 58 s later with no delete sent. For the run limit, with `max_run_duration: 10m`: the cloud stopped the VM at the limit, the shed was listed `stopped (run_limit)` 63 s after it, and `shed start` (44 s) gave the VM a new limit counted from that start.

The `--repo` clone of a create runs with `GIT_TERMINAL_PROMPT=0`, for at most 5 minutes an attempt and at most 3 attempts. A clone that still fails is a warning, as on the other backends: the shed runs without the repository.

## Error codes

| Code | HTTP | When |
|---|---|---|
| `SERVER_LIMIT_REACHED` | 503 | The server's `max_running` or `max_sheds` limit is reached. |
| `CLOUD_QUOTA_EXCEEDED` | 503 | The cloud project's quota is reached. |
| `ZONE_CAPACITY_UNAVAILABLE` | 503 | The zone has no room for the machine type. |
| `FIREWALL_CHECK_FAILED` | 503 | The [firewall check](#the-firewall-check) is failing. The message names the rule, what was expected and what was found. |
| `FEATURE_UNSUPPORTED` | 422 | A create asks for `local_dir`, `add_dirs`, a non-empty `mounts`, `from_snapshot` or egress profiles. |
| `FEATURE_UNSUPPORTED` | 501 | A snapshot, reset, image-management or egress write route is called. |
| `INVALID_REQUEST` | 400 | No configured size has the requested `cpus` and `memory_mb`. |

None of the 503 refusals is ever a 409, because a client reads a 409 on create as "the shed already exists". A quota, capacity or limit refusal that arrives after the create was accepted leaves the shed in `error` (create) or `stopped` (start) with the matching reason. `FIREWALL_CHECK_FAILED` is answered on the request, before anything is created or started; a client treats it like the others: wait and try again. The server asks the check again just before it sends the cloud an insert or a start, because that can be long after the request (a create resumed after a server restart, a start queued behind a stop). If the check fails then, nothing is sent and the shed is left in `error` (create) or `stopped` (start) with reason `firewall`.

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
  max_run_duration: 24h          # 10m to 2880h; a client's shed gets the larger of this and its max_age
  agent_dir: <path>              # holds shed-agent-linux-<arch> (default <state dir>/agent)
  agent_install: incompatible    # incompatible | always | never; only never is built (see below)
  ssh:
    client_key_env: SHED_REMOTE_SSH_KEY             # base64 of the OpenSSH private key
    client_key_file_env: SHED_REMOTE_SSH_KEY_FILE   # or a path to it; exactly one must be set
  firewall:                      # see "The firewall check"
    mode: static                 # static | auto | any
    source_ranges: []            # static: the rule's sources, as IPv4 CIDRs, or
    source_ranges_env: SHED_REMOTE_SOURCE_RANGES   # static: a variable holding them, comma-separated
    acknowledge_any: false       # must be true for mode: any, and for a static range wider than a /24
    echo_urls: []                # auto: at least two https services, on different hosts, that answer with the caller's address
  gce:
    project_env: SHED_GCE_PROJECT
    zone_env: SHED_GCE_ZONE
    network_env: SHED_GCE_NETWORK
    subnet_env: SHED_GCE_SUBNET
    credentials_env: GOOGLE_CREDENTIALS_BASE64        # base64 of the service account JSON key
    credentials_file_env: SHED_GCE_CREDENTIALS_FILE   # or a path to it; exactly one must be set
```

Only `environment` and `default_image` are required; every other field has the default shown. With `default_backend: remote` the server refuses to load when the config also has a `firecracker:` or `vz:` block, `env_file`, top-level `mounts`, `egress.enabled`, `network.allow_cidrs`, `extensions.enabled`, a client `policy.server_mounts`, a client `policy.images` entry that is not an `image_aliases` name, or `firewall.mode: any` (or `firewall.acknowledge_any: true` in any mode) beside service clients or an `isolated` default profile. A `remote:` block under any other backend is refused too.

One field is accepted by the loader but not built yet:

| Field | What happens today |
|---|---|
| `agent_install` | Every value behaves as `never`: the server does not copy an agent into a VM. A VM whose agent reports an incompatible protocol or guest layout fails its create (`error`, reason `layout`) or start (`stopped`, reason `layout`). Bake a new image instead (`make gce-image`). |

## The firewall check

Every shed's SSH port is open to whatever one cloud firewall rule admits: `shed-<environment>-ssh`, which `scripts/remote/gce-setup.sh` creates. The `isolated` profile holds only while no other rule lets VMs reach each other. The check covers ingress only: no rule denies a shed's outbound traffic, so nothing in the cloud keeps a shed from a public address of the server host. The server never creates or deletes a rule. It reads the rule and its network, and refuses to create or start a VM while they are not as expected.

The check passes when all of these hold:

| Clause | Reason when it fails |
|---|---|
| The rule `shed-<environment>-ssh` exists | `rule_missing` |
| It is on the network the server puts VMs on | `rule_network` |
| It is an ingress rule | `rule_direction` |
| It is enabled | `rule_disabled` |
| It allows `tcp:22` and nothing else | `rule_allow` |
| It targets exactly the network tag `shed-<environment>` | `rule_target` |
| It admits by address only (no source tags or service accounts) | `rule_source_kind` |
| Its source ranges equal the expected set (see the modes) | `rule_sources` |
| No other enabled ingress allow rule on the network applies to the tag `shed-<environment>` or to every instance | `stray_rule` |
| The network is not the project's `default` network | `default_network` |
| The rules could be read | `unreadable` |
| In `auto` mode, the server knows its public address | `address_unknown` |

Another environment's rule on a shared network targets its own tag and is not a stray. Disabled rules, deny rules, egress rules and rules that target only a service account are ignored.

The check runs when the server starts, in every periodic pass (once a minute), and before a create or start whose last result is more than 60 seconds old. While it fails:

- `shed create` and `shed start` (and an SSH connection that would wake a stopped shed) are refused with `503 FIREWALL_CHECK_FAILED`. The message says which rule, what was expected and what was found.
- `stop`, `delete`, `list`, `exec` and connections to running sheds work as before. When the rule itself no longer admits the server, the SSH connections the server already holds keep working (a rule change does not cut an open connection), so `exec` works until one drops; `stop`, `delete` and `list` never need the rule.
- The server still starts, so existing sheds can be listed and deleted.
- Nothing is stopped or deleted because of it.

A change to the rule is seen at the next check, so for up to a minute after it a create or start is still judged by the last result. Measured: a rule changed by hand was reported failing 29 to 53 s later, and a repaired one passing within a minute, with no restart.

The result is written to the server log when it changes, and again every ten minutes while it fails:

```text
remote: firewall check passed (mode static, rule shed-<env>-ssh, sources 203.0.113.7/32)
remote: firewall check FAILED (mode static, reason rule_sources): firewall rule shed-<env>-ssh admits the wrong sources: expected source ranges [203.0.113.7/32], found source ranges [198.51.100.9/32]. Creates and starts are refused until it passes; stop, delete, list and exec are not affected
```

### Modes

| `firewall.mode` | Expected source ranges | The server changes the rule |
|---|---|---|
| `static` (default) | The configured list | Never. Its role can only list rules. |
| `auto` | The server's own public address, as one `/32` | Yes: only the rule's source ranges, only under the conditions below. |
| `any` | Not compared | Never. |

**static.** Set exactly one of `source_ranges` (a list in the config file) and `source_ranges_env` (the name of an environment variable holding the list, comma-separated). The variable keeps the address out of a config file that lives in a public repository; with neither set, the variable `SHED_REMOTE_SOURCE_RANGES` is read. The server refuses to start when the list is empty or holds anything but IPv4 CIDRs. A range wider than a `/24`, `0.0.0.0/0` included, is refused unless `acknowledge_any: true`. So is a range in private, carrier-grade NAT, loopback, link-local, benchmarking (`198.18.0.0/15`), protocol-assignment (`192.0.0.0/24`) or reserved space: sheds have private addresses, so a rule that admits such a range can admit one shed to another's SSH port, and the server always connects to a shed's public address.

```yaml
remote:
  firewall:
    mode: static
    source_ranges_env: SHED_REMOTE_SOURCE_RANGES   # e.g. 203.0.113.7/32,198.51.100.0/24
```

**auto.** For a server whose public address changes. Each check asks every `echo_urls` service for the caller's address. A service must be `https`, answer `200` without a redirect, with at most 64 bytes that are exactly one IPv4 address. The address is believed only when at least two services report the same one, and never when it is in space a static source range could not name without an acknowledgement (private, loopback, link-local, unspecified, multicast, carrier-grade NAT, benchmarking, protocol-assignment or reserved). One service alone can never move the rule: each entry must be on a different host name (two entries on one host are refused when the config loads, and a host name outside ASCII must be written in its punycode form so that it has one spelling), and the services should be run by different operators.

When the believed address differs from the rule's source ranges, and every other clause of the check passes, the server patches the rule's source ranges to that one `/32`, waits for the cloud, reads the rule back and logs the old and new sources. It patches at most once every 5 minutes, and never when the rule or the network fails any other clause. When the services have not agreed for 15 minutes, or have never agreed since the server started, creates and starts are refused (`address_unknown`) and the rule is left alone.

```yaml
remote:
  firewall:
    mode: auto
    echo_urls:
      - https://<first-echo-service>/
      - https://<second-echo-service>/
```

Auto mode needs two more custom roles, which `gce-setup.sh --print --firewall-auto` prints. They are not part of the base role:

| Role | Permissions | Bound |
|---|---|---|
| `<role-id>FirewallRule` | `compute.firewalls.get`, `compute.firewalls.update` | With an IAM condition on `resource.name`, so it holds for the one rule `shed-<environment>-ssh` |
| `<role-id>FirewallNet` | `compute.networks.updatePolicy`, `compute.globalOperations.get` | Without a condition (IAM conditions cannot name a network or an operation); alone these change no rule |

A holder of both can change any field of that one rule, not only its sources: Google has no narrower permission. A stolen key could open the sheds' ports to any address, so use `auto` only where the address really changes. The condition comes from Google's IAM documentation, which lists firewall rules among the resources a condition can name. It was run once by hand on the development project: the binding was accepted, the server patched its own rule, and its key was refused an update, a create and a delete of another rule. That project's base role already held `compute.firewalls.get` and `compute.globalOperations.get`, so the run does not show that `compute.networks.updatePolicy` is needed, or that the read of the rule works through the condition alone. Confirm it in your project by checking that the server's key cannot update another rule.

Measured on the development server: a rule whose sources were changed by hand was patched back 45 and 47 s later (at the next pass). With its port changed as well it was left alone for six minutes, creates refused with `rule_allow`, and its sources were patched 45 s after the port was put back. Sources changed again a minute after a patch were not repaired at the next pass, and the message said so: `not repaired yet: the rule was last patched 1m0s ago and is patched at most once every 5m0s`.

**any.** The rule may admit any address; its sources are not compared and the rest of the check still runs. It needs `acknowledge_any: true`, makes the `isolated` profile unavailable, and is refused beside service clients. `acknowledge_any: true` in static mode has the same two effects.

### When the check fails

Read the reason in the server log or in the `503` message, then:

| Reason | What to do |
|---|---|
| `rule_sources` (static) | Your address changed, or someone edited the rule. Update the rule (`gcloud compute firewall-rules update shed-<env>-ssh --source-ranges=<address>/32`) and the server's configured list so they match, and restart the server if the list changed. |
| `rule_sources` (auto) | The message says why the rule was not repaired: the last patch was under 5 minutes ago (wait), the patch was refused (the extra roles are missing), or the services did not agree in that check. |
| `address_unknown` | Check that the echo services answer from the server host. The message lists what each one said. |
| `rule_missing`, `rule_network`, `rule_direction`, `rule_disabled`, `rule_allow`, `rule_target`, `rule_source_kind` | Recreate the rule as `gce-setup.sh --print` shows it. |
| `stray_rule` | Delete or disable the named rule, or give it a target tag the sheds do not carry. |
| `default_network` | Move the environment to its own custom-mode network (`gce-setup.sh --print`). |
| `unreadable` | The message carries the cloud's error: usually the credentials, the role (`compute.firewalls.list`) or the network. |

The next periodic pass, at most a minute later, picks up the fix; no restart is needed unless the configured list changed.

The check covers VPC firewall rules. It does not read hierarchical or network firewall policies, VPC peering or routes. The network and subnet must be in the project the sheds are in: a network of another project (a Shared VPC) is refused when the server starts, because the check reads that one project's rules.

## Server restarts

A server stop leaves every VM as it is: running sheds keep running and keep costing money. At start-up the server lists the cloud VMs of its `environment` and reconciles them with its record cache (`state_dir`):

- A VM labelled ready that has no record is adopted under its own shed ID.
- A record left `starting` or `stopping` resumes its job.
- A record whose VM is stopped but was `running` becomes `stopped` with reason `run_limit` or `external`.
- A record whose VM is gone is dropped, unless the shed is in `error` or its create had not reached the cloud.
- A VM labelled `deleting`, or one with a local delete marker, is deleted. So is a VM still labelled `creating` 15 minutes after the cloud made it that has no record: a create nobody will finish. Nothing else is ever removed.

## Not supported yet

| Item | Today |
|---|---|
| Agent install | Not built; see `agent_install` above. |
| Firewall policies and peering | The firewall check reads VPC firewall rules only; see [The firewall check](#the-firewall-check). |
| Zones | One zone per server, from its environment variable. |
| Architectures | `amd64` images only. |
