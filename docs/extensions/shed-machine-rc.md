# shed-machine-rc (retired)

`shed-machine-rc` was the host-side sibling of [`shed-ext-rc`](rc-helper.md): the same
RC Session Convention v2 engine, shipped as a CLI for **native machines** — a laptop,
workstation, or tailnet host — instead of baked into a shed image. It created and drove
`rc-<slug>` `tmux` sessions, and its `serve` verb ran the machine's RC activity hub.

**It is retired.** Both halves have a replacement:

| What it did | What does it now |
|---|---|
| the one-shot verbs (`create`, `list`, `probe`, `prompt`, `kill`, …) | [`sx rc <verb>`](sx.md) — wire-identical, and what `sx --on machine:<name>` invokes over SSH |
| `serve` — the machine RC activity hub on `127.0.0.1:1029` | the [`shed-host-agent` daemon](sx.md#the-machine-hub), which hosts the hub as a supervised resident role |

The Rust port is wire-identical by construction, not by assertion: the
`tests/rc-parity` differential suite builds a test-only copy of this binary's engine
(`tests/rc-parity/oracle`) and diffs it against `sx` verb-by-verb, and runs both hub
implementations side by side against the same `/v1` cells. That suite is the standing
proof and still runs on every relevant change.

!!! warning "No new releases"
    The `machine-rc` release component is gone — `cmd/shed-machine-rc`, its
    goreleaser config, and its version selector are deleted from the tree, and the
    release scripts reject the `machine-rc` token outright. Artifacts published
    before the retirement are not being withdrawn, but nothing new will be built
    from them and they receive no fixes. **Move machines to `sx` +
    `shed-host-agent`.**

The Go engine itself lives on: it is what [`shed-ext-rc`](rc-helper.md) — baked into
every `extensions`/`full` rootfs image — is built from, and sheds continue to run the
Go hub in-guest. Only the machine-facing binary retired.

## Migrating a machine

1. Install `shed-host-agent` and let it run; it hosts the hub (see
   [The machine hub](sx.md#the-machine-hub)). Unlike `shed-machine-rc serve`, it does
   not exit after an idle period.
2. Install `sx` on the machine — `brew install charliek/tap/sx` on macOS,
   `sudo apt install sx` on Debian/Ubuntu (see [Install](sx.md#install)). It takes the
   same brew+apt channel pair this binary had, so it lands in `/usr/local/bin` (apt) or
   the Homebrew prefix. If that is not on the non-login `PATH` an SSH exec sees, point
   `machines[].rc_bin` at its absolute path.
3. Remove the old binary at your convenience — `brew uninstall shed-machine-rc`, or
   `apt remove shed-machine-rc`. Nothing in the current tree invokes it.
