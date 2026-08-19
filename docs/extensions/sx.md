# sx (RC session porcelain)

`sx` is the **kickoff and observe porcelain** for RC agent sessions: one command starts an
agent on this machine, on a native machine over SSH, or inside a shed, and the same
command set lists, watches, attaches to, and kills those sessions wherever they run.

It is a Rust CLI built from `crates/sx` on the shared client core (`shed-core` +
`shed-app`). Two layers live in one binary:

| Layer | Commands | Contract |
|------|----------|----------|
| Porcelain | `agent`, `plan`, `ls`, `watch`, `attach`, `kill` | Human/skill-facing. Chooses defaults, prints prose, reaches remote targets. |
| Engine-compat | `sx rc <subcommand>` | The one-shot RC engine — the frozen wire a `machine:` target speaks, and what the [retired `shed-machine-rc`](shed-machine-rc.md) used to serve (the `tests/rc-parity` differential suite is the standing proof). |

!!! warning "Unreleased"
    `sx` ships in no release component — no Homebrew formula, no `.deb`, no GitHub
    release artifact. It is built from source (below) and its surface may change without
    a deprecation cycle. `shed-ext-rc` remains the released, supported guest RC
    binary. Machine targets need `sx` present on the far side — see
    [Build and install](#build-and-install).

## Build and install

```bash
cd crates && cargo build --release -p sx
# binary at crates/target/release/sx — put it on PATH yourself, e.g.:
ln -sf "$PWD/target/release/sx" ~/.local/bin/sx
```

Requirements on whichever host actually runs a session: **tmux ≥ 3.2** (`new-session -e`
is how session metadata is stamped — an implicit floor the Go engine shares), `bash`, and
the agent CLI itself (`claude`, `codex`, `cursor-agent`, `opencode`) installed and
authenticated. Remote targets additionally need `ssh` on this machine and an RC binary on
the far side.

## Targets

Every verb takes `--on <target>`; omitting it means `local`.

| Target | Meaning | How the verb reaches it |
|--------|---------|-------------------------|
| `local` (default) | This machine | In-process engine, local `tmux` |
| `machine:<name>` | A `machines:` entry (below) | `ssh <machine> <rc_bin> …` |
| `shed:<name>` | A running shed, located across every configured server | `ssh <shed> shed-ext-rc …` |
| `shed:<name>@<server>` | The same, pinned to one server (skips the HTTP lookup) | `ssh <shed> shed-ext-rc …` |

An unqualified `shed:<name>` is resolved once — asking each configured server for its
running sheds — and then pinned to the server that answered, so later lookups (and the
watch stream) never guess a different host. An ambiguous name is an error naming the
candidate servers, never a silent pick.

## The `machines:` config section

Machine targets come from a `machines:` section in `~/.shed/config.yaml`, alongside the
existing `servers:` section:

```yaml
machines:
  mini2:
    host: mini2.local
    user: charliek
    ssh_port: 22
    rc_bin: /opt/homebrew/bin/shed-machine-rc
    known_hosts: /Users/charliek/.ssh/known_hosts_shed
```

| Field | Default | Purpose |
|-------|---------|---------|
| `host` | the entry name | SSH hostname or IP. |
| `user` | ssh's own default | SSH login user. |
| `ssh_port` | `22` | SSH port. |
| `rc_bin` | `sx` | Where `sx` lives on that machine — the remote invocation is always `<rc_bin> rc <verb>`. Set an absolute path when `sx` is not on the **non-login** `PATH` an SSH exec sees (Homebrew's `/opt/homebrew/bin` typically is not). |
| `known_hosts` | ssh's own default | `UserKnownHostsFile` to pin against; same semantics as a server entry. |

!!! warning "Older `shed` CLIs delete this section"
    The Go `shed` CLI rewrites the whole config document whenever it updates it (`shed
    server add`, a shed-cache refresh, a token mint, even `shed delete`). A release that
    predates the `machines:` passthrough **silently drops the section on the next such
    command** — observed live during this feature's validation. Until you are running a
    `shed` build that carries the passthrough, keep a copy of the block and re-add it if
    a `shed` command eats it.

## Verbs

| Command | Purpose |
|---------|---------|
| `sx agent <tool> [flags]` | Start a session for `claude`, `codex`, `cursor`, `opencode`, or `shell` and report it. |
| `sx plan <file> [flags]` | Ship a plan document to a fresh session and kick it off. |
| `sx ls [--on <target>]` | One table of every reachable session: local, every machine, every running shed. |
| `sx watch <slug> [--on <target>]` | Line-stream a session's activity and message feed. |
| `sx attach <slug> [--on <target>] [--print]` | Hand the terminal to the session's tmux pane (`--print` emits the command instead). |
| `sx kill <slug> [--on <target>]` | Tear the session down. |
| `sx rc <subcommand>` | The engine-compat surface (see below). |
| `sx version`, `sx help` | Version, usage. |

### Kickoff flags

Shared by `sx agent` and `sx plan`:

| Flag | Effect |
|------|--------|
| `--on <target>` | Where to run it (default `local`). |
| `-p`, `--prompt <text>` | A kickoff line, or — with a plan — the framing prepended to the plan's kickoff. |
| `--permission-mode <m>` | `default` \| `auto` \| `skip` for every kind; claude also accepts `acceptEdits` \| `plan` \| `dontAsk` \| `bypassPermissions`. See [permission modes](rc-helper.md#permission-modes). |
| `--skip` | Shorthand for `--permission-mode skip` (full bypass). Mutually exclusive with `--permission-mode`. |
| `--workdir <dir>` | Session working directory (default `$SHED_WORKSPACE`, then `$HOME`, resolved on the target). |
| `--name <display>` | Display name (defaults below). |
| `--slug <s>` | Caller-supplied slug (generated when absent). |
| `--json` | Print the raw session DTO instead of the prose summary. |

`sx agent` additionally takes `--plan <file>` and `--no-wait`; `sx plan` additionally
takes `--tool <t>` (default `claude`) and always waits.

```bash
sx agent claude -p 'triage the failing integration test'
sx agent opencode --on machine:mini2
sx plan ./plan.md --on shed:plan-topic@mac-mini
sx ls
sx watch ab12cd --on machine:mini2
sx attach ab12cd --print
sx kill ab12cd
```

### Dispatch rules

The rules that change behavior per target and per tool:

| Rule | Behavior |
|------|----------|
| Interactive shell | `local` and `machine:` sessions wrap the agent in `bash -ic`, so a tool installed by a shell rc-file (nvm, mise, asdf, brew shellenv) is on `PATH`. **`shed:` targets do not** — the SSH `bash -lc` wrap already supplies the guest's login `PATH`, and there is no interactive rc-file there. |
| Display name | `<shorthost>/<slug>` on `local` and `machine:` targets; the **bare slug** in a shed, where the server already renders `<shed>/<slug>`. `--name` overrides. |
| Permission posture | `sx agent claude` defaults to `auto` (the posture the `shed-machine-rc claude` verb it absorbs used). Every other tool gets **no posture flag** — its own default — because "auto" means something different in each agent's CLI. `sx plan` defaults to `auto` for every tool, matching `shed plan`. |
| Waiting | Kickoff verbs wait for the session to reach `ready` and then deliver the prompt/plan. `--no-wait` (agent only) is **rejected together with `-p`/`--plan`**: a kickoff is delivered only after the pane is ready, so "don't wait" and "deliver this" contradict each other. |
| Exit code | A waiting create that did not reach `ready` exits non-zero (a script can tell `ready` from `needs-auth`/`starting`); `--no-wait` always exits `0`. Remote engine exit classes `2`/`3`/`4` pass through the SSH hop verbatim; anything else — including a transport failure — collapses to `1`. |
| Hub | On a create, `sx` best-effort probes the [machine hub](#the-machine-hub) (`GET /v1/health` on the fixed port); when nothing healthy answers it prints a one-line stderr hint naming `shed-host-agent` as the hub's owner — never fatal, and it never spawns anything. Set `SHED_RC_NO_HUB=1` to skip it entirely. |

### Observing

`sx ls` fans out over local, every `machines:` entry, and every running shed on every
configured server, sorted by target. A per-target failure becomes an annotated line under
the table, never the end of the listing. The fan-out is sequential in v1.

The `WATCH` column is capability-aware (see
[`kind_features`](rc-helper.md#kind_features-matrix)):

| What the `list` envelope carried | `WATCH` |
|---|---|
| A `kind_features` row with a message feed | `feed` |
| A `kind_features` row without one | `activity` |
| No row for that kind (`shell`, `claude-broker`) | `-` |
| No `capabilities` block at all (an older RC binary) | `?`, plus a note naming the target |

`sx watch` picks its transport from the same capability data, never from a bare error:

| Target | Feed transport |
|--------|----------------|
| `local` | The hub on `127.0.0.1:1029` |
| `machine:<m>` | `ssh -N -L <ephemeral>:127.0.0.1:1029 <m>`, then the same client |
| `shed:<s>` | The server's aggregate `GET /api/rc/events` plus the `/messages` proxy |

When the kind advertises no message feed, the RC binary predates capability discovery, or
the hub is unreachable (including `RC_HUB_UNAVAILABLE`, and including a hub that dies
mid-stream), `sx watch` **degrades to probe polling** and prints one note saying why. A
missing hub is a degraded feed, not a failed command.

## Engine-compat surface (`sx rc`)

`sx rc <subcommand>` is the ported one-shot engine: `create`, `list`, `capabilities`,
`probe`, `accept-trust`, `prompt`, `kill`, `version` — the same flags, the same stdin
framing (`--prompt-stdin` / `--plan-stdin` / `--prompt-b64`), the same
[JSON DTO](rc-helper.md#json-output), and the same
[exit codes](rc-helper.md#exit-codes) as `shed-machine-rc`. Use it when a caller wants
the machine-readable engine rather than the porcelain's prose.

The hub's `serve` verb is **not** ported and has no `sx rc` equivalent.

That compatibility is enforced, not asserted: `tests/rc-parity/` runs each scenario
against both binaries side by side, requires the normalized results to match, and pins
the agreed value to a committed golden. Preseed artifacts (`~/.claude.json`,
`~/.cursor/hooks.json`, the cursor hook script, plan files) are compared as **raw bytes**
— a mixed fleet rewrites those files in place, so merge idempotence only survives if both
implementations write identical bytes. Run it with `make test-rc-parity`.

## The machine hub

Live activity on a machine (`sx ls` activity columns, `sx watch`) comes from the
**machine RC hub** — the same loopback HTTP service a shed runs, bound to
`127.0.0.1:1029`. **`shed-host-agent` hosts it**, as a supervised resident role:
the daemon binds the port at startup and keeps the hub up for as long as it
runs. Opt out with `rc_hub.enabled: false` in the agent's config.

Because the daemon is supervised (brew services / systemd), the hub does not
come and go with session activity — unlike the retired `shed-machine-rc serve`,
which exited after 15 idle minutes. If some other process already holds the
port, the agent logs it, retries with backoff, and takes over when the port
frees.

`sx` itself is **probe-only**: create's best-effort hub ensure checks
`GET /v1/health` on the fixed port and is done if a healthy hub answers;
otherwise it prints a one-line stderr hint naming the agent as the hub's
owner. It never starts a hub. Sessions work without one; only live activity
is missing.

**The trust model is the machine's own.** There is no server proxy on a
machine: the hub binds loopback only and does no authorization — the loopback
bind plus your SSH tunnel (`ssh -L`) IS the boundary. Never widen the bind.
Note what "local" means here: every process of every app running under any
uid that can reach loopback on the machine — not a sandboxed VM. That is
still the machine's existing trust boundary (a local process that could POST
to the hub's cursor-hook ingest could already drive the same tmux session
directly with `send-keys`), so the hub adds a convenience channel within
local trust, not a new boundary — but the scope of "local" is the whole
machine, and it is worth saying plainly.

**Machine-posture deltas from the guest hub** (deliberate, not drift): inside
the agent the hub is a supervised resident role — no 15-minute idle exit, no
detach double-fork, no pidfile; at zero sessions the watchers quiesce and the
recurring cost is one `tmux ls` per idle tick. The agent's bind loop retries
rather than exits, so a permanently held port shows up as `RC hub: deferred`
in `shed-host-agent status`, not a dead daemon.

## v1 limits

- **mTLS-enrolled servers need the shed host agent running.** Locating an unqualified
  `shed:<name>`, and the full `sx ls` fan-out, both query the HTTP API. A server enrolled
  for mTLS holds no static `control_token`, so its credential has to be minted: `sx`
  connects to `shed-host-agent` over its UDS and mints one exactly as the desktop does.
  The wiring is gated on the agent actually answering — with no agent running (or a stale
  socket left by a crashed one) `sx` falls back to the static `control_token` from each
  server entry, which is enough for token-mode servers and leaves an mTLS server's sheds
  out of the listing. The inverse also holds: **with** a live agent the minter takes over
  for *every* server, so a token-mode server whose config token works but whose host-agent
  SSH key is not allowlisted there drops out of the fan-out (the same posture the desktop
  runs — the agent is expected to hold keys for the servers this machine uses).
  `--on shed:<name>@<server>` is pure config plus SSH: it needs no credential, no agent,
  and always works.
- **`sx watch` is a line stream**, not a TUI.
- **No steering verbs.** `turn`, `interrupt`, and approval responses ride the hub/proxy
  HTTP surface consumed by the desktop and mobile clients; they are not in `sx` v1.
- **Cursor workspace trust is handled automatically.** The engine launches
  `cursor-agent --trust` (both implementations, lockstep), so a fresh workspace
  goes straight to the ready composer instead of stalling at the trust dialog —
  the same posture as the claude kinds' trust preseed.
- **No auto-install.** A skill or script invoking `sx` must handle its absence itself.

## See also

- [`shed-ext-rc` (RC session helper)](rc-helper.md) — the wire contract, kinds,
  permission modes, JSON DTO, exit codes, and the activity hub.
- [`shed-machine-rc`](shed-machine-rc.md) — the retired Go engine this
  replaced (tombstone).
