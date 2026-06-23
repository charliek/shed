---
name: shed-plan
description: "Use when the user wants to hand a plan or task off to a shed to run autonomously — phrases like 'send this plan to a shed', 'run this plan on a remote shed', 'spin up a shed and have it do X', 'kick off an agent on a shed to work on this while I close my laptop', or 'execute this autonomously on mini2/mini3'. This skill authors a plan file, creates (or reuses) a shed on a server, ships the plan, and starts an autonomous Claude Remote Control session you can watch from claude.ai. For everyday shed usage (create/list/attach/exec/sync without the autonomous-plan flow) use the `shed` skill instead."
---

# Shed Plan: run a plan autonomously on a remote shed

Hand a multi-step plan to a **shed** (an isolated remote VM) and let a Claude agent
execute it autonomously. You keep a `claude.ai/code` URL to watch and steer from your
phone or browser; the laptop can close. Built on `shed attach`'s Remote Control mode,
which drives the in-shed `shed-ext-rc` binary over SSH.

Prerequisite: **Claude must be logged in inside the shed.** This is assumed, not set
up by this skill — host login does not reach the shed VM. On servers that mount a
persistent Claude config dir into sheds (e.g. the user's local Mac), a fresh shed is
already authed. If a run reports `needs-auth`, tell the user to
`shed attach <shed>` → run `claude` → `/login` once, then retry.

## The flow

1. **Author the plan.** Work with the user to produce a concrete, self-contained
   markdown plan (goal, steps, acceptance criteria, how to verify). Save it to a local
   file (e.g. `./plan.md` or under the scratchpad). The plan is shipped verbatim and
   the agent is told to execute it to completion, so it must stand on its own — assume
   no further human input mid-run.

2. **Choose the target shed.**
   - **Default: create a fresh, disposable shed per plan** (clean blast radius). Clone
     the repo the work targets: `shed create <name> --repo <owner/repo> -s <server>`.
     Use `--repo` (not `--local-dir`) — the value here is remote execution.
   - **Ask which server** if the user didn't say (e.g. their default, `mini2`, `mini3`).
     Don't guess across servers silently.
   - **Reuse an existing shed only when the user directs it** ("use my shed X"): skip
     `create` and target that shed.
   - Pick a short, recognizable shed name (e.g. `plan-<topic>`).

3. **Ship the plan and start the run (detached):**
   ```
   shed attach <shed> --plan ./plan.md -d
   ```
   - Defaults to `auto` permission mode (autonomous with safety checks).
   - Pass `--skip` **only if the user explicitly asks for full bypass** (no permission
     prompts at all) — confirm first; it is safe because the shed is an isolated VM.
   - To override the default kickoff prompt, add `-p "..."` (a single line); otherwise
     a default prompt that references the shipped plan is used.

4. **Report back.** Surface the shed name, the `rc-<slug>` session, the
   `claude.ai/code/session_…` URL, and how to follow along:
   - Watch / steer: `shed attach <shed> --slug <slug>`
   - Status across sheds: `shed sessions` (shows kind + state for `rc-*` sessions)
   - Stop it: `shed sessions kill <shed> rc-<slug>`

## Guardrails

- **Never use `--edit` / `--plan-edit`** here — they open `$EDITOR` and can't run
  unattended. Always pass the plan via `--plan <file>` and any prompt via `-p`.
- **Default to `auto`.** Only use `--skip` on explicit user request, and say so.
- Keep the kickoff **prompt** a single line; put all multi-step detail in the **plan**.
- If `shed attach` reports `needs-auth`/`needs-trust`, relay the one-time fix rather
  than retrying blindly.
- For the underlying shed operations (create, list, delete, servers), defer to the
  `shed` skill / `shed <cmd> --help`; this skill is the high-level workflow, not a CLI
  manual — see `references/usage.md` for the exact flags it relies on.
