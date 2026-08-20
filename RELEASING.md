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
| `sx` | `crates/sx/VERSION` | brew `sx` + apt `sx` deb (the channel pair the retired `machine-rc` vacated). See "sx: Rust binary" below for the **glibc floor** that comes with the linux deb. |
| `desktop` | `desktop/VERSION` (with `crates/Cargo.toml`, the Tauri `Cargo.toml`/`tauri.conf.json`, and both Cargo locks in verified lockstep) | ShedDesktop DMG + Sparkle appcast, `shed-desktop` debs — during the Swift→Tauri transition, **stable** tags ship the Swift DMG and **prerelease** (`-`) tags ship the Tauri DMG on the appcast beta channel (see [`desktop/RELEASING.md`](desktop/RELEASING.md)) |

`server`, `host-agent` and `sx` are the three **goreleaser** components
— each published by its own split config (`.goreleaser.server.yaml`,
`.goreleaser.host-agent.yaml`, `.goreleaser.sx.yaml`; see "What happens"
below). Only `desktop` has a beta channel — a prerelease (`-suffix`) tag
that would ship a goreleaser component is rejected by `release-plan.sh`
(stable-only guard).

> **The retired `machine-rc` component (plan 010).** `shed-machine-rc`
> shipped as a fourth component through v0.8.2; the shed-host-agent
> daemon now hosts the machine RC hub and `sx` carries the one-shot
> verbs, so the component is retired: `cmd/shed-machine-rc`,
> `.goreleaser.machine-rc.yaml`, and its selector are deleted, and
> `update-version.sh`/`release-plan.sh` reject the `machine-rc` token.
> **Published artifacts are not withdrawn, but they are not supported.**
> Nothing new is built for them and they get no fixes; the intended
> move is `sx` + `shed-host-agent` (see
> [`docs/extensions/shed-machine-rc.md`](docs/extensions/shed-machine-rc.md)).
> Its brew+apt channel pair is now `sx`'s (plan 011). The tap/apt
> housekeeping landed with that block: `Formula/shed-machine-rc.rb` was
> dropped from `charliek/homebrew-tap`, and the `shed-machine-rc` entry
> was dropped from `charliek/apt-charliek`'s `packages.yaml` (that entry
> still pointed at the pre-monorepo `charliek/shed-extensions` repo — it
> was never repointed, so the v0.8.x machine-rc debs were never indexed
> by apt in the first place).
> **Old-tag guard:** a PRE-retirement tag's own `release-plan.sh` can
> emit `ship_machine_rc=true`; the current `publish-images.yaml` has no
> machine-rc jobs. On a tag **push** it **fails loudly** instead of
> silently ignoring it — build machine-rc from the tag's own workflow,
> or accept the gap. On a `workflow_dispatch` image republish it only
> **warns** and continues: the `release` job is tag-push gated, so a
> dispatch never builds goreleaser artifacts anyway and the documented
> image-recovery path for pre-retirement tags keeps working.

**The `host-agent` and `sx` `VERSION` files are ship-selectors ONLY** —
same convention as `.claude-plugin/plugin.json` for `server`. The
*shipped* binary version is always the tag value GoReleaser injects at
build time (`SHED_HOST_AGENT_VERSION` / `SX_VERSION`), never read from
these files. Both are deliberately **independent of**
`crates/Cargo.toml`'s `[workspace.package].version` (owned by the
`desktop` component, which shares the Rust workspace) — they will
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
     — server, host-agent, sx, and desktop each advance on their own tag
     history).
   - Requires a full clone (hard-errors with an `--unshallow` hint on a
     shallow one) and a target strictly greater than the max version
     across all four manifests (the tag family is monotonic — versions
     are never reused).
   - **First-ship bootstrap**: a component that has never shipped has no
     last-shipped tag to diff against, so the walk would hard-error "no
     historical basis". The script carries an explicit `NEVER_SHIPPED`
     list (today: `sx`) whose members instead report the basis
     `(never shipped)`, count as changed, and are recommended at every
     level. Anything NOT on that list keeps the hard error — for an
     established component, no basis means the tag history is broken.
     **Prune an entry from `NEVER_SHIPPED` once it has shipped once**;
     the walk finds its own tag from then on.
   - The recommendation is a **starting point**: the path sets are
     deliberately coarse (over-recommending is free — under-recommending
     is the costly failure mode), and the script prints a caveat for the
     one known gap (a `[workspace.dependencies]`-only bump doesn't
     auto-flag `host-agent`). The human confirms or edits the set.
2. The human authors the CHANGELOG entry's `**Ships:**` line with the
   confirmed set, then runs `scripts/release/update-version.sh X.Y.Z
   --components <confirmed set>` (`go` is a deprecated alias for
   `server`).
3. **Mandatory before tagging:** run
   `scripts/release/release-plan.sh vX.Y.Z` locally, before `git tag`.
   This is a pre-tag mirror of every guard CI will run — including the
   `**Ships:**` cross-check (see below) — so a mismatch is caught before
   the tag exists, not after (a failure that fired after tagging would
   strand a stale local tag). CI is the backstop, not the first line of
   defense: a post-tag failure costs a **fresh version** under the
   never-retag rule (see "Break-glass recovery" below), so catching it
   locally is not optional.

### `**Ships:**` enforcement

Each stable-tag CHANGELOG entry's `**Ships:**` line uses the canonical
tokens `server`, `host-agent`, `sx`, `desktop` (comma-separated; `machine-rc`
is rejected with a retirement message on entries written after plan 010;
legacy `server/CLI` is accepted as an alias for `server`, for entries
written before the rename). `release-plan.sh` locates the tag's
`## vX.Y.Z — date` section, parses the line, and **hard-fails the whole
workflow** if it disagrees with the manifest-computed ship set, contains
an unknown/duplicate token, or is missing — this is enforced on every
stable tag, not just a documentation convention. Prerelease tags have no
CHANGELOG entry and skip the check entirely.

- **CI-side selection**: `publish-images.yaml`'s first job runs
  `scripts/release/release-plan.sh`, which maps the tag to
  `ship_server`/`ship_host_agent`/`ship_sx`/`ship_desktop` outputs, plus a derived
  `ship_goreleaser` (true iff any goreleaser component ships — computed
  in the script, not re-inferred in YAML, so it's unit-tested). Every downstream job gates on those.
- **No-manifest-matches guard**: if a tag matches NONE of the four
  manifests (a forgotten `update-version.sh` run), `release-plan.sh`
  exits 1 and the whole workflow fails loudly — a silent no-op release
  is impossible. Fix by bumping the right manifest(s) and cutting a
  fresh tag.
- **Interleaved component versions**: component versions advance
  independently — each component's "current version" is **the most
  recent tag that shipped it**, not the most recent tag overall. The
  server may sit at a newer tag than host-agent, sx or desktop (or any
  permutation), legitimately. Never reuse a version for a different
  component set. Corollary: a **helper-only** tag (host-agent and/or
  `sx`, no server) publishes **no** `ghcr.io/charliek/shed-{vz,fc}-*`
  rootfs images and leaves the other
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
     - `host-agent` writes + grep-verifies its standalone `VERSION`
       file
     - `desktop` bumps the lockstep set (unchanged)
   - Commits as `chore(version): bump to X.Y.Z`
   - **Runs `scripts/release/release-plan.sh vX.Y.Z` locally and
     confirms it passes** — this is the mandatory pre-tag mirror of the
     CI guards (see "Recommend + confirm" above). It runs **before**
     `git tag` on purpose: a guard failure must not leave a stale local
     tag to unwind.
   - Tags `vX.Y.Z` (annotated) on the version commit
   - `git push --follow-tags` (admin bypasses the ruleset)

2. **`publish-images.yaml`** (CI, on tag push `v*`) — a `release-plan`
   job first (component selection, see above), then the image chain
   below (gated on `ship_server`) plus the goreleaser `release` job
   (gated on `ship_goreleaser`, i.e. server and/or host-agent)
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
     host-agent**, each step individually gated on its own
     `ship_<component> == 'true'`:
     - `.goreleaser.server.yaml` — 3 Go binaries × OS/arch matrix with
       ldflag versioning, `checksums-server.txt`, `Formula/shed.rb`
       (pushed to homebrew-tap), `shed-server_*.deb` (via nfpm).
     - `.goreleaser.host-agent.yaml` — the Rust `shed-host-agent`
       binary (`builder: rust`, via `cargo zigbuild`), a GH linux
       tarball, `checksums-host-agent.txt`, `Formula/shed-host-agent.rb`
       — **no `.deb`**, brew-only. See "Host-agent" below.
     - Both share `release.mode: keep-existing` +
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
   - Dispatches `event_type=publish` to apt-charliek for the one
     apt-carrying component — `client_payload[package]=shed-server`
     (`if: ship_server`); host-agent has no apt dispatch (brew-only).
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
   `ship_server`/`ship_host_agent`/`ship_goreleaser` together, so
   old-tag republishes keep working unchanged. (When a post-migration,
   pre-retirement tag's own script emits `ship_machine_rc=true`, the job
   fails loudly on a tag push and warns on a dispatch republish — see
   the retired-component note under "Component selection".)

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
- `--components sx`: `crates/sx/VERSION` (same shape as host-agent —
  write + grep-verify, a standalone ship-selector)
- `--components desktop`: `desktop/VERSION`, `crates/Cargo.toml`
  (`[workspace.package].version`) + `crates/Cargo.lock` regen,
  `desktop/tauri/src-tauri/Cargo.toml` + `tauri.conf.json` +
  `desktop/tauri/src-tauri/Cargo.lock` regen (with a path-dep
  lock-entry verify for `shed-core`/`shed-app`)

Components combine: `--components server,host-agent,sx,desktop` bumps
all four in one call. `server`/`host-agent`/`sx` reject a prerelease
(`-suffix`) version — they are stable-only; a desktop-only prerelease is
allowed (the Tauri rc-rehearsal path).

The Go binaries' version comes from a build-time `-X` ldflag injected
by GoReleaser via the split config's `builds[].ldflags`
(`.goreleaser.server.yaml`). The Docker images are tagged from
`GITHUB_REF_NAME` at workflow time. None of
those need a source-tree bump.

> **Cross-selector subtlety — the Rust `shed-host-agent` and `sx`.** Their source
> version (`crates/Cargo.toml [workspace.package].version`) tracks the
> **desktop** selector (both live in the shared Rust workspace), but the
> binary **ships on its own `host-agent` selector**
> (`crates/shed-host-agent/VERSION`, see "Host-agent: Rust binary"
> below) — the two are intentionally independent and will
> normally show different values. The shipped `shed-host-agent version`
> is **not** `CARGO_PKG_VERSION` either way; GoReleaser's `builder: rust`
> build in `.goreleaser.host-agent.yaml` sets
> `SHED_HOST_AGENT_VERSION={{ .Version }}` (the tag), which
> `crates/shed-host-agent/src/version.rs` reads via `option_env!`. So no
> source bump is needed for the host-agent's shipped version — only its
> standalone `VERSION` ship-selector. **`sx` is the identical story**:
> `.goreleaser.sx.yaml` sets `SX_VERSION={{ .Version }}`,
> `crates/sx/src/version.rs` reads it via `option_env!` with the same
> `pick_version` fallback, and `crates/sx/VERSION` is its selector.

`CHANGELOG.md` is maintained by the release skill for human-readable
in-repo history. GoReleaser's auto-generated release notes (filtered:
skip `docs:`, `test:`, `chore:`, `ci:`) go on the GitHub Release body.

`pyproject.toml` is for the docs site only and has its own version cadence;
not touched by `update-version.sh`.

## Host-agent: Rust binary

`host-agent` ships on its **own** selector
(`crates/shed-host-agent/VERSION`, see "Component selection" above) —
it no longer rides the `server`/`go` selector. It's built from the
**Rust** `crates/shed-host-agent`, with the version injected from the
tag, via GoReleaser's OSS `builder: rust` in
`.goreleaser.host-agent.yaml` (which runs `cargo zigbuild`; the
`release` + `release-snapshot` jobs install `zig` + `cargo-zigbuild`,
and the `release` job only pays that install cost when
`ship_host_agent` is true). `builder: prebuilt` is GoReleaser
**Pro-only** and was not an option.

- **Install identity is unchanged.** Same binary/formula/archive name, same
  `brew services` `service` block (run args, PATH env, `keep_alive`, log
  paths), same bundled `extensions.example.yaml` → `etc/shed/extensions.yaml`,
  same `shed-host-agent status`/`version` surface. The swap only changed where
  the binary is compiled from. No apt/`.deb` (the host-agent is brew-only).
- **The Go daemon and its rollback path are retired.** `cmd/shed-host-agent`,
  the Go goreleaser ids (`shed-host-agent-{darwin,linux}`), and the
  `hostagent`-filter's `cmd/shed-host-agent/**` CI entry were deleted in plan
  006 (mtls-cleanup / host-agent sunset). This is a deliberate, explicit
  waiver of the retirement window this section previously described (**"~2
  clean releases"**): only **v0.8.0** had shipped the Rust agent when
  retirement landed here — the maintainer accepted that early, judging the
  Rust port's differential coverage (see below) sufficient. Source-level
  rollback (flipping a goreleaser id back to a Go build) **ends with this
  change** — there is no Go source left in the tree to build.
- **Post-removal rollback procedure.** If the Rust `shed-host-agent` needs to
  be rolled back after this point, reinstall the last released formula/bottle
  that still carries the desired behavior — e.g. `brew install
  shed-host-agent` pinned at `v0.8.0` (or the prior tag) via the formula's
  versioned bottle/tap history. There is no source-level revert; this is an
  install-time pin.
- **The differential harness became the golden-pinned wire harness.** Every
  value the Go-vs-Rust differential harness once asserted was recorded as a
  golden before the Go daemon was deleted, so `tests/host-agent-diff/` keeps
  its coverage — it now runs the Rust `shed-host-agent` daemon alone against
  those committed goldens instead of against a live Go process. See
  `tests/host-agent-diff/README.md` for the mechanics and why the directory
  and Makefile target still say "diff".

## sx: Rust binary

`sx` (plan 011) ships on its **own** selector `crates/sx/VERSION`, is
built from the **Rust** `crates/sx` with the version injected from the
tag, and publishes through `.goreleaser.sx.yaml` — the same
`builder: rust` / `cargo zigbuild` path the host-agent leg has used
since plan 006, so the `release` + `release-snapshot` jobs install
`zig` + `cargo-zigbuild` once and both components ride it. It takes the
**brew + apt** channel pair the retired `machine-rc` component vacated.

- **Packaging is deliberately minimal.** `Formula/sx.rb` is
  `bin.install "sx"` with no `service` block and no caveats; the deb is
  one binary in `/usr/local/bin` with **no** systemd unit, **no** config
  file and **no** maintainer scripts. `sx` is a plain CLI — contrast the
  `shed-server` deb, which carries all three.
- **The linux glibc floor is part of this component's support
  statement.** The `*-unknown-linux-gnu` targets are cross-built from
  macOS by `cargo zigbuild`, which links against a default glibc of
  **~2.30** — i.e. **Ubuntu 20.04+ / RHEL 9+**. For the host-agent that
  floor is a footnote (its linux tarball is a secondary download and
  brew/macOS is the wired install path); for `sx` the **linux deb is a
  primary install path**, so state it rather than discovering it on an
  older box. mini2/mini3 are Ubuntu 24.04 (glibc 2.39) — comfortably
  above it. The floor **cannot currently be pinned lower** on the pinned
  goreleaser v2.15.3 (its rust builder passes the glibc-suffixed triple
  straight to `rustup target add`, which rejects it — only `cargo
  zigbuild` itself strips the suffix). If it ever bites, the contained
  fix is a native-runner build matrix, which changes the BUILDER only —
  selectors, `**Ships:**` cross-check and dispatch wiring stay put.
- **Shipping `sx` is not a stability promise.** It remains a
  fast-moving dev tool whose surface may change without a deprecation
  cycle; it is packaged so the dev loop across this Mac and mini2/mini3
  does not need a Rust toolchain on every machine. `shed-ext-rc` remains
  the released, supported *guest* RC binary.
- **Local pre-CI proof**: `make snapshot-sx` runs the same goreleaser
  snapshot CI does, from the `.mise.toml`-pinned goreleaser/zig, once
  `cargo-zigbuild` and the three non-native rustup targets are installed
  (the target's comment lists both commands).

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

**Checksums note (no mixed state)**: a release's goreleaser assets are
never a *mix* of legacy and per-component checksums. The `release` job is
event-gated on `tag-push` (see "What happens" above and "Notes for this
repo" below), so a `workflow_dispatch` republish never runs goreleaser at
all — dispatch only re-runs the image chain and cannot touch goreleaser
assets. Concretely:

- **Pre-migration releases** (monolith-era) carry a single
  `checksums.txt` and **keep it forever** — nothing rewrites it.
- **Post-migration releases** (cut under, or migrated to, the split-
  config model) carry per-component `checksums-<component>.txt` files,
  which are authoritative.
- The **only** way an existing release's goreleaser assets change is
  re-running a **failed** `release` job on a post-migration tag (the
  Break-glass rerun above) — and that produces per-component files only.

So a stray `checksums.txt` means a pre-migration tag; per-component
`checksums-<component>.txt` means a post-migration tag; you will never
see both on one release as a result of a dispatch republish.

### Image-publish job failed (build-tools / VZ / FC)

Re-run the failed job from the Actions UI. The downstream `release`
job will start once the failed publish job + smoke pass on the rerun.

### Wrong version got tagged (sync-version was the safety net, now it's gone)

Every manifest that shipped on the released tag must be at that
version on main, even without the sync-version job:

```bash
# Manually fix the drifted manifest(s) on a hotfix branch — include
# ONLY the components that shipped on vX.Y.Z but whose manifests
# drifted (bumping all four would falsely mark non-shipping
# components as released at X.Y.Z):
git checkout -b fix/version-resync
scripts/release/update-version.sh X.Y.Z --components <drifted components>
git commit -am "chore(version): resync manifests to vX.Y.Z"
# Open a PR; merge through normal flow.
```

Then, when the next release is due, run the NORMAL release flow for
the concrete next version — it bumps the intended components'
manifests to that new version *before* tagging. (Don't resync-then-tag
`vX.Y.Z+1` directly: a tag whose version matches no manifest is
rejected by release-plan.sh.) Don't try to retag `vX.Y.Z` — fight that
URGE; force-updating tags strands published images and creates a
confusing state.

### Revert path — the split pipeline is fundamentally broken

If a defect in the split-config model itself (not a one-off CI flake)
is blocking every release:

1. **Revert the migration.** `git revert` the plan-002 merge commit on
   `main`. That restores the single pre-migration monolith goreleaser
   config (`release.mode: replace`) and the two-component `go`/`desktop`
   selector wholesale.
2. **Resync only the drifted manifests, if any.** If a manifest fell out
   of sync with the last released version, resync **only the drifted
   ones** back to that released version on a normal PR. Don't sweep every
   manifest, and don't tag off this PR — it's housekeeping, not a
   release.
3. **Release the next version through the normal flow.** Run
   `/release-workflows:release` for the concrete next version. Under the
   reverted legacy pipeline that's the old `--components go,desktop`
   behavior: the flow bumps the intended components' manifests to the
   **new** version *before* tagging, so the tag matches its manifests and
   the release proceeds. Do **not** resync manifests to the already-
   released version and then tag the next one — a fresh tag whose
   manifests still read the old version matches no manifest and
   `release-plan.sh` rejects it.

Never retag the tag that exposed the defect — same never-retag
discipline as "Wrong version got tagged" above, just at pipeline scope
instead of manifest scope.

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
  v2.15.3` everywhere it's invoked (the `release` job here, ci.yml's
  `release-snapshot` job, and `.mise.toml` for local `make snapshot-sx`
  runs) — the split-config model was spiked and
  proven against this line. v2.15.3 ships the secret-redaction fix that
  prevents secrets from leaking into logs, so it's the security floor;
  it's still **below** v2.16, where `brews:` (used by all three split
  configs) is **hard-deprecated**, so the pin keeps `brews:` functional
  while deliberately deferring that migration. A `brews:` →
  `homebrew_casks:` rewrite is named future work, not scoped here.
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
