# Upgrade Guide: Cloud Sheds from the Account Service

A remote server used to talk to a cloud itself: it held a cloud credential, made every cloud call, and was configured with the environment's sizes, images and run limit. It now holds **no cloud credential and makes no cloud call**. Every machine comes from the **account service** the server is enrolled with (`auth.directory`), over the [compute contract](../reference/compute-contract.md). The service holds the credential, decides the sizes, images, limits and lease of each machine, and deletes it at the lease's end.

This is a flag day. The server, the account service, the desktop app, the phone client and any orchestrator that creates cloud sheds move together, and **cloud sheds made by the old server are not migrated**.

!!! warning "Delete your cloud sheds before you upgrade"
    A server that finds records or delete markers written by the old server **refuses to start** and names each one. Delete every cloud shed on the old binary first (`shed delete`), and confirm the machines are gone from the cloud. Machines the old server made and the service does not know are the cloud's, and nothing in shed will delete them for you.

## Order

Do these in order. Steps 1 to 3 are the account service's operator's; the rest are the shed operator's.

1. **Pause every orchestrator** that creates cloud sheds (a service client such as an agent runner), so nothing creates a shed on a server that is about to change.
2. **Delete every cloud shed on every old remote server, with the old binary.** `shed list`, then `shed delete <name>` for each (a delete returns once the machine is gone). Check the cloud's own list is empty of shed machines. This is [the only way to retire old state](#old-cloud-sheds): the new server cannot read it.
3. **Give the account service its cloud credential.** Make the credential with the least role the service's own documentation names (one custom role in a project that holds nothing but shed machines), store it where the service reads it, and confirm the service can reach the cloud's API from where it runs.
4. **Deploy the account service version that supplies compute.** Seed or check its sizes and images; register each orchestrator as a service client for each server; and allow compute for each server (the grant, which is off by default).
5. **Upgrade the shed servers and desktop apps, the phone client and the orchestrators together**, with the config changes below. Enrol nothing again: the server's enrolment is the one it has.
6. **Resume the orchestrators.**

A server with no remote backend needs none of this.

## What is removed

| Removed | What replaces it |
|---|---|
| The in-tree cloud provider (`remote.provider: gce`) | `remote.provider: service`, the default. The server refuses to load with `provider: gce`. |
| `remote.environment` | Nothing: the service names its own machines and network. A machine is the calling server's by its **enrolled name**. |
| `remote.sizes`, `remote.default_size` | The service's catalog. `shed create` picks from it (`SIZE_UNAVAILABLE` when none fits); a service client's `policy.caps` are checked against the size a create gets. |
| `remote.default_image`, `remote.image_aliases` | The service's catalog: the image `default` and its aliases. A service client's `policy.images` names aliases the catalog offers (checked at create, not at config load). |
| `remote.max_run_duration`, and the `run_limit` reason | The lease. Each machine is deleted at its lease's end, running or stopped (see below). A stopped machine is no longer stopped by a run limit. |
| `remote.gce` and every cloud environment variable of the server (the credentials, project, zone, network, subnet, tag) | Nothing. The server reads none of them. (The image bake, `make gce-image`, still reads its own variables; see [Remote Backend Development](../development/remote-backend.md#baking-an-image).) |
| The server's custom role, service account and the firewall rule it read | The service's own cloud setup. `scripts/remote/gce-setup.sh` now prints only the image bake's setup. |
| The cloud labels the server wrote (`shed_env`, the claim label, ...) | The machine's members at the service (`user`, `client`, `shed_id`) and the server's working labels. |

Each removed key is refused at config load, **by name**, with a message that names this page:

```text
config key remote.max_run_duration was removed: a remote server gets its machines, sizes, images and lifetimes from the account service (remote.provider: service with auth.directory); remove it (see docs/upgrades/remote-compute.md)
```

A remote server needs `auth.directory` now. Without it the config is refused. What stays: `disk_gb` (when set it wins over the size's default), `limits`, `transport`, `ssh`, `firewall` and `agent_dir`; new is `remote.access.allow`.

## What changes for people and operators

- **Leases.** Every cloud shed has an end, shown in `shed list` (`LEASE`, marked `!` near the end) and in a one-line banner at an interactive login. Extend it with [`shed extend`](../reference/cli.md#shed-extend), never past its cap. **At the end the machine and its disk are deleted**, running or stopped, and there is no undelete. No timer in the server deletes anything. See [Remote Backend › Leases](../reference/remote-backend.md#leases).
- **Local admins cannot create cloud sheds.** A create by an admin of the server's own `auth.users` is `422 COMPUTE_POLICY_REFUSED` with reason `person`: the service makes machines for the people it lists and for registered service clients only. Create as a person of the service, or as a client. A local admin can list, stop and delete any cloud shed and extend a person's; it can start, enter and exec in a service client's shed but not in a person's (`403`), and a client's lease cannot be extended (`EXTEND_NOT_ALLOWED`). See [Remote Backend › Leases](../reference/remote-backend.md#leases).
- **`shed assign` is not supported on a cloud shed** (`501 FEATURE_UNSUPPORTED`): its user is its machine's, fixed by the service at create.
- **A create is the service's.** It can be refused by the service's limits (`COMPUTE_LIMIT_REACHED`, with the reason `person` or `organization`), its policy (`COMPUTE_POLICY_REFUSED`: size, lease, image, access, person, client) or the grant (`COMPUTE_DENIED`), and a service that cannot be reached is `503 COMPUTE_UNAVAILABLE`. The list is in [Remote Backend › Codes and reasons](../reference/remote-backend.md#codes-and-reasons).
- **A server whose service is down starts degraded.** Reads and existing sessions keep working; creates, starts, stops, deletes and extends are `503 COMPUTE_UNAVAILABLE` until the service answers.

## Ingress, `isolated` and the `/32` rule

The server no longer reads a cloud firewall rule; it reads and (in two modes) writes the **ingress** the service applies for it.

- **Static mode now writes its sources.** With `firewall.mode: static`, a server that finds the service's ingress different from its configured sources **writes them** (at most once every 5 minutes) when the service's ingress is writable. It only compares, as before, when the service says its ingress is managed elsewhere. If you relied on static mode never changing anything, make sure the configured sources are the ones you want applied.
- **`auto` mode needs a writable ingress.** The server refuses to start in `auto` against a service whose ingress is managed elsewhere, naming the mode to use.
- **`isolated` needs an exclusive ingress.** A shed with the `isolated` profile (a service client's policy `network: isolated`, usually) is created and started only while the service says the ingress is *exclusive*: nothing but the sources reaches the machines, and no machine reaches another. A Compute Engine adapter can prove that only for a **single `/32` IPv4 source** on a simple dedicated network. So either run `firewall.mode: auto` (one `/32`: the server's address), or `static` with exactly one `/32`, or the isolated sheds are refused (`503 FIREWALL_CHECK_FAILED`).
- **`remote.access.allow`** (new) is the set of CIDRs a machine's address must be in for the server to dial it; empty allows every unicast address outside loopback, link-local, unspecified and multicast. Set it to the ranges your service's machines can have. See [Reaching a machine](../reference/remote-backend.md#reaching-a-machine).

## Rotating the link key

The server logs in to every machine as the user `shed-link` with one SSH key. Its **public half is given to the service at each create and fixed into the machine's `authorized_keys`**; a machine accepts only the keys it was made with. So a new key reaches only machines made after the change.

To rotate: keep the old key configured (`remote.ssh`) **until every machine made with it has passed its cap** (at the latest, its `max_expires_at`), then switch. The server holds one key and sends one at each create, so a new key cannot reach an older machine: that shed is `starting (unreachable)` until it is deleted or its lease ends. Plan the switch for a time when every older shed is gone, or recreate them.

## Old cloud sheds

Old cloud sheds are **not migrated**. A record or delete marker the previous server wrote carries no schema number the new server accepts. At start the server stops and names each one:

```text
remote: the remote state directory holds sheds of the in-tree cloud provider this server no longer has: shed ada-dev (it was written by an earlier server); an unfinished delete of shed ada-old (01HZ...). Run the previous shed-server release once more and `shed delete` each of these sheds; or, once their VMs are confirmed gone from the cloud, remove the directories sheds/ and deleting/ in <dir> (not the directory itself: it holds the audit log). Then start this server (see docs/upgrades/remote-compute.md)
```

Do one of the two:

1. Run the previous shed-server release once more, `shed delete` each shed it names, wait for the machines to be gone, then start the new server. (This is step 2 above, done late.)
2. Confirm in the cloud that the machines are gone, then remove the `sheds/` (records) and `deleting/` (delete markers) directories in the directory the message names (`remote.state_dir`, by default `remote/` under the server's state directory), and start the new server. Keep the directory itself: it also holds the audit log, `audit.jsonl`.

The message lists what it found; a machine still in the cloud that no record names is the cloud's to delete, not shed's.

## Related

- [Remote Backend (Cloud)](../reference/remote-backend.md)
- [Compute Contract](../reference/compute-contract.md)
- [Guest Contract](../reference/guest-contract.md)
- [CLI Reference › shed extend](../reference/cli.md#shed-extend)
