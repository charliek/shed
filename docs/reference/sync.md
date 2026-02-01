# File Sync

Shed supports syncing files from your local machine to shed containers. This is useful for development certificates, dotfiles, SSH keys, and custom scripts.

## Quick Start

Sync files using the default profile:

```bash
shed sync myproj
```

Preview without syncing:

```bash
shed sync myproj --dry-run
```

## Configuration

Create `~/.shed/sync.yaml` on your local machine:

```yaml
features:
  devproxy:
    description: "Sync mkcert certificates for HTTPS development"
    paths:
      - source: ~/.local/share/mkcert/rootCA.pem
        target: /usr/local/share/ca-certificates/mkcert-ca.crt
      - source: ~/.devproxy/certs
        target: /etc/ssl/devproxy
        include: "*.pem"
    postSync:
      - run: update-ca-certificates

  dotfiles:
    description: "Sync shell config and git settings"
    paths:
      - source: ~/.gitconfig
        target: /root/.gitconfig
      - source: ~/.bashrc
        target: /root/.bashrc

  scripts:
    description: "Sync custom scripts"
    paths:
      - source: ~/bin
        target: /usr/local/bin
        include: "*.sh"

profiles:
  default:
    features: [devproxy]

  full:
    features: [devproxy, dotfiles, scripts]
```

## Features

A feature is a self-contained sync unit with paths and optional post-sync commands.

### Feature Fields

| Field | Type | Description |
|-------|------|-------------|
| `description` | string | Human-readable description |
| `paths` | list | Files or directories to sync |
| `postSync` | list | Commands to run after syncing |

### Path Fields

| Field | Type | Description |
|-------|------|-------------|
| `source` | string | Local path (supports `~` expansion) |
| `target` | string | Remote path in container |
| `include` | string | Optional glob pattern to filter files |

## Profiles

Profiles group features together for common sync scenarios.

```yaml
profiles:
  default:
    features: [devproxy]

  full:
    features: [devproxy, dotfiles, scripts]
```

- The `default` profile syncs automatically on `shed create`
- Use `--profile` to specify a different profile
- Use `--feature` to sync a single feature

## Commands

### Sync Files

```bash
shed sync <name> [flags]
```

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--profile` | `-p` | `default` | Sync profile to use |
| `--feature` | `-f` | | Sync single feature |
| `--dry-run` | | `false` | Preview without syncing |

**Examples:**

```bash
# Sync default profile
shed sync myproj

# Sync specific profile
shed sync myproj -p full

# Sync single feature
shed sync myproj -f devproxy

# Preview without syncing
shed sync myproj --dry-run
```

### Auto-Sync on Create

```bash
# Auto-sync with default profile
shed create myproj --repo user/repo

# Create with specific profile
shed create myproj --sync-profile full

# Skip sync on create
shed create myproj --no-sync
```

## Example: Development Certificates

Sync mkcert certificates so containers trust your local CA:

```yaml
features:
  devproxy:
    description: "Sync mkcert certificates for devproxy"
    paths:
      - source: ~/.local/share/mkcert/rootCA.pem
        target: /usr/local/share/ca-certificates/mkcert-ca.crt
      - source: ~/.devproxy/certs
        target: /etc/ssl/devproxy
        include: "*.pem"
    postSync:
      - run: update-ca-certificates

profiles:
  default:
    features: [devproxy]
```

Verify the CA was added:

```bash
shed exec myproj "cat /etc/ssl/certs/ca-certificates.crt | grep -c mkcert"
```

## Logs

Sync output is saved to `/var/log/shed/sync.log` in the container:

```bash
shed console myproj
cat /var/log/shed/sync.log
```
