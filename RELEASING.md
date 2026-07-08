# Releasing shed

The general release framework is `cc-plugins:release-workflows`; this
file documents what's specific to this repo (which is a lot — shed is
the most complex consumer).

## TL;DR

    /release-workflows:release v0.5.9

Everything else is automatic.

## Component selection (the manifest-selected release model)

The monorepo carries multiple release components on ONE `vX.Y.Z` tag
family. A component ships in a release **iff its version manifest
equals the tag**:

| Component | Ship selector (version manifest) | Artifacts |
|---|---|---|
| `go` | `.claude-plugin/plugin.json` `.version` | Go binaries, brew formulas, `shed-server`/`shed-machine-rc` debs, ghcr rootfs images |
| `desktop` | `desktop/VERSION` (with `crates/Cargo.toml`, the Tauri `Cargo.toml`/`tauri.conf.json`, and both Cargo locks in verified lockstep) | ShedDesktop DMG + Sparkle appcast, `shed-desktop` debs |

- **Bumping**: `scripts/release/update-version.sh X.Y.Z
  [--components go,desktop]`. The default is `go` (preserves the
  historical one-arg behavior); the release skill computes the
  component set for the release being cut and passes `--components`
  explicitly. Unknown components hard-error.
- **CI-side selection**: `publish-images.yaml`'s first job runs
  `scripts/release/release-plan.sh`, which maps the tag to
  `ship_go`/`ship_desktop` outputs. Every downstream job gates on
  those.
- **No-manifest-matches guard**: if a tag matches NEITHER manifest
  (a forgotten `update-version.sh` run), `release-plan.sh` exits 1 and
  the whole workflow fails loudly — a silent no-op release is
  impossible. Fix by bumping the right manifest(s) and cutting a fresh
  tag.
- **Interleaved component versions**: component versions advance
  independently — each component's "current version" is **the most
  recent tag that shipped it**, not the most recent tag overall. The
  server may sit at a newer tag than desktop (or vice versa),
  legitimately. Never reuse a version for a different component set.
  Corollary: a desktop-only tag publishes **no**
  `ghcr.io/charliek/shed-{vz,fc}-*` rootfs images — pin servers to the
  last Go-shipping tag.
- Both scripts are covered by `scripts/release/release-scripts-test.sh`
  (run in CI by `ci.yml`'s `plugin` job).

The desktop leg's recurring specifics (secrets, DMG/notarize, Sparkle
appcast, debs, apt dispatch, rc-tag rehearsals) live in
[`desktop/RELEASING.md`](desktop/RELEASING.md).

## What happens

1. **`release-workflows:release`** (LLM, local):
   - Verifies branch (`main`) + clean tree + CI green on HEAD. ci.yml
     has a `ci-success` aggregator (skipped-by-path-filter counts as
     pass) — the skill's ci-success check is a real gate now.
   - Asks/confirms version
   - Drafts a CHANGELOG entry from `git log v<previous>..HEAD`, commits
     as `docs(changelog): vX.Y.Z entry`
   - Runs `scripts/release/update-version.sh X.Y.Z`:
     - Delegates to `scripts/set-version.sh` (the existing in-repo
       bumper) to update `.claude-plugin/plugin.json`'s top-level
       `.version`
     - jq-verifies the bump landed (defense against set-version.sh's
       silent-failure mode on malformed JSON)
   - Commits as `chore(version): bump to X.Y.Z`
   - Tags `vX.Y.Z` (annotated) on the version commit
   - `git push --follow-tags` (admin bypasses the ruleset)

2. **`publish-images.yaml`** (CI, on tag push `v*`) — a `release-plan`
   job first (component selection, see above), then the Go chain below
   (gated on `ship_go`) plus the desktop jobs (gated on `ship_desktop`;
   documented in `desktop/RELEASING.md`). The Go chain keeps its strict
   dependency ordering:

   **`publish-build-tools`** (`ubuntu-latest`, multi-arch):
   - Builds + pushes `ghcr.io/charliek/shed-build-tools:vX.Y.Z` (and
     `:latest`) for `linux/amd64` + `linux/arm64`. The downstream
     image-publish jobs FROM/docker-run this image during their builds,
     so it must exist on ghcr.io before they start.

   **`publish-vz`** (`ubuntu-24.04-arm`, needs build-tools):
   - Builds shed-overlay initramfs + 3 OCI images
     (`shed-vz-{base,extensions,full}` arm64), pushes to ghcr.io.

   **`publish-fc`** (`ubuntu-latest`, needs build-tools):
   - Same as `publish-vz` but for `linux/amd64` Firecracker images.
     Runs in parallel with `publish-vz`.

   **`smoke`** (reusable `smoke-linux.yml`):
   - Install-only smoke against the tagged commit on GHA's
     ubuntu-latest. KVM-required cycle is the maintainer's
     responsibility (see `scripts/smoke-test-linux.sh --from-local`
     for the matching local script).

   **`release`** (needs all four above; tag-push only, `ship_go` only):
   - The old inline "plugin.json matches the tag" check moved to the
     `release-plan` job (see "Component selection" above), which gates
     this entire chain.
   - Runs `go test ./...` + golangci-lint
   - Mints a release-bot App token scoped to `charliek/homebrew-tap`
   - Runs `goreleaser release --clean`:
     - Builds 3 Go binaries × OS/arch matrix with ldflag versioning
     - Tarballs + checksums.txt + `Formula/shed.rb` (pushed to
       homebrew-tap with App-minted token) + `shed-server_*.deb` (via
       nfpms)
   - Mints a release-bot App token scoped to `charliek/apt-charliek`
   - Dispatches `event_type=publish` with
     `client_payload[package]=shed-server` to apt-charliek so the new
     .deb gets indexed into apt.stridelabs.ai. Retry loop with stderr
     capture for diagnosability.

   `workflow_dispatch` (manual republish) re-runs only the
   image-publish chain; the `release` job is gated on `tag-push`
   events so manual republishes don't accidentally cut a new GitHub
   Release.

   The `sync-version` job that existed pre-migration is **removed** —
   plugin.json is now bumped locally by the release skill before
   tagging, matching the convention.

The maintainer runs step 1; everything else is automated.

## Version files this repo owns

`scripts/release/update-version.sh` bumps:

- `--components go` (default): `.claude-plugin/plugin.json` `.version`
  (via `scripts/set-version.sh`, with a jq-verify safety net)
- `--components desktop`: `desktop/VERSION`, `crates/Cargo.toml`
  (`[workspace.package].version`) + `crates/Cargo.lock` regen,
  `desktop/tauri/src-tauri/Cargo.toml` + `tauri.conf.json` +
  `desktop/tauri/src-tauri/Cargo.lock` regen (with a path-dep
  lock-entry verify for `shed-core`/`shed-app`)

The Go binaries' version comes from a build-time `-X` ldflag injected
by GoReleaser via `.goreleaser.yaml`'s `builds[].ldflags`. The Docker
images are tagged from `GITHUB_REF_NAME` at workflow time. None of
those need a source-tree bump.

`CHANGELOG.md` is maintained by the release skill for human-readable
in-repo history. GoReleaser's auto-generated release notes (filtered:
skip `docs:`, `test:`, `chore:`, `ci:`) go on the GitHub Release body.

`pyproject.toml` is for mkdocs only and has its own version cadence;
not touched by `update-version.sh`.

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
| `release` job fails at "Verify plugin.json matches tag" | Plugin.json wasn't bumped before tagging; tag was created against the wrong commit; or developer ran `git tag` manually without using `/release-workflows:release` | Don't force-fix in CI — re-run `/release-workflows:release` against a corrected tag |
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

GoReleaser's `mode: replace` reuses the existing Release.

### Image-publish job failed (build-tools / VZ / FC)

Re-run the failed job from the Actions UI. The downstream `release`
job will start once the failed publish job + smoke pass on the rerun.

### Wrong version got tagged (sync-version was the safety net, now it's gone)

The plugin.json must be at the released tag's version on main, even
without the sync-version job:

```bash
# Manually fix plugin.json on a hotfix branch:
git checkout -b fix/plugin-version-resync
scripts/set-version.sh X.Y.Z
git commit -am "chore(version): resync plugin.json to vX.Y.Z"
# Open a PR; merge through normal flow.
```

Then cut a fresh patch tag (`vX.Y.Z+1`) so the release pipeline picks
up the corrected plugin.json. Don't try to retag `vX.Y.Z` — fight that
URGE; force-updating tags strands published images and creates a
confusing state.

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
- **Inline `Verify plugin.json matches tag` step**: the post-migration
  replacement for the deleted `sync-version` job. The pre-migration
  flow could tolerate a "tag was pushed before plugin.json was bumped"
  state because sync-version would fix it after the fact. The new
  flow REJECTS that state at release time — the maintainer reruns the
  skill against a corrected tag rather than shipping mismatched bytes.
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
