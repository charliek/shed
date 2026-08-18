---
name: shed-plan
description: "Use when the user wants to hand a plan or task off to a shed to run autonomously — phrases like 'send this plan to a shed', 'run this plan on a remote shed', 'spin up a shed and have it do X', 'kick off an agent on a shed to work on this while I close my laptop', or 'execute this autonomously on mini2/mini3'. This skill authors a plan file and ships it to a shed with a single `shed plan` command, which creates the shed if needed, starts an autonomous agent session (Claude by default; codex/cursor/opencode on request), and reports how to watch it. For everyday shed usage (create/list/attach/exec/sync without the autonomous-plan flow) use the `shed` skill instead."
---

# Shed Plan: run a plan autonomously on a remote shed

Hand a multi-step plan to a **shed** (an isolated remote VM) and let an agent execute
it autonomously. With **Claude** (the default) you keep a `claude.ai/code` URL to watch
and steer from your phone or browser; the laptop can close. The whole flow is one
command — `shed plan` — which creates the shed if it doesn't exist, ships the plan,
starts the agent under an autonomous permission posture, and reports the session.

## Prerequisite: the agent must be logged in inside the shed

Authentication lives **inside the shed VM** — host login does not reach it, and this
skill does not set it up. On servers that mount a persistent agent config dir into
sheds (e.g. the user's local Mac for Claude; codex/opencode often arrive authed via
mounted config), a fresh shed is already authed.

If a run reports **`needs-auth`**, `shed plan` exits non-zero, leaves the session
running, and prints the per-agent remediation. Relay it to the user, then retry:

| Agent (`--kind`) | Log in once inside the shed |
|------------------|------------------------------|
| `claude-rc` (default) | `shed attach <shed>` → run `claude` → `/login` |
| `codex` | `shed attach <shed>` → run `codex` and complete login (`codex login`) |
| `opencode` | `shed attach <shed>` → run `opencode auth login` |
| `cursor` | `shed attach <shed>` → run `cursor-agent login` |

`needs-trust` is similar (a workspace-trust prompt is showing) — the fix is
`shed attach <shed> --slug <slug>` to accept it, then retry.

## The flow

1. **Author the plan.** Work with the user to produce a concrete, self-contained
   markdown plan (goal, steps, acceptance criteria, how to verify). Save it to a local
   file (e.g. `./plan.md` or under the scratchpad). The plan is shipped verbatim and the
   agent is told to execute it to completion, so it must stand on its own — assume no
   further human input mid-run.

2. **Choose the target shed and server.**
   - **Default: a fresh, disposable shed per plan** (clean blast radius), created from
     the repo the work targets. Pick a short, recognizable name (e.g. `plan-<topic>`).
   - **Ask which server** if the user didn't say (e.g. their default, `mini2`, `mini3`).
     Don't guess across servers silently.
   - **Reuse an existing shed only when the user directs it** ("use my shed X"): drop
     `--repo` and target that shed by name.

3. **Ship the plan and start the run — one command:**
   ```bash
   shed plan ./plan.md --shed plan-<topic> --repo <owner/repo> -s <server> -d
   ```
   - `--repo` creates the shed if it's missing (uses `--repo`, not `--local-dir` — the
     value here is remote execution). For an **existing** shed, drop `--repo`; passing it
     on a shed that already exists warns and is ignored. A missing shed with no `--repo`
     is a hard error.
   - `-d/--detach` reports the session and returns instead of dropping you into it. Use
     it for the autonomous, close-the-laptop workflow.
   - Runs under the `auto` permission posture (autonomous with safety checks) — you don't
     pass a mode flag for the default. `--permission-mode`/`--skip` are available directly
     on `shed plan` now (same validation as `shed attach`) if the user wants a different
     posture without dropping to the lower-level `shed attach` primitive.
   - `--workdir <dir>` sets the RC session's working directory inside the shed, if the
     default (`$SHED_WORKSPACE`, falling back to `$HOME`) isn't where the work should
     happen.
   - The plan is written to a HOME-rooted location inside the shed (Claude:
     `~/.claude/plans/plan-<slug>.md`; other agents: `~/.shed-plans/plan-<slug>.md`),
     never the workspace, so it can't dirty a `--repo` clone. The kickoff references its
     absolute path.
   - To lead with your own framing, add `-p "..."` — your prompt runs first and the plan
     location is appended automatically, so you send both in one shot.

4. **Send to a different agent (on request).** Claude is the default. When the user asks
   for codex, cursor, or opencode, add `--kind`:
   ```bash
   shed plan ./plan.md --shed plan-<topic> --repo <owner/repo> -s <server> --kind codex -d
   ```
   Kinds: `claude-rc` (default), `codex`, `cursor`, `opencode`, `shell`.
   **Note:** only Claude sessions have a `claude.ai/code` URL. For codex/cursor/opencode
   there is no browser URL to hand back — report the shed, the `rc-<slug>`, and the watch
   command (`shed attach <shed> --slug <slug>`) instead.

5. **Report back.** Surface what the command printed:
   - Shed name and the `rc-<slug>` session.
   - **Claude only:** the `claude.ai/code/session_…` URL to watch/steer from a phone.
   - Watch / steer: `shed attach <shed> --slug <slug>`
   - Status across sheds: `shed sessions` (shows KIND + RC-STATE for `rc-*` sessions)
   - Stop it: `shed sessions kill <shed> rc-<slug>`

## Target a native machine instead of a shed (`sx`)

When the user wants the plan run on a **machine** rather than a shed ("run it on my
mac-mini", "kick this off on mini2 itself"), the tool is `sx` — the RC porcelain
(`docs/extensions/sx.md`). It is **unreleased and installed nowhere by default**, so
resolve it in this order and stop at the first that works:

1. `sx` on `PATH` (`command -v sx`).
2. A shed checkout on this machine: `cd <shed-repo>/crates && cargo run -q -p sx -- <args>`
   (or `<shed-repo>/crates/target/{debug,release}/sx` if it is already built).
3. **Fallback — the engine over SSH**, when this machine has no `sx` but the
   target machine has one (or a still-installed, retired `shed-machine-rc` —
   the two are wire-identical):
   ```bash
   ssh <machine> sx rc create --kind claude-rc --name "<machine>/plan" \
     --wait --interactive-shell --permission-mode auto --plan-stdin < ./plan.md
   # (swap `sx rc` for `shed-machine-rc` on a machine still running the retired binary)
   ```
4. **Neither available** — say so and stop. Do not improvise a raw `tmux`/`ssh`
   kickoff; the posture flags, installed-agent gate, and trust/onboarding pre-seed are
   exactly what these tools exist to apply.

Live activity for machine sessions comes from the machine RC hub on the target: the
`shed-host-agent` daemon hosts it as a resident role (its home — install and start the
agent to get it), and a still-installed, retired `shed-machine-rc serve` daemon keeps
providing it through the mixed window (the agent defers while that hub holds the port
and takes over when it exits). `sx` probes and hints; it never spawns a hub. A machine
with neither still runs sessions fine; it just reports no live activity in
`sx ls`/`sx watch`.

With `sx` the flow is one command, and the plan file stays local (it is read here and
shipped over stdin):

```bash
sx plan ./plan.md --on machine:<name>          # a machines: entry in ~/.shed/config.yaml
sx plan ./plan.md                              # this machine
sx plan ./plan.md --on shed:<name>@<server>    # a shed, same porcelain
sx plan ./plan.md --on machine:<name> --tool codex
```

Same posture rules as `shed plan`: `auto` by default, `--skip` only on explicit user
request. Report back with `sx watch <slug> --on machine:<name>` (activity stream),
`sx attach <slug> --on machine:<name>` (terminal), `sx kill <slug> --on machine:<name>`.

Prerequisite for a machine target: a `machines:` entry in `~/.shed/config.yaml` (name,
`host`, optional `user`/`ssh_port`/`rc_bin`). If the section is missing, ask the user
before writing one — and warn that a `shed` CLI older than the `machines:` passthrough
deletes the section on its next config rewrite.

**Sheds remain the default.** Use `shed plan` for shed targets unless the user
specifically wants the porcelain; a machine is not an isolated VM, so the blast radius
of an autonomous run is the user's real machine.

## Exit contract (what the non-zero cases mean)

`shed plan` exits **0 only when the session reached `ready` and the kickoff was
delivered.** Any other outcome exits non-zero and **leaves the session/shed in place**
so you can fix and retry — nothing is auto-deleted:

- **`needs-auth` / `needs-trust`** — session created, plan not started. Relay the
  per-agent remediation above and retry.
- **Shed created but the session failed** — the message reports *both* facts (the shed
  was created AND the plan couldn't ship) and that the shed was NOT deleted; retry
  `shed plan ... --shed <name>` after fixing the cause.
- **Old shed image** — a shed whose baked-in `shed-ext-rc` predates multi-agent RC
  rejects `--kind codex|cursor|opencode` and plan delivery with a "recreate the shed"
  message. Recreate it (or use a fresh `--repo` shed).

## Guardrails

- **Prefer `shed plan` over hand-driving `shed attach --plan`.** It is the one-command
  porcelain; `shed attach --kind/--plan` is the lower-level primitive it builds on.
- **Never use `--edit` / `--plan-edit`** in any autonomous flow — they open `$EDITOR`
  and can't run unattended. Always pass the plan as a file (or `-`) and any framing via
  `-p`.
- **Default to `auto`.** Only escalate to full bypass on **explicit user request**, and
  say so. Full bypass is `--skip` (maps to the generic `skip` mode; works on both `shed
  plan` and `shed attach`) — it's safe because a shed is an isolated VM.
- Put the multi-step detail in the **plan** file; keep any `-p` framing to high-level
  context (it may be multi-line, but the plan is where the steps live).
- For the underlying shed operations (create, list, delete, servers), defer to the
  `shed` skill / `shed <cmd> --help`; this skill is the high-level workflow, not a CLI
  manual — see `references/usage.md` for the exact flags it relies on.
