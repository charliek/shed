# Monorepo Phase 2 — Validation Parity Report

**Branch:** `feature/monorepo-desktop-import` · **Date:** 2026-07-08 · **Commit range:** `b9d992a..2f1d64c` (5 import commits `1f50afe`→`b2d9c73`, plus fixes `e347f36`, `e649fb2`, `2f1d64c`) · **Verdict:** all 11 live-validation steps pass.

This report is the durable record of the Phase 2 monorepo import validation (importing the shed-desktop Rust workspace to `crates/` and the Swift/Tauri app + packaging + harness to `desktop/`, wiring the manifest-selected release model, Sparkle continuity, and path-filtered CI). It mirrors the plan's "Live validation plan" — one row per step, run once, recorded with result and evidence pointer. No tag or release is cut in this PR; the release-time proofs are deferred (see the last section).

Screenshots and raw command logs referenced below live in the session artifacts (not committed). CI run IDs are the durable evidence for step 7.

## Parity matrix

| # | Step | Where | Result | Key counts / evidence |
|---|------|-------|--------|-----------------------|
| 1 | Go parity | Local | **PASS** (4 env failures, non-import) | `make check` golangci 0 issues, all Go tests pass; `go list ./...` = 44 pkgs, 0 `desktop`/`crates` leakage; `make test-integration` 77 passed / 4 env-failed / 16 skipped. Failures diagnosed below. |
| 2 | Rust core | Local | **PASS** | `cargo test --workspace --locked` + `-p shed-app --features rc` + clippy ×2 (`-D warnings`) all `0 failed`, clippy clean. From `crates/`. |
| 3 | Swift app | Local | **PASS** | `build`; `test` 122 executed / 0 failures (incl. FFI canary); `e2e-ci` 74 passed / 17 skipped; `m0-gates` all green (arm64-only, 8.7 MB, golden 3 payloads byte-identical rust↔swift); `smoke-real-launch` + `smoke-launch-window` OK. |
| 4 | Swift-on-Rust drives a real shed | Local | **PASS** | Bundled app vs `localmac-dev` (secure mode): `identify.core=rust`, `test_mode=false`; `host list` reachable; create `val2-check` → app observes **running** → delete confirmed gone. Screenshots in session artifacts. |
| 5 | Tauri | Local | **PASS** | `tauri-test-linux` (approval-seam) OK; `tauri-build-linux` WebKitGTK 2.44 Xvfb render gate 62 passed / 29 skipped; `deb` built `shed-desktop_0.0.1~dev_arm64.deb`; `deb-validate` clean ubuntu:24.04 → `IDENTIFY OK: platform=tauri core=rust`; `e2e-tauri` (native mac WKWebView) 61 passed / 30 skipped. |
| 6 | DMG + host-agent handshake | Local | **PASS** (after 1 fix) | `bundle` + `dmg` → `ShedDesktop-0.0.13.dmg` (6.7M, ad-hoc signed); `live-verify.sh --handshake` → **HANDSHAKE VERIFIED** over the UDS, brew agent undisturbed. Socket-isolation fix `e649fb2` (below). |
| 7 | CI selectivity | CI | **PASS** (caught 2 real bugs) | Three draft scratch PRs (#245 go-only, #246 desktop-only, #247 workflow-change; now closed, branches deleted). After fixes: go-only ran ONLY Go legs (desktop skipped); desktop-only skipped ALL Go legs, ran only swift; workflow PR ran everything. `ci-success` green with skips counted as pass in every case. Runs below. |
| 8 | Both skills | Local | **PASS** (no skill fixes) | `shedtest-mac`: fast-path (build/test 122·0/e2e-ci 74·17) + by-hand shedctl drive all correct. `shedtest-linux`: `tauri-build-linux` 62·29, `tauri-test-linux`, `core-linux` 127 passed, `deb-validate` all green. Both skills accurate in the monorepo. |
| 9 | Release-model tests | Local + CI | **PASS** | `scripts/release/release-scripts-test.sh` → all 11 checks passed (go-only / desktop-only / both / neither→exit1 / lockstep-broken→exit1, `--components go\|desktop`, the `0.0.x→0.8.0` jump, idempotent re-run, tauri.conf.json + stale-lock drift). Runs in a temp copy; tree clean after. Wired into CI (`plugin` leg). |
| 10 | Sparkle (pre-release portion) | Local | **PASS** | `xmllint --noout docs/appcast.xml` well-formed; throwaway ed25519 seed + `sign_update`; `update-appcast.py` append (13→14) + dedupe (byte-identical re-run, pubDate preserved); served over HTTP → curl 200, well-formed. Real old→new feed hop deferred (below). |
| 11 | Docs | Local | **PASS** | `make docs` built clean; `cmp docs/appcast.xml site-build/appcast.xml` **byte-identical** (mkdocs copies the feed verbatim). |

## Issues found by validation

The validation process caught three real defects that the automated gates alone would have missed — each is recorded here as the process working as designed (local gates passing while a CI-checkout or bind-collision path failed, then fixed).

| Issue | Root cause | Fix |
|-------|-----------|-----|
| **dorny `go` filter predicate-quantifier** | `dorny/paths-filter`'s default `predicate-quantifier=some` made the negations in the `go` filter dead — `'**'` matched every changed file, so the negations (`!desktop/**`, `!crates/**`, …) never excluded anything and Go legs ran on desktop-only PRs. | The `go` filter moved to its own dorny step with `predicate-quantifier: every`. Commit **`e347f36`**. Verified by the desktop-only scratch PR (#246) then skipping all Go legs. |
| **`Resources/bin` swallowed by `bin/` gitignore** | The unanchored `bin/` gitignore rule silently excluded `desktop/Resources/bin/{shed-open-ghostty,shed-open-iterm2,shed-open-roost.py,shed-open-warp}` from the import commit. Local gates passed (files present on disk) but CI checkouts lacked them → swift Bundle + tauri-build `generate_context` resource validation failed on GitHub runners. | Added a negation for the path and committed the four files with exec bits. Commit **`2f1d64c`**. Verified by the feature-branch full run + desktop-only scratch run going green. |
| **`live-verify.sh --handshake` socket collision** | The in-repo rewrite updated the host-agent build path but not the socket-isolation mechanism; the current `cmd/shed-host-agent` ignores the deprecated `desktop.socket_path` config and the production brew host-agent already holds the default socket, so the private handshake agent refused to bind. | Launch the private agent with `SHED_HOST_AGENT_SOCKET_DIR="$TMP"` (the documented test escape hatch) so it binds an isolated socket dir, brew agent untouched. Commit **`e649fb2`**. |

## Environmental findings

None of these implicate the import (the branch changes zero Go; `go list` + `make check` confirm no traversal into `desktop/`/`crates/`). They are pre-existing environment/state artifacts, recorded so the next operator does not re-diagnose them.

- **`test_integration` — mini3 FC slowness (×2):** `test_delete_running_shed_is_fast[fc]` (delete took 25.4 s vs the `<10s` ceiling) and `test_lifecycle_plain[fc]` (`shed stop` → `context deadline exceeded` POST to `mini3:8443`). Both reproduced on a quiet machine → a perf/network property of mini3's released deb FC server, not the import.
- **`test_integration` — brew VZ stale-agent stderr separation (×2):** `test_nopty_stderr_separated[vz]` and `test_binary_stdout_survives_stderr[vz]` both fail because stderr folds into stdout. Their docstrings require "a rootfs rebuilt with the stderr-separation change" — the fix is baked into the shed-agent *image*, not the host server, and the brew VZ server's installed rootfs carries an older shed-agent (the "agent is baked into the rootfs image" trap). Fixing it means rebuilding the production brew rootfs (out of scope).
- **`SHED_VZ_SERVER` default vs reality:** the default `SHED_VZ_SERVER=my-server` has no matching `~/.shed/config.yaml` entry on this host, so a plain `make test-integration` SKIPs all VZ tests. The brew VZ server is registered as `localmac-dev`; the VZ leg was run with `SHED_VZ_SERVER=localmac-dev`.
- **Desktop app "host agent · not connected":** observed while the app still read the secure `localmac-dev` server fine. Expected — the app's host-agent link is only needed for approval/write flows, not reads.
- **`shed delete` needs `-f` when scripted:** a bare `shed delete` prompts `y/N`; non-interactive cleanup must pass `-f`. Harmless, worth knowing.

## Deferred to release time

These proofs need published releases, the real CI EdDSA key, and notarization credentials — they cannot run pre-merge and are sequenced in the plan's post-merge **Release sequencing** checklist (steps A/B), each user-approved, no tag cut unilaterally.

- **Real old→new Sparkle feed hop:** staged `0.0.13` (old feed) → `0.0.14` (SUFeedURL repoint) → `0.8.0` (new feed) update with no reinstall, on a machine running 0.0.13. Needs the published final old-repo release + the real key. The pre-release parse/EdDSA/append/dedupe path is proven above (step 10).
- **Notarized DMG:** ad-hoc signing suffices for the PR gate; the notarized build runs in CI on `macos-15` with the six Apple secrets.
- **Appcast bot-push to main:** the release-bot commit of `docs/appcast.xml` to `main` depends on the branch-protection bypass for the release-bot App (a flagged user action); exercised for real at the recommended `v0.8.0-rc.1` rehearsal.
- **`apt install shed-desktop`:** proven after the first combined release ships the `.deb` and apt-charliek repoints `shed-desktop` → `charliek/shed` (then flip `optional: true` → required).

Expected asymmetry to note at release time: the new feed permanently lacks the `0.0.14` entry (appended only to the old feed) — by design, not a bug.
