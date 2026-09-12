# shed-machine-rc (retired)

`shed-machine-rc` was the host-side sibling of [`shed-ext-rc`](rc-helper.md): the same
RC Session Convention v2 engine, shipped as a CLI for **native machines** — a laptop,
workstation, or tailnet host — instead of baked into a shed image. It created and drove
`rc-<slug>` `tmux` sessions, and its `serve` verb ran the machine's RC activity hub.

**It is retired.** Both halves it carried ended up in different places:

| What it did | What does it now |
|---|---|
| the one-shot verbs (`create`, `list`, `probe`, `prompt`, `kill`, …) | **No shipped successor.** The Rust porcelain that absorbed them (`sx`) was itself [sunset in plan 016](sx.md) before it ever shipped. The guest [`shed-ext-rc`](rc-helper.md) keeps serving these verbs inside sheds until the RC hub is retired (S6); machine kickoff moved to [roost's own palette](roost-provider.md) (S4), with [`roost-session` bootstrapped onto the machine first](roost-session-hosts.md) (S5) when it doesn't already have one. |
| `serve` — the machine RC activity hub on `127.0.0.1:1029` | the [`shed-host-agent` daemon](rc-helper.md#the-machine-hub-shed-host-agent), which hosts the hub as a supervised resident role |

The Rust port was wire-identical by construction, not by assertion: the
`tests/rc-parity` differential suite still builds a test-only copy of this binary's
engine (`tests/rc-parity/oracle`) and runs it against the RC activity hub — the
**hub family** diffs the Go oracle's `serve --foreground` against
`shed-host-agent rc-hub` over the same `/v1` wire, and that family remains the standing
proof today. (The one-shot differential against `sx` was retired alongside it in plan
016.)

!!! warning "No new releases"
    The `machine-rc` release component is gone — `cmd/shed-machine-rc`, its
    goreleaser config, and its version selector are deleted from the tree, and the
    release scripts reject the `machine-rc` token outright. Artifacts published
    before the retirement are not being withdrawn, but nothing new will be built
    from them and they receive no fixes.

The Go engine itself lives on: it is what [`shed-ext-rc`](rc-helper.md) — baked into
every `extensions`/`full` rootfs image — is built from, and sheds continue to run the
Go hub in-guest. Only the machine-facing binary retired.

## Migrating a machine

1. Install `shed-host-agent` and let it run; it hosts the hub (see
   [The machine hub](rc-helper.md#the-machine-hub-shed-host-agent)). Unlike
   `shed-machine-rc serve`, it does not exit after an idle period.
2. There is nothing to install for the one-shot verbs — `sx` was sunset unreleased
   (see [its tombstone](sx.md) for where each job went). Machine kickoff runs through
   [roost's palette](roost-provider.md) instead, bootstrapping
   [`roost-session` onto the machine](roost-session-hosts.md) first if needed.
3. Remove the old binary at your convenience — `brew uninstall shed-machine-rc`, or
   `apt remove shed-machine-rc`. Nothing in the current tree invokes it.
