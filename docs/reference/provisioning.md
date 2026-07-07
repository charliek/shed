# Provisioning

Shed supports in-repo provisioning scripts that run automatically when sheds start. These scripts are version-controlled with your code.

Provisioning works with both VM backends:

- **Firecracker**: Hooks execute via vsock
- **VZ**: Hooks execute via vsock (same mechanism as Firecracker)

## Shed Lifecycle

Understanding the full sequence of events during shed operations helps you know what's available at each stage — for example, mounts are set up before hooks run, so your install script can use SSH keys or API tokens.

Hooks run in — and `.shed/provision.yaml` is read from — the shed's
**landing directory** (the project directory):

| Create flag | Landing directory |
|-------------|-------------------|
| `--repo <url>` | `/home/shed/<reponame>` (the clone target) |
| `--local-dir <hostdir>` | `/home/shed/<basename>` (the mount point) |
| neither | `/home/shed` |

The `SHED_WORKSPACE` environment variable equals this landing directory.

### Create Sequence

When you run `shed create`, the following steps execute in order:

| Step | Firecracker | VZ |
|------|-------------|-----|
| 1. Storage setup | Copy base rootfs to instance directory | Copy base rootfs to instance directory |
| 2. VM start | Spawn Firecracker process, allocate TAP device and IP, wait for agent health | Spawn vfkit process, wait for agent health |
| 3. Local-dir mount | 9P mount at `/home/shed/<basename>` (plus each `--add-dir`) | VirtioFS mount at `/home/shed/<basename>` (plus each `--add-dir`) |
| 4. Mount setup | All `mounts:` entries mounted via 9P | All `mounts:` entries mounted via VirtioFS |
| 5. Repo clone | `git clone` into `/home/shed/<reponame>` (skipped if `--local-dir`) | Same |
| 6. Install hook | Runs via vsock; state file marks completion | Same as Firecracker |
| 7. Startup hook | Runs via vsock | Same as Firecracker |
| 8. Auto-sync | Default [sync](sync.md) profile from `~/.shed/sync.yaml` runs (unless `--no-sync`) | Same |

Steps 1–7 are server-side. Step 8 runs on the CLI client after the server returns.

### Start Sequence

When you run `shed start` on a stopped shed, the sequence is shorter:

| Step | Firecracker | VZ |
|------|-------------|-----|
| 1. VM start | Spawn Firecracker process, wait for agent health | Spawn vfkit process, wait for agent health |
| 2. Local-dir re-mount | 9P re-mount (mounts do not persist across VM reboots) | VirtioFS re-mount (mounts do not persist across VM reboots) |
| 3. Mount refresh | All `mounts:` entries re-mounted via 9P | All `mounts:` entries re-mounted via VirtioFS |
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
| Mount mechanism | 9P mount | VirtioFS mount |
| Local-dir / add-dir support | 9P | VirtioFS |
| Shutdown hook | Supported | Supported |
| Mount live sync | Automatic via 9P | Automatic via VirtioFS |
| Home-directory persistence | Rootfs image (survives stop/start) | Rootfs image (survives stop/start) |

### Error Handling

Not all failures during create are fatal:

| Step | On failure |
|------|-----------|
| Storage setup, VM start, agent health check | **Fatal** — create fails, resources cleaned up |
| Local-dir mount | **Fatal** — VM stopped, create fails |
| Mount setup | Warning logged, create continues |
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

Place `.shed/provision.yaml` in your repository root. Shed reads it from
the shed's landing directory — `/home/shed/<reponame>` for `--repo`,
`/home/shed/<basename>` for `--local-dir`, otherwise `/home/shed` — and
hooks execute from that same directory. Shed detects and executes it
automatically.

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

## Installing Language Toolchains

The `full` image ships [mise](https://mise.jdx.dev/) and [bun](https://bun.sh/),
but **not** `uv`, a JDK, or a system Node/Python. Install what your project needs
in the install hook. Write each step to be idempotent (check first, install if
missing) so it is a no-op when a tool is already present and re-running the hook
is safe:

```bash
ensure_uv()  { command -v uv  >/dev/null 2>&1 || curl -LsSf https://astral.sh/uv/install.sh | sh; }
ensure_bun() { command -v bun >/dev/null 2>&1 || curl -fsSL https://bun.com/install | bash; }
```

| Language | Recommended tool | Notes |
|----------|------------------|-------|
| Python | [uv](https://docs.astral.sh/uv/) | `uv sync` auto-downloads the interpreter pinned in `.python-version`; no system Python needed. |
| TypeScript / JS | [bun](https://bun.sh/) | Built into the `full` image. `bun install`, `bun test`, `bun run build`. |
| Java / Kotlin | [SDKMAN](https://sdkman.io/) | `sdk env install` installs the JDK pinned in `.sdkmanrc`. Wrap `sdk` calls in `set +u` — SDKMAN's scripts reference unset variables. |

These tools install into the shed user's home (`~/.local/bin`, `~/.bun/bin`,
`~/.sdkman`), which survives stop/start on the rootfs upper layer.

!!! warning "Login PATH for `shed exec`"
    `shed exec` and `shed console` run a **non-interactive** login shell. Tools
    that only add themselves to `~/.bashrc` (e.g. SDKMAN's snippet) won't be on
    `PATH` there. To expose a tool to every session, write an
    `/etc/profile.d/*.sh` snippet from your install hook — login shells source
    it regardless of interactivity:

    ```bash
    sudo tee /etc/profile.d/zz-java.sh >/dev/null <<'EOF'
    if [ -d "$HOME/.sdkman/candidates/java/current/bin" ]; then
      export JAVA_HOME="$HOME/.sdkman/candidates/java/current"
      export PATH="$JAVA_HOME/bin:$PATH"
    fi
    EOF
    ```

## Running Services with Docker Compose

The `full` image ships the Docker daemon (started automatically) and `docker
compose`. Provisioning a service stack is now as simple as bringing up your
project's own `compose.yaml` — no need to apt-install and hand-manage databases.

**`.shed/provision.yaml`:**

```yaml
hooks:
  install: .shed/scripts/install.sh
  startup: .shed/scripts/startup.sh
  shutdown: .shed/scripts/shutdown.sh
```

**`startup.sh`** brings the stack up every boot and waits for health:

```bash
#!/bin/bash
set -euo pipefail
cd "${SHED_WORKSPACE:-$PWD}"

# Daemon auto-starts, but may not be ready the instant the hook fires.
for i in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 1; done

docker compose up -d
# Wait until every service with a healthcheck reports healthy.
for i in $(seq 1 30); do
  pending="$(docker compose ps --format '{{.Name}} {{.Health}}' \
    | awk '$2 != "" && $2 != "healthy" {print $1}')"
  [ -z "$pending" ] && break
  sleep 2
done
```

**`shutdown.sh`** stops it gracefully (named volumes persist):

```bash
#!/bin/bash
set -euo pipefail
cd "${SHED_WORKSPACE:-$PWD}"
docker compose down || true
```

!!! warning "Docker requires the `full` image"
    Only the `full` variant ships the Docker daemon. With `base`/`extensions`,
    `docker` is absent and these hooks will warn and continue.

!!! note "Docker networking"
    The `full` image enables Docker's default `docker0` bridge on **both**
    backends — VZ via the vfkit guest kernel, Firecracker via a custom kernel
    built with the netfilter/NAT options docker needs. So `docker run`,
    published ports, outbound NAT, and [Testcontainers](https://testcontainers.com/)
    work the same on VZ and Firecracker.

## Example: PostgreSQL via Docker Compose

For most projects, run Postgres from your `compose.yaml` (above) rather than
installing it into the image. The example below is the **no-Docker alternative**
— installing the service directly — kept for `base`/`extensions` images.

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

The `full` image uses this mechanism to configure the credential extensions:

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
| `SHED_WORKSPACE` | The shed's landing directory — `/home/shed/<reponame>` for `--repo`, `/home/shed/<basename>` for `--local-dir`, otherwise `/home/shed` |

Add custom variables in `provision.yaml`:

```yaml
env:
  DATABASE_URL: "postgresql://localhost/myapp"
  NODE_ENV: "development"
```

!!! warning "`provision.yaml` `env:` reaches hooks only"
    Variables in the `env:` map are injected into the **provisioning hooks**,
    not into later `shed exec` / `shed console` sessions or the app. To make a
    variable available everywhere, write it to `/etc/environment.d/*.conf` from
    your install hook (it is read per-exec):

    ```bash
    # In the install hook — available to every session afterward.
    sudo tee /etc/environment.d/90-myapp.conf >/dev/null <<'EOF'
    DATABASE_URL=postgresql://localhost/myapp
    TESTCONTAINERS_RYUK_DISABLED=true
    EOF
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
