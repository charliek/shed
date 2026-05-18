# Provisioning

Shed supports in-repo provisioning scripts that run automatically when sheds start. These scripts are version-controlled with your code.

Provisioning works with both VM backends:

- **Firecracker**: Hooks execute via vsock
- **VZ**: Hooks execute via vsock (same mechanism as Firecracker)

## Shed Lifecycle

Understanding the full sequence of events during shed operations helps you know what's available at each stage — for example, credentials are set up before hooks run, so your install script can use SSH keys or API tokens.

### Create Sequence

When you run `shed create`, the following steps execute in order:

| Step | Firecracker | VZ |
|------|-------------|-----|
| 1. Storage setup | Copy base rootfs to instance directory | Copy base rootfs to instance directory |
| 2. VM start | Spawn Firecracker process, allocate TAP device and IP, wait for agent health | Spawn vfkit process, wait for agent health |
| 3. Local-dir mount | Not supported | VirtioFS mount at `/workspace` |
| 4. Credential setup | All credentials mounted via 9P | All credentials mounted via VirtioFS |
| 5. Repo clone | `git clone` in `/workspace` (skipped if `--local-dir`) | Same |
| 6. Install hook | Runs via vsock; state file marks completion | Same as Firecracker |
| 7. Startup hook | Runs via vsock | Same as Firecracker |
| 8. Auto-sync | Default [sync](sync.md) profile from `~/.shed/sync.yaml` runs (unless `--no-sync`) | Same |

Steps 1–7 are server-side. Step 8 runs on the CLI client after the server returns.

### Start Sequence

When you run `shed start` on a stopped shed, the sequence is shorter:

| Step | Firecracker | VZ |
|------|-------------|-----|
| 1. VM start | Spawn Firecracker process, wait for agent health | Spawn vfkit process, wait for agent health |
| 2. Local-dir re-mount | Not supported | VirtioFS re-mount (mounts do not persist across VM reboots) |
| 3. Credential refresh | All credentials re-mounted via 9P | All credentials re-mounted via VirtioFS |
| 4. Startup hook | Runs (install hook skipped — state file records it already ran) | Same |

No storage setup, repo clone, install hook, or auto-sync on start.

### Stop Sequence

| Step | Firecracker | VZ |
|------|-------------|-----|
| 1. Shutdown hook | Runs via vsock (budget: half of stop timeout, max 30s) | Same as Firecracker |
| 2. Agent drain | 5-second drain timeout for in-flight operations | Same as Firecracker |
| 3. Process stop | Firecracker API shutdown, SIGKILL fallback | vfkit SIGTERM, then SIGKILL fallback |

### Delete Sequence

`shed delete` calls stop (running the shutdown hook), then removes all resources (instance directory and rootfs).

### Backend Differences at a Glance

| Feature | Firecracker | VZ |
|---------|-------------|-----|
| Credential mechanism | 9P mount | VirtioFS mount |
| Local-dir support | Not supported | VirtioFS |
| Shutdown hook | Supported | Supported |
| Credential live sync | Automatic via 9P | Automatic via VirtioFS |
| Workspace persistence | Rootfs image (survives stop/start) | Rootfs image (survives stop/start) |

### Error Handling

Not all failures during create are fatal:

| Step | On failure |
|------|-----------|
| Storage setup, VM start, agent health check | **Fatal** — create fails, resources cleaned up |
| Local-dir mount (VZ) | **Fatal** — VM stopped, create fails |
| Credential setup | Warning logged, create continues |
| Repo clone | Warning logged, create continues |
| Provisioning hooks | Warning logged, create continues |
| Auto-sync | Warning logged, create continues |

## Quick Start

Create `.shed/provision.yaml` in your repository root:

```yaml
hooks:
  install: scripts/provision/install.sh
  startup: scripts/provision/startup.sh
  shutdown: scripts/provision/shutdown.sh

env:
  MY_VAR: "my_value"
```

## Configuration

### Provision File Location

Place `.shed/provision.yaml` in your repository root. Shed detects and executes it automatically.

### Fields

| Field | Type | Description |
|-------|------|-------------|
| `hooks.install` | string | Script that runs once on shed create |
| `hooks.startup` | string | Script that runs on every start |
| `hooks.shutdown` | string | Script that runs before shed stop |
| `env` | map | Custom environment variables |

## Hooks

### Install Hook

Runs once when the shed is created. Use for one-time setup:

- Installing packages
- Creating databases
- Initial configuration

### Startup Hook

Runs every time the shed starts. Use for:

- Starting services
- Verifying dependencies
- Runtime configuration

### Shutdown Hook

Runs before the shed stops (on `shed stop` and `shed delete`). Use for:

- Gracefully stopping databases (e.g., `pg_ctl stop`)
- Flushing caches (e.g., `redis-cli shutdown`)
- Saving application state

The shutdown hook has a time budget of half the configured stop timeout (capped at 30s). If the hook exceeds this budget or fails, the shed still stops — hook failures are logged as warnings.

**Note:** The shutdown hook is supported on both the Firecracker and VZ backends.

After the shutdown hook completes, the agent enforces a 5-second drain timeout on active connections before the VM exits. This gives in-flight exec and file transfer operations time to finish cleanly.

## PATH Propagation

All shed hooks run as login shells (`bash --login -c`), which source `~/.bash_profile`. The shed base images set up `~/.bash_profile` to source `~/.bashrc`, so tools that add PATH entries to `~/.bashrc` (e.g., bun, nvm) are automatically available to subsequent hooks.

The base images also include `/etc/profile.d/shed-path.sh` which ensures [mise](https://mise.jdx.dev/) shims and `~/.local/bin` are in PATH for login shells.

## Example: PostgreSQL Setup

**`.shed/provision.yaml`:**

```yaml
hooks:
  install: scripts/provision/install.sh
  startup: scripts/provision/startup.sh

env:
  DATABASE_URL: "postgresql://localhost/myapp"
```

**`scripts/provision/install.sh`:**

```bash
#!/bin/bash
set -euo pipefail

# Install PostgreSQL
sudo apt-get update
sudo apt-get install -y postgresql-16

# Create database
sudo -u postgres createdb myapp || true

echo "PostgreSQL installed"
```

**`scripts/provision/startup.sh`:**

```bash
#!/bin/bash
set -euo pipefail

# Clean stale PostgreSQL state from prior stop
sudo rm -rf /var/run/postgresql
sudo mkdir -p /var/run/postgresql
sudo chown postgres:postgres /var/run/postgresql 2>/dev/null || true
sudo rm -f /var/lib/postgresql/16/main/postmaster.pid 2>/dev/null || true

# Start PostgreSQL if not running
if ! pg_isready -q 2>/dev/null; then
    echo "Starting PostgreSQL..."
    sudo pg_ctlcluster 16 main start

    for i in {1..10}; do
        pg_isready -q && break
        sleep 1
    done
fi

echo "PostgreSQL is ready"
```

## Startup Hook Best Practices

### Handling Stale State After Stop/Start

When services aren't stopped gracefully before `shed stop`, they leave stale PID files, sockets, and shared memory. On the next `shed start`, these stale files can prevent services from restarting.

The best approach is to use a **shutdown hook** to stop services gracefully before the VM exits. The startup hook then serves as a safety net for cases where the shutdown hook wasn't available or failed:

```yaml
hooks:
  startup: .shed/scripts/startup.sh
  shutdown: .shed/scripts/shutdown.sh
```

Your startup hook should still clean stale runtime state **before** starting services (backward compatibility):

```bash
#!/bin/bash
set -euo pipefail

# Clean stale PostgreSQL state from prior stop
sudo rm -rf /var/run/postgresql
sudo mkdir -p /var/run/postgresql
sudo chown postgres:postgres /var/run/postgresql 2>/dev/null || true
sudo rm -f /var/lib/postgresql/16/main/postmaster.pid 2>/dev/null || true

# Start PostgreSQL
if ! pg_isready -q 2>/dev/null; then
    sudo pg_ctlcluster 16 main start
fi
```

**Key points:**

- Remove and recreate runtime directories (`/var/run/<service>`) with correct ownership
- Remove stale PID files from data directories (e.g., `postmaster.pid`)
- Guard commands with `2>/dev/null || true` so cleanup is safe on first boot (e.g., `chown` won't fail if the service user doesn't exist yet, `rm` won't fail if PID files are missing)
- This startup-hook stale-state cleanup pattern works identically on Firecracker and VZ

## Environment Variables

### environment.d (VZ and Firecracker)

The shed agent loads environment variables from `/etc/environment.d/*.conf` files following the [systemd environment.d convention](https://www.freedesktop.org/software/systemd/man/latest/environment.d.html). These variables are injected into all exec sessions (`shed exec`, `shed console`, `shed attach`, provisioning hooks).

Files are read in alphabetical order — later files can override values from earlier ones. Each file contains one `KEY=VALUE` pair per line. Blank lines and lines starting with `#` are ignored.

The `full` image variant uses this mechanism to configure shed-extensions:

```dotenv
# /etc/environment.d/shed-extensions.conf
SSH_AUTH_SOCK=/run/shed-extensions/ssh-agent.sock
AWS_CONTAINER_CREDENTIALS_FULL_URI=http://127.0.0.1:499/credentials
```

To add your own environment variables, create a `.conf` file in your image Dockerfile or via a provisioning install hook:

```bash
# In a provisioning install hook
sudo tee /etc/environment.d/90-myapp.conf << 'EOF'
DATABASE_URL=postgresql://localhost/myapp
REDIS_URL=redis://localhost:6379
EOF
```

Use numeric prefixes (e.g., `90-`) to control ordering relative to other files.

### Shed-managed variables

Shed sets these variables automatically:

| Variable | Description |
|----------|-------------|
| `SHED_CONTAINER` | Always `true` in shed containers |
| `SHED_NAME` | Shed name (e.g., `myproject`) |
| `SHED_WORKSPACE` | Workspace path (`/workspace`) |

Add custom variables in `provision.yaml`:

```yaml
env:
  DATABASE_URL: "postgresql://localhost/myapp"
  NODE_ENV: "development"
```

## Skipping Provisioning

```bash
shed create myproject --repo github.com/user/repo --no-provision
```

## Debugging

If provisioning fails, check the logs in the shed:

```bash
shed console myproject
cat /var/log/shed/install.log
cat /var/log/shed/startup.log
cat /var/log/shed/shutdown.log
```

**Common issues:**

- **Script not executable**: Shed automatically runs `chmod +x` before executing
- **Missing dependencies**: Install script should handle all dependencies
- **Non-zero exit**: Hook failures are logged as warnings but shed creation continues

!!! tip "Environment Detection"
    Check if running in a shed container using `[ "$SHED_CONTAINER" = "true" ]`.
