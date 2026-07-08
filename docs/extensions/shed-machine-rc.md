# shed-machine-rc (RC sessions on a native machine)

`shed-machine-rc` is the **host-side** sibling of [`shed-ext-rc`](rc-helper.md): the same
RC Session Convention v2 engine, shipped as a CLI you install on a **native machine** — a
laptop, workstation, or tailnet host — instead of baked into a shed image. It creates,
lists, probes, prompts, and tears down the same `rc-<slug>` `tmux` sessions running
`claude` (or a shell), and prints the same neutral [JSON DTO](rc-helper.md#json-output).

This lets the orchestrators that already drive sheds — shed-remote-agent, and (in future)
shed-mobile — bootstrap and watch `claude remote-control` sessions on machines that aren't
sheds, by invoking it over SSH exactly as they invoke `shed-ext-rc` inside a shed:

```bash
ssh <user>@<machine> shed-machine-rc <command> [flags]
```

It is a one-shot CLI (no daemon); all `tmux` work happens locally on the machine. `claude`
and `tmux` must be installed on the machine (with `claude` authenticated) — the same
prerequisites a shed image bakes in.

## Install

=== "macOS (Homebrew)"

    ```bash
    brew install charliek/tap/shed-machine-rc
    ```

=== "Linux (apt)"

    Add the `apt.stridelabs.ai` repository once (the same repo that serves `shed-server`;
    skip if you already have it), then install:

    ```bash
    sudo install -d -m 0755 /etc/apt/keyrings
    curl -fsSL https://apt.stridelabs.ai/pubkey.gpg | \
      sudo tee /etc/apt/keyrings/apt-charliek.gpg > /dev/null
    echo 'deb [signed-by=/etc/apt/keyrings/apt-charliek.gpg] https://apt.stridelabs.ai noble main' | \
      sudo tee /etc/apt/sources.list.d/apt-charliek.list
    sudo apt update
    sudo apt install shed-machine-rc
    ```

    The `.deb` installs to `/usr/local/bin`.

## `claude` — start a session and walk away

The host-only convenience verb starts a local `claude` remote-control session in the
autonomous **`auto`** posture, waits until it is ready, prints the `claude.ai` URL, and
returns — leaving the session live in `tmux` and watchable from your phone or from
shed-remote-agent / shed-mobile:

```bash
shed-machine-rc claude
#   Started claude-rc session "mymac/ab12cd" — permission-mode=auto (tools run UNATTENDED).
#     Watch/steer from your phone or browser: https://claude.ai/code/session_…
#     Attach locally:  tmux attach -t rc-ab12cd
#     Visible to shed-remote-agent / shed-mobile on this machine.
```

| Flag | Effect |
|------|--------|
| `--name <display>` | Display name (default `<hostname>/<slug>`). |
| `--workdir <dir>` | Working directory (default `$SHED_WORKSPACE`, then `$HOME`). |
| `--slug <s>` | Caller-supplied slug (generated when empty). |
| `--permission-mode <m>` | Override the posture (default `auto`); see [permission modes](rc-helper.md#permission-modes). |
| `--skip` | Shorthand for `--permission-mode bypassPermissions` (mutually exclusive with `--permission-mode`). |

!!! warning
    The `claude` verb runs the session **unattended** (`auto` by default, full bypass with
    `--skip`). It is a deliberate, human-initiated local convenience — review what you are
    handing off before you walk away.

## Other commands

`create`, `list`, `probe`, `accept-trust`, `prompt`, and `kill` are **identical** to
`shed-ext-rc` — see the [RC Session Helper reference](rc-helper.md) for the full command
table, kinds, permission modes, the JSON DTO, exit codes, and the workspace-trust /
onboarding pre-seed. Two host-specific differences:

- The default `--created-by` provenance is `shed-machine-rc` (vs `shed-ext-rc`).
- An orchestrator invoking it over SSH must be able to find it on the machine's
  **non-login** `PATH`. The `.deb` installs to `/usr/local/bin` (already searched); a
  Homebrew install on Apple-Silicon macOS lands in `/opt/homebrew/bin`, which a non-login
  SSH shell may not search — point the orchestrator at an absolute path in that case (e.g.
  shed-remote-agent's per-machine `rc_bin`).
