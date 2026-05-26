# shed CLI: usage command reference

Client (`shed`) commands only. Server operation (`shed-server setup/serve/install/uninstall/pull-images`) is intentionally omitted — see the repo's setup docs for that. `shed <command> --help` is the authoritative, version-matched source for every flag.

## Global flags

| Flag | Short | Description |
|------|-------|-------------|
| `--server` | `-s` | Target a specific server (overrides the default) |
| `--verbose` | `-v` | More output (`-v` expanded, `-vv` full detail) |
| `--config` | `-c` | Config file path (default `~/.shed/config.yaml`) |
| `--json` | | Structured JSON to stdout (for scripting) |

`--json` + a destructive command requires `--force` (no interactive prompt in JSON mode).

## Servers

| Command | Purpose |
|---------|---------|
| `shed server add <host> [--name <n>] [--port 8080]` | Add a server to client config (SSH port auto-discovered) |
| `shed server list` | List configured servers, online status, default marker |
| `shed server remove <name>` | Remove a server |
| `shed server set-default <name>` | Set the default server |

## Shed lifecycle

`shed create <name> [flags]`

| Flag | Description |
|------|-------------|
| `--repo, -r <owner/repo\|url>` | Clone a repo into the shed |
| `--local-dir <path>` | Mount a host directory as the workspace (mutually exclusive with `--repo`) |
| `--from-snapshot <name>` | Spawn from a snapshot (mutually exclusive with `--repo`/`--image`) |
| `--image, -i <base\|extensions\|full>` | Image variant (default: server default, usually `full`) |
| `--backend <firecracker\|vz>` | Override backend (default: server default) |
| `--cpus <n>` / `--memory <MB>` | Resource overrides |
| `--upper-size <size>` | Size of the per-shed writable upper layer, e.g. `5G`, `20G` (default: server config) |
| `--sync-profile <name>` | Profile to sync after create (default `default`) |
| `--no-sync` / `--no-provision` | Skip sync / skip provisioning hooks |
| `--timeout <dur>` | Create timeout, e.g. `30s`, `5m`, `30m` (default `10m`) |

| Command | Purpose |
|---------|---------|
| `shed list [--all] [-s <server>] [-v]` | List sheds (default server, or all servers) |
| `shed start <name> [--timeout <dur>]` | Start a stopped shed |
| `shed stop <name>` | Stop a running shed (state kept) |
| `shed delete <name> [--keep-volume] [--force/-f]` | Delete a shed (`--force` skips confirm; required with `--json`) |
| `shed reset <name> [--force/-f]` | Discard the per-shed writable upper (in-VM changes); keeps the lower image and the workspace. Shed must be stopped |

```bash
shed create scratch
shed create codelens --repo charliek/codelens
shed create myproj --local-dir ~/projects/myproj
shed create big --repo org/large-repo --timeout 30m
```

## Interactive access & sessions

| Command | Purpose |
|---------|---------|
| `shed attach <name> [-S <session>] [--new]` | Attach to a persistent tmux session (creates if absent). Detach: `Ctrl-B D` |
| `shed console <name>` | Direct bash shell; ephemeral (dies on disconnect). Auto-starts a stopped shed |
| `shed exec <name> <command...> [-S <session>]` | Run a single command over SSH; argv is passed through verbatim (not a shell) |
| `shed sessions [shed] [--all]` | List tmux sessions (one shed, default server, or all servers) |
| `shed sessions kill <shed> <session>` | Kill a tmux session |

```bash
shed attach codelens
shed attach codelens -S debug
shed exec codelens git status
shed exec codelens bash -lc "cd /workspace && npm test"   # shell features need an explicit shell
shed sessions --all
```

## Port forwarding (tunnels)

| Command | Purpose |
|---------|---------|
| `shed tunnels start <shed> [-t <port\|local:remote>]... [-p <profile>] [-d] [--replace]` | Open tunnels (`-d` = background) |
| `shed tunnels stop [shed] [--all]` | Stop tunnels |
| `shed tunnels list [-v] [--json]` | List active tunnels |
| `shed tunnels config <shed> [-p <profile>] [-t ...]` | Preview the mapping without starting |

```bash
shed tunnels start myproj -t 3000 -d
shed tunnels start myproj -t 4501:4096
shed tunnels start myproj -p webdev -t 5432 -d
```

`-t` takes a bare `port` (same local+remote) or `local:remote`. Profiles live in `~/.shed/tunnels.yaml`. Background tunnels track PIDs in `~/.shed/tunnels.state`.

## File sync

| Command | Purpose |
|---------|---------|
| `shed sync <name> [-p <profile>] [-f <feature>] [--dry-run]` | Push local files into a shed per `~/.shed/sync.yaml` |

The `default` profile auto-runs on `shed create` (skip with `--no-sync`). For ad-hoc transfers use `scp`/`rsync`/`sftp` against the `shed-<name>` hosts written by `shed ssh-config`.

## IDE integration

| Command | Purpose |
|---------|---------|
| `shed ssh-config [name] [--all] [--install] [--dry-run] [--uninstall]` | Generate/install SSH config entries for Cursor/VS Code Remote-SSH |

```bash
shed ssh-config --all --install --dry-run   # preview
shed ssh-config --all --install             # apply (managed block in ~/.ssh/config)
```

## Snapshots

A snapshot captures a **stopped** shed's rootfs as a named, immutable artifact; new sheds spawned from it get a fresh identity.

| Command | Purpose |
|---------|---------|
| `shed snapshot create <shed> <snap> [--comment "..."]` | Capture a stopped shed |
| `shed snapshot list` / `shed snapshot info <snap>` | List / inspect snapshots |
| `shed snapshot delete <snap> [-f]` | Remove a snapshot (spawned sheds remain independent) |
| `shed create <new> --from-snapshot <snap>` | Spawn a shed from a snapshot |

## Images

Rootfs images are content-addressed OCI artifacts in variants `base` / `extensions` / `full`; tags point at digests, Docker-style. Most users never touch these — `shed create` pulls what it needs.

| Command | Purpose |
|---------|---------|
| `shed image ls` | List image tags (alias: `list`) |
| `shed image pull <docker-ref> [-t <tag>] [--platform <p>]` | Pull a Docker/OCI ref into the blob store and advance a tag (replaces the old `build --from`) |
| `shed image build [context] -n <name> [-f Dockerfile] [--target <stage>] [--platform <p>] [--force]` | Build a rootfs image from a Dockerfile |
| `shed image tag <src> <new-tag>` | Point a new tag at an existing image |
| `shed image rm <name> [--force]` | Remove a tag (alias: `delete`; the blob is garbage-collected later by `prune`) |
| `shed image prune [--dry-run] [--force]` | Remove cached images not referenced by config or any shed |
| `shed image push <tag-or-digest> <dest-ref> [--local]` | Push an image to a registry, byte-perfect |
| `shed image inspect` / `history` / `save` / `load` | Inspect a manifest, show layer history, or export/import an OCI-layout tar |

`shed image build` no longer has `--size` or `--from`: a shed's writable size is set per-shed with `create --upper-size`, and pulling a prebuilt ref is now `shed image pull <ref>`.

## System (disk usage)

| Command | Purpose |
|---------|---------|
| `shed system df [--all] [-s <server>] [-v] [--json]` | Disk usage: image cache, per-shed rootfs, orphans |
| `shed system prune [--images] [--instances] [--logs] [--orphans] [--until <dur>] [--dry-run] [--force] [--all]` | Reclaim space (additive scopes; previews before mutating) |

```bash
shed system df -v
shed system prune --dry-run
shed system prune --instances --until 1h --force
```

## Utility

| Command | Purpose |
|---------|---------|
| `shed version [-v]` | Version (`-v` includes build/dependency detail) |
