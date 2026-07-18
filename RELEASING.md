# Releasing shed

The general release framework is `cc-plugins:release-workflows`; this
file documents what's specific to this repo (which is a lot — shed is
the most complex consumer).

## TL;DR

    /release-workflows:release v0.5.9

Everything else is automatic.

## Component selection (the manifest-selected release model)

The monorepo carries **four** release components on ONE `vX.Y.Z` tag
family. A component ships in a release **iff its version manifest
equals the tag**:

| Component | Ship selector (version manifest) | Artifacts |
|---|---|---|
| `server` | `.claude-plugin/plugin.json` `.version` (file unchanged; the component was renamed from `go`) | brew `shed`, apt `shed-server` deb, ghcr rootfs images (vz/fc + build-tools) |
| `host-agent` | `crates/shed-host-agent/VERSION` | brew `shed-host-agent` + a GH release linux tarball. **brew-only — no apt deb.** |
| `machine-rc` | `cmd/shed-machine-rc/VERSION` | brew `shed-machine-rc` + apt `shed-machine-rc` deb |
| `desktop` | `desktop/VERSION` (with `crates/Cargo.toml`, the Tauri `Cargo.toml`/`tauri.conf.json`, and both Cargo locks in verified lockstep) | ShedDesktop DMG + Sparkle appcast, `shed-desktop` debs — during the Swift→Tauri transition, **stable** tags ship the Swift DMG and **prerelease** (`-`) tags ship the Tauri DMG on the appcast beta channel (see [`desktop/RELEASING.md`](desktop/RELEASING.md)) |

`server`, `host-agent`, and `machine-rc` are the three **goreleaser**
components — each published by its own split config
(`.goreleaser.server.yaml`, `.goreleaser.host-agent.yaml`,
`.goreleaser.machine-rc.yaml`; see "What happens" below). Only
`desktop` has a beta channel — a prerelease (`-suffix`) tag that would
ship any of the three goreleaser components is rejected by
`release-plan.sh` (stable-only guard).

**The two new `VERSION` files are ship-selectors ONLY** — same
convention as `.claude-plugin/plugin.json` for `server`. The *shipped*
`shed-host-agent`/`shed-machine-rc` binary version is always the tag
ldflag GoReleaser injects at build time, never read from these files.
`crates/shed-host-agent/VERSION` is deliberately **independent of**
`crates/Cargo.toml`'s `[workspace.package].version` (owned by the
`desktop` component, which shares the Rust workspace) — the two will
diverge in normal operation and that's expected, not a bug.

### Recommend + confirm

Cutting a release is a two-step flow, not a single command:

1. **`scripts/release/recommend-components.sh X.Y.Z`** — inspects
   committed history and prints a recommendation (component | last-shipped
   tag | changed? | sample paths) plus a derived bump level:
   - **minor/major** bump (target's major or minor exceeds every
     manifest's) → recommends **all four** components.
   - **patch** bump → recommends only the components that **changed**
     since each component's own last-shipped tag (walked independently
     — server, host-agent, machine-rc, and desktop each advance on
     their own tag history).
   - Requires a full clone (hard-errors with an `--unshallow` hint on a
     shallow one) and a target strictly greater than the max version
     across all four manifests (the tag family is monotonic — versions
     are never reused).
   - The recommendation is a **starting point**: the path sets are
     deliberately coarse (over-recommending is free — under-recommending
     is the costly failure mode), and the script prints a caveat for the
     one known gap (a `[workspace.dependencies]`-only bump doesn't
     auto-flag `host-agent`). The human confirms or edits the set.
2. The human authors the CHANGELOG entry's `**Ships:**` line with the
   confirmed set, then runs `scripts/release/update-version.sh X.Y.Z
   --components <confirmed set>` (`go` is a deprecated alias for
   `server`).
3. **Mandatory before pushing the tag:** run
   `scripts/release/release-plan.sh vX.Y.Z` locally. This is a pre-tag
   mirror of every guard CI will run — including the `**Ships:**`
   cross-check (see below) — so a mismatch is caught before the tag
   exists, not after. CI is the backstop, not the first line of
   defense: a post-tag failure costs a **fresh version** under the
   never-retag rule (see "Break-glass recovery" below), so catching it
   locally is not optional.

### `**Ships:**` enforcement

Each stable-tag CHANGELOG entry's `**Ships:**` line uses the canonical
tokens `server`, `host-agent`, `machine-rc`, `desktop` (comma-separated;
legacy `server/CLI` is accepted as an alias for `server`, for entries
written before the rename). `release-plan.sh` locates the tag's
`## vX.Y.Z — date` section, parses the line, and **hard-fails the whole
workflow** if it disagrees with the manifest-computed ship set, contains
an unknown/duplicate token, or is missing — this is enforced on every
stable tag, not just a documentation convention. Prerelease tags have no
CHANGELOG entry and skip the check entirely.

- **CI-side selection**: `publish-images.yaml`'s first job runs
  `scripts/release/release-plan.sh`, which maps the tag to
  `ship_server`/`ship_host_agent`/`ship_machine_rc`/`ship_desktop`
  outputs, plus a derived `ship_goreleaser` (true iff any of the three
  goreleaser components ship — computed in the script, not re-inferred
  in YAML, so it's unit-tested). Every downstream job gates on those.
- **No-manifest-matches guard**: if a tag matches NONE of the four
  manifests (a forgotten `update-version.sh` run), `release-plan.sh`
  exits 1 and the whole workflow fails loudly — a silent no-op release
  is impossible. Fix by bumping the right manifest(s) and cutting a
  fresh tag.
- **Interleaved component versions**: component versions advance
  independently — each component's "current version" is **the most
  recent tag that shipped it**, not the most recent tag overall. The
  server may sit at a newer tag than host-agent, machine-rc, or desktop
  (or any permutation), legitimately. Never reuse a version for a
  different component set. Corollary: a **helper-only** tag (host-agent
  and/or machine-rc, no server) publishes **no**
  `ghcr.io/charliek/shed-{vz,fc}-*` rootfs images and leaves the other
  components' brew/apt entries pinned at their prior release — same
  rule the desktop-only case has always followed.
- **The apt reality**: apt-charliek indexes a package by scanning the
  latest GitHub release **assets** that match its glob — the
  `repository_dispatch` payload is a freshness nudge only, ignored for
  correctness. That's *why* an unshipped component must produce **no
  release assets at all**: the per-component split goreleaser configs
  (below) are the mechanism that keeps an unshipped component's deb
  entirely off the release, so apt's scan can't accidentally pick it up.
- Both bump/plan scripts and the recommender are covered by
  `scripts/release/release-scripts-test.sh` (run in CI by `ci.yml`'s
  `plugin` job).

The desktop leg's recurring specifics (secrets, DMG/notarize, Sparkle
appcast, debs, apt dispatch, rc-tag rehearsals) live in
[`desktop/RELEASING.md`](desktop/RELEASING.md).

## What happens

1. **`release-workflows:release`** (LLM, local):
   - Verifies branch (`main`) + clean tree + CI green on HEAD. ci.yml
     has a `ci-success` aggregator (skipped-by-path-filter counts as
     pass) — the skill's ci-success check is a real gate now.
   - Asks/confirms version
   - Runs `scripts/release/recommend-components.sh X.Y.Z`, presents the
     recommendation, and gets the human's confirmed/edited component
     set (see "Recommend + confirm" above).
   - Drafts a CHANGELOG entry from `git log v<previous>..HEAD` with a
     `**Ships:**` line naming the confirmed set, commits as
     `docs(changelog): vX.Y.Z entry`
   - Runs `scripts/release/update-version.sh X.Y.Z --components
     <confirmed set>`:
     - `server` delegates to `scripts/set-version.sh` (the existing
       in-repo bumper) to update `.claude-plugin/plugin.json`'s
       top-level `.version`, then jq-verifies the bump landed (defense
       against set-version.sh's silent-failure mode on malformed JSON)
     - `host-agent`/`machine-rc` write + grep-verify their standalone
       `VERSION` files
     - `desktop` bumps the lockstep set (unchanged)
   - Commits as `chore(version): bump to X.Y.Z`
   - Tags `vX.Y.Z` (annotated) on the version commit
   - **Runs `scripts/release/release-plan.sh vX.Y.Z` locally and
     confirms it passes** before pushing — this is the mandatory
     pre-tag mirror of the CI guards (see "Recommend + confirm" above).
   - `git push --follow-tags` (admin bypasses the ruleset)

2. **`publish-images.yaml`** (CI, on tag push `v*`) — a `release-plan`
   job first (component selection, see above), then the image chain
   below (gated on `ship_server`) plus the goreleaser `release` job
   (gated on `ship_goreleaser`, i.e. any of server/host-agent/machine-rc)
   plus the desktop jobs (gated on `ship_desktop`; documented in
   `desktop/RELEASING.md`). The image chain keeps its strict dependency
   ordering:

   **`publish-build-tools`** (`ubuntu-latest`, multi-arch, `if:
   ship_server`):
   - Builds + pushes `ghcr.io/charliek/shed-build-tools:vX.Y.Z` (and
     `:latest`) for `linux/amd64` + `linux/arm64`. The downstream
     image-publish jobs FROM/docker-run this image during their builds,
     so it must exist on ghcr.io before they start.

   **`publish-vz`** (`ubuntu-24.04-arm`, needs build-tools; transitively
   gated on `ship_server`):
   - Builds shed-overlay initramfs + 3 OCI images
     (`shed-vz-{base,extensions,full}` arm64), pushes to ghcr.io.

   **`publish-fc`** (`ubuntu-latest`, needs build-tools; transitively
   gated on `ship_server`):
   - Same as `publish-vz` but for `linux/amd64` Firecracker images.
     Runs in parallel with `publish-vz`.

   **`smoke`** (reusable `smoke-linux.yml`, `if: ship_server`):
   - Install-only smoke against the tagged commit on GHA's
     ubuntu-latest. KVM-required cycle is the maintainer's
     responsibility (see `scripts/smoke-test-linux.sh --from-local`
     for the matching local script).

   **`release`** (`macos-latest`; needs `release-plan` +
   `publish-build-tools`/`publish-vz`/`publish-fc`/`smoke`; tag-push
   only; `if: ship_goreleaser`, and — when `ship_server` — additionally
   requires all four image/smoke jobs to have `== 'success'`):
   - Runs `go test ./...` + golangci-lint (unconditional — every
     goreleaser-shipping tag exercises the Go test suite, not just
     server tags).
   - Mints a release-bot App token scoped to `charliek/homebrew-tap`
     once, shared by every invocation below.
   - Runs **one `goreleaser release --clean -f <config>` invocation per
     shipping goreleaser component**, in fixed order **server →
     host-agent → machine-rc**, each step individually gated on its own
     `ship_<component> == 'true'`:
     - `.goreleaser.server.yaml` — 3 Go binaries × OS/arch matrix with
       ldflag versioning, `checksums-server.txt`, `Formula/shed.rb`
       (pushed to homebrew-tap), `shed-server_*.deb` (via nfpm).
     - `.goreleaser.host-agent.yaml` — the Rust `shed-host-agent`
       binary (`builder: rust`, via `cargo zigbuild`), a GH linux
       tarball, `checksums-host-agent.txt`, `Formula/shed-host-agent.rb`
       — **no `.deb`**, brew-only. Also builds (but doesn't archive) the
       two Go rollback ids — see "Host-agent" below.
     - `.goreleaser.machine-rc.yaml` — the `shed-machine-rc` binary,
       `checksums-machine-rc.txt`, `Formula/shed-machine-rc.rb`,
       `shed-machine-rc_*.deb`.
     - All three share `release.mode: keep-existing` +
       `release.replace_existing_artifacts: true`, identical
       `changelog`/`prerelease: auto`/`project_name: shed`. **The FIRST
       shipping invocation creates the GitHub Release and writes the
       changelog body**; later invocations in the same run only add
       their own assets — they never touch the body. This makes re-runs
       safe: `replace_existing_artifacts` lets a re-run replace its own
       already-uploaded assets instead of failing on `already_exists`.
     - A single-component tag's release notes still describe the
       **entire** `last-tag..tag` commit range (the goreleaser changelog
       is repo-level, not per-component-path-filtered) — don't read a
       short notes body as "nothing else changed."
     - Before-hooks in every split config are constrained to **not
       mutate tracked files** — CI asserts `go mod tidy` leaves
       `go.mod`/`go.sum` clean and that `packaging/server.yaml.generated`
       (written by the server config's sed before-hook) stays gitignored.
       A tracked-file mutation between invocation N and N+1 would fail
       goreleaser's dirty-check on the second invocation.
   - Mints a release-bot App token scoped to `charliek/apt-charliek`.
   - Dispatches `event_type=publish` to apt-charliek for each shipping
     apt-carrying component — `client_payload[package]=shed-server`
     (`if: ship_server`) and `client_payload[package]=shed-machine-rc`
     (`if: ship_machine_rc`); host-agent has no apt dispatch (brew-only).
     Retry loop with stderr capture for diagnosability. These dispatches
     are freshness nudges only — see "The apt reality" above; the split
     configs (no assets built for an unshipped component) are what
     actually keeps it out of apt's scan.

   `workflow_dispatch` (manual republish) re-runs only the
   image-publish chain; the `release` job is gated on `tag-push`
   events so manual republishes don't accidentally cut a new GitHub
   Release. A `workflow_dispatch` republish of a tag from **before**
   this migration re-runs that tag's old two-output
   `release-plan.sh` (checked out at the tag) — the `release-plan` job
   has a permanent legacy-dispatch shim that detects the missing
   `ship_server=` line and maps that old script's single combined-Go
   output onto
   `ship_server`/`ship_host_agent`/`ship_machine_rc`/`ship_goreleaser`
   together (the old `go` selector shipped all three as one unit), so
   old-tag republishes keep working unchanged.

   The `sync-version` job that existed pre-migration is **removed** —
   plugin.json is now bumped locally by the release skill before
   tagging, matching the convention.

The maintainer runs step 1; everything else is automated.

## Version files this repo owns

`scripts/release/update-version.sh` bumps:

- `--components server` (default; `go` accepted as a deprecated alias
  that warns on stderr): `.claude-plugin/plugin.json` `.version` (via
  `scripts/set-version.sh`, with a jq-verify safety net)
- `--components host-agent`: `crates/shed-host-agent/VERSION` (write +
  grep-verify; a standalone ship-selector, see the cross-selector note
  below)
- `--components machine-rc`: `cmd/shed-machine-rc/VERSION` (write +
  grep-verify; same standalone-selector pattern as host-agent)
- `--components desktop`: `desktop/VERSION`, `crates/Cargo.toml`
  (`[workspace.package].version`) + `crates/Cargo.lock` regen,
  `desktop/tauri/src-tauri/Cargo.toml` + `tauri.conf.json` +
  `desktop/tauri/src-tauri/Cargo.lock` regen (with a path-dep
  lock-entry verify for `shed-core`/`shed-app`)

Components combine: `--components server,host-agent,machine-rc,desktop`
bumps all four in one call. `server`/`host-agent`/`machine-rc` reject a
prerelease (`-suffix`) version — those three are stable-only; a
desktop-only prerelease is allowed (the Tauri rc-rehearsal path).

The Go binaries' version comes from a build-time `-X` ldflag injected
by GoReleaser via each split config's `builds[].ldflags`
(`.goreleaser.server.yaml`, `.goreleaser.machine-rc.yaml`, and the Go
rollback ids in `.goreleaser.host-agent.yaml`). The Docker images are
tagged from `GITHUB_REF_NAME` at workflow time. None of those need a
source-tree bump.

> **Cross-selector subtlety — the Rust `shed-host-agent`.** Its source
> version (`crates/Cargo.toml [workspace.package].version`) tracks the
> **desktop** selector (both live in the shared Rust workspace), but the
> binary **ships on its own `host-agent` selector**
> (`crates/shed-host-agent/VERSION`, see "Host-agent: Rust binary, Go
> rollback" below) — the two are intentionally independent and will
> normally show different values. The shipped `shed-host-agent version`
> is **not** `CARGO_PKG_VERSION` either way; GoReleaser's `builder: rust`
> build in `.goreleaser.host-agent.yaml` sets
> `SHED_HOST_AGENT_VERSION={{ .Version }}` (the tag), which
> `crates/shed-host-agent/src/version.rs` reads via `option_env!`. So no
> source bump is needed for the host-agent's shipped version — only its
> standalone `VERSION` ship-selector.

`CHANGELOG.md` is maintained by the release skill for human-readable
in-repo history. GoReleaser's auto-generated release notes (filtered:
skip `docs:`, `test:`, `chore:`, `ci:`) go on the GitHub Release body.

`pyproject.toml` is for mkdocs only and has its own version cadence;
not touched by `update-version.sh`.

## Host-agent: Rust binary, Go rollback

`host-agent` ships on its **own** selector
(`crates/shed-host-agent/VERSION`, see "Component selection" above) —
it no longer rides the `server`/`go` selector. It's still built from
the **Rust** `crates/shed-host-agent` (not the Go `cmd/shed-host-agent`),
with the version injected from the tag, via GoReleaser's OSS
`builder: rust` in `.goreleaser.host-agent.yaml` (which runs `cargo
zigbuild`; the `release` + `release-snapshot` jobs install `zig` +
`cargo-zigbuild`, and the `release` job only pays that install cost when
`ship_host_agent` is true). `builder: prebuilt` is GoReleaser
**Pro-only** and was not an option.

- **Install identity is unchanged.** Same binary/formula/archive name, same
  `brew services` `service` block (run args, PATH env, `keep_alive`, log
  paths), same bundled `extensions.example.yaml` → `etc/shed/extensions.yaml`,
  same `shed-host-agent status`/`version` surface. The swap only changed where
  the binary is compiled from. No apt/`.deb` (the host-agent is brew-only).
- **The Go build is kept as rollback insurance.** The Go ids
  `shed-host-agent-{darwin,linux}` now live in
  `.goreleaser.host-agent.yaml` (GoReleaser compiles every `builds[]`
  entry regardless of archive references, so they are still built +
  CI-exercised every snapshot; `release-snapshot` asserts them in
  `dist/host-agent/artifacts.json`) but are **detached from the
  archive**. To revert to the Go binary, flip
  `archives[shed-host-agent].ids` back to the two Go ids in
  `.goreleaser.host-agent.yaml` — a one-line change that already
  compiles green.
- **Retirement trigger (a SEPARATE future task, not yet done).** Once the Rust
  `shed-host-agent` has shipped clean for ~2 releases, delete `cmd/shed-host-agent`,
  the two Go goreleaser ids, and the `hostagent`-filter's `cmd/shed-host-agent/**`
  entry; the Go-vs-Rust differential harness (`tests/host-agent-diff/`) retires
  with the Go daemon. Do this deliberately in its own PR — do NOT bundle it with
  a feature change.

## Snapshot / dev versioning

Not used in the formal sense. `shed version` between releases shows
the last released version (the binary's compiled-in ldflag).

The `make dev-server-up*` targets DO inject a custom dev
`SHED_BUILD_TOOLS_REF` env var so a parallel dev shed-server can run
against the in-progress build-tools image without colliding with the
brew/.deb production server. See `make help` for details — not a
release-time concern.

## Secrets

| Secret | Purpose | Required? |
|---|---|---|
| `RELEASE_BOT_CLIENT_ID` | `charliek-release-bot` GitHub App client ID (used by `actions/create-github-app-token`) | required |
| `RELEASE_BOT_APP_KEY` | App private key (.pem) | required |
| `GITHUB_TOKEN` (workflow-provided) | All 3 Docker push jobs (build-tools, VZ, FC) to ghcr.io as `${{ github.actor }}` | provided automatically; do not set |
| Desktop secrets (six `CAN_NOTARIZE` + `SPARKLE_ED_PRIVATE_KEY`) | Desktop release leg — see [`desktop/RELEASING.md`](desktop/RELEASING.md) | required before the first desktop-shipping tag |

Retired (deleted from `gh secret list -R charliek/shed` during the
convention adoption):

- `HOMEBREW_TAP_TOKEN` — replaced by the App-minted homebrew-tap
  token. GoReleaser still reads the env var named
  `HOMEBREW_TAP_TOKEN`; the workflow sets it from
  `steps.tap.outputs.token` instead of from `secrets`.
- `APT_DISPATCH_TOKEN` — replaced by the App-minted apt-charliek
  token used for the `repository_dispatch` call.

Confirm with `gh secret list -R charliek/shed` that the
`RELEASE_BOT_CLIENT_ID`/`RELEASE_BOT_APP_KEY` pair (plus the desktop
secrets, once added) are present and the retired tokens are gone.

## Branch protection

`main` is protected by a ruleset (created during the convention
adoption) with rules `deletion` + `non_fast_forward`. ci.yml now has a
`ci-success` aggregator (skipped-by-filter counts as pass) — the single
job suited to a `required_status_checks` rule; adding it is the
recommended follow-up. Bypass actors:

- `charliek-release-bot` (App, type `Integration`) — **load-bearing**:
  the desktop release leg's appcast step commits `docs/appcast.xml`
  back to `main` as the bot (see `publish-images.yaml` `desktop-macos`)
- Admin role (id `5`, type `RepositoryRole`) — lets
  `/release-workflows:release`'s push of the changelog + version
  commits + tag land

Inspect or edit at https://github.com/charliek/shed/rules.

## App installation

The release-bot App must be installed on three repos:

- `charliek/shed` itself (so the workflow's `RELEASE_BOT_APP_*` secrets
  resolve)
- `charliek/homebrew-tap` (so the minted token can push
  `Formula/shed.rb`)
- `charliek/apt-charliek` (so the minted token can dispatch
  `event_type=publish`)

The three Docker push jobs do NOT need the App on a separate repo —
ghcr.io packages owned by this same repo accept pushes from the
workflow's `GITHUB_TOKEN` as `${{ github.actor }}`. Canonical ghcr.io
pattern. Intentionally unchanged.

Verify all three via the `sanity-check-app.yml` workflow (Actions →
Run workflow). Each block must print the expected repo name.

## When things break

| Symptom | Cause | Fix |
|---|---|---|
| `git push` rejected from local | Pusher not in ruleset bypass | Confirm App + admin role in `main`'s ruleset `bypass_actors` |
| `release-plan` job fails with "tag matches NO component manifest" (or a Ships cross-check mismatch) | A manifest wasn't bumped before tagging (forgotten `update-version.sh --components ...`), the CHANGELOG `**Ships:**` line disagrees with the manifests, or the tag was created against the wrong commit | Don't force-fix in CI — re-run `/release-workflows:release` against a corrected tag. This is exactly what the mandatory local `scripts/release/release-plan.sh` pre-tag run (see "Recommend + confirm") is meant to catch before the tag ever gets pushed |
| GoReleaser fails at `brews` with `Bad credentials` | `RELEASE_BOT_CLIENT_ID` unset OR App not installed on `homebrew-tap` | Verify via `sanity-check-app.yml`'s homebrew-tap block; install the App on the tap |
| apt-charliek dispatch fails after 3 retries | App not installed on `charliek/apt-charliek` OR scope mismatch | Verify via `sanity-check-app.yml`'s apt-charliek block; check `RELEASE_BOT_APP_KEY` is the current `.pem` |
| `publish-vz` / `publish-fc` fails on `FROM ghcr.io/charliek/shed-build-tools:vX.Y.Z` (image not found) | The build-tools push hadn't propagated yet, OR a previous `workflow_dispatch` failed to publish the new tag | Re-run the failed image-publish job; it'll pick up the now-existent build-tools image |
| Docker push fails at "Log in to ghcr.io" | `packages: write` permission missing on the job | Confirm `permissions.packages: write` at workflow + job level |
| `brew install charliek/tap/shed` finds old version | Tap cache | `brew untap charliek/tap && brew tap charliek/tap` |
| `apt update; apt install shed-server` finds old version | apt-charliek didn't run, OR it ran but didn't pick up the new .deb | Check apt-charliek Actions tab for the `repository_dispatch` run triggered by this release |
| `.claude-plugin/plugin.json` on main is one version behind the tag | The pre-migration `sync-version` job would have fixed this; the new flow doesn't have it. Means the maintainer tagged without `/release-workflows:release` | Re-run `/release-workflows:release` to bump locally and re-tag |

## Break-glass recovery

### GoReleaser failed after some artifacts uploaded

```bash
RUN_ID=$(gh run list -R charliek/shed --workflow publish-images.yaml \
                     --limit 1 --json databaseId --jq '.[0].databaseId')
gh run rerun "${RUN_ID}" -R charliek/shed --failed
```

Each split config runs `release.mode: keep-existing` +
`release.replace_existing_artifacts: true` — a re-run is safe. The
GitHub Release itself isn't recreated (`keep-existing` finds it already
exists and leaves the body alone — it was written once, by whichever
invocation ran first in the original attempt); each re-invocation just
re-uploads **its own** component's assets, replacing any it had already
uploaded before the failure rather than erroring on `already_exists`.
An invocation that never got to run in the failed attempt runs fresh, as
normal.

**Mixed-checksums note**: a `workflow_dispatch` republish of a tag from
**before** this migration (the monolith-era, single `checksums.txt`)
will leave that legacy `checksums.txt` sitting alongside any
newer per-component `checksums-<component>.txt` files if you re-run it
today — the legacy-dispatch shim (see "What happens" above) only maps
`ship_*` outputs, it doesn't touch already-published assets. For any
tag migrated to (or cut under) the split-config model, the
per-component `checksums-<component>.txt` files are authoritative;
ignore a stray `checksums.txt` from a pre-migration tag.

### Image-publish job failed (build-tools / VZ / FC)

Re-run the failed job from the Actions UI. The downstream `release`
job will start once the failed publish job + smoke pass on the rerun.

### Wrong version got tagged (sync-version was the safety net, now it's gone)

Every manifest that shipped on the released tag must be at that
version on main, even without the sync-version job:

```bash
# Manually fix the drifted manifest(s) on a hotfix branch:
git checkout -b fix/version-resync
scripts/release/update-version.sh X.Y.Z --components server,host-agent,machine-rc,desktop
git commit -am "chore(version): resync manifests to vX.Y.Z"
# Open a PR; merge through normal flow.
```

Then cut a fresh patch tag (`vX.Y.Z+1`) so the release pipeline picks
up the corrected manifest(s). Don't try to retag `vX.Y.Z` — fight that
URGE; force-updating tags strands published images and creates a
confusing state.

### Revert path — the split pipeline is fundamentally broken

If a defect in the split-config model itself (not a one-off CI flake)
is blocking every release, `git revert` the plan-002 merge commit on
`main`. That restores the single pre-migration monolith goreleaser
config (`release.mode: replace`) and the two-component `go`/`desktop`
selector wholesale. Cut a **fresh** version under the legacy component
behavior — never retag the tag that exposed the defect. This is the
same never-retag discipline as "Wrong version got tagged" above, just
at pipeline scope instead of manifest scope.

## Adopting the convention (for new contributors)

If you're new to this repo and need to understand the release
pipeline, read [`cc-plugins/plugins/release-workflows/references/convention.md`](https://github.com/charliek/cc-plugins/blob/main/plugins/release-workflows/references/convention.md)
in the framework repo.

## Notes for this repo

- **All Docker pushes (build-tools, VZ, FC) use `GITHUB_TOKEN`, NOT the
  release-bot App**: ghcr.io packages owned by this same repo accept
  pushes from the workflow's `GITHUB_TOKEN` as `${{ github.actor }}`.
  Canonical ghcr.io pattern. The App's value is single-identity for
  *cross-repo* pushes; these are same-repo. Intentionally unchanged.
- **`release-plan` job owns the at-tag-time manifest check**: the
  post-migration replacement for the deleted `sync-version` job (and,
  since plan 002, for the old inline `release` job step too — the
  check now lives entirely in `scripts/release/release-plan.sh`, run
  once up front for all four manifests plus the `**Ships:**`
  cross-check). The pre-migration flow could tolerate a "tag was
  pushed before the manifest was bumped" state because sync-version
  would fix it after the fact. The current flow REJECTS that state at
  release time — the maintainer reruns the skill against a corrected
  tag rather than shipping mismatched bytes. The mandatory local
  `release-plan.sh` run before pushing the tag (see "Recommend +
  confirm") exists so this rejection is caught pre-tag, not post-tag.
- **GoReleaser pin**: `goreleaser-action@v7` is pinned to `version:
  v2.15.2` everywhere it's invoked (the `release` job here, and ci.yml's
  `release-snapshot` job) — the split-config model was spiked and
  proven against this exact version. `brews:` (used by all three split
  configs) is **hard-deprecated in goreleaser 2.16**; the pin
  deliberately defers that migration. A `brews:` → `homebrew_casks:`
  rewrite is named future work, not scoped here.
- **No `ci-gate` job in the release pipeline**: the inline `go test` +
  golangci-lint steps in the `release` job serve as the at-tag-time
  gate. The smoke job is a separate gate on `release`'s `needs:`. CI
  on PRs runs `ci.yml` (test/lint/plugin) but is informational —
  there's no aggregator that the release skill blocks on.
- **`publish-images.yaml`'s strict job ordering** is the v0.5.2-era
  race fix: the apt-charliek dispatch must happen after all
  referenced ghcr images exist, because the binary hard-fails on
  missing `io.shed.rootfs.erofs.digest` if it tries to pull an image
  the apt deb references but ghcr doesn't have yet. The dependency
  chain `build-tools → vz+fc → smoke → release` enforces that.
- **`scripts/set-version.sh` is preserved**: the convention's
  `scripts/release/update-version.sh` delegates to it (with a
  jq-verify wrap), so the in-repo bumper isn't duplicated.
- **`workflow_dispatch` republishes are still useful**: if an image
  build flakes after a release, you can re-run
  `publish-images.yaml` manually with the version input and it'll
  republish the images without creating a new Release (the `release`
  job is gated on `tag-push` events).
