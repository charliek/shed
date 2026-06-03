# Changelog

All notable changes to this project will be documented in this file.

## v0.6.1 — 2026-06-03

### Images API: `alias` + `is_default` metadata (#171)

`GET /api/images` (and `shed --json image ls`) now label each config-sourced
image with its friendly `image_aliases` key (`alias`) and flag the
`default_image` entry (`is_default`). Both fields are additive and
`omitempty`, so existing clients and the `shed image ls` table view are
unchanged. This lets the shed-desktop New-Shed picker list aliases by name
with the default preselected, instead of requiring a raw ref. Also corrects
the documented `source` values to `config` / `user` / `dangling`.

## v0.6.0 — 2026-06-03

Image-system milestone: VM images move to a **Docker-style, ref-keyed
identity model**, replacing the old `_base`-tag / `base_rootfs` scheme.
This is a **breaking config change** — read
`docs/upgrades/v0.5.9-to-v0.6.0.md` before upgrading. The work lands in
three parts (the breaking core, an additive UX layer, a docs pass) plus
a CI-auth follow-up.

### Breaking: ref-keyed image identity + `pull_policy` (#168)

**Why**: a real upgrade-day failure — after a brew bump + config edit,
`shed image ls` still showed an internal `_base` tag pinned to the old
version, the old blob was un-addressable, and `shed create` silently
reused it.

Config keys change (the loader now **rejects** the removed keys rather
than silently ignoring them, which would recreate the original bug):

| Old (≤0.5.9) | New (0.6.0) |
|---|---|
| `base_rootfs: <ref>` | `default_image: <ref>` |
| `images: {…}` | `image_aliases: {…}` (optional) |
| _(none)_ | `pull_policy: missing` *(missing\|always\|never)* |

- **Identity is the Docker ref**, resolved O(1) via a
  `refs/<sha256(ref)>.json` sidecar index; `_base` is gone everywhere
  user-visible.
- **`pull_policy`** enforced in `EnsureImage`: `missing` (cache, pull if
  absent), `always`, `never`. A configured version bump is now a cache
  miss → auto-pull on next create.
- **`ls`/`rm`/`prune` are ref-keyed**: `rm` takes a ref/digest/label,
  blocks only on live shed/snapshot pins, and leaves the blob for prune
  (Docker model); prune protects the configured `default_image`/alias
  digests.
- **Packaging**: template, brew formula, example/dev configs migrated.
  New `shed-server config-validate`; the deb postinstall preflights the
  config and **skips the restart** on an un-migrated config (no
  crash-loop).

### Image UX layer (#169)

Additive — no `shed create` / boot-path change.

- **`shed image pull` streams progress over SSE** (like `shed create`),
  with a plain-JSON fallback so a new CLI still works against a pre-SSE
  server.
- **`shed image ls`/`inspect`** display the configured/pulled ref (then
  the manifest source-ref only as a cold fallback), so divergent tags
  (`:latest`, digest pin, mirror) stay in the `config` bucket.
- **`shed image rm`** warns + confirms when the target is referenced by
  server config (`default_image` / `image_aliases`).
- **`shed image prune`** groups output by image (ref + reclaimed size +
  total); `--verbose` lists the constituent blob digests.
- **`shed list -vv`** shows each shed's image as `<label> (sha256:short)`.

### Docs + cleanup (#170)

- Rewrote the residual prose still describing the old `base_rootfs` /
  `images:` / `_base` / tag-auto-discovery model across
  `reference/{images,cli,api}.md`, the VZ/FC setup guides, the
  build-your-own-image tutorial, and `development/testing.md`.
- Bumped stale `v0.5.0-dev` example configs to `v0.5.9`.
- Removed the now-dead `ImageDiskEntry.IsBase` API field.

### Release process

- CI: switched the release-bot token from the deprecated `app-id` to
  `client-id` for `create-github-app-token` v3 (#167).

**Upgrade**: this release changes the config format. Follow
`docs/upgrades/v0.5.9-to-v0.6.0.md` — rename `base_rootfs` →
`default_image`, `images:` → `image_aliases:`, and (optionally) set
`pull_policy`. The deb postinstall skips the service restart until the
config is migrated.

## v0.5.9 — 2026-05-31

Substantial maintenance release covering three areas: a complete
rebuild of the developer's local-shed-server validation workflow
(parallel dev server replacing the old swap-the-binary dance),
a hardening pass on the VM lifecycle and packaging (mount retries,
VMM exit verification, .deb postinst restart, `StartShed`
late-failure metadata persistence), and an internal refactor pulling
both backends onto a shared orchestrator `BackendStarter`. Also
adopts the `cc-plugins:release-workflows` convention for the release
pipeline.

No manifest format changes, no image cache wipe required — same
`shed image rm <tag> && shed image prune` upgrade workflow as v0.5.8.

### Parallel dev shed-server workflow (#157, #158, #159, #161, #162, #163)

**Why**: every server-side PR — VZ or FC — now has a one-command
path to validate against the developer's source tree, **without**
the pre-v0.5.9 swap-the-binary workflows that were stateful, hard
to back out of, and easy to leave dirty.

Replaces:

- The Mac "swap brew shed-server binary, run the suite, restore"
  workflow (deleted in #162).
- The Linux/FC "swap the .deb-installed binary on a remote box"
  workflow (deleted in #163).

…with a parallel dev shed-server running **alongside** the brew or
.deb production server on different ports + sockets, driven by new
Makefile targets:

| Target | Platform | What it does |
|---|---|---|
| `make dev-server-up` | Mac | Builds + launches `bin/shed-server` via `nohup` with `SHED_BUILD_TOOLS_REF` inline. Polls `shed -s my-server-dev list` for readiness. |
| `make dev-server-down` | Mac | Graceful TERM (5s budget) then KILL. Idempotent. |
| `make dev-server-up-fc` | Linux remote (`$SHED_FC_HOST=mini3` default) | Cross-compiles for remote GOARCH, scps binary + config, launches via `sudo nohup`. |
| `make dev-server-down-fc` | Linux remote | Same shape as the Mac target. |
| `make install-local-server` | Mac | Earlier-cycle "swap brew binary" path, kept for cases where parallel doesn't apply. Refuses to clobber without `FORCE=1`. (#158) |
| `make restore-brew-server` | Mac | Restore brew binary + clear env + restart brew. No-op when no backup. (#158) |
| `make install-remote-server` | Linux remote | scp + backup + swap + systemd notify. (#159) |
| `make restore-remote-server` | Linux remote | Same pattern as Mac restore. (#159) |
| `make test-integration-local` | Mac | End-to-end integration suite against local-built `shed-server`. (#158) |
| `make test-integration-local-fc` | Linux remote | Same suite, FC backend. (#159) |

The integration suite is the core fixture (#161): a Pytest harness
that bring-up + tears-down sheds against a known-up server, with the
"is the server ready" probe + timing assertions hardened to skip
correctly when the suite runs against a freshly-launched dev binary
(template_fallback cost inflates `agent_ms` ~4 s; not a regression).

The timing gates were split into two narrower regressions (#157):
- `test_create_agent_p50` — agent-phase p50 ≤ per-backend ceiling
  (skipped if any sample triggered `template_fallback`).
- `test_create_rootfs_template_present` — `rootfs_ms ≤ 100 ms`
  (host-side template-cache hit, no in-guest mkfs).

End result: a typical server-side PR cycle is now `make dev-server-up
&& make test-integration-local && make dev-server-down` on Mac, and
the same shape on the FC side. No binary swap, no leftover state.

### Lifecycle + packaging hardening (#150, #151, #152, #156)

**`.deb` postinst now restarts shed-server on upgrade (#150).** The
v0.5.8 → mini2/mini3 rollout surfaced that `sudo dpkg -i
shed-server_0.5.x_amd64.deb` over an existing install left the running
0.5.(x-1) process serving while `dpkg-query` reported 0.5.x — a
silent version skew between the package manager's view and the
actual binary in memory. `packaging/postinstall.sh` now does a
`try-restart` of `shed-server.service` only on **upgrade** (`$2` set);
fresh installs preserve the existing "edit `server.yaml`, then
start" contract. Closes the gap documented as a follow-up in
`docs/upgrades/v0.5.7-to-v0.5.8.md`.

**VMM-exit verification + PID-reuse guard on FC (#151).** Three gaps
in the stop/start lifecycle could silently leave shed running a
second VMM under the same name:

1. `stopShedLocked` flipped `meta.Status=stopped`/`PID=0` after
   `vm.Stop()` returned `nil`, even when the post-SIGKILL
   `waitForProcessExit` swallowed its 2s timeout.
2. `StartShed` only checked `IsRunning()` when `meta.Status=Running` —
   a stale `Status=stopped` with the process still alive bypassed
   the guard and spawned a second VMM.
3. FC's `IsRunning()` was a bare `kill -0 PID` check, false-positive
   on PID reuse after a long-stopped shed.

Fixed: lifecycle now verifies actual process exit before flipping
metadata, the start guard runs unconditionally, and FC's running
check cross-references the process command line.

**VirtioFS/9P workspace mount retry (#152).** A single transient
agent-RPC blip during the workspace mount used to kill an entire
10s VM bring-up — the only recovery was `shed delete` + recreate.
Both backends' mount paths now use a small bounded retry envelope
(new leaf package `internal/retry/`, scoped narrowly to avoid the
`config → vmimage → vmutil → config` import cycle that promoting
`withRetry` to `internal/vmutil` would have closed). Brief flakes
no longer escalate to a full failure.

**`StartShed` late-failure metadata persistence (#156).** When
`StartShed` succeeded through `PersistRunningState` and then a
downstream hook failed (most likely `MountLocalDir`, even with
#152's retries — a terminal third-attempt 9P/VirtioFS failure is
still possible), the cleanup stack unwound `remove from vms map` +
`stop VM` but never wrote `Status=Stopped` back to disk. Next list
would show the shed as `Running` with no underlying process. Now
the cleanup explicitly persists `Stopped` metadata as part of the
unwind.

### Internal refactors (#153, #154, #155)

**`StartShed` migrated to orchestrator `BackendStarter` on both
backends (#153 VZ, #154 FC).** Pre-v0.5.9, VZ and FC each had their
own ~200-line `StartShed` that duplicated metadata lifecycle, hook
sequencing, and cleanup unwinding. Both now delegate to a shared
`internal/orchestrator/BackendStarter` that owns the lifecycle
contract; each backend supplies only the actual VMM-bringup steps
via the `Starter` interface. Cuts each backend's bring-up code by
roughly half and gives the lifecycle a single source of truth (set
up to make #151's hardening simpler to land — the verification
logic now lives in one place, not two).

**Deleted `Backend.GetNetworkEndpoint` (#155).** Vestigial — zero
callers via the interface. The API layer's network-routing
(`internal/api/connect.go`) uses `Backend.DialService`, and
`Shed.IPAddress` is populated directly in each backend's
`metadataToShed`. The method was flagged as "a lie" in the
discovery doc (VZ returned a hardcoded `"127.0.0.1"`). Deleted
rather than typed-up; the contract is now `DialService`.

### Documentation (#160, #164, #165)

- **Integration suite workflow + e2e validation discipline (#160).**
  Documents the parallel-dev-server flow (post-#157/#158/#159) as the
  primary path for server-side PR validation, with the swap workflows
  deprecated and slated for deletion in the next cycle.
- **Retired discovery doc (#164).** `docs/discovery/integration-suite-server-coverage.md`
  is closed-out — every gap it identified is now addressed by #157-163.
- **Pre-release validation for build-tools + base image changes
  (#165).** New section in `docs/development/releasing.md` covering
  when `scripts/release-validation.sh` is the right gate (any change
  to `build-tools/`, `firecracker/`, `vz/`, `initramfs/`, or
  `scripts/build-*.sh`) vs the lighter local check (everything else).

### Release process — convention adoption (#166)

Adopts the `cc-plugins:release-workflows` convention now used by
every other repo in the constellation (strix, roost, prox, codelens,
envsecrets, shed-extensions). Net effect for maintainers: one
command (`/release-workflows:release vX.Y.Z`) handles changelog +
plugin.json bump + commit + tag + push, replacing the prior
"tag-then-let-CI-bump-plugin.json" pattern.

Specifically:

- **`HOMEBREW_TAP_TOKEN` + `APT_DISPATCH_TOKEN` PATs retired.** Both
  are now minted at workflow time as scoped `charliek-release-bot`
  GitHub App tokens (`owner: charliek` + `repositories: homebrew-tap`
  / `apt-charliek`) with `permission-contents: write` defense-in-
  depth. GoReleaser still reads `HOMEBREW_TAP_TOKEN` from env (it's
  token-source-agnostic); the workflow now sources it from
  `steps.tap.outputs.token`.
- **`sync-version` job deleted.** Plugin.json is now bumped LOCALLY
  by `/release-workflows:release` before tagging (the convention's
  source-tree-bump-local rule). Replaced by an inline
  `Verify plugin.json matches tag` jq cross-check in the `release`
  job that fails the release loud if a developer ever tags the
  wrong commit instead of silently fixing it up.
- **`actions/create-github-app-token@v3`** on both new mint steps
  (Node 24; resolves the upcoming Sep 2026 Node 20 EOL on GHA
  runners).
- **Branch protection ruleset on `main`** with the App
  (`charliek-release-bot`, id `3902108`) + admin role (id `5`) in
  `bypass_actors`. Previously shed had no branch protection at all.
- **New `sanity-check-app.yml`** (manual workflow) verifies the
  release-bot App reaches `charliek/shed` + `charliek/homebrew-tap`
  + `charliek/apt-charliek` before each release. Both runs validated
  before this release.
- **New `RELEASING.md`** — per-repo policy + failure-mode table +
  break-glass recovery runbooks.
- **`scripts/release/update-version.sh`** (new wrapper around the
  existing `scripts/set-version.sh`) adds a jq-verify of the bump,
  defending against the silent-failure mode of `set-version.sh`'s
  regex substitution if the JSON ever gains a nested `"version"`
  field.

All Docker pushes (build-tools + 6 image variants) intentionally
stay on `GITHUB_TOKEN` — canonical pattern for same-repo ghcr.io
packages, no need for an App token. The strict job ordering
(build-tools → vz+fc → smoke → release) is preserved; it's the
v0.5.2-era race fix that ensures the apt-charliek dispatch only
fires after every referenced ghcr image is live.

## v0.5.8 — 2026-05-29

Maintenance release closing two operational bugs surfaced while rolling
v0.5.7 out to mini2 / mini3 / the mac, plus a documented playbook for
the routine "I upgraded shed, clean up the old images and reclaim disk
space" workflow. No manifest format changes, no image cache wipe
required — not the v0.5.1 → v0.5.2 kind of upgrade.

See [docs/upgrades/v0.5.7-to-v0.5.8.md](docs/upgrades/v0.5.7-to-v0.5.8.md)
for the operator upgrade steps and the cleanup playbook.

### `shed image prune` now protects tagged manifests (#147)

Pre-v0.5.8 prune followed Docker's "tags are informational" model: only
sheds, snapshots, and in-flight create markers protected blobs. That
made `shed image pull <tag> && shed image prune` a footgun on a fresh
host — prune deleted the manifest just pulled (no shed was yet pinning
it), either leaving the tag pointing at a missing blob or silently
reverting it to an older locally-cached manifest. mini2 saw `base` flip
from the v0.5.7 manifest back to a v0.5.3 manifest with the missing
`zip`.

v0.5.8 makes tags protective: the prune walker now treats every tag's
manifest digest as live, including the manifest's transitive blobs
(config, layers, kernel, initrd, rootfs erofs). The documented cleanup
workflow is now `shed image rm <tag>` first, then `shed image prune` —
same shape as Docker's `docker rmi` followed by `docker image prune`.

`vmimage.ProtectiveRefs()` and the relevant CLI docs were updated to
match. Four new unit tests in `internal/vmimage/manager_test.go` cover
the contract (tag protects manifest + transitive blobs; untag-then-
prune deletes the orphan; prune handles a stale tag without panic;
shed-pinned manifests still protected). Two existing `internal/vz`
prune tests were updated to untag their dangling fixtures up-front (the
snapshot-pin test already followed this pattern).

### Local image builds pin the Ubuntu kernel package (#148)

Pre-v0.5.8 `initramfs/Dockerfile` and `vz/Dockerfile` each installed
the `linux-image-virtual` apt metapackage independently. The two
installs run in separate `docker buildx build` invocations with their
own BuildKit cache; when those caches diverged (common in iterative
local rebuilds via `./scripts/build-vz-rootfs.sh`), the initramfs's
staged `erofs.ko` + `libcrc32c.ko` targeted a different kernel ABI
than the booted `vmlinuz` and the VZ guest panicked with
`SHED-INIT-03: failed to mount /dev/vdb at /lower (erofs)`.

GitHub Actions builds were safe (fresh BuildKit cache per runner), so
**published images on ghcr.io are unaffected**. The bug only bit
operators iterating on the image scripts locally.

v0.5.8 pins both Dockerfiles to `ARG LINUX_IMAGE_VERSION=6.8.0-124`
and installs `linux-image-${LINUX_IMAGE_VERSION}-generic` directly. A
new `make check-kernel-pin` target (wired into `make check`) fails
the build if the two values drift apart. The `initramfs/Dockerfile`'s
module-staging `find` is scoped to the pinned kver explicitly so a
wrong pin fails fast rather than silently picking a stale module.
`firecracker/Dockerfile` is intentionally not pinned: the FC rootfs
uses the custom `KERNEL_TAG`-built kernel and doesn't install
`linux-image-virtual` at all; FC's initramfs IS the same artifact
`initramfs/Dockerfile` produces, so pinning that file covers the FC
initramfs path automatically.

`docs/reference/images.md` gains a "Kernel version pinning" section
explaining the ARG, the bump procedure, and the FC carve-out.

### Documentation (#149)

New `docs/upgrades/v0.5.7-to-v0.5.8.md` with operator upgrade steps
for both Linux/.deb (with an explicit `systemctl restart shed-server`
callout — the .deb postinst doesn't restart automatically, tracked as
a follow-up) and macOS/brew (with the manual `server.yaml` images-map
bump that brew doesn't manage). Includes the four-step image cache
cleanup playbook covering both the shed-server image store and the
local Docker layer cache. `mkdocs.yml` nav updated; `images.md`
cleanup section cross-references the new page and now correctly
reports that tags are protective.

## v0.5.7 — 2026-05-29

Minor release with a **substantive behavior change to the SSH command
channel** (#131) plus the **consistency & simplicity refactor cycle**
from `docs/discovery/platform-runtime-optimization.md` §15 Phase 1+2
landing as one bundle. Also ships the §16 integration test suite (live
on both backends), a `shed-extensions` v0.3.2 bump, and `zip` in the
base images.

### Behavior change — raw SSH is now POSIX-shell by default (#131)

`shed-server` now wraps the SSH command channel server-side in
`bash -lc <raw>` (PR #146). This makes raw `ssh shed 'cmd | pipe'`
Just Work like every other dev VM (Docker, Codespaces, Coder,
devcontainers, Zed Remote-SSH, VS Code Remote-SSH, JetBrains Gateway,
`rsync`):

- Pipes, redirects, semicolons, `$VAR`, `$(…)`, `${…}`, and bash
  builtins all fire on the guest.
- `-l` sources `/etc/profile` + `/etc/profile.d/*.sh` + `~/.profile`,
  so mise, nvm, rustup, and similar PATH-mutating tools take effect
  for SSH-driven commands.

`shed exec`'s argv-literal semantics are preserved: the CLI single-
quote-wraps each argv element before SSH, and bash treats single-
quoted text as literal data. End result — `shed exec name -- echo
'$HOME'` still echoes the literal `$HOME`. The CLI quoter
(`cmd/shed/console.go:validateAndQuoteArgs`) is now the security gate;
a real-bash round-trip test (`TestShellQuoteBashRoundTrip`, 10
metacharacter cases including `$(rm -rf /)`, backticks, embedded
newlines, UTF-8) is the audit. NUL byte rejection added to the
quoter — it's the one byte single-quote wrapping can't safely carry.

Reference docs (`CLAUDE.md`, `docs/reference/cli.md`) rewritten to
reflect the new contract.

**Possible breakage:** anyone whose tooling relied on the old "raw
`ssh shed 'cmd'` runs `cmd` as literal argv (no shell)" semantics
will now see bash expansion. The `shed exec` CLI path is unaffected.

### Code shape (§15 Phase 1+2 — orchestrator refactor)

Same speed (or marginally faster), substantially less code, less
per-backend divergence. Every future feature / speed PR is now a
one-place change.

- **`healthPollInterval` 150 ms → 50 ms** (PR #133). Saves up to
  100 ms per create with zero downside — the agent gets probed a bit
  more often during the first ~1 s of boot, then never again.
- **`backend.Progress` split into `backend.Phase` + `backend.Status`**
  (PR #135). `Phase(ctx, name)` moves the timer; `Status(ctx, message)`
  emits the SSE event. Every call site migrated; the PhaseTimer log
  line now shows no duplicate phase entries.
- **Divergent backend config defaults documented** (PR #136). Per-field
  comments in `internal/config/server.go` explain the "why" of each
  VZ-vs-FC value (e.g. why `StartTimeout` is 60 s on VZ vs 30 s on FC).
- **LIFO cleanup-stack helper** (PR #137). New
  `internal/backend/cleanup.go` provides a `Register("step", fn)` →
  `RunReverse(err)` pattern; both backends' `CreateShed` migrated. ~250
  lines of inline rollback removed; future cleanup logic provably
  correct.
- **Shared `BackendCreator` orchestrator** (PR #138). New
  `internal/backend/orchestrator/create.go` implements the `CreateShed`
  lifecycle once against a small interface; contract tests against a
  mock backend pin the design.
- **VZ + FC `CreateShed` migrated to the orchestrator** (PRs #139 and
  #140). Each backend's `CreateShed` is now a thin `BackendCreator`
  implementation that delegates to the shared orchestrator. The inline
  duplicate code in both `client.go` files is gone.

`StartShed` / `--from-snapshot` orchestrator migration is deferred to
a follow-up (tracked in `docs/discovery/platform-runtime-optimization.md`
§15 as "2e (deferred)").

### Tests

- **§16 integration test suite MVP** (PR #132, plus operator docs in
  #141 and FC e2e calibration in #142). Pytest + subprocess, in-tree at
  `tests/integration/`, managed with `uv`. Five MVP tests parameterized
  over `["vz", "fc"]`; runs against VZ on the mac and FC against
  `mini3` (the brew-installed `my-server` and the SSH-attached
  `mini3` server respectively). `make test-integration` is now the
  canonical "before each PR" check.
- **`test_extensions_image_smoke`** (PR #145). First integration
  coverage for the `extensions` image variant: creates with
  `image="extensions"` and asserts each shed-extensions binary
  (`shed-ext-ssh-agent`, `shed-ext-aws-credentials`,
  `docker-credential-shed`) is present at `/usr/local/bin/` and
  executable. Gate for future shed-extensions bumps.
- **`test_exec_shell.py`** (PR #146). Eight tests × 2 backends that
  encode the #131 security model: five raw-SSH tests prove the
  `bash -lc` wrap fires (pipes, `$HOME`, `$(hostname)`, bash builtins,
  `/etc/profile.d` sourcing); three `shed exec` tests prove argv stays
  literal across the bash reparse.

### Images

- **`zip` added to the base apt-install** (PR #144, closes #129). Both
  VZ and Firecracker Dockerfiles already shipped `unzip`; sdkman,
  gradle wrappers, and any other tool that *creates* zip archives
  needed the matching `zip` packager. Negligible image-size cost
  (~110 KB), parity with `unzip`. Affects all three image variants
  (base / extensions / full).
- **shed-extensions v0.3.1 → v0.3.2** (PR #145). Picks up the upstream
  fixes for Touch ID approval in clamshell mode (#13/#14), Docker
  credential helper PATH under launchd (#15), and a handful of docs /
  Homebrew quality-of-life improvements. Affects the `extensions` and
  `full` image variants (the `base` variant doesn't layer
  shed-extensions).

### Docs

- **Runtime optimization discovery doc updated** (PR #143). §0 records
  §15 Phase 1 (#133, #135, #136), §15 Phase 2 (#137, #138, #139, #140),
  and §16 MVP (#132, #141, #142) all landed on `main`. Per-sub-phase
  `**Status:**` lines added throughout §15a / §15b. §15b 2d / 2e
  reflects the execution-time split (FC `CreateShed` migration vs the
  deferred `StartShed` migration). §15c Phase 3 marked deferred. §16
  milestone block captures the MVP plus the PR #142 calibration finding
  (delete-between-samples + FC ceiling bump 2100 → 2900 ms).
- **Integration test operator guide** in `CLAUDE.md` and the new
  Development → Testing docs page (PR #141).

## v0.5.6 — 2026-05-28

Patch release shipping a **Firecracker-only `shed create` speedup —
~20 % faster** (median wall-clock −450 ms on mini3) — plus structural
test coverage that locks the boot-ordering invariants the speedup depends
on across both backends. Drop-in upgrade from v0.5.5 — no config or
on-disk format changes; the FC win takes effect once the rebuilt FC base
image lands.

### Speed

- **Firecracker firstboot reorder** (PR #126). Order
  `shed-firstboot.service` `Before=ssh.service` only on the FC unit
  (was: also `Before=sysinit.target` / `shed-agent.service` /
  `network-setup.service`). The broad ordering gated `shed-agent` —
  which `shed create` waits on — by firstboot's full crng-blocked
  `ssh-keygen` duration. Measured on mini3 (apples-to-apples, same
  shed-server + same build pipeline, only the `Before=` line differs):
  median `agent` phase **2256 ms → 1804 ms (−452 ms / ~20 %)**; every
  after-sample beats every before-sample. `--repo` creates show **no
  regression** (FC has a static IP — `network-setup` stays fast and
  still gates the agent, so clone has the network when it runs).
  Host-key uniqueness invariant preserved (`Before=ssh.service` keeps
  keygen-before-sshd).

  The same change was deliberately **not** applied to VZ. On VZ the
  identical edit was measured to buy only ~150 ms on plain creates
  (fixed VMM/kernel overhead is the ceiling) *and* to regress `--repo`
  creates by ~450 ms (network readiness no longer overlaps boot — the
  host pays the DHCP wait serially before clone). See
  `docs/discovery/platform-runtime-optimization.md` §14 for full
  measurements and reasoning.

### Tests

- **Guest unit-file ordering invariants locked** (PR #127). Seven
  pure-file-parsing Go tests in
  `internal/vmutil/guest_unit_ordering_test.go` lock the boot-ordering
  decisions across FC and VZ — the FC firstboot `Before=ssh.service`
  edge and bans on the three removed `Before=` tokens; the FC
  `network-setup.service` `Before=shed-agent.service` static-IP
  guardrail; the *intentional* VZ non-changes (broad firstboot ordering
  + `network-setup` agent gating preserved); plus banned `After=`
  tokens on both backends' `shed-agent.service` and `WantedBy=`
  presence on firstboot + network-setup so the edges aren't unreachable
  code. Runs on every PR (no VM needed; GitHub-hosted runners are
  fine).

### Docs

- **Platform runtime optimization writeup updated** (PRs #125, #126,
  #127). §0 now records the corrected understanding — the agent's gate
  in the shipped config is `shed-firstboot` (~633 ms), not the
  projected `network-setup` DHCP wait. New §14 records the full v0.5.6
  measurements (VZ A/B and FC apples-to-apples) plus the failure-mode
  honesty for the security invariant (`Before=` is an ordering edge,
  not failure-propagation). §10 / §13 reframed: 3c is superseded; 3b
  shipped FC-only.

## v0.5.5 — 2026-05-27

Patch release fixing a v0.5.4 regression where the macOS/VZ copy-on-write
upper (the Phase 2 `shed create` speedup) silently failed to activate.

### Fixed

- **VZ upper-template activation** (PR #124). v0.5.4 resolved the
  `shed-build-tools` image tag without the leading `v` (`:0.5.4` instead
  of the published `:v0.5.4`), so the upper-template mint failed and
  `shed create` fell back to the slow in-guest `mkfs.ext4` — the v0.5.4
  speedup did nothing on a fresh install. Fixed by routing all
  build-tools ref resolution through one canonical helper that always
  v-prefixes the release tag. A warm `shed create` on Apple Silicon is
  back to ~1.8s (down from ~6s). Firecracker was unaffected.

## v0.5.4 — 2026-05-27

A performance-and-robustness pass on shed creation, plus plugin
distribution. Drop-in upgrade from v0.5.3 — no config or on-disk format
changes.

### New

- **Copy-on-write upper on macOS/VZ** (PR #119). A new shed's writable
  upper is now cloned (APFS `clonefile`) from a pre-formatted ext4
  template instead of being formatted inside the guest on first boot.
  That removes the multi-second in-guest `mkfs.ext4` from the boot
  critical path — a warm-cache `shed create` on Apple Silicon drops from
  ~5.9s to ~1.7s. The template is minted once via `mkfs.ext4` in the
  `shed-build-tools` container (which now ships `e2fsprogs`) and is
  sparse (~4 MB on disk for a 5 GB filesystem). Best-effort with a safe
  fallback to in-guest formatting, so creation never regresses. VZ only;
  Firecracker's in-guest `mkfs` is already fast (~0.2s) and is unchanged.

- **Per-phase create timing** (PR #118). `shed-server` logs one
  structured timing line per `shed create`, breaking the operation into
  named phases (image, rootfs, vm, agent, mounts, …). Server-log only —
  the CLI output is unchanged; this is a developer signal for seeing
  where create time goes. The agent health-poll interval was also
  tightened (500ms → 150ms).

- **Distributable Claude Code plugin** (PR #117). shed ships as a
  Claude Code plugin.

### Fixed

- **network-setup interface-rename robustness** (PR #123).
  `network-setup.sh` now re-resolves the network interface on each poll
  rather than latching a name the kernel may rename (`eth0 → enp0s1`),
  and the Firecracker script detects the interface dynamically instead
  of hardcoding `eth0`. Prevents a ~30s boot stall if network setup runs
  before the rename — a latent issue today, and a prerequisite for moving
  first-boot identity setup off the create critical path.

## v0.5.3 — 2026-05-25

Follow-up to v0.5.2's architectural fix. Two small features and a
big cleanup. No format changes, no breaking changes — drop-in
upgrade from v0.5.2.

### New

- **`shed-server doctor`** (PR #108). One-pass health report
  against the local Firecracker install. Each check reports
  `PASS` / `WARN` / `FAIL`; exits non-zero if any `FAIL` fires.
  Covers: KVM readable, docker on PATH, firecracker binary
  present, server.yaml parses, kernel_path sanity, bridge
  interface state, every installed tag's manifest + erofs blob
  chain, every enabled extension's manifest, systemd unit
  active. Honors `--config` so it reports the actual file in
  use, not a guess. Linux-only. Run it first when something
  feels off.

- **Registry-pull retry envelope** (PR #108). Wraps the two
  network-touching calls (`remote.Get` for the manifest
  descriptor, `remote.Layer + Compressed` for each loose blob)
  in a 3-attempt exponential backoff (1 s, 4 s). Retries on
  transient shapes — `net.OpError`, `io.EOF` /
  `io.ErrUnexpectedEOF`, `transport.Error` 5xx + 429, plus
  case-insensitive DNS / connection-reset / TLS-handshake-timeout
  string fallbacks. 4xx errors and context cancellations
  short-circuit so the user sees real diagnostics immediately.
  Closes a class of "shed-server pull-images failed because
  ghcr blipped for 200 ms during the kernel blob fetch" papercuts.

### Improved

- **File-credential migration hint** (PR #108). When a server.yaml
  credential's `source` is a regular file (not a directory) the
  validator now embeds the exact `~/.shed/sync.yaml` snippet the
  user needs, with name + source + target substituted in — lifts
  the error from "what do I do now?" to "paste this."

### Removed

- **Docker-daemon fallback dead code** (PR #107, -558 lines).
  v0.5.2 already made the on-host `mkfs.erofs` + docker-create +
  docker-export flatten path unreachable; this release deletes the
  dead implementation:
  - `internal/vmimage/manager.go`: `convertAndInstall` method;
    fallback branches in `EnsureImage` and `PullImage` — both now
    surface the registry pull error verbatim instead of the old
    "registry pull and docker fallback both failed" compound
    message.
  - `internal/vmimage/convert.go`: `convertFromDockerExport` and
    its helpers (`gzipFileWithDigests`, `mustBlobPath`,
    `dockerCreate`, `dockerExport`, `dockerRemove`,
    `dockerRunScript`, `extractKernel`, `extractInitrd`).
    `Convert()` now requires `OCIArchivePath`.
  - `internal/vmimage/cache.go`: `EnsureLowerFromManifest` (the
    local mkfs.erofs invocation), `CacheLowerExists`,
    `RemoveCachedLower`. Survivors: `CacheLowerPath`,
    `CacheLowerSize`, `CacheLowerExt` — still used by
    `PruneImages` to sweep v0.5.1-era legacy cache files during
    the upgrade window.
- **`erofs-utils` dep dropped** from both the brew formula and
  the deb. Hosts running shed-server no longer invoke
  `mkfs.erofs` anywhere. `apt remove erofs-utils` is safe after
  upgrade.

### CI

- Linux smoke gate (added in v0.5.2) now runs on every PR
  against this release line too — both v0.5.3 PRs were validated
  end-to-end on a fresh ubuntu-latest runner before merge.

### Net diff

```
v0.5.2 → v0.5.3:  9 files changed, +752 / -678 lines
```

## v0.5.2 — 2026-05-25

### Overlay-stability release

v0.5.1 shipped an end-to-end-broken Linux install: the on-host
`mkfs.erofs --tar=f -E force-inode-compact -z lz4` invocation in
`internal/vmimage/cache.go` triggered a writer bug in `erofs-utils`
1.7.1 (Ubuntu noble, Pop!_OS 24.04, the apt-charliek deployment
targets — and the version most distros currently package) where
inodes were marked as using big pcluster without the matching
superblock feature flag. The guest kernel then rejected the rootfs
at boot with `erofs: per-inode big pcluster without sb feature for
nid N`, `z_erofs_read_folio: failed to read, err [-117]`. Userspace
couldn't read /workspace and `shed create` failed at the 9P mount
step. The Docker backend and macOS VZ stack were unaffected; only
Linux+Firecracker was broken.

v0.5.2 moves `mkfs.erofs` off the host entirely. The image producer
mints the read-only rootfs erofs once at publish time inside the
new pinned `shed-build-tools` container, then ships the result as a
content-addressed OCI blob carried by a new manifest annotation.
Hosts download the blob and mount it directly — no local
`mkfs.erofs`, no host-distro variance in the on-disk filesystem
layout, no ~30 s mkfs step at first `shed create`. Net disk usage
on hosts drops ~37 % per cached image (the duplicate
`cache/<digest>.erofs` file goes away — the blob *is* the cache).

#### Breaking changes

- **Pre-v0.5.2 images are rejected at boot.** Cached images from
  v0.5.1 or earlier lack the new
  `io.shed.rootfs.erofs.digest` annotation and fail with a precise
  error pointing at the upgrade command. No silent fallback. **See
  the [v0.5.1 → v0.5.2 upgrade guide](docs/upgrades/v0.5.1-to-v0.5.2.md)
  for the required `shed image rm` / `shed-server pull-images`
  sequence — users upgrading from v0.5.1 must wipe their cached
  images and re-pull.**
- **Host-side `erofs-utils` is no longer required.** Existing
  installs can `apt remove erofs-utils` after upgrade (the deb's
  declared `Depends:` will be relaxed in a follow-up; it currently
  still pulls erofs-utils as a transitive courtesy, harmless dead
  weight).
- **`shed exec` argv handling (carried from rev 1 of this changelog;
  same notes apply).** The backend no longer wraps non-empty argv
  in `bash --login -c "<joined argv>"` — argv is exec'd directly,
  Docker-style. Tools installed via rustup / mise / nvm / `~/.profile`
  PATH additions need an explicit `shed exec name -- bash -lc 'tool'`
  to source those startup files; the v0.5.1 wrapped path silently
  mangled pipes, redirects, and nested quotes through the SSH
  argv-as-string round-trip and is gone.

#### Architecture

- **New `ghcr.io/charliek/shed-build-tools:vX.Y.Z` image** (PR #103)
  carries pinned versions of the binaries shed invokes during
  image publish — currently `erofs-utils` v1.9.1 built from upstream
  source, `mkfs.erofs` / `dump.erofs` / `fsck.erofs`. Tagged in
  lockstep with shed-server releases. See
  [`docs/reference/build-tools.md`](docs/reference/build-tools.md)
  and the new `build-tools/` directory.
- **`io.shed.rootfs.erofs.digest` annotation** (PR #104) on every
  shed-built manifest points at the prebuilt erofs blob. Pull and
  push paths walk the annotation alongside
  `io.shed.kernel.digest` / `io.shed.initrd.digest` (same loose-blob
  pattern). `resolveManifestLower` now resolves the annotation to
  a blob path; the legacy `cache/sha256/<manifest-digest>.erofs`
  materializer is unreachable and gets fully deleted in v0.5.3.
- **`shed image build --build-tools-version <tag>`** pins the
  build-tools image the CLI invokes when minting the erofs.
  Defaults: release builds resolve to `ghcr.io/.../shed-build-tools:vX.Y.Z`
  matching the shed CLI's own version; dev builds resolve to a
  locally-built `shed-build-tools:dev` (no registry round-trip).

#### Operations

- **`resolveBaseRootfs` populates `Digest` on warm-cache hits** (PR
  #102) so `EnsureImage` no longer returns an empty digest that
  the backend defense layer would reject as "image resolved to a
  path outside the blob store." Without this fix any v0.5.x
  default `shed create` (no `--image` flag) against a server with
  the base image already pulled would hit the defense.
- **New `scripts/smoke-test-linux.sh` and `.github/workflows/smoke-linux.yml`**
  (PR #105) install-only smoke that gates every push to `main` and
  every release tag. Catches the v0.5.1 regression class (binary
  builds + unit tests pass, fresh apt install does not).
- **Sequenced release workflow** (PR #106) — `release.yaml` is gone;
  the goreleaser + apt-charliek dispatch jobs now live in
  `publish-images.yaml` after `publish-build-tools` →
  `publish-vz` / `publish-fc` → `smoke`. Eliminates the parallel
  race where the deb could go live on apt-charliek before its
  referenced ghcr images existed.

#### Documentation

- **New `docs/reference/build-tools.md`**: shed-build-tools image
  purpose, versioning, bump procedure.
- **New `docs/upgrades/v0.5.1-to-v0.5.2.md`**: required wipe + pull
  sequence, rationale for no on-host fallback, rollback notes.
- **Refreshed `docs/reference/images.md`** and `storage-model.md`:
  prose updated for the erofs-as-blob model; removed `mkfs.erofs`
  prerequisite from the "what hosts need" section.
- **`CLAUDE.md`** updated with the new model.

## v0.5.1 — 2026-05-22

### Flatten + host-native materialize

- **One flattened erofs lower per OCI manifest** replaces the
  multi-layer overlay + per-layer materialize VM. On both Linux and
  macOS, shed now reads every layer of an OCI manifest in order,
  applies OCI whiteouts (`.wh.foo`, `.wh..wh..opq`), and feeds the
  merged tree to `mkfs.erofs --tar=f` to produce one content-addressed
  erofs file at `{imagesDir}/cache/sha256/<manifest-digest>.erofs`.
  Boot becomes "mount one read-only lower + per-shed writable upper +
  overlay." This is the same pattern Lima / Colima / OrbStack /
  Podman Machine v5+ use.

- **Image sizes** stayed at the v0.5.1 plan numbers from earlier
  pre-release commits (drop nano/vim/jed/htop + locale strip, drop
  Cursor CLI from full, Bun replaces Node+Python+uv,
  linux-image-virtual instead of -generic, Docker moved to full). VZ
  full is ~50% smaller compressed than v0.5.0.

- **Vsock fix on VZ:** `linux-image-virtual` ships a minimal recommended
  modules tree without `vmw_vsock_virtio_transport[_common]`. The VZ
  rootfs Dockerfile now derives the matching
  `linux-modules-extra-<kvers>-generic` package name from the
  installed kernel and installs it alongside, so shed-agent can open
  its vsock listener.

- **systemd-firstboot.service is now masked** in the VZ rootfs. With
  the transient `/etc/machine-id → /run/machine-id` symlink, systemd
  evaluates `ConditionFirstBoot=yes` on every boot and the interactive
  wizard would block `sysinit.target` → `multi-user.target` →
  `shed-agent.service` indefinitely on `/dev/console`.

- **Boot-log preservation:** failed `CreateShed` paths now copy
  `console.log` to `{imagesDir}/../logs/<name>-<timestamp>.log` before
  the instance dir is removed. No more "rerun and hope it repeats"
  debugging.

### Required cleanup on upgrade

The cache layout changed from `<layer-digest>.{erofs,ext4}` to
`<manifest-digest>.erofs`, and the new prune walker only recognizes
`.erofs`. v0.5.0 `.ext4` cache files become orphans — wipe the cache
dir once on upgrade. The cache lives under `{images_dir}/cache/`, so
the exact path depends on backend config:

```bash
# Mac (VZ default)
rm -rf ~/Library/Application\ Support/shed/vz/cache
# Linux (Firecracker default — uses images_dir/cache)
rm -rf /var/lib/shed/firecracker/images/cache
```

`shed image prune` will GC any orphaned `.erofs` files automatically
on subsequent runs.

### New runtime dependency

`mkfs.erofs` (from `erofs-utils`) must be on PATH on the host running
`shed-server`:

- macOS: `brew install erofs-utils`
- Debian/Ubuntu: `apt install erofs-utils`

Shed errors at first materialize attempt with an install hint if
absent.

### Other

- `EnsureImage` prefers registry-direct pull over docker-export
  (closes #98). Cuts cold-start materialize wallclock on first pull,
  and produces multi-layer manifests with the shed-overlay initrd
  annotation rather than single flattened blobs.
- Local tags always win over the configured registry ref. Previously
  the server config's `ref:` had to match the manifest's
  `io.shed.source-ref` exactly, which broke local rebuild workflows.
- `shed image build` derives `--source-ref` from `shed version` so
  the published annotation tracks releases automatically.
- Initramfs no longer ships `mkfs.erofs` + `libgcc_s.so.1` + busybox
  tar/gunzip — materialize happens host-side, the initrd just mounts.
  Initramfs shrinks ~5-10 MB.
- `internal/firecracker/kernel-config-docker.fragment` keeps
  `CONFIG_EROFS_FS=y` so the Firecracker kernel can mount erofs
  without the host insmod choreography.

## v0.5.0 — 2026-05-18

### Release-readiness fixes

- **`shed image push --local` works from a Linux runner pointing at a
  `default_backend: vz` config.** The publish workflow stages a small
  config file with the relevant backend block; on the `ubuntu-24.04-arm`
  runner that meant `vz:`, which the strict validator rejected
  (`vz backend is only supported on macOS`). Added
  `config.LoadServerConfigForCLI` + `ServerConfig.ValidateNoHostCoupling`
  for the image-only flows that never start a VM. Callers that *do* start
  a VM (shed-server serve) still use the strict path.
- **`shed image build` picks the right tag prefix AND `io.shed.source-ref`
  for cross-builds.** The PR #92 fix corrected `--platform` for
  `--target shed-vz-*` invocations from a Linux runner, but the tag
  prefix (`shed-fc-` vs `shed-vz-`), kernel-extraction flag, and initrd
  flag were still derived from `runtime.GOOS` — so a Linux-built
  `shed-vz-full` image landed on disk tagged `shed-fc-full:latest` and
  its manifest's `io.shed.source-ref` annotation lied accordingly. The
  per-target table is now centralized and driven off `--target`
  uniformly. Added `--source-ref <ref>` so CI publish workflows can pin
  the annotation to the final `ghcr.io/charliek/shed-*-*:<version>` ref
  the image will be pushed to; without this, the server's
  `resolveImage` cache-hit check (which compares the manifest annotation
  against the configured `ref:`) missed on every subsequent `shed create`
  and forced a re-pull.
- **`shed image build` picks the right `--platform` for the target backend.**
  Previously the CLI inferred the platform purely from `runtime.GOOS`, so
  invoking `shed image build --target shed-vz-*` from a Linux runner (e.g.,
  the `ubuntu-24.04-arm` GitHub Actions runner used by the publish workflow)
  silently flipped to `linux/amd64`, which then crashed the per-layer ext4
  materialization step with `exec format error`. The CLI now inspects
  `--target shed-vz-*` / `shed-fc-*` and picks `linux/arm64` / `linux/amd64`
  accordingly; an explicit `--platform <plat>` flag is available as an
  override.
- **`POST /api/images/pull` no longer 405s.** A chi router precedence
  bug routed `POST /api/images/pull` to the parametric
  `DELETE /api/images/{name}` handler, returning `405 Method Not Allowed,
  Allow: DELETE`. Fixed in `internal/api/server.go` by mounting the
  static `/pull` route before the parametric `/{name}` sibling.
- **Legacy bundled-blob layout now surfaces an actionable error.**
  v0.4.x users upgrading without wiping `images_dir` previously hit a
  cryptic `... is a directory` error. `internal/vmimage/ocilayout.go`
  now detects this case and wraps it as `ErrLegacyBundledBlob`; the CLI
  surfaces a message pointing the user at [`docs/upgrades/v0.4-to-v0.5.md`](docs/upgrades/v0.4-to-v0.5.md).
- **`image has N layers (max 16)` rejection now includes a recovery
  hint.** `internal/vmimage/registry.go` extends the pull-time
  `MaxLayers` error with concrete next steps (wait for the v0.5.0+
  published image, or rebuild locally with the backend's build script).
- **`SHED-INIT-04` panic cross-references the upgrade guide.** The
  initramfs overlay-mount panic in `initramfs/init` now points at
  [`docs/upgrades/v0.4-to-v0.5.md#shed-init-04-panic-during-vm-boot`](docs/upgrades/v0.4-to-v0.5.md#shed-init-04-panic-during-vm-boot)
  so operators know to refresh the cached initramfs by re-running the
  build script.
- **New top-level [`docs/upgrades/v0.4-to-v0.5.md`](docs/upgrades/v0.4-to-v0.5.md).** Walks v0.4.x →
  v0.5.0 with a "what's new", a keep/lose table, links into the
  backend-specific wipe steps in `vz-setup.md` / `fc-setup.md`, and a
  recovery-scenarios index keyed off the error strings users actually
  see (cryptic "is a directory", `>MaxLayers`, `SHED-INIT-04`, the
  now-fixed 405 on `/api/images/pull`). Added to the MkDocs nav under
  "Getting Started".
- **Getting-started doc gap repairs.**
  [`quick-start.md`](docs/getting-started/quick-start.md) now frames the
  published-vs-source install paths explicitly, routes source builders
  into the backend setup guides instead of dead-ending at `make build`,
  and drops the stale `VERSION=0.3.3` pin in favor of a
  `gh release view`-driven lookup.
  [`vz-setup.md`](docs/getting-started/vz-setup.md) and
  [`fc-setup.md`](docs/getting-started/fc-setup.md) gain a working
  local-build `server.yaml` example (omit `base_rootfs` + `images:`,
  rely on tag auto-discovery, pass `--image <tag>`), demote
  `kernel_path` / `initrd_path` to optional fallbacks for OCI images,
  spell out `shed-server serve --config <path>` (the binary doesn't
  read `~/.config/shed/server.yaml` by default), document `uppers_dir`
  as an optional FC config field that should track non-default
  `instance_dir`, restructure the FC manual-setup section so source
  builders see it as required rather than optional reference material,
  call out the Go-on-host requirement for the FC rootfs build script,
  and reframe `shed server add localhost` so users know it registers
  under the server's `name:` field (with `shed server list` to confirm).
  Concrete `:v0.5.0` placeholders replace the unresolvable `:v{version}`
  pseudo-syntax.

### Storage rewrite (Phase A / B / C)

- **`shed image build` preserves the docker layer structure (multi-layer per variant).** The previous flow flattened every variant to a single layer via `docker create` + `docker export`, defeating cross-variant sharing on disk. The new flow uses `docker buildx --output type=oci,dest=<tar>` and ingests each layer blob into the local store, so `base`, `extensions`, and `full` share their common parent layers byte-for-byte. The `vz/` and `firecracker/` Dockerfiles are consolidated (BuildKit 1.7 `--mount=type=bind` for context staging) to keep each variant under the 16-layer `MaxLayers` cap: VZ ships 5 / 7 / 9 layers and FC ships 6 / 8 / 10. Measured on an arm64 Mac with all three VZ variants built locally: **~5.0 GB total** (1.7 GB blobs + 3.3 GB cache) versus **~12 GB** for the same three under the old flattened model — about **60% less disk** for users who keep multiple variants installed. Single-variant footprint is roughly unchanged; the gain is from cross-variant blob dedup.
- **Multi-layer boot fixes surfaced during validation.** Three latent bugs only triggered once a variant carried more than one lower:
  - **vfkit bootloader cmdline:** `--bootloader linux,kernel=,initrd=,cmdline=…` is comma-separated key=value, and `shed.lowers=/dev/vdb,/dev/vdc,…` embeds commas vfkit interprets as bogus options (`unknown option /dev/vdc`). Switched to the dedicated `--kernel` / `--initrd` / `--kernel-cmdline` flags.
  - **`/proc` mounted after cmdline parse:** the initramfs read `/proc/cmdline` before mounting `/proc`, silently fell through to the legacy `LOWER_DEVS=/dev/vdb` fallback — accidentally correct for one lower, wrong for N. Fixed by mounting `/proc`, `/sys`, `/dev` first.
  - **`overlay.ko` module probe path:** only looked under `/lib/modules/…`, but on Ubuntu 24.04 the `/lib → /usr/lib` symlink lives in the bare-distro layer while the kernel module ships in the APT layer; inside an isolated layer ext4 only `/usr/lib/modules/…` resolves. Now probes both paths in every lower.
- **BREAKING — content-addressed image store + tag indirection (storage rewrite, Phase A):** The flat-file image cache (`{name}-rootfs.ext4` + `.source` sidecar + `.lock`) is gone, replaced by a Docker-style blob store at `{images_dir}/blobs/sha256/<digest>/` with tags at `{images_dir}/tags/<tag>.json`. Image identity is now the sha256 of the produced ext4 bytes. Tags name digests; multiple tags can point at one digest with no extra disk cost. **All existing images and sheds must be discarded on upgrade** — `shed-server` refuses to load v1 instance metadata with a clear delete-and-recreate message. Tag → blob resolution is atomic (tmp + rename), and concurrent EnsureImage/InstallBlob calls serialize per-tag and per-digest via flock. See [`docs/reference/storage-model.md`](docs/reference/storage-model.md) for the layout and lifecycle commands.
- **BREAKING — overlay-in-guest boot (storage rewrite, Phase B):** Each shed now boots from a writable per-shed *upper* (sparse `uppers/<name>/upper.ext4`, default 5 GB) mounted on top of the shared read-only *lower* (the cached blob's `rootfs.ext4`) via a new busybox-based initramfs that builds the overlay and `pivot_root`s into the merged tree. Both backends now pass the initrd from the per-image blob dir and append a second virtio-blk drive for the lower; the kernel cmdline drops `root=` and adds `shed.upper=` / `shed.lower=`. `CopyRootfs` is gone from the create path; new `EnsureUpper(uppersDir, name, sizeBytes)` allocates the sparse upper and the in-guest initramfs `mkfs.ext4`-formats it on first boot. Per-shed disk cost drops from a 2-5 GB rootfs clone to a few hundred MB of written upper, *independent of host filesystem reflink support*.
- **`shed create --upper-size <N>G` + `upper_size_default` config (storage rewrite, Phase B).** New per-shed override (1G-100G validated; integer-overflow guarded) plus per-backend default `upper_size_default: 5G`. Stored as `upper_path` + `upper_size_bytes` in instance metadata.
- **`shed reset <name>` (new, storage rewrite, Phase C).** Wipes and recreates the per-shed writable upper while leaving the shared lower image and `/workspace` (mounted post-boot, outside the overlay) untouched. Requires the shed to be stopped. `Backend.ResetShed` + `POST /api/sheds/{name}/reset` + `SHED_NOT_STOPPED` (409) sentinel.
- **Snapshots capture only the upper (storage rewrite, Phase C).** `shed snapshot create` now clones the per-shed upper instead of the merged rootfs, so snapshots are typically a few hundred MB rather than a full image. Snapshot metadata gains `lower_digest` pinning the underlying-blob digest; spawning from a snapshot inherits the pin. `shed snapshot info` warns when the pinned lower digest is no longer cached (`Lower digest: sha256:... (MISSING — pull or rebuild the image before spawning)`), and `shed create --from-snapshot` fail-fasts with an actionable error pointing at the original image tag.
- **`shed system df` per-shed accounting (storage rewrite, Phase C).** The per-shed row now reports only the writable upper (typically hundreds of MB), and the shared lower image is reported once under `images` rather than duplicated under every shed pinning it. The "physical bytes may overcount shared extents on APFS / reflink" caveat is dropped — the upper/lower split removes the double-counting. CLI numbers should now match `du -k` to within a few KiB.
- **Initramfs build pipeline (storage rewrite, Phase B).** New top-level `initramfs/` directory with a busybox-based init script and a dedicated `initramfs/Dockerfile` for the cpio.gz build. `scripts/build-initramfs.sh` wraps the Dockerfile stage. `scripts/build-{vz,firecracker}-rootfs.sh` stage the initramfs into a tempfile and call the new `shed image install` Go subcommand to atomically install the rootfs + kernel + initrd into the content-addressed blob store, advancing the variant tag. The legacy `scripts/install-blob.sh` (a partial bash re-implementation of the install protocol — no fsync, no flock, no JSON escaping) is deleted; the build pipeline now goes through the same code path as the runtime EnsureImage flow.
- **Metadata schema v2 (FC + VZ).** Instance metadata gains `lower_digest` + `lower_image_tag` + `upper_path` + `upper_size_bytes`, recorded at create time and inherited from snapshots when spawning. Snapshot metadata gains `lower_digest` so spawn-from-snapshot inherits the underlying-blob pin. Pre-v2 metadata loads now error with an actionable message pointing operators at manual `rm -rf` cleanup of the instance directory (since `shed delete` itself goes through `LoadMetadata` and would hit the same error).
- **Refcount-protected prune.** `shed image prune` walks `instances/*/metadata.json` and `snapshots/*/snapshot.json` and removes any blob whose digest has zero shed/snapshot references. Tags do **not** protect a digest (Docker model). `shed image rm <tag>` removes the tag only; the blob persists for `prune` to GC. `shed system prune --images` is wired through the same refcount path. The ref scanner fail-closes on snapshot-load errors so a transient I/O failure can't trick prune into deleting a still-pinned blob.
- **CLI:** `shed image ls` (alias `list`), `shed image rm` (alias `delete`), `shed image inspect <tag-or-digest>`, `shed image tag <src> <new>`, `shed image pull <docker-ref> [-t <tag>]`, `shed image install --rootfs <path> [...]`, `shed reset <name>`. `shed image build` now writes into the blob store and advances a tag — no `.source` sidecar, no `LinkCachedImage` hop. `shed image ls` output gains DIGEST and IN USE columns. `shed image rm` no longer claims `(freed X)` — only the tag is removed; `shed image prune` reclaims the blob. `shed create` gains `--upper-size`.
- **API:** New `POST /api/images/tag`, `POST /api/images/pull`, `GET /api/images/inspect/{name}`, `POST /api/sheds/{name}/reset`. `ImagesResponse.images[]` gains `digest`, `tag`, and `in_use` fields. `Snapshot` wire format gains a transient `lower_cached` bool (recomputed on each read; never persisted). `CreateShedRequest` accepts `upper_size_bytes`. Sentinel error mapping: `ErrShedNotStoppedSentinel` → 409 `SHED_NOT_STOPPED`; `ErrNotSupportedSentinel` → 501 (VZ-on-Linux + FC-on-darwin stubs both map here). Tag and pull request handlers return 400 `INVALID_REQUEST` for malformed JSON / empty fields / unsafe tag names.
- **Server config resolution.** `config.ResolveImage` and `config.ResolveBaseRootfs` look up cached images through the blob-store tag layer. A blob is treated as fresh when its `manifest.source_ref` matches the configured Docker ref; otherwise the resolver returns the Docker ref so `EnsureImage` can pull a new digest and advance the tag.
- **`shed-server pull-images`.** When `base_rootfs` and an `images:` entry share a Docker ref, `_base` is now realized as a tag pointing at the same digest — no hardlink dance needed (the blob is shared by definition).
- **Removed:** `vmimage.{CheckCache,WriteSource,SourceFilename,LinkCachedImage}` and the legacy `*-rootfs.ext4` flat-file layout. `inUseImageNames` closure plumbing replaced by a `vmimage.RefScanner` interface that returns shed + snapshot digest references.
- **Tests:** New `internal/vmimage/blobstore.go`, `refs.go`, plus rewritten manager/convert tests covering digest determinism, install atomicity, refs scanning, tag resolution, and config ResolveImage's tag-aware fast path. FC + VZ test fixtures install fake blobs through the public `vmimage` API rather than seeding flat files.

## v0.4.4

- **`POST /api/sheds` clone failures now reach the SSE stream (#84):** When `git clone` failed inside a freshly-created shed, the failure was logged to journald via `log.Printf` but never emitted on the SSE stream — the API client saw `event: complete` while `/workspace` was actually empty. Both backends (`internal/vz/client.go`, `internal/firecracker/client.go`) now emit a `progress` event with `warning: true` on clone failure, and a `Repository cloned` `progress` event on success. The `repo` phase always has a terminal event regardless of outcome. Shed creation itself is still considered successful when only the clone fails — the shed VM is healthy, the operator can clone manually after fixing whatever — so the schema-additive `warning: true` field is non-breaking for existing SSE consumers. The journald log is preserved for sysadmins.
- **In-VM `~/.ssh/known_hosts` seeded before SSH `git clone` (#85):** PR #58 (v0.4.0) removed the `git_ssh` credential mount that previously supplied both keys and host trust. The shed-extensions ssh-agent forwarding replaces *keys*, but the agent protocol carries no host trust — so a clone of `git@github.com:...` failed immediately with `Host key verification failed` before key auth even ran. The server now writes `~/.ssh/known_hosts` into the VM via the agent (`umask 077`, owned `shed:shed`) before invoking `git clone`, but only when the URL is SSH-form (`git@host:path` or `ssh://...`) — HTTPS / git:// / http:// URLs skip this step. Built-in defaults bake GitHub's published host keys (ed25519, ecdsa, rsa) into the server binary so the common case works with no operator config. No image rebuild needed; `~/.ssh` already exists in v0.4.x rootfs from the existing Dockerfile `mkdir`.
- **New `git.extra_known_hosts` server config:** Optional list of `known_hosts`-format lines that operators can paste in to trust additional SSH hosts (GitLab, GitHub Enterprise, self-hosted Gitea, etc.). Always *additive* on top of the built-in GitHub defaults — OpenSSH treats multiple lines for the same host as any-match-wins, so there's no `disable_default_hosts` flag. Validation runs at server startup: each entry must have at least three whitespace-separated fields and a recognized SSH key type (`ssh-rsa`, `ssh-ed25519`, `ssh-dss`, `ecdsa-sha2-nistp256/384/521`); malformed entries fail server startup with a clear error pointing at the bad index. Generate entries by running `ssh-keyscan <host>` on a trusted machine and pasting the output. Documented in `docs/reference/configuration.md` (new `## Git` section); commented example in `configs/server.example.yaml`. If GitHub or another host rotates keys, operators can extend trust via config without waiting for a release.
- **URL credentials no longer leak into SSE stream or server log:** Defense-in-depth follow-up applied during PR review. The SSE warning emits a fixed-string message (`Failed to clone repository (see server logs for details)`) — never `req.Repo` and never the wrapped `err`, since either could carry credentials from URLs like `https://user:pw@host/repo.git` (or from git/ssh stderr if a future refactor surfaces it into err). The server-side `log.Printf` line still logs the URL — operators need "which repo failed" to debug — but routes it through the new `config.SanitizeRepoURL` helper which strips just the password component from the URL's userinfo while preserving the username, scheme, host, and path. SSH-form URLs (`git@host:path`) and shorthand pass through unchanged.
- **Tests:** `TestCreateShed_SSE_SurfacesProgressAndWarning` (asserts `warning: true` propagates from backend through SSE handler, and that the warning message contains no URL fragments — `git@`, `://`); `TestGitConfigValidate` (table-driven, 11 cases covering valid ed25519/ecdsa/rsa, empty/whitespace/single-field/two-field/unknown-type rejections); `TestBuildKnownHosts_*` (nil cfg, nil git, additivity, exact-match dedupe, CRLF trimming); `TestIsSSHRepoURL` and `TestSanitizeRepoURL` (table-driven URL classification and password stripping). End-to-end validated on local Firecracker — HTTPS clone path emitted the new success event, SSH clone path wrote a 0600 shed:shed-owned `known_hosts` containing all three GitHub defaults plus a configured `gitlab.com` extra, and a malformed config was rejected at server startup. No Dockerfile / rootfs / agent-protocol changes — server-only release; v0.4.x rootfs images work unchanged.

## v0.4.3

- **Snapshot orphan reclamation:** `shed system prune --orphans` now reclaims partial snapshot directories left behind when a host crashes between `rootfs.ext4` and the atomic `snapshot.json` rename. Pre-fix these dirs were invisible to `shed snapshot list` and unreachable to prune; operators had to `rm -rf` manually. Race-safety is via a `.creating` marker dropped (durably, with `syncDir`) at the start of `CreateSnapshot` and removed via `defer` on every exit path. Stale markers (>24h) are treated as crash residue and the dir is reclaimed. Stat errors are fail-closed: a permission/transient I/O failure on either `snapshot.json` or `.creating` reports a `SkippedItem` rather than enqueuing the dir for deletion. Adds the `snapshot_orphan` kind to `FileEntry` / `PrunedItem`.
- **`shed system df` totals:** `SnapshotDiskEntry` now carries an `OtherFiles` slice (mirroring `ShedDiskEntry`), and both backends stat `snapshot.json` alongside the rootfs and add its bytes into the per-snapshot `Total`. CLI counts replace the hardcoded `len(snapshots) * 2` with a sum over `rootfs + OtherFiles`. The "snapshots and sheds spawned from them share extents via reflink" note is reworded so it no longer implies metadata bytes are shared.
- **`--from-snapshot` validation:** Mutual exclusion against `--image` / `--repo` is now wrapped in a new `ErrInvalidShedRequestSentinel` and routed through `mapBackendError` to a uniform 400 INVALID_REQUEST. The API handler keeps `ValidateSnapshotName` and the mutex check pre-SSE (so the SSE path doesn't surface a 200 + streamed error). Adds backend unit tests verifying the wrapped sentinel is returned.
- **`internal/lockmap` (new internal package):** `NamedMutexMap` consolidates the four duplicated per-name mutex maps (`createMu`/`createLocks` + `snapshotMu`/`snapshotLocks` × 2 backends). Zero-value-safe so existing tests that use `Client{}` continue to work without changes. The field rename `createLocks` → `shedLocks` matches the broader semantics already documented (Create/Start/Stop/Delete/snapshot-source — not just Create). Wrapper methods on `Client` are kept so callsites and lock-order docstrings stay put.
- **`TestHandleConnectSuccess` flake fix:** `handleConnect` now wraps the hijacked `clientConn` with `vmutil.BufferedConn` (mirroring the existing pattern in `internal/tunnels/connect.go`) so any bytes the HTTP server pre-buffered past the request headers aren't stranded. Eliminates the "read from VM side: unexpected EOF" failure that intermittently tripped CI on unrelated PRs.
- **Tests:** New `internal/firecracker/system_prune_test.go` (FC had no prune coverage before); new `TestPrune_SnapshotOrphans` table-driven harnesses in both backends; `internal/lockmap/lockmap_test.go` covers serialization + zero-value usage. No image/Dockerfile/rootfs changes — server-only release.

## v0.4.2

- **Snapshots — machine-id regeneration:** Every shed (fresh-create AND snapshot-spawn) now gets a unique `/etc/machine-id` at every VM boot. Pre-fix, `dbus`'s postinst baked a single UUID into the rootfs at Docker build time, so all sheds inherited it; spawning multiple sheds from one snapshot collided on identity. Fixed in both rootfs Dockerfiles via systemd's "transient machine-id" pattern: `/etc/machine-id` is a symlink to `/run/machine-id` (tmpfs), `/var/lib/dbus/machine-id` symlinks to `/etc/machine-id`, and `systemd-machine-id-commit.service` is masked so systemd doesn't replace the symlink with a regular file. PID 1 generates a fresh UUID per boot; nothing persists to disk. **Behavior change:** machine-id regenerates on every boot of the same shed, not just first boot. For applications that key persistent state on machine-id and expect it stable across reboots, recreate the shed instead of stop+starting it. Documented in `docs/reference/snapshots.md`.
- **Snapshots — SSH host key comment:** `shed-firstboot` now sets `/etc/hostname` BEFORE running `ssh-keygen -A`, so cloned sheds' SSH host keys carry the spawn's hostname (e.g. `root@my-spawn`) in the comment field rather than the source's.
- **Snapshots — internal cleanup:** `shed-firstboot` no longer touches machine-id (the previous `truncate + systemd-machine-id-setup` flow was broken — it pulled the source's value back from `/var/lib/dbus/machine-id`). With the rootfs symlink in place, machine-id is handled cleanly by systemd alone.

## v0.4.1

- **Snapshots:** Drop `ConditionVirtualization=vm` from `shed-firstboot.service`. In v0.4.0 the unit was loaded and enabled but never ran on snapshot-spawned sheds because `systemd-detect-virt` returns `docker` inside the Docker-built rootfs (container-y artifacts in `/` confuse detection), and the condition blocked the boot. The shed-firstboot binary already short-circuits when `/proc/cmdline` has no `shed.name=`, so the systemd-side gate was redundant. After this fix, snapshot-spawned sheds get fresh SSH host keys and the correct hostname on first boot. (The machine-id PID 1 caching caveat from `docs/reference/snapshots.md` still applies — fix is queued for a follow-up.)

## v0.4.0

- **Snapshots (new feature):** `shed snapshot create|list|info|delete` plus `shed create --from-snapshot <name>` on both backends (#81). A snapshot captures a stopped shed's rootfs as a named, immutable artifact (mode `0o444`) under a separate `snapshots_dir`; new sheds spawn from it via reflink (APFS clonefile / FICLONE) with their own writable rootfs. Snapshots survive deletion of the source shed and show up in `shed system df` with a reflink double-count note. Mutually exclusive with `--image` and `--repo`; `--local-dir` and credential mounts compose. Snapshot create of a `--local-dir`-backed shed surfaces a warning that workspace contents are not captured. See [`docs/reference/snapshots.md`](docs/reference/snapshots.md).
- **Snapshots — in-guest identity regeneration:** New `shed-firstboot` oneshot service (sysinit.target, before D-Bus / journald / sshd / shed-agent) regenerates `/etc/machine-id`, SSH host keys, and hostname when a cloned rootfs is detected (recorded shed name in `/var/lib/shed/identity.json` mismatches the boot-time `shed.name=` cmdline arg). Backends now append `shed.name=<name>` to kernel cmdline. Idempotent across normal restarts.
- **Snapshots — lifecycle lock broadening (internal behavior change):** `acquireCreateLock` is now a per-shed-name lifecycle lock taken by `Create`, `Start`, `Stop`, `Delete`, and `CreateSnapshot` of a shed-as-source. This closes TOCTOU races between snapshot-of-stopped-shed and concurrent Start/Delete of the same source. New separate `acquireSnapshotLock` keyspace serializes `CreateSnapshot` / `DeleteSnapshot` / `CreateShed --from-snapshot` for the same snapshot name. Lock-order rule: `snapshotLock -> createLock` (no AB-BA cycle). `DeleteShed` of a running shed uses an internal `stopShedLocked` helper to avoid re-entering the non-reentrant mutex.
- **`shed system df` (new):** New `shed system df` and `shed system df --verbose --all` for per-server disk usage reporting (#80). Categories: images (kernel/initrd/cached variants), sheds (rootfs + console logs + sidecars), snapshots, orphans. Returns both logical (apparent) and physical (block) bytes; `Notes` flag APFS clonefile and reflink double-counting.
- **`shed system prune` (new):** Scoped cleanup pass with `--scope images|instances|logs|orphans`, `--until <duration>`, `--dry-run`, and per-server `--all`. Age-based instance prune uses `mtime(metadata.json)` as the "last touched" proxy.
- **Reflink rootfs copies:** `CopyRootfs` now uses `clonefile(2)` on darwin/APFS and `FICLONE` (with `copy_file_range` and `io.Copy` fallbacks) on linux. `shed create` is near-instant and near-zero physical cost on supported filesystems; falls back transparently otherwise.
- **`CopyRootfs` writable-by-default:** All clone strategies now produce a `0o644` instance rootfs regardless of the source's mode. Required for spawn-from-snapshot (snapshot rootfs is `0o444` immutable) and a defensive no-op everywhere else.
- **Image cache:** `LinkCachedImage` auto-cleans stale `.tmp` orphans before retrying (#78); fixes a class of "ENOSPC after a previous interrupted conversion" failures.
- **Docs:** New [`docs/reference/snapshots.md`](docs/reference/snapshots.md) and [`docs/reference/disk-management.md`](docs/reference/disk-management.md). Snapshot subcommands added to the CLI reference.

## v0.3.5

- **Image cache:** Fix `shed-server pull-images` skipping `_base-rootfs.ext4` when `base_rootfs` shared a Docker ref with an `images:` entry. `pull-images` now hardlinks `_base` to the matching variant (zero extra disk) so the first subsequent `shed create` (no `--image`) is immediate instead of triggering an unexpected ~60s lazy pull. Verified on Firecracker (ext4) and VZ (APFS).
- **Image cache:** Make `shed image prune` source-aware for every Docker-ref entry (variants and `_base`). After a config ref bump (e.g. v0.3.3 → v0.3.4) stale `.ext4` caches whose `.source` sidecar no longer matches the current config ref are now reclaimed; previously they survived prune and had to be `sudo rm`'d manually.
- **Image cache:** Fix local-path exclusion in prune so a configured path like `images.prod: /var/lib/shed/firecracker/images/base-rootfs.ext4` protects the actual on-disk file, not just the map key. Prune derives the protected name from the path when it lives in `images_dir`.
- **Image cache:** `LinkCachedImage` helper uses atomic `os.Link`-to-temp + `os.Rename` so in-flight `CopyRootfs` readers keep valid open FDs on the old inode, and cleans up partially-written state if the sidecar write fails.
- **Docs:** New "`base_rootfs` vs `images:`" and "On-Disk Layout" sections in `docs/reference/images.md`, with per-backend (Firecracker/VZ) layout tables and an upgrade-and-reclaim cookbook. Cross-linked from `docs/getting-started/fc-setup.md`, `docs/getting-started/vz-setup.md`, and the `shed image prune` / `shed-server pull-images` entries in `docs/reference/cli.md`.
- **Config templates:** Bumped `configs/server.localmac.yaml` and `configs/server.localfc.yaml` image refs from `:v0.3.1` to `:v0.3.4`.

## v0.3.4

- **Firecracker:** Fix PS1 prompt showing time instead of shed name (Dockerfile heredoc with single-quoted PS1, matching VZ)
- **Firecracker:** Fix credential mounts (`~/.codex`, `~/.claude`, `~/.config/gh`) appearing as `root:root` inside the VM — `resolvePathOwner` now falls back to UID 1000 (shed user) for root-owned host dirs instead of triggering p9 passthrough
- **Firecracker:** Implement missing 9P ops (`UnlinkAt`, `SetAttr` chmod, `SetAttr` timestamps) on `remappingFile`; p9 library's localfs returned ENOSYS for these
- **Firecracker (security):** Skip `chmod`/`chtimes` on symlinks in 9P `SetAttr` — `os.Chmod` and `os.Chtimes` follow symlinks, which could let a guest modify files outside the shared directory by crafting a symlink (matches the existing `os.Lchown` bypass for ownership changes)
- **Firecracker / VZ:** Add `bubblewrap` to both Dockerfiles (required by Codex CLI sandbox); remove obsolete `// +build` tags
- **Tests:** Stabilize `TestHandleConnectSuccess` CI flake — replace `net.Pipe` mock with real TCP socketpair so `BidirectionalCopy`'s `CloseWrite`-based EOF propagation works correctly, add I/O deadlines, exercise both copy directions (#74, #75)

## v0.3.3

- **Breaking (deb only):** Rename deb package `shed` → `shed-server` and the release artifact to `shed-server_<version>_<arch>.deb` to avoid silent collisions with Ubuntu's `shed` hex editor. Add `Conflicts: shed` so the two packages can never coexist. Hosts on the old v0.3.2 deb need to `sudo apt purge shed && sudo dpkg -i shed-server_0.3.3_*.deb` (the old binary at `/usr/local/bin/shed-server` may have been silently removed by an `apt upgrade` — `readlink /proc/$(pidof shed-server)/exe` showing `(deleted)` confirms this)
- Document the deb as the primary Linux install path (#71)
- Align `configs/server.local.yaml` (localfc) with localmac defaults (extensions, mounts, images)

## v0.3.2

- Add `.deb` package support via GoReleaser nfpm for Linux (Ubuntu/Pop!OS) deployment
- Add `shed-server setup` command for automated Firecracker infrastructure provisioning (Linux-only)
- Add `shed-server pull-images` command to pre-cache VM images from Docker refs (cross-platform)
- Add Homebrew tap automation via GoReleaser
- Add VZ entitlement codesigning in Homebrew post_install
- Improve Homebrew config with platform-specific defaults and extensions guidance
- Update docs for Homebrew install workflow

## v0.3.1

- Add Docker credential helper to experimental images (docker-credential-shed, guest Docker config)
- Enable `docker-credentials` namespace in local dev server config
- Bump shed-extensions to v0.3.1
- Add DialService, Connect API, and vsock TCP proxy (#62)
- Run initial extension health check immediately (#61)

## v0.2.0

- Replace `typescript` image variant with `experimental` (default + shed-extensions credential brokering)
- Publish `shed-vz-experimental` and `shed-fc-experimental` images to ghcr.io
- Add `--shed-ext-version` flag to build scripts for local development
- Add SFTP support and `environment.d` loading in shed-agent
- Consolidate health checks onto message bus with heartbeats
- Upgrade GitHub Actions to Node.js 24 compatible versions
- Fix hardcoded image versions in docs, reorient Quick Start

## v0.1.2

- Fix kernel extraction failing on Firecracker images due to `set -euo pipefail` aborting on glob mismatch before reaching the `/boot/vmlinux` fallback path

## v0.1.1

- Include custom Firecracker kernel in published images — users no longer need to compile a kernel or run `build-firecracker-kernel.sh` when using published images
- Add `GetNeedsInitrd()` to ImageConfig interface to make initrd extraction optional (VZ only)
- Update `extractKernel()` to handle both VZ-style compressed and FC-style uncompressed kernels
- Default Firecracker `kernel_path` to `{images_dir}/vmlinux` (auto-populated from published images)
- Defer Firecracker `kernel_path` validation when Docker refs are configured
- Extract `hasAnyDockerRef()` and `dockerRunScript()` shared helpers
- Update Firecracker setup docs and example configs for kernel-in-image workflow

## v0.1.0

### Features

- Add VZ backend for macOS Apple Silicon using vfkit/Virtualization.framework
- Add `--local-dir` flag for mounting host directories as workspace (VZ with VirtioFS, Firecracker with 9P)
- Add image extensibility with Docker ref support and multiple image variants
- Add image delete and prune commands for managing cached rootfs images
- Add Firecracker image management parity with VZ backend
- Add plugin message bus for extensible VM-host communication
- Add CI workflow to publish VZ and Firecracker base images to ghcr.io on release tags
- Add SSE progress streaming for shed create
- Add enriched shed metadata and tiered CLI verbosity

### Firecracker Backend

- Add 9P filesystem and UID remapping for local-dir mounts
- Add exclude patterns to credential mounts
- Add credential change notifications over persistent vsock channel

### VZ Backend

- Switch to VirtioFS for credential mounts
- Add Docker CE networking in guest VMs
- Add multiple image variants with multi-stage Dockerfile (base, default, typescript)
- Fix DNS resolution and credential transfer

### Fixes

- Fix credential exclude glob matching for dir/* patterns
- Fix SSH config incompatibility causing git clone failures
- Fix console hang after shell exit by closing PTY master promptly
- Fix race condition in Firecracker dialer and exec stdin framing
- Fix shed exec PATH and improve backend error propagation
- Fix VM provisioning failing on first run due to state file check
- Fix credential sync tar failure on ephemeral files

### Documentation

- Unify image reference documentation for both VZ and Firecracker backends
- Restructure Firecracker docs into getting-started and reference sections
- Add comprehensive shed lifecycle documentation across all backends
- Add provisioning, tunnels, and file sync reference pages

### Infrastructure

- Upgrade golangci-lint to v2.10.1 managed via mise
- Add Dockerfile linting to CI

## v0.0.1

Initial release of shed, an SSH-based development environment manager.

### Features

- Docker container backend with bind mounts and Docker exec
- Firecracker microVM backend with vsock communication, TAP networking, and rootfs overlays
- SSH server (port 2222) and HTTP API (port 8080)
- Provisioning and file sync for containers and VMs
- SSH tunnel management for port forwarding
- Tmux session management for persistent CLI sessions
- Credential mounts for CLI tools (Git, SSH, AWS, etc.)
- Bidirectional credential sync for Firecracker VMs
- Graceful shutdown hooks for Firecracker VMs
- JSON output flag for machine-readable CLI output
- Repo URL validation with shorthand expansion (e.g., `owner/repo`)
- Configurable timeouts for create and start operations
- MkDocs-based documentation

### Firecracker Backend

- Run VM commands as non-root shed user
- Kernel build scripts and Firecracker v1.14.1 support
- Config validation with upper-bound constraints
- Version metadata for VM instances

### Infrastructure

- CI with linting and tests
- Release pipeline with GoReleaser and GitHub Actions
