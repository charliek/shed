# Tunnels

Shed tunnels provide port forwarding from your local machine to services running inside shed VMs. Tunnels use the shed-server [Connect API](api.md#connect-api) to establish TCP streams into VMs, working identically on both VZ and Firecracker backends.

## Quick Start

Forward port 3000 from a shed to your local machine:

```bash
shed tunnels start myproj -t 3000
```

Run in background:

```bash
shed tunnels start myproj -t 3000 -d
```

## How It Works

When you start a tunnel, the CLI opens a local TCP listener on the specified port. For each incoming connection, it establishes a tunnel to the shed VM via the shed-server Connect API (`GET /api/sheds/{name}/connect/{port}`). Traffic flows bidirectionally between the local port and the VM service.

```text
localhost:3000  -->  shed-server Connect API  -->  VM service :3000
```

## Configuration

Create `~/.shed/tunnels.yaml` to define reusable tunnel profiles:

```yaml
sheds:
  myproj:
    profiles:
      webdev:
        - "3000"
        - "5173"

      database:
        - "5432"
        - "6379"

      full:
        - "3000"
        - "5173"
        - "5432"
        - "6379"
        - "8080"
```

## Commands

### Start Tunnels

```bash
shed tunnels start <shed> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--profile` | `-p` | None | Use a defined profile (repeatable) |
| `--tunnel` | `-t` | None | Port mapping (`local:remote` or just `port`) |
| `--background` | `-d` | `false` | Run in background |
| `--replace` | | `false` | Replace existing tunnel without prompting |

**Examples:**

```bash
# Single port
shed tunnels start myproj -t 3000

# Different local and remote ports
shed tunnels start myproj -t 4501:4096

# Multiple ports
shed tunnels start myproj -t 3000 -t 5432

# Using a profile
shed tunnels start myproj -p webdev

# Merging profiles with extra ports
shed tunnels start myproj -p webdev -t 5432 -d
```

### Stop Tunnels

```bash
shed tunnels stop [shed] [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--all` | `false` | Stop all tunnels |

### List Tunnels

```bash
shed tunnels list [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--verbose` | `-v` | `false` | Show detailed info (PID, ports, server) |
| `--json` | | `false` | Output as JSON |

### Preview Configuration

```bash
shed tunnels config <shed> [flags]
```

Shows the port mappings and server address that would be used, without starting tunnels.

## Foreground Mode

Without `-d`, `shed tunnels start` runs in the foreground and prints the active
forwards. Press `Ctrl+C` to stop — open connections are torn down and the
command exits promptly. (A second `Ctrl+C` force-quits if a connection is slow
to close.)

## Background Mode

With `-d`, `shed tunnels start` detaches: it starts the tunnels in a separate
daemon process, prints the forwards, and returns your shell. The daemon keeps
running after the launching terminal is closed.

- `shed tunnels start <shed> -d` returns once the tunnels are listening (it
  reports a real error if startup fails, rather than hanging).
- The daemon's PID is recorded in `~/.shed/tunnel-state.json`.
- Use `shed tunnels list` to see active tunnels and `shed tunnels stop <shed>`
  to terminate one (it signals the daemon).
- Dead tunnel daemons are cleaned out of the state file on the next `list` or
  `start`.
- Each daemon writes connection errors to `~/.shed/logs/tunnel-<shed>.log`. The
  file is truncated on each start and is not rotated (it only records errors, so
  it stays small in normal use).

## Port Conflicts

If a local port is already in use, the tunnel will fail to start with a descriptive error. Use a different local port:

```bash
shed tunnels start myproj -t 3001:3000
```

## Common Use Cases

### Web Development

```bash
shed tunnels start myproj -t 3000 -d
# Access at http://localhost:3000
```

### Database Access

```bash
shed tunnels start myproj -t 5432 -d
psql -h localhost -p 5432 -U postgres mydb
```

### Multiple Services

```bash
shed tunnels start myproj -p full -d
```
