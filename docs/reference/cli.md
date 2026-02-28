# CLI Reference

Complete reference for the `shed` command-line interface.

## Global Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--server` | `-s` | Target server (overrides default) |
| `--verbose` | `-v` | Enable debug output |
| `--config` | `-c` | Config file path (default: `~/.shed/config.yaml`) |
| `--json` | | Emit structured JSON to stdout (suppresses verbose output) |

## Server Management

### shed server add

Adds a new server to the client configuration.

```bash
shed server add <host> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--name` | `-n` | Derived from host | Friendly name for server |
| `--port` | `-p` | `8080` | HTTP API port |

The SSH port is automatically discovered from the server's `/api/info` endpoint.

**Example:**

```bash
shed server add mini-desktop.tailnet.ts.net --name mini
```

### shed server list

Lists configured servers.

```bash
shed server list
```

**Output:**

```
NAME            HOST                              HTTP    SSH     STATUS     DEFAULT
mini-desktop    mini-desktop.tailnet.ts.net       8080    2222    online     *
cloud-vps       vps.tailnet.ts.net                8080    2222    offline
```

### shed server remove

Removes a server from configuration.

```bash
shed server remove <name>
```

### shed server set-default

Sets the default server.

```bash
shed server set-default <name>
```

## Shed Lifecycle

### shed create

Creates a new shed.

```bash
shed create <name> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--repo` | `-r` | None | Repository to clone (`owner/repo` shorthand or full URL) |
| `--server` | `-s` | Default server | Target server |
| `--image` | `-i` | Server default | Base Docker image |
| `--no-provision` | | `false` | Skip provisioning hooks |
| `--sync-profile` | | `default` | Profile to sync after creation |
| `--no-sync` | | `false` | Skip syncing default profile |
| `--timeout` | | `10m` | Timeout for create operation (e.g., `30s`, `5m`, `2h`) |

**Examples:**

```bash
shed create scratch
shed create codelens --repo charliek/codelens
shed create stbot --repo charliek/stbot --server cloud-vps
shed create myproj --sync-profile full
shed create myproj --no-sync
shed create bigproj --repo org/large-repo --timeout 30m
```

### shed list

Lists sheds.

```bash
shed list [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--server` | `-s` | Default | List from specific server |
| `--all` | `-a` | false | List from all servers |

**Output:**

```
SERVER          NAME          STATUS     CREATED          REPO
mini-desktop    codelens      running    2 hours ago      charliek/codelens
mini-desktop    mcp-test      stopped    3 days ago       -
```

### shed start

Starts a stopped shed.

```bash
shed start <name> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--timeout` | | `10m` | Timeout for start operation (e.g., `30s`, `5m`, `2h`) |

### shed stop

Stops a running shed.

```bash
shed stop <name>
```

### shed delete

Deletes a shed.

```bash
shed delete <name> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--keep-volume` | | `false` | Preserve workspace data |
| `--force` | `-f` | `false` | Skip confirmation |

**Note:** When using `--json`, the `--force` flag is required (interactive confirmation is not supported in JSON mode).

## Interactive Access

### shed console

Opens an interactive shell in a shed.

```bash
shed console <name>
```

Opens `/bin/bash` in the container. If the shed is stopped, it will be started automatically.

### shed exec

Executes a command in a shed.

```bash
shed exec <name> <command...>
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--session` | `-S` | None | Run in tmux session context |

**Examples:**

```bash
shed exec codelens git status
shed exec codelens "cd /workspace && npm test"
shed exec codelens --session default git status
```

### shed attach

Attaches to a tmux session. Creates the session if it doesn't exist.

```bash
shed attach <name> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--session` | `-S` | `default` | Session name |
| `--new` | | `false` | Force create new session |

**Examples:**

```bash
shed attach codelens
shed attach codelens --session debug
shed attach codelens --new --session experiment
```

Detach with `Ctrl-B D`.

## Session Management

### shed sessions

Lists tmux sessions.

```bash
shed sessions [shed-name] [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--all` | `-a` | `false` | List from all servers |

**Examples:**

```bash
shed sessions                  # All sessions on default server
shed sessions myproj           # Sessions in specific shed
shed sessions --all            # Across all servers
```

**Output:**

```
SHED        SESSION      STATUS      CREATED      WINDOWS
codelens    default      attached    2h ago       1
codelens    debug        detached    30m ago      2
```

### shed sessions kill

Terminates a tmux session.

```bash
shed sessions kill <shed-name> <session-name>
```

## Port Forwarding

### shed tunnels start

Starts SSH tunnels for port forwarding.

```bash
shed tunnels start <shed> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--profile` | `-p` | None | Use tunnel profile |
| `--tunnel` | `-t` | None | Port mapping (local:remote) |
| `--background` | `-d` | `false` | Run in background |
| `--replace` | | `false` | Replace existing tunnel without prompting |

**Examples:**

```bash
shed tunnels start myproj -t 3000:3000
shed tunnels start myproj -t 3000:3000 -t 5432:5432
shed tunnels start myproj -p webdev -d
shed tunnels start myproj -p webdev --replace
```

### shed tunnels stop

Stops tunnels.

```bash
shed tunnels stop [shed] [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--all` | | `false` | Stop all tunnels |

### shed tunnels list

Lists active tunnels.

```bash
shed tunnels list [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--verbose` | `-v` | `false` | Show detailed info |

### shed tunnels config

Previews tunnel configuration.

```bash
shed tunnels config <shed> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--profile` | `-p` | None | Profile to preview |
| `--tunnel` | `-t` | None | Additional tunnels to include |

## File Sync

### shed sync

Syncs local files to a shed container.

```bash
shed sync <name> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--profile` | `-p` | `default` | Sync profile to use |
| `--feature` | `-f` | None | Sync single feature |
| `--dry-run` | | `false` | Preview without syncing |

**Examples:**

```bash
shed sync myproj
shed sync myproj -p full
shed sync myproj -f devproxy
shed sync myproj --dry-run
```

## IDE Integration

### shed ssh-config

Generates or installs SSH config for IDE integration.

```bash
shed ssh-config [name] [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--all` | | `false` | Generate for all known sheds |
| `--install` | | `false` | Write to `~/.ssh/config` |
| `--dry-run` | | `false` | Show changes without applying |
| `--uninstall` | | `false` | Remove entries |

**Examples:**

```bash
shed ssh-config codelens              # Print config for one shed
shed ssh-config --all --install --dry-run  # Preview changes
shed ssh-config --all --install       # Apply changes
shed ssh-config --uninstall           # Remove managed block
```

## Utility

### shed version

Shows version information.

```bash
shed version [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--verbose` | `-v` | `false` | Show full version info including dependencies |
