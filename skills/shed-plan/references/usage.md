# shed-plan: command reference

The flags this skill relies on. `shed <command> --help` is the authoritative,
version-matched source.

## One command: ship a plan and run it

```bash
shed plan <file> --shed <name> [--repo <owner/repo>] [-s <server>] [--kind <k>] \
  [-p "<framing>"] [--permission-mode <m> | --skip] [--workdir <dir>] [-d]
```

| Flag | Purpose |
|------|---------|
| `<file>` | Plan file to ship (`-` reads stdin). Validated client-side (non-empty, UTF-8, ≤ 1 MiB) before any shed or session is touched. |
| `--shed <name>` | Target shed (required). |
| `--repo <owner/repo>` | Create the shed from this repo **if it doesn't exist**. On an existing shed it's warned-and-ignored. A missing shed with no `--repo` is a hard error. |
| `-s, --server <name>` | Which server. **Ask the user if unspecified.** |
| `--kind <k>` | Agent kind: `claude-rc` (default), `codex`, `cursor`, `opencode`, `shell`. |
| `-p, --prompt <framing>` | Optional framing prepended to the composed plan kickoff (may be multi-line). |
| `--permission-mode <m>` | `default` \| `auto` (default) \| `skip` for all kinds; Claude also accepts `acceptEdits` \| `plan` \| `dontAsk` \| `bypassPermissions`. |
| `--skip` | Shorthand for `--permission-mode skip` (full bypass). Confirm with the user first — mutually exclusive with `--permission-mode`. |
| `--workdir <dir>` | Working directory inside the shed for the RC session (default `$SHED_WORKSPACE`/`$HOME`). |
| `-d, --detach` | Report the session and return instead of attaching when it's ready. |

Runs under the `auto` permission posture by default. The plan is written HOME-rooted
inside the shed (Claude: `~/.claude/plans/plan-<slug>.md`; others:
`~/.shed-plans/plan-<slug>.md`), never the workspace, and the kickoff references its
absolute path.

**Exit contract:** `0` only when the session reached `ready` and the kickoff was
delivered. `needs-auth` / `needs-trust`, a failed session, or an old shed image exits
non-zero and leaves the session/shed running for you to fix — nothing is auto-deleted.
When `--repo` created the shed but the plan then failed, the error reports both facts
and that the shed was **not** deleted.

## Lower-level primitive (`shed attach`)

`shed plan` is porcelain over `shed attach`'s Remote Control mode. Drop to it when you
need full permission bypass or want to attach directly:

```bash
shed attach <shed> --plan <file> -d [--kind <k>] [--skip] [-p "<framing>"]
```

| Flag | Purpose |
|------|---------|
| `--plan <file>` | Ship a plan file into the shed (`-` reads stdin); the agent is told to execute it. |
| `--kind <k>` | `claude-rc` (default), `codex`, `cursor`, `opencode`, `shell`. (`claude-broker` is a valid `shed attach` kind but takes **no** `--plan`/`--prompt` — it is driven from claude.ai — and `shed plan` rejects it outright, so it is never part of this flow.) |
| `-d, --detach` | Create the session, print its summary (Claude: the `claude.ai` URL), and return. |
| `-p, --prompt <line>` | Framing prepended to the plan kickoff. Multi-line via `--prompt-file`. |
| `--permission-mode <m>` | `default` \| `auto` (default) \| `skip` for all kinds; Claude also accepts `acceptEdits` \| `plan` \| `dontAsk` \| `bypassPermissions`. |
| `--skip` | Shorthand for the generic `skip` mode (full bypass). Confirm with the user first. |
| `--workdir <dir>` | Working directory inside the shed for the RC session (default `$SHED_WORKSPACE`/`$HOME`). |
| `--name <display>` | Session display name (default `<shed>/<slug>`). |
| `--slug <slug>` | Set the session slug (otherwise generated). |

Notes:
- Do **not** use `--edit` / `--plan-edit` in the autonomous flow (they open `$EDITOR`).
- Only Claude kinds produce a `claude.ai/code` URL; for codex/cursor/opencode report the
  slug + watch command instead.
- An old shed image whose baked-in `shed-ext-rc` predates multi-agent RC rejects the new
  `--kind` values / plan delivery with a "recreate the shed" error.

## Log in an agent inside the shed (needs-auth remediation)

| `--kind` | Log in once inside the shed |
|----------|------------------------------|
| `claude-rc` | `shed attach <shed>` → run `claude` → `/login` |
| `codex` | `shed attach <shed>` → run `codex` and complete login (`codex login`) |
| `opencode` | `shed attach <shed>` → run `opencode auth login` |
| `cursor` | `shed attach <shed>` → run `cursor-agent login` |

Servers that mount a persistent agent config dir into sheds make this automatic for
fresh sheds. After logging in, retry the `shed plan` command.

## Watch, list, stop

```bash
shed attach <shed> --slug <slug>          # attach a terminal to rc-<slug>
shed sessions [<shed>] [--all] [--json]   # list (rc-* rows show KIND + RC-STATE)
shed sessions kill <shed> rc-<slug>       # stop the session
```

## Machine targets (`sx`)

`shed plan` only targets sheds. For a **native machine**, the porcelain is `sx`
(unreleased — resolve it per the SKILL's guarded order: `PATH`, then
`cargo run -q -p sx --` from a shed checkout, then the remote engine over SSH).

```bash
sx plan <file> [--on <target>] [--tool <t>] [-p "<framing>"] \
  [--permission-mode <m> | --skip] [--workdir <dir>] [--name <n>] [--slug <s>] [--json]
```

| Flag | Purpose |
|------|---------|
| `<file>` | Plan file, read **locally** and shipped over stdin (non-empty, UTF-8, ≤ 1 MiB). |
| `--on <target>` | `local` (default), `machine:<name>`, `shed:<name>`, `shed:<name>@<server>`. |
| `--tool <t>` | `claude` (default), `codex`, `cursor`, `opencode`. |
| `-p, --prompt <framing>` | Framing prepended to the composed plan kickoff. |
| `--permission-mode <m>` \| `--skip` | Same vocabulary and default (`auto`) as `shed plan`. |
| `--workdir <dir>` | Session working directory on the target (default `$SHED_WORKSPACE`/`$HOME`). |
| `--name <n>` / `--slug <s>` | Display name (default `<shorthost>/<slug>`, bare slug in a shed) / caller-supplied slug. |
| `--json` | Print the raw session DTO instead of the prose summary. |

Machine entries come from the `machines:` section of `~/.shed/config.yaml`. Exit is
non-zero when a waiting create did not reach `ready` — the same contract as `shed plan`.
Observe with `sx ls`, `sx watch <slug> --on <target>`, `sx attach <slug> --on <target>`,
`sx kill <slug> --on <target>`. Full reference: `docs/extensions/sx.md`.
