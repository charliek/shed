# Rust core (`shed-core`)

The shed-server protocol client — HTTP + SSE, control-token auth, TLS pinning,
and the wire DTOs — lives in a shared Rust core in the monorepo's top-level
`crates/` workspace, so every client runs the same logic instead of
re-implementing it per language. Its consumers:

- **the desktop app** — the Tauri client on macOS and Linux (`desktop/tauri/src-tauri`),
  which links `shed-core` + `shed-app` as path dependencies;
- **shed-mobile** — the phone client, which takes `shed-core` as a git dependency and calls
  it from Dart through flutter_rust_bridge (FRB);
- **`shed-host-agent`** — the credential-broker daemon, built on `shed-broker`, which itself
  builds on `shed-core`.

## Layout

A cargo workspace under `crates/`, every member a default-member. The client
core is `shed-core`, `shed-app` and `shedctl`, below; beside them sit the
credential broker (`shed-broker`) and its daemon (`shed-host-agent`), and the
two [agent-lane](agent-lanes.md) adapters, `shed-opencode` and `shed-craze`:

- **`shed-core`** — a *pure* Rust lib (no UI): wire DTOs + serde decoders
  (`models.rs`), the reqwest(rustls) client (`http.rs`), the SSE parser
  (`sse.rs`), leaf-cert pinning (`tls.rs`), and the control-token FSM
  (`token.rs`), plus a `config` parser, a pull-based `create` orchestration
  store, the approval policy model (`approval/`), the agent-lane contract
  (`lane.rs`) — and **`rc.rs`** (the pure Remote-Control **model**: `RcKind`,
  `RcSession`, `RcState`, `RcActivity`, `RcKindFeatures`, `RcCapabilities`,
  `RcAgentInfo`, `RcError` and the wire DTOs — types read by the lane crates,
  `roost::model`, and shed-mobile. Plan 022 (S6,
  [`charliek/shed#328`](https://github.com/charliek/shed/issues/328)) deleted
  everything in this file that *did* something — the `shed-ext-rc` argv
  builders, the permission-mode table, and the pane classifier — with the RC
  hub itself).
- **`shed-app`** — the UI-free app-logic layer (`Backend`, the approval
  `Coordinator`, `HostAgentClient`, `AuditStore`, the roost client), consumed by
  the Tauri client as a cross-workspace path dep. `broker` (the in-process
  host-agent-replacement bridge, backed by `shed-broker`) is its only
  non-default feature — the RC hub's `RcRunner` seam, `rc.rs` and `RcService`
  were deleted with the hub in plan 022 (S6); agent sessions are driven through
  `roost.rs` instead (see [Agent lanes](agent-lanes.md) and
  [IPC § Agent sessions](ipc.md#agent-sessions)).
- **`shedctl`** — a headless client for the desktop app's IPC socket, shipped in
  the Linux `.deb` alongside `shed-desktop`. It depends on no other crate here
  (only `serde_json`, `base64` and `libc`); see [shedctl](cli.md).
- **Tauri client** — *not* a `crates/` workspace member, but the main consumer:
  `desktop/tauri/src-tauri` is its own standalone cargo workspace (so its
  WebKitGTK/Tauri dependencies stay out of `crates/`). Its `[[bin]]` is
  **`shed-desktop-tauri`** — installed to `/usr/bin/shed-desktop` in the Linux
  `.deb` (built via nfpm, `linux/scripts/build-deb.sh`), and bundled as
  `ShedDesktop.app` on macOS (WKWebView).

## Invariants

- **Defensive decoding**: `{"sheds": null}` → `[]`, omitted optionals, lenient
  `ShedStatus`, `"?"` name sentinels, and timestamps carried verbatim.
- **Auth, fail-closed**: a provider mint failure sends **no** token (never the
  static one — no secure-by-default downgrade); a 401 invalidates + retries once
  (provider-backed only); create mints once with no retry.
- **TLS**: leaf SHA-256 pin (`sha256:<lowercase-hex>`), fail-closed on a
  non-`https://` URL.
- **SSE create**: cross-chunk framing, and the `error` / stream-ended-without-
  `complete` terminal semantics.

## Building & testing

All targets run from the monorepo root (or `make -C desktop …`):

```bash
make -C desktop core-test       # cargo test the workspace (+ shed-app's `broker` feature)
make -C desktop core-lint       # cargo clippy -D warnings (same two passes)
make -C desktop core-fmt        # cargo fmt --all
make -C desktop core-linux      # shed-core test + clippy on ubuntu:24.04, in Docker
```

You can also drive the workspace directly from `crates/` (`cargo test`, `cargo clippy`). Rust
changes need `cargo`/`rustup` (the workspace pins the channel in `crates/rust-toolchain.toml`).
In CI, `core-mac` runs the test and clippy targets on macOS and `core-linux` on Linux.

## Status & scope

The core covers the read client, lifecycle, SSE create, TLS pinning, the
control-token FSM, a `config` parser, the create orchestration store, the
approval policy model and the agent-lane contract, and builds and tests on macOS
and Linux. The Tauri client, built on `shed-core` + `shed-app`, is the shipped
desktop client on both platforms. It carries lifecycle + create, the approval
spine (polkit gate + D-Bus notifier on Linux, Touch ID on macOS), the embedded
credential broker, the [Agents pane](rc-sessions.md), the tray, and
launch-at-login, driven by the `tools/shedtest` harness.
