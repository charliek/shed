# Changelog

All notable changes to this project will be documented in this file.

<!--
  `**Ships:**` convention: each release entry opens with a `**Ships:** …`
  line naming the components that tag actually shipped, using the
  canonical tokens `server`, `host-agent`, `machine-rc`, `desktop`
  (comma-separated; legacy `server/CLI` is accepted as an alias for
  `server`, for entries written before the rename). A component ships iff
  its version manifest equals the tag (server: .claude-plugin/plugin.json;
  host-agent: crates/shed-host-agent/VERSION; machine-rc:
  cmd/shed-machine-rc/VERSION; desktop: desktop/VERSION) — see
  RELEASING.md "Component selection". ENFORCED by
  scripts/release/release-plan.sh on stable tags (a mismatched, missing,
  unknown, or duplicate token fails the release); prerelease tags have no
  entry and are exempt. shed-desktop's pre-monorepo changelog stays in the
  archived charliek/shed-desktop repo.
-->

## Unreleased

_Staged by plan 010 (the machine-hub port); at release time fold this body
into the new `## vX.Y.Z` section and replace this note with a real
`**Ships:**` line (host-agent at minimum; machine-rc if the retirement lands
in the same tag) — release-plan.sh never reads an `## Unreleased` heading._

- **The machine RC hub moves into `shed-host-agent`.** The daemon hosts the
  activity hub (`127.0.0.1:1029`) as a supervised resident role: bind-as-lock
  with a polite defer-and-retry while an older `shed-machine-rc serve` holds
  the port, `rc_hub.enabled` config knob (default on), an `RC hub:` line in
  `shed-host-agent status` (+ the `rc_hub` LiveStatus field), and a
  `shed-host-agent rc-hub` foreground diagnostic subcommand. The hub itself is
  a Rust port of the Go hub (`shed-broker`'s `rc_hub`), wire-identical at
  `/v1` under the `tests/rc-parity` hub differential family (snapshot, SSE,
  side-effect, opencode-lane, and cursor-ingest cells — both daemons run side
  by side in CI).
- **`sx` create's hub ensure is probe-first**: a healthy hub of either
  provider short-circuits; the `shed-machine-rc serve --detach` spawn remains
  as the fallback for machines without the agent.
- Machine-posture docs: `docs/extensions/sx.md` gains "The machine hub"
  (trust model: loopback + SSH tunnel is the boundary; no proxy on machines),
  `shed-machine-rc.md` carries the retirement roadmap note.

## v0.8.2 — 2026-08-17

**Ships:** server, machine-rc

This patch makes the observatory real on live sheds: the guest rc hub, the v2
wire contract, and the cursor/codex/opencode signals shipped in the three
preceding blocks have all been sitting unreleased, so a rebaked rootfs is what
this tag is for. The client-side Rust porcelain (`sx`) and its Go↔Rust parity
harness land in-tree but ship **no release component** — `sx` is built from
`crates/` for now. `host-agent` and `desktop` are deliberately NOT bumped: they
pick up only library-level `crates/` changes (notably an `RcRunner` stdin
error-propagation fix the desktop will inherit), which ride the next desktop
release rather than forcing a DMG/appcast cycle here.

### Added

- **RC wire-contract v2 (`rc_version` 3 → 4).** The `shed-ext-rc` guest hub gains a
  `lane` field on every session DTO (always present; `"tui"` for every kind today),
  `kind_features` grows `feed`/`interrupt`/`attach` alongside the existing
  `post_input`/`approvals`/`watch`/`input` (`watch` is now deprecated in favor of
  `feed`, kept in lockstep; `post_input` stays current), and three reserved hub verbs
  — `POST /v1/sessions/{slug}/turn`, `/interrupt`, `/approvals/{id}` — are routed on
  both the hub and the server's proxy allowlist with their full 413/400/404/409
  precedence and success shapes pinned, so clients (mobile above all) can code
  against a stable surface before any lane implements them (every kind answers `409
  not_supported` today). The message feed gains an `approval_request` row type
  carrying a machine-readable `approval` block (id-keyed, last-write-wins), sessions
  gain a `pending_approvals` snapshot (hub-layer, empty this phase), and
  `needs_approval` becomes a legal `activity` wire value (still unproduced). New
  `capabilities` feature token `contract-v2` is the client's route-existence check.
  The Rust and Swift/TS mirrors are updated to match, and all four golden fixture
  copies are enforced byte-identical by a new parity guard. See
  `docs/extensions/rc-helper.md` for the full contract. Contract-only — no lane
  implementation, no new activity derivation, no client UI behavior change.

- **The rc observatory goes live: opencode dual-control, cursor/codex approval
  signals, kickoff hardening.** The contract-v2 verbs shipped as scaffolding above now
  have a real lane: **opencode's `turn`/`interrupt`/`approvals/{id}` are live**,
  steering the session through its TUI's embedded HTTP+SSE server while the same
  session stays tmux-attachable (dual control — a client and a human can watch/steer
  the same session at once). Every other kind still answers `409 not_supported`.
  `needs_approval` is now produced for the first time, from two different mechanisms
  matched to what each agent actually exposes: opencode derives it from live
  `permission.asked`/`question.asked` events (settled, demoted to stability on a dead
  SSE stream), while codex and cursor — whose native surfaces carry no
  approval-pending signal at all (codex's rollout JSONL provably filters approval
  records before they're written; cursor's hooks fire no approval event) — derive it
  from a debounced pane anchor and emit **informational** `approval_request` rows
  (`approvals` stays `"tui"`; no decisions are ever remotely honored). Cursor stops
  being stability-only: a hub-owned hook script (merged into `~/.cursor/hooks.json`,
  never clobbering existing entries) relays turn boundaries, tool calls/results, and
  assistant messages into a normalized message feed and `gated` input, over a new
  guest-internal ingest route (`POST /v1/ingest/cursor`, 256 KiB cap, never proxied by
  the server). Kickoff hardening: `shed create`/`attach`/`plan` gate on the target
  agent binary actually being installed (naming it in the error, instead of an opaque
  "died on create"), `shed plan` gains `--permission-mode`/`--skip` (previously
  `shed attach`-only), both `attach` and `plan` gain `--workdir`, and opencode gets a
  positive needs-auth classifier (its "Connect a provider" dialog) instead of reading
  `starting` until timeout. **Behavior break:** opencode's `POST /input` now returns
  the ordinary non-gated `409` — `kind_features.input` moved from `gated` to `turn` for
  opencode, and steering moves to the `turn` verb; no shipped client posted to
  opencode's `/input`, so first-party consumers move in lockstep. See
  `docs/extensions/rc-helper.md` for the full as-built contract.

- **RC engine seams for the Rust porcelain, and a `machines:` config section the CLI
  stops eating.** Three narrow, behavior-neutral changes on the Go side, landed for the
  Rust one-shot engine port (`sx`, below). (1) A new `SHED_RC_NO_HUB` environment
  kill-switch (any non-empty value) makes `shed-ext-rc`/`shed-machine-rc` skip the post-create activity-hub
  ensure — every `create` otherwise spawns or health-probes a detached `serve` daemon on
  `127.0.0.1:1029` unconditionally, which a hermetic test harness cannot tolerate.
  Unset, nothing changes. (2) The `warnHook` and `EnsureHub` diagnostic lines hardcoded
  the `shed-ext-rc: ` prefix regardless of binary, so **`shed-machine-rc` reported
  itself as `shed-ext-rc`**; both now stamp the running binary's own program name.
  (3) `~/.shed/config.yaml` may now carry a top-level `machines:` section (native RC
  hosts — the schema is owned by the Rust client core), and the `shed` CLI round-trips
  it **byte-intact** through the whole-document rewrite that `shed server add`, a
  shed-cache refresh, a token mint, or a `shed delete` performs. Previously any such
  command silently deleted a hand-added section. Go never interprets the subtree.

  The porcelain itself — `sx` (`crates/sx`: `agent`/`plan`/`ls`/`watch`/`attach`/`kill`
  across `local | machine:<name> | shed:<name>[@<server>]`, plus the engine-compat
  `sx rc <subcommand>`) — and the `tests/rc-parity` Go↔Rust differential harness that
  pins its wire compatibility **ship no release component**: they are unreleased dev
  tooling built from `crates/`, and giving `sx` a release component is future work.
  `shed-machine-rc` is unaffected as a product: it remains the machine hub provider
  (the hub is deliberately not ported) and the parity oracle. See
  `docs/extensions/sx.md`.

## v0.8.1 — 2026-07-28

**Ships:** server, host-agent, machine-rc, desktop

### Added

- **`auth.mode: mtls` — internet-exposable posture with key-bound client
  certificates.** A third auth mode alongside `open` and `token`: the client
  credential is a short-lived certificate bound to a private key that never
  leaves the client's device, issued over the same already-authenticated
  `_bootstrap` SSH channel that mints bearer tokens in `token` mode. The
  server's HTTPS listener runs `RequireAndVerifyClientCert` against a small
  internal CA — an unauthenticated peer can never send an HTTP byte or reach
  the router (live-verified: `curl -k` with no client cert gets no HTTP
  status at all). mtls is now the **recommended posture for anything
  internet-facing**; `token` is retained, unchanged, and not deprecated,
  because `curl`/scripts/CI/third-party callers can't present a client
  certificate. mtls inherits every `token`-mode invariant (SSH enforce, key
  source at preflight, TLS-only, `https_port` default 8443, non-loopback
  bind needing no acknowledgment) and re-validates the certificate's
  expiry/allowlist-membership/scope on **every request**, not just at the
  TLS handshake, giving it the same revocation-lands-on-the-next-request
  property tokens have always had. Revocation is coarser than token mode's
  per-token `RevokeBySubject`: it means removing the SSH key, which also
  cuts shell/SFTP access — a deliberate, documented tradeoff. An
  already-established SSE stream or `shed forward` tunnel survives
  revocation/expiry until it closes, identical to token-mode parity.
- **`shed server add` is SSH-first for every mode.** The old HTTP TOFU probe
  of `/api/info` survives only as the `open`-mode fallback; every other mode
  goes SSH-first (`--ssh-port`, default `2222`), confirms the host key, runs
  `_bootstrap`, and provisions whatever credential shape the returned bundle
  carries (bearer token or client certificate). **Behavior change:** adding
  against a `token`/`mtls` server now requires an allowlisted SSH key at add
  time, not just lazily on first API call.
- **Adaptive credential transport, both directions, zero manual steps.**
  Every secure-mode HTTP client (Go and the shared Rust core) is built once
  with both a dynamic client-cert resolver and bearer-header injection
  always installed; a mode flip on the server (`token` ⟷ `mtls`) or ordinary
  renewal is a pure credential-state change inside the provider — the
  underlying `http.Transport`/`reqwest::Client` is never rebuilt. An
  existing client entry silently re-bootstraps on its next command and
  migrates to whatever mode the server now reports, live-verified in both
  directions with zero manual steps.
- **Host-agent (both Go and Rust) hold their own credentials-scope
  certificate** on both the credential bus and egress transports when
  talking to an mtls server, mirroring the CLI's control-scope credential.
  The desktop↔host-agent UDS protocol gains a new mode-agnostic
  `credential.get` message plus a `hello_ack` capability advertisement, with
  explicit "upgrade shed-host-agent" / "upgrade the app" errors on either
  side of a version mismatch — desktop users upgrading to mTLS should
  upgrade `shed-host-agent` before or alongside the app (separate release
  selectors; see the upgrade guide below).
- **Desktop mtls support across all three client shapes.** The Swift macOS
  app (Rust-core path; the legacy `URLSession` path fails fast with an
  explicit error instead of attempting mTLS) and the Tauri Linux app's three
  broker modes (external agent, embedded broker, headless-coexist) all mint
  and drive an mtls server, relaying only the CSR — never the private
  key — across the host-agent UDS boundary. The app persists no credential;
  a cold launch re-mints in memory.
- **Docs.** [Security](https://charliek.github.io/shed/reference/security/#mtls-mode),
  [Security Configuration](https://charliek.github.io/shed/guides/security-configuration/),
  [Configuration](https://charliek.github.io/shed/reference/configuration/),
  [API reference](https://charliek.github.io/shed/reference/api/), and
  [Public VPS Deployment](https://charliek.github.io/shed/guides/vps-deployment/)
  all cover the mtls posture end-to-end, including the precise (not
  overclaimed) exposure guarantee, per-request re-validation, revocation
  coarseness, and the accepted limitations. A new [Upgrading to
  mTLS](https://charliek.github.io/shed/upgrades/token-to-mtls/) guide covers
  the client-then-server rollout order, the SSH-first `shed server add`
  behavior change, the desktop component-upgrade ordering, and the
  fleet-wide-but-self-healing CA rotation story.

### Removed

- **The Go `shed-host-agent` is retired.** The shipped binary has been the
  Rust `crates/shed-host-agent` since the GoReleaser `builder: rust` swap;
  the Go twin under `cmd/shed-host-agent` survived only as rollback
  insurance and as one half of the Go-vs-Rust differential test harness.
  Both are gone, along with the two detached Go build ids, the
  release-snapshot assertions that pinned them, and `make build`'s
  `build-host-agent` target. **Install identity is unchanged** — same
  formula, same binary and archive names, same flags, env, and
  `status`/`version` surface; nothing about upgrading or running the agent
  changes. What does change is the rollback path: source-level rollback
  ends here, so falling back now means reinstalling the previous release's
  Homebrew formula. The harness kept its coverage rather than losing it —
  every cell's canonical wire output was recorded as a committed golden
  **while both implementations still agreed** (verified on macOS and Linux
  before the freeze), so it now pins the Rust daemon against those frozen
  values; the cross-language fixture vectors and their Rust/Swift consumers
  are untouched.

### Fixed

- **The desktop apps now lead with the remedy when `shed-host-agent` is too
  old to obtain a certificate.** This is the unattended Sparkle-update
  state — the app auto-updates, the Homebrew agent doesn't — and both
  clients mishandled it: the macOS app truncated the actionable clause off
  the end of the banner, leaked a raw `Config(message: "…")` enum wrapper
  into user-facing text, and then contradicted itself with a "check
  `~/.shed/config.yaml` and that shed-server is running" empty state
  pointing at two things that were fine; the Tauri app dropped failed hosts
  from its Sheds pane entirely, so the only symptom was an empty list with
  no explanation. The failure is now a typed case end to end: a short
  remedy-first sentence in the banner, transport detail demoted to the
  tooltip and log, an empty state that names the real cause, per-host error
  rows on the Tauri Sheds pane, and host tooltips on both.
- **A mint attempted before the host agent has announced its capabilities
  now says so.** It previously surfaced as a generic
  `credential resolution timeout` — the request's outer timeout cancelling
  the mint before the specific, actionable error could be produced — which
  read as a network problem rather than "the agent is wedged or still
  starting". Both the Rust and Swift minters now wait a bounded moment for
  the agent's handshake, in every auth mode, and report the actual
  condition; a burst of concurrent requests pays that wait once.
- **`shed --server B <cmd> <shed>` no longer operates on a different
  server.** `--server` was consulted only when the shed name missed the
  local location cache, so an explicit flag could be silently overridden
  for `delete`/`reset`/`stop`. An explicit `--server` now wins
  unconditionally; the cache still serves the unqualified form.
- **Concurrent `shed` processes no longer clobber each other's
  `~/.shed/config.yaml`.** Each process loaded the whole config, mutated
  its own entry, and wrote the entire stale snapshot back, so a credential
  persist racing any other config write could silently lose its recorded
  certificate paths and re-mint over SSH on every later command. Config
  writes now take a cross-process lock and re-read under it before
  mutating, credential persists verify the entry still exists and still
  points at the endpoint they minted against, and the temp file used for
  the atomic rename is unique per write.
- **`GET /api/info`'s `auth_mode` field keeps reporting the legacy `"secure"`
  spelling for token mode on the wire**, so already-released clients (which
  gate their bootstrap decision on that exact string) keep working unchanged
  against an upgraded server; current clients normalize `"secure"` back to
  `"token"` on decode. `open` and `mtls` are unaffected (`open` predates the
  rename; an mtls-mode `/api/info` is certificate-gated, so no pre-rename
  client can ever observe it).
- Several pre-existing bugs surfaced by this work, unrelated to mtls itself:
  `shed server rm ..` resolved (via `url.PathEscape` not escaping `..`) to
  `~/.shed` itself, risking deleting the entire client config directory;
  IPv6 server URLs were built by string concatenation instead of
  `net.JoinHostPort`; and `sdk.WithClientCertificates` unconditionally
  suppressed the bearer header the moment a certificate provider was wired,
  even while the provider held nothing — breaking token-mode authentication
  for any SDK consumer with a certificate provider configured.

## v0.8.0 — 2026-07-18

**Ships:** server, host-agent, machine-rc, desktop

### Added

- **Tauri desktop app embeds the credential broker — no separate `shed-host-agent`
  install needed.** The Linux client and the macOS Tauri beta can now broker SSH/AWS/
  Docker credential approvals **in-process**, so a default install is `brew install shed`
  (or `apt install shed`) plus the desktop app, full stop. A new `Preferences →
  Credential broker` setting (`Automatic` / `In-app (embedded)` / `External daemon`,
  default `Automatic`) picks the mode at launch: a running daemon ⇒ dial it as before
  (`external`, zero change for existing daemon users); a *headless* daemon (no desktop
  socket) ⇒ `headless-coexist` (mint-only, no in-app approvals, no namespace fights); no
  daemon ⇒ `embedded`. The mode is fixed for the process lifetime — changing the pref
  applies on the next launch, surfaced as `restart_required` in `broker.status` /
  `identify.broker_mode`. The embedded broker honors an existing
  `~/.config/shed/extensions.yaml` identically to the daemon, or synthesizes a working
  fresh-install default when none exists (all configured servers, SSH auto-detect routed
  to in-app approval); a malformed file fails the broker closed without taking the app
  down, surfaced in Preferences. The Swift macOS app is unchanged — it still uses the
  standalone daemon. Under the hood, the daemon's broker logic moved into a new library
  crate (`crates/shed-broker`), shared by the standalone `shed-host-agent` binary (now a
  thin shell around it — wire-identical, unchanged CLI/config/socket behavior) and the
  desktop app's new embedded path.
- **Multi-agent Remote Control sessions.** RC sessions (the detached `rc-<slug>` tmux
  sessions driven by `shed-ext-rc`) now run **codex**, **cursor**, and **opencode**
  alongside `claude-rc`/`claude-broker`/`shell`, each with its own pane classifier
  (`starting`/`ready`/`needs-auth`/`needs-trust`/`dead`). `shed attach --kind` accepts
  every kind and `--plan` ships a plan to all non-broker kinds. A generic permission
  tri-state — `default` \| `auto` \| `skip` — is accepted by every kind and mapped per
  agent to its real flags; the Claude kinds keep their full historical
  `--permission-mode` set. The `full` image installs all four agents (cursor-agent added;
  the codex runtime shim fixed so it starts under bun).
- **Capability discovery replaces error-string sniffing.** `shed-ext-rc capabilities`
  (also embedded in the `list` envelope) reports `rc_version`, the offered kinds,
  per-agent install/version, a feature set (`generic-perm`, `plan-stdin`, `prompt-b64`),
  and per-kind UI hints. `rc_version` (capability/protocol version, now 3) is decoupled
  from `SHED_RC_V` (metadata schema, still 2). An unrecognized kind is preserved verbatim
  and rendered neutrally rather than aliased to `claude-broker`.
- **`shed plan <file> --shed <name>` porcelain.** Ships a plan and runs it autonomously in
  one command: creates the shed if missing (with `--repo`), writes the plan to a
  HOME-rooted location inside the shed (never the workspace), starts an agent session
  under the `auto` posture, and reports it. Exit is `0` only when the session reached
  `ready` and the kickoff was delivered; `needs-auth`/`needs-trust`/failed-ship cases exit
  non-zero, print per-agent remediation, and leave the session and shed in place (a shed
  auto-created for a run is never deleted on failure). Plan delivery moved into the rc
  core (`create --plan-stdin` / `--prompt-b64`), shared by every orchestrator.
- **Server-side RC enrichment (`#242`, `#199`).** `GET /api/sessions` and
  `GET /api/sheds/{n}/sessions` now populate an `rc` object per `rc-*` row server-side (by
  execing `shed-ext-rc list` over the agent channel, cached per shed with a bounded
  concurrency/timeout and a `warnings` entry on degradation); `?rc=0` opts out. HTTP-only
  clients no longer fan out one SSH connection per shed to read RC state.
- **`GET /api/overview` + feature discovery.** One call returns the server info (with a
  new `features` array, also on `GET /api/info`), disk usage, and every shed with its
  rc-enriched sessions and per-shed `rc_capabilities` — a phone renders a host's landing
  and sessions views from a single HTTPS request. Blocks degrade independently with
  `warnings`; `?rc=0` and `?fresh=1` control enrichment and capability re-probing.
- **Desktop multi-agent support.** The shared Rust core and both desktop UIs (Swift
  macOS, Tauri Linux) gain the new kinds and the generic permission mode, gate the create
  sheet's kind chips on each shed's capabilities, surface `needs-auth` per agent, and
  apply the unknown-kind neutral-rendering policy.
- **Live RC activity (the rc hub).** `shed-ext-rc serve` runs a resident, on-demand,
  self-exiting per-shed daemon (loopback `127.0.0.1:1029`) that tails codex rollout and
  claude transcript JSONL — with a spinner-normalizing pane-stability engine as the
  universal fallback for every kind — to derive a live `activity` dimension
  (`working`/`needs_input`/`idle`/`unknown`) plus `activity_at` and a sanitized
  `last_message`, additive inside each session's `rc` block. Lifecycle trumps activity: a
  `needs-trust`/`needs-auth`/`dead` session reports none. `capabilities.features` gains
  `serve`, `activity`, and `messages`. Session listings enrich activity by consulting an
  already-running hub (~200 ms budget, never starting one), with instant fallback to the
  hub-less behavior.
- **Codex message feed + gated input.** The hub folds the codex rollout stream into a
  normalized, bounded per-session message ring served by
  `GET /v1/sessions/{slug}/messages` (`since`/`limit`, `truncated` on cursor
  misalignment), notified by a `message.appended` SSE event, and accepts
  `POST /v1/sessions/{slug}/input` gated on a fresh re-derivation of the session state
  (`kind_features` gains `watch` and `input: "gated"`, codex-only in this phase).
- **Server rc proxy + aggregate SSE.** `GET/POST /api/sheds/{name}/rc/*` reverse-proxies
  the hub's `/v1` API into a shed over `DialService` with a strict method/path allowlist,
  SSE flushing, header/body bounds, ensure-start (singleflight + circuit breaker), and
  credential-stripping — the server is the authorization boundary for the loopback-only
  hub. `GET /api/rc/events` is a demand-driven aggregate activity SSE stream across every
  shed (zero clients ⇒ zero upstreams). Both are advertised as the `rc-proxy` and
  `rc-events` feature tokens. `DialService` routes through the guest agent's vsock TCP
  proxy on **both** VZ and Firecracker, so the loopback hub is reachable on either
  backend; the proxy degrades to `503 RC_HUB_UNAVAILABLE` (and listings carry no activity
  fields) only when the hub is genuinely down or the image predates it. On Firecracker
  this also means `connect/{port}` now reaches loopback-bound guest services — parity with
  VZ — instead of dialing the VM's bridge IP (the peer address a guest service sees
  becomes `127.0.0.1`).
- **Tauri macOS app gains Sparkle auto-update.** The Tauri client now packages a macOS
  DMG (`ShedDesktop-<version>.dmg`) containing a signed `ShedDesktop.app` with an
  embedded `Sparkle.framework`, wired to
  the **same feed and EdDSA keys as the Swift app**
  (`https://charliek.github.io/shed/appcast.xml`). Updates are **user-invoked only** — the
  tray popover's **Check for Updates…** row is now live (no automatic background checks;
  Swift parity) — and the macOS bundle identity is aligned to `ai.stridelabs.ShedDesktop`
  so the eventual Swift→Tauri hop is a same-key, in-place update. On Linux the row stays a
  truthful "Updates arrive via apt" tooltip. Drivable over IPC via `updater.status` /
  `updater.check`.
- **Tauri mac DMG packaging + prerelease-tag release job.** New `make tauri-dmg-mac`
  packaging (Sparkle staged by `scripts/fetch-sparkle.sh`, signed in Sparkle's ordered
  inner→outer sequence) and a `desktop-macos-tauri` release job: **prerelease** desktop
  tags (`vX.Y.Z-rc.N`) build and — when Apple signing credentials are configured —
  notarize the Tauri DMG and append a **beta-channel** appcast
  entry, while stable tags keep shipping the Swift DMG (mutually exclusive gates). This is
  the beta rollout track — stable users are untouched until promotion.
- **Sheds dashboard auto-refresh + a Sheds Refresh button.** The Tauri client now polls
  the shed list every 5s — gated on the native window being visible (not WebKit occlusion),
  and refreshing immediately on regaining foreground — so a shed created out-of-band
  (`shed create` from the CLI, or another client) appears on an already-open dashboard with
  no manual action, matching the Swift host-poller. A `RefreshCw` **Refresh** button now
  sits in the Sheds header (shared markup with the Agents pane), and the dead "+ Add" hosts
  link (it only reopened the System pane) was removed. (#276)
- **macOS Tahoe glass app icon.** On macOS 26 a loose `.icns` is treated as legacy and inset
  on the system glass tile; the Tauri bundle now ships an Icon Composer catalog (orange as a
  native `srgb` fill + the owl as a translucent foreground layer, translucency tuned to 0.35)
  compiled by `actool` into `Assets.car` with `CFBundleIconName=AppIcon`, so the Dock icon
  renders edge-to-edge glass. Falls back gracefully to the flat `.icns` when `actool` is
  absent; flat PNG/`.ico` and Linux `.deb` hicolor outputs are unchanged. (#276)
- **Shared Rust client core gains the mobile client surface (Phase A).** `shed-core` and
  `shed-app` add — additively, library-only — the read-plane and RC surface the shed-mobile
  Flutter app uses, ported field-for-field from the Dart sources so Swift (UniFFI) and Tauri
  (and a later flutter_rust_bridge layer) consume one implementation: the `Overview` DTOs +
  `overview()` (`GET /api/overview`) with Dart-tolerant per-field decode; `list_sessions()`/
  `delete_session()`/`rc_messages()`/`rc_input()` and their DTOs; an `RcEvent` never-error
  decode with an `ActivityOverlay` fold plus a reconnecting `RcEventsWatcher` (byte-level
  idle timer, 500ms→30s backoff); `ServerInfo.features` + `has_feature()`; token-provider
  FSM knobs (`with_now`/`with_refresh_window`/`with_mint_cooldown`/…) with a stale-401
  `invalidate_if_current` guard; fail-closed token-bundle parsing; and rc permission modes.
  Three strictly-safer desktop changes ride along: keep a still-valid cached control token on
  a proactive-mint failure, the stale-401 invalidation guard, and one-segment URL encoding of
  identifiers that closes a path-traversal class (byte-identical requests for valid names).
  Also improves peer-ID detection portability toward Android. No manifest bump or FFI change
  in this PR. (#277)

### Changed

- **The monorepo now ships four independently selected release components.** A `vX.Y.Z` tag
  ships only the components whose version manifest equals the tag — `server`
  (`.claude-plugin/plugin.json`), `host-agent` (`crates/shed-host-agent/VERSION`),
  `machine-rc` (`cmd/shed-machine-rc/VERSION`), and `desktop` (`desktop/VERSION`) — so a
  one-line `shed-host-agent` fix no longer forces a full server + rootfs-image republish, and
  `shed-host-agent`/`shed-machine-rc` can now rev independently of server releases. Per-component
  goreleaser configs (`.goreleaser.{server,host-agent,machine-rc}.yaml`) replace the monolithic
  `.goreleaser.yaml`, so an unshipped component produces no release assets — load-bearing because
  apt-charliek indexes by scanning release assets (unshipped = unbuilt = no asset). A new
  `scripts/release/recommend-components.sh <X.Y.Z>` recommends the component set (minor/major →
  all; patch → changed-since-last-shipped), and each CHANGELOG entry's `**Ships:**` line is now
  enforced by `scripts/release/release-plan.sh` against the manifest-computed ship set on stable
  tags. **v0.8.0 is the first release cut under this model, and ships all four components.** (#278)

## v0.7.10 — 2026-07-08

**Ships:** server/CLI, desktop

### Added
- **Shed Desktop now ships from the monorepo.** The macOS menu-bar app and the
  Tauri Linux client (built on the shared `crates/` Rust core) are released from
  `charliek/shed` on the shared version line for the first time, jumping from the
  standalone repo's `v0.0.x` track to `v0.7.10`. macOS updates flow through Sparkle
  at the new feed (`https://charliek.github.io/shed/appcast.xml`); Linux installs via
  `apt install shed-desktop`. Existing `v0.0.13` installs migrate automatically via
  the final `charliek/shed-desktop` `v0.0.14` release, which repointed the update
  feed. (#248)

## v0.7.9 — 2026-07-08

### Changed

- **shed-extensions now builds from this repo (monorepo Phase 1).** The host
  binaries (`shed-host-agent`, `shed-machine-rc`) and the four guest extension
  binaries (`shed-ext-ssh-agent`, `shed-ext-aws-credentials`,
  `docker-credential-shed`, `shed-ext-rc`) are built in-tree; the rootfs image
  builds stage the guest binaries directly, replacing the
  `ghcr.io/charliek/shed-extensions` image and the `SHED_EXT_VERSION`
  Dockerfile pins — a bus-protocol change is now one PR, one tag, no
  version-skew window. The `shed-host-agent` and `shed-machine-rc` brew
  formulas and the `shed-machine-rc` deb publish from this repo's releases
  (version numbers jump from the extensions repo's v0.4.9 to this line), and
  the release job runs on macOS for the Touch ID CGO build. First release cut
  from the consolidated pipeline. (#243)

## v0.7.8 — 2026-07-03

### Fixed

- **Guest clock stays correct across host sleep.** A long-running guest shipped no
  time synchronization, so when the host slept the VZ guest's wall clock froze
  behind real time and was never re-synced on wake — breaking AWS SigV4/STS
  signing, TLS validity windows, and token expiry (observed ~3 h behind). Guest
  images now ship `systemd-timesyncd`, and `shed-agent` periodically steps the
  clock forward from the host-backed RTC (VZ; forward-only, gated on a large gap,
  bounded to a plausible window, and a clean no-op where no RTC exists).
  Firecracker has no RTC and its host doesn't sleep, so `timesyncd` covers it.
  Takes effect once you pull a rebuilt image. (#240)
- **`shed forward` works against secure-mode servers.** In secure mode the Connect
  route required the credentials scope, so the CLI's control token was rejected
  (403) and every `shed forward` connection reset. The route now accepts either
  the control scope (CLI) or credentials (host-agent reverse proxy); the credential
  bus stays credentials-only. (#238)
- **Background tunnels survive the token TTL on secure-mode servers.** The egress
  audit stream (`GET /api/egress/stream`) now accepts control or credentials
  (GET-only), and the CLI + tunnels share a self-refreshing bearer-token source
  that re-mints before expiry — so long-lived background tunnels no longer drop
  when the token TTL lapses. (#239)

## v0.7.7 — 2026-07-01

### Fixed

- **Zed Remote-SSH connects cleanly over the non-PTY exec channel.** The in-VM
  agent no longer folds the command's stderr into stdout, so Zed's
  length-prefixed protobuf readiness frame on stdout stays byte-clean while its
  JSON logs stay on stderr — completing the Zed fix started in #222/#225. (#230)
- **`shed exec` output stays 8-bit-clean when captured or piped.** `shed exec`
  now auto-detects whether stdin is a TTY instead of always requesting a remote
  PTY, so the PTY's `ONLCR` line discipline can't rewrite `\n`→`\r\n` or mangle
  control bytes when you pull binary data out of a shed. Interactive
  `shed console` still gets a PTY. (#233)
- **`shed delete` of a running shed is fast and streams progress.** Delete now
  takes a destroy path that SIGKILLs the VM immediately (it always discards the
  writable upper anyway) instead of running a ~30 s graceful guest shutdown, so
  a running-shed delete drops from ~30 s to ~1–2 s and no longer times out the
  client. Progress streams over SSE (`Terminating virtual machine… → Removing
  volume…`), and writable host-backed mounts (`--local-dir`/`--add-dir` and
  configured server `mounts:`) are still synced before the kill. Use `shed stop`
  for a graceful, restartable stop. Removes the no-op `--keep-volume` flag. (#234)

### Changed

- **Internal: the SSE streaming machinery is shared across create, delete, and
  image pull.** `handlePullImageSSE` now delegates to the same `streamSSE`
  pump/drain/terminal helper as create and delete (no wire-behavior change), so a
  future SSE fix lands in one place instead of three. (#235)

## v0.7.6 — 2026-06-30

### Added

- **Bootstrap secure-mode tokens via the system `ssh` client (`sdk/bootstrap`).**
  The host-agent now mints its `_bootstrap` token by invoking the system `ssh`
  instead of an in-process client, so the exchange honors the user's SSH agent,
  macOS Keychain, `IdentityAgent`, hardware keys, and `~/.ssh/config` — fixing
  bootstrap when the developer's key is passphrase-protected or agent-only
  (1Password, Secretive, hardware keys). The shed `known_hosts` stays the sole
  host-key trust root. (#226)
- **Sane tmux defaults baked into the VZ + Firecracker base images.**

### Changed

- **Bump the baked `shed-extensions` to v0.4.9** in both base images, so the
  `extensions`/`full` variants layer the latest published guest binaries. (#229)

### Fixed

- **Zed Remote-SSH (and other raw-SSH clients) can connect again.** The in-VM
  agent now uses blocking stdin reads on the exec channel, ending the
  binary-protocol desync that broke Zed; exec-channel teardown is also reliable
  on host disconnect (process-group SIGHUP, no orphaned commands or leaked
  handler goroutines). (#222, #225)
- **`shed tunnels start -d` truly daemonizes**, returning the terminal, and
  Ctrl+C / SIGTERM now stops tunnels gracefully instead of hanging on open
  tunneled connections. (#223, #224)
- **`shed image build` records the ref-index**, so `shed create --image <ref>`
  resolves a freshly built image instead of a stale pull; and the agent install
  layer is content-hash cache-busted (`SHED_INSTALL_SHA`) so a rebuilt agent
  can't be silently served stale from BuildKit's bind-mount cache. Fixes the
  dev-image rebuild loop — no more manual `docker buildx prune` or ref-index
  edits. (#227, #228)

## v0.7.5 — 2026-06-26

### Added

- **Run a plan autonomously on a shed (`shed attach` Remote Control mode + the
  `shed-plan` skill).** `shed attach <shed> --plan <file> -d` ships a plan into a shed
  and starts a Claude Remote Control session that executes it unattended, printing a
  `claude.ai/code` URL to watch/steer (the laptop can close). The plan is shipped to
  Claude's plans directory (`~/.claude/plans/plan-<slug>.md`, outside the workspace) and
  the kickoff references it; `--plan` and `-p` combine (generic kickoff by default, your
  prompt + appended plan path when given). `--kind`/`--prompt`/
  `--prompt-file`/`--edit`/`--plan-edit` cover session kind and kickoff-prompt sources;
  `--permission-mode` (default `auto`) and `--skip` (full bypass) set the autonomy
  posture; `--slug` connects to an existing `rc-<slug>`. Plain `shed attach` is
  unchanged. Requires Claude to be logged in inside the shed. The new `shed-plan`
  skill drives the end-to-end author → fresh shed → run → report flow. Supersedes the
  HTTP-enumeration approach in #199 (the same metadata is surfaced over SSH).
- **`shed sessions` surfaces RC metadata.** `rc-*` sessions show `KIND` and `RC-STATE`
  columns (and an `rc` object in `--json`), read from the in-shed `shed-ext-rc` binary;
  degrades silently when a shed is unreachable or lacks the binary.
- **Multi-line kickoff prompts.** `shed attach --prompt-file`/`--edit` may now be
  multi-line; `shed-ext-rc` delivers them as one input via a bracketed paste (prefer a
  plan file for large/multi-step work).
- **User-managed egress profiles (runtime, no server-config edit).** A second,
  runtime-editable profile store alongside `server.yaml`: `shed egress profile
  set <name> --file <doc>` / `edit` / `ls` / `show` / `rm`, backed by
  `GET/PUT/DELETE /api/egress/profiles[/{name}]` (control-scoped). User profiles
  are referenced by name exactly like config profiles (`--egress
  github,my-stack`). Server-config profiles stay a **read-only baseline** (a
  collision is rejected; `ls` tags each `source: config|user`); **editing a
  profile live-re-pushes every running shed that references it**; deleting a
  referenced profile is rejected. Profiles persist under
  `{state}/egress-profiles/`. See
  [Egress Control](docs/reference/egress.md#user-managed-profiles) (shed #214).

### Changed

- **Base images bump shed-extensions to v0.4.7.** `vz/Dockerfile` +
  `firecracker/Dockerfile` pin `SHED_EXT_VERSION=v0.4.7`, so the `extensions`/`full`
  rootfs images ship the `shed-ext-rc` with `--permission-mode`/`--skip`, onboarding +
  interstitial pre-seeding, and multi-line prompt delivery — i.e. `shed attach --plan`
  runs autonomously on these images (older images feature-detect and degrade).

### Fixed

- **`shed-server` postinstall uses an absolute path for config-validate.** The deb
  postinst invoked `shed-server config-validate` by bare name; it now uses the absolute
  install path so validation runs regardless of `PATH` at install time (shed #213).
- **`shed egress show --json` emits snake_case keys.** The `rules` map's profile
  definitions serialized with Go field names (`Mode`/`Allow`/`Deny`/`Rule`, plus a
  noisy `"Deny":null`) because `config.EgressProfile` lacked JSON tags —
  inconsistent with the rest of the API (`shed`, `resolved_ip`, …) and the YAML
  config keys. They are now `mode`/`allow`/`deny`/`rule` with `omitempty`. A minor
  `--json` **output-shape change**: no first-party consumer parsed the old shape,
  and Go clients interop across it (`encoding/json` matches field names
  case-insensitively).

### Docs

- **Egress: quickstart + recommended starter profiles.** `docs/reference/egress.md`
  gains a 3-step quickstart and a "Recommended starter profiles" section
  (`ai-agents`, `github`, `package-registries`, `os-updates`, `containers`, sourced
  from real-world sandbox allowlists) with apex-vs-wildcard guidance; a matching
  commented `egress:` block lands in `configs/server.example.yaml` (guarded by a
  validation test). The "plain HTTP is deny-by-default" caveat is corrected — plain
  HTTP uses the same allow/deny/rule chain as HTTPS.

## v0.7.4 — 2026-06-19

Local-first, zero-plaintext-secure network surface — finishes the `0.7` auth
line: the credential bus loses its plaintext escape hatch, binding is one knob
that defaults to loopback, and `http_port` is no longer required in secure mode.
A `0.7.x` patch that carries **breaking config changes** — see the
[upgrade guide](docs/upgrades/v0.7.3-to-v0.7.4.md).

### Changed

- **One listener per mode — `internal_http_port` removed.** The credential bus
  (`/api/plugins/*`) and Connect tunnel (`/api/sheds/*/connect/*`) now ride the
  single listener (pinned TLS in secure mode, plain HTTP in open), gated by the
  `credentials` scope — so secure mode has no plaintext listener anywhere. Remote
  `shed forward` works uniformly again (the old split forced Connect to loopback).
- **`http_bind`/`ssh_bind` unified into `bind_address`, defaulting to loopback.**
  One interface governs every listener (HTTP, HTTPS, SSH) and **defaults to
  127.0.0.1 in both modes** — shed is local-development-first. Bind a routable
  address (`0.0.0.0`/`*`, `::`, or a LAN/tailnet IP) to expose it; in open mode a
  non-loopback bind also requires the new `allow_insecure_exposure: true` ack
  (secure mode needs none). A startup `WARNING` flags the new loopback default.
- **`http_port` is optional in secure mode** (no plain HTTP served there); it is
  omitted from `/api/info` and the written client entry. Open mode still requires it.
- **`bind_address` is format-validated** — a malformed value (hostname, typo,
  zoned IPv6) is rejected at `config-validate` time with a clear message rather
  than failing cryptically at `net.Listen` on startup.

### Breaking

- `internal_http_port`, `http_bind`, and `ssh_bind` are **rejected at startup**
  (the first removed; the binds renamed to `bind_address`).
- With no `bind_address` set, every listener now binds **loopback only** (was all
  interfaces) — a networked server becomes local-only until you set `bind_address`.
  The deb postinstall warns on upgrade; other channels surface it via a startup
  `WARNING`. Migration + recovery: [upgrade guide](docs/upgrades/v0.7.3-to-v0.7.4.md).

### Packaging

- Fresh brew/apt configs ship `bind_address: 127.0.0.1` with inline guidance for
  exposing off-box (secure mode, or open + `allow_insecure_exposure`).
- The `full` rootfs image now bundles the `shed-ext-rc` guest binary
  (shed-extensions v0.4.6).

## v0.7.3 — 2026-06-18

CLI/API polish so `auth.mode: secure` (TLS-only) servers are first-class in the
`shed` client. Additive and backward-compatible — no config or behavior change
for existing deployments.

### Added

- **`/api/info` reports `https_port`** so a client can discover a secure server's
  TLS endpoint (omitted in open mode; older clients decode it as zero — no break).
- **`shed server list` shows the real control-plane `ENDPOINT` + a `SECURITY`
  column** (`secure` / `open` / `unpinned`) instead of the stale `HTTP`/`8080`
  column that a TLS-only secure server never serves. `--json` gains `endpoint`,
  `https_port`, `security`, `tls_pinned`, and the pinned cert fingerprint (never
  tokens). Derived from local config — no live probe, so STATUS is unchanged.
- **`shed server add` auto-discovers a secure server's TLS port.** With no
  `--https-port`, a refused plain-HTTP probe auto-retries the pinned-TLS
  bootstrap on the default secure port (`8443`); a new `--secure` flag skips the
  plain probe entirely. The trust gate (interactive prompt / `--trust-on-first-use`
  / `--tls-fingerprint`; never silent-trust under `--json`) is unchanged.

See [#209](https://github.com/charliek/shed/pull/209).

## v0.7.2 — 2026-06-16

Auth-surface simplification: the `auth.mode: open | secure` headline is
unchanged, but the intermediate states beneath it are removed so two invariants
always hold — **tokens ⟺ TLS ⟺ secure** and **https ⟺ secure**. **Breaking** (a
`0.7.x` patch; the auth interface is still settling) — but most LAN / Tailscale
deployments, and plain `auth.mode: secure` + `github_users` configs, are
unaffected.

### Changed

- **Secure mode is now TLS-only.** It no longer serves a plain-HTTP listener
  (not even on loopback) — only the pinned-TLS `https_port` listener faces
  clients. On-box tooling uses the HTTPS endpoint; `shed server add` against a
  secure server needs `--https-port`. A loopback credential-bus channel remains
  available only via the explicit, opt-in `internal_http_port` (unchanged).
- **Bundled shed-extensions bumped to v0.4.3** — the in-VM host-agent now mints a
  credentials token only for secure (`https`) servers, so the `extensions`/`full`
  image variants no longer log the spurious "token provider returned no token"
  WARN against open-mode servers.

### Removed (rejected at startup)

- **`auth.http.mode`** (and the whole `auth.http` block) — HTTP token
  enforcement now derives purely from `auth.mode: secure`.
- **`https_port` under `auth.mode: open`** — `https_port` requires secure mode
  (this is what lets a client treat an `https` `api_url` as proof of secure).
- **`auth.ssh.mode: enforce` under `auth.mode: open`** — use `secure`, or `warn`
  to stage an allowlist on a trusted network.
- An explicit **`auth.ssh.mode: off`/`warn` under `auth.mode: secure`** — secure
  forces `enforce`.

See [Upgrading v0.7.1 → v0.7.2](docs/upgrades/v0.7.1-to-v0.7.2.md).

## v0.7.1 — 2026-06-15

Secure-by-default authentication that makes a public-VPS shed server safe to
deploy with a single `shed server add` — no token paste, no manual pinning.
Plus a guest-MTU fix for reduced-MTU / VPN paths and opt-in (off-by-default)
egress control. **One breaking change** — two removed config keys now hard-fail
at startup — but most LAN / Tailscale deployments are unaffected.

### Secure-by-default authentication (#207)

A redesign of how the server issues and tracks API tokens. A new binary
`auth.mode: open | secure` (default `open`) replaces the per-layer opt-in knobs.

- **Tokens are minted over SSH, not configured.** In `secure` mode `shed server
  add` opens a reserved `_bootstrap` SSH channel (authenticated by your
  allowlisted key against the pinned host key) and mints a scoped **control**
  token automatically — written to client config, never printed. The host-agent
  self-mints a **credentials** token the same way; the desktop is issued a
  control token over the host-agent socket. No `shed-server token new`, no static
  token list, no copy-paste.
- **Opaque, server-tracked tokens.** Tokens are random opaque strings stored
  server-side as SHA-256 hashes with a scope + 24h TTL; clients refresh
  transparently near expiry and retry once on a 401. Revocation is automatic —
  removing a key from the allowlist invalidates the tokens minted for it.
- **`secure` derives the hardened posture** (SSH-allowlist enforce + HTTP-token
  enforce + pinned TLS on `:8443`) from one switch; `open` mode is byte-identical
  to v0.7.0's default for LAN / Tailscale.
- **Scopes narrowed to `control` + `credentials`** (the `admin` scope is gone).

  **⚠️ Breaking (removed config keys hard-fail).** `public_exposure` and
  `auth.http.tokens` were removed; a config that still sets either is **rejected
  at startup** with a migration message. Token issuance is now automatic, so
  `shed-server token` has no subcommands. See the
  [v0.7.0 → v0.7.1 upgrade guide](upgrades/v0.7.0-to-v0.7.1.md) — most users
  (no `auth` block, or `auth.mode` unset) need no change.

### shed-extensions v0.4.2 (#207)

The `extensions`/`full` rootfs images bundle shed-extensions v0.4.2, whose
host-agent self-mints its `credentials` token over SSH (replacing the static
`credentials_token`) and serves the desktop's `token.get` request. Guest
binaries are unchanged, so no guest-side migration is needed.

### Guest MTU auto-detection for VPNs / reduced-MTU paths (#196)

Behind a VPN (or any host path with an MTU below 1500), full-size guest packets
were silently black-holed — `docker pull` failed with a TLS handshake timeout
even though `curl` to the same registry worked, because dockerd's larger
ClientHello exceeded the path MTU. shed-server now **detects the host's egress
path MTU when a shed starts** and lowers the guest's primary interface to match
(passed via a `shed.mtu=` kernel arg). On a normal 1500 path nothing changes, so
there is no penalty for the common no-VPN case.

- **Both backends** lower the guest MTU; on VZ an MSS-clamp rule additionally
  fixes Docker *container* egress behind a reduced-MTU path. (The Firecracker
  container-egress clamp is a fast-follow — its custom kernel lacks the
  `xt_TCPMSS` target.)
- New optional `vz.guest_mtu` / `firecracker.guest_mtu` config field (`0` =
  auto-detect; set `1280`–`1500` to pin a value when detection misses).
- Detection runs at VM start: toggling a VPN under a running shed needs
  `shed restart <name>` to re-detect.
- **Requires a rebuilt rootfs image** (the guest-side change ships in the image)
  — published with this release.

### Opt-in egress control (audit-first, Level 1) (#203)

See and optionally control what a shed's sandbox reaches on the network, for a
trusted team. **Off by default**; works on both backends.

- A new `shed-egress-proxy` child of shed-server (shipped in brew + deb)
  evaluates **composable named CEL profiles** (with `allow:`/`deny:` domain-glob
  sugar) per shed. Always-on deny-CIDR guards (IMDS/private/gateway, IPv6) are
  checked against **every** resolved address; the proxy resolves and **pins**
  the upstream IP (DNS-rebinding/metadata defense); CEL errors **fail closed**.
- `shed create --egress base,github`, plus `shed egress show|set|off` (live
  re-push + re-inject on a running shed). Per-shed listener port + token
  attribution; guest gets `HTTP(S)_PROXY` + an in-guest dockerd proxy drop-in.
- Durable JSONL audit + a recent-decision ring (`shed egress show`) + an SSE
  stream consumed by shed-host-agent → shed-desktop's view-only Egress feed.
- Configure with `egress.enabled` + `egress.profiles` in the server config; an
  invalid glob/CEL fails server start. **Honest posture:** Level 1 is
  cooperative (`HTTP_PROXY`-based), **not** a security boundary — see
  [Egress Control](docs/reference/egress.md) for the full bypass inventory.

## v0.7.0 — 2026-06-13

Optional, default-off authentication and transport encryption so a shed server
can be deployed on a public-internet VPS. Tailscale / LAN remains the unchanged
primary path — every new control is off by default, and existing deployments
are byte-identical until opted in.

### Optional auth + pinned TLS for public-VPS deployment (#198)

- **SSH key allowlist** — restrict the SSH control channel to specific public
  keys, optionally seeded from GitHub (`auth.ssh.github_users`), with a
  fail-closed last-known-good cache.
- **Scoped HTTP bearer tokens** — `control` / `credentials` / `admin` scopes
  minted with `shed-server token new --scope`, enforced deny-by-default when
  `auth.http.mode=enforce`. Bootstrap endpoints (`/api/info`,
  `/api/ssh-host-key`) stay exempt.
- **Native pinned TLS** — the server self-signs an ECDSA P-256 cert; clients
  pin it by SHA-256 fingerprint (`shed server add --https-port`), the same
  trust model as the SSH host key. No CA, ACME, or domain required.
- **Credential-bus hardening** — the plugin bus drops forged `/respond`
  envelopes and requires the `credentials` scope under enforcement.
- **`public_exposure` preflight** — refuses to start a public server unless the
  full bundle is present (SSH enforce + HTTP enforce + a strong token + TLS).
- **Network-surface controls** — `http_bind` / `ssh_bind` / `internal_http_port`
  / `trusted_proxy`, and an idle SSE keepalive for NAT survival.

See the new [Security reference](reference/security.md), the
[Public VPS deployment guide](guides/vps-deployment.md), and the
[v0.6 → v0.7 upgrade guide](upgrades/v0.6-to-v0.7.md).

### shed-extensions v0.4.1 (#200)

The `extensions`/`full` rootfs images bundle shed-extensions v0.4.1, whose
host-agent gains pinned-TLS + scoped-token support for the credential bus.
Guest binaries are unchanged from v0.4.0, so no guest-side migration is needed.

## v0.6.6 — 2026-06-12

Session env vars for the landing directory, a credential-brokering refresh
(shed-extensions v0.4.0), and a docs pass (host-agent quickstart + the
`${shed.version}` token).

### `SHED_WORKSPACE` / `SHED_ADD_DIRS` session env vars (#193)

`shed exec` and interactive logins now **export** the project landing directory
as `SHED_WORKSPACE` (the `--local-dir`/`--repo` project dir, or `/home/shed`)
and the colon-joined guest paths of any `--add-dir` mounts as `SHED_ADD_DIRS`.
The same values are mirrored to provisioning hooks.

### shed-extensions v0.4.0 (#197)

The `extensions`/`full` rootfs images now bundle shed-extensions v0.4.0 guest
binaries. Host-side, `shed-host-agent` gains an always-on, fixed-path credential
socket and a live `shed-host-agent status` that reports the config the running
agent actually loaded, plus opt-in AWS passthrough mode for SSO/SAML. The
bundled guest binaries' interface is unchanged, so no guest-side migration is
needed.

### Docs (#193, #195)

- The macOS quickstart drops the `desktop.enabled` / `socket_path` /
  `timeout_ms` config block — the approval channel is always on — and verifies
  setup with bare `shed-host-agent status`.
- Corrected the `${shed.version}` token docs (it expands only in shed config;
  published image tags and Dockerfile `FROM` lines use a concrete `:vX.Y.Z`),
  added an "Image references and `${shed.version}`" section, and slimmed the
  README.

## v0.6.5 — 2026-06-08

Docker's default `docker0` bridge now works on **both** backends, so plain
`docker run`, published ports, outbound NAT, and Testcontainers behave the same
on VZ and Firecracker — no per-project workarounds. Plus a fix for
`--local-dir` provisioning being silently skipped, and the `shed-extensions`
v0.3.7 credential-helper fix.

### Docker default bridge (docker0) on both backends (#191, #192)

The `full` image previously shipped `daemon.json` with `"bridge": "none"`,
disabling Docker's default `docker0` bridge — only `docker compose`
(user-defined networks) worked, so plain `docker run`, published ports, and
**Testcontainers** (the default network) got no IP and no port forwarding. Every
Testcontainers project had to carry a bridge-enabling workaround.

- **VZ (#191):** drop `"bridge": "none"` from `vz/daemon.json`. The vfkit guest
  kernel already has the netfilter/iptables NAT the bridge driver needs, and the
  image already sets `net.ipv4.ip_forward=1`.
- **Firecracker (#192):** the microVM's custom kernel shipped with `NF_TABLES`
  off, so dockerd couldn't create the `nat DOCKER` chain. The kernel-config
  fragment now asserts the `nf_tables` + `nft_compat` NAT path and the
  `x_tables` extensions Docker's rules use (all `=y` — a microVM has no loadable
  modules); `daemon.json` drops `"bridge": "none"` + `"iptables": false`; and
  the Dockerfile adds `net.ipv4.ip_forward=1` (the other half of outbound NAT,
  which FC was missing). Validated end-to-end on real FC hardware: clean boot
  with the bridge enabled, `docker0` present, published-port DNAT and outbound
  MASQUERADE both working.

### `--local-dir` provisioning no longer silently skipped (#190)

On a `--local-dir` shed, provisioning could be silently skipped (~1-in-6 on a
fast VZ host): `LoadConfig` read `.shed/provision.yaml` immediately after the
project mount, but the VirtioFS mount syscall returns before the host share is
coherent, so the read momentarily saw the file missing or empty and skipped all
hooks. `LoadConfig` now reads through a probe that distinguishes a genuinely
absent config from an unsettled mount and retries with backoff — a bare shed
with no config pays zero latency; a project landing dir retries until the mount
settles.

### shed-extensions v0.3.7 — credential-helper fix (#192)

Both images bump `SHED_EXT_VERSION` `v0.3.2 → v0.3.7`. The guest
`docker-credential-shed` helper now emits the standard `credentials not found
in native keychain` sentinel, so anonymous public image pulls work with
`credsStore=shed` intact (previously a misbehaving helper could block pulls).

### Docs (#189, #191, #192)

Refreshed the provisioning guide for the Docker-capable `full` image (the
docker-compose service pattern, an "installing language toolchains" section for
uv/bun/SDKMAN, and the `/etc/profile.d` / `/etc/environment.d` PATH-and-env
caveats for `shed exec`), added Gradle/TypeScript/Python provisioning tutorials,
and corrected stale image-variant claims (`base`/`extensions` don't ship Docker;
`full` ships Docker + mise + bun, not system Node/Python).

## v0.6.4 — 2026-06-07

Home-rooted workspaces: a shed's repo and mounted directories now live under
the home directory instead of the magic `/workspace` path, plus a new
`--add-dir` flag for mounting reference projects alongside a primary. The
server-config `credentials:` section is renamed to `mounts:`.

### Home-rooted workspaces (#188)

`/workspace` is removed (clean break). A bare shed lands in `/home/shed`;
`--repo` clones into `~/<reponame>` and lands there; `--local-dir` mounts a host
directory at `~/<basename>` and becomes the landing directory; the new
repeatable `--add-dir` (requires `--local-dir`) mounts additional host
directories at `~/<basename>` each as reference siblings. Two mounts can't share
a basename, and dotfile-style basenames are rejected. Interactive logins
(`console`, `attach`, raw `ssh`, VS Code Remote-SSH) and `shed exec` start in the
shed's landing directory. Project directories mount via VirtioFS (VZ) / 9P (FC),
each with a unique tag.

**Breaking:** pre-existing sheds created with `--local-dir` should be recreated —
their mount is not migrated and won't reattach on restart; other existing sheds
now land in `/home/shed` rather than `/workspace`.

### Config: `credentials:` renamed to `mounts:` (#188)

The server-config `credentials:` section is renamed to `mounts:` (identical
shape). The deprecated `credentials:` key still works as a fallback when
`mounts:` is absent; an explicit `mounts: {}` wins.

## v0.6.3 — 2026-06-06

Image refs that no longer need re-pinning each release; quieter,
self-observable host-agent reconnects; and a Firecracker networking fix.

### Image refs resolved from the server version (#187)

`default_image` / `image_aliases`, when unset, are now synthesized from a
release `shed-server`'s own version and resolved once at config load — so
`create`, image-prune reachability, and `/info` all see concrete refs,
without hand-editing pinned image tags on every upgrade.

### SDK: reconnect-log dedup + connection state (#186, #182)

The host-client SDK no longer logs a `WARN` on every reconnect attempt — a
persistently-down or namespace-conflicting server used to flood the log
(observed at 21 MB downstream in `shed-host-agent`). It now logs once on the
down-transition and `DEBUG` while it stays down. New `HostClient.Status()`
exposes per-namespace connection state (connected / reconnecting + reason),
so a host agent can report its own health without scraping logs.

### Firecracker: IP-conflict detection, netlink retry, TAP cleanup (#185)

Hardens the Firecracker network allocation path (Linux-only): passive
netlink-based IP-conflict detection in TAP/IP selection, netlink retry, and
TAP cleanup.

### Docs (#184)

Discovery docs cut to release shape.

## v0.6.2 — 2026-06-06

Image-pull overhaul: faster, clearer, and smaller. Pulls now download
blobs in parallel, render Docker-style live progress, and skip the layer
tarballs the host never boots from. **Non-breaking** — see
`docs/upgrades/v0.6.1-to-v0.6.2.md` for the one behavior change
(boot-only pulls).

### Boot-only image pulls (#179)

`shed image pull` and `shed create` now pull **boot-only** by default —
config + kernel + initrd + erofs, but not the OCI layer tarballs (the
host boots from the erofs and never reads them), cutting roughly 40% of
the download and on-disk size of a typical image (measured: VZ base
945→540 MB, FC base 539→333 MB). `--with-layers` pulls the full image or
hydrates a boot-only one; `shed image push` of a *pulled* image needs it
first (it fails a clear preflight, HTTP 409, otherwise). Building your
own image is unaffected — `shed image build` produces its layers locally.
`shed image ls` gains a `LAYERS` column (`full` / `boot-only`) and SIZE
now reflects real on-disk usage.

### Parallel blob downloads (#177)

Image blobs (layers + kernel/initrd/erofs) download concurrently, bounded
by a new `pull_concurrency` server config (default 3) — ~2× faster on a
high-latency link. Per-digest singleflight dedup; the first error cancels
the group; no tag advances on a partial pull.

### Docker-style live progress (#174, #175, #176, #178)

`shed image pull` and the pull leg of `shed create` render live per-blob
progress bars on an interactive terminal (size, percent, ✓), backed by a
structured byte-level progress wire format gated behind a `?progress=blob`
opt-in (so line-mode and older clients are byte-for-byte unchanged).
Piped / `--json` output is unchanged. Cached layers are now reported
("already present") instead of leaving gaps in the layer counter.

### Docs (#180, #181)

Image pull / build / push / on-disk architecture folded into the
[Images reference](docs/reference/images.md); new v0.6.1→v0.6.2
[upgrade guide](docs/upgrades/v0.6.1-to-v0.6.2.md).

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
