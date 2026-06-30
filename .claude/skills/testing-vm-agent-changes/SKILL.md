---
name: testing-vm-agent-changes
description: >
  Validate changes to the in-VM agent (cmd/shed-agent) on a real VZ/FC VM. Use
  whenever you edit cmd/shed-agent/ and need to prove the change works on a live
  VM, or when an integration test exercises agent behavior. CRITICAL: the agent
  is baked into the rootfs IMAGE, not the host shed-server, so `make
  test-integration-dev` (which only restarts the dev server) does NOT pick up
  agent changes — you must rebuild the rootfs into the dev image store. Covers
  that rebuild loop plus the codesign / build-tools / BuildKit-cache / ref-index
  gremlins, and running the linux-only agent unit tests via Docker on macOS.
  Keep this file updated whenever you hit a new rough edge.
---

# Testing in-VM agent (`cmd/shed-agent`) changes

> Scope: the end-to-end loop below is the **macOS / VZ** parallel-dev setup. The
> Firecracker remote loop is analogous (`make dev-server-*-fc`, `OUTPUT_DIR`
> under `/var/lib/shed-dev/...`) — see CLAUDE.md. All commands use `$HOME`/`~`
> and read repo-relative paths, so nothing is tied to a specific machine; the
> image **tags** below (`vX.Y.Z`, `golang:1.NN`) are point-in-time examples —
> resolve the real values from your config / `go.mod` / local `docker images`
> as noted at each step.

The agent runs **inside** the VM and is shipped **inside the rootfs image**.
The host `shed-server` is a separate binary. This split is the #1 source of
"my change didn't take effect" confusion:

> `make test-integration-dev` rebuilds and restarts the dev **server**. It does
> **NOT** rebuild the rootfs, so it does **NOT** pick up `cmd/shed-agent`
> changes. You must rebuild the rootfs image and create a fresh shed from it.

## Fast unit tests (do this first, every time)

`cmd/shed-agent/*.go` is `//go:build linux`, so its tests don't run under
`make test` on macOS. Run them in Docker (native arch on Apple Silicon):

```bash
docker run --rm -v "$PWD":/src -w /src -e GOFLAGS=-buildvcs=false \
  golang:1.25 go test -count=1 -race ./cmd/shed-agent/   # match the `go` line in go.mod
```

Cross-compile + lint locally without running:

```bash
GOOS=linux GOARCH=arm64 go build -o /dev/null ./cmd/shed-agent   # VZ
GOOS=linux GOARCH=amd64 go build -o /dev/null ./cmd/shed-agent   # FC
GOOS=linux golangci-lint run ./cmd/shed-agent/...
```

## End-to-end on a real VZ VM (the parallel dev server)

Prereqs (see CLAUDE.md "Server-side changes — parallel dev server"): a
`my-server-dev` entry in `~/.shed/config.yaml` on ports 18080/12222, and the
dev config `configs/server.dev-parallel.mac.yaml`.

### 1. Build + codesign the host server

```bash
make build
codesign --entitlements internal/vz/entitlements.plist -s - ./bin/shed-server
```

**Gotcha:** `make dev-server-up` and `make test-integration-dev` both depend on
the `build` target, which rebuilds `bin/shed-server` **unsigned** — clobbering
your codesign and breaking VZ. So start the dev server **manually** with the
codesigned binary (don't use `make dev-server-up` after codesigning), and use a
**local** build-tools image (ghcr pulls are often denied):

```bash
# <BT_TAG>: any shed-build-tools tag you have locally (see step 2 — ghcr pulls
# are often denied). The server uses it to mint the per-shed ext4 upper template.
SHED_BUILD_TOOLS_REF="ghcr.io/charliek/shed-build-tools:<BT_TAG>" \
  nohup bin/shed-server serve --config configs/server.dev-parallel.mac.yaml \
  > "$HOME/.shed/dev/server.log" 2>&1 &
echo $! > "$HOME/.shed/dev/server.pid"
# readiness:
for i in $(seq 1 20); do shed -s my-server-dev list >/dev/null 2>&1 && break; sleep 1; done
```

### 2. Rebuild the rootfs with your agent INTO the dev image store

`OUTPUT_DIR` points the blobs at the dev server's store (the VZ dev `images_dir`
from your dev config — `~/Library/Application Support/shed-dev/vz` on macOS).
`SHED_SOURCE_REF` **must equal the dev config's `image_aliases.base` value** so
`--image base` resolves to *your* build — read the current value out of the
config rather than copying the tag below verbatim:

```bash
grep -A3 image_aliases configs/server.dev-parallel.mac.yaml   # → base: ghcr.io/charliek/shed-vz-base:<TAG>
```

`--build-tools-version` is any `shed-build-tools` tag you have **locally** —
ghcr pulls are frequently denied, and the exact version need not match your
source unless your change touches build-tools/erofs:

```bash
docker images | grep shed-build-tools     # pick a tag that's already pulled
```

Then build (substitute the `<TAG>`/`<BT_TAG>` you found above):

```bash
SHED_SOURCE_REF="ghcr.io/charliek/shed-vz-base:<TAG>" \
OUTPUT_DIR="$HOME/Library/Application Support/shed-dev/vz" \
  ./scripts/build-vz-rootfs.sh --variant base --build-tools-version <BT_TAG>
```

### 3. Make the dev server resolve to your new manifest

Two gremlins routinely make the freshly-built agent NOT reach the VM. Always
**verify** (step 4) before trusting a run.

- **BuildKit caches the agent install layer** (`RUN --mount=type=bind … install
  /ctx/shed-agent`). A changed binary can be silently reused from cache. If the
  baked agent is stale, bust it and rebuild:
  ```bash
  docker buildx prune -af && docker builder prune -af
  ```
- **The ref-index can point at a stale manifest** after an `OUTPUT_DIR` build
  (the running dev server + repeated builds leave `refs/<hash>.json` pointing at
  an older digest than the one you just built). Force it to your build, then
  restart the server:
  ```bash
  cd "$HOME/Library/Application Support/shed-dev/vz"
  REF="ghcr.io/charliek/shed-vz-base:<TAG>"   # the same image_aliases.base value as above
  REFHASH=$(printf '%s' "$REF" | shasum -a 256 | awk '{print $1}')
  cat "refs/$REFHASH.json"          # what it points at now
  # <DIGEST> = your build's manifest digest from its "Built image (sha256:...)" line:
  printf '{"ref":"%s","digest":"sha256:<DIGEST>"}\n' "$REF" > "refs/$REFHASH.json"
  # then restart the dev server (kill the PID in ~/.shed/dev/server.pid, relaunch as in step 1)
  ```

### 4. VERIFY the VM is running YOUR agent (do not skip)

Create a fresh shed and grep the baked binary for a symbol/string only your
change adds (function names survive in the Go binary):

```bash
shed -s my-server-dev create dbg --image base
shed -s my-server-dev exec dbg -- bash -c \
  "strings /usr/local/bin/shed-agent | grep -c '<your-new-symbol-or-log-string>'"
# >0 means your agent is baked in; 0 means a stale layer/manifest — go back to step 3.
```

To extract+inspect the agent from a manifest without booting a VM (useful to
tell "build baked it" from "resolution is stale"):

```bash
cd "$HOME/Library/Application Support/shed-dev/vz/blobs/sha256"
# for each layer of the manifest, find usr/local/bin/shed-agent and strings it
```

### 5. Run the integration suite against the dev server

```bash
cd tests/integration
SHED_VZ_SERVER=my-server-dev SHED_VZ_LOG_PATH="$HOME/.shed/dev/server.log" \
SHED_VZ_DEV_SERVER=my-server-dev SHED_VZ_DEV_LOG_PATH="$HOME/.shed/dev/server.log" \
  uv run pytest -v -k vz <test_files...>
```

`-k vz` skips the FC params (unreachable from a Mac). The in-VM agent log is at
`shed -s my-server-dev exec <shed> -- sudo journalctl -u shed-agent`.

## Per-iteration loop

Edit agent → unit tests (Docker) → rebuild rootfs (step 2) → if stale, bust
cache + force ref-index (step 3) → verify (step 4) → integration (step 5).
Comment-only edits don't change the binary, so they don't need a rebuild.

## When you hit a NEW rough edge

Add it here. This file exists because the agent-in-image split and the
build/cache/ref-index resolution have several non-obvious traps; capturing each
one saves the next session an hour.
