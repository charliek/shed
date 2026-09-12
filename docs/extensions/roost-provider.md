# The `shed` roost provider

[roost](https://github.com/charliek/roost) is a headless per-user session daemon (a
`roost-session`) with a command palette (`roost-iced`) in front of it. Its palette is
extensible through **dynamic providers** — small executables that print a menu and act on a
selection. `shed roost-provider` is shed's provider: it puts "start an agent on a shed or
machine" into roost's own palette, so opening a `claude`, `codex`, `cursor`, `opencode`, `gx`,
or `grok` session no longer needs a separate tool.

This is the S4 half of the Roost Pivot (`epics/roost-pivot.md`) and the replacement for
[`sx`](sx.md), which was sunset unreleased before it ever shipped a kickoff path. See
[roost-session-hosts.md](roost-session-hosts.md) for S5 — putting `roost-session` itself onto a
shed or machine that doesn't have one yet, which is what turns a shed row from "not
installed" into something this provider can open a tab on.

**`shed roost-provider` does not install `roost-session` anywhere.** It only speaks to a
`roost-session` that is already running on the target. Getting one there is the desktop or
mobile app's job — see [roost-session-hosts.md](roost-session-hosts.md).

## Installing the provider

```bash
shed roost-provider --install              # write the launcher
shed roost-provider --install --dry-run    # preview without writing
shed roost-provider --uninstall            # remove it
```

The launcher is a small POSIX shell script roost discovers by scanning its `providers/`
directory. `--install` writes it to:

```text
<dir>/providers/shed
```

where `<dir>` is the **parent directory of `$ROOST_CONFIG`** (a file path) when that variable
is non-empty, else `$HOME/.config/roost`. This matches roost's own default — **roost does not
consult `$XDG_CONFIG_HOME`**, so the launcher never does either. The file is written mode
`0755`, atomically (a temporary file in the same directory, renamed into place), and pins the
running `shed` binary's resolved absolute path:

```sh
#!/bin/sh
# @roost.label: shed
# @roost.title: Start an agent on a shed or machine
# Written by `shed roost-provider --install`; re-run it if shed moves. Roost runs
# this by absolute path with its own (possibly minimal) PATH, so the shed binary
# is pinned here and the usual install dirs are prefixed, as roost's example does.
PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:$HOME/.local/bin:$PATH"; export PATH
exec '/abs/path/to/shed' roost-provider "$@"
```

Re-run `--install` whenever the `shed` binary moves — a Homebrew upgrade, a rebuild
elsewhere — to repin the path. `--install` and `--uninstall` both refuse to touch a file that
was not written by a previous `--install`: they check for the label header above before
writing or removing anything, and refuse outright (no read, no write) if the path is a
symlink or anything other than a regular file. Re-running `--install` against an unchanged
binary path is a no-op. Full flag reference: [`shed roost-provider` in the CLI
reference](../reference/cli.md#shed-roost-provider).

## The menu, end to end

roost drives the provider itself, through `list` and `activate` phases (a person does not type
these day to day):

1. **`list`** — the top-level menu. No SSH: every running shed across the servers in
   `~/.shed/config.yaml`, plus every entry under `machines:`. A **stopped** shed is not
   listed — it cannot take a tab, and `shed start` is slow work that belongs inside a tab, not
   inside a five-second `list`. Row titles are `shed: <name>` (subtitle: `<server> · <landing
   dir or ~>`) and `machine: <name>` (subtitle: `[user@]host[:port]`).
2. **Choosing a host** runs one SSH round trip (a probe over `bash -lc`) and two calls over
   roost's own client-bridge (`session.identify`, `tab.list`) — no `roostctl` needed on the far
   side; the provider speaks roost's wire directly. The probe uses **the same shell verb the
   launch itself uses**, so the menu never offers an agent a tab would then fail to find. On
   success it lists one row per agent binary the probe actually found on that host —
   `claude`, `codex`, `cursor`, `opencode`, `gx`, `grok` — each subtitled with its resolved
   path.
3. **Workdir selection** — one candidate per far-side roost project, plus `Home` (the probed
   absolute `$HOME`) and the shed's landing directory when it differs and exists. Exactly one
   candidate collapses this step automatically.
4. **Open** — `tab.open` with an absolute `cwd` and `argv: ["bash", "-lc", "exec \"$@\"", "shed",
   "<binary>"]`: a login shell resolves the agent through the target's login PATH, then
   `exec`s it, so the tab **is** the agent process from the start. `~` never reaches
   `tab.open`. Success prints `opened tab <id> on <host>` (not JSON, so roost's palette just
   closes).

Until [a `roost-session` exists on a shed](roost-session-hosts.md), every shed row lands on
the first non-actionable row below.

## Non-actionable rows

Six far-side or local states are surfaced as **non-actionable rows** rather than menu items —
each exits the provider phase with status 0 and a single pinned row, because refusing to
answer would look like a crash to roost's palette:

| State | Title | Subtitle |
|---|---|---|
| `roost-session` binary not found on the target (exit 127 / `command not found`) | `roost-session is not installed on <host>` | `connect from the shed desktop or mobile app to install it` |
| Installed but not running (`client-bridge: no session`) | `roost-session is not running on <host>` | `connect from the shed app to start it, or run roost-session start there` |
| A running session speaks a different session protocol | `roost-session on <host> speaks protocol <n>; this shed speaks 4` | `upgrade whichever is older` |
| SSH itself failed (unreachable, timeout, exit 255) | `<host> is unreachable` | ssh's own last stderr line |
| No local `ssh` binary found | `ssh is not installed where roost can see it` | the paths searched (`$PATH`, then `/usr/bin/ssh`, `/opt/homebrew/bin/ssh`, `/usr/local/bin/ssh`) |
| The probe found none of the six agent binaries | `no agents found on <host>` | `looked for claude, codex, cursor-agent, opencode, gx, grok under bash -lc` |

A **protocol mismatch is reported, never acted on** — shed never stops or restarts a session it
does not speak the same protocol as; a running session belongs to whoever started it. These
six rows appear only on drill-in (choosing a host), never in the top-level `list`, so an
unreachable fleet never blows roost's 5-second `list` budget across every configured host.

An id this process did not just mint, or a malformed reply from the far side (an empty
`tab.open`/`session.identify` result, for instance), is treated as a **provider failure** —
the phase exits non-zero — rather than folded into one of the rows above: a row would claim to
know something about the target that this side did not actually learn.

## Timeouts

Both phases run inside **roost's own provider timeout**, which defaults to 5 seconds and which
roost enforces by killing the provider process outright. shed's own internal budget for each
phase defaults to 4 seconds — under roost's, so there is room to serialize and flush a row
before roost's own clock runs out.

| Variable | Default | Effect |
|---|---|---|
| `SHED_ROOST_PROVIDER_TIMEOUT` | `4s` | Internal budget for one `list` or `activate` phase. Must stay **below** roost's own `timeout=` for this provider, or roost kills the phase before shed can answer at all. An unparseable or non-positive value is ignored, with a warning on stderr. |

If you raise roost's own timeout for this provider in its config form — for a slow link, or a
target that takes its time to answer — raise `SHED_ROOST_PROVIDER_TIMEOUT` to match:

```text
# ~/.config/roost/config.conf (roost's own config-form syntax)
provider = label="shed" run="/home/user/.config/roost/providers/shed" timeout=20
```

```bash
export SHED_ROOST_PROVIDER_TIMEOUT=15s   # stay under the timeout=20 above
```

Raising only one of the two numbers doesn't help: raising roost's `timeout=` alone leaves
shed still giving up at 4 seconds; raising shed's alone just moves the point at which roost
kills the process before a row is ever printed. `activate` is the phase most likely to want
more headroom on a slow WAN — it runs several sequential round trips (an inventory check,
the probe, `session.identify`, `tab.list`, and sometimes `tab.open`) rather than `list`'s
single fan-out.

## See also

- [`shed roost-provider` in the CLI reference](../reference/cli.md#shed-roost-provider) — the
  flag table.
- [roost-session-hosts.md](roost-session-hosts.md) — putting `roost-session` on a shed or
  machine in the first place (S5), which is what makes this provider's menu actually useful on
  a fresh shed.
- [`sx` (sunset)](sx.md) — the tool this provider's kickoff half replaces.
