---
name: shed
description: "Use when the user works with shed, the CLI for persistent VM-based development environments, or mentions a shed, sheds, or ~/.shed config. Covers day-to-day client usage: creating, listing, starting, stopping, and deleting sheds; connecting via console or attaching to persistent tmux sessions; running commands with exec; forwarding ports with tunnels; syncing files; managing images, snapshots, and disk usage; and working across multiple servers. Also reach for it on phrases like 'spin up a shed', 'attach to my shed', 'create a shed for this repo', or 'run it on a shed'. This is the shed dev-environment CLI specifically; do not use it for operating or installing the shed-server daemon (server/admin setup), or for unrelated meanings of the word 'shed'."
---

# Shed: Remote Dev Environments

shed is a CLI for persistent, VM-based development environments that live on one or more servers. You create a **shed** (an isolated Linux VM, optionally cloned from a repo, with AI tools like Claude Code preinstalled), attach to it over SSH, work inside it, disconnect, and reconnect later with your work intact. Backends are Apple VZ (macOS Apple Silicon) and Firecracker (Linux); as a user you rarely care which.

## Scope: using sheds, not running the server

This skill covers **using the `shed` client** day-to-day. Operating the server — the `shed-server` daemon, `shed-server setup`/`serve`/`install`, backend/KVM/systemd configuration, and `server.yaml` — is out of scope. For server setup, point the user to the repo docs (`docs/getting-started/vz-setup.md`, `docs/getting-started/fc-setup.md`).

## Mental model

- A **shed** is a named, persistent VM that lives on a **server**. The client can target several servers; commands act on the default server unless you pass `-s/--server`, and `--all` fans out across all of them (for `list`, `sessions`, `system`).
- **The VM persists** after you disconnect. To also keep the *programs* inside alive (an agent, a dev server), run them in a **tmux** session via `shed attach` — not `shed console`.
- A shed's rootfs is a shared, read-only **image** (variants `base`/`extensions`/`full`) plus a per-shed **writable upper layer**. `shed reset` discards everything written inside the VM and starts fresh from the image, while leaving the workspace intact.
- The client is configured by `~/.shed/config.yaml` (servers + a cache of known sheds). Networking assumes a private network (Tailscale); auth is SSH-key based, so there is no login/token flag.

## First step: know the servers and sheds

Before acting, read the lay of the land:

```bash
shed server list          # configured servers + which is default/online
shed list                 # sheds on the default server (use --all for every server)
shed list -v              # expanded: backend, status, SSH target, IP, resources
```

If no server is configured yet, add one (one-time): `shed server add <host> --name <name>`.

## Core workflow

```bash
shed create myproj --repo charliek/myrepo   # create a shed and clone a repo into it
shed attach myproj                          # open a persistent tmux session (auto-starts the shed)
#   ...work inside; e.g. run `claude` or a dev server...
#   detach with Ctrl-B D — the VM AND the tmux session keep running
shed attach myproj                          # later: reattach exactly where you left off
shed stop myproj                            # stop the VM when done (state is kept; `shed start` resumes)
```

Other ways to create: `--local-dir ~/path` mounts a host directory as the workspace (instead of `--repo`); `--from-snapshot <name>` spawns from a saved snapshot; bare `shed create myproj` makes an empty shed.

## Commands you will use most

| Task | Command |
|------|---------|
| Create a shed | `shed create <name> [--repo owner/repo \| --local-dir PATH]` |
| List / inspect | `shed list [--all] [-v]` |
| Start / stop / delete | `shed start <name>` · `shed stop <name>` · `shed delete <name> [--force]` |
| Reset the writable layer | `shed reset <name>` (discard in-VM changes, keep the workspace; shed must be stopped) |
| Persistent session (preferred) | `shed attach <name> [-S <session>]` (detach: `Ctrl-B D`) |
| Ephemeral shell | `shed console <name>` |
| One-off command | `shed exec <name> <command...>` |
| List / kill sessions | `shed sessions [--all]` · `shed sessions kill <shed> <session>` |
| Forward ports | `shed tunnels start <name> -t <port> -d` |
| Sync local files in | `shed sync <name> [-p <profile>]` |
| IDE (Cursor/VS Code) | `shed ssh-config --all --install` |
| Disk usage / cleanup | `shed system df` · `shed system prune` |
| Snapshots | `shed snapshot create <shed> <snap>` · `shed create <new> --from-snapshot <snap>` |

Usage notes that matter:

- **`console` vs `attach`:** `console` is a direct shell that dies on disconnect; `attach` is a tmux session that survives. For anything long-running (agents, dev servers), use `attach`.
- **One-off vs interactive:** `shed exec <name> <command...>` runs a single command over SSH with the argv passed through verbatim (it is not a shell), e.g. `shed exec myproj git status`. For pipes, `&&`, redirection, or `cd`, wrap it yourself: `shed exec myproj bash -lc "cd /workspace && npm test"`. Reserve `attach`/`console` for interactive work.
- **Multi-server:** target one with `-s <server>`; sweep all with `--all` (on `list`, `sessions`, `system`).
- **Scripting:** `--json` emits structured output. Destructive commands require `--force` when combined with `--json` (no interactive prompt).

## Reaching services inside a shed (tunnels)

Forward a VM port to localhost:

```bash
shed tunnels start myproj -t 3000 -d        # localhost:3000 -> shed :3000, in background
shed tunnels start myproj -t 4501:4096      # different local:remote ports
shed tunnels start myproj -p webdev -d      # a named profile from ~/.shed/tunnels.yaml
shed tunnels list                           # active tunnels    (stop: shed tunnels stop myproj)
```

## Getting files in (sync)

`shed sync` pushes local files (dotfiles, certs, scripts) into a shed using profiles/features from `~/.shed/sync.yaml`; the `default` profile runs automatically on `shed create` (skip with `--no-sync`).

```bash
shed sync myproj            # default profile      (preview with --dry-run)
shed sync myproj -p full    # a named profile
```

For ad-hoc transfers, `scp`/`rsync`/`sftp` work through the SSH config that `shed ssh-config --all --install` writes (hosts named `shed-<name>`).

## CLI help is authoritative

For exact flags and subcommands, run `shed <command> --help` (and `shed <command> <subcommand> --help`). The built-in help is comprehensive and always matches the installed version.

## References

- `references/commands.md` — usage command + flag reference (client commands only)
- `references/configuration.md` — `~/.shed/config.yaml`, `sync.yaml`, and `tunnels.yaml`
