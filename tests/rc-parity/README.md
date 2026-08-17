# tests/rc-parity — the Go↔Rust RC-engine differential harness

The **fourth** pytest suite in this repo, and — like the other three — never merged
with them:

| suite | what it drives |
|---|---|
| `tests/integration/` | a LIVE `shed-server` create cycle |
| `tests/host-agent-diff/` | the `shed-host-agent` daemon's wire output, vs recorded goldens |
| `desktop/tools/shedtest/` | the desktop app over its IPC socket |
| **`tests/rc-parity/`** | **`shed-machine-rc <verb>` vs `sx rc <verb>`, side by side** |

## Purpose

The one-shot RC engine now exists twice: in Go (`internal/ext/rc` + `internal/ext/clirc`,
shipped as `shed-machine-rc` and the guest's `shed-ext-rc`) and in Rust
(`crates/shed-core::rc_agents` + `crates/shed-app::rc_engine`, shipped as `sx`). The Go
side stays alive as the machine hub provider **and as this harness's oracle** (plan
009 §0).

Each cell here runs the SAME scenario against BOTH binaries, asserts the two
normalized results are identical, and then pins the Go value to a committed golden.
A golden is therefore "the wire shape the two implementations agreed on" — the same
provenance `tests/host-agent-diff`'s goldens carry, except both runners are still
here to keep proving it.

## Running

```bash
make test-rc-parity          # from the repo root (uv guard + tmux guard)

# or directly:
cd tests/rc-parity && uv sync && uv run pytest -v
```

Requirements: **Go** (builds `shed-machine-rc`), **Rust/cargo** (builds `sx`), **uv**,
and **tmux ≥ 3.2** (`new-session -e` is how session metadata is stamped — an implicit
floor on BOTH implementations, asserted by the `tmux_bin` fixture).

Nothing real is ever launched: the four agent binaries (`claude`, `codex`, `opencode`,
`cursor-agent`) are `sh` shims on a constructed PATH.

## Recording and updating goldens

```bash
UPDATE_GOLDEN=1 uv run pytest        # (re-)record every visited cell
```

Recording is idempotent by content, so an unchanged golden leaves a clean
`git status`. **A missing golden is a failure, not an auto-record** — for an existing
cell it means the file was deleted, and re-recording would silently bless whatever the
binaries do today.

Guards (inherited from `tests/host-agent-diff`): one `differential()` call per test
(the golden key is the nodeid), case-insensitive key-collision detection (macOS folds
case, CI does not), and a stale-golden sweep on a clean unfiltered run — which stands
down for `-k`/`-m`/explicit paths/`--lf`/`--collect-only` and for any skipped or
failed differential cell.

## Comparison model (plan 009 §3.5)

| surface | model |
|---|---|
| create/list/probe DTO stdout | **structural canonical JSON** — keys sorted, list order kept. Deliberately NOT byte equality: Go's `json.Encoder` HTML-escapes `<`/`>`/`&` and appends a newline, serde_json does neither, and every consumer parses. Field **presence** (Go's `omitempty`) IS contract and the structural compare sees it. |
| exit codes | exact, for the contract classes: 2 bad args, 3 duplicate slug, 4 gone session, and **0 for `kill` on a missing slug** (the idempotence pin). |
| stderr | only the contract classes' messages, masked. Everything else (usage dumps, third-party parser detail) is out of contract. |
| `show-environment` | a sorted key→value mapping of the `SHED_RC_*`/`OPENCODE_*` keys. tmux's render ORDER is a tmux-version detail; `BuildEnvArgs` ordering is pinned by Rust unit tests instead (§3.6). |
| inner-command argv | the exact argv the agent received, order-sensitive (recorded by the shim). |
| preseed artifacts, plan files | **raw bytes** — a mixed fleet rewrites these in place. Arrives with C5. |
| `version` | fully masked after a `<prog> <version>` shape assert. |

Masks: `<id>` (uuid), `<ts>` (RFC3339, shape-asserted first), `<home>`, `<port>`,
`<prog>` (the binary's own name — `shed-machine-rc` vs `sx` is a designed difference,
not a divergence), `<detail>` (a third-party parser's wording).

## Hermeticity

* **The hub.** Every Go `create` otherwise spawns a detached hub daemon on the fixed
  loopback port **1029**. `SHED_RC_NO_HUB=1` (the C2 oracle seam, honored identically
  by the Rust engine's hook) is set for every leg, and a session-scoped guard asserts
  no test-spawned process ended up holding the port.
* **tmux.** Each implementation leg gets its own `TMUX_TMPDIR` — a *shallow* `mkdtemp`,
  because an AF_UNIX bind path caps at ~104 bytes and pytest's tmp tree blows past it —
  so the two legs run on separate servers. Teardown kills each server and asserts no
  `rc-*` session survived.
* **PATH.** `bash -lc`/`-l`/`-ic` rebuild PATH from `/etc/profile` (+ macOS
  `path_helper`), so a prepend on the pytest process vanishes. Each leg therefore gets
  a fresh `HOME` with `.bash_profile`, `.bashrc` and `.profile` prepending its shim dir,
  plus a **constructed** minimal PATH (shim dir, tmux's dir, bash's dir, `/usr/bin`,
  `/bin`) rather than the developer's. Known residual: on a Mac where tmux is
  brew-installed, that dir is also where a brew `shed-machine-rc` lives — visible to
  `sx`'s ensure-hub hook, which the kill-switch and the port guard already cover.
* **Slugs.** Every create pins `--slug` AND `--name`: the two implementations generate
  slugs differently (crypto-rand vs uuid-derived) and `slug`/`tmux_session` are
  deliberately not masked.
* **No sleeps.** Every wait is a deadline poll that reports its last snapshot.

## Isolation flavors

**isolated** (this commit): each implementation gets its own tmux server + HOME, and
both use the same pinned slug — independent differentials with no cross-talk.

**shared** (C6): one server + one HOME serving both implementations, for the
cross-impl interop cells (create with Go, probe/prompt/kill with Rust and vice versa)
and preseed-in-place idempotence.

## Scope today (C4) and what is coming

Here: `version`, `create` (DTO + session environment + inner-command argv, `shell` and
`opencode`), `probe`, `kill` (including its idempotence), `list` on an empty server,
and the exit-code classes.

**The `list` differential strips the `capabilities` block from BOTH sides** — Go's
`doList` always embeds it and the Rust engine has no `capabilities.rs` until C5. That
is a pinned decision (plan 009 §5, C4 row), not a bug: flip `strip_capabilities` in
`normalize.mask_list` at C5 when the full envelope lands.

C5 adds the preseed raw-byte matrix, the capabilities surface, and reactive shims for
the `--wait` trust/bypass auto-accept scenarios. C6 adds cross-impl interop,
multi-line paste delivery, and the CI job.
