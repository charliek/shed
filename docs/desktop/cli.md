# shedctl CLI

`shedctl` drives the [IPC socket](ipc.md) — for humans and scripts. The app bundle embeds
it at `Contents/Resources/bin/shedctl`; `swift build` also produces it under `.build`.

It resolves the socket from `$SHED_DESKTOP_SOCKET`, falling back to the default profile
path.

## Commands

| Command | Description |
|---------|-------------|
| `shedctl identify` | Print app identity + protocol version |
| `shedctl ui state` | Dump the current view-model (panes, hosts, sheds) |
| `shedctl ui navigate <pane>` | Switch the dashboard pane |
| `shedctl ui show-window` | Bring the dashboard window front |
| `shedctl ui hide-window` | Close the dashboard (revert to menu-bar-only) |
| `shedctl ui window-state` | Report dashboard visibility + activation policy |
| `shedctl ui open-menu <true\|false>` | Open/close the menu-bar popover |
| `shedctl host list` | List configured hosts + reachability |
| `shedctl sheds list [--host NAME]` | List sheds (optionally one host) |
| `shedctl sheds refresh` | Force an immediate poll |
| `shedctl screenshot [--surface window\|menu] [--scale 1\|2] --out FILE` | Capture a PNG |
| `shedctl call <op> [key=value ...]` | Generic call; values parse as JSON when possible |

The named subcommands cover inspection and UI control (including the mutating
`ui hide-window`). Everything else in the
[IPC op catalog](ipc.md) — lifecycle (`shed.start` …), create, terminal, remote control
(`rc.*`), approvals (`approval.decide` …), and `system.df` — is reached through
`shedctl call <op>`. Panes for `ui navigate` are `sheds | approvals | agents | activity |
system`.

## Examples

```bash
shedctl identify
shedctl ui navigate system
shedctl ui show-window
shedctl screenshot --surface window --scale 2 --out /tmp/shot.png
shedctl call sheds.list host=mini3
shedctl call system.df                       # per-host disk usage
shedctl call approval.decide id=<req-id> decision=approve
```
