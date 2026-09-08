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
| Kickoff (`agent`, `plan`) | Roost's own palette, via the `shed` dynamic-provider script (S4, [`charliek/shed#326`](https://github.com/charliek/shed/issues/326)) — **not yet built**. |
| Observe (`ls`, `watch`, `attach`, `kill`) | The desktop and mobile clients, reading roost directly. |
| Engine-compat (`sx rc <subcommand>`) | The guest [`shed-ext-rc`](rc-helper.md), until the RC hub is retired (S6, [`charliek/shed#328`](https://github.com/charliek/shed/issues/328)). |

!!! warning "The S7 → S6 window"
    `machines[].rc_bin` still defaults to `sx`, and the desktop's Add-Machine dialog
    still asks for its path — that wiring is S6's to remove, not this plan's. A
    machine's RC sessions keep working only where a **pre-sunset** `sx` binary is
    already installed, and after this sunset there is no way to install one anywhere
    else. Machine *status* is unaffected: it has read roost, not `sx`, since plan 013.

## See also

- [`shed-ext-rc` (RC session helper)](rc-helper.md) — the wire contract, kinds,
  permission modes, JSON DTO, exit codes, and the activity hub, including
  [the machine hub](rc-helper.md#the-machine-hub-shed-host-agent) `sx` used to probe.
- [`shed-machine-rc`](shed-machine-rc.md) — the retired Go engine `sx` absorbed.
