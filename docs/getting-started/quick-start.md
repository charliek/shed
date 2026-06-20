# Getting Started

Shed runs persistent, VM-based dev environments you reach over SSH. Start with
the packaged quickstart for your platform, then use the reference below for the
day-to-day commands.

## Choose your path

**Quickstarts (packaged, recommended)** — install from a package manager, copy a
config, create a shed:

- **[macOS Quickstart](macos-quickstart.md)** — Homebrew + the shed-desktop
  approval app; runs VMs via Apple's Virtualization.framework (VZ backend).
- **[Linux Quickstart](linux-quickstart.md)** — the `shed-server` deb running
  Firecracker microVMs (the tested remote-host path).

**Developer setup** — build from source, custom images, or the full manual setup:

- **[macOS Developer Setup](vz-setup.md)** (VZ backend)
- **[Firecracker Developer Setup](fc-setup.md)** (Linux / KVM backend)

The rest of this page is the platform-agnostic command reference once a server is
running.

## Add a Server

Register a server that has `shed-server` running. For a **local** server (the
installed default — open, bound to loopback):

```bash
shed server add localhost --name my-server
```

For a server you reach **over the network**, the server must bind a non-loopback
`bind_address` (since v0.7.4 the default is loopback-only). Secure mode is the
preferred networked posture — pin its TLS cert and mint a token over SSH:

```bash
shed server add my-server.tailnet.ts.net --https-port 8443 --name my-server
```

For an open server on a trusted private network (configured with
`allow_insecure_exposure: true`), drop `--https-port`:
`shed server add my-server.tailnet.ts.net --name my-server`. See the
[Security Configuration guide](../guides/security-configuration.md) for the
server-side posture setup.

This connects to the server, retrieves its SSH host key (and, in secure mode,
pins the TLS cert and mints your token), and saves the configuration.

## Create a Shed

```bash
# Create an empty shed — you land in /home/shed
shed create my-project

# Or clone a repository — cloned into ~/<reponame>, login lands there
shed create my-project --repo git@github.com:user/repo.git

# Or mount a local directory — mounted at ~/<basename>, login lands there
shed create my-project --local-dir ~/projects/my-project

# Mount additional reference dirs alongside the primary --local-dir
# (each lands at ~/<basename>; repeatable; --add-dir requires --local-dir)
shed create app --local-dir ~/projects/app --add-dir ~/projects/shared-lib
```

Interactive logins (`shed console`, `shed attach`, raw `ssh`, VS Code Remote-SSH)
land in the shed's project directory: `~/<reponame>` with `--repo`, `~/<basename>`
with `--local-dir`, otherwise `/home/shed`. `shed exec <name> -- <cmd>` runs there
too, with `$SHED_WORKSPACE` (the project dir) and `$SHED_ADD_DIRS` (any `--add-dir`
mounts) set. `--repo` and `--local-dir`/`--add-dir` are mutually exclusive.

Once you have a few sheds, `shed system df` shows what's on disk and
`shed system prune` reclaims unused space. See [Disk Management](../reference/disk-management.md).

## Connect

### Direct Shell

```bash
shed console my-project
```

Opens a bash shell in the VM. Exits when you disconnect.

### Persistent Session

```bash
shed attach my-project
```

Opens a tmux session that persists after you disconnect. Detach with `Ctrl-B D`
and reconnect later with the same command.

## IDE Integration

Generate SSH config entries for VS Code or Cursor:

```bash
# Preview the config
shed ssh-config my-project

# Install to ~/.ssh/config
shed ssh-config --all --install
```

Then connect using VS Code Remote-SSH to `shed-my-project`.

## Common Workflows

### Run a coding agent

```bash
# Create a shed and attach to a persistent session
shed create myproj --repo user/repo
shed attach myproj

# Inside the session, start Claude Code
claude

# Detach with Ctrl-B D - the agent keeps running; reattach later
shed attach myproj
```

### Port forwarding

```bash
# Start tunnels for web development (-d runs in background)
shed tunnels start myproj -t 3000:3000 -d
```

## Next Steps

- [CLI Reference](../reference/cli.md) — all available commands
- [Configuration](../reference/configuration.md) — client and server config options
- [Extensions](../reference/extensions.md) — credential brokering with the host agent
- [Tunnels](../reference/tunnels.md) — port forwarding configuration
- [Provisioning](../reference/provisioning.md) — `.shed/` install/startup hooks
