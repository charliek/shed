# sx (sunset)

`sx` was the **kickoff and observe porcelain** for RC agent sessions: one command
started an agent on this machine, on a native machine over SSH, or inside a shed, and
the same command set listed, watched, attached to, and killed those sessions wherever
they ran. It also carried an engine-compat surface, `sx rc <subcommand>`, that spoke
the same one-shot wire the retired [`shed-machine-rc`](shed-machine-rc.md) used to
serve.

**It never shipped a release.** `sx` was wired to its own release component
(`crates/sx/VERSION` as selector, its own goreleaser config, a brew + apt slot) from
plan 011 onward, but that selector sat at `0.0.0` and no tag ever matched it — no
`sx` binary was ever published.

**It is sunset**, in plan 016 (S7,
[`charliek/shed#329`](https://github.com/charliek/shed/issues/329)): every verb it
carried is a hub/tmux verb the Roost Pivot replaces, and keeping an unreleased
component alive for a role the pivot was already taking over had no upside. Each job
moved:

| What `sx` did | Where it went |
|---|---|
| Kickoff (`agent`, `plan`) | Roost's own palette, via the [`shed` dynamic-provider script](roost-provider.md) (S4, [`charliek/shed#326`](https://github.com/charliek/shed/issues/326)). |
| Observe (`ls`, `watch`, `attach`, `kill`) | The desktop and mobile clients, reading roost directly. |
| Engine-compat (`sx rc <subcommand>`) | Gone with the RC hub itself, retired in S6 ([`charliek/shed#328`](https://github.com/charliek/shed/issues/328), plan 022) — see [`shed-ext-rc` (retired)](rc-helper.md). There is no engine-compat surface any more, `sx` or otherwise. |

!!! note "The S7 → S6 window has closed"
    Until S6 landed, `machines[].rc_bin` still defaulted to `sx` and the desktop's
    Add-Machine dialog still asked for its path. S6 removed both: `rc_bin` is gone from
    the Rust config model and the Add-Machine dialog (the Go CLI's config model never
    modeled `rc_bin` at all — plan 019 pin P7 deliberately left it Rust/desktop-only). A
    `~/.shed/config.yaml` that still carries `rc_bin:` under a machine loads unchanged on
    both sides — the key is ignored, not rejected. Machine *status* was unaffected
    throughout — it has read roost, not `sx`, since plan 013.

## See also

- [`shed-ext-rc` (retired)](rc-helper.md) — what replaced RC sessions end to end:
  roost tabs, `shed attach`/`shed sessions`, and the roost provider/session-hosts
  mechanism.
- [`shed-machine-rc`](shed-machine-rc.md) — the retired Go engine `sx` absorbed.
