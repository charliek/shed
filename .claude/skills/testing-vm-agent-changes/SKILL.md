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

> **Gotcha (shed#317): the erofs mint uses `$TMPDIR`, not `OUTPUT_DIR`.** If your
> image store is on an external volume but `$TMPDIR` is on the internal disk, the
> build dies AFTER the whole Docker build with `no space left on device` writing
> `.../shed-mint-erofs-*/rootfs.tar`. The flattened rootfs is multi-GB. Set it
> explicitly:
>
> ```bash
> TMPDIR=/Volumes/<vol>/tmp SHED_SOURCE_REF=... OUTPUT_DIR=... \
>   ./scripts/build-vz-rootfs.sh --variant full --build-tools-version <BT_TAG>
> ```

### 3. Resolution is automatic (since #227)

The rebuild "just works" now — **no `docker buildx prune`, no hand-edited
`refs/<hash>.json`.** Two mechanisms make it reliable (both fixed the gremlins
that used to live here):

- **The install layer busts on content change.** `build-vz-rootfs.sh` computes a
  content hash of the **whole build context** and passes
  `--build-arg SHED_INSTALL_SHA=…`, which the Dockerfile's bind-mount install RUN
  references. A changed agent re-runs that layer — the build log prints
  `SHED_INSTALL_SHA=<hash>` and the step is **not** `CACHED` — while the expensive
  apt layer stays cached. An unchanged agent leaves the install layer `CACHED`.
  Docker `ARG`s are **stage-scoped**, so the **extensions stage redeclares
  `ARG SHED_INSTALL_SHA` and echoes it** in its install RUN too — that guards the
  in-tree guest-binary install the same way (a changed `shed-ext-*` binary re-runs
  the extensions install layer instead of reusing a stale BuildKit bind-mount
  cache).
- **The ref-index is written by the build.** `shed image build` records
  `refs/<sha256(source-ref)>.json` → the new manifest digest, so
  `shed create --image base` resolves your build immediately. `SHED_SOURCE_REF`
  (step 2) **must** equal the dev config's `image_aliases.base` so the build
  writes the ref create reads. The dev server reads the ref-index per-create, so
  no restart is needed.

Still **verify** (step 4) before trusting a run — it's cheap insurance.

### 4. VERIFY the VM is running YOUR agent (do not skip)

Create a fresh shed and grep the baked binary for a symbol/string only your
change adds (function names survive in the Go binary):

```bash
shed -s my-server-dev create dbg --image base
shed -s my-server-dev exec dbg -- bash -c \
  "strings /usr/local/bin/shed-agent | grep -c '<your-new-symbol-or-log-string>'"
# >0 means your agent is baked in; 0 means a stale layer/manifest — check the
# build log shows the install RUN ran (`SHED_INSTALL_SHA=…`, not `CACHED`) and
# that SHED_SOURCE_REF (step 2) matched the dev config's image_aliases.base.
```

**The same verify applies to the guest extension binaries.** Since the monorepo
import, the `extensions` / `full` variants bake the three guest binaries
(`shed-ext-ssh-agent`, `shed-ext-aws-credentials`, `docker-credential-shed`)
**in-tree** — cross-compiled from `cmd/shed-ext-*` and staged into
the build context by `scripts/stage-guest-binaries.sh` (called by the rootfs
scripts). There is no `ghcr.io/charliek/shed-extensions` image to `COPY --from`.
(A fourth binary, `shed-ext-rc`, was baked the same way through v0.8.x; it was
dropped from the image in plan 022's S6 retirement — see
[`shed-ext-rc` (retired)](../../../docs/extensions/rc-helper.md) — and `strix` +
`prox`, installed from the stridelabs apt repo rather than built in-tree, took
its place.) If you changed a guest binary, build the `extensions` (or `full`)
variant and confirm the VM runs **your** build — the dev-build convention is no
ldflags, so the version is a dev string, not the last extensions release:

```bash
shed -s my-server-dev create dbg --image extensions
shed -s my-server-dev exec dbg -- docker-credential-shed version   # must report a dev string
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

Edit agent → unit tests (Docker) → rebuild rootfs (step 2) → verify (step 4) →
integration (step 5). Comment-only edits don't change the binary, so they don't
need a rebuild.

## Guest extension binaries (`shed-ext-ssh-agent` and friends)

The surviving `cmd/shed-ext-*` binaries — `shed-ext-ssh-agent`, `shed-ext-aws-credentials`,
`docker-credential-shed` — are baked into the `extensions`/`full` variants the same way
`shed-agent` is baked into every variant, so the **full rebuild loop above applies
unchanged**: edit `cmd/shed-ext-*` (or the `internal/ext/{sshagent,awsproxy,dockercred}`
package behind it) → rebuild the `extensions` variant (step 2, `--variant extensions`) →
verify (a `version` subcommand reports a dev string, step 4) → create a fresh shed.
`internal/ext/*.go` is **not** `//go:build linux`, so its unit tests run under plain
`make test` on macOS (no Docker needed, unlike the agent tests).

> **A fourth guest binary, `shed-ext-rc`, and the RC activity hub it hosted (`serve`,
> `internal/ext/rc`) were retired in plan 022's S6
> ([`charliek/shed#328`](https://github.com/charliek/shed/issues/328)) — deleted from the
> tree, dropped from the `extensions`/`full` images, and replaced there by `strix` and
> `prox` (installed from the stridelabs apt repo, not built in-tree, so they need no
> rebuild loop at all). Everything this section used to say about rc sessions, the guest
> hub, `tests/integration/test_rc_kickoff.py`/`test_rc_enrichment.py`/
> `test_rc_hub_activity.py`, and the opencode-verb/cursor-hook smoke tests is gone with
> it — none of those tests or binaries exist any more. Agent sessions are roost tabs now;
> see [`shed-ext-rc` (retired)](../../../docs/extensions/rc-helper.md) if you land here
> looking for where that went.

### Fast loop: copy the binary into a running shed

A full rootfs rebuild is minutes; for a tight edit→test loop on a guest binary you can
**cross-compile and drop it into a running shed**, skipping the image rebuild entirely:

```bash
# VZ is arm64; FC is amd64. Match the shed's arch.
GOOS=linux GOARCH=arm64 go build -o /tmp/docker-credential-shed ./cmd/docker-credential-shed
shed -s my-server-dev cp /tmp/docker-credential-shed dbg:/tmp/docker-credential-shed   # or: pipe over `exec … tee`
shed -s my-server-dev exec dbg -- sudo install -m0755 /tmp/docker-credential-shed /usr/local/bin/docker-credential-shed
shed -s my-server-dev exec dbg -- docker-credential-shed version   # confirm the dev build
```

If `shed cp` is unavailable, stream it: `go build -o /dev/stdout … | shed -s … exec dbg
-- sudo tee /usr/local/bin/docker-credential-shed >/dev/null` then `chmod +x`.

**One caveat:** a recreated shed reverts to the image binary. The copy lives only in that
shed's writable upper — `shed create`/recreate (or a snapshot restore) boots the baked
image's binary again. Use the copy shortcut for iteration; use the full rootfs rebuild
(step 2) for anything you'll assert on across a recreate, and for the final pre-PR
verification. `shed-ext-ssh-agent` and `shed-ext-aws-credentials` run as systemd services
rather than one-shot CLIs, so after installing a copy of either, restart its unit
(`shed exec dbg -- sudo systemctl restart shed-ext-ssh-agent`, respectively
`shed-ext-aws-credentials`) so the new binary actually takes over the running process.

## Gremlin: FC remote rootfs build + mise + sudo

The FC dev image is built on the remote (`mini3`), and the dev image store
(`/var/lib/shed-dev/firecracker/images`) is root-owned, so the instinct is to
run `build-firecracker-rootfs.sh` under `sudo`. That fails at the first
`go build`: the script resolves `go` through the mise shim, and mise refuses an
**untrusted** `.mise.toml` — worse under `sudo`, where mise trust lives in
root's state, `secure_path` overrides your `PATH`, and the shim wins anyway.
`sudo mise trust` doesn't stick (HOME mismatch). Remedy that sidesteps it
entirely: make the store user-writable and build as the normal user (no sudo,
mise works):

```sh
ssh mini3 'sudo chown -R $USER:$USER /var/lib/shed-dev/firecracker'
ssh mini3 'cd ~/projects/shed && export PATH="$HOME/.local/share/mise/shims:$PATH" && \
  OUTPUT_DIR=/var/lib/shed-dev/firecracker/images \
  ./scripts/build-firecracker-rootfs.sh --variant extensions --build-tools-version <ref>'
```

The root-run FC dev server (sudo nohup) still reads the user-owned blobs fine.

Three more edges from plan 023 (2026-09-22), all setup, none in the Dockerfile:

- **Under `sudo` the build also dies later, at the OCI export** — `OCI exporter is not
  supported for the docker driver` — because root's Docker has only the plain `docker`
  builder; the user's `shedoci` (docker-container) builder is what the script needs. A
  past `sudo` run can also leave root-owned files under `~/.docker/buildx/`, which then
  breaks `docker buildx ls` for the user (`permission denied`): `sudo chown -R $USER
  ~/.docker` fixes it. Same remedy: build as the user.
- **A fresh scratch checkout / worktree of the repo (on the Mac or on mini3) has an
  untrusted `.mise.toml`**, and the failure is instant ("Config files … are not
  trusted"). `mise trust <checkout>/.mise.toml` first — with the REAL mise binary
  (`~/.local/bin/mise`), which is NOT on a non-interactive ssh PATH even though its
  shims dir is; `mise trust -q` is not a thing and fails silently.
- **`--build-tools-version <released tag>` is mandatory when the host has no local
  `shed-build-tools:dev`** (the Mac mini does not): the default mints the erofs through
  `shed-build-tools:dev` and the whole Docker build succeeds before it fails at
  `Unable to find image 'shed-build-tools:dev'`. Pass the latest `v*` tag
  (`git tag --list 'v*' | sort -V | tail -1`) and it pulls `ghcr.io/charliek/
  shed-build-tools:<tag>` instead.

(Guest **extension** binaries — `extensions`/`full` variants — are now built
in-tree by `scripts/stage-guest-binaries.sh`, staged into the context like
shed-agent; verify `docker-credential-shed version` reports a non-release version
in the booted shed, same as the shed-agent check.)

## Gremlin: `kill 0` (or any empty-pid kill) over tailscale ssh reaches OTHER sessions

Every non-pty tailscale ssh session on mini3 runs in tailscaled's process group, and so
does anything you `nohup … &` from one — the dev shed-server and every firecracker VMM it
spawns. A script that does `kill -KILL $PID` with `$PID` empty or `0` (e.g. read from a
`systemctl show -p MainPID` of a unit that failed to start) signals that whole group: it
killed an orphaned VMM that had survived a real SIGKILL of its server, and restarted
tailscaled (plan 023 live-05, 2026-09-22). Guard every signal: refuse an empty/0/1 pid.
Related, for KillMode legs: a transient unit that merely ADOPTS a VMM spawned elsewhere
cannot reap it on stop under any KillMode (wrong cgroup) — spawn the shed UNDER the unit
you are testing, and `cat /proc/<vmm>/cgroup` to prove it.

## When you hit a NEW rough edge

Add it here. This file exists because the agent-in-image split has non-obvious
traps; capturing each one saves the next session an hour. (The stale-ref-index
and BuildKit-cache traps that used to live in step 3 were fixed in **#227** —
the rebuild now busts the install layer and writes the ref-index on its own.)
