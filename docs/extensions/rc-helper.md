# shed-ext-rc (retired)

`shed-ext-rc` was the guest-side helper for **remote-control (RC) sessions**: detached
`tmux` sessions (`rc-<slug>`) that ran an agent (`claude`, `codex`, `cursor-agent`,
`opencode`) or a shell inside a shed, plus a resident **RC activity hub**
(`shed-ext-rc serve`) that derived live status for them.

**It is gone.** Plan 022 (S6, [`charliek/shed#328`](https://github.com/charliek/shed/issues/328))
retired the whole RC-hub stack this binary was part of: the guest binary no longer ships
in the `extensions`/`full` rootfs images (an existing shed built from an older image keeps
an inert copy that nothing calls any more — nothing invokes it, and it is not restarted or
cleaned up); the server's RC routes, enrichment, and feature tokens are deleted;
`shed plan` and the Remote-Control mode of `shed attach` are removed, not rebased; the
desktop app's hub half of the Agents pane is gone; and the Rust hub (`crates/shed-rc-engine`,
`shed-broker`'s `rc_hub` module, the host-agent's `rc-hub` role) is deleted with it. See the
[v0.8.2 → v0.9.0 upgrade note](../upgrades/v0.8.2-to-v0.9.0.md) for what that means for an
existing shed and desktop install.

## What replaced it

Agent sessions are now **roost tabs**, not `rc-*` tmux sessions:

- **`shed attach <shed>`** opens a shed as a roost tab when a local
  [roost](https://github.com/charliek/roost) app is running; with no local roost app,
  `--tmux`, or `SHED_ATTACH=tmux`, it falls back to a plain tmux session — the same floor
  this binary used to manage, now driven directly by the CLI instead of `shed-ext-rc`. See
  [`shed attach`](../reference/cli.md#shed-attach).
- **`shed sessions`** lists a shed's roost tabs above its tmux rows, and `sessions kill`
  closes either. See [`shed sessions`](../reference/cli.md#shed-sessions).
- **The desktop app's Agents pane** shows a shed's roost tabs the same way it shows a
  machine's. See [Machines and RC sessions](../desktop/rc-sessions.md).
- **Kicking off an agent from roost's own command palette** — on a shed or a native
  machine — is [the `shed` roost provider](roost-provider.md); getting a `roost-session`
  onto a shed or machine that doesn't have one yet is
  [roost session hosts](roost-session-hosts.md) (S5).

Status for a shed's `codex`/`cursor`/`claude` rows now comes from **roost** once a
`roost-session` is reached on the guest, the same way a native machine's status always has
— not from a second, shed-side derivation of the same signal.

## See also

- [`sx` (sunset)](sx.md) — the Rust porcelain that was meant to succeed this binary's
  machine-facing half, and was itself sunset unreleased.
- [`shed-machine-rc` (retired)](shed-machine-rc.md) — the machine-side sibling this binary
  shared a wire contract with.
- [roost Provider](roost-provider.md) and [roost Session Hosts](roost-session-hosts.md) —
  the S4/S5 mechanism that replaced this binary's kickoff and status duties.
