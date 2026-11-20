# Releasing shed

The general release framework is `cc-plugins:release-workflows`; this
file documents what's specific to this repo (which is a lot — shed is
the most complex consumer).

## TL;DR

    /release-workflows:release v0.5.9

Everything else is automatic.

## Component selection (the manifest-selected release model)

The monorepo carries **two** release components on ONE `vX.Y.Z` tag
family. A component ships in a release **iff its version manifest
equals the tag**:

| Component | Ship selector (version manifest) | Artifacts |
|---|---|---|
| `server` | `.claude-plugin/plugin.json` `.version` (file unchanged; the component was renamed from `go`) | brew `shed`, apt `shed-server` deb, ghcr rootfs images (vz/fc + build-tools) |
| `desktop` | `desktop/VERSION` (with `crates/Cargo.toml`, the Tauri `Cargo.toml`/`tauri.conf.json`, and both Cargo locks in verified lockstep) | ShedDesktop DMG (Tauri, the macOS client as of 0.9.0) + Sparkle appcast, `shed-desktop` debs — a **prerelease** (`-`) tag ships on the appcast beta channel, a stable tag on the stable channel (see [`desktop/RELEASING.md`](desktop/RELEASING.md)) |

`server` is the one **goreleaser** component, published by
`.goreleaser.server.yaml` (see "What happens" below). Only `desktop` has
a beta channel — a prerelease (`-suffix`) tag that would ship the server
is rejected by `release-plan.sh` (stable-only guard).

### Retired components

Three components have left the release. Their published artifacts are
not withdrawn, but they are not supported: nothing new is built for
them and they get no fixes.

- **`machine-rc` (retired in plan 010).** `shed-machine-rc` shipped as a
  fourth component through v0.8.2 (brew `shed-machine-rc` and a deb);
  the shed-host-agent daemon took over the machine RC hub, and that hub
  was itself retired in plan 022 (see
  [`docs/extensions/shed-machine-rc.md`](docs/extensions/shed-machine-rc.md)).
  `cmd/shed-machine-rc`, `.goreleaser.machine-rc.yaml` and its selector
  are deleted. The tap/apt housekeeping landed with the retirement:
  `Formula/shed-machine-rc.rb` was dropped from `charliek/homebrew-tap`,
  and the `shed-machine-rc` entry was dropped from
  `charliek/apt-charliek`'s `packages.yaml` (that entry still pointed at
  the pre-monorepo `charliek/shed-extensions` repo — it was never
  repointed, so the v0.8.x machine-rc debs were never indexed by apt in
  the first place). Its brew+apt channel pair passed to `sx` in plan 011.
  **Refused** through the generic errors, with no message of its own:
  `update-version.sh --components machine-rc` exits 2 as an unknown
  component and a `**Ships:** machine-rc` token exits 1 as an unknown
  token.
  **Old-tag guard:** a post-migration, pre-retirement tag's own
  `release-plan.sh` can emit `ship_machine_rc=true`; the current
  `publish-images.yaml` has no machine-rc jobs. On a tag **push** it
  **fails loudly** instead of silently ignoring it — build machine-rc
  from the tag's own workflow, or accept the gap. On a
  `workflow_dispatch` image republish it only **warns** and continues:
  the `release` job is tag-push gated, so a dispatch never builds
  goreleaser artifacts anyway and the documented image-recovery path for
  pre-retirement tags keeps working.
- **`sx` (sunset in plan 016, #329).** The RC session porcelain was never
  tagged, so nothing of it was ever published. Its selector
  (`crates/sx/VERSION`), `.goreleaser.sx.yaml` and the brew + apt
  channel pair it inherited from machine-rc were removed outright; nobody
  holds that pair now. **Refused** through the generic errors:
  `--components sx` exits 2 and a `**Ships:** sx` token exits 1, as
  unknown.
- **`host-agent` (retired in plan 037).** `shed-host-agent`, the
  host-side credential broker daemon, shipped on its own selector
  (`crates/shed-host-agent/VERSION`) through v0.9.0, as brew
  `shed-host-agent` plus GitHub release tarballs (darwin and linux,
  amd64 and arm64), built from the Rust crate by
  `.goreleaser.host-agent.yaml` (`builder: rust`, cross-compiled with
  `cargo zigbuild`). It was never an apt package. The desktop app now
  brokers credentials in-process on macOS and Linux and is the only
  broker. The selector, the goreleaser config, the zig install script
  and the `release` job's Rust and host-agent steps are deleted. Its
  artifacts stay frozen at v0.9.0: the GitHub release assets of the tags
  that shipped it stay where they are, and the tap's `shed-host-agent`
  formula is deleted (a commit in `charliek/homebrew-tap`, not here; the
  upgrade guide tells users to uninstall it). **Refused** through the generic
  errors, with no message of its own: `--components host-agent` exits 2
  with `unknown component 'host-agent' (valid: server, desktop)`, and a
  `**Ships:** host-agent` token exits 1 as an unknown token. **No
  old-tag guard:** a tag push runs that tag's own workflow, so an old tag
  brings its own host-agent jobs; a `workflow_dispatch` of an old tag
  reads its `ship_host_agent=true` line into an output no job declares,
  which is ignored, and the `release` job never runs on a dispatch. Every
  tag that shipped host-agent also shipped the server, so its
  `ship_goreleaser` equals its `ship_server` and `desktop-release-create`
  is skipped exactly as before.

### Recommend + confirm

Cutting a release is a two-step flow, not a single command:

1. **`scripts/release/recommend-components.sh X.Y.Z`** — inspects
   committed history and prints a recommendation (component | last-shipped
   tag | changed? | sample paths) plus a derived bump level:
   - **minor/major** bump (target's major or minor exceeds every
     manifest's) → recommends **both** components.
   - **patch** bump → recommends only the components that **changed**
     since each component's own last-shipped tag (walked independently
     — server and desktop each advance on their own tag history).
   - Requires a full clone (hard-errors with an `--unshallow` hint on a
     shallow one) and a target strictly greater than the max version
     across both manifests (the tag family is monotonic — versions
     are never reused).
   - **First-ship bootstrap**: a component that has never shipped has no
     last-shipped tag to diff against, so the walk would hard-error "no
     historical basis". The script carries an explicit `NEVER_SHIPPED`
     list whose members instead report the basis
     `(never shipped)`, count as changed, and are recommended at every
     level. Anything NOT on that list keeps the hard error — for an
     established component, no basis means the tag history is broken.
     **Prune an entry from `NEVER_SHIPPED` once it has shipped once**;
     the walk finds its own tag from then on.
     **The list is EMPTY since plan 016** — its only entry, `sx`, was
     sunset before it ever shipped. The mechanism is kept, not deleted,
     because the next brand-new component needs it: add that component's
     name to `NEVER_SHIPPED=()` in `recommend-components.sh` alongside
     its `COMPONENTS` / `manifest_path()` / `get_paths()` entries, and
     prune it again after its first shipping tag. With the list empty,
     `release-scripts-test.sh` proves the bootstrap synthetically (a
     sed-patched scratch copy of the recommender listing `desktop`,
     run against the basis-less H3 fixture) so the mechanism cannot rot
     while unused.
   - The recommendation is a **starting point**: the path sets are
     deliberately coarse (over-recommending is free — under-recommending
     is the costly failure mode). The human confirms or edits the set.
2. The human authors the CHANGELOG entry's `**Ships:**` line with the
   confirmed set, then runs `scripts/release/update-version.sh X.Y.Z
   --components <confirmed set>` (`server`, `desktop`; any other name,
   the old `go` among them, exits 2 as an unknown component).
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
tokens `server`, `desktop` (comma-separated, with no alias; any other
token, a retired component's or the old `server/CLI` among them, is
refused as unknown). `release-plan.sh` locates the tag's
`## vX.Y.Z — date` section, parses the line, and **hard-fails the whole
workflow** if it disagrees with the manifest-computed ship set, contains
an unknown/duplicate token, or is missing — this is enforced on every
stable tag, not just a documentation convention. Prerelease tags have no
CHANGELOG entry and skip the check entirely.

- **CI-side selection**: `publish-images.yaml`'s first job runs
  `scripts/release/release-plan.sh`, which maps the tag to
  `ship_server`/`ship_desktop` outputs, plus a derived
  `ship_goreleaser` (true iff goreleaser runs, which today equals
  `ship_server` — computed in the script, not re-inferred in YAML, so
  it's unit-tested; `desktop-release-create` keys on it). Every
  downstream job gates on those.
- **No-manifest-matches guard**: if a tag matches NEITHER of the two
  manifests (a forgotten `update-version.sh` run), `release-plan.sh`
  exits 1 and the whole workflow fails loudly — a silent no-op release
  is impossible. Fix by bumping the right manifest(s) and cutting a
  fresh tag.
- **The desktop signing gate (fail closed)**: when `ship_desktop`, the
  `release-plan` job runs `scripts/release/desktop-signing-gate.sh` right
  after `release-plan.sh`. It takes six presence flags, built as
  `${{ secrets.X != '' }}` for the six Apple secrets `CAN_NOTARIZE` reads,
  and never a secret value. A **stable** desktop tag missing any of them
  fails there, naming the missing ones, **before anything publishes**: no
  images, no brew/apt, no GitHub release, no appcast entry. A prerelease
  passes with an ad-hoc note (the rc rehearsal may build unsigned).
  `desktop-macos` runs the same gate again right after its checkout,
  before any build. The local pre-tag `release-plan.sh` run can't see
  repository secrets, so confirm the six with
  `gh secret list -R charliek/shed` before tagging a stable desktop
  release. (One exception: when the tag's checkout has no gate script, a
  `workflow_dispatch` of a bare `v0.Y.Z` tag with Y at most 8 and no
  suffix (the tags that predate the gate; v0.8.0 and v0.8.1 ship desktop)
  skips it with a warning so its image republish still works;
  `desktop-macos` refuses those tags' mac republish anyway. Any other run
  without the script fails closed: a tag push, any suffixed or malformed
  tag, and a dispatch of 0.9.0 or later.)
- **Interleaved component versions**: component versions advance
  independently — each component's "current version" is **the most
  recent tag that shipped it**, not the most recent tag overall. The
  server may sit at a newer tag than the desktop (or the reverse),
  legitimately. Never reuse a version for a different
  component set. Corollary: a **desktop-only** tag publishes **no**
  `ghcr.io/charliek/shed-{vz,fc}-*` rootfs images and leaves the
  server's brew/apt entries pinned at their prior release.
- **The apt reality**: apt-charliek indexes a package by scanning the
  latest GitHub release **assets** that match its glob — the
  `repository_dispatch` payload is a freshness nudge only, ignored for
  correctness. That's *why* an unshipped component must produce **no
  release assets at all**: the server's goreleaser config runs only when
  the server ships, so an unshipped server's deb stays entirely off the
  release and apt's scan can't accidentally pick it up.
- Both bump/plan scripts and the recommender are covered by
  `scripts/release/release-scripts-test.sh` (run in CI by `ci.yml`'s
  `plugin` job).

The desktop leg's recurring specifics (secrets, DMG/notarize, Sparkle
appcast, debs, apt dispatch, rc-tag rehearsals) live in
[`desktop/RELEASING.md`](desktop/RELEASING.md).

### craze pin (plan 025)

`craze-pin.env` at the repo root holds two independent pins for
[craze](https://github.com/charliek/craze), the agent-lane provider shed's
desktop and phone clients drive:

- **`CRAZE_TEST_SHA`** — a MERGED craze `main` commit (never a branch head)
  that both this repo's CI and shed-mobile's CI build `craze`,
  `craze-fake-host` and `craze-fake-agent` from for their hermetic tests.
  `make craze-binaries` builds the same four binaries locally (plus
  `craze-0.0.1`, the too-old test cell, built at the real `v0.0.1` tag); the
  `.github/actions/craze-binaries` composite action does the equivalent in
  CI, shared by both repos so their craze SHAs can't drift apart.
- **`CRAZE_RELEASE`** — the craze release the shed images actually bake
  (bare `X.Y.Z`), or empty before a bake lands. First set to `0.1.0` by plan
  025 §3.9 / C13; `0.1.1` in plan 029 C1; `0.2.0` since.

`craze-pin.env` is never sourced as shell by anything that reads it — every
consumer (the Makefile targets, `check-craze-pin.sh`, the composite action,
`release-plan.sh`) goes through `scripts/release/read-craze-pin.sh`, the one
shared strict parser that treats the file as DATA: only `CRAZE_TEST_SHA=`
and `CRAZE_RELEASE=` lines are recognized, validated before anything uses
them, and any other line is refused outright. Sourcing it would let the file
itself run a command or flip the release-time check's self-test seam.

`make check-craze-pin` (offline, run by `make check` and CI's `pins` job)
keeps the two pins well-formed, requires
`crates/shed-craze/fixtures/wire.PIN` — the sha shed-craze's vendored wire
fixtures were copied from — to equal `CRAZE_TEST_SHA` (so the pin never moves
without the fixtures, or the other way round), and, once `CRAZE_RELEASE` is
set, keeps both Dockerfiles' `CRAZE_VERSION`/digest `ARG`s in lockstep with it
and with each other — the same fail-fast-on-drift shape as `check-roost-pin`.
Moving `CRAZE_TEST_SHA` means re-vendoring craze's
`internal/fakehost/testdata/wire` (its `README.md` included) into
`crates/shed-craze/fixtures/wire` and rewriting `wire.PIN` in the same commit;
CI's `craze-binaries` action diffs the two trees on every run.

**The release-time check.** `scripts/release/check-craze-pin.sh --release
v<CRAZE_RELEASE>` (network) takes **craze's own version**, e.g. `v0.2.0`,
**not** the shed tag; `release-plan.sh` passes `v$CRAZE_RELEASE`. It resolves
`v$CRAZE_RELEASE` against
`github.com/charliek/craze` via `git ls-remote` and requires the resolved
commit to equal `CRAZE_TEST_SHA` — O4's rule that "at the shed release the
test SHA must equal the baked release's tag commit, enforced by a check."
Passing the shed tag by mistake fails with "not found" plus a hint that the
argument is craze's version from `craze-pin.env`.
`release-plan.sh` calls it for every **stable tag whose component set
includes `server`** (the images ship with the server component), in both
the mandatory local pre-tag run and CI's `release-plan` job.

> **Consequence.** The craze bake landed in plan 025 §3.9 / C13 (craze
> 0.1.0), moved to 0.1.1 in plan 029 C1 and to 0.2.0 after it:
> `CRAZE_RELEASE=0.2.0` and `CRAZE_TEST_SHA` equals v0.2.0's tag commit, so
> this check passes against the real tag. Before the bake landed,
> `CRAZE_RELEASE` was empty and **every** stable tag that ships `server`
> from `main`, a hotfix included, was refused — that was the rule working
> as intended: 0.9.0 must bake craze. The same trap returns if `CRAZE_RELEASE`
> is ever cleared or a future craze release moves `CRAZE_TEST_SHA` without a
> matching bump here. For a true emergency, the owner may set
> `SHED_RELEASE_ALLOW_NO_CRAZE=<reason>`, which prints loudly and records
> the override in `release-plan.sh`'s output instead of being silently
> honoured — it must never be set by CI.

`scripts/release/release-scripts-test.sh` covers the release-time check
too, through a `CRAZE_PIN_LS_REMOTE` seam honoured **only** under
`SHED_RELEASE_SELFTEST=1` (so a stray variable can never bypass a real
release run) — match passes, mismatch fails, an empty `CRAZE_RELEASE`
fails, and the seam is proven ignored without the self-test flag.

## What happens

1. **`release-workflows:release`** (LLM, local):
   - Verifies branch (`main`) + clean tree + CI green on HEAD. ci.yml
     has a `ci-success` aggregator (skipped-by-path-filter counts as
     pass) — the skill's ci-success check is a real gate now.
   - Asks/confirms version
   - Runs `scripts/release/recommend-components.sh X.Y.Z`, presents the
     recommendation, and gets the human's confirmed/edited component
     set (see "Recommend + confirm" above).
   - Drafts a CHANGELOG entry from `git log v<previous>..HEAD` (or, when
     `## Unreleased` is curated, folds it instead — see "Folding a
     curated `## Unreleased`") with a
     `**Ships:**` line naming the confirmed set, commits as
     `docs(changelog): vX.Y.Z entry`
   - Runs `scripts/release/update-version.sh X.Y.Z --components
     <confirmed set>`:
     - `server` delegates to `scripts/set-version.sh` (the existing
       in-repo bumper) to update `.claude-plugin/plugin.json`'s
       top-level `.version`, then jq-verifies the bump landed (defense
       against set-version.sh's silent-failure mode on malformed JSON)
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
   (gated on `ship_server` and the whole server chain)
   plus the desktop jobs (gated on `ship_desktop`; documented in
   `desktop/RELEASING.md`). The image chain keeps its strict dependency
   ordering:

   **Server signing, before anything publishes** (plan 028, #304; tag
   pushes that ship `server`). On macOS, `shed-server` ships inside a
   Developer ID signed `ShedServer.app`, signed by the goreleaser build
   hook `scripts/release/macos-server-app.sh`. The release fails closed
   without that signature:
   - The `release-plan` job runs `scripts/release/server-signing-gate.sh`
     right after the desktop gate. A server tag without the four signing
     secrets (see "Secrets") fails there. A prerelease fails too: the
     server has no ad hoc release path.
   - **`server-signing-preflight`** (`macos-latest`, needs `release-plan`)
     imports the certificate into a throwaway keychain and signs a
     scratch binary the way the hook signs `shed-server`, `--timestamp`
     included. It then runs the hook's required checks (`--verify`:
     Developer ID authority, Team ID, secure timestamp, designated
     requirement, entitlements). A bad certificate, password, identity
     string or team, or an unreachable timestamp server, fails here.
     Every image job and `release` need it.
   - A `workflow_dispatch` republish signs nothing, so it skips both,
     and the image jobs accept the skipped preflight. On a tag push they
     run only after it succeeded.

   **`publish-build-tools`** (`ubuntu-latest`, multi-arch, `if:
   ship_server`, needs `server-signing-preflight`):
   - Builds + pushes `ghcr.io/charliek/shed-build-tools:vX.Y.Z` (and
     `:latest`) for `linux/amd64` + `linux/arm64`. The downstream
     image-publish jobs FROM/docker-run this image during their builds,
     so it must exist on ghcr.io before they start.

   **`publish-vz`** (`ubuntu-24.04-arm`, needs build-tools and the
   preflight; transitively gated on `ship_server`):
   - Builds shed-overlay initramfs + 3 OCI images
     (`shed-vz-{base,extensions,full}` arm64), pushes to ghcr.io.

   **`publish-fc`** (`ubuntu-latest`, needs build-tools and the
   preflight; transitively gated on `ship_server`):
   - Same as `publish-vz` but for `linux/amd64` Firecracker images.
     Runs in parallel with `publish-vz`.

   **`smoke`** (reusable `smoke-linux.yml`, `if: ship_server`):
   - Install-only smoke against the tagged commit on GHA's
     ubuntu-latest. KVM-required cycle is the maintainer's
     responsibility (see `scripts/smoke-test-linux.sh --from-local`
     for the matching local script).

   **`release`** (`macos-latest`; needs `release-plan` +
   `server-signing-preflight` +
   `publish-build-tools`/`publish-vz`/`publish-fc`/`smoke`; tag-push
   only; `if: ship_server`, and requires the preflight and all four
   image/smoke jobs to have `== 'success'`; `macos-latest` because the
   server build hook signs with `codesign`):
   - Runs `go test ./...` + golangci-lint.
   - Mints a release-bot App token scoped to `charliek/homebrew-tap`
     for the formula push.
   - Runs **one `goreleaser release --clean -f .goreleaser.server.yaml`
     invocation**: 3 Go binaries × OS/arch matrix with ldflag
     versioning, `checksums-server.txt`, `Formula/shed.rb` (pushed to
     homebrew-tap), `shed-server_*.deb` (via nfpm).
     - The step right before it imports the Developer ID certificate
       into a throwaway keychain, and the invocation runs with
       `SHED_SERVER_SIGNING=required`. The build hook therefore signs
       the darwin `shed-server` and `ShedServer.app` with that identity,
       and refuses anything but a timestamped Developer ID signature from
       the team. The step right after it deletes the keychain, whatever
       the outcome.
     - **No third-party build code runs while the certificate is
       imported.** The job installs no Rust toolchain and runs no
       `cargo`, `rustup` or `brew`. What runs before the delete is
       trusted at the release workflow's level: the actions it uses, and
       the repo's own code (tests, lint, release notes, the server build)
       with the `go.sum`-pinned modules the shipped binaries link.
       `release-scripts-test.sh` fails if any step running `cargo`,
       `rustup` or `brew` comes before the delete, or if a Rust
       toolchain step appears in the job at all.
     - The config uses `release.mode: keep-existing` +
       `release.replace_existing_artifacts: true`. **It creates the
       GitHub Release, with the tag's curated CHANGELOG section as its
       body**: the job extracts it with
       `scripts/release/changelog-section.sh` into a file outside the
       checkout and passes `--release-notes <file>`. The desktop jobs
       later only add their own assets — they never touch the body. This
       makes re-runs safe: `replace_existing_artifacts` lets a re-run
       replace its own already-uploaded assets instead of failing on
       `already_exists`.
     - The body is the CHANGELOG section, not goreleaser's commit list,
       so anything users must read on the release page (a one-time
       upgrade warning, say) belongs in the CHANGELOG entry.
     - The config's before-hooks are constrained to **not mutate
       tracked files** — CI asserts `go mod tidy` leaves
       `go.mod`/`go.sum` clean and that `packaging/server.yaml.generated`
       (written by the server config's sed before-hook) stays gitignored,
       since a tracked-file mutation would fail goreleaser's dirty-check.
   - Mints a release-bot App token scoped to `charliek/apt-charliek`,
     gated on `ship_server` — the only apt-carrying component.
   - Dispatches `event_type=publish` to apt-charliek with
     `client_payload[package]=shed-server` (`if: ship_server`). Retry
     loop with stderr capture for diagnosability. The dispatch is a
     freshness nudge only — see "The apt reality" above; building no
     assets for an unshipped component is what actually keeps it out of
     apt's scan.

   `workflow_dispatch` (manual republish) re-runs only the
   image-publish chain; the `release` job is gated on `tag-push`
   events so manual republishes don't accidentally cut a new GitHub
   Release. A `workflow_dispatch` republish of a tag from **before**
   this migration re-runs that tag's old two-output
   `release-plan.sh` (checked out at the tag) — the `release-plan` job
   has a permanent legacy-dispatch shim that detects the missing
   `ship_server=` line and maps that old script's single combined-Go
   output onto
   `ship_server`/`ship_goreleaser` together, so
   old-tag republishes keep working unchanged. (A tag's own script that
   emits a retired component's line is handled as "Retired components"
   above describes.)

   **One republish is deliberately NOT idempotent, and is refused.** The
   macOS client changed implementation at **0.9.0** (Swift → Tauri), so
   dispatching an older stable tag would build that tag's Tauri target,
   `--clobber` the Swift DMG its users already installed, and re-sign
   that version's appcast entry — same input, different application. The
   `desktop-macos` job's first step detects a pre-0.9.0 dispatch and
   **fails loudly** rather than shipping a different client into an
   existing release. Image republishes of those tags are unaffected
   (separate jobs); if you need one, re-run the image jobs alone.

   The `sync-version` job that existed pre-migration is **removed** —
   plugin.json is now bumped locally by the release skill before
   tagging, matching the convention.

The maintainer runs step 1; everything else is automated.

## Version files this repo owns

`scripts/release/update-version.sh` bumps:

- `--components server` (default): `.claude-plugin/plugin.json` `.version` (via
  `scripts/set-version.sh`, with a jq-verify safety net)
- `--components desktop`: `desktop/VERSION`, `crates/Cargo.toml`
  (`[workspace.package].version`) + `crates/Cargo.lock` regen,
  `desktop/tauri/src-tauri/Cargo.toml` + `tauri.conf.json` +
  `desktop/tauri/src-tauri/Cargo.lock` regen (with a path-dep
  lock-entry verify for `shed-core`/`shed-app`)

Components combine: `--components server,desktop` bumps both in one
call. `server` rejects a prerelease (`-suffix`) version — it is
stable-only; a desktop-only prerelease is allowed (the Tauri
rc-rehearsal path). Any other token, a retired component's or the old
name `go` among them, fails as an unknown component (exit 2, listing the
valid ones).

The Go binaries' version comes from a build-time `-X` ldflag injected
by GoReleaser via the config's `builds[].ldflags`
(`.goreleaser.server.yaml`). The Docker images are tagged from
`GITHUB_REF_NAME` at workflow time. None of
those need a source-tree bump.

`CHANGELOG.md` is maintained by the release skill for human-readable
in-repo history, and **the tag's section is the GitHub Release body** on
every stable tag. `scripts/release/changelog-section.sh vX.Y.Z` prints it
(the same extractor `release-plan.sh`'s Ships check reads, so the section
that passed the check is the one published). The section ends only at the
next `## Unreleased` or `## v<digit>` heading, so any other `## ` line (an
upgrade example's, say) stays in the body. **A curated section must not
contain a line starting `## Unreleased` or `## v<digit>`**: it would end the
release body there. The `release` job passes it
to the server's goreleaser invocation as `--release-notes`;
`desktop-release-create` passes it as `--notes-file` on a desktop-only
tag, and `desktop-macos` then appends its notarization line. GoReleaser's
generated changelog (`changelog: use: github`, filtered: skip `docs:`,
`test:`, `chore:`, `ci:`) no longer reaches the body.

`pyproject.toml` is for the docs site only and has its own version cadence;
not touched by `update-version.sh`.

### Folding a curated `## Unreleased`

When the `## Unreleased` section was curated as release notes while the work
landed (as 0.9.0's was), fold it instead of drafting a new entry:

1. Rename the heading to `## vX.Y.Z — YYYY-MM-DD` (an em dash, as
   `release-plan.sh` requires) and keep the `**Ships:**` line first under it.
   Add a fresh empty `## Unreleased` above it if you want one.
2. Do **not** let the release skill draft the entry from `git log
   v<previous>..HEAD` when the section is curated: that replaces the curated
   text with a commit list. Tell the skill to fold the existing section.
3. Check it before tagging: `scripts/release/changelog-section.sh vX.Y.Z`
   prints the section, and `scripts/release/release-plan.sh vX.Y.Z` passes.

The folded section **is the release page body**: the `release` job publishes
exactly what `changelog-section.sh` prints, so it must read as release notes
from its first line, with any one-time upgrade warning near the top. It must
not contain a line starting `## Unreleased` or `## v<digit>` (see above).

## Snapshot / dev versioning

Not used in the formal sense. `shed version` between releases shows
the last released version (the binary's compiled-in ldflag).

The `make dev-server-up*` targets DO inject a custom dev
`SHED_BUILD_TOOLS_REF` env var so a parallel dev shed-server can run
against the in-progress build-tools image without colliding with the
brew/.deb production server. See `make help` for details — not a
release-time concern.

## Server signing dry run

`.github/workflows/server-signing-dry-run.yml` builds the server
component as the release job does, Developer ID signed, and publishes
nothing. It runs a goreleaser snapshot (`--skip=publish`) with
`SHED_SERVER_SIGNING=required`, then re-runs the hook's required checks
on each extracted darwin `shed-homebrew` tarball (the bundle, its
executable and the standalone `shed-server`). It prints each signature
and designated requirement, and uploads the darwin tarballs as an
artifact for 3 days.

Run it after any change to the server's signing (the hook, the
goreleaser build or archive config, the entitlements, the certificate)
or to the brew formula, and before tagging a release that ships it:

```bash
gh workflow run server-signing-dry-run.yml -R charliek/shed --ref main
```

The job runs only on `main`. That is workflow logic, not a secret
boundary: anyone who can push a workflow and dispatch it can reach the
repository secrets, as with the tag-triggered jobs. Two runs a few
minutes apart give two builds with different CDHashes and the same
designated requirement, which is what a Homebrew upgrade test of the
"grant once" behavior needs.

## Secrets

| Secret | Purpose | Required? |
|---|---|---|
| `RELEASE_BOT_CLIENT_ID` | `charliek-release-bot` GitHub App client ID (used by `actions/create-github-app-token`) | required |
| `RELEASE_BOT_APP_KEY` | App private key (.pem) | required |
| `GITHUB_TOKEN` (workflow-provided) | All 3 Docker push jobs (build-tools, VZ, FC) to ghcr.io as `${{ github.actor }}` | provided automatically; do not set |
| Desktop secrets (six `CAN_NOTARIZE` + `SPARKLE_ED_PRIVATE_KEY`) | Desktop release leg — see [`desktop/RELEASING.md`](desktop/RELEASING.md) | required for every stable desktop tag: without the six, the desktop signing gate fails the `release-plan` job before anything publishes |
| Server signing: `MACOS_CERTIFICATE_P12_BASE64`, `MACOS_CERTIFICATE_PASSWORD`, `SHED_DESKTOP_DEVELOPER_ID_IDENTITY`, `APPLE_TEAM_ID` | Developer ID signing of `ShedServer.app` and the darwin `shed-server` (plan 028, #304). Four of the desktop's six: the same certificate, no notarization | required for every server tag: without them the server signing gate fails the `release-plan` job before anything publishes |

The server shares the desktop's Developer ID Application certificate.
macOS keys a user's external-drive grant on the bundle ID plus the
designated requirement, which names the identifier and the Team ID. A
renewed or rotated certificate from the same team therefore keeps
users' grants; a different team makes every user answer the prompt once
more. Confirm all four with `gh secret list -R charliek/shed` before
tagging a server release: the local `release-plan.sh` run can't see
repository secrets.

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
| `release-plan` job fails with "a stable desktop tag requires Developer ID signing and notarization; missing: …" | One or more of the six Apple secrets is unset (the desktop signing gate) | Add the named secrets (see [`desktop/RELEASING.md`](desktop/RELEASING.md), Secrets), then re-run the failed run: nothing has published, so the tag stands. An unsigned rehearsal needs a prerelease (`-rc.N`) tag instead |
| `release-plan` job fails with "a server tag requires a Developer ID signed ShedServer.app; missing: …" | One or more of the four server signing secrets is unset (the server signing gate) | Add the named secrets (see "Secrets"), then re-run the failed run: nothing has published, so the tag stands |
| `server-signing-preflight` fails (import, `codesign` or `macos-server-app.sh --verify`) | The certificate, its password, the identity string or the team doesn't match, the certificate expired, or Apple's timestamp server was unreachable | Fix the secret, then re-run the failed jobs: no image job has started, so the tag stands. A timestamp failure usually passes on a plain re-run |
| `Run GoReleaser (server)` fails in the `macos-server-app.sh` build hook | A signing failure the preflight didn't catch. The images are already published, but no server artifact, formula or deb is | Fix the cause, then re-run the failed `release` job (see "GoReleaser failed after some artifacts uploaded") |
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

The server config runs `release.mode: keep-existing` +
`release.replace_existing_artifacts: true` — a re-run is safe. The
GitHub Release itself isn't recreated (`keep-existing` finds it already
exists and leaves the body alone — it was written once, in the original
attempt); the re-invocation just re-uploads the server's assets,
replacing any it had already uploaded before the failure rather than
erroring on `already_exists`.

**A signing failure in the server build hook** is the one release-only
failure the gate and the preflight can't rule out. It happens after the
images are published but before goreleaser uploads any server artifact,
formula or deb. Fix the cause and re-run the failed `release` job as
above: the rerun imports the certificate again and rebuilds the server
from scratch. To check a fix without a tag, run the server signing dry
run first.

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
# drifted (bumping both would falsely mark a non-shipping
# component as released at X.Y.Z):
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
  once up front for both manifests plus the `**Ships:**`
  cross-check). The pre-migration flow could tolerate a "tag was
  pushed before the manifest was bumped" state because sync-version
  would fix it after the fact. The current flow REJECTS that state at
  release time — the maintainer reruns the skill against a corrected
  tag rather than shipping mismatched bytes. The mandatory local
  `release-plan.sh` run before pushing the tag (see "Recommend +
  confirm") exists so this rejection is caught pre-tag, not post-tag.
- **GoReleaser pin**: `goreleaser-action@v7` is pinned to `version:
  v2.15.3` everywhere it's invoked (the `release` job here, ci.yml's
  `release-snapshot` job, `server-signing-dry-run.yml`, and `.mise.toml`
  for local `make snapshot-server` runs) — the
  split-config model was spiked and proven against this line. v2.15.3
  ships the secret-redaction fix that prevents secrets from leaking
  into logs, so it's the security floor;
  it's still **below** v2.16, where `brews:` (used by the server
  config) is **hard-deprecated**, so the pin keeps `brews:` functional
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
  chain `build-tools → vz+fc → smoke → release` enforces that. The
  server signing preflight sits in front of the whole chain, so a
  signing problem stops a server tag before any image publishes.
- **`scripts/set-version.sh` is preserved**: the convention's
  `scripts/release/update-version.sh` delegates to it (with a
  jq-verify wrap), so the in-repo bumper isn't duplicated.
- **`workflow_dispatch` republishes are still useful**: if an image
  build flakes after a release, you can re-run
  `publish-images.yaml` manually with the version input and it'll
  republish the images without creating a new Release (the `release`
  job is gated on `tag-push` events).
