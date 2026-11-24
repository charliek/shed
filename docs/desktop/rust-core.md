# Rust core (`shed-core`)

The shed-server protocol client — HTTP + SSE, control-token auth, TLS pinning,
and the wire DTOs — lives in a shared Rust core in the monorepo's top-level
`crates/` workspace, so every client runs the same logic instead of
re-implementing it per language. Its consumers:

- **the desktop app** — the Tauri client on macOS and Linux (`desktop/tauri/src-tauri`),
  which links `shed-core`, `shed-app`, `shed-desktop-core`, `shed-source`, `shed-provision`,
  `shed-provision-providers`, `shed-lane`, `shed-roost` and `shed-craze` as path dependencies;
- **shed-mobile** — the phone client, which takes `shed-core` as a git dependency and calls
  it from Dart through flutter_rust_bridge (FRB);
- **`shed-source`** — the machine identity, certificate store, control-token minting and the delivery stream client the desktop app's source runtime is built on (extracted from the credential broker the app embedded before v0.11);
- **`shed-provision`** and **`shed-provision-providers`** — the provision contract's documents and the five providers (`ssh-key`, `github`, `api-keys`, `envsecrets-login`, `files`); pure, no async runtime and no HTTP client. See [Provisioning](provisioning.md).

## Layout

A cargo workspace under `crates/`. Every member is a default member. The desktop links these crates (the
opencode adapter crate was removed in plan 038; craze is the only lane):

- **`shed-lane`** — pure leaf: the agent-lane contract (`lane`), RC session DTOs
  (`rc`), slash menu, craze command ladder, shell quoting, lane view and text/time
  helpers. No `shed-core`, no `reqwest`, no `roost-ipc`.
- **`shed-core`** — a *pure* Rust lib (no UI): wire DTOs + serde decoders
  (`models.rs`), the reqwest(rustls) client (`http.rs`), the SSE parser
  (`sse.rs`), leaf-cert pinning (`tls.rs`), and the control-token FSM
  (`token.rs`), plus a `config` parser (`servers:` and `default_server` only;
  `known_hosts_path()`), a pull-based `create` orchestration store, It depends on `shed-lane` for `RcSession`.
  Plan 022 (S6, [`charliek/shed#328`](https://github.com/charliek/shed/issues/328))
  deleted the RC hub; plan 038 removed `machines:` from this crate.
- **`shed-roost`** — the roost-session client (connection, model, bootstrap,
  reach, spawn gate). The only crate in `crates/` that depends on `roost-ipc`.
- **`shed-app`** — the UI-free shed client (`Backend`, `AuthModeRegistry`).
  It has no features. It does not link `roost-ipc` or `shed-lane`.
- **`shed-desktop-core`** — the toolkit-free shed-only runtime (`ShedId`,
  `ShedHosts`, `ShedCraze`, `ShedLanes`, shutdown, the delivery runtime, the egress
  feed and the control-certificate mint). The Tauri shell is a thin
  adapter over it. No `tauri`, no `iced`, no direct `roost-ipc`.
- **`shed-craze`** — the craze agent-lane adapter. Depends on `shed-lane` only.
- **`shedctl`** — a headless client for the desktop app's IPC socket, shipped in
  the Linux `.deb` alongside `shed-desktop`. It depends on no other crate here
  (only `serde_json`, `base64` and `libc`); see [shedctl](cli.md).
- **Tauri client** — *not* a `crates/` workspace member, but the main consumer:
  `desktop/tauri/src-tauri` is its own standalone cargo workspace (so its
  WebKitGTK/Tauri dependencies stay out of `crates/`). Its `[[bin]]` is
  **`shed-desktop-tauri`** — installed to `/usr/bin/shed-desktop` in the Linux
  `.deb` (built via nfpm, `linux/scripts/build-deb.sh`), and bundled as
  `ShedDesktop.app` on macOS (WKWebView). It links the crates above and does not
  depend on `roost-ipc` directly.

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
make -C desktop core-test       # cargo test the workspace
make -C desktop core-lint       # cargo clippy -D warnings (same two passes)
make -C desktop core-fmt        # cargo fmt --all
make -C desktop core-linux      # shared-crate test + clippy on ubuntu:24.04, in Docker
```

You can also drive the workspace directly from `crates/` (`cargo test`, `cargo clippy`). Rust
changes need `cargo`/`rustup` (the workspace pins the channel in `crates/rust-toolchain.toml`).
In CI, `core-mac` runs the test and clippy targets on macOS and `core-linux` on Linux.

## Status & scope

The core covers the read client, lifecycle, SSE create, TLS pinning, the
control-token FSM, a `config` parser, the create orchestration store
and the agent-lane contract, and builds and tests on macOS
and Linux. The Tauri client, built on `shed-core` + `shed-app`, is the shipped
desktop client on both platforms. It carries lifecycle + create, the credential
source and its delivery runtime, the [Agents pane](rc-sessions.md), the tray, and
launch-at-login, driven by the `tools/shedtest` harness.
