# Monorepo Consolidation: shed + shed-desktop + shed-extensions

Design document for consolidating the shed family of repos into a single monorepo with a shared Rust client core, a simplified install/update story, and selective per-component releases.

> **Status (2026-07-09).** **Phases 1–2 are complete and released.** Phase 1
> (shed-extensions import) shipped in **v0.7.9**; Phase 2 (shed-desktop import)
> shipped desktop on the shared version line in **v0.7.10**, after the final
> old-repo Sparkle migration release `shed-desktop v0.0.14` (the old→new feed-move
> was maintainer-validated on a real install). Both old repos are archived. The
> Rust core lives at `crates/`, the Swift/Tauri app + packaging + `shedtest` at
> `desktop/`; the manifest-selected release model + Sparkle continuity are live.
> **`shed-gtk` was retired pre-import** — Tauri is the shipped Linux client, so the
> §5.3 rows mentioning `shed-gtk` are void. **Phase 3 is redefined (§6/§7):
> client-side Rust unification** — port the host-agent to Rust, absorb it in-process
> into the desktop clients, then port the CLI — superseding the original
> bundle-first plan (see the Appendix for the reversed decision). The leg 3a.1
> host-agent-port plan is panel-reviewed and pending a go-ahead. The shed-mobile
> Rust spike (§7 Phase 0) runs in parallel and gates no phase here. §3 decisions
> were settled 2026-07-06/07; §10 lists remaining implementation details.

## 1. Current State Summary

### Repo inventory

| Repo | Language(s) | Contents | Release artifacts |
|---|---|---|---|
| `charliek/shed` | Go | `shed` CLI, `shed-server`, `shed-agent` (in-VM), `shed-egress-proxy`, nested `sdk/` module, VZ/FC rootfs image builds, MkDocs site | brew `shed` formula; apt `shed-server` .deb; ghcr `shed-build-tools`, `shed-vz-*`, `shed-fc-*` images |
| `charliek/shed-desktop` | Swift, Rust, TS/React | Swift menu-bar app (SwiftPM + Sparkle 2), Rust workspace `core/` (`shed-core`, `shed-app`, `shed-core-ffi` UniFFI, `shed-gtk`, `shedctl`), separate Tauri v2 workspace + React UI, pytest e2e harness | DMG + Sparkle appcast (GitHub Pages `shed-desktop/appcast.xml`); Linux `shed-desktop` .deb into apt |
| `charliek/shed-extensions` | Go (CGO on darwin) | `shed-host-agent` (credential broker, Touch ID), `shed-machine-rc`, 4 pure-Go guest binaries (`shed-ext-ssh-agent`, `shed-ext-aws-credentials`, `docker-credential-shed`, `shed-ext-rc`), guest image overlay (`image/etc/`) | brew `shed-host-agent` + `shed-machine-rc` formulas; apt `shed-machine-rc` .deb; ghcr `shed-extensions` multi-arch guest image |
| `charliek/shed-mobile` | Dart/Flutter | Fat client (pinned-TLS HTTP, dartssh2, in-app terminal); Android/macOS/Linux; **no Rust yet** | ad hoc (sideload APK via its own release flow; no store) |
| `charliek/shed-remote-agent` | TS/Bun | RC-session orchestrator web UI (Hono API + React) | none (run from source) — **stays out of the monorepo** (feature donor) |
| `charliek/homebrew-tap`, `charliek/apt-charliek` | — | Distribution repos | receive artifacts from the above; **stay separate** |

### The duplication map

The core motivation. Each protocol/behavior below is independently hand-implemented per language, with parity maintained by fixtures and discipline rather than a shared source:

| Concern | Go | Swift | Rust | Dart | TS |
|---|---|---|---|---|---|
| Secure-shed transport (pinned TLS + SSH-minted control token) | `cmd/shed`, `sdk/` | `ShedKit` | `shed-core` (`tls.rs`, `token.rs`) | `lib/net/`, `lib/control/` | `apps/api/src/lib/secureTransport.ts` |
| `~/.shed/config.yaml` parsing | `internal/config/client.go` | `ShedConfig.swift` | `shed-core/src/config.rs` | `lib/servers/` | `appConfig.ts` |
| Host-agent UDS approval protocol (v2, NDJSON) | shed-extensions mini-RFC + impl | `HostAgentProtocol.swift` | `shed-core/src/approval/protocol.rs` | — | — |
| RC session convention (tmux `rc-<slug>`, DTO) | `shed-extensions/internal/rc` | — | `shed-core/src/rc.rs` | `lib/rc/` | `packages/shared` Zod + golden DTO |

The strategic answer (already begun in shed-desktop) is the **Rust core**: `shed-core`/`shed-app` become the single client-side implementation, consumed in-process via UniFFI (Swift, transitional), Tauri (desktop end-state), and flutter_rust_bridge or Tauri-mobile (mobile, pending spike). The Go server and CLI stay Go; the CLI *may* move to Rust much later (§7 Future).

### Cross-repo release choreography today

Shipping a guest-binary change currently takes: tag shed-extensions → wait for ghcr multi-arch publish → PR bumping `ARG SHED_EXT_VERSION` in **two** Dockerfiles (`vz/Dockerfile:27`, `firecracker/Dockerfile:27`) → tag shed → rebuilt rootfs images. Host-agent protocol changes need coordinated releases across shed + shed-extensions + shed-desktop. Three repos fire releases into two distribution repos plus Sparkle, each with its own release-bot wiring, changelog, and version track.

### Install story today (macOS)

```
brew install charliek/tap/shed            # CLI + server + egress-proxy
brew install charliek/tap/shed-host-agent # credential broker (launchd service)
brew services start shed-host-agent
<download shed-desktop DMG, xattr -dr quarantine>   # updates via Sparkle
```

Four steps, three update mechanisms (brew, brew-services, Sparkle).

## 2. Target End State

### North star

- **One repo** (`charliek/shed`). Go server/CLI at the root (module path unchanged), one shared Rust workspace as the client foundation, Tauri as the single desktop app on macOS + Linux, mobile on the same Rust core (mechanism decided by spike).
- **Two installables**: `shed` (CLI + server; brew/apt) and `shed-desktop` (DMG+Sparkle / .deb; absorbs shed-host-agent via a Rust rewrite + in-process absorption — Phase 3, not a bundled Go binary). Everything else is an internal artifact.
- **Selective releases on one version line**: a single `vX.Y.Z` tag family; each release includes only the components whose version manifests were bumped to match the tag (§4). `v0.1.8` may ship shed + desktop; `v0.1.9` may ship only the server. Mobile keeps its ad hoc flow.
- **shed-remote-agent stays out** — its RC-orchestration features migrate into desktop/CLI over time; the repo itself is not merged.

### Target repo layout

```
shed/                          # module github.com/charliek/shed (unchanged, at root)
├── cmd/                       # ALL Go binaries
│   ├── shed/  shed-server/  shed-agent/  shed-egress-proxy/     (existing)
│   ├── shed-host-agent/  shed-machine-rc/                       (from extensions; host-side)
│   └── shed-ext-ssh-agent/  shed-ext-aws-credentials/
│       docker-credential-shed/  shed-ext-rc/                    (from extensions; guest-side)
├── internal/                  # existing packages + internal/ext/* (from extensions)
├── sdk/                       # nested Go module (unchanged; extensions code already imports it)
├── crates/                    # shared Rust workspace (from shed-desktop core/)
│   ├── shed-core/  shed-app/  shed-core-ffi/  shedctl/
│   └── shed-gtk/              # transitional; retired with the Swift app
├── desktop/                   # from shed-desktop
│   ├── Sources/  Tests/  Package.swift        # Swift app (transitional)
│   ├── tauri/                 # Tauri app + its own isolated Cargo workspace
│   ├── linux/  packaging/  Resources/  scripts/  tools/
│   ├── VERSION  CHANGELOG.md
├── mobile/                    # Flutter app (Phase 4, after the spike picks a direction)
├── guest/extensions/          # guest image overlay (from shed-extensions image/etc/)
├── vz/  firecracker/  build-tools/  scripts/   # unchanged (Dockerfiles gain ext-builder stage)
├── docs/                      # one MkDocs site; gains Desktop + Extensions sections
│   └── appcast.xml            # Sparkle feed moves here (§4.5)
├── configs/                   # + extensions.example.yaml
└── skills/  tests/  .github/workflows/
```

Two Cargo workspaces on purpose (carried over from shed-desktop): `crates/` is the dependency-clean core; `desktop/tauri/src-tauri/` keeps its own workspace so WebKitGTK/Tauri deps never bleed into the core (mobile will consume `crates/` via path deps the same way).

### Install story, end state (macOS)

```
brew install charliek/tap/shed
brew install --cask charliek/tap/shed-desktop   # cask is optional-but-nice; DMG works too
```

Desktop bundles/absorbs the host-agent (§7 Phase 3), so the separate brew formula + `brew services` step disappears. Updates: brew for CLI/server, Sparkle for desktop (cask marked `auto_updates`). Linux: `apt install shed-server shed-desktop`.

## 3. Design Decisions (settled 2026-07-06/07)

1. **Ordering: extensions first, desktop close behind.** Extensions is Go-into-Go, touches nothing in flight, and immediately deletes the worst cross-repo friction (ghcr publish + `SHED_EXT_VERSION` pins). Desktop imports after Tauri Phase C + the Swift-on-Rust-core cutover land in the old repo. Phases 1 and 2 are intended to run close together.
2. **Go module stays rooted at `github.com/charliek/shed`.** Zero import rewrites for existing code; goreleaser/CI paths mostly survive. Extensions' packages move *into* this module (its `sdk` import already points at the right path). No `go.work`; the existing `sdk` nested-module + `replace` pattern is kept.
3. **Shared Rust workspace at top-level `crates/`,** not under `desktop/`, because mobile is a second consumer.
4. **Release model: one shared version line, selective content** (§4). Every release is a `vX.Y.Z` tag; the version-bump script is the component selector (a component ships iff its manifest equals the tag). Rejected: unified releases of everything on every tag (no-op deploy noise as desktop/server/mobile iterate at different speeds) and two independent tag families (`v*` + `desktop-v*` — superseded same-day by this scheme; see Appendix).
5. **Sparkle survives the move** via a final old-repo release that repoints `SUFeedURL` at the monorepo feed (§4.5). Cost is near zero; uninstall/reinstall remains the accepted fallback.
6. **One MkDocs site.** Desktop and extensions docs fold in as sections, page-by-page evaluation during the merge (some pages will be stale or redundant with shed's existing `docs/reference/extensions.md`).
7. **Mobile: parallel spike now, import later.** flutter_rust_bridge-on-`shed-core` vs Tauri-mobile is evaluated with path deps across sibling repos *before* any import; shed-mobile keeps its ad hoc release flow throughout and after import.
8. **Host-agent absorption is rewrite-first, absorb-second** (§7 Phase 3; *reversed 2026-07-09* — this originally read "bundle-first, rewrite-second," where the install-story win did not wait for the Rust rewrite). The host-agent is ported to a standalone Rust daemon first, then absorbed in-process into the desktop clients; the install-story win now lands with the absorption (3a.2), not before. See §6 and the Appendix for the tradeoff.
9. **shed-remote-agent is a feature donor, not a merge target.**

Settled 2026-07-07 (resolving the original open questions):

10. **Extensions packages import under `internal/ext/`** — a pure mechanical move. Promoting/rehoming packages happens in the *protocol-unification future phase* (§7 Future), never during the import itself.
11. **Release-skill component selection: auto-detect with confirmation as the default.** Path-diff per component since its last shipped release, echo the computed set before tagging. When the release request already names the components (or says to defer some), that counts as the confirmation and the skill proceeds without re-asking.
12. **apt-charliek resolution: scan-until-found with a generous cap (~30 releases)**, `optional:` semantics unchanged; staleness reporting deferred. The work may be planned from this repo but lands as an apt-charliek PR.
13. **shed-machine-rc stays a standalone formula/deb** through the consolidation; a Rust port is plausible later (no urgency — Future bucket). Whether its target-side binary ever merges into the shed CLI is decided by the machine-RC UX work, not here.
14. **Guest binaries follow the existing shed-agent staging pattern** (§4.3): script-cross-compiled into the Dockerfile context, bind-mount installed, covered by the `SHED_INSTALL_SHA` content-hash cache-buster. No in-Docker Go builds.
15. **`shed-desktop` cask lands at Phase 3**, generated by the desktop release leg, with `auto_updates true` so brew defers day-to-day updates to Sparkle.

## 4. Release & Distribution Design

### 4.1 One version line, manifest-selected content

Every release is a `vX.Y.Z` tag on shed's existing track (v0.7.x → …). What a release *includes* is declared by which component version manifests were bumped to the tag value — **a component ships in `vX.Y.Z` iff its manifest equals `X.Y.Z` at the tag**. This generalizes gates both repos already run: shed's release job jq-verifies `.claude-plugin/plugin.json` matches the tag; shed-desktop's create-release gates on tag == `VERSION` == Cargo version. Those existing checks become the *inclusion signals* rather than hard failures.

| Component | Version manifest (the selector) | Jobs gated on manifest == tag |
|---|---|---|
| Go side | `.claude-plugin/plugin.json` | goreleaser (binaries `shed`, `shed-server`, `shed-agent`, `shed-egress-proxy`, `shed-host-agent`¹, `shed-machine-rc`; brew formulas `shed`, `shed-host-agent`¹, `shed-machine-rc`; debs `shed-server`, `shed-machine-rc`); ghcr `shed-build-tools` + VZ/FC rootfs images (guest binaries compiled from the tree) |
| Desktop | `desktop/VERSION` (+ `crates/` Cargo versions in lockstep) | macOS DMG + EdDSA-signed Sparkle appcast entry + notarization; Linux `shed-desktop` .deb; apt dispatch |
| Mobile | — (ad hoc, unchanged from today's shed-mobile flow) | manual / existing workflow, carried over at import |

¹ `shed-host-agent` is now built from the **Rust** crate (`builder: rust`) as of leg 3a.1's release-wiring change — same brew formula + tarballs. The Go daemon (`cmd/shed-host-agent`) and its rollback path were retired in plan 006 (see §6 · 3a.1 Status). The standalone *artifact* (the Rust daemon as its own install target) is retired only at 3a.2 (absorbed into the desktop app; the headless standalone persists for server/no-desktop use).

So `v0.1.8` may bump both manifests (shed + desktop ship together — the default when both changed); `v0.1.9` may bump only `plugin.json` (server-only). The release skill takes a component list and bumps only those manifests; the changelog entry states what shipped.

Rules and consequences:

- **Guard against forgotten bumps**: the release workflow fails loudly if *no* manifest matches the tag.
- **Never reuse a version for a different component set.** A desktop hotfix after a server-only `v0.1.9` is `v0.1.10` with only `desktop/VERSION` bumped; the appcast steps 0.1.8 → 0.1.10 (Sparkle only needs monotonic — gaps and the one-time 0.0.13 → 0.8.x jump at import are fine).
- **Component versions interleave**: a component's "current version" is the last release that included it (server at 0.1.9 while desktop is at 0.1.8). Anything answering "is X up to date?" must compare against the latest release *containing that component's artifact*, not the latest tag — this affects apt-charliek (§4.4) and any future update-check/fleet-upgrade tooling (`/api/info` reports the server's last-shipped version, which may lag the newest tag legitimately).
- **One shared semver**: a breaking change in any component bumps the global number; the version alone doesn't signal per-component compatibility. Accepted for a solo-maintainer project.
- **GitHub release creation**: goreleaser creates it when the Go side ships; on desktop-only releases the desktop leg creates it (shed-desktop's existing idempotent create-release pattern).

### 4.2 Consolidated goreleaser

One `.goreleaser.yaml` gains the extensions builds. `shed-host-agent` needs **CGO on darwin** (Touch ID via LocalAuthentication), and shed's release job currently runs `ubuntu-latest` — the merged release job moves to `macos-latest`, which shed-extensions' pipeline already proves can produce the full artifact set (darwin CGO builds + CGO-off linux cross-builds + nfpm debs + brew formula pushes). Docker image jobs stay on their current runners; only the goreleaser leg moves. Extensions' snapshot assertions (machine-rc tarball contains exactly one binary; per-arch debs exist) move into shed's CI.

Version ldflags unify on `github.com/charliek/shed/internal/version`; extensions' own `internal/version` package is deleted in the merge.

### 4.3 Guest binaries: build in-tree, delete the ghcr publish

The mechanism mirrors how `shed-agent`/`shed-firstboot` already get into the rootfs images — they are **not** built inside Docker. `scripts/build-{vz,firecracker}-rootfs.sh` cross-compiles them on the host (`GOOS=linux GOARCH={arm64|amd64} go build`) into the Dockerfile context dir, and the Dockerfile installs them from a bind-mount (`RUN --mount=type=bind,source=.,target=/ctx`), with the `ARG SHED_INSTALL_SHA` content hash busting the layer when a rebuilt binary would otherwise be silently reused from BuildKit's size+mtime-keyed cache (#227).

The four guest extension binaries join that exact pattern:

1. `scripts/build-{vz,firecracker}-rootfs.sh` additionally cross-compile `shed-ext-ssh-agent`, `shed-ext-aws-credentials`, `docker-credential-shed`, `shed-ext-rc` (`CGO_ENABLED=0`, version ldflags) into the context dir, and stage the overlay from `guest/extensions/` (systemd units, `shed-extensions.d/` manifests, `environment.d/`) alongside.
2. The `extensions` stage installs binaries + overlay from `/ctx` via the same bind-mount RUN, replacing today's `RUN --mount=type=bind,from=shed-extensions` copy from the remote image.
3. `SHED_INSTALL_SHA` extends to cover the four new binaries + overlay files, preserving the stale-binary protection.
4. Deleted outright: `ARG SHED_EXT_VERSION` + the `FROM ghcr.io/charliek/shed-extensions` stage in both Dockerfiles, the `--shed-ext-version` script flags, shed-extensions' root `Dockerfile`, and its ghcr `docker` release job.

No `golang` builder stage, no build-context change (stays `vz/` / `firecracker/`), no new caching strategy — host Go build caching applies as it does for `shed-agent` today.

Benefit beyond fewer releases: guest binaries, server, and SDK change **atomically** — a bus-protocol change is one PR, one tag, no version-skew window.

### 4.4 apt-charliek and homebrew-tap changes

- `homebrew-tap`: no repo changes needed — goreleaser keeps pushing formulas with the same names (`shed`, `shed-host-agent`, `shed-machine-rc`), now all from `charliek/shed`. `brew upgrade` is seamless. New at Phase 3: a `shed-desktop` **cask** pushed by the desktop release leg — points at the DMG release asset, `auto_updates true` (Sparkle owns in-place updates; `brew upgrade` skips it unless `--greedy`), with an `uninstall`/`zap` stanza for clean removal. The cask installs the same `.app` from the same DMG into `/Applications` — indistinguishable from a drag-install, so Sparkle updates it in place normally, and an explicit `brew upgrade --cask` still works too (both are generated by the same release leg, so they never disagree at release time). The DMG remains the direct-download path; the cask is the scriptable one.
- `apt-charliek/packages.yaml`: `shed-machine-rc` and `shed-desktop` entries change `repo:` to `charliek/shed`.
- **⚠ Required apt-charliek change — selective releases break "latest release".** apt-charliek's collect resolves each package against a repo's *latest release*, hard-failing when the matching `.deb` is absent (see the shed v0.7.8 incident). With selective content, a server-only release has no `shed-desktop_*.deb` and a desktop-only release has no `shed-server_*.deb` — and since all tags share one family, a per-package tag-pattern filter can't distinguish them. apt-charliek instead needs **"latest release containing a matching asset"** resolution: per package, scan back through recent releases (bounded, e.g. last N) to the newest one whose assets match the glob. Marking everything `optional: true` is the fallback but guts the missing-deb guard. Small, self-contained apt-charliek PR; must land **before the first selective release** — i.e. by Phase 2, since Phase 1 releases always carry the full Go artifact set.

### 4.5 Sparkle / appcast migration

1. Monorepo hosts the feed at `docs/appcast.xml` → published at `https://charliek.github.io/shed/appcast.xml` (MkDocs copies non-`.md` files in `docs_dir` verbatim; shed's `docs.yml` deploys it). Seed it with the old feed's entries.
2. `scripts/update-appcast.py` + the release-bot append-and-push flow carry over into the desktop release leg unchanged (runs only when `desktop/VERSION` matches the tag).
3. **Final old-repo release**: one last shed-desktop release whose only change is `SUFeedURL` → the new feed URL. Existing installs take one update through the old feed and are migrated. The old repo is then archived; GitHub keeps serving its Pages, so stragglers still see the (frozen) old feed and can be nudged manually.
4. `SUPublicEDKey` and the `SPARKLE_ED_PRIVATE_KEY` secret move to the monorepo unchanged — same key, no re-trust needed. Apple notarization secrets (the six `CAN_NOTARIZE` gates) move too.
5. When the Tauri app replaces the Swift app (Future), updater choice is re-decided (Sparkle vs tauri-plugin-updater); that is the one acceptable breaking-update point.

### 4.6 Changelog and version bumps

One root `CHANGELOG.md`; each entry lists the components shipped (e.g. a `**Ships:** server, desktop` line). `scripts/release/update-version.sh X.Y.Z --components go,desktop` bumps only the named manifests (`.claude-plugin/plugin.json`; `desktop/VERSION` + `crates/` Cargo versions in lockstep, absorbing the old repo's bump script). The release skill grows the component argument; default is "everything that changed since its last shipped release". Desktop's old tag == `VERSION` == crate-version check survives as the inclusion gate (§4.1), not a failure condition.

### 4.7 What still publishes externally, and why

The consolidation eliminates every external artifact that existed *only* as inter-repo plumbing. What remains is product distribution, not release choreography:

| Artifact | Fate | Why |
|---|---|---|
| ghcr `shed-extensions` guest image | **deleted** (§4.3) | existed only for the `COPY --from` pin |
| Standalone `shed-host-agent` formula/tarballs | **deleted at Phase 3** | absorbed into the desktop app |
| ghcr `shed-vz-*` / `shed-fc-*` rootfs images | stays | the product's VM-image distribution channel — servers `shed image pull` them at runtime; `default_image` refs point at ghcr |
| ghcr `shed-build-tools` | stays | pinned erofs tooling pulled at *build* time by CI and dev machines (`RELEASE_BUILD_TOOLS_REF`); hosts never need it at runtime |
| GitHub release tarballs/DMG/`.deb`s | stays | what brew formulas, the cask, apt, and Sparkle point at |

## 5. Import Mechanics

### 5.1 History-preserving merge

For each source repo, rewrite paths with `git filter-repo` in a scratch clone, then merge:

```bash
# example: shed-extensions
git clone git@github.com:charliek/shed-extensions.git /tmp/ext-import && cd /tmp/ext-import
git filter-repo --path-rename cmd/:cmd/ --path-rename internal/:internal/ext/ \
                --path-rename image/etc/:guest/extensions/ …   # full map in §5.2
cd ~/projects/shed
git remote add ext-import /tmp/ext-import
git fetch ext-import && git merge --allow-unrelated-histories ext-import/main
```

Full history (`git log --follow`, blame) survives. Straight `git subtree add --squash` is the fallback if filter-repo path-collision handling gets fiddly, at the cost of history granularity.

### 5.2 shed-extensions path map

| From (shed-extensions) | To (monorepo) | Notes |
|---|---|---|
| `cmd/*` (all 6) | `cmd/*` | no collisions with shed's 4 existing cmds |
| `internal/{protocol,sshagent,awsproxy,dockercred,rc,clirc,testutil}` | `internal/ext/…` | import-path rewrite `charliek/shed-extensions/internal/X` → `charliek/shed/internal/ext/X` (mechanical sed; `go build ./...` is the check) |
| `internal/version` | *deleted* | binaries switch to shed's `internal/version`; goreleaser ldflags updated |
| `image/etc/` | `guest/extensions/` | systemd units, `shed-extensions.d/` manifests, `environment.d/` — consumed by the new Dockerfile builder stage |
| `configs/extensions.example.yaml` | `configs/` | brew formula seeding path updated in goreleaser brew stanza |
| `Dockerfile` (guest image) | *deleted* | replaced by in-tree builder stage (§4.3) |
| `.goreleaser.yaml` | merged into root `.goreleaser.yaml` | builds/archives/brews/nfpms stanzas |
| `.github/workflows/release.yaml` | merged into `publish-images.yaml` release job | macos runner move (§4.2); `docker` job deleted |
| `docs/` | `docs/` sections (page-by-page) | dedupe against existing `docs/reference/extensions.md`; `security-posture.md`, `host-agent-ipc.md`, `shed-machine-rc.md` likely survive largely intact |
| `RELEASING.md`, `CLAUDE.md` | folded into shed's | note extensions' RELEASING.md had drift (claimed "no apt" while machine-rc ships a .deb) — rewrite, don't copy |

The `github.com/charliek/shed/sdk` imports in extensions code need **no** rewrite — the path is already correct and resolves via the existing root-module `replace`.

Deliberate non-goal at import time: extensions' `internal/protocol` stays a *separate copy* of the bus wire types (it was intentionally decoupled from shed's plugin package). Unifying them is a Future item — do the mechanical move first, the semantic merge later.

### 5.3 shed-desktop path map

| From (shed-desktop) | To (monorepo) | Notes |
|---|---|---|
| `core/{shed-core,shed-app,shed-core-ffi,shedctl}` | `crates/…` | workspace root becomes `crates/Cargo.toml`. (`shed-gtk` was retired in the old repo pre-import — not imported.) |
| `core/artifacts/`, `core/fixtures/` | `crates/…` | UniFFI-generated Swift bindings + parity fixtures |
| `Sources/`, `Tests/`, `Package.swift` | `desktop/` | `Package.swift` path refs to the FFI xcframework updated |
| `tauri/` (UI + `src-tauri/` workspace) | `desktop/tauri/` | its `Cargo.toml` path-deps repointed `../core/…` → `../../crates/…`; keeps its own lockfile/workspace |
| `linux/`, `packaging/`, `Resources/` | `desktop/…` | `.deb` build scripts get path updates |
| `scripts/{bundle,make-dmg,notarize,update-appcast,build-core}.sh|py` | `desktop/scripts/` | `build-core.sh` gets the `crates/` path |
| `tools/` (shedtest, fake-host-agent, …) | `desktop/tools/` | pytest harness; its uv env stays desktop-scoped |
| `docs/appcast.xml` | `docs/appcast.xml` | §4.5 |
| `docs/*` (reference, development) | `docs/` Desktop section | page-by-page evaluation |
| `VERSION` | `desktop/VERSION` | becomes the desktop inclusion selector (§4.1); adopts the shared version line |
| `CHANGELOG.md` | folded into root `CHANGELOG.md` | single changelog with per-entry "Ships:" component list (§4.6) |
| `plans/` | drop or archive under `docs/discovery/desktop-history/` | phase plans are historical once phases land |
| `.github/workflows/{ci,release}.yml` | merged: CI legs into root `ci.yml` (path-filtered); release jobs become the desktop leg of the release workflow, gated on `desktop/VERSION == tag` (§4.1) | desktop repo's path-filtered `changes` job pattern is adopted at the monorepo root |
| `Makefile` | `desktop/Makefile`, with root `Makefile` gaining `desktop-*` passthrough targets | keeps `make build/test` at root meaning "Go", avoids a mega-Makefile |

### 5.4 CI shape after both imports

Root `ci.yml` adds a `changes` path-filter job (desktop repo pattern) gating: Go test/lint (existing), `crates/` cargo test + clippy (macOS + Linux), Swift build/test + e2e (macos-15), Tauri legs, deb-build smoke. A shed-server-only PR runs only the Go legs; a desktop-only PR skips them. Rough runner-cost note: the macOS legs are the expensive ones and run only when `desktop/**` or `crates/**` change.

## 6. What Phase 3 (client-side Rust unification) looks like

> **Redefined 2026-07-09.** Phase 3 was originally "bundle the Go host-agent inside the desktop app (bundle-first)." It is now **client-side Rust unification**: rewrite the host-agent in Rust (a standalone daemon), absorb it in-process into the desktop clients, then port the CLI. This **reverses** the earlier bundle-first decision — see the Appendix for the tradeoff. Leg **3a.1** (host-agent port) is **done through release-wiring** (pre-tag); leg **3a.2** (in-process absorption) has a **pre-panel draft plan** (see the 3a.2 Status bullet).

The consolidation thesis (§1) is one shared client-side implementation — the Rust core — replacing per-language reimplementations. The desktop app already rides `crates/shed-core`/`shed-app`; the two clients still on their own Go implementations are the **host-agent** and the **CLI**. Phase 3 retires both onto the core. The Go **server stays Go** (the other end of the wire; heavier/more complex; stronger libraries — the same reason `shed image`'s OCI work stays Go).

**Leg 3a — host-agent → Rust**, in two plans:

- **3a.1 — port.** A standalone Rust host-agent daemon (`crates/shed-host-agent`), **wire-compatible** with the Go server, guest extensions, and desktop client, across the daemon's **two wire surfaces**: (A) the desktop/status **UDS** (approval / `token.get` / status / audit — wire types + client already in Rust), and (B) the shed-server **plugin bus** via `sdk.HostClient` (where the credential backends live — networked HTTP-streaming subscribe/respond, TLS-pinned, green-field in Rust). Shipped opt-in, diffed against the Go daemon via a differential + golden-fixture harness, then default. `objc2-local-authentication` replaces the Go CGO Touch-ID path (removes the CGO-on-darwin dependency). Ships **no user-visible change** — the install win is 3a.2.
    - **Status (port done, Go retired).** The port + full parity/differential harness are complete, and the release pipeline builds the shipped `shed-host-agent` homebrew binary from the Rust crate via GoReleaser's OSS `builder: rust` (`cargo zigbuild`; `builder: prebuilt` is Pro-only). The Go `cmd/shed-host-agent` and its rollback path (goreleaser ids, CI filter entry, the Go-vs-Rust differential harness) were **retired in plan 006** — a deliberate maintainer waiver of the ~2-release rollback window this doc originally described (only v0.8.0 had shipped the Rust agent at retirement). Post-removal rollback is a prior-release brew formula reinstall; source-level rollback ends there. `tests/host-agent-diff/` continues as the wire harness, now asserting the Rust daemon against goldens recorded from the last agreeing Go↔Rust run rather than against a live Go process. See `RELEASING.md` § "Host-agent: Rust binary". No tag has been cut for the Rust-only host-agent line yet — the first tag that ships it is a deliberate maintainer action.
- **3a.2 — absorb into the desktop clients.** Move the daemon in-process into the desktop app (mac Swift + Tauri Linux; the Rust `HostAgentClient` already exists → process-to-in-process). **This is where the install-story win lands for desktop users**: the separate host-agent install step disappears (its brew formula + launchd/systemd unit retire for the desktop path) → `brew install shed` + the app. The **headless standalone host-agent** distribution stays available for server / no-desktop environments (see the Headless brokering bullet).
    - **Status (first-cut plan drafted, pre-panel).** A DRAFT executable plan exists (`~/.claude/plans/monorepo-phase3-3a2-absorb.md`), written at the close of the 3a.1 completion program — **not yet panel-reviewed** (its `/planning:ask-panel` pass is its own session, run against the post-3a.1-merge tree) and **not** an execution authorization (3a.2 begins only after 3a.1 merges + soaks). The seam it cuts along: 3a.1's `desktop-forwarding` cargo feature cleanly isolates **Surface A** (the desktop UDS), so 3a.2 relocates the **Surface-B core** (bus loop + credential backends + approval + minting) into a linkable library the clients embed — likely a dedicated `crates/shed-broker` lib the headless bin also uses. Its central open question is the **packaging split** (mac in-process via the `shed-core-ffi`-style staticlib/UniFFI xcframework vs a Linux Tauri sidecar/systemd unit) and whether the broker must outlive the GUI — deliberately left to the panel.

Consequences to design for:

- **Headless brokering.** Most clients run the desktop app continuously (the assumed common case), where the app owns brokering in-process. For **headless** environments (Linux server, no desktop) the 3a.1 standalone daemon **persists as a stripped-down headless host-agent CLI** — brokering over the plugin bus with policy/native approval, with the **desktop-forwarding surface (A) removed** (redundant/broken once the desktop owns brokering; 3a.1 gates surface A behind a feature so this is a clean omission). So the standalone binary is *not* deleted — it becomes the headless variant.
- **Behavior change:** on a desktop machine, app-not-running ⇒ background token minting for tunnels + egress audit stop. Acceptable / the simplification goal.
- `shed-machine-rc` is **not** absorbed — it stays a standalone Go CLI for orchestrators (shed-remote-agent, mobile) to drive over SSH.

**Leg 3b — CLI → Rust** (after 3a.1 + a CLI-hard-cores spike). Grow `crates/shed-app` + a thin `crates/shed-cli`, differential-harness against the Go CLI, opt-in then default. **The image/OCI subsystem stays Go** — `shed image` is client-side OCI work today; server-delegate it or ship a small Go helper the Rust CLI dispatches to. Same "heavy/strong-Go-libs stays Go" principle as the server.

## 7. Implementation Phases

**Phase 0 — pre-work (now, parallel).**
Goal: clear the runway; pick the mobile mechanism.
1. Land Tauri Phase C + Swift-on-Rust-core cutover in the shed-desktop repo (in flight).
2. Mobile spike in the existing sibling repos: wire `shed-core` into shed-mobile via flutter_rust_bridge (path dep on `../shed-desktop/core`); prototype the same screens on Tauri-mobile. Decide FRB-in-Flutter vs Tauri-mobile. Neither blocks Phases 1–3.
3. Merge this doc; open the apt-charliek asset-aware-resolution issue (§4.4) so it's ready before Phase 2.
Deliverable: Phase C merged; a written mobile-direction decision; apt-charliek change scoped.

**Phase 1 — import shed-extensions.**
Goal: one Go module, guest binaries built in-tree, ghcr publish deleted.
1. filter-repo import per §5.2; import-path rewrites; `go build ./... && go test ./...` green.
2. Extend the rootfs build scripts + Dockerfiles per §4.3 (script-compiled guest binaries + `guest/extensions/` overlay staged via the existing bind-mount + `SHED_INSTALL_SHA` pattern); delete `SHED_EXT_VERSION` plumbing; rebuild + boot-test both backends via the dev-server flow (`make test-integration-dev`, FC variant).
3. Merge goreleaser configs; move the release job to macos-latest; port extensions' snapshot assertions into CI.
4. Cut `vX.Y.Z`: confirm brew formulas (`shed`, `shed-host-agent`, `shed-machine-rc`) publish from the monorepo, apt gets `shed-server` + `shed-machine-rc` debs, rootfs images carry in-tree guest binaries.
5. Archive shed-extensions (README pointer to the monorepo).
Deliverable: a single tagged release producing every former shed + shed-extensions artifact; shed-extensions archived.

**Phase 2 — import shed-desktop.**
Goal: desktop builds, tests, and releases from the monorepo; Sparkle continuity.
1. Land the apt-charliek asset-aware-resolution change; update `packages.yaml` repo fields.
2. filter-repo import per §5.3; `crates/` workspace at top level; Tauri path-deps repointed; root CI gains path-filtered desktop/crates legs; desktop e2e harness green in-tree.
3. Add the desktop leg to the release workflow (gated on `desktop/VERSION == tag`, §4.1); move Sparkle + notarization secrets; seed `docs/appcast.xml`; add the no-manifest-matches-tag guard.
4. Cut the **final old-repo release** repointing `SUFeedURL`; then the first monorepo release that includes desktop (desktop jumps 0.0.13 → the shared 0.8.x line); verify a real Sparkle update old→new feed and `apt install shed-desktop`.
5. Archive shed-desktop.
Deliverable: desktop released from the monorepo; an existing install updated across the feed move without reinstall.

**Phase 3 — client-side Rust unification** (redefined 2026-07-09; supersedes bundle-first — see §6 + Appendix).
Goal: retire the two remaining Go client-side reimplementations (host-agent, CLI) onto the Rust core. Sequential; each sub-step is its own panel-reviewed plan.
1. **3a.1 — host-agent port.** Standalone Rust daemon (`crates/shed-host-agent`), wire-compatible over both surfaces (desktop UDS + plugin bus); differential + golden-fixture harness; opt-in → default. No user-visible change (foundation).
2. **3a.2 — host-agent absorption.** In-process into the desktop clients; retire the standalone formula/launchd **for the desktop path** → **the install-story win** for desktop users (`brew install shed` + app). Headless server/no-desktop use keeps the stripped-down standalone daemon (desktop-forwarding removed). Optional `shed-desktop` cask.
3. **3b — CLI port.** Grow `crates/shed-app` + thin `crates/shed-cli`; image/OCI stays Go; opt-in → default.
Deliverable: the Rust core is the single client implementation; fresh-machine install is `brew install shed` + desktop app, no separate host-agent step.

**Phase 4 — import shed-mobile.**
Goal: mobile in-tree on the shared core, per the Phase 0 decision.
1. Import under `mobile/` (history preserved); wire path deps to `crates/`; CI legs path-filtered; keep the ad hoc release flow.
Deliverable: mobile builds in-tree against `crates/shed-core`.

**Future bucket (explicitly not scheduled):** Swift app retirement at Tauri parity (+ updater re-decision, the accepted breaking point); GTK crate retirement; machine-RC/"run a plan on a machine" UX in desktop + CLI (absorbing the `shed-plan` skill's coordination and shed-remote-agent's features; decides whether the target-side binary merges into the shed CLI); possible Rust port of `shed-machine-rc` (no urgency); **the protocol-unification pass** — (a) Go-internal: collapse `internal/ext/protocol` into a single canonical bus-wire package shared by server and extension binaries (the copies exist only because the repos were separate), promoting/rehoming `internal/ext/*` packages as part of it, and (b) cross-language: one source of truth for the host-agent UDS protocol, RC DTO, and client config schema (plausibly defined in `shed-core` or a JSON schema) with other languages generated or fixture-validated against it, replacing today's hand-maintained parity copies (Phase 3's host-agent differential + golden-fixture harness is a first instance of this fixture-validation, and porting the daemon adds the Rust copy it should validate); reconcile `shed-ext-rc` vs `shed-machine-rc` naming.

## 8. Key Files Reference

| Repo | File | Change |
|---|---|---|
| shed | `vz/Dockerfile:25-28`, `firecracker/Dockerfile:25-28` | drop the ghcr `FROM` stage; install guest binaries + overlay from `/ctx` (§4.3); extend `SHED_INSTALL_SHA` coverage |
| shed | `scripts/build-{vz,firecracker}-rootfs.sh` | drop `--shed-ext-version`; cross-compile the 4 guest binaries into the context (shed-agent pattern) |
| shed | `.goreleaser.yaml` | + host-agent (CGO darwin), machine-rc builds; + 2 brew formulas; + machine-rc nfpm |
| shed | `.github/workflows/publish-images.yaml` | release job → macos-latest; + machine-rc apt dispatch |
| shed | `.github/workflows/ci.yml` | + `changes` path-filter job; + crates/desktop legs (Phase 2) |
| shed | `mkdocs.yml` | + Desktop + Extensions nav sections; + this doc |
| shed | `go.mod` | absorb extensions deps (CGO darwin bits are build-tagged, no module-level change) |
| shed-extensions | *everything* | imported per §5.2, then archived |
| shed-desktop | *everything* | imported per §5.3 after Phase C, then archived |
| shed-desktop | final release | `SUFeedURL` → `charliek.github.io/shed/appcast.xml` |
| apt-charliek | `packages.yaml` + collect logic | latest-release-with-matching-asset resolution (§4.4); `repo:` fields → `charliek/shed` |
| homebrew-tap | — | no change; optional `shed-desktop` cask later |

## 9. Risks

| Risk | Mitigation |
|---|---|
| Selective releases confuse apt-charliek's latest-release resolution | §4.4 asset-aware change lands before the first selective release; Phase 1 releases always carry the full Go artifact set |
| Tag cut with no manifest bumped → silent no-op release | workflow guard fails loudly when no component manifest matches the tag (§4.1) |
| Stale guest binaries silently reused from BuildKit's bind-mount cache | extend the existing `SHED_INSTALL_SHA` content-hash cache-buster (#227 pattern) to cover the new binaries + overlay |
| goreleaser-on-macos regression for the existing Go artifact set | extensions' pipeline is the existence proof; dry-run `goreleaser release --snapshot` in CI before the first tag |
| Sparkle feed-move release missed by a client | old Pages keeps serving (archived repos still serve Pages); manual reinstall is the accepted fallback |
| Desktop import while Phase C unfinished | hard-gated: import waits for Phase C + Swift-on-Rust cutover |
| CI cost/time blowup in one repo | path-filtered `changes` job (desktop repo already runs this pattern) |
| History/blame loss | filter-repo path-rename (not squash) is the primary plan |

## 10. Open Questions

The original six open questions were resolved 2026-07-07 (§3 items 10–15). What remains is small implementation detail:

1. **Release-skill path→component map** — the exact path globs for auto-detection (§3.11) and where the map lives (skill vs a checked-in file the workflow can also read). Decide when building the release skill changes.
2. **apt-charliek scan specifics** — exact cap value and API-call bounding for the latest-release-with-matching-asset scan (§3.12). Decide in that PR.

## Appendix: Alternatives considered

| Alternative | Verdict |
|---|---|
| Desktop-first import order | Rejected — Phase C in flight; extensions is the cheap, independent win |
| One unified version; every tag ships everything | Rejected — no-op deploys as components iterate at different speeds |
| Two independent tag families (`vX.Y.Z` + `desktop-vX.Y.Z`, per-component version tracks) | Superseded (2026-07-06, same discussion) by one shared version line with manifest-selected content (§4.1) — one changelog/version to reason about, no tag-filter split across workflows, and the per-package apt fix it required (tag patterns) is replaced by the more robust asset-aware resolution |
| Keep the ghcr guest-image publish, just automate the pin bump | Rejected — the publish exists only for the pin; in-tree builds make protocol changes atomic |
| Restructure Go code under `server/` / new module path | Rejected — import-path churn across the module, sdk, and external references for zero functional gain |
| `go.work` multi-module (extensions as its own module) | Rejected — single module + existing nested `sdk` is fewer moving parts; extensions never needs independent tagging again |
| Merge shed-remote-agent | Rejected — it's the feature donor; its capabilities migrate, the repo doesn't |
| Merge shed-mobile now | Deferred — direction (FRB vs Tauri-mobile) must be picked by the Phase 0 spike first |
| Switch desktop updates to tauri-plugin-updater during the move | Deferred — keep Sparkle through the transition; re-decide at Swift retirement, the one accepted breaking point |
| Rust-rewrite host-agent before absorbing it (vs bundle-first) | **Adopted 2026-07-09, reversing the earlier verdict.** Bundle-first ships a Go binary *inside* the Swift/Tauri apps that we would delete anyway; the desktop client is already Rust, so in-process absorption is clean rather than throwaway. Accepted cost: the host-agent port (leg 3a.1) ships no user-visible improvement — the install win moves to 3a.2 — and there is a two-daemon transition period. See §6/§7. |
| Plan the whole Rust-unification arc cold / big-bang both legs | Rejected — the `sdk.HostClient` plugin-bus transport and the CLI's hard cores (in-process OCI, SSH/PTY) are unsized until the host-agent port ships and proves the differential-harness methodology; each sub-step is its own panel-reviewed plan |
