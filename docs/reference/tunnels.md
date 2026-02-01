# Tunnels

Shed supports SSH tunnels for port forwarding, allowing you to access services running in shed containers from your local machine.

## Quick Start

Forward port 3000 from a shed to your local machine:

```bash
shed tunnels start myproj -t 3000:3000
```

Run in background:

```bash
shed tunnels start myproj -t 3000:3000 -d
```

## Configuration

Create `~/.shed/tunnels.yaml` to define reusable tunnel profiles:

```yaml
profiles:
  webdev:
    description: "Web development ports"
    ports:
      - local: 3000
        remote: 3000
      - local: 5173
        remote: 5173

  database:
    description: "Database access"
    ports:
      - local: 5432
        remote: 5432
      - local: 6379
        remote: 6379

  full:
    description: "All development ports"
    ports:
      - local: 3000
        remote: 3000
      - local: 5173
        remote: 5173
      - local: 5432
        remote: 5432
      - local: 6379
        remote: 6379
      - local: 8080
        remote: 8080
```

## Commands

### Start Tunnels

```bash
shed tunnels start <shed> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--profile` | `-p` | None | Use a defined profile (repeatable) |
| `--tunnel` | `-t` | None | Port mapping (local:remote or just port) |
| `--background` | `-d` | `false` | Run in background |
| `--replace` | | `false` | Replace existing tunnel without prompting |

**Examples:**

```bash
# Single port
shed tunnels start myproj -t 3000:3000

# Port shorthand (equivalent to -t 3000:3000)
shed tunnels start myproj -t 3000

# Multiple ports
shed tunnels start myproj -t 3000:3000 -t 5432:5432

# Using a profile
shed tunnels start myproj -p webdev

# Merging multiple profiles
shed tunnels start myproj -p webdev -p database

# Background mode
shed tunnels start myproj -p webdev -d
```

### Stop Tunnels

```bash
shed tunnels stop [shed] [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--all` | | `false` | Stop all tunnels |

**Examples:**

```bash
# Stop tunnels for specific shed
shed tunnels stop myproj

# Stop all tunnels
shed tunnels stop --all
```

### List Tunnels

```bash
shed tunnels list [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--verbose` | `-v` | `false` | Show detailed info |
| `--json` | | `false` | Output as JSON |

**Output:**

```
SHED        LOCAL    REMOTE   STATUS
myproj      3000     3000     active
myproj      5432     5432     active
```

### Preview Configuration

```bash
shed tunnels config <shed> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--profile` | `-p` | None | Profile to preview |
| `--tunnel` | `-t` | None | Additional tunnels to include |

Shows what tunnels would be created based on the configuration.

## Configuration Reference

### Profile Fields

| Field | Type | Description |
|-------|------|-------------|
| `description` | string | Human-readable description |
| `ports` | list | Port mappings |
| `ports[].local` | int | Local port to listen on |
| `ports[].remote` | int | Remote port in container |

## Common Use Cases

### Web Development

Forward a development server:

```bash
shed tunnels start myproj -t 3000:3000 -d
# Access at http://localhost:3000
```

### Database Access

Connect to PostgreSQL in a shed:

```bash
shed tunnels start myproj -t 5432:5432 -d
psql -h localhost -p 5432 -U postgres mydb
```

### Multiple Services

Forward all development ports:

```bash
shed tunnels start myproj -p full -d
```

## Background Mode

When running with `-d`, tunnels run as a background process:

- Tunnels persist until explicitly stopped
- Use `shed tunnels list` to see active tunnels
- Use `shed tunnels stop` to terminate

## Port Conflicts

If a local port is already in use, the tunnel will fail to start. Check for conflicts:

```bash
lsof -i :3000
```

Use a different local port if needed:

```bash
shed tunnels start myproj -t 3001:3000
```
