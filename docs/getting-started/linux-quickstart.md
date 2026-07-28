# Linux Quickstart

The opinionated, packaged path to a working `shed-server` on Linux, where it runs
[Firecracker](https://firecracker-microvm.github.io/) microVMs. This is the path
shed's Linux testing focuses on: a remote host you reach from a Mac (or another
Linux box) with the `shed` CLI.

Building from source, custom kernels/images, or the full manual setup? See
[Firecracker Developer Setup](fc-setup.md).

!!! note "Scope on Linux today"
    Linux currently runs the **server** (`shed-server`). Host-side credential
    brokering (`shed-host-agent`) and a desktop GUI are roadmap items — a headless
    Linux host-agent is planned first, once the macOS pieces are established. For
    now, credential brokering and the shed-desktop approval UI are macOS-only (see
    the [macOS Quickstart](macos-quickstart.md)).

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

**Recommended — token mode** (pinned TLS + minted bearer tokens + an SSH key
allowlist; the preferred posture for anything networked):

```yaml
# /etc/shed/server.yaml
# Full reference: https://charliek.github.io/shed/reference/configuration/
name: my-linux-host
ssh_port: 2222
default_backend: firecracker

auth:
  mode: token                    # forces SSH enforce + HTTP tokens + TLS-only
  ssh:
    github_users: [your-github-username]   # only these keys may SSH in (and mint tokens)
bind_address: 0.0.0.0            # face the network (or a specific tailnet/LAN IP)
# https_port defaults to 8443; http_port is optional in token mode.

firecracker:
  pull_policy: missing
  # default_image / image_aliases omitted -> synthesized from the server version.
```

**Alternative — open on a trusted private network** (plaintext; Tailscale / LAN
only, where the network is the trust boundary):

```yaml
# /etc/shed/server.yaml
name: my-linux-host
http_port: 8080
ssh_port: 2222
default_backend: firecracker

bind_address: 0.0.0.0            # or a specific tailnet/LAN IP
allow_insecure_exposure: true    # required: acknowledge a non-loopback bind with no TLS

firecracker:
  pull_policy: missing
```

See the [Security Configuration guide](../guides/security-configuration.md) for
the full posture walkthrough and [Configuration](../reference/configuration.md)
for the Firecracker-specific fields (bridge name/CIDR, vsock, resource defaults).

## 4. Start it

```bash
sudo systemctl start shed-server
systemctl status shed-server                  # should be active (running)
# token mode (TLS-only) — check over HTTPS on the box:
curl -sk https://localhost:8443/api/info      # name, version, backend: firecracker
# open mode instead? use: curl -s http://localhost:8080/api/info
```

The first `shed create` pulls the matching `shed-fc-full` image automatically
(`pull_policy: missing`). To pre-cache it, run `sudo shed-server pull-images`.

## 5. Connect from your workstation

From the Mac/Linux box that has the `shed` CLI (over Tailscale/LAN). For a
token-mode server, `shed server add --https-port` pins the TLS cert + SSH host key and mints
your token over SSH (your key must be in the `github_users` allowlist):

```bash
shed server add my-linux-host.tailnet.ts.net --https-port 8443 --name my-linux-host
shed create demo --repo charliek/your-repo
shed attach demo
```

For an **open** server (the LAN alternative above), drop `--https-port`:
`shed server add my-linux-host.tailnet.ts.net --name my-linux-host`.

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
