# Remote Backend Development

This page sets up a development shed-server that runs `default_backend: remote` on your machine, beside any installed shed-server. Each shed it creates is a virtual machine in your own Google Cloud project. For what the backend does and how clients see it, see [Remote Backend (Cloud)](../reference/remote-backend.md).

!!! warning "Every shed is a billed VM"
    A shed on this server is a Compute Engine VM with its own boot disk. It keeps running, and costing money, after you stop the dev server or close your terminal. Delete what you create and [check that nothing is left](#checking-nothing-is-left).

## Ports and paths

The remote dev server never touches an installed server's ports (8080 and 2222) or its state.

| Item | Value |
|---|---|
| HTTPS API (mTLS) | `127.0.0.1:18490` |
| SSH entry point | `127.0.0.1:12290` |
| HTTP port | `18090`, declared only: an enforced auth mode starts no plain-HTTP listener |
| Rendered config | `~/.shed/dev-remote/server.yaml`, from `configs/server.dev-remote.yaml.tmpl` |
| Log | `~/.shed/dev-remote/server.log` |
| PID file | `~/.shed/dev-remote/server.pid` |
| Record cache | `~/.shed/dev-remote/remote/` |
| Agent build | `~/.shed/dev-remote/agent/shed-agent-linux-amd64` |
| Server identity (host key, TLS pair, client CA) | `~/.shed/dev-remote/state/` |
| CLI entry | `remote-dev` |

## One-time cloud setup

Use a Google Cloud project that runs nothing but sheds. `scripts/remote/gce-setup.sh` prints the `gcloud` commands for one environment and runs none of them:

```bash
scripts/remote/gce-setup.sh --print \
  --project <project-id> \
  --env <env-name> \
  --region <region> \
  --source-range <your-public-address>/32 \
  --key-out ~/.config/shed/remote-dev/gce-key.b64
```

Use a documentation address such as `203.0.113.7/32` when you only want to read the output.

| Flag | Meaning |
|---|---|
| `--project` | The Google Cloud project ID. |
| `--env` | The environment name (`remote.environment`): 1 to 20 of `a-z`, `0-9` and `-`, starting with a letter. VMs, the network, subnet, network tag, firewall rule and service account are named `shed-<env>...`. |
| `--region` | The region of the zone the server uses. |
| `--source-range` | Your machine's public address as a `/32`: the only source the firewall rule admits on port 22. |
| `--subnet-range` | The subnet range (default `10.10.0.0/24`). |
| `--role-id` | The custom role ID (default `shedServer`), shared by every environment in the project. |
| `--key-out` | Where the base64 service account key is written, mode `0600`. The file must not exist yet: the printed block stops before making a key when it does. |
| `--firewall-auto` | Also print the two extra roles `remote.firewall.mode: auto` needs. Leave it out for `static`, the dev default. |

Read each printed block, then run it yourself. It enables the Compute Engine API and creates a custom-mode network and subnet, one firewall rule (tcp:22 from your address to the environment's tag), the custom role, a service account holding only that role, and its key. The printed text ends with the environment variables to set.

The server never creates or deletes the firewall rule, but it reads it. Unless the env file says otherwise, the dev config is rendered with `firewall.mode: static` and `source_ranges_env: SHED_REMOTE_SOURCE_RANGES`: the rule's source ranges must equal that variable, and while they do not, `shed create` and `shed start` answer `503 FIREWALL_CHECK_FAILED`. When your public address changes, update both:

```bash
gcloud compute firewall-rules update shed-<env-name>-ssh --project <project-id> \
  --source-ranges=<new-address>/32
# then set SHED_REMOTE_SOURCE_RANGES=<new-address>/32 in the env file and
make dev-server-restart-remote
```

If the address changes often, let the server follow it: run the block `gce-setup.sh --print --firewall-auto` adds (pass the `--role-id` your project's base role was made with, and check the printed service account is the one your key belongs to), then set two variables in the env file and restart:

```bash
SHED_REMOTE_FIREWALL_MODE=auto
SHED_REMOTE_ECHO_URLS=https://<first-echo-service>/,https://<second-echo-service>/
```

The config is then rendered with `mode: auto` and those `echo_urls`, and `SHED_REMOTE_SOURCE_RANGES` is no longer read. The start-up log line `firewall check passed (mode auto, ...)` shows the address the services agreed on. See [The firewall check](../reference/remote-backend.md#the-firewall-check) for every clause, the two extra roles and what each failure means.

Create the SSH key the server uses to log in to every VM:

```bash
mkdir -p ~/.config/shed/remote-dev && chmod 700 ~/.config/shed/remote-dev
ssh-keygen -t ed25519 -N '' -C shed-remote-link -f ~/.config/shed/remote-dev/link_ed25519
```

## The private env file

The cloud settings live in a private env file, never in the repository or the config file. The Make targets source `~/.config/shed/remote-dev/env` (override with `SHED_REMOTE_ENV_FILE`). It holds `VAR=value` lines:

```bash
SHED_REMOTE_ENV=<env-name>
SHED_GCE_PROJECT=<project-id>
SHED_GCE_ZONE=<zone>
SHED_GCE_NETWORK=shed-<env-name>
SHED_GCE_SUBNET=shed-<env-name>
SHED_GCE_TAG=shed-<env-name>
SHED_GCE_CREDENTIALS_FILE=<path to the key file>
SHED_REMOTE_SSH_KEY_FILE=<path to link_ed25519>
SHED_REMOTE_SOURCE_RANGES=<your-public-address>/32
```

| Variable | Read by | Meaning |
|---|---|---|
| `SHED_REMOTE_ENV` | `dev-server-up-remote` | The environment name, rendered into `remote.environment`. |
| `SHED_GCE_PROJECT` | server, `gce-image` | The project ID. |
| `SHED_GCE_ZONE` | server, `gce-image` | The zone every VM is created in. |
| `SHED_GCE_NETWORK` | server, `gce-image` | The environment's network. |
| `SHED_GCE_SUBNET` | server, `gce-image` | The environment's subnet. |
| `SHED_GCE_TAG` | `gce-image` | The network tag the firewall rule admits; the image bake gives it to its builder VM (`SHED_GCE_BAKE_TAG` overrides). |
| `SHED_GCE_CREDENTIALS_FILE` | server | Path to the service account key file: the JSON key or its base64, as `--key-out` writes it. `GOOGLE_CREDENTIALS_BASE64` (the key itself) is the alternative; set exactly one. |
| `SHED_REMOTE_SSH_KEY_FILE` | server | Path to the SSH private key. `SHED_REMOTE_SSH_KEY` (base64 of the key) is the alternative; set exactly one. |
| `SHED_REMOTE_SOURCE_RANGES` | server | The firewall rule's source ranges, comma-separated IPv4 CIDRs: what you passed as `--source-range`. The server compares the rule with it (`remote.firewall.source_ranges_env`). Not read in `auto` mode. |
| `SHED_REMOTE_FIREWALL_MODE` | `dev-server-up-remote` | Optional: `static` (the default) or `auto`, rendered into `remote.firewall.mode`. |
| `SHED_REMOTE_ECHO_URLS` | `dev-server-up-remote` | For `auto` only: two or more https echo services on different hosts, comma-separated with no spaces, rendered into `remote.firewall.echo_urls`. |

Keep the file mode `0600`. The server refuses to start, naming the variable, when one it needs is unset.

## Baking an image

A remote shed boots from an image baked by `scripts/build-gce-image.sh`. The bake boots a builder VM from stock Ubuntu 24.04, provisions and verifies it, saves its disk as an image in the family `shed-extensions-amd64`, and deletes the builder.

```bash
set -a; . ~/.config/shed/remote-dev/env; set +a
make gce-image GCE_IMAGE_ARGS=--print   # show every step, run nothing
make gce-image                          # bake (the builder VM is billed while it runs)
make test-gce-image                     # offline checks of the image tooling
```

The dev config boots the newest image of the family (`default_image: family/shed-extensions-amd64`, alias `cloud`). Rebake after any change to `cmd/shed-agent` or `gce/`: the server does not copy an agent into a VM (`agent_install` is not built), so a VM whose agent is incompatible fails its create with reason `layout`.

## Running the dev server

```bash
make dev-server-up-remote        # build, render the config, start
make dev-server-status-remote    # running? reachable through the CLI?
make dev-server-logs-remote      # follow the log
make build && make dev-server-restart-remote   # pick up a rebuild
make dev-server-down-remote      # stop the server; its sheds keep running
```

`dev-server-up-remote` refuses to start when the env file is missing or a dev server is already running. Like the other dev servers it runs under `nohup` with a PID file: no unit, no restart on crash, nothing that survives a reboot. Stopping the server does not stop or delete any VM; at its next start it lists the cloud and adopts what it finds (see [Server restarts](../reference/remote-backend.md#server-restarts)).

`SHED_REMOTE_GITHUB_USER` (a Make variable) names the GitHub account whose SSH keys may enrol a CLI. The rendered config also defines a `nocturn` service client; issue its bundle with:

```bash
bin/shed-server client issue nocturn --config ~/.shed/dev-remote/server.yaml --ttl 72h --out <file>
```

## Registering the CLI entry

Once, after the first start:

```bash
shed server add localhost --port 18090 --ssh-port 12290 --name remote-dev
```

First contact is over SSH, so name the SSH port. The CLI enrols with the server's client CA and records an entry with the HTTPS API URL (port 18490), the pinned certificate fingerprint and a client certificate under `~/.shed/creds/remote-dev/`, which it renews when it nears expiry. Then:

```bash
shed -s remote-dev create t1
shed -s remote-dev exec t1 -- uname -a
ssh t1@localhost -p 12290
shed -s remote-dev stop t1
shed -s remote-dev start t1
shed -s remote-dev delete t1 --force
```

## Checking nothing is left

Before you stop for the day:

```bash
shed -s remote-dev list
gcloud compute instances list --project <project-id> --filter='labels.shed_env=<env-name>'
gcloud compute disks list --project <project-id> --filter='labels.shed_env=<env-name>'
```

Both `gcloud` lists must be empty once every shed is deleted. `shed delete` returns only when the VM is gone (about 30 s); a JSON `DELETE` through the API answers in about 3 s and the VM goes in the background, so it can show for a short while. A destroy that fails is retried by the server's periodic pass, which runs once a minute. A VM that should not exist can be deleted with `gcloud compute instances delete`; the server forgets its record at the next pass.

Every VM also carries a maximum run duration (the larger of `remote.max_run_duration`, 24 h in the dev config, and the owning client's `max_age`), after which the cloud stops it and the shed is listed `stopped (run_limit)`. A stopped VM still bills for its disk. To see it happen, render the config with `max_run_duration: 10m`, the shortest the server accepts.

## Integration tests

The integration suite's `[remote]` cases run only when `SHED_REMOTE_SERVER` names this entry:

```bash
make test-integration-remote                                          # every [remote] case
make test-integration-remote PYTEST_ARGS="test_remote_refusals.py"    # no VMs
```

The target is manual only and never starts the server for you. It sets every vz and fc server variable empty, so no other backend is touched. A full run creates about 25 VMs one after another, never more than `remote.limits.max_running` (2) at once, and takes about 42 minutes. Tests the backend cannot run (mounts, image store, guest MTU, vz/fc timing and image bakes) skip with a reason. See `tests/integration/README.md` for the list.

## Measured timings

From three live runs on the dev project (`e2-standard-2`, 20 GB image, one zone). The cloud's own share varies from run to run: the provider phase of a create took between 12 and 35 s.

| Step | Seconds |
|---|---|
| `shed create` (streamed) | 52 to 58 usually, 81 once: provider 12 to 35, boot 33 to 43, provision 0.1 to 0.5, ready 3 |
| `shed create --repo` (a public repo) | 59.7: clone 1.4, provision 4.2 |
| `shed create --repo` (a repository that does not exist) | 68: three clone attempts 11, then a warning and a running shed |
| `shed exec` on a running shed | 0.22 |
| `shed stop` (VM powered off) | 28 to 34 |
| `shed start` (disk and host key kept) | 42 to 54 |
| Create refused at `max_running` | 0.3, no VM created |
| JSON create (`POST /api/sheds`), until 201 `starting` | 11 to 15 |
| Adopting a running VM after the record cache was removed | probe 0.6 |
| `shed delete` (streamed) | 29 to 34, returns when the VM is gone |
| JSON `DELETE` | 204 after about 3; VM gone after about 30 |
| A VM stopped outside shed, until listed `stopped (external)` | 10 after the VM stopped (the pass runs once a minute) |
| A VM deleted outside shed, until its record is gone | 2 to 12 after the VM was gone |
| `make gce-image` | 272 |
| A create from an image with no shed agent, until `error (create_failed)` | 324 (the 5 minute wait for the agent); VM and disk gone 58 later |
| A VM at its run limit (10 minutes), until listed `stopped (run_limit)` | 63 after the limit; `shed start` 44, with a new limit |
| `shed delete` of a VM under deletion protection | 4, with an error; a create refused at `max_running` from 2 minutes on; VM gone 80 after the protection was cleared |
| Firewall rule changed by hand, until creates are refused | 29 to 53 (the next pass) |
| Auto mode: rule sources changed by hand, until patched back | 45 to 47 |
