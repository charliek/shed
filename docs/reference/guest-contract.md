# Guest Contract

The **guest contract** is what a cloud shed's machine must run so that a shed-server can drive it. A compute service (an account service that supplies compute, the [compute contract](https://github.com/charliek/shed/tree/main/tests/compute-contract)) chooses its own images and makes its own cloud calls; shed-server asks for an image by name (`default` or an alias) and relies on the image meeting this contract. shed's image recipe (`gce/`, built by `make gce-image`) is the reference implementation.

The names on this page come from one place in shed's source, `internal/remote/guest` (`guest.go` and `sshd.go`); a test keeps the image scripts in step with it. Version numbers are **guest layout 1** (`guest.Layout`) and **agent protocol 2** (`guest.AgentProtocol`).

## What the guest provides

| Part | Requirement |
|---|---|
| The link user | A system user `shed-link`, group `shed-link`, with no password and a real shell. It is the only user the guest's sshd lets in. The server logs in as it and nothing else |
| Its authorized keys | The `authorized_keys` lines of the create (the server's link key, two lines while that key is being rotated), written on every boot to `/etc/ssh/shed-link/authorized_keys` (root-owned, mode 0644, in a root-owned directory, mode 0755) |
| The workspace user | `shed`, UID 1000, with passwordless sudo. The agent runs user commands as it; it cannot log in over the guest's sshd |
| sshd | Listens on port 22, the only port that listens off loopback, with exactly the policy in `guest.SSHDConfig`: no root, no passwords, `AllowUsers shed-link`, one host key, forwarding only to the agent's three sockets, no TTY, no agent or X11 forwarding |
| The host key | One `ssh-ed25519` key at `/etc/ssh/ssh_host_ed25519_key`, made on the machine's first boot (an image carries none). Its public half, `ssh-ed25519 <base64>` with no comment, is **published to the compute service**, which keeps the first key published and attests only that one. The server compares it with the key sshd presents at first contact, pins it, and refuses a change |
| The shed's name and id | The machine knows the shed's name (it becomes the hostname) and its permanent id. A boot unit, `shed-identity.service`, runs on every boot before sshd and the agent and writes `SHED_NAME` and `SHED_ID` to `/run/shed/identity.env` |
| The agent | `shed-agent` at `/usr/local/bin/shed-agent` (root-owned, mode 0755), run by `shed-agent.service` with `KillMode=process`, listening on unix sockets in `/run/shed-agent` (directory `root:shed-link` 0750, sockets `root:shed-link` 0660): `1024.sock` (exec and console), `1026.sock` (messages and health), `1028.sock` (proxied connections). It speaks agent protocol 2 (the server accepts 1 and 2) and reports itself with `shed-agent version` |
| The layout file | `/etc/shed/layout` holds the guest layout, `1`, as decimal digits and a newline |
| Agent updates | `/usr/local/sbin/shed-agent-install` (root-owned, mode 0755), which `shed-link` may run as root without a password and nothing else (`/etc/sudoers.d/shed-link`) |

A change to anything an image carries (a path, a unit name, a user, a socket mode, how the boot unit reads its inputs) is a new guest layout. A change to what the agent and the server say to each other is a new agent protocol.

## How the inputs reach the guest

Each compute service's adapter delivers the shed's name, its id and the authorized keys by its own means (instance metadata, a cloud-init document, a secret mount), and reads the published host key back by its own means. The contract fixes what the guest ends up holding, not how it got there. None of these inputs is a secret: any process in the guest may be able to read them.

shed's reference image, built for Compute Engine, reads them from instance metadata and publishes its key as a guest attribute:

| Input | Compute Engine | Source constant |
|---|---|---|
| The shed's name | metadata key `shed-name` | `guest.MetadataShedName` |
| The shed's id | metadata key `shed-id` | `guest.MetadataShedID` |
| The authorized keys, one per line | metadata key `shed-link-authorized-keys` | `guest.MetadataAuthorizedKeys` |
| The published host key | guest attribute `shed/host-key` | `guest.HostKeyAttrNamespace`, `guest.HostKeyAttrKey` |

An image for another cloud keeps every requirement of the first table and replaces only the boot unit's way of reading these inputs and publishing the key.
