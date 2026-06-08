# Provisioning a TypeScript (bun) Project

This tutorial sets up `.shed/` provisioning for a TypeScript project that uses
[bun](https://bun.sh/) as its runtime and package manager, with Postgres and
Redis supplied by the project's `compose.yaml`.

It assumes the [`full` image](../reference/images.md) (the default), which ships
bun and the Docker daemon.

## Layout

```
.shed/
├── provision.yaml
└── scripts/
    ├── lib.sh
    ├── install.sh
    ├── startup.sh
    └── shutdown.sh
```

## `provision.yaml`

```yaml
hooks:
  install: .shed/scripts/install.sh
  startup: .shed/scripts/startup.sh
  shutdown: .shed/scripts/shutdown.sh

# First bun install + image pulls can be slow under the vfs storage driver.
timeout: 30m
```

## `scripts/lib.sh`

```bash
#!/bin/bash
log() { echo "[provision $(date +%H:%M:%S)] $*"; }

# bun ships in the `full` image; the guard installs it on leaner images.
ensure_bun() { command -v bun >/dev/null 2>&1 || curl -fsSL https://bun.com/install | bash; }

wait_for_docker() {
  for i in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 1; done
  docker info >/dev/null 2>&1 || log "WARN: docker not ready (needs the 'full' image)"
}

# Wait until every compose service with a healthcheck reports healthy.
wait_for_compose_healthy() {
  for i in $(seq 1 30); do
    pending="$(docker compose ps --format '{{.Name}} {{.Health}}' \
      | awk '$2 != "" && $2 != "healthy" {print $1}')"
    [ -z "$pending" ] && return 0
    sleep 2
  done
}

# Persist KEY=VALUE env to every exec/console session (read per-exec).
persist_session_env() {
  local name="$1"; shift
  sudo mkdir -p /etc/environment.d
  printf '%s\n' "$@" | sudo tee "/etc/environment.d/90-${name}.conf" >/dev/null
}
```

## `scripts/install.sh`

```bash
#!/bin/bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
cd "${SHED_WORKSPACE:-$(cd "$(dirname "$0")/../.." && pwd)}"

ensure_bun
wait_for_docker

# App/session env (used by the dev server, migrations, and `shed exec`). Tests
# usually load their own .env.test, so this just mirrors compose.yaml.
persist_session_env myapp \
  DATABASE_URL=postgres://dev:dev@localhost:5432/myapp \
  REDIS_URL=redis://localhost:6379

bun install                                  # workspace deps
docker pull postgres:16-alpine redis:7-alpine || true   # warm the compose images
```

## `scripts/startup.sh`

```bash
#!/bin/bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
cd "${SHED_WORKSPACE:-$(cd "$(dirname "$0")/../.." && pwd)}"

wait_for_docker
docker compose up -d
wait_for_compose_healthy

# Apply schema/migrations the tests rely on (adjust to your tool).
bun run db:push || log "WARN: db:push failed"
```

## `scripts/shutdown.sh`

```bash
#!/bin/bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
cd "${SHED_WORKSPACE:-$(cd "$(dirname "$0")/../.." && pwd)}"
docker compose down || true
```

## Build and test

```bash
shed create myproj --local-dir .
shed exec myproj -- bash -lc 'cd ~/myproj && bun run test'
shed exec myproj -- bash -lc 'cd ~/myproj && bun run build'
shed exec myproj -- bash -lc 'cd ~/myproj && bun run lint'
```

!!! note "Compose and Testcontainers networking"
    `docker compose` creates its own user-defined network, and the `full` image
    enables Docker's default `docker0` bridge — both work on the VZ and
    Firecracker backends, so published ports and Testcontainers behave the same
    on each.
