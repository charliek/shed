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
   (idempotent). It runs only when **no** goreleaser component ships
   (`ship_goreleaser != 'true'`, i.e. neither server nor host-agent).
   Whenever any goreleaser component *does* ship, this job is
   **skipped**: the root `release` job now runs one `goreleaser release`
   invocation per shipping component (server → host-agent,
   `release.mode: keep-existing`), and the FIRST of those invocations
   creates the release and owns the
   changelog body — later goreleaser invocations, and the desktop jobs
   (which run strictly after `release` succeeds), only add assets and
   never touch the body or clobber each other's uploads.

2. **`desktop-macos`** (macos-15) — the Tauri build, the macOS client as
   of 0.9.0:
   - Builds the Rust core + Tauri frontend and assembles the signed
     `ShedDesktop.app`/DMG via `make -C desktop tauri-dmg-mac`
     (`scripts/bundle-tauri-mac.sh` under the hood — Sparkle staged
     first by `scripts/fetch-sparkle.sh`), then runs
     `make -C desktop tauri-test` (the Tauri crate's unit tests).
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
     `SPARKLE_ED_PRIVATE_KEY` via
     `desktop/tauri/src-tauri/.sparkle-dist/bin/sign_update` (staged by
     `fetch-sparkle.sh` — there is no SwiftPM artifacts path for this
     job), appends the entry to `docs/appcast.xml` with
     `desktop/scripts/update-appcast.py` (run from the REPO ROOT —
     `SHED_DESKTOP_APPCAST=docs/appcast.xml`,
     `SHED_DESKTOP_REPO=charliek/shed`), validates with `xmllint`, and
     pushes the change to `main` as the release-bot App (3-attempt
     rebase-retry). The feed serves at
     `https://charliek.github.io/shed/appcast.xml` once docs.yml
     redeploys Pages. The bot must be a branch-protection bypass actor
     on `main` for this push. `update-appcast.py` is unchanged by the
     0.9.0 promotion: it still stamps
     `<sparkle:channel>beta</sparkle:channel>` iff the tag has a `-`.

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
| `SPARKLE_ED_PRIVATE_KEY` | EdDSA private key (base64) for appcast signing; the matching public key is baked into `desktop/tauri/src-tauri/Info.plist:33-36` (`SUPublicEDKey`) — never rotate one without the other |
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

## The Tauri macOS app

`desktop-macos` is the only macOS desktop job — it runs on every
desktop-shipping tag, stable and prerelease alike (`ship_desktop` still
gates it, i.e. `desktop/VERSION` == the tag). It builds and ships the
**Tauri** DMG; there is no separate Swift release job any more.

The appcast **channel** still depends on the tag shape, unchanged by the
promotion: `update-appcast.py` stamps
`<sparkle:channel>beta</sparkle:channel>` whenever the tag contains `-`
(the same rc-tag guard as above), and plain `<sparkle:channel>` (stable)
otherwise. The Tauri client itself subscribes to the **beta** channel
iff its own version carries a prerelease suffix (`CARGO_PKG_VERSION`
contains `-`) — so an rc build receives rc appcast entries and a stable
build never does. That is what makes an rc1→rc2 pair a **real in-place
Sparkle update**, not a same-version no-op.

### Job specifics

- Builds via `make -C desktop tauri-dmg-mac` (Sparkle staged first by
  `scripts/fetch-sparkle.sh`, pinned Sparkle 2.8.1) and runs
  `make -C desktop tauri-test` (the Tauri crate's unit tests).
- `sign_update` comes from
  `desktop/tauri/src-tauri/.sparkle-dist/bin/sign_update` (staged by
  `fetch-sparkle.sh`) — there is no SwiftPM artifacts path for this job.
- The Tauri script (`scripts/bundle-tauri-mac.sh`) signs Sparkle's nested
  helpers in the required inner→outer order (`Installer.xpc` →
  `Downloader.xpc` with `--preserve-metadata=entitlements` → `Autoupdate`
  → `Updater.app` → `Sparkle.framework` → app) and **never `--deep`** —
  wrong order / `--deep` signs and notarizes clean but breaks at update
  time.

### Promoted in 0.9.0

The Tauri app is the shipped macOS client as of 0.9.0 — the former
Swift release job (which used to own stable tags) is retired; the Swift
*sources* stay in the tree pending a later demolition (see the
follow-up ticket filed alongside the promotion commit).

The two-tag rc rehearsal described above (cut `vX.Y.Z-rc.1`, install by
hand, cut `vX.Y.Z-rc.2`, verify a real in-place Sparkle update) remains
the **recommended path for any future signing change** — PR-time CI
cannot prove installability; a wrong signing order notarizes clean and
only fails when Sparkle applies the update.

**0.9.0 itself skipped that rehearsal, by owner decision** (few users;
if the signing order is wrong, fix it live or in 0.9.1).

Be precise about what that leaves unproven. The Tauri mac bundle carries
the Swift app's identity (`ai.stridelabs.ShedDesktop`), the same EdDSA
key and the same feed URL — all three verified statically — so the 0.9.0
Tauri release is *aimed* straight at existing Swift installs through the
same appcast chain. **Nothing has established that Sparkle performs that
swap**: matching identity and key is what makes the update possible, not
evidence that it works. The mac job has never run, and this is the first
tag on which it will.

That is exactly why the rehearsal exists, and the risk it covers is not
hypothetical: a signing order that notarizes clean and only fails when
Sparkle applies the update would leave Swift users on 0.8.1 with a
failed-update dialog until 0.9.1. Accepted knowingly. Keep a Mac on
Swift 0.8.1 to watch the first update land, and treat a failure there as
a 0.9.1 blocker rather than a surprise.

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
