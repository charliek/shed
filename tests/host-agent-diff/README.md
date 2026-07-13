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
2. `go test ./cmd/shed-host-agent/... -run Golden` — the Go golden-fixture runners.
3. `cargo test -p shed-host-agent golden` — the Rust golden-fixture runners (name
   filter, so it covers both `tests/golden.rs` and the **in-crate** goldens —
   `load_discovered_servers` in `controltoken.rs` and `ssh_payload_shapes` in `bus.rs`;
   with `~/.cargo/bin` on PATH).

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
  The surface-A desktop tests use `normalize.mask_hello_ack()` in the same spirit — it
  masks a `hello_ack` frame's volatile `id`/`ts` (and, on an **accepted** ack, the
  build `agent.version`, shape-asserted nonempty first) while diffing the rest
  (`v`, `type`, `accepted`, `reason`, `agent.approval_method`, `namespaces`,
  `gate_namespaces`, `request_timeout_ms`). A **superseded** (`accepted:false`) ack's
  `agent` is the zero value `{"version":"","approval_method":""}`, so its empty
  `version` is diffed as a stable constant, not masked. The surface-B bus test uses
  `normalize.mask_bus_response()` in the same spirit — it masks a response Envelope's
  volatile `id` (a fresh UUID: v7 in Go's `NewResponse`, v4 in Rust's `new_response`)
  and `timestamp` (RFC3339 shape-asserted first) while **diffing** `in_reply_to` (the
  correlation id — it must echo the request's `id`, so it is asserted, never masked),
  `namespace`, `type`, `final`, `payload`, and the echoed `shed`.

- **The gated cross-surface `sign` flow compares the ed25519 signature UNMASKED.** The
  capstone `test_bus_sign_gated.py` drives one bus `sign` request → an
  `approval_request` to the desktop consumer → (approve) a signature + audit `event` +
  durable JSONL line, or (deny) `{approval denied, SIGN_FAILED}` + a `denied` audit.
  Because ed25519 signing is **deterministic** (RFC 8032 — the nonce is derived from
  key+message, not randomness), the Go `x/crypto/ssh` signer and the Rust
  `ed25519-dalek` backend, loading the SAME committed `~/.ssh/id_ed25519`
  (`fixtures/test_ed25519`, installed by the `daemon` fixture) and signing the SAME
  fixed challenge, produce the SAME 64-byte blob — so `mask_bus_response` leaves
  `payload.blob` to be **diffed** (never masked/normalized-to-"present"), and it is
  additionally pinned as an absolute golden (`EXPECTED_SIGN_BLOB_B64`). The surface-A
  frames use `normalize.mask_approval_request()` (masks the volatile `id`/`ts`/
  `expires_at`; diffs `namespace`/`op`/`shed`/`detail` and the single-server ABSENCE
  of `server`) and `normalize.mask_event()` (masks `id`/`ts`; diffs `kind` + every
  audit field, so the omitempty set — e.g. NO `detail`/`code` on a deny — is pinned);
  the durable line uses `normalize.mask_audit_entry()` (masks `ts` only). Field *order*
  in every frame is normalized away by `canonical()`; the sign blob is compared, not
  masked.

## Golden fixtures

`fixtures/effective_policy.json` and `fixtures/gate_namespaces.json` are `input →
expected-output` vectors carrying `"protocol_version": 2`. Both the Go runner
(`cmd/shed-host-agent/golden_test.go`) and the Rust runner
(`crates/shed-host-agent/tests/golden.rs`) read the SAME files (from the neutral
`tests/host-agent-diff/fixtures/` home) and assert `EffectivePolicy` /
`desktopGateNamespaces` (Go) and `effective_policy_from_raw` /
`HostAgentConfig::gate_namespaces` (Rust) match every vector.

`fixtures/load_discovered_servers.json` (`"protocol_version": 1`) is `config.yaml text →
expected []ServerTarget`. The Go runner (`TestGoldenLoadDiscoveredServers` in
`golden_test.go`) and the Rust runner (`golden_load_discovered_servers`, an **in-crate**
`controltoken.rs` test — the binary crate has no lib, so `tests/golden.rs` can't reach
`load_discovered_servers`) both parse each vector's YAML and assert equal `ServerTarget`s.
It pins the load-bearing divergences from `shed-core::ShedConfig`: `ssh_port` defaults to
**0** (not 22) when omitted, empty-host entries are skipped, targets sort by name.

`fixtures/ssh_payload_shapes.json` (`"protocol_version": 1`) pins the four ssh-agent
**response payload shapes** (`internal/ext/protocol/ssh.go`). The Go runner
(`TestGoldenSSHPayloadShapes` in `golden_test.go`) builds the `protocol.SSH*Response`
structs; the Rust runner (`golden_ssh_payload_shapes`, an **in-crate** `bus.rs` test —
the bus serde types are bin-crate-internal, so `tests/golden.rs` can't reach them, same
precedent as `load_discovered_servers`) builds the bus serde types; both marshal and
assert equal to each vector's `expected` (compared as parsed JSON values). It pins the
tag names, the base64 pass-through of `blob`s, the always-present `rest:""`, and that an
empty key list marshals as `[]` not `null`.

**Key fixtures.** `fixtures/test_ed25519{,.pub}` (slice 0) plus `fixtures/test_rsa{,.pub}`
(2048-bit) and `fixtures/test_ecdsa{,.pub}` (P-256), added for the ssh-backend cells, are
**throwaway, non-secret, passphrase-less** OpenSSH keypairs generated once (comment
`hadiff-test`) and committed so both daemons load an identical key set. They guard nothing
and must never be reused anywhere real. The `daemon` fixture installs them into each
daemon's isolated `<HOME>/.ssh/id_<algo>`; `fake_ssh_agent.py` also serves them as agent
identities (real-signing ed25519, canned blobs for rsa/ecdsa).

**Committed TLS pair (secure-bus cells).** `fixtures/synthetic_bus_{cert,key}.pem` is a
**throwaway, non-secret** self-signed RSA-2048 cert/key (CN `shed-host-agent-diff synthetic
bus`, `subjectAltName=IP:127.0.0.1`, 100-year validity) generated ONCE with `openssl req
-x509` and committed because Python's stdlib `ssl` cannot generate a cert at runtime — a
fixed pair means a **stable leaf-DER fingerprint**. `synthetic_bus.py`'s TLS mode serves it
via `ssl.SSLContext.wrap_socket`; the `discovery:` config pins it as `tls_cert_fingerprint =
"sha256:" + hex(sha256(leaf_DER))` — the SAME derivation as Go's `sdk.certFingerprint`
(`sha256:` + `hex.EncodeToString(sha256(rawCerts[0]))`, the leaf DER) and Rust's
`shed_core::tls::fingerprint` (leaf DER, lowercase hex), so ONE fingerprint string pins
both impls. **If the pair is regenerated, recompute the pin** (`python3 -c "import
ssl,hashlib; print('sha256:'+hashlib.sha256(ssl.PEM_cert_to_DER_cert(open('fixtures/synthetic_bus_cert.pem').read())).hexdigest())"`)
and update `CERT_PIN` in `test_secure_bus.py`. It guards nothing; never reuse it anywhere
real.

## Known contract gaps (slice 0)

- **Config parsing & validation — STRUCTURAL sub-class retired; typed-decode residue
  documented-open.** The Rust config reader (`config.rs` `yaml_lite`) is now backed by
  `saphyr-parser` (a pure-Rust YAML-1.2 event parser, `default-features = false`), not the
  old line/colon reader. That **retires the structural sub-class** of the Go-vs-Rust gap:
  inline **flow maps** (`ssh: { approval: { policy: shed-desktop } }`) and **flow
  sequences** parse like block style (`test_config_inline_flow.py`); **block-style
  sequences** parse (the shipped `configs/extensions.example.yaml` `registries:` block list
  parses to `[index.docker.io, ghcr.io]` instead of silently dropping to empty — golden
  `config_validate` + the `example_config_loads_and_validates` unit); **malformed YAML is
  detected** (returns `Err` → exit 1) instead of being swallowed; and a bare `key:` (YAML
  null) is distinct from `key: ""` (empty string), so Go's null-vs-empty merge is
  reproduced (`source_profile` cross-language golden). On top of the parser, the Rust
  `HostAgentConfig::load` now runs a faithful `validate()` port and **rejects (exit 1) the
  SAME configs Go `LoadConfig`/`Validate` rejects** — unknown/biometric policy strings, the
  AWS/Docker biometric policies, `aws.mode`/`aws.sheds` errors, a non-positive/invalid
  `approval_timeout`, malformed YAML, and duplicate map keys — enforced live by
  `test_config_validate.py` (exit-1 parity on both impls) and pinned per-vector by the
  `config_validate.json` golden (both runners).

  **Documented-open (a real parser fixes STRUCTURE, not typed resolution):** the `Node`
  model is stringly-typed and the readers coerce leniently, so a **typed-decode residue**
  survives and is NOT closed here — **scalar-into-typed-field coercion** (`http_port:
  not-a-number` → Go errors, Rust defaults; `approval_timeout: [a,b]` → Go errors, Rust
  falls back to 25s; a scalar where a list is expected), and **bool alternate forms**
  (`allow_all: yes` / `on` / `True` → Go `yaml.v3` resolves to a bool, but Rust `opt_bool`
  treats only lowercase `"true"` as true — CodeRabbit-confirmed). **Closed** by the swap:
  duplicate map keys (→ `Err`, matching yaml.v3) and multi-document input (first document
  consumed, matching Go's `yaml.Unmarshal`). Also documented-open: anchors/aliases/
  merge-keys, non-string keys, tagged scalars (low real-world risk), and the **cross-client
  inconsistency on the shared `~/.shed/config.yaml`** — the host-agent (`saphyr-parser`) and
  the desktop app (shed-core's own hand-rolled `yaml_lite`, a separate crate with a Swift
  byte-parity test) may now diverge on the SAME file; converging the two readers is a
  separate shed-core slice, not this one.

- **Bounded connect timeout.** The Rust `status` client and the daemon's live-socket
  stale-probe use blocking Unix `connect()`; Go uses `net.DialTimeout` (2s / 500ms). The
  normal path resolves immediately either way; a pathological full-backlog peer could hang
  the Rust side longer. Low severity, tracked for the config/lifecycle slice.

- **Bus subscription set — converged.** In single-server mode (no `discovery:` block)
  both daemons connect to `server:` and subscribe to `ssh-agent`; when `aws.*` is
  configured (mode passthrough, or a role) both ALSO subscribe `aws-credentials`; and
  both subscribe `docker-credentials` in the common case — the Docker backend is non-nil
  even unconfigured (its constructor errors ONLY on an explicit-but-unstat-able
  `config_path`), so the namespace is subscribed for every server. Each cell
  `wait_for_subscribe`-es its namespace on both impls (`test_bus_ping_pong.py`,
  `test_aws_backend.py`, `test_docker_backend.py`), so they compare apples to apples.
  As of the egress slice both daemons ALSO GET the always-on egress-audit stream
  (`/api/egress/stream`), so the Go and Rust **endpoint sets fully converge** — there is
  no residual endpoint asymmetry. The synthetic bus now asserts (not tolerates) that both
  impls hit egress: `test_egress.py` proves the subscribe, the fixed-ts audit diff, and
  the per-impl 501 hard-backoff (no reconnect in window).

- **Supervisor, discovery & `servers[]` — converged.** Both daemons now run ONE supervisor
  in BOTH modes (single-server = a discovery config that reconciles once and never reloads;
  the single unnamed target has `ssh_host=""` → `should_mint` false → open/no-pin). The
  supervisor's `health()` populates `LiveStatus.servers[]` (one entry per watched server;
  `name:""` for the single unnamed target), with each namespace's connection state — incl.
  the 409-`rejected` terminal — surfaced identically (masking only `since`, RFC3339-shape
  asserted first; `state`/`last_error` diffed). The Rust `health()` sorts `namespaces` by
  name to match Go's `HostClient.Status()` sort. A SECURE server reached via `discovery:`
  self-mints a **credentials-scope** bus token over SSH and subscribes over a TLS-pinned
  https connection; the `discovery:` reload loop (`off`/`poll`) converges both impls to the
  same server set (the live diff drives `poll` for determinism; the production `fsnotify`
  default is `notify`-unit-covered). Cells: `test_servers.py` (servers[] connected +
  409-rejected), `test_discovery.py` (off/poll convergence), `test_secure_bus.py`
  (TLS-pinned mint + 401-remint + wrong-pin-fails-closed), plus the `should_mint.json`
  golden and the `supervisor.rs`/`watcher.rs` units.

- **Event replay ring.** The surface-A handshake (`hello`→`hello_ack`), the non-hello
  drop, single-consumer supersede, event fan-out + approval correlation (via the gated
  `sign`), and **`token.get`** (via the real SSH-bootstrap minter + a PATH-shim `ssh`,
  `test_token_get.py`) are all differentially enforced. One surface-A cell stays `xfail`:
  the **event replay ring** (`replay_events` on connect) is not exercised by the current
  flows (they connect a fresh consumer with `replay_events:0`) — a buffered-then-connect
  drive is a follow-up.

- **Minter — malformed `~/.shed/config.yaml` — CLOSED.** The Rust `load_discovered_servers`
  is backed by the saphyr `yaml_lite` reader now, so a malformed `~/.shed/config.yaml`
  returns `Err` (was silently permissive), matching Go `LoadDiscoveredServers`. Both impls
  surface it as `reading server config: …` in the `token.response.error` when a `token.get`
  triggers a resolve — enforced live by `test_token_get.py::test_token_get_malformed_shed_config`
  (the outer `reading server config:` prefix, per-impl; the inner body is yaml-lib specific
  — Go `parsing shed config` vs the saphyr message — and excluded per the docker suffix
  precedent). This stays a SEPARATE cell from the `config-validate` matrix: it exercises a
  DIFFERENT file (`~/.shed/config.yaml`, not the launch `-config`) and a DIFFERENT chain
  (`token.get` → `resolve` → `load_discovered_servers`).

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
| config-validate | reject unknown/biometric policy, bad mode/timeout, malformed YAML, duplicate key (exit-1 parity) | **enforced** | live (`test_config_validate.py`) + golden (`config_validate.json`, Go + Rust runners) |
| decision | `EffectivePolicy` (`""→deny-all`, echoes) | **enforced** | golden (Go + Rust runners) |
| decision | `desktopGateNamespaces` ordered gate list | **enforced** | golden (Go + Rust runners) |
| config-parse | inline-flow-style YAML (`{ ... }`) parity (masked `LiveStatus` policies canonical-equal) | **enforced** | live (`test_config_inline_flow.py`) |
| desktop UDS server | hello/hello_ack handshake (masked canonical-equal) + non-hello first line dropped | **enforced** | live surface A (`test_desktop_handshake.py`, `test_desktop_first_non_hello_drops.py`) |
| desktop UDS server | single-consumer last-writer-wins supersede (`accepted:false, reason:"superseded"`, old closed) | **enforced** | live surface A (`test_desktop_supersede.py`) |
| desktop UDS server | `token.get` → `token.response` (masked; `token`+`expires_at` compared UNMASKED via the PATH-shim mint) | **enforced** | live surface A (`test_token_get.py`) |
| desktop UDS server | event fan-out + approval request/response correlation (driven by the gated `sign`) | **enforced** | live surface A+B (`test_bus_sign_gated.py`) |
| desktop UDS server | event replay ring (`replay_events` on connect) | **xfail** | live — not exercised by the sign flow (fresh consumer, `replay_events:0`); needs a buffered-then-connect drive |
| approval | gated `sign` delegated approve/deny (deterministic ed25519 blob compared UNMASKED) | **enforced** | live surface A+B (`test_bus_sign_gated.py`) |
| approval | gated `sign` timeout / no-consumer fail-closed | **xfail** | unit-covered (`desktop.rs`); a live differential drive is a follow-up |
| approval | native Touch-ID (biometrics) | **out-of-scope** | separate manual sign-off (needs live biometric) |
| bus | subscribe → ping → respond (open) ping/pong (masked canonical-equal) | **enforced** | live surface B (`test_bus_ping_pong.py`) |
| bus | secure (TLS-pin) subscribe/respond, 401/409, reconnect | **enforced** | live surface B (`test_secure_bus.py`: a `discovery:`-config secure server, committed-cert TLS pin, minted-Bearer subscribe, 401→re-mint→reconnect, wrong-pin fails closed) + `bus.rs` unit (409-rejected + pin/401 machinery) |
| bus | aws-credentials subscription (when configured) | **enforced** | live surface B (`test_aws_backend.py`: both impls `wait_for_subscribe("aws-credentials")`) |
| bus | docker-credentials subscription (even unconfigured) | **enforced** | live surface B (`test_docker_backend.py`: both impls `wait_for_subscribe("docker-credentials")`, incl. the unconfigured cell) |
| status | single-server `LiveStatus.servers[]` per-namespace state (incl. 409-rejected) | **enforced** | live surface B (`test_servers.py`: masked `servers[]` canonical-equal for the connected case + the 409-`rejected` terminal, `since` masked / `state`+`last_error` diffed) |
| egress | subscription convergence (both impls GET `/api/egress/stream`) | **enforced** | live surface B (`test_egress.py`: both `wait_for_egress`) |
| egress | events → durable audit line (fixed-ts, diffed UNMASKED incl. `"approval":""`) | **enforced** | live surface B (`test_egress.py`) |
| egress | 501 → hard 5m backoff (per-impl `egress_hits()==1`, no reconnect in window) | **enforced** | live surface B (`test_egress.py`) |
| egress | 401-invalidate + control-token scope | **out-of-scope** | unit (`egress.rs`: `status_401_invalidates_source`, `sends_control_token_not_credentials`) — harness runs open (no token to invalidate) |
| egress | 404→unavailable + backoff constants (1s/30s/5m, no held-reset) | **out-of-scope** | unit (`egress.rs`: `status_404_returns_unavailable`, `backoff_*`) — control-flow, not pure in→out |
| egress | `egressDecision`→`AuditEntry` mapping (detail/ts-UTC/empty-ts/offset/`approval:""`) | **enforced** | golden (Go + Rust runners, `egress_audit_entry.json`) |
| ssh backend · local-keys | `list` (masked canonical-equal + durable non-gated audit line) | **enforced** | live (`test_ssh_backend.py`) |
| ssh backend · local-keys | `sign` rsa flags 0/2/4/6 (→ `ssh-rsa`/`rsa-sha2-256`/`rsa-sha2-512`/`rsa-sha2-256`; format diffed, blob verified per-impl) | **enforced** | live (`test_ssh_backend.py`, verify-not-bytes) |
| ssh backend · local-keys | `sign` ecdsa (format diffed, blob verified per-impl) | **enforced** | live (`test_ssh_backend.py`) |
| ssh backend · local-keys | `sign` ed25519 (deterministic blob compared UNMASKED) | **enforced** | live (`test_bus_sign_gated.py`) |
| ssh backend · local-keys | `status` (`{connected:true, mode:"local-keys", key_count:3}`) | **enforced** | live (`test_ssh_backend.py`) |
| ssh backend · agent-forward | `list` (3 fake identities canonical-equal + audit + fake transcript diff) | **enforced** | live (`test_ssh_backend.py` + `fake_ssh_agent.py`) |
| ssh backend · agent-forward | `sign` ed25519 (fake real-signs → blob UNMASKED) + `sign` rsa flags=2 (canned blob byte-equal, transcript `flags==2` passthrough) | **enforced** | live (`test_ssh_backend.py`) |
| ssh backend · agent-forward | `status` (`mode:"agent-forward"`, extra REQUEST_IDENTITIES on the transcript) | **enforced** | live (`test_ssh_backend.py`) |
| ssh backend · mode resolution | unknown `ssh.mode` → exit 1 (single-server AND `discovery:` config shapes) | **enforced** | live (`test_ssh_mode_error.py`) |
| ssh backend | response payload shapes (`list`/`sign`/`status`/`error`: tag names, b64 pass-through, `rest:""`, empty-list `[]` not `null`) | **enforced** | golden (Go + Rust runners on `ssh_payload_shapes.json`) |
| ssh backend | agent-client wire (framing/failure/oversize/wedged bounds), flag-bit matrix, resolve matrix, missing/encrypted skip, unknown-op/invalid-payload strings, list-error audit | **enforced (unit)** | Rust unit (`ssh_backend*.rs`, `bus.rs`) + fake-seam self-test (`test_fake_ssh_agent.py`) |
| aws backend · passthrough | `get_credentials` (success + no-expiry-hint + no-static error; payload + audit diffed, error detail home-normalized), `status` (`passthrough:<profile>` + `cached_until`), `ping`, unknown-op, re-login pickup (atomic rewrite) | **enforced** | live (`test_aws_backend.py`) |
| aws backend · assume-role | cache hit/stale, role resolution + layering, STS/nil-creds errors, session-name shape | **out-of-scope** | golden (`aws_resolve`/`aws_expiry` runners) + Rust unit (`AssumeRoler` fake) — no Go STS seam to drive live |
| aws backend | response payload shapes (`get_credentials`/`status`/`ping`/`error`: tag names, `expiration`/`cached_until` omitempty) + per-namespace gate selection | **enforced (unit)** | Rust unit (`bus.rs`: `aws_*`, incl. `aws_uses_aws_gate_not_ssh`, `aws_payload_tag_names_match_protocol`) |
| docker backend | `get` inline-auth (payload+audit) / helper (transcript diffed + payload + ok audit) / not-allowed (backend REGISTRY_NOT_ALLOWED) / approval-deny (guest REGISTRY_NOT_ALLOWED + audit APPROVAL_DENIED, the two-code split) / not-found→anonymous (guest CREDENTIALS_NOT_FOUND + audit anonymous) / `list` (positional `count:N`) / `status` / `ping` / unknown-op / **unconfigured** (still subscribes + denies) | **enforced** | live (`test_docker_backend.py`, fake `docker-credential-testhelper` seam + self-test) |
| docker backend | `resolve` layering (Option registries/allow_all, flow-list), `normalize_registry` (one-occurrence strip), inline-auth decode, PATH augment (append), helper-exec seam (abs-resolve, 5s timeout, PascalCase-avoidance tag guard) | **enforced (golden+unit)** | golden (`docker_resolve`/`docker_normalize`/`docker_inline_auth`/`docker_path_augment` runners) + Rust unit (`docker_backend.rs`, `bus.rs` docker handler incl. `docker_uses_docker_gate_not_ssh_or_aws`, `docker_payload_tag_names_match_protocol`) |
| minter | control-scope argv + success `token.response` | **enforced** | live (`test_token_get.py`: `token`/`expires_at` compared + argv == expected vector, `<scope>` == `control`) |
| minter | host-key-mismatch terminal; single-flight; refresh cadence | **enforced (unit)** | unit parity (`minter.rs` / `bootstrap.rs`, incl. real-runner shell-shim tests) |
| minter | `load_discovered_servers` shape (ssh_port=0-vs-22, empty-host skip, sort) | **enforced (golden)** | Go + Rust golden runners on `fixtures/load_discovered_servers.json` |
| minter | credentials-scope live bus drive | **enforced** | live surface B (`test_secure_bus.py::test_secure_bus_credentials_mint`: a secure discovery server self-mints a credentials-scope token over the PATH-shim `ssh` and presents `Bearer <minted>` on the TLS-pinned bus subscribe) |
| minter | malformed `~/.shed/config.yaml` error parity (outer `reading server config:` prefix, per-impl) | **enforced** | live (`test_token_get.py::test_token_get_malformed_shed_config`) |
| supervisor | reconcile / restart-on-cred-or-TLS-change | **enforced** | golden (`should_mint.json`, Go + Rust runners) + Rust unit (`supervisor.rs`: `reconcile_{add_remove,no_churn,url_change,credential_or_pin_change,dedup}`, `shutdown`, `health`, the fake-group factory) — reconcile/restart is fake-group-owned (no wire surface); `should_mint` is the wire-visible half |
| discovery | off / poll / fsnotify | **enforced** | live (`test_discovery.py`: `poll` convergence within a deadline + `off` no-reload, `servers[]` names converge on both impls) + `watcher.rs` `notify`-backed unit smoke (the production `fsnotify` default; the live diff drives POLL for determinism) + golden (`load_discovered_servers.json`) |
| concurrency | single-flight mint, drop-on-full fan-out | **out-of-scope** | unit parity |

Update this table as slices land (Kimi acceptance-criteria ask in the plan).
