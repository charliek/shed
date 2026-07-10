# host-agent-diff — the Go-vs-Rust host-agent differential harness

The correctness gate for the shed-host-agent Rust port (Phase 3, leg 3a.1). A hermetic
pytest suite that spawns **both** `shed-host-agent` daemon binaries — the Go
`cmd/shed-host-agent` and the Rust `crates/shed-host-agent` — and asserts they produce
**equal wire-visible output under a defined canonicalization**, plus two
language-neutral **golden-fixture** runners (one Go, one Rust) that pin the pure
decision functions so the two impls cannot silently drift *together*.

This is the **third** pytest suite in the repo and is **never merged** with
`tests/integration/` (live server create-cycle) or `desktop/tools/shedtest/` (mock UI
harness). See the root `CLAUDE.md`.

The design (comparison model, normalization contract, slices, acceptance matrix) lives
in `~/.claude/plans/monorepo-phase3-host-agent-difftest-harness.md`; the authoritative
wire-behavior reference is `docs/development/host-agent-wire-catalog.md`.

## Running

```bash
make test-host-agent-diff
```

That target (root `Makefile`) runs, in order:

1. `cd tests/host-agent-diff && uv sync && uv run pytest -v` — the live differential
   (the session fixture builds both binaries with `go build` + `cargo build`).
2. `go test ./cmd/shed-host-agent/... -run Golden` — the Go golden-fixture runner.
3. `cargo test -p shed-host-agent --test golden` — the Rust golden-fixture runner
   (with `~/.cargo/bin` on PATH).

Requires **Go + Rust (cargo) + Python/uv**. Install uv via `brew install uv`.

To run just the pytest suite directly:

```bash
export PATH="$HOME/.cargo/bin:$PATH"   # so the session fixture's cargo build resolves
cd tests/host-agent-diff && uv sync && uv run pytest -v
```

## Hermeticity

Every test runs with no network and a fresh, isolated `$SHED_HOST_AGENT_SOCKET_DIR`
(the two fixed sockets live there) **and** `$HOME` (so no real `~/.ssh` / `~/.shed` is
read); `SSH_AUTH_SOCK` is stripped so the Go daemon falls back to an empty local-keys
backend. Daemons write their operational log to a per-test `-log-file` (that log is
**not** a differential target — Go `slog` vs Rust `tracing` differ by design). See
`conftest.py`.

## Comparison model (D2) and normalization (D3)

- **Structural canonical-JSON, never raw bytes.** `normalize.canonical()` recursively
  sorts object keys (lists stay ordered), so a Go `map` / Rust `BTreeMap` or
  field-declaration order can't read as a diff.
- **Determinism over blanking.** `normalize.mask_live_status()` masks only the genuinely
  volatile fields — `version`, `pid`, `started_at`, `written_at`, `config_path`,
  `approval_channel.socket_path` (dir prefix only; the `host-agent.sock` basename is
  kept), and each `servers[].namespaces[].since`. Every masked timestamp has its
  **RFC3339 shape asserted before** masking. Everything else (`schema`, `policies`,
  `gate_namespaces`, `approval_channel.consumer_connected`, `servers[]`) is diffed.

## Golden fixtures

`fixtures/effective_policy.json` and `fixtures/gate_namespaces.json` are `input →
expected-output` vectors carrying `"protocol_version": 2`. Both the Go runner
(`cmd/shed-host-agent/golden_test.go`) and the Rust runner
(`crates/shed-host-agent/tests/golden.rs`) read the SAME files (from the neutral
`tests/host-agent-diff/fixtures/` home) and assert `EffectivePolicy` /
`desktopGateNamespaces` (Go) and `effective_policy_from_raw` /
`HostAgentConfig::gate_namespaces` (Rust) match every vector.

## Known contract gaps (slice 0)

- **Inline flow-style YAML config.** The Rust slice-0 `yaml_lite` config parser handles
  **block-style** maps only; it treats an inline flow map like
  `ssh: { approval: { policy: shed-desktop } }` as an opaque scalar and falls back to
  all-`deny-all` with an empty gate list, whereas the Go daemon (real YAML) parses it.
  The harness therefore writes its launch config in **block style** (both parsers agree),
  and this divergence is tracked as an `xfail`/out-of-scope cell below (`config-parse ·
  inline-flow`) — a later slice brings the Rust parser to parity.

- **Config validation.** The Rust slice-0 config reader is `LiveStatus`-scoped: it does
  **not** yet reject the things Go `LoadConfig`/`Validate` rejects (unknown policy strings,
  the AWS/Docker biometric policies, `aws.mode`/`aws.sheds` errors, a non-positive
  `approval_timeout`, malformed YAML). So e.g. `aws.approval.policy: biometrics` exits 1 in
  Go but currently starts in Rust and echoes `biometrics`. The full config **port +
  validation-parity differential** is its own later slice (config.go's `Validate` is ~120
  lines); tracked as `config-validate` below.

- **Bounded connect timeout.** The Rust `status` client and the daemon's live-socket
  stale-probe use blocking Unix `connect()`; Go uses `net.DialTimeout` (2s / 500ms). The
  normal path resolves immediately either way; a pathological full-backlog peer could hang
  the Rust side longer. Low severity, tracked for the config/lifecycle slice.

## Per-cell status table

`enforced` = asserted equal (or smoke-asserted) now; `xfail` = a real port surface not
yet implemented in Rust, tracked to flip to `enforced` when it lands; `out-of-scope` =
owned by a different mechanism (golden/unit) or later slice, not the live diff.

| Axis | Cell | Status | Owning mechanism |
|---|---|---|---|
| CLI | `version` (exit 0 + nonempty stdout, smoke) | **enforced** | live (`test_version.py`) |
| CLI | `status` not-running (exit 1 + masked stderr byte-equal) | **enforced** | live (`test_status_not_running.py`) |
| CLI | `status --bogus` / `--live` (exit 2 + stderr equal) | **enforced** | live (`test_status_badarg.py`) |
| CLI | `status --json` running (masked `LiveStatus` canonical-equal) | **enforced** | live (`test_status_running.py`) |
| CLI | `status` text render running (masked equal) | **enforced** | live (`test_status_running.py`) |
| lifecycle | config-load error → exit 1 (exit code only) | **enforced** | live (`test_config_error.py`) |
| lifecycle | SIGTERM → exit 0 + both sockets unlinked | **enforced** | live (`test_lifecycle.py`) |
| socket | status socket bind + `0600` file perms | **enforced** | live (`conftest` daemon + `test_socket_perms.py`) |
| socket | socket-dir `0700` + stale-vs-live rebinding | **xfail** | live (config/lifecycle slice) |
| config-validate | reject unknown/biometric policy, bad mode/timeout, malformed YAML (parity) | **xfail** | live (config-port slice; see "Known contract gaps") |
| decision | `EffectivePolicy` (`""→deny-all`, echoes) | **enforced** | golden (Go + Rust runners) |
| decision | `desktopGateNamespaces` ordered gate list | **enforced** | golden (Go + Rust runners) |
| config-parse | inline-flow-style YAML (`{ ... }`) parity | **xfail** | live — Rust `yaml_lite` block-only (see "Known contract gaps") |
| desktop UDS server | hello/hello_ack, last-writer-wins, replay ring, event fan-out, `token.get` | **xfail** | live surface A (slice 1) |
| approval | gated `sign` delegated approve/deny/timeout | **xfail** | live (slice 1) |
| approval | native Touch-ID (biometrics) | **out-of-scope** | separate manual sign-off (needs live biometric) |
| bus | subscribe/respond, open + secure(TLS-pin), 401/409, reconnect | **xfail** | live surface B (slice 1) |
| egress | events / 401 / 404-501, 5m vs 30s backoff | **xfail** | live ("no reconnect in window") + unit (consts) |
| ssh backend | agent-forward / local-keys, list/sign/ping/status | **xfail** | live (transcripts) + golden (payloads) |
| aws backend | passthrough, cache-hit/expiry | **xfail** (live) | live; assume-role → **out-of-scope** (golden+unit, no Go STS seam) |
| docker backend | allowlist / allow_all / helper / inline, not-found vs not-allowed | **xfail** | live (helper transcript) + golden (resolution matrix) |
| minter | control vs credentials scope, host-key-mismatch terminal | **xfail** | live (argv `<scope>`) + unit (single-flight, refresh) |
| supervisor | reconcile / restart-on-cred-or-TLS-change | **xfail** | live; `shouldMint` matrix → golden |
| discovery | off / poll / fsnotify | **xfail** | live (convergence w/ deadline); `LoadDiscoveredServers` shape → golden |
| concurrency | single-flight mint, drop-on-full fan-out | **out-of-scope** | unit parity |

Update this table as slices land (Kimi acceptance-criteria ask in the plan).
