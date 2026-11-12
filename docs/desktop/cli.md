# shedctl CLI

`shedctl` drives the [IPC socket](ipc.md) — for humans and scripts. It is a small Rust binary
(`crates/shedctl`) with no GUI toolkit: it sends one request, prints the reply, and exits.

The Linux `.deb` installs it at `/usr/bin/shedctl` beside `shed-desktop`. The macOS app bundle
does not include it; build it from the monorepo with `cargo build --release -p shedctl` in
`crates/` (the binary lands in `crates/target/release/shedctl`).

## Socket resolution

The first of these that is set wins:

1. `--socket PATH` (a global flag, accepted anywhere on the command line)
2. `$SHED_TAURI_SOCKET`
3. `$XDG_RUNTIME_DIR/shed-tauri.sock`
4. `/tmp/shed-tauri-<uid>/shed-tauri.sock`

This matches how the app picks its own socket, so with no overrides `shedctl` finds the
running app.

## Commands

| Command | Op | Description |
|---------|----|-------------|
| `shedctl identify` | `identify` | Print the running app's identity, test mode and broker mode |
| `shedctl sheds list` | `sheds.list` | List sheds across configured hosts, with per-host failures |
| `shedctl dashboard dump` | `dashboard.dump` | Dump the shed rows the dashboard rendered |
| `shedctl hosts auth` | `hosts.auth` | Per-server credential mode (`token` or `mtls`) and whether it was learned this session |
| `shedctl screenshot [--out FILE] [--scale N]` | `app.screenshot` | Capture a PNG; with `--out`, decode it to FILE and print `wrote FILE (WxH)`, else print the JSON reply. `--scale` is sent but the app ignores it |
| `shedctl shed <start\|stop\|reset\|delete> NAME [--host H]` | `shed.<action>` | Run a shed lifecycle action |
| `shedctl raw <op> [--param k=v ...]` | any | Call an arbitrary op; each value is parsed as JSON when it parses (`3`, `true`, `{"a":1}`), else sent as a string |

Every other op in the [IPC catalog](ipc.md) — navigation, create, terminal, agent sessions,
approvals, `system.df` — is reached through `shedctl raw`.

A successful reply prints its `result` as pretty JSON and exits 0. An error reply prints
`error <code>: <message>` and exits 1, as does an unreachable socket. A usage error exits 2.

## Examples

```bash
shedctl identify
shedctl raw ui.navigate --param pane=system
shedctl raw ui.show_window
shedctl screenshot --out /tmp/shot.png
shedctl shed stop my-shed --host mini3
shedctl raw system.df                        # per-host disk usage
shedctl raw approval.decide --param id=<req-id> --param decision=approve
```
