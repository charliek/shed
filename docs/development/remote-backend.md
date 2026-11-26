# Remote Backend Development

This page sets up a development shed-server that runs `default_backend: remote` on your machine, beside any installed shed-server. Its machines come from an **account service**, never from a cloud directly (see [Remote Backend (Cloud)](../reference/remote-backend.md)). You can run it against:

- **The in-tree reference service**, an in-memory account service that ships with the repository. With `transport: local` each shed's agent runs on your own host, so there is no cloud, no private value and no billing. Start here.
- **A local account service** of your own (or a development instance of one), which makes real cloud machines. Each shed is then a billed machine until its lease ends.

For what the backend does and how clients see it, see [Remote Backend (Cloud)](../reference/remote-backend.md); for the protocol, the [compute contract](../reference/compute-contract.md).

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
| Server identity (host key, TLS pair, client CA, enrolment) | `~/.shed/dev-remote/state/` |
| Reference service (control files, log, PID) | `~/.shed/dev-remote/ref/` (override with `SHED_COMPUTE_REF_DIR`), on `127.0.0.1:18700` (`SHED_COMPUTE_REF_PORT`) |
| CLI entry | `remote-dev` |

## Against the reference service

The reference service (`internal/directory/refserver`, built by `make directory-ref` into `bin/test/`) holds machines as records in memory, answers every call of the compute contract, and is steered through files in its control directory. It is built and used only by tests and by this loop; no release carries it.

1. Make the private env file the Make targets read, `~/.config/shed/remote-dev/env` (override with `SHED_REMOTE_ENV_FILE`), mode `0600`:

   ```bash
   SHED_REMOTE_DIRECTORY_URL=http://127.0.0.1:18700
   SHED_REMOTE_TRANSPORT=local
   SHED_REMOTE_ADMIN_KEY="ssh-ed25519 AAAA... you@host"
   SHED_REMOTE_SOURCE_RANGES=203.0.113.7/32
   ```

   `SHED_REMOTE_SSH_KEY_FILE` is not needed with the local transport. `SHED_REMOTE_ADMIN_KEY` is the public key line of the local admin `dev` (default `~/.ssh/id_ed25519.pub`). A plain `http://` URL is accepted only for a loopback address, as the reference service's.

2. Start the service, then the server:

   ```bash
   make dev-compute-ref-up        # builds the service, starts it, prints the enrolment code
   make dev-server-up-remote      # renders the config, builds, starts the server
   # the one-time code, on standard input (there is no --code flag)
   awk '{print $2}' ~/.shed/dev-remote/ref/control/codes \
     | bin/shed-server directory enroll --config ~/.shed/dev-remote/server.yaml --name remote-dev
   make dev-compute-ref-grant     # lets remote-dev manage compute; registers the client nocturn
   ```

   The enrolment happens once. `shed-server directory enroll` reads the code from standard input, never from the command line: `dev-compute-ref-up` prints the one-time code and writes it to `~/.shed/dev-remote/ref/control/codes` (`remote-dev <code>`), so pipe it in as above, or run the command without the pipe and paste the code at its prompt (which does not echo). If the server started before the enrolment or the grant, restart it (`make dev-server-restart-remote`) so it reads the service's machines.

3. Register the CLI entry once:

   ```bash
   shed server add localhost --port 18090 --ssh-port 12290 --name remote-dev
   ```

   First contact is over SSH, so name the SSH port. The CLI enrols with the server's client CA and records an entry with the HTTPS API URL (port 18490), the pinned certificate fingerprint and a client certificate under `~/.shed/creds/remote-dev/`, which it renews when it nears expiry.

4. Make a person. The server's local admin `dev` (the `remote-dev` entry, enrolled with `SHED_REMOTE_ADMIN_KEY`) **creates no cloud sheds** (`COMPUTE_POLICY_REFUSED`, reason `person`); the service's people do, and service clients. A person needs a key of their own and a CLI entry enrolled with it:

   ```bash
   ssh-keygen -t ed25519 -N '' -C ada -f ~/.config/shed/remote-dev/ada_ed25519
   ```

   List the person, with the public key of that key pair **without its comment** (the key type and the key only), in `~/.shed/dev-remote/ref/control/users.json`, as the reference service reads it. `cut -d' ' -f1,2 ~/.config/shed/remote-dev/ada_ed25519.pub` prints it:

   ```json
   [{"name": "ada", "admin": false, "keys": [{"key": "ssh-ed25519 AAAA...", "label": "laptop"}]}]
   ```

   Have the server read the list now (as the local admin), then enrol a second entry as `ada`, with her key:

   ```bash
   shed -s remote-dev users refresh
   shed server add localhost --port 18090 --ssh-port 12290 --name remote-dev-ada \
     --identity-file ~/.config/shed/remote-dev/ada_ed25519
   shed server list        # remote-dev-ada: user ada; remote-dev: user dev
   ```

   The integration suite does all of this for its people (`itest-alice` and friends, each with a HOME of its own): see `tests/integration/README.md`. A service client's create works too: issue the `nocturn` bundle with

   ```bash
   bin/shed-server client issue nocturn --config ~/.shed/dev-remote/server.yaml --ttl 72h --out <file>
   ```

5. Create, extend and delete, as `ada`:

   ```bash
   shed -s remote-dev-ada create t1
   shed -s remote-dev-ada list              # SIZE and LEASE columns, the Cloud: footer
   shed -s remote-dev-ada extend t1 --hours 6
   shed -s remote-dev-ada exec t1 -- uname -a
   shed -s remote-dev-ada delete t1 --force
   ```

   The local admin's entry sees and manages the shed too (`shed -s remote-dev list`, `stop`, `delete`, `extend`), but cannot enter or start it.

6. Stop the service when you are done (`make dev-compute-ref-down` forgets every machine it held, and the shed records then go at the server's next pass):

   ```bash
   make dev-server-down-remote
   make dev-compute-ref-down
   ```

`make dev-compute-ref-grant` writes `control/compute.json`, which the service reads each tick. Its `steps` are the named set-up steps and failures of the contract's fixtures (`compute.allow`, `client.register`, `quota.fill`, `cloud.fail`, `clock.advance`, ...); the test cases use the same file to make quotas full, a cloud fail or a lease end.

## Against a local account service

An account service of your own that implements the [compute contract](../reference/compute-contract.md) works the same way, with these differences:

- `SHED_REMOTE_DIRECTORY_URL` is its `https://` URL (or a loopback `http://` URL for one on your machine), and `SHED_REMOTE_TRANSPORT` is `ssh` (the default).
- The server logs in to each machine with an SSH key you make. The public half is sent in every create and fixed into the machine:

  ```bash
  mkdir -p ~/.config/shed/remote-dev && chmod 700 ~/.config/shed/remote-dev
  ssh-keygen -t ed25519 -N '' -C shed-remote-link -f ~/.config/shed/remote-dev/link_ed25519
  ```

  and in the env file `SHED_REMOTE_SSH_KEY_FILE=<path to link_ed25519>`.
- The service must let this server manage compute (the grant, in its own operator setup) and know your people and clients. Enrol the server under the name `remote-dev`.
- The ingress is the service's. In the default `static` mode the server writes `SHED_REMOTE_SOURCE_RANGES` (your public address as a `/32`) to it, or only compares when the service says its ingress is managed elsewhere. If your address changes often, set `SHED_REMOTE_FIREWALL_MODE=auto` and `SHED_REMOTE_ECHO_URLS` (two or more https echo services on different hosts, comma-separated, no spaces) so the server follows it. See [Ingress and the firewall modes](../reference/remote-backend.md#ingress-and-the-firewall-modes).

| Variable | Read by | Meaning |
|---|---|---|
| `SHED_REMOTE_DIRECTORY_URL` | `dev-server-up-remote` | The account service's URL, rendered into `auth.directory.url`. |
| `SHED_REMOTE_ADMIN_KEY` | `dev-server-up-remote` | The local admin's public key line. |
| `SHED_REMOTE_TRANSPORT` | `dev-server-up-remote` | `ssh` (default) or `local` (a real agent on this host; reference service only). |
| `SHED_REMOTE_SSH_KEY_FILE` | server | Path to the SSH private key the server logs in with. `SHED_REMOTE_SSH_KEY` (base64 of the key) is the alternative; set exactly one. Not read with `local`. |
| `SHED_REMOTE_SOURCE_RANGES` | server | The ingress's source ranges, comma-separated IPv4 CIDRs (`remote.firewall.source_ranges_env`). Not read in `auto` mode. |
| `SHED_REMOTE_FIREWALL_MODE` | `dev-server-up-remote` | Optional: `static` (the default) or `auto`, rendered into `remote.firewall.mode`. |
| `SHED_REMOTE_ECHO_URLS` | `dev-server-up-remote` | For `auto` only: the echo services, rendered into `remote.firewall.echo_urls`. |

Keep the env file mode `0600`. The server refuses to start, naming the variable, when one it needs is unset.

## Running the dev server

```bash
make dev-server-up-remote        # build, render the config, start
make dev-server-status-remote    # running? reachable through the CLI?
make dev-server-logs-remote      # follow the log
make build && make dev-server-restart-remote   # pick up a rebuild
make dev-server-down-remote      # stop the server; its sheds' machines keep running
```

`dev-server-up-remote` refuses to start when the env file is missing or a dev server is already running. Like the other dev servers it runs under `nohup` with a PID file: no unit, no restart on crash, nothing that survives a reboot. Stopping the server does not stop or delete any machine; at its next start it lists the service and adopts what it finds (see [Server restarts](../reference/remote-backend.md#server-restarts)). A server whose state directory holds records from before the account service supplied machines refuses to start; see the [upgrade guide](../upgrades/remote-compute.md#old-cloud-sheds).

## Baking an image

Against a real cloud, a machine boots from an image the service's cloud adapter can use. shed bakes the reference image for Compute Engine, the one example adapter: `scripts/build-gce-image.sh` boots a builder VM from stock Ubuntu 24.04, provisions and verifies it, saves its disk as an image in the family `shed-full-amd64` (or `shed-base-amd64` with `--variant base`), and deletes the builder. The server itself calls no cloud, so the image is the only thing this repository needs from a project.

`scripts/remote/gce-setup.sh` prints the one-time `gcloud` commands the bake needs (a network, a subnet and one ingress rule for the baking machine), and runs none of them:

```bash
scripts/remote/gce-setup.sh --print \
  --project <project-id> \
  --region <region> \
  --source-range <your-public-address>/32
```

Use a documentation address such as `203.0.113.7/32` when you only want to read the output.

| Flag | Meaning |
|---|---|
| `--project` | The Google Cloud project ID. |
| `--region` | The region of the zone the builder runs in. |
| `--source-range` | Your machine's public address as a `/32`: the only source the firewall rule admits on port 22 of the builder. |
| `--name` | The name of the network, subnet, firewall rule and network tag (default `shed-bake`). |
| `--subnet-range` | The subnet range (default `10.10.0.0/24`). |

Read each printed block, then run it yourself with your own gcloud sign-in. It creates no service account, role or key. Then bake:

```bash
export SHED_GCE_PROJECT=<project-id> SHED_GCE_ZONE=<zone> \
       SHED_GCE_NETWORK=shed-bake SHED_GCE_SUBNET=shed-bake SHED_GCE_BAKE_TAG=shed-bake
make gce-image GCE_IMAGE_ARGS=--print   # show every step, run nothing
make gce-image                          # bake (the builder VM is billed while it runs)
make test-gce-image                     # offline checks of the image tooling
```

Rebake after any change to `cmd/shed-agent` or `gce/`: the server does not copy an agent into a machine (`agent_install` is not built), so a machine whose agent is incompatible fails its create with reason `layout`.

## Checking nothing is left

Every shed on a real service is a billed machine. **No timer in the server deletes anything**: the account service deletes a machine at its lease's end, and a delete does it earlier. Before you stop for the day:

```bash
shed -s remote-dev list
```

Every row is a machine, with its lease in the `LEASE` column. Delete what you do not need (`shed -s remote-dev delete <name> --force`); `shed delete` returns only when the machine is gone. Check the service's own view of the machines too (its page, or your cloud's list filtered by the labels your adapter sets), since a machine whose record was lost is still the service's until its lease ends. Stopping the dev server stops nothing.

## Integration tests

Two targets run the suite's cloud cases. Neither starts a dev server for you except where stated.

```bash
make test-integration-compute                    # no cloud: hermetic worlds, Linux only, about ten minutes
make test-integration-compute PYTEST_ARGS="-k quotas"
make test-integration-remote                     # the [remote] cases against the running remote dev server
SHED_REMOTE_SERVICE=ref make test-integration-remote PYTEST_ARGS="test_remote_compute.py"
```

`make test-integration-compute` needs no cloud and no dev server: each case starts its own reference service and a private `shed-server` from `bin/` with the local transport, and the shared `[remote]` cases run against one such world. `make test-integration-remote` is manual only and runs against `remote-dev`; against a real service every case that creates a shed creates a billed machine (see the suite's README for the counts). `SHED_REMOTE_SERVICE` (`ref` or `service`), `SHED_COMPUTE_CONTROL` and `SHED_REMOTE_RESTART` steer the cloud cases; every variable is in `tests/integration/README.md`. Cases the backend cannot run (mounts, image store, guest MTU, vz/fc timing) skip with a reason.

## Measured timings

Measured on a development project before the server's machines came from the account service (a direct Compute Engine provider, `e2-standard-2`, 20 GB image, one zone). They are indicative: the service adds its own few seconds, and the cloud's share varies from run to run.

| Step | Seconds |
|---|---|
| `shed create` (streamed) | 52 to 58 usually, 81 once: cloud 12 to 35, boot 33 to 43, provision 0.1 to 0.5, ready 3 |
| `shed create --repo` (a public repo) | 59.7: clone 1.4, provision 4.2 |
| `shed exec` on a running shed | 0.22 |
| `shed stop` (machine powered off) | 28 to 34 |
| `shed start` (disk and host key kept) | 42 to 54 |
| Create refused at `max_running` | 0.3, no machine created |
| JSON create (`POST /api/sheds`), until 201 `starting` | 11 to 15 |
| `shed delete` (streamed) | 29 to 34, returns when the machine is gone |
| JSON `DELETE` | 204 after about 3; machine gone after about 30 |
| `make gce-image` | 272 |
