# shed-plan: command reference

The flags this skill relies on. `shed <command> --help` is the authoritative,
version-matched source.

## Create a fresh shed (default path)

```
shed create <name> --repo <owner/repo> -s <server>
```

| Flag | Purpose |
|------|---------|
| `--repo <owner/repo>` | Clone the target repo into the shed (preferred over `--local-dir` for remote autonomous runs). |
| `-s, --server <name>` | Which server to create on. **Ask the user if unspecified.** |

## Start an autonomous Remote Control run

```
shed attach <shed> --plan <file> -d [--skip] [-p "<single-line kickoff>"]
```

| Flag | Purpose |
|------|---------|
| `--plan <file>` | Ship a plan file into the shed (`-` reads stdin); the agent is told to execute it. |
| `-d, --detach` | Create the session, print the `claude.ai` URL, and return without attaching. |
| `--kind <k>` | Session kind: `claude-rc` (default), `claude-broker`, `shell`. The plan flow uses `claude-rc`. |
| `-p, --prompt <line>` | Override the kickoff prompt (single line). Default references the shipped plan. |
| `--permission-mode <m>` | `auto` (default) \| `acceptEdits` \| `plan` \| `dontAsk` \| `default` \| `bypassPermissions`. |
| `--skip` | Shorthand for `--permission-mode bypassPermissions` (full bypass). Confirm with the user first. |
| `--name <display>` | Session display name (default `<shed>/<slug>`). |
| `--slug <slug>` | Set the session slug (otherwise generated). |

Notes:
- The plan is written to `<workdir>/.shed/plan-<slug>.md` inside the shed and the
  default kickoff prompt references it. The kickoff prompt must be a single line.
- Do **not** use `--edit` / `--plan-edit` in the autonomous flow (they open `$EDITOR`).
- On an old shed image whose `shed-ext-rc` predates `--permission-mode`: an explicit
  `--skip`/`--permission-mode` errors with an upgrade hint; the default `auto` falls
  back to starting without an autonomous posture (with a warning).

## Watch, list, stop

```
shed attach <shed> --slug <slug>     # attach a terminal to rc-<slug>
shed sessions [<shed>] [--all] [--json]   # list (rc-* rows show KIND + RC-STATE)
shed sessions kill <shed> rc-<slug>  # stop the session
```

## Auth prerequisite

Claude must be logged in **inside the shed VM**. If a run reports `needs-auth`:

```
shed attach <shed>     # then run `claude`, do /login once, exit
```

then retry. Servers that mount a persistent Claude config dir into sheds make this
automatic for fresh sheds.
