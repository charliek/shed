# Shed Provisioning

Shed supports two types of provisioning to customize container environments:

1. **In-Repo Provisioning** - Scripts that live in your repository and run at container lifecycle events
2. **Client-Side File Sync** - Sync files from your local machine to containers (certificates, dotfiles, etc.)

## In-Repo Provisioning

In-repo provisioning lets you define scripts that run automatically when containers start. These scripts are version-controlled with your code.

### Configuration

Create `.shed/provision.yaml` in your repository root:

```yaml
# .shed/provision.yaml

hooks:
  install: scripts/provision/install.sh   # Runs once on shed create
  startup: scripts/provision/startup.sh   # Runs on every start

env:
  MY_VAR: "my_value"   # Custom environment variables
```

### Example: PostgreSQL Setup

Install script (runs once):

```bash
#!/bin/bash
# scripts/provision/install.sh
set -euo pipefail

# Install PostgreSQL
sudo apt-get update
sudo apt-get install -y postgresql-16

# Create database
sudo -u postgres createdb myapp || true

echo "PostgreSQL installed"
```

Startup script (runs on every start):

```bash
#!/bin/bash
# scripts/provision/startup.sh
set -euo pipefail

# Start PostgreSQL if not running
if ! pg_isready -q 2>/dev/null; then
    echo "Starting PostgreSQL..."
    sudo pg_ctlcluster 16 main start

    # Wait for ready
    for i in {1..10}; do
        pg_isready -q && break
        sleep 1
    done
fi

echo "PostgreSQL is ready"
```

### Environment Variables

Shed sets these environment variables automatically:

| Variable | Description |
| -------- | ----------- |
| `SHED_CONTAINER` | Always `true` in shed containers |
| `SHED_NAME` | Container name (e.g., `myproject`) |
| `SHED_WORKSPACE` | Workspace path (`/workspace`) |

Add custom variables in `provision.yaml`:

```yaml
env:
  DATABASE_URL: "postgresql://localhost/myapp"
  NODE_ENV: "development"
```

### Lifecycle

```text
shed create myproject --repo github.com/user/repo
    │
    ├── Container created and started
    ├── Repository cloned to /workspace
    ├── Load .shed/provision.yaml
    ├── Run install hook (logged to /var/log/shed/install.log)
    ├── Run startup hook (logged to /var/log/shed/startup.log)
    └── Ready

shed start myproject
    │
    ├── Container started
    ├── Run startup hook only
    └── Ready
```

### Debugging

If provisioning fails, logs are saved in the container:

```bash
# Connect to container
shed console myproject

# Check logs
cat /var/log/shed/install.log
cat /var/log/shed/startup.log
```

Common issues:
- **Script not executable**: Shed automatically runs `chmod +x` before executing
- **Missing dependencies**: Install script should handle all dependencies
- **Non-zero exit**: Hook failures are logged as warnings but container creation continues. Check logs for details.

### Skipping Provisioning

```bash
# Skip provisioning hooks during create
shed create myproject --repo github.com/user/repo --no-provision
```

## Client-Side File Sync

Sync files from your local machine to shed containers. Useful for:
- Development certificates (mkcert, devproxy)
- Dotfiles (.gitconfig, .bashrc)
- SSH keys and credentials
- Custom scripts

### Configuration

Create `~/.shed/sync.yaml` on your local machine:

```yaml
# ~/.shed/sync.yaml

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

### Features

A feature is a self-contained sync unit with:
- **paths**: Files or directories to sync
- **postSync**: Commands to run after syncing

Path mapping options:
- `source`: Local path (supports `~` expansion)
- `target`: Remote path in container
- `include`: Optional glob pattern to filter files

### Profiles

Profiles group features together:
- `default` profile syncs automatically on `shed create`
- Use `--profile` to specify a different profile
- Use `--feature` to sync a single feature

### Usage

```bash
# Sync default profile
shed sync myproject

# Sync specific profile
shed sync myproject --profile full

# Sync single feature
shed sync myproject --feature devproxy

# Preview without syncing
shed sync myproject --dry-run

# Auto-sync on create (uses default profile)
shed create myproject --repo github.com/user/repo

# Create with specific profile
shed create myproject --sync-profile full

# Skip sync on create
shed create myproject --no-sync
```

### Example: devproxy Certificates

Sync mkcert certificates so containers trust your local CA:

```yaml
features:
  devproxy:
    description: "Sync mkcert certificates for devproxy"
    paths:
      # Root CA for trust
      - source: ~/.local/share/mkcert/rootCA.pem
        target: /usr/local/share/ca-certificates/mkcert-ca.crt
      # Individual certs
      - source: ~/.devproxy/certs
        target: /etc/ssl/devproxy
        include: "*.pem"
    postSync:
      - run: update-ca-certificates

profiles:
  default:
    features: [devproxy]
```

After syncing:
```bash
# Verify CA was added
ssh -p 2222 myproject@server "cat /etc/ssl/certs/ca-certificates.crt | grep -c mkcert"
```

### How It Works

1. Creates tar archive of source paths (preserves permissions with `tar -p`)
2. Transfers via SSH pipe to container
3. Extracts with `tar xzpf` (preserves executable bits)
4. Runs postSync hooks in order

### Logs

Sync output is saved to `/var/log/shed/sync.log` in the container.

## Environment Detection

Scripts can check the environment to detect when running in a Shed container:

```bash
#!/bin/bash
# scripts/provision/startup.sh

if [ "$SHED_CONTAINER" = "true" ]; then
    echo "Running in Shed container: $SHED_NAME"
fi

# Startup logic...
```
