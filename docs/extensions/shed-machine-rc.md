# shed-machine-rc (retired)

`shed-machine-rc` was the host-side sibling of the now-retired [`shed-ext-rc`](rc-helper.md):
the same RC Session Convention v2 engine, shipped as a CLI for **native machines** — a
laptop, workstation, or tailnet host — instead of baked into a shed image. It created and
drove `rc-<slug>` `tmux` sessions, and its `serve` verb ran the machine's RC activity hub.

**It is retired**, in two stages:

| What it did | What happened to it |
|---|---|
| the one-shot verbs (`create`, `list`, `probe`, `prompt`, `kill`, …) | Plan 010 ported this half to Rust and shipped it as `sx`; `sx` was itself [sunset unreleased in plan 016](sx.md) before it ever shipped. Machine kickoff moved to [roost's own palette](roost-provider.md) (S4), with [`roost-session` bootstrapped onto the machine first](roost-session-hosts.md) (S5) when it doesn't already have one. |
| `serve` — the machine RC activity hub on `127.0.0.1:1029` | Plan 010 ported this half into `shed-host-agent` (`shed-broker`'s `rc_hub` module) as a supervised resident role. Plan 022 (S6, [`charliek/shed#328`](https://github.com/charliek/shed/issues/328)) then retired the hub itself — machine session activity now comes from [roost](roost-session-hosts.md), the same way it does for a shed. `shed-host-agent rc-hub` is now a tombstone: it exits naming the retirement instead of starting a hub. |

The `tests/rc-parity` differential suite that once proved the Rust hub port wire-identical
to this binary's engine (its Go oracle relocated to `tests/rc-parity/oracle/` at the plan
010 retirement) is deleted whole as part of the S6 retirement — there is no hub left on
either side of the wire to diff.

!!! warning "No new releases"
    The `machine-rc` release component is gone — `cmd/shed-machine-rc`, its
    goreleaser config, and its version selector are deleted from the tree, and the
    release scripts reject the `machine-rc` token outright. Artifacts published
    before the retirement are not being withdrawn, but nothing new will be built
    from them and they receive no fixes.

The Go RC engine this binary was built from (`internal/ext/rc`, `internal/ext/clirc`) is
also gone — it was deleted along with the guest binary `shed-ext-rc` in the same S6
retirement, not kept on as a guest-side survivor.

## Migrating a machine

There is nothing to install as a `shed-machine-rc` successor. A native machine's agent
sessions are roost tabs, the same as a shed's:

1. Install and start a [`roost-session`](roost-session-hosts.md) on the machine — from the
   shed desktop or mobile app (S5), or by hand per roost's own docs.
2. Kick off an agent through [roost's own palette](roost-provider.md), or through the shed
   desktop/mobile app's Agents pane.
3. Remove the old binary at your convenience — `brew uninstall shed-machine-rc`, or
   `apt remove shed-machine-rc`. Nothing in the current tree invokes it.
