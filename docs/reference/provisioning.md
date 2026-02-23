# Provisioning

Shed supports in-repo provisioning scripts that run automatically when sheds start. These scripts are version-controlled with your code.

Provisioning works with both backends:

- **Docker**: Hooks execute via `docker exec`
- **Firecracker**: Hooks execute via vsock

## Quick Start

Create `.shed/provision.yaml` in your repository root:

```yaml
hooks:
  install: scripts/provision/install.sh
  startup: scripts/provision/startup.sh

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

## PATH Propagation

After the install hook completes, shed captures the PATH (including any additions made by installers to `~/.bashrc`) and persists it to `/etc/profile.d/shed-installed-tools.sh`. This ensures tools installed by the install hook are available to the startup hook and subsequent commands.

For example, if your install hook runs `curl -fsSL https://bun.sh/install | bash`, bun's installer adds `~/.bun/bin` to `~/.bashrc`. Shed detects this and writes:

```bash
export PATH="/root/.bun/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin"
```

to `/etc/profile.d/shed-installed-tools.sh`. Since startup hooks run as login shells (`bash --login -c`), they automatically source this file and inherit the installed tools.

Shed also detects [mise](https://mise.jdx.dev/) shims. If your install hook uses `mise use --global` to install tools, the mise shims directory (`~/.local/share/mise/shims`) is automatically included in the captured PATH.

**Debugging**: If tools installed by the install hook aren't found during the startup hook, check the captured PATH:

```bash
shed exec myproject -- cat /etc/profile.d/shed-installed-tools.sh
```

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

## Environment Variables

Shed sets these variables automatically:

| Variable | Description |
|----------|-------------|
| `SHED_CONTAINER` | Always `true` in shed containers |
| `SHED_NAME` | Container name (e.g., `myproject`) |
| `SHED_WORKSPACE` | Workspace path (`/workspace`) |

Add custom variables in `provision.yaml`:

```yaml
env:
  DATABASE_URL: "postgresql://localhost/myapp"
  NODE_ENV: "development"
```

## Lifecycle

On `shed create`: The container starts, the repository is cloned, then the install hook runs followed by the startup hook.

On `shed start`: Only the startup hook runs.

## Skipping Provisioning

```bash
shed create myproject --repo github.com/user/repo --no-provision
```

## Debugging

If provisioning fails, check the logs in the container:

```bash
shed console myproject
cat /var/log/shed/install.log
cat /var/log/shed/startup.log
```

**Common issues:**

- **Script not executable**: Shed automatically runs `chmod +x` before executing
- **Missing dependencies**: Install script should handle all dependencies
- **Non-zero exit**: Hook failures are logged as warnings but container creation continues

!!! tip "Environment Detection"
    Check if running in a shed container using `[ "$SHED_CONTAINER" = "true" ]`.
