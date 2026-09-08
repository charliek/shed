# tests/rc-parity — the Go↔Rust RC-hub differential harness

One of the **five** pytest suites in this repo, and — like the other four — never
merged with them:

| suite | what it drives |
|---|---|
| `tests/integration/` | a LIVE `shed-server` create cycle |
| `tests/host-agent-diff/` | the `shed-host-agent` daemon's wire output, vs recorded goldens |
| `desktop/tools/shedtest/` | the desktop app over its IPC socket |
| `tests/machine-transport/` | the machine SSH argv contract, every wire line through a real sshd |
| **`tests/rc-parity/`** | **`shed-machine-rc serve` vs `shed-host-agent rc-hub`, side by side** |

## Purpose

The resident RC hub exists twice: in Go (`internal/ext/rc`, served by
`shed-machine-rc serve --foreground`) and in Rust (`crates/shed-broker::rc_hub`,
mounted by `shed-host-agent rc-hub`). The Go side stays alive **as this harness's
oracle** (plan 009 §0): the machine-facing `shed-machine-rc` binary retired at
plan 010 H15, and its main lives on test-only as `tests/rc-parity/oracle/main.go`
— byte-identical behavior (same `clirc.Run` call, same `shed-machine-rc` ProgName
the goldens were recorded under), built by this suite, shipped nowhere.

Each cell here runs the SAME scenario against BOTH daemons, asserts the two
normalized `/v1` results are identical, and then pins the Go value to a committed
golden. A golden is therefore "the wire shape the two implementations agreed on" —
the same provenance `tests/host-agent-diff`'s goldens carry, except both runners
are still here to keep proving it.

**Both legs are stimulated by the Go oracle CLI, on purpose.** The Rust leg's
session-creation stimulus was `sx rc create` until plan 016 (S7,
`charliek/shed#329`) sunset that crate; the surviving cells invoke the CLI only
for `create`, and the retired one-shot family had already proved the two engines
wire-identical there — so one shared stimulus costs the differential nothing and
leaves the DAEMON as its only controlled variable. This is not a wiring bug to
"fix" later. Because a shared stimulus means the goldens can no longer tell the
two hubs apart, the wiring is asserted directly instead: `conftest.start_hub`
checks each leg's daemon basename and records its argv as the first line of that
leg's `$HOME/hub.log`, and `test_hub_wiring.py::test_legs_run_distinct_daemons`
pins that the two legs launch different binaries. There is deliberately no switch
for running the Go leg alone.

## Running

```bash
make test-rc-parity          # from the repo root (uv guard + tmux guard)

# or directly:
cd tests/rc-parity && uv sync && uv run pytest -v
```

Requirements: **Go** (builds the oracle from `tests/rc-parity/oracle`),
**Rust/cargo** (builds `shed-host-agent`), **uv**, and **tmux ≥ 3.2**
(`new-session -e` is how session metadata is stamped — an implicit floor on BOTH
implementations, asserted by the `tmux_bin` fixture).

Nothing real is ever launched: the four agent binaries (`claude`, `codex`, `opencode`,
`cursor-agent`) are `sh` shims on a constructed PATH. Each answers `--version` (so
capability discovery probes something deterministic), draws a fixed pane, then holds
it open on stdin.

## Recording and updating goldens

```bash
UPDATE_GOLDEN=1 uv run pytest        # (re-)record every visited cell
```

Recording is idempotent by content, so an unchanged golden leaves a clean
`git status`. **A missing golden is a failure, not an auto-record** — for an existing
cell it means the file was deleted, and re-recording would silently bless whatever the
binaries do today.

Guards (inherited from `tests/host-agent-diff`): one `hub_differential()` call per
test (the golden key is the nodeid), case-insensitive key-collision detection (macOS
folds case, CI does not), and a stale-golden sweep on a clean unfiltered run — which
stands down for `-k`/`-m`/explicit paths/`--lf`/`--collect-only` and for any skipped or
failed differential cell.

## Comparison model (plan 009 §3.5)

| surface | model |
|---|---|
| `/v1` JSON bodies | **structural canonical JSON** — keys sorted, list order kept. Deliberately NOT byte equality: Go's `json.Encoder` HTML-escapes `<`/`>`/`&` and appends a newline, serde_json does neither, and every consumer parses. Field **presence** (Go's `omitempty`) IS contract and the structural compare sees it. |
| HTTP status codes | exact, including the 4xx/409 verb matrix and the stream-cap 413-vs-400 pins. |
| SSE frames | the decoded `event:`/`data:` pairs in arrival order (the within-tick ordering IS wire); `: ok` openers and heartbeats are liveness, not wire, and are dropped. |

Masks: `<id>` (uuid), `<ts>` (RFC3339, shape-asserted first), `<seq>` (a feed
row's sequence number), `<home>`, `<pid>` (two distinct daemons), `<version>` (a
daemon's reported version), `<prog>` (the binary's own name).

A session's tmux `show-environment` is still READ (`hub_opencode.lane_session`
takes the allocated opencode port, the workdir and the back-written session pin
from it) but is no longer a compared surface: the cells that pinned the
`SHED_RC_*` stamps were one-shot ones.

## Hermeticity

* **The hub.** Every Go `create` otherwise spawns a detached hub daemon on the fixed
  loopback port **1029**. `SHED_RC_NO_HUB=1` (the C2 oracle seam, honored identically
  by the Rust engine's hook) is set for every leg, and a session-scoped guard asserts
  no test-spawned process ended up holding the port.
* **tmux.** Each *context* gets its own `TMUX_TMPDIR` — a *shallow* `mkdtemp`, because
  an AF_UNIX bind path caps at ~104 bytes and pytest's tmp tree blows past it. A
  context is one leg, so the two legs run on separate servers and cannot see each
  other's sessions. Teardown `kill-server`s every context this test built, which IS
  the session cleanup — a cell may legitimately leave sessions behind for it.
* **PATH.** `bash -lc`/`-l`/`-ic` rebuild PATH from `/etc/profile` (+ macOS
  `path_helper`), so a prepend on the pytest process vanishes. Each leg therefore gets
  a fresh `HOME` with `.bash_profile`, `.bashrc` and `.profile` prepending its shim dir,
  plus a **constructed** minimal PATH (shim dir, tmux's dir, bash's dir, `/usr/bin`,
  `/bin`) rather than the developer's. Known residual: on a Mac where tmux is
  brew-installed, that dir is also where a brew `shed-machine-rc` lives — visible to
  the engine's ensure-hub hook, which the kill-switch and the port guard already
  cover.
* **Slugs.** Every create pins `--slug` AND `--name`: `slug`/`tmux_session` are
  deliberately not masked, so the two legs' sessions must be named identically for
  the DTOs to compare.
* **No sleeps.** Every wait is a deadline poll that reports its last snapshot.

## Legs

Each leg is one hermetic context — its own `HOME`, its own tmux server, its own
shim PATH — plus one resident hub daemon on its own ephemeral loopback port
(`SHED_RC_HUB_ADDR`), started with identical fast-tick tuning
(`SHED_RC_HUB_*_MS`) so no cell ever settle-and-compares. The `hub_leg` fixture
builds at most one leg per impl and tears both down, daemon first.

The two legs differ in exactly one thing, their **daemon**:

| leg | CLI stimulus | daemon under test |
|---|---|---|
| `go` | the Go oracle | `shed-machine-rc serve --foreground` |
| `rust` | the Go oracle | `shed-host-agent rc-hub` |

The shared stimulus is deliberate and asserted — see **Purpose** above.

## Scope today

One family, `test_hub*.py` (marker `hub`): **38 differential cells**, one golden
each, plus the non-golden wiring cell. Since H12 both legs run and every cell is
equality-then-pin against the goldens the Go hub froze at H1½.

* **snapshot** (`test_hub.py`, 34): health identity, the sessions overlay,
  messages paging, the 4xx/409 verb matrix (stream-cap 413-vs-400 pins
  included, and `/input`'s surviving 409 `not_accepting`), bare-mux
  status-only;
* **SSE** (`test_hub_sse.py`, 2): the appear→activity within-tick order via a
  registered-before-create subscription (the `: ok` opener is the
  registration proof), and the stalled-reader survivability cell (the
  connection-teardown half stays unit-level per side — the TCP close is
  legitimately different plumbing);
* **lane** (`test_hub_lane.py`, 2): the contract-v2 verbs against a live
  opencode lane — each leg drives its own `fake_opencode.py` instance
  (identical scripts) whose pinGuard fails the cell on any unscoped POST;
  turn/interrupt/approvals incl. the idempotent same-decision replay with no
  second upstream POST. It also carries the surviving SIDE-EFFECT proof: the
  `SHED_RC_AGENT_SESSION` back-write is what `hub_opencode.lane_session`
  waits for;
* **wiring** (`test_hub_wiring.py`, 1, no golden): the two legs launch two
  different daemons.

Two kinds carry the family. **codex** is the capability workhorse — it advertises
none of the contract-v2 verbs, so its matrix cells pin the kind-based 409s;
**opencode** is the only watchable kind left, so the lane-agnostic contracts
(`/messages` paging, both SSE cells) ride it through the shared
`hub_opencode.lane_session` setup.

### Families that retired

* A6 (`charliek/shed#322`) retired the **ingest** family (`test_hub_ingest.py`,
  9 cells) with the cursor hook route it drove, and the **side-effect** family
  (`test_hub_effects.py`, 2 cells) with the codex correlation back-write and the
  gated `/input` delivery it pinned.
* Plan 016 (S7, `charliek/shed#329`) retired the **one-shot** family — 8 modules
  and 46 goldens covering `version`/`create`/`probe`/`kill`/`list`, the exit-code
  classes, the raw-byte preseeds, capabilities, the `--wait` transitions and the
  cross-implementation interop cells — together with the `sx` binary they
  compared against. What they proved (that the two engines are wire-identical,
  session environment included) is why the surviving hub cells can share one
  stimulus; the Rust engine keeps its own unit coverage
  (`cargo test -p shed-rc-engine --features test-support`, `-p shed-app --features
  rc`, `crates/shed-broker`'s hub tests) and retires with the hub at S6
  (`charliek/shed#328`).

## CI

`.github/workflows/ci.yml` runs this suite as the **`rc-parity (Go↔Rust wire
goldens)`** job (part of the required `ci-success` check), gated on the `rcparity`
path filter: both hubs and the engines under them (`internal/ext/rc`,
`internal/ext/clirc`, `crates/shed-core`, `crates/shed-app`, `crates/shed-rc-engine`,
`crates/shed-broker`, `crates/shed-host-agent`), the Go oracle (via this harness's
own path entry) and `cmd/shed-ext-rc`, this harness, and the shared build manifests
(`crates/Cargo.*`, `crates/rust-toolchain.toml`, `go.mod`/`go.sum`).

The job installs Go + Rust + uv, `apt-get install`s **tmux** (not preinstalled on
GitHub runners) and asserts the ≥ 3.2 floor before building anything, then runs the
Rust legs no other job covers — `cargo test`/`clippy` for `-p shed-rc-engine
--features test-support` and `-p shed-app --features rc`, which since plan 016 are
the ONLY coverage of shed-app's non-default `rc` feature — followed by
`make test-rc-parity`. It is the only job in that workflow with a `timeout-minutes`,
because it drives real tmux sessions and a wedged pane is a hang rather than a
failure.
