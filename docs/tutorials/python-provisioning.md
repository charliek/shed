# Provisioning a Python (uv) Project

This tutorial sets up `.shed/` provisioning for a Python project managed with
[uv](https://docs.astral.sh/uv/). `uv` provisions the interpreter itself from
`.python-version`, so no system Python is required. Postgres and Redis come from
the project's `compose.yaml`, and integration tests use
[Testcontainers](https://testcontainers.com/).

It assumes the [`full` image](../reference/images.md) (the default), which ships
the Docker daemon Testcontainers needs.

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

# First uv sync (downloads the interpreter + deps) can be slow under vfs.
timeout: 30m
```

## `scripts/lib.sh`

```bash
#!/bin/bash
log() { echo "[provision $(date +%H:%M:%S)] $*"; }

# uv is not in any image; it installs to ~/.local/bin (already on the login PATH).
ensure_uv() { command -v uv >/dev/null 2>&1 || curl -LsSf https://astral.sh/uv/install.sh | sh; }

wait_for_docker() {
  for i in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 1; done
  docker info >/dev/null 2>&1 || log "WARN: docker not ready (needs the 'full' image)"
}

wait_for_compose_healthy() {
  for i in $(seq 1 30); do
    pending="$(docker compose ps --format '{{.Name}} {{.Health}}' \
      | awk '$2 != "" && $2 != "healthy" {print $1}')"
    [ -z "$pending" ] && return 0
    sleep 2
  done
}

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

ensure_uv
wait_for_docker

# UV_LINK_MODE=copy avoids a hardlink warning on the VirtioFS-mounted project.
# RYUK is reaped by the test process; the reaper is flaky under nested docker.
persist_session_env myapp \
  UV_LINK_MODE=copy \
  DATABASE_URL=postgresql://dev:dev@localhost:5432/myapp \
  TESTCONTAINERS_RYUK_DISABLED=true

# uv reads .python-version, downloads CPython if absent, builds .venv, installs
# the locked deps + the dev group.
uv sync

docker pull postgres:16-alpine || true   # warm the Testcontainers image
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
uv run alembic upgrade head || log "WARN: migrations failed"
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
shed exec myproj -- bash -lc 'cd ~/myproj && uv run pytest -m unit'
shed exec myproj -- bash -lc 'cd ~/myproj && uv run pytest -m integration'   # Testcontainers
```

!!! tip "uv provisions Python for you"
    There is no system Python in the `full` image. `uv sync` (or
    `uv python install`) downloads the interpreter pinned in `.python-version`
    into uv's own toolchain dir — nothing to apt-install.
