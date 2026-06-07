# Provisioning a Gradle (JVM) Project

This tutorial sets up `.shed/` provisioning for a Gradle project — Java or
Kotlin — that builds with the Gradle wrapper and runs
[Testcontainers](https://testcontainers.com/) integration tests. Java is
installed with [SDKMAN](https://sdkman.io/), pinned to the project's
`.sdkmanrc`.

It assumes the [`full` image](../reference/images.md) (the default), which ships
the Docker daemon that Testcontainers needs.

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

# JDK download + first build can be slow under the vfs storage driver.
timeout: 30m
```

## `scripts/lib.sh`

Shared helpers, sourced by each hook. Every step is idempotent so re-running a
hook is safe.

```bash
#!/bin/bash
log() { echo "[provision $(date +%H:%M:%S)] $*"; }

# Source SDKMAN (installing it first if missing). Disable nounset around it —
# SDKMAN's scripts reference unset variables.
ensure_sdkman() {
  [ -s "$HOME/.sdkman/bin/sdkman-init.sh" ] || curl -fsSL "https://get.sdkman.io" | bash
  export sdkman_auto_answer=true sdkman_selfupdate_feature=false
  set +u; source "$HOME/.sdkman/bin/sdkman-init.sh"; set -u
}

wait_for_docker() {
  for i in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 1; done
  docker info >/dev/null 2>&1 || log "WARN: docker not ready (needs the 'full' image)"
}

# Public Docker Hub pulls fail under the image's credsStore=shed; reset it.
enable_public_image_pulls() {
  local cfg="$HOME/.docker/config.json"
  grep -q '"credsStore"' "$cfg" 2>/dev/null && { cp "$cfg" "$cfg.bak"; echo '{}' >"$cfg"; }
}

# Testcontainers launches containers on docker's DEFAULT bridge, which the image
# disables (bridge: none). Re-enable it (compose is unaffected; it uses its own).
enable_docker_default_bridge() {
  docker network inspect bridge >/dev/null 2>&1 && return
  local cfg=/etc/docker/daemon.json tmp; tmp="$(mktemp)"
  jq 'del(.bridge) | del(.iptables)' "$cfg" >"$tmp" && sudo install -m 0644 "$tmp" "$cfg"; rm -f "$tmp"
  sudo systemctl restart docker
  for i in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 1; done
}
```

## `scripts/install.sh`

```bash
#!/bin/bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
cd "${SHED_WORKSPACE:-$(cd "$(dirname "$0")/../.." && pwd)}"

wait_for_docker
enable_docker_default_bridge
enable_public_image_pulls
ensure_sdkman

# Install the JDK pinned in .sdkmanrc (e.g. java=21.0.5-tem). Disable nounset
# around every sdk call.
set +u
sdk env install || sdk install java "$(awk -F= '/^java=/{print $2}' .sdkmanrc)"
sdk env use
set -u
java -version

# Expose the JDK to every login shell (shed exec/console), not just this hook.
sudo tee /etc/profile.d/zz-java.sh >/dev/null <<'EOF'
if [ -d "$HOME/.sdkman/candidates/java/current/bin" ]; then
  export JAVA_HOME="$HOME/.sdkman/candidates/java/current"
  export PATH="$JAVA_HOME/bin:$PATH"
fi
EOF

# Ryuk (Testcontainers' reaper) is flaky under the nested docker config; the
# test JVM reaps its own containers. Persist for all test sessions.
sudo mkdir -p /etc/environment.d
echo 'TESTCONTAINERS_RYUK_DISABLED=true' | sudo tee /etc/environment.d/90-gradle.conf >/dev/null

./gradlew --no-daemon help >/dev/null   # warm the wrapper + dependency cache
```

## `scripts/startup.sh` and `scripts/shutdown.sh`

Testcontainers manages its own ephemeral containers, so there are no long-running
services to start or stop:

```bash
#!/bin/bash
# startup.sh
set -euo pipefail
source "$(dirname "$0")/lib.sh"
wait_for_docker
enable_docker_default_bridge   # no-op after first create (config persists)
```

```bash
#!/bin/bash
# shutdown.sh — nothing to stop; Testcontainers are torn down by the test JVM.
echo "no managed services"
```

## Build and test

```bash
shed create myproj --local-dir .
shed exec myproj -- bash -lc 'cd ~/myproj && ./gradlew build'
```

`./gradlew build` compiles and runs the unit + Testcontainers integration suite
against the shed's Docker daemon.

!!! tip "Iterating on the install hook"
    The install hook runs once. While developing it, re-run the script directly
    — it's idempotent: `shed exec myproj -- bash '$SHED_WORKSPACE/.shed/scripts/install.sh'`.
    Then do a clean `shed delete` + `shed create` to validate the real hook path.
