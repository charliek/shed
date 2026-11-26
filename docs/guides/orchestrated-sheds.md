# Orchestrated Sheds

An orchestrator, such as a CI system or an agent runner, can drive sheds on a
shed-server it does not administer. It authenticates as a **service client**: an
mTLS identity that can create sheds within a policy, run commands in them over
HTTPS, and delete them, and can see or touch nothing else on the server. Its
sheds never get the credential bus, get no server mounts unless its policy
names some, and by default, on Firecracker, get an `isolated` network: the
internet, the server's `network.allow_cidrs` and the gateway's own ports for
its egress proxy and server mounts, not the host, the LAN or other sheds. VZ has
no isolation, and the remote backend isolates by network topology; the rule is
in [Security › Network
profiles](../reference/security.md#network-profiles).

This guide sets one up. The model and its limits are in [Security › Service
clients](../reference/security.md#service-clients); every key is in
[Configuration › Service clients](../reference/configuration.md#service-clients).

## Before you start

- The server runs `auth.mode: mtls` (see [Security
  Configuration](security-configuration.md#case-3-networked-mtls-facing-the-network)).
  Service clients do not exist in `open` mode.
- Nothing about a client changes with a [user list](multi-user-server.md):
  the same certificate, routes and policy work on a server with or without
  `auth.users`. A client name may not also be a user name.
- For the `isolated` network profile, the server uses the Firecracker backend
  and can run `nft` (the deb package's root unit can). A VZ server works too, with
  `network: open` in the client's policy. On the remote backend the network
  provides `isolated` instead: see [On a directory server or the remote
  backend](#on-a-directory-server-or-the-remote-backend).
- Every client request is written to the audit log, which is turned on
  automatically.

## 1. Declare the client

Add the client under `auth.clients` in `server.yaml` and restart `serve`:

```yaml
auth:
  mode: mtls
  ssh:
    github_users: [alice-gh]
  clients:
    nocturn:
      cert_max_ttl: 2160h          # longest certificate client issue signs (90 days)
      policy:
        name_prefix: "nocturn-"    # every shed it creates starts with this
        max_sheds: 4               # owned sheds at once, running or stopped
        max_age: 30h               # the server deletes an owned shed older than this
        network: isolated          # the default; must be open on VZ
        server_mounts: []          # the default: no host directories
        images: []                 # the default: only the server's default_image
        caps: {cpus: 4, memory_mb: 8192}
```

To let the client pick a different image, add an alias under the backend's
`image_aliases` and name the alias in `images`. A typo anywhere under
`auth.clients` stops the server from starting, with the key named.

Check what the server will enforce:

```bash
sudo shed-server client list
```

## 2. Issue a bundle

On the shed host, as the user that runs `serve` and with the same config:

```bash
sudo shed-server client issue nocturn --out nocturn-bundle.json
```

`sudo` matches the deb install, whose unit runs `serve` as root with
`/etc/shed/server.yaml`. A brew install on macOS runs `serve` as your own user
with `--config "$(brew --prefix)/etc/shed/server.yaml"`, so run the command
without `sudo` and with that `--config`: as root it would use `/etc/shed` as
its state directory instead of `~/.shed`, and sign with a different CA than the
one `serve` trusts.

The file (mode `0600`) holds one JSON object: the client name, `auth_mode`,
`https_port`, the server's `tls_cert_fingerprint`, the PEM `client_cert` and
`client_key`, the `cert_serial` and `expires_at`. `--ttl 720h` issues a
shorter-lived certificate; it cannot exceed `cert_max_ttl`.

## 3. Store it

The bundle holds a private key: treat it as a secret. Move it to the
orchestrator's secret store (an environment variable holding the JSON, a secret
manager) and delete the file from the shed host. Whoever holds it has root in
that client's sheds (see [What a leaked bundle gives](#what-a-leaked-bundle-gives)).

## 4. Call the API

The client talks to the server's HTTPS port with its certificate, and pins the
server's self-signed certificate by `tls_cert_fingerprint`. Step 2 wrote the
bundle as root with mode `0600`, so run these as a user who can read it: as
root, or after you have put the bundle where the orchestrator's own user reads
it. With `curl` and `jq`:

```bash
jq -r .client_cert nocturn-bundle.json > client.crt
jq -r .client_key  nocturn-bundle.json > client.key
chmod 600 client.key

# Fetch the server certificate and stop unless it matches the bundle's pin.
openssl s_client -connect shed-host:8443 -cert client.crt -key client.key </dev/null 2>/dev/null \
  | openssl x509 > server.pem
got="sha256:$(openssl x509 -in server.pem -noout -fingerprint -sha256 | cut -d= -f2 | tr -d : | tr A-F a-f)"
want="$(jq -r .tls_cert_fingerprint nocturn-bundle.json)"
[ "$got" = "$want" ] || { echo "server certificate does not match the pin" >&2; exit 1; }

# Pin the verified certificate's public key for every later call.
PIN="sha256//$(openssl x509 -in server.pem -noout -pubkey \
  | openssl pkey -pubin -outform der | openssl dgst -sha256 -binary | base64)"
```

Every call then passes `--pinnedpubkey "$PIN"`, so curl refuses any server
whose key is not the one just checked against the bundle. `--cacert server.pem`
alone is not enough: curl built with OpenSSL can keep trusting its system CA
directory alongside the file, so a certificate from a system-trusted CA (an
intercepting proxy's, say) would also pass. curl also checks the host name, so
call the server by a name in the certificate: `localhost`, `127.0.0.1`, or one
listed in the server's `tls_names`. A client written in a programming language
should instead compare the SHA-256 of the presented leaf certificate (DER) with
the pin on every connection, as the `shed` CLI does.

```bash
API="curl -sS --cacert server.pem --pinnedpubkey $PIN --cert client.crt --key client.key -H Content-Type:application/json"

# What the server supports: expect service_clients and http_exec.
$API https://shed-host:8443/api/info | jq .features

# Create a shed (the name must carry the prefix).
$API -d '{"name":"nocturn-demo"}' https://shed-host:8443/api/sheds | jq '{name,status,owner,mounts,network_profile}'

# Run a command. argv is not wrapped in a shell: ask for one when you need it.
$API -d '{"argv":["bash","-lc","uname -a; id -un"],"timeout_seconds":30}' \
  https://shed-host:8443/api/sheds/nocturn-demo/exec \
  | jq -j 'if .t == "exit" then "\nexit \(.code)\n" else (.d | @base64d) end'

# Delete it.
$API -X DELETE https://shed-host:8443/api/sheds/nocturn-demo
```

The exec response is one JSON frame per line: `stdout` and `stderr` chunks in
base64, then an `exit` frame. A stream that ends without an `exit` frame means
the result was lost. Pass `stdin` as base64, set `timeout_seconds` (default 60)
and `max_output_bytes` (default 4 MiB) to bound each call, and expect
`409 SHED_NOT_RUNNING` for a stopped shed: exec never starts one. The contract
is in [API › POST /api/sheds/{name}/exec](../reference/api.md#post-apishedsnameexec).

## What the client can and cannot do

| Can | Cannot |
|-----|--------|
| Read `/api/info` and `/api/images` | Reach any other route (`403 FORBIDDEN`) |
| List, create, get, start, stop, delete and exec in its own sheds | See or touch a person's or another client's shed (`404 SHED_NOT_FOUND`, the same as a shed that does not exist) |
| Create within its policy: prefix, image list, caps, `max_sheds` | Use `local_dir`, `add_dirs`, `from_snapshot`, another network profile, or mounts outside `server_mounts` (`403 POLICY_VIOLATION`) |
| Run anything as root inside its sheds (`sudo`) | Use the credential bus, SSH, Connect tunnels, snapshots, or the egress routes (a create may still name `egress` profiles) |

`GET /api/info` needs no shed, so call it first: its `features` say what the
server supports (`service_clients`, `http_exec`, `mount_selection`, and
`network_profiles` on Firecracker). `GET /api/images` returns the server's
whole image list, the same one an admin sees; the client can create only from
the server's `default_image` and the aliases its policy's `images` names.

Admins keep the full API and see every shed, with its `owner`, in
`shed list -v`. An admin may also stop, delete and enter a client's shed. On a
server with users, a user who is not an admin cannot see a client's sheds, and
the client cannot see theirs.

### Repositories and credentials

A create's `repo` is cloned inside the shed with only the egress proxy
settings in its environment, never the server's `env_file` variables, so the
server's git credentials do not reach an owned shed. Use a public `https://`
URL (an `owner/repo` shorthand expands to an SSH URL,
`git@github.com:owner/repo.git`, which needs a key the shed does not have), or
create the shed without `repo` and clone it yourself with a short-lived token
the orchestrator passes in the exec request's `env`:

```bash
$API -d '{"argv":["bash","-lc","git clone \"https://x-access-token:${TOKEN}@github.com/org/repo.git\" ~/repo"],
          "env":{"TOKEN":"SHORT_LIVED_TOKEN"},"timeout_seconds":300}' \
  https://shed-host:8443/api/sheds/nocturn-demo/exec | jq -c 'select(.t == "exit")'
```

The token is then in the shed (the clone's `.git/config` keeps the URL), so
scope it to the one repository and keep its lifetime short.

The [credential bus](../reference/security.md#credential-bus) is always off for
an owned shed, so SSH-agent, AWS and Docker credential forwarding do not work
there: `ssh-add -l`, the Docker and AWS credential helpers, and any publish to
the in-guest agent at `127.0.0.1:498` get no answer: each request waits out the
agent's 30 s timeout, then fails. An owned shed has no server mounts unless the policy's
`server_mounts` names some, so no host credential directory is there either.

## Lifetime

The orchestrator should delete its sheds when it is done. As a backstop, the
server deletes any owned shed older than the client's `max_age`: a reaper runs
at `serve` start and every 60 s, stops the shed and deletes it, and writes a
`reap` line to the audit log. Sheds whose client was removed from the config
are left alone for an admin to clean up.

## The audit log

`audit.jsonl` lives next to `egress-audit.jsonl`, in the parent of the backend's
`instance_dir` (`/var/lib/shed/firecracker/audit.jsonl` with the Firecracker
defaults). Each client request writes one `api` line (identity and certificate
serial, route, path, shed, status), each HTTPS exec an `exec.end` line (argv[0]
and the argument count, exit code, duration), and each reap a `reap` line. To
log the full argv set `audit.exec_argv: full`; it can carry secrets. See
[Configuration › Audit log](../reference/configuration.md#audit-log).

```bash
sudo jq -c 'select(.identity.name == "nocturn")' /var/lib/shed/firecracker/audit.jsonl | tail
```

Every line's `identity` says who acted. The client is `kind: client` with its
`name` and certificate `serial`. A person, such as an admin who runs an HTTPS
exec in one of the client's sheds, is `kind: user` with the `user` name,
`admin` and `script` (each `true` or `false`) and the key's fingerprint as
`subject`; on a server with no user list the person is `implicit: true` with
no name. A `reap` line is `kind: system`: the server's `max_age` reaper acted,
not a person. A `reap` line without `error` records a shed the reaper deleted.
One with `error` records a delete that failed, with the backend's error text:
the shed may still exist, and while it does (and its client is still
configured) the reaper tries again on its next pass, which writes another line. To list the HTTPS execs people ran, in any
shed:

```bash
sudo jq -c 'select(.event == "exec.end" and .identity.kind == "user")' /var/lib/shed/firecracker/audit.jsonl | tail
```

When a person enters one of the client's sheds over SSH (a shell, `shed exec`
or `shed attach`, sftp or scp, or a connection forwarded with `ssh -L`), an
`ssh` line records it: the person (`kind: user`, by name and key
fingerprint), the peer's `remote` address, the `shed`, its `owner` (the
client) and the `channel` (`shell`, `exec`, `sftp` or `tunnel`, with the
`port` for a tunnel). It is written once the entry has happened, never for a
refused or failed attempt, and it never holds the command. Entering a
person's own shed writes none. A `shed tunnels` connection uses the HTTPS
Connect API instead, and writes a `connect` line once its tunnel into the
client's shed is established: the person as for an HTTPS exec, the `shed`,
its `owner` and the `port`. To list who entered the client's sheds over SSH
or through a tunnel:

```bash
sudo jq -c 'select(.event == "ssh" or .event == "connect")' /var/lib/shed/firecracker/audit.jsonl | tail
```

The log does not yet record everything a person can do to a client's shed:

- **Recorded:** an HTTPS exec (`exec.end`); a shell, a command, sftp or scp,
  or a connection forwarded over SSH (`ssh`); a tunnel through the HTTPS
  Connect API, as `shed tunnels` opens (`connect`, once the tunnel is
  established).
- **Not recorded yet:** starting, stopping, deleting or resetting the shed;
  taking a snapshot of it; changing its egress rules; reading its egress
  decisions (its recent decisions, `GET /api/egress/{name}`, or the live
  egress stream, `GET /api/egress/stream`); listing or killing its sessions;
  reading its record (`shed list`, `GET /api/sheds/{name}`). The access guard
  lets an admin do each of these to a client's shed, and none writes an audit
  line.

## Revoking a client

There is no per-certificate revocation yet. To revoke a client, remove it from
`auth.clients` and restart `serve`: every certificate issued for it is refused
from then on, at the TLS handshake. Its sheds stay, admin-only, until an admin
deletes them. Re-adding the same name later would trust the old certificates
again until they expire, so give a replacement client a new name.

## On a directory server or the remote backend

A client works the same way on a server that takes its users from an
[account service](user-directory.md) and on a server whose sheds are cloud
VMs (the [remote backend](../reference/remote-backend.md)): the same bundle,
routes and policy. What differs is who else can reach its sheds, what
`isolated` means, and, on the remote backend, the create options, the
lifecycle and the server-wide limits.

### Who can enter the client's sheds

Every admin of the server can enter a client's shed: over SSH (a shell,
`shed exec`, sftp or scp, a forwarded port), with an HTTPS exec, and through
a `shed tunnels` Connect tunnel. A user who is not an admin cannot see the
client's sheds. On a server with an account service, the admins are:

- the server's **local admins** (`auth.users` beside `auth.directory`);
- every user the account service **lists as an admin** for this server
  (`admin: true` in the list it sends).

So **the people who administer the account service decide who can read a
running task's credentials**, by two routes: whoever can make a person an
admin there, themselves included, and whoever can add a key to a person who
already is one, and so log in as that person. Either way they can enter the
client's sheds on every server that lists that person as an admin, and read
what the orchestrator put in them (a clone, a token passed to a command). No
server setting keeps an admin out of a client's shed. What an operator can do
today:

- **Keep the list's admins few.** Who is listed, and whether as an admin, is
  what the service sends this server, and it may differ from one server to
  another ([Security › A list admin controls the
  host](../reference/security.md#a-list-admin-controls-the-host)). Ask the
  service's operators to list as admins of this server only the people who
  must be. A list with no admin loads; the local admins stay admins.
- **Administer with a local admin on a backup key.** The service cannot
  change a local admin, and a local admin logs in whatever state the service
  is in ([User Directory › Keeping the local admin on a backup
  key](user-directory.md#keeping-the-local-admin-on-a-backup-key)).
- **Read the audit lines.** Each entry above writes a line that names the
  person: `ssh`, `exec.end` or `connect`. A list admin is recorded under their
  list name, with `admin: true`, whichever of their keys was used; the
  `subject` (the key's fingerprint) tells the keys apart. [The audit
  log](#the-audit-log) lists what is recorded and what is not yet.

```bash
sudo jq -c 'select(.event == "ssh" or .event == "connect" or (.event == "exec.end" and .identity.kind == "user"))' /var/lib/shed/firecracker/audit.jsonl | tail
```

A person whose admin flag is taken away on the service loses it for each new
SSH channel and API request after the server's next pull; `shed users
refresh` pulls at once. What they already have open stays open: an SSH
shell, command or forwarded port, a Connect tunnel, an API stream. Removing
the person from the list closes their SSH connections at that pull, and every
channel on them, but not a Connect tunnel or an API stream; a restart of
`serve` closes all of them, and whatever reconnects is checked again ([User
Directory › Admins, removals and
keys](user-directory.md#admins-removals-and-keys)).

The client itself does not depend on the account service. Its certificate is
never checked against the list, so it keeps working while the service cannot
be reached and after the list's trust time ends. A list entry with the
client's name is skipped, and `shed users status` shows it under `Skipped
entries`; a local admin may not have the client's name (the config does not
load).

### What `isolated` means on each backend

| Backend | `isolated` | The client's `policy.network` |
|---|---|---|
| Firecracker | Firewall rules the server installs on its own host. The guest reaches the internet and, beyond it, only its own ports on the gateway (its egress proxy when it has an egress profile, and its 9P ports when its policy names server mounts) and the destinations in the server's `network.allow_cidrs`; not the rest of the host, the LAN or other sheds. The full rule, and what the egress proxy can reach for it, is in [Security › Network profiles](../reference/security.md#network-profiles). | `isolated` (the default) or `open` |
| VZ | Not offered. A command reaches whatever the host's NAT reaches, including the host, the LAN and other sheds. | `open` only: any other value, the default included, stops the server from starting |
| Remote | The network gives a shed no route to another shed and no private route to the server's host. Outbound traffic is not filtered: a shed has a public address and reaches any public address, including a port the server's host exposes to the internet. | `isolated` (the default) or `open`; `remote.firewall.mode: any` and `acknowledge_any: true` are refused beside service clients |

A shed's record says which applies: its `isolation` member is `firewall`
(Firecracker), `topology` (remote) or `none` (an `open` shed, and an
`isolated` one whose backend cannot hold isolation now), so a client that
requires isolation checks `isolation` on the record its create returns, not
`network_profile`, which says `isolated` on Firecracker and on remote alike.
A server that does not list `uniform_shed_record` in `GET /api/info`
`features` sends no `isolation` on a record: treat such a shed as not
isolated, or read the server's kind from `/api/info` (`backend`, and
`isolation: "topology"` on remote, which names the backend's kind: check that
`features` also lists `network_profiles`, which says the server offers
`isolated` now).

### A client on the remote backend

The client's policy on a remote server:

| Policy key | On the remote backend |
|---|---|
| `name_prefix`, `max_sheds` | As on any backend. |
| `max_age` | As on any backend, and it is also the **lease** of each cloud shed the client creates: the account service deletes the machine, running or stopped, that long after its create (a create may name a shorter one in `lease`; above `max_age` is `POLICY_VIOLATION`). A client's lease cannot be extended. |
| `network` | `isolated` (the default) or `open`; see the table above. |
| `images` | Names from the account service's catalog (`default` and its aliases), checked at create: a create naming one the catalog lacks is refused, and the server logs the name when it first reads the catalog. `GET /api/images` lists nothing on a remote server. |
| `caps` | Compared with the [size](../reference/remote-backend.md#sizes-and-images) a create gets, not with the numbers it asked for. A create that names neither `cpus` nor `memory_mb` gets the catalog's default size, so caps below that size's numbers refuse such a create. |
| `server_mounts` | Must be empty: a client that names any stops the server from starting. |

A client's create may carry `name`, `repo`, `image`, `cpus` and `memory_mb`
(which pick a size), `upper_size_bytes` (the boot disk, 10 to 500 GB),
`network` (its own profile), `mounts` omitted or `[]`, and `egress` omitted,
`[]` or `["off"]`. These are refused before a VM is asked for, checked in
this order, and the first that fails is the answer:

| Order | Request | Answer |
|---|---|---|
| 1 | `cpus` and `memory_mb` that no configured size provides (a negative value included): the size is resolved before the policy runs | `400 INVALID_REQUEST` |
| 2 | `local_dir`, `add_dirs`, `from_snapshot`, a named `mounts` entry (a remote client's `server_mounts` is empty), an image outside `images`, a size over `caps`, a `network` other than its own | `403 POLICY_VIOLATION` |
| 3 | `egress` naming any profile (anything but omitted, `[]` or `["off"]`) | `422 FEATURE_UNSUPPORTED` |

So `{"name":"ci-probe","local_dir":"/tmp","cpus":999}` answers `400`, not
`403`.

The lifecycle is **asynchronous** (`features` holds `async_lifecycle`). A JSON
create answers `201` with `status: "starting"` and goes on in the background.
Before it answers, the server may run its firewall check (when the last result
is stale), looks the image up, and waits up to 15 seconds for the cloud to
accept the VM. Each step has its own bound, but the reply has no single one:
give the request a timeout of a minute or more. A quota or capacity refusal
that arrives in those 15 seconds is answered as a `503` and leaves no shed; a
later one leaves the shed in `error`. If the request times out on the
client's side, the shed may still have been created: get it by name before
creating it again. A start answers `200`
with `starting`, a stop `200` with `stopping`, and a delete `204` at once:
the shed leaves the list then, and its VM is destroyed in the background.
An exec on a shed that is not `running` answers `409 SHED_NOT_RUNNING`, so
poll `GET /api/sheds/{name}` before the first exec.
With `$API` from step 4, on a server whose `tls_names` lists
`shed.example.test`:

```bash
$API -d '{"name":"ci-demo"}' https://shed.example.test:8443/api/sheds | jq '{name,status}'

# Poll until the shed leaves starting, for up to 10 minutes.
for i in $(seq 120); do
  st="$($API https://shed.example.test:8443/api/sheds/ci-demo | jq -r .status)"
  [ "$st" = starting ] || break
  sleep 5
done
$API https://shed.example.test:8443/api/sheds/ci-demo | jq '{name,status,status_reason}'
```

A create with `Accept: text/event-stream` instead keeps the stream open until
the shed settles: its `complete` event carries the running shed, and an
`error` event reports a create that failed. What each status and
`status_reason` asks of the client (the full table is in [Remote backend ›
Statuses](../reference/remote-backend.md#statuses)):

| `status` (`status_reason`) | What the client does |
|---|---|
| `starting` (none, `no_pin`, `unreachable`, `cloud_unreadable`) | Keep polling: the server is still bringing the machine up, reaching it, or asking the account service. A create whose guest never answers stays `starting (unreachable)` and is tried again each pass until it is deleted or its lease ends: give up on it by deleting it. |
| `running` | Exec works. |
| `stopping` | Wait; exec answers `409 SHED_NOT_RUNNING`. |
| `stopped` (none or `external`) | A stop request stopped it, a start failed for a reason with no code of its own (the server log has it), or someone stopped the machine outside shed. Start it again, or delete it. A start at or after the lease's end is `409 LEASE_ENDED`: the machine is being deleted. |
| `stopped` (`capacity`, `quota`, `limit` or `firewall`) | A start was refused, by the cloud, the server's `max_running` or the ingress check. Start it again later, or delete it. |
| `stopped` (`unreachable`) | The VM came up but its agent did not answer, so the server stopped it again. Start it again; if it happens again, ask an admin to read the server log. |
| `stopped` (`host_key` or `layout`) | The VM presented a host key other than the pinned one, or its agent is one this server cannot drive. Starting again is not expected to help: delete it, and tell an admin. |
| `error` (`capacity`, `quota`, `create_failed`, `host_key`, `access`, `layout` or `firewall`) | The create failed. A delete leaves this state; so does the machine going (its lease ends, or the account service deletes it) when the create had made one, and the shed is then no longer listed. Until then the shed counts against `max_sheds`. Delete it; after `capacity`, `quota` or `firewall`, create again later. |

A create or start that the account service's limits, the cloud, the server's
limits or the ingress check refuses at once answers `503`, never `409`:
`COMPUTE_LIMIT_REACHED` (with a `reason` of `person` or `organization`),
`COMPUTE_UNAVAILABLE`, `SERVER_LIMIT_REACHED`, `CLOUD_QUOTA_EXCEEDED`,
`ZONE_CAPACITY_UNAVAILABLE` or `FIREWALL_CHECK_FAILED` ([Remote backend ›
Codes and reasons](../reference/remote-backend.md#codes-and-reasons)). Wait
and try again. A policy refusal is a `422 COMPUTE_POLICY_REFUSED` with the
rule in `reason` (`size`, `lease`, `image`, `access`, `person`, `client`) and
is not retried: a `client` refusal means the account service has not
registered this client for this server. A create whose name is held by a
machine still being deleted is `409 COMPUTE_DELETING`: wait and send it
again (it is never `SHED_ALREADY_EXISTS`).

For a service client the server shortens one message: `FIREWALL_CHECK_FAILED`,
whose full text names the server's public address and the ingress sources.
The account service's refusals reach a client by their code and reason only:
the text the service sends is never relayed, and nothing in a client's
responses names a cloud project, zone or address.

**Size the server-wide limits for the client.** They count every shed on the
server, people's and every client's together:

| Setting | Counts | Set it to |
|---|---|---|
| `remote.limits.max_sheds` (default off), checked at create | Every shed, in any state (`error` included) | At least the clients' `max_sheds` added up, plus what people need |
| `remote.limits.max_running` (default off, at most `max_sheds`), checked at create and at start | Every shed that is `starting`, `running` or `stopping`, plus each machine the server is deleting whose delete has not finished two minutes after it began | At least the client sheds that run at once, plus what people need |

A create past either limit, or a start past `max_running`, is refused with
`503 SERVER_LIMIT_REACHED`, and a start refused this way leaves the shed
`stopped (limit)`. A start does not check `max_sheds`.
Both limits are off unless set: the account service holds the quotas
(`COMPUTE_LIMIT_REACHED`). Firecracker and VZ have no server-wide shed limit:
there the client's `max_sheds` is its only bound.

Every remote shed is a cloud machine, billed while it exists. The `max_age`
reaper is off for cloud sheds: the account service deletes each machine at
its lease's end (the client's `max_age`, or the `lease` its create named), so
a stopped server leaves the client's machines to be deleted by the service.
Read a shed's lease in `cloud.lease.expires_at`.

On a remote server the audit log is `audit.jsonl` in `remote.state_dir`,
which by default is `remote` under the server's state directory
(`/etc/shed/remote/audit.jsonl` for a server that runs as root).

### The certificate's lifetime

`cert_max_ttl` (default `2160h`, 90 days) bounds what `shed-server client
issue` signs, and `--ttl` issues a shorter certificate. The bundle's
`expires_at` says when it ends; after that the server refuses it. To issue
again before then, run the issue command on the server's host as the user
that runs `serve` and with its config (step 2), naming an output file that
does not exist yet: `--out` never overwrites one. Run with `sudo`, it writes
the bundle owned by root with mode `0600`, so read it with `sudo` too. Put the
new bundle in the orchestrator's secret store, have the orchestrator load it,
and delete the file:

```bash
sudo shed-server client issue ci --ttl 720h --out ci-bundle-2.json
sudo jq -r .expires_at ci-bundle-2.json
```

The old certificate keeps working until it expires: there is no
per-certificate revocation. This is the same on every backend, with or
without an account service.

### Revoking

[Revoking a client](#revoking-a-client) is unchanged: remove it from
`auth.clients`, restart `serve`, and give a replacement client a new name.
The reaper leaves a removed client's sheds alone. On a remote server a
running one keeps running, and billed, until its machine's lease ends at the
account service or an admin stops or deletes it.

## What a leaked bundle gives

Whoever holds the bundle can do everything the client can, within its policy:
create sheds, run commands as root in its live sheds, read what the
orchestrator put in them, and delete them. Through the API it cannot touch a
person's sheds or another client's, and it gives no admin route and no SSH.
What those commands can reach on the network and the host is set by the
shed's profile and mounts, not by the certificate: an `isolated` shed on
Firecracker reaches the internet, the server's `network.allow_cidrs` and its own
gateway ports (egress proxy, server mounts), not the rest of the host or the
LAN (the full rule is in [Security › Network
profiles](../reference/security.md#network-profiles)), while with `policy.network:
open` (required on VZ) a command reaches whatever the host's NAT reaches,
including the host, the LAN and other open sheds; the client's
`server_mounts` are host directories it can read and, unless read-only, write.
See [Security › Service clients](../reference/security.md#residual-risks).
Keep `cert_max_ttl` short, keep the
bundle in a secret store, and revoke the client as above if it leaks.
