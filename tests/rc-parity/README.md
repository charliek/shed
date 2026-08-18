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
`cursor-agent`) are `sh` shims on a constructed PATH. Each answers `--version` (so
capability discovery probes something deterministic) and records the argv it was
launched with; the **reactive** variants additionally record every stdin byte and
redraw as ready once a keystroke arrives — which is what makes the `--wait`
trust/bypass cells finish in one poll tick instead of eating the 20 s timeout.

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
| preseed artifacts (`~/.claude.json`, `~/.cursor/hooks.json`), the cursor hook script, plan files | **raw bytes** — a mixed fleet rewrites these in place, so key order, indentation, HTML escaping and number fidelity are all contract. The only substitutions are the leg's `HOME` and the per-run pytest workdir. |
| `capabilities` (standalone and embedded in `list`) | structural, with the agent **version values** masked after a `<major>.<minor>.<patch>` shape assert. `installed` booleans are diffed — the shims pin them, including the false case. |
| `--wait` keystrokes | the exact bytes the agent received on stdin, in hex, in order (recorded by a reactive shim). |
| `version` | fully masked after a `<prog> <version>` shape assert. |

Masks: `<id>` (uuid), `<ts>` (RFC3339, shape-asserted first), `<home>`, `<workdir>`
(the per-run pytest dir a preseed records as a `projects` key), `<port>`,
`<version>` (an agent's probed version), `<prog>` (the binary's own name —
`shed-machine-rc` vs `sx` is a designed difference, not a divergence), `<detail>`
(a third-party parser's wording).

## Hermeticity

* **The hub.** Every Go `create` otherwise spawns a detached hub daemon on the fixed
  loopback port **1029**. `SHED_RC_NO_HUB=1` (the C2 oracle seam, honored identically
  by the Rust engine's hook) is set for every leg, and a session-scoped guard asserts
  no test-spawned process ended up holding the port.
* **tmux.** Each *context* gets its own `TMUX_TMPDIR` — a *shallow* `mkdtemp`, because
  an AF_UNIX bind path caps at ~104 bytes and pytest's tmp tree blows past it. Under
  the `isolated` flavor a context is one implementation leg, so the two legs run on
  separate servers and cannot see each other's sessions; under `shared` one context
  serves both. Teardown `kill-server`s every context this test built, which IS the
  session cleanup — a cell may legitimately leave sessions behind for it.
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

**isolated** — each implementation gets its own tmux server + HOME, and both use the
same pinned slug: independent differentials with no cross-talk. Everything except
`test_interop.py` uses this.

**shared** — ONE tmux server + ONE HOME that both implementations drive, selected per
call (`rig.run(impl, verb, …)`). Sharing is not a convenience here, it is the property
under test: interop means one binary reading, prompting and killing a session the
OTHER created (two sealed servers cannot express that), and preseed-in-place means two
binaries merging into one file on one machine, in sequence. Coexisting sessions take
DISTINCT pinned slugs — a shared server is exactly where a repeated slug is the
duplicate-slug error.

A test may ask for several NAMED shared rigs (`shared("chain-go")`,
`shared("chain-rust")`): the interop cells stay differentials by varying *which
implementation drives*, so each direction needs its own world. Teardown is the same
discipline as `isolated` — every rig's server is killed and its `TMUX_TMPDIR` removed.

## Scope today and what is coming

Two families now share the goldens dir and the stale-sweep: the ONE-SHOT family
(plan 009 — 52 cells) and the HUB family (plan 010 — `test_hub.py`, marker
`hub`): resident hub daemons on ephemeral loopback ports via the sanctioned
`SHED_RC_HUB_ADDR`/`SHED_RC_HUB_*_MS` seams, snapshot cells (health identity,
sessions overlay, messages paging, the 4xx/409 verb matrix, bare-mux
status-only) pinned from the Go hub. The hub family's `hub_differential` is
**Go-only until the Rust hub exists** (plan 010 H12) — its goldens are the
frozen wire the Rust port must answer — then it becomes equality-then-pin like
everything else.

The one-shot family — 52 differential cells, one golden each:

* `version`; `create` (DTO + session environment + inner-command argv, `shell` and
  `opencode`); `probe`; `kill` (including its idempotence); `list`; the exit-code
  classes (`test_version.py`, `test_create.py`, `test_probe_kill.py`,
  `test_exit_classes.py`).
* **Preseeds as raw bytes** (`test_preseed.py`): `~/.claude.json` against a seeded
  matrix — absent, empty, `null`, unknown keys with nested objects/arrays, the
  number-fidelity set (`>2^53`, `1e10`, `0.10`, `-0`), HTML/`U+2028`/non-ASCII
  escaping, the trailing-garbage refusal (file untouched, create still exit 0, the
  reason on stderr), and merge idempotence — plus `~/.cursor/hooks.json` (fresh,
  idempotent, user hooks preserved), the hub hook script's bytes and mode, and a
  plan file's bytes and mode (with `--prompt-b64` framing riding alongside without
  touching the file).
* **Capabilities** (`test_capabilities.py`): the payload with two agents
  deliberately off PATH, the full `list` envelope beside a live session, and the
  absence of the block on a bare session DTO.
* **`--wait` transitions** (`test_wait.py`): the trust dialog answered with exactly
  one Enter, the bypass dialog answered with `Down` then Enter (`1b 5b 42 0a`), and
  the single-line vs bracketed-paste kickoff deliveries.

* **Cross-impl interop** (`test_interop.py`, the shared flavor): a Go-created
  session probed + listed identically by both binaries and vice versa; two
  sessions of different kinds created by different binaries coexisting in ONE
  server and enumerated identically by both; the full chain (create with one,
  `prompt` + `kill` with the other, then the creator's `probe` → exit 4) run both
  ways round and compared; `accept-trust` answered across the boundary on a
  reactive trust shim (the transcript proves the single Enter, and the creator
  then reads `ready`); and the preseed files merged in place across the boundary
  — Go→Rust and Rust→Go compared byte for byte against the pure-Go reference
  sequence, for `~/.claude.json` and `~/.cursor/hooks.json`.

The C4 carve-out that stripped `capabilities` from the `list` differential is
**gone** — both implementations embed the block, so the full envelope is compared
and that golden was deliberately re-recorded.

## CI

`.github/workflows/ci.yml` runs this suite as the **`rc-parity (Go↔Rust wire
goldens)`** job (part of the required `ci-success` check), gated on the `rcparity`
path filter: both engines (`internal/ext/rc`, `internal/ext/clirc`,
`crates/shed-core`, `crates/shed-app`), both CLIs (`cmd/shed-machine-rc`,
`cmd/shed-ext-rc`, `crates/sx`), this harness, and the shared build manifests
(`crates/Cargo.*`, `crates/rust-toolchain.toml`, `go.mod`/`go.sum`).

The job installs Go + Rust + uv, `apt-get install`s **tmux** (not preinstalled on
GitHub runners) and asserts the ≥ 3.2 floor before building anything, then runs the
Rust legs no other job covers — `cargo test`/`clippy` for `-p shed-app --features rc`
and `-p sx` — followed by `make test-rc-parity`. It is the only job in that workflow
with a `timeout-minutes`, because it drives real tmux sessions and a wedged pane is a
hang rather than a failure.

### Known sensitivity

Capability discovery has a hard **750 ms** budget shared by all agent probes, and a
laggard degrades to "installed, version unknown" — deliberately, on both sides. If a
machine is loaded enough that one leg's probes miss the budget and the other's do
not, a capabilities cell fails on the `version` key rather than flaking silently.
Re-run before investigating.
