# Releasing shed desktop (the `desktop` component)

How the desktop leg of a shed release works. The framework is the
monorepo's manifest-selected release model — read the root
[`RELEASING.md`](../RELEASING.md) "Component selection" section first.
This file covers only what's specific to the desktop component.

## TL;DR

Desktop ships whenever a `vX.Y.Z` tag equals `desktop/VERSION`:

    scripts/release/update-version.sh X.Y.Z --components desktop   # or go,desktop

(run from the repo root; the release skill does this for you). That one
command bumps every desktop surface in lockstep: `desktop/VERSION`,
`crates/Cargo.toml` (+ lock), `desktop/tauri/src-tauri/Cargo.toml` +
`tauri.conf.json` (+ lock). `scripts/release/release-plan.sh` hard-verifies
the lockstep at workflow time and refuses to ship a drifted tree.

## What the workflow does (publish-images.yaml, desktop jobs)

1. **`desktop-release-create`** — desktop-only tags need a GitHub
   Release to upload into, so this job `gh release create`s one
   (idempotent). On combined tags it is **skipped**: goreleaser
   (`mode: replace`) owns release creation, and the desktop jobs run
   strictly after the `release` job succeeds so goreleaser can never
   clobber desktop assets.

2. **`desktop-macos`** (macos-15):
   - Builds the Rust core + Swift app and assembles
     `ShedDesktop.app` (`desktop/scripts/bundle.sh release`), runs the
     unit tests, and packages the drag-install DMG
     (`desktop/scripts/make-dmg.sh`).
   - **Developer ID signing + notarization** activate automatically
     when ALL six Apple secrets exist (the `CAN_NOTARIZE` gate — any
     missing secret means an ad-hoc-signed DMG with the FIRST-LAUNCH
     Gatekeeper note, never a broken build). The cert is imported into
     a throwaway keychain; `notarize.sh` submits and staples **before**
     the EdDSA signing, because stapling rewrites the DMG bytes.
   - Uploads the DMG to the release. Release notes: on combined
     releases the notes are goreleaser's — never touched; on
     desktop-only releases the notarization guidance is **appended**
     (not overwritten).
   - **Sparkle appcast**: signs the (stapled) DMG with
     `SPARKLE_ED_PRIVATE_KEY` via the Sparkle distribution's
     `sign_update`, appends the entry to `docs/appcast.xml` with
     `desktop/scripts/update-appcast.py` (run from the REPO ROOT —
     `SHED_DESKTOP_APPCAST=docs/appcast.xml`,
     `SHED_DESKTOP_REPO=charliek/shed`), validates with `xmllint`, and
     pushes the change to `main` as the release-bot App (3-attempt
     rebase-retry). The feed serves at
     `https://charliek.github.io/shed/appcast.xml` once docs.yml
     redeploys Pages. The bot must be a branch-protection bypass actor
     on `main` for this push.

3. **`desktop-linux`** (matrix: ubuntu-24.04/amd64 +
   ubuntu-24.04-arm/arm64) — builds the Tauri-client `.deb` per native
   arch (`desktop/linux/scripts/build-deb.sh`), install-validates it in
   a clean container (`validate-deb.sh`), and uploads both debs to the
   release.

4. **`desktop-apt-dispatch`** — tells `charliek/apt-charliek` to pull
   the new `shed-desktop` deb into apt.stridelabs.ai
   (`event_type=publish`, `client_payload[package]=shed-desktop`).
   **Prerelease tags (`*-*`) skip this dispatch** — see below.

## Secrets

All on `charliek/shed` (Settings → Secrets → Actions):

| Secret | Purpose |
|---|---|
| `MACOS_CERTIFICATE_P12_BASE64` | Developer ID Application cert (.p12, base64) — CAN_NOTARIZE 1/6 |
| `MACOS_CERTIFICATE_PASSWORD` | .p12 passphrase — CAN_NOTARIZE 2/6 |
| `SHED_DESKTOP_DEVELOPER_ID_IDENTITY` | codesign identity string — CAN_NOTARIZE 3/6 |
| `APPLE_ID` | notarytool Apple ID — CAN_NOTARIZE 4/6 |
| `APPLE_TEAM_ID` | notarytool team — CAN_NOTARIZE 5/6 |
| `APPLE_APP_SPECIFIC_PASSWORD` | notarytool app-specific password — CAN_NOTARIZE 6/6 |
| `SPARKLE_ED_PRIVATE_KEY` | EdDSA private key (base64) for appcast signing; the matching public key is baked into `desktop/Resources/Info.plist.template` (`SUPublicEDKey`) — never rotate one without the other |
| `RELEASE_BOT_CLIENT_ID` / `RELEASE_BOT_APP_KEY` | release-bot GitHub App (shared with the Go leg) — mints the appcast-push and apt-dispatch tokens |

Verify the App installation + the self-repo `contents: write` floor
(the appcast push) via the `sanity-check-app.yml` workflow.

## rc tags (`vX.Y.Z-rc.N`) — the safe rehearsal

A prerelease tag (anything containing `-`) exercises the full desktop
leg with two built-in guards:

- `update-appcast.py` adds `<sparkle:channel>beta</sparkle:channel>` to
  the entry, so only beta-channel Sparkle subscribers see it — stable
  users are untouched.
- `desktop-apt-dispatch` skips (`*-*` guard), so the rc deb never
  reaches the apt index.

That makes `vX.Y.Z-rc.1` (with the desktop manifests bumped to match)
the recommended dress rehearsal for DMG + notarize + EdDSA + appcast +
deb before a first-of-its-kind release.

## Tauri macOS app — the beta rollout (transition)

The macOS app is transitioning from the Swift menu-bar app to the Tauri
client. During the transition **the tag's channel picks the mac
artifact** — two mutually exclusive jobs, exactly one per desktop-shipping
tag (both still require `ship_desktop`, i.e. `desktop/VERSION` == the tag):

| Tag shape | Job | Mac artifact | Appcast channel |
|---|---|---|---|
| Stable (`vX.Y.Z`) | `desktop-macos` (Swift) | `ShedDesktop-<ver>.dmg` (Swift) | stable |
| Prerelease (`vX.Y.Z-rc.N`, any `-`) | `desktop-macos-tauri` | `ShedDesktop-<ver>.dmg` (Tauri) | beta |

The gate is `contains(github.event.inputs.version || github.ref_name,
'-')` (the Swift job carries the negation) — the `inputs.version ||`
half keeps the `workflow_dispatch` republish path routing correctly
(`ref_name` is a branch on dispatch). Both jobs emit the identically
named `ShedDesktop-<ver>.dmg`, so the appcast append step is unchanged;
`update-appcast.py` stamps `<sparkle:channel>beta</sparkle:channel>`
whenever the tag contains `-` (the same rc-tag guard above).

The Tauri client subscribes to the **beta** channel iff its own version
carries a prerelease suffix (`CARGO_PKG_VERSION` contains `-`) — so an rc
build receives rc appcast entries and a stable build never does. That is
what makes an rc1→rc2 pair a **real in-place Sparkle update**, not a
same-version no-op.

### `desktop-macos-tauri` specifics (delta from the Swift job)

- Builds via `make -C desktop tauri-dmg-mac` (Sparkle staged first by
  `scripts/fetch-sparkle.sh`, pinned Sparkle 2.8.1) and runs
  `make -C desktop tauri-test` (the Tauri crate's unit tests — **not**
  the Swift `make test`).
- `sign_update` comes from
  `desktop/tauri/src-tauri/.sparkle-dist/bin/sign_update` (staged by
  `fetch-sparkle.sh`), **NOT** the SwiftPM `desktop/.build/artifacts`
  path the Swift job uses.
- Signing is **deliberately stricter** than the Swift `bundle.sh`. The
  Tauri script (`scripts/bundle-tauri-mac.sh`) signs Sparkle's nested
  helpers in the required inner→outer order (`Installer.xpc` →
  `Downloader.xpc` with `--preserve-metadata=entitlements` → `Autoupdate`
  → `Updater.app` → `Sparkle.framework` → app) and **never `--deep`**,
  while the Swift `bundle.sh` still `--deep`-signs its framework. Do NOT
  "align" the Tauri script down to `--deep` — wrong order / `--deep`
  signs and notarizes clean but breaks at update time.

### Mandatory post-merge rehearsal before any promotion

PR-time CI cannot prove installability — a wrong signing order
notarizes clean and only fails when Sparkle applies the update. The only
real proof is a two-tag rehearsal, done **after merge, before promotion**:

1. Cut `vX.Y.Z-rc.1` (desktop-only prerelease bump via
   `scripts/release/update-version.sh X.Y.Z-rc.1 --components desktop`)
   → `desktop-macos-tauri` builds/notarizes the beta DMG.
2. **Install rc1's DMG by hand** (first install — nothing to update from
   yet).
3. Cut `vX.Y.Z-rc.2` (repeat the desktop version bump —
   `update-version.sh X.Y.Z-rc.2 --components desktop` — commit, tag; a
   tag without the bump fails `release-plan.sh`) and verify a **real
   in-place Sparkle update rc1→rc2** in the installed app (the rc build
   is on the beta channel, so it sees the rc2 entry). This is the
   signing-order proof.

### Promotion (a later decision — NOT this PR)

Promotion flips the two job gates so **stable** tags build the Tauri DMG
(and the Swift job retires). Because the Tauri mac bundle carries the
Swift app's identity (`ai.stridelabs.ShedDesktop`) and the same EdDSA
key, the first stable Tauri release rides the same-key appcast chain
straight into existing Swift installs as an in-place update.

## Local commands

```bash
make -C desktop bundle        # build + assemble ShedDesktop.app (debug)
make -C desktop dmg           # release bundle + drag-install DMG (ad-hoc unless
                              # SHED_DESKTOP_DEVELOPER_ID_IDENTITY is set)
make -C desktop test          # swift unit tests (builds the Rust core first)
make -C desktop tauri-dmg-mac # Tauri mac bundle + drag-install DMG (Sparkle staged + signed;
                              # clobbers the Swift `dmg` outputs under desktop/build/)
make -C desktop deb           # the Tauri .deb via Docker
make -C desktop deb-validate  # install-validate it in a clean container
scripts/release/release-scripts-test.sh   # self-test the release scripts (repo root)
```

## Version surfaces (never hand-edit)

`desktop/VERSION` == `crates/Cargo.toml` `[workspace.package].version`
== `desktop/tauri/src-tauri/Cargo.toml` == `tauri.conf.json` == the
Tauri `Cargo.lock`'s `shed-core`/`shed-app` entries. One command owns
all of them: `scripts/release/update-version.sh X.Y.Z --components
desktop`. `release-plan.sh` exits 1 naming the offender if they drift.
(`desktop/tauri/ui/package.json` is deliberately NOT a version surface —
the Tauri bundle version comes from `tauri.conf.json`.)

The mac-only Tauri overlay `desktop/tauri/src-tauri/tauri.macos.conf.json`
(productName/identifier/embedded `Sparkle.framework`) carries **no**
`version` key — the base `tauri.conf.json` remains the single lockstep
surface, so the Tauri mac DMG and the Linux `.deb` share one version.

## History

Desktop releases ≤ v0.0.13 (and the final v0.0.14 feed-repoint release)
shipped from the archived `charliek/shed-desktop` repo; its appcast on
the old GitHub Pages remains as a frozen fallback feed. The monorepo
feed (`docs/appcast.xml`) was seeded from it — old entries point at
old-repo release assets, which remain valid.
