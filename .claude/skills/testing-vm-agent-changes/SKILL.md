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
import, the `extensions` / `full` variants bake the four guest binaries
(`shed-ext-ssh-agent`, `shed-ext-aws-credentials`, `docker-credential-shed`,
`shed-ext-rc`) **in-tree** — cross-compiled from `cmd/shed-ext-*` and staged into
the build context by `scripts/stage-guest-binaries.sh` (called by the rootfs
scripts). There is no `ghcr.io/charliek/shed-extensions` image to `COPY --from`.
If you changed a guest binary, build the `extensions` (or `full`) variant and
confirm the VM runs **your** build — the dev-build convention is no ldflags, so
the version is a dev string, not the last extensions release (`v0.4.9`):

```bash
shed -s my-server-dev create dbg --image extensions
shed -s my-server-dev exec dbg -- shed-ext-rc version   # must NOT report v0.4.9
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

## Guest extension binaries (`shed-ext-rc` and friends)

The `cmd/shed-ext-*` binaries (notably **`shed-ext-rc`**, which now includes the resident
`serve` **rc activity hub**) are baked into the `extensions`/`full` variants the same way
`shed-agent` is baked into every variant — so the **full rebuild loop above applies
unchanged**: edit `cmd/shed-ext-rc/` (or `internal/ext/rc/`) → rebuild the `extensions`
variant (step 2, `--variant extensions`) → verify (`shed-ext-rc version` reports a dev
string, step 4) → create a fresh shed. `internal/ext/rc/*.go` is **not** `//go:build
linux`, so its unit tests run under plain `make test` on macOS (no Docker needed, unlike
the agent tests).

> **Rebuild BOTH `extensions` AND `full` for the rc-hub integration tests.** The rc-hub
> integration tests (`tests/integration/test_rc_enrichment.py`,
> `tests/integration/test_rc_hub_activity.py`) provision their sheds from the
> **`extensions`** alias (`server.create(shed, image="extensions")`), *not* the dev
> server's usual `full` `default_image`. So a dev-image rebuild done only as
> `--variant full` leaves the `extensions` alias pointing at a **stale** image — the rc
> tests then run old guest code (or skip on an image that predates `shed-ext-rc serve`)
> while looking green. When validating an rc change, rebuild **both**
> `./scripts/build-vz-rootfs.sh --variant full …` **and** `--variant extensions …` (FC:
> the matching `build-firecracker-rootfs.sh` invocations) so both aliases carry your build.
> This two-variant rebuild requirement was correct before plan 008 and stays correct — no
> change needed there.

**New guest surfaces since plan 008 (opencode dual-control + cursor hooks) — smoke
these explicitly when touching the rc hub, since the existing integration suite has no
coverage for them yet (a `test_rc_kickoff.py`-style CLI-driven suite is future work):**

- **opencode verbs** (`turn`/`interrupt`/`approvals/{id}`, live only for opencode):
  create an opencode rc session on the rebuilt image, drive a turn/interrupt/approval
  through `curl` against the server's `/api/sheds/{name}/rc/v1/sessions/{slug}/{verb}`
  proxy route (or the guest-local hub port directly, `shed exec <shed> curl
  127.0.0.1:1029/v1/sessions`), and confirm the steer renders in the attached TUI at
  the same time — that's the dual-control property the whole design bets on. Two
  sessions in one opencode store is the WS-B regression to re-check by hand
  occasionally: steering session A must never touch session B.
- **cursor hook ingestion**: create a cursor rc session on a host with cursor auth
  mounted (`~/.config/cursor`, **not** `~/.cursor` — see
  `docs/reference/configuration.md`), run a turn, and confirm the feed
  (`GET .../messages`) picks up hook-derived rows (`beforeSubmitPrompt`, tool
  use/result, `afterAgentResponse`) and that `~/.shed-rc-hub/hub.log` shows no
  `hooks.json` write-skip warning (the foreign-device guard). If cursor auth mounts
  aren't set up on the dev host, this leg is Mac-local-hub-only — see the plan's
  §Verified conditionality note for AC-3.

### Fast loop: copy the binary into a running shed

A full rootfs rebuild is minutes; for a tight edit→test loop on a guest binary you can
**cross-compile and drop it into a running shed**, skipping the image rebuild entirely:

```bash
# VZ is arm64; FC is amd64. Match the shed's arch.
GOOS=linux GOARCH=arm64 go build -o /tmp/shed-ext-rc ./cmd/shed-ext-rc
shed -s my-server-dev cp /tmp/shed-ext-rc dbg:/tmp/shed-ext-rc   # or: pipe over `exec … tee`
shed -s my-server-dev exec dbg -- sudo install -m0755 /tmp/shed-ext-rc /usr/local/bin/shed-ext-rc
shed -s my-server-dev exec dbg -- shed-ext-rc version            # confirm the dev build
```

If `shed cp` is unavailable, stream it: `go build -o /dev/stdout … | shed -s … exec dbg
-- sudo tee /usr/local/bin/shed-ext-rc >/dev/null` then `chmod +x`.

**Two caveats specific to the rc hub:**

1. **A recreated shed reverts to the image binary.** The copy lives only in that shed's
   writable upper — `shed create`/recreate (or a snapshot restore) boots the baked
   image's `shed-ext-rc` again. Use the copy shortcut for iteration; use the full rootfs
   rebuild (step 2) for anything you'll assert on across a recreate, and for the final
   pre-PR verification.
2. **Kill the running hub so your new binary takes over.** The old `serve` daemon keeps
   running the *previous* binary (the port bind is the lock, so a fresh `serve` just
   exits 0 against it). After installing, stop it — kill the hub process (or every
   `rc-*` session, which lets it idle-exit) — so the next ensure-start spawns **your**
   build. Confirm with `shed -s … exec dbg -- pgrep -af 'shed-ext-rc serve'` and check
   `~/.shed-rc-hub/hub.log`.

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
(Guest **extension** binaries — `extensions`/`full` variants — are now built
in-tree by `scripts/stage-guest-binaries.sh`, staged into the context like
shed-agent; verify `shed-ext-rc version` reports a non-release version in the
booted shed, same as the shed-agent check.)

## When you hit a NEW rough edge

Add it here. This file exists because the agent-in-image split has non-obvious
traps; capturing each one saves the next session an hour. (The stale-ref-index
and BuildKit-cache traps that used to live in step 3 were fixed in **#227** —
the rebuild now busts the install layer and writes the ref-index on its own.)
