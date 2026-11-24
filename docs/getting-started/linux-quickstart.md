# Linux Quickstart

The opinionated, packaged path to a working `shed-server` on Linux, where it runs
[Firecracker](https://firecracker-microvm.github.io/) microVMs. This is the path
shed's Linux testing focuses on: a remote host you reach from a Mac (or another
Linux box) with the `shed` CLI.

Building from source, custom kernels/images, or the full manual setup? See
[Firecracker Developer Setup](fc-setup.md).

!!! note "Scope on Linux"
    Linux runs the **server** (`shed-server`) and the **desktop app**
    (`shed-desktop`, a Tauri client, installed from the same apt repository with
    `sudo apt install shed-desktop`). The desktop app is your credential source
    (it is the only one): it delivers your credentials into new sheds;
    there is no separate daemon. See
    [Desktop installation](../desktop/installation.md).

## Prerequisites

- Linux with **KVM** (`/dev/kvm` present; bare metal or a nested-virt VM).
- root / sudo.
- A private network (Tailscale or LAN) if you'll reach it from another machine.

## 1. Install the server (apt)

Add the apt repository and install `shed-server` — this gives you `apt`-managed
upgrades:

```bash
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://apt.stridelabs.ai/pubkey.gpg | \
  sudo tee /etc/apt/keyrings/apt-charliek.gpg > /dev/null
echo 'deb [signed-by=/etc/apt/keyrings/apt-charliek.gpg] https://apt.stridelabs.ai noble main' | \
  sudo tee /etc/apt/sources.list.d/apt-charliek.list
sudo apt update
sudo apt install shed-server
```

This installs `shed` + `shed-server`, generates a default Firecracker config at
`/etc/shed/server.yaml`, and registers a systemd service.

**Alternative — without the apt repo**, grab the `.deb` from the
[releases page](https://github.com/charliek/shed/releases):

```bash
VERSION=$(gh release view --repo charliek/shed --json tagName -q .tagName | sed 's/^v//')
wget https://github.com/charliek/shed/releases/download/v${VERSION}/shed-server_${VERSION}_amd64.deb
sudo dpkg -i shed-server_${VERSION}_amd64.deb
```

## 2. Set up Firecracker infrastructure

One command provisions everything the Firecracker backend needs — KVM access, the
bridge network, IP forwarding + NAT, and capabilities:

```bash
sudo shed-server setup
```

## 3. Configure the server for network access

The default config at `/etc/shed/server.yaml` binds **loopback only**
(`bind_address` defaults to `127.0.0.1` since v0.7.4), so a fresh install is
reachable only on the box itself. Since you reach this host from a workstation,
edit the config so it faces the network.

!!! warning "Edit the generated file — don't replace it"
    The snippets below show only the keys to **add or change** in the
    generated `/etc/shed/server.yaml`. Keep everything else — in particular
    the generated `firecracker:` block: its path and sizing fields
    (`instance_dir`, `socket_dir`, `default_cpus`, `default_memory_mb`,
    `default_disk_gb`, `console_port`, `vsock_base_cid`) are **required**,
    and a config without them refuses to start, one
    `firecracker config: <field> is required` error at a time.

**Networked — mtls mode** (pinned TLS + a client certificate enrolled over
SSH + an SSH key allowlist; the only mode that may face the network):

```yaml
# Merge into /etc/shed/server.yaml (keep the generated firecracker: block).
# Full reference: https://charliek.github.io/shed/reference/configuration/
name: my-linux-host
ssh_port: 2222
default_backend: firecracker

auth:
  mode: mtls                     # forces SSH enforce + client certificates + TLS-only
  ssh:
    github_users: [your-github-username]   # only these keys may SSH in (and enrol)
bind_address: 0.0.0.0            # face the network (or a specific tailnet/LAN IP)
# https_port defaults to 8443; http_port is not served in mtls mode.
```

**Local only — open mode** (plain HTTP, no credential): the generated config's
default, reachable on the box itself. An open server binds loopback only; a
non-loopback `bind_address` in open mode fails to load. (`auth.mode: token`
and `allow_insecure_exposure` were removed in v0.10.0 and also fail to
load.)

See the [Security Configuration guide](../guides/security-configuration.md) for
the full posture walkthrough and [Configuration](../reference/configuration.md)
for the Firecracker-specific fields (bridge name/CIDR, vsock, resource defaults).

## 4. Start it

```bash
sudo systemctl start shed-server
systemctl status shed-server                  # should be active (running)
# open mode (the default) — check over plain HTTP on the box:
curl -s http://localhost:8080/api/info        # name, version, backend: firecracker
# mtls mode answers only a client that presents a certificate; check it from
# your workstation with `shed server add` (next step) instead.
```

The first `shed create` pulls the matching `shed-fc-full` image automatically
(`pull_policy: missing`). To pre-cache it, run `sudo shed-server pull-images`.

## 5. Connect from your workstation

From the Mac/Linux box that has the `shed` CLI (over Tailscale/LAN). For an
mtls server, `shed server add` pins the SSH host key, enrolls a client
certificate over SSH and pins the TLS cert (your key must be in the
`github_users` allowlist):

```bash
shed server add my-linux-host.example.com --name my-linux-host
shed create demo --repo charliek/your-repo
shed attach demo
```

An **open** server is loopback only, so it is reachable only from the box
itself (`shed server add localhost` there). Several people sharing the server
each get their own sheds with a user list: see the [Multi-user server
guide](../guides/multi-user-server.md).

## Upgrading

```bash
sudo apt update && sudo apt install --only-upgrade shed-server
sudo systemctl restart shed-server
```

Image refs synthesize from the server version, so a fresh shed pulls the matching
image with no config edit — see
[Configuration → Image references](../reference/configuration.md#image-references-and-shedversion).

## Next steps

- [Firecracker Developer Setup](fc-setup.md) — manual setup, custom kernels/images, from source.
- [Firecracker Operations](../reference/fc-operations.md) — running and debugging the FC backend.
- [Configuration](../reference/configuration.md) — all server-config fields.
- [Security Configuration](../guides/security-configuration.md) — server postures, and hardening sheds with `--mounts none` and `--network isolated`.
- [Orchestrated Sheds](../guides/orchestrated-sheds.md) — letting CI or an agent runner drive its own sheds over the API.
