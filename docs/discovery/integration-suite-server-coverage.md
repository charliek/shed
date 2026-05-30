# Integration Suite — Server-Code Coverage Gap

Discovery notes from the v0.5.9 bundle session (2026-05-29). Captures
a class of bug the current `tests/integration/` suite cannot catch by
design, the user-facing scenarios where this matters, and a graduated
set of fixes ranked by cost.

> **Provenance.** Surfaced while validating PR-B1 (`refactor/start-shed-orchestrator-vz`).
> The `test_plain_create_timing[vz]` gate fired with agent p50 ~5800 ms
> against a 2200 ms ceiling on a `make build`-produced shed-server,
> while the brew v0.5.8 binary passed the same gate at ~1500 ms. The
> bisect rules out a code regression and instead reveals two
> structural facts about the suite that are worth recording.

---

## 1. Goal of the integration suite

From `tests/integration/README.md` and `docs/development/testing.md`,
the stated goal is:

> Live create-cycle tests that drive a running `shed-server` via the
> `shed` CLI. … Complements the Go unit tests — those catch logic /
> file-shape regressions on every PR; the integration suite catches
> live boot-path / SSE / timing regressions that need a real VM.

The suite is parameterized over `["vz", "fc"]`. VZ tests target the
brew-installed `shed-server` on the developer's mac via the
`my-server` config entry. FC tests target `mini3` (or
`$SHED_FC_HOST`) over SSH against the deb-installed `shed-server`
there. The `test_plain_create_timing[backend]` test is the dynamic
regression gate that PR-time GHA CI can't be (no /dev/kvm on GHA).

### Implicit assumption

What's not stated, but is structurally true: **the suite tests
whatever shed-server is currently running**, not the source tree the
developer is working in. The shed-server binary, its tag/version
string, its embedded constants — those all came from whatever
installer last touched the host (`brew install shed`, `apt install
shed`, `make install`, etc.).

For most PRs this is fine: changes to the CLI, image format, or
backend handlers are exercised end-to-end by running the new CLI
against any server that speaks the protocol. But a PR that **only**
changes server code that has no CLI-visible signature change runs
through the suite without ever executing the new server code.

---

## 2. The use case that broke

Cycle A of the v0.5.9 bundle shipped two PRs whose primary effect
was server-side:

- **PR-A1 (#151)** — added defensive sentinel errors in
  `StartShed` / `stopShedLocked` on both backends.
- **PR-A2 (#152)** — wrapped workspace mount calls in a retry
  envelope on both backends.

Both opened with "make test-integration: 38/38 pass on both VZ
(my-server) and FC (mini3)" in their PR bodies. That's literally
true — and structurally meaningless for the changes in question.
The brew-installed v0.5.8 server was still running. The 38 tests
exercised the CLI, the API contract, the boot path, and the
PhaseTimer log shape — none of which PR-A1 or PR-A2 touched. The
new server-side branches never ran.

Neither PR introduced a runtime regression, so nothing broke in
practice. But the validation language overstates the coverage. If
PR-A1 had a subtle bug in the new `stopShedLocked` path (e.g., the
post-Stop `IsProcessAlive` check returning the wrong answer when
the VMM is mid-shutdown), the integration suite would not have
caught it. Unit tests would have to be the gate, and the dynamic
"agent p50 stays under ceiling" regression check would only fire if
the regression manifested on the brew binary — which it can't,
because the brew binary doesn't have the new code.

PR-B1's swap-the-brew-binary workaround (described in §4) is what
exposed the second-order issue:

### The dev-build vs release-build path divergence

When a `make build`-produced shed-server is swapped into the brew
Cellar path and the brew service is restarted, `test_plain_create_timing`
fires at agent p50 ~5800 ms vs the 2200 ms ceiling. Bisect:

- Brew v0.5.8 binary: ~1500 ms ✅
- `make build` from v0.5.8 source: ~5800 ms ❌
- `make build` from main (post PR-A1+A2): ~5800 ms ❌
- `make build` + `launchctl setenv SHED_BUILD_TOOLS_REF
  ghcr.io/charliek/shed-build-tools:v0.5.4` + brew restart: ~1500 ms ✅

Root cause: `internal/version/buildtools.go:BuildToolsRefForTag`
returns `""` for any version string with a `-dirty` or `-N-g<hash>`
suffix. Dev builds therefore have no shed-build-tools image
reference, fall back to formatting the upper layer in-guest via
`mkfs.ext4` on first boot (~4 s), and miss the pre-formatted
upper-template fast path that release builds use.

That's **intentional dev-build behavior** — a dev binary can't
assume a published shed-build-tools image matches its source state.
But the integration suite's timing ceiling was calibrated against
brew (release) binaries, so any time someone wants to validate
server-side changes locally they have to either:

1. Live with the false-positive timing failure, or
2. Manually set `SHED_BUILD_TOOLS_REF` to the most recent release
   tag (with the corresponding implicit assumption that the source
   hasn't drifted from that release in a way that matters), or
3. Skip the swap and accept that they're not actually testing their
   server code.

Combined with §2's "suite tests whatever's running" issue, the
result is that **validating a server-side PR locally currently
requires the developer to:**

- Build the binary
- Codesign it ad-hoc (`codesign -s -`)  — macOS will SIGKILL an
  unsigned binary on launchd start
- Chmod the brew Cellar path writable, copy in, chmod read-only
- `launchctl setenv SHED_BUILD_TOOLS_REF
  ghcr.io/charliek/shed-build-tools:v<RELEASE>`
- `brew services restart shed`
- Run the integration suite
- Restore the brew binary (chmod / cp / codesign / chmod / restart)
- `launchctl unsetenv SHED_BUILD_TOOLS_REF`

That's a 7-step manual procedure with at least two failure modes
(adhoc codesign, env var) that produce confusing symptoms (process
gets SIGKILL'd by launchd; agent p50 inexplicably 4 s slower than
yesterday). It's the kind of friction that pushes developers toward
"just trust the unit tests and CI" — which is exactly how PR-A1
and PR-A2 went through the cycle without integration coverage of
their new code paths.

---

## 3. CI gap

The same structural issue plays out differently in CI. The GHA
`test` job runs `go test ./...` only — unit tests on the source.
The `Linux smoke` job verifies the deb install runs at all. There's
no path in CI that exercises the integration suite, because none of
the runners can host a /dev/kvm Firecracker or a vfkit VZ. The
dynamic regression gate (`test_plain_create_timing`) lives only in
the local-developer workflow.

That's a separate question from §1's gap. Even if CI could run the
suite, **it would have the same "test whatever's running" issue**
unless the CI workflow installs the just-built binary onto the
runner first. So the two gaps interact:

- §1's gap is a **content gap**: the suite doesn't run the new
  server code locally without manual binary-swap surgery.
- §3's gap is an **availability gap**: the suite doesn't run in CI
  at all.

A fix for either alone helps. A fix for both is the only way to
get "server-side regressions are caught before a developer notices
in production."

---

## 4. Where this lands on the release path

Today's release flow is:

1. Developer writes server-side change, opens PR.
2. CI runs unit tests (PR-time signal: passes/fails on Go-level
   correctness).
3. Developer locally runs `make test-integration` — passes because
   the suite is hitting the brew/deb v_{N-1} server, not the source
   under change.
4. PR review (human + CodeRabbit).
5. Merge.
6. Release cut from main — first time the changed server code runs
   on any host that uses the brew/deb binary.
7. User updates their brew/deb install.

The implicit assumption that "the integration suite caught
server-side regressions" is wrong at step 3. The first time a
server-side regression actually has a chance to manifest is step 6
(when the release binary is built and the release-validation
workflow on `mini3` boots it). The first time a regression with
release-only timing characteristics could manifest is the next user
update at step 7.

For v0.5.4 → v0.5.5 the cost was concrete: the v0.5.4 release
silently failed to activate Phase 2 (CoW upper) on fresh installs
because the build-tools-ref resolution went wrong. That was caught
by a developer noticing slow creates after `brew upgrade`, not by
the suite. Same class of bug as PR-A1/A2 in this cycle, different
phase. The fix (#124) shipped two days later as v0.5.5.

---

## 5. Recommended fixes

Ordered from least to most disruptive. **Recommendation: do
Options 1 (local + remote) + 3 together**, accept Option 2 as later
if 1+3 don't catch a regression. See §11 for the execution plan and
§12 for the cross-host bootstrap framing that emerged after the
v0.5.9 cycle.

### Option 1 — `make install-local-server` / `make restore-brew-server` targets

A pair of Makefile targets that automate the 7-step binary-swap
procedure:

```makefile
install-local-server: build
	@brew services stop shed
	@chmod +w /opt/homebrew/Cellar/shed/$(BREW_VERSION)/bin/shed-server
	@cp -f bin/shed-server /opt/homebrew/Cellar/shed/$(BREW_VERSION)/bin/shed-server
	@codesign -s - /opt/homebrew/Cellar/shed/$(BREW_VERSION)/bin/shed-server
	@chmod -w /opt/homebrew/Cellar/shed/$(BREW_VERSION)/bin/shed-server
	@launchctl setenv SHED_BUILD_TOOLS_REF $(RELEASE_BUILD_TOOLS_REF)
	@brew services start shed
	@echo "Local shed-server installed. Run 'make restore-brew-server' to revert."

restore-brew-server:
	# symmetric — restores /tmp/shed-server-vN.M.K.bak created on first install
```

Plus a `make test-integration-local` that chains
`install-local-server` → `test-integration` → `restore-brew-server`
so a developer can validate server-side changes with one command.

**Cost:** ~50 lines of Makefile plus a one-paragraph addition to
`tests/integration/README.md`. Zero changes to the test suite or
fixtures.

**Catches:** anyone with server-side code on a PR who remembers to
run `make test-integration-local` before pushing. Doesn't change
the CI story.

**Scope as originally written:** Mac local only. The plan in
`~/.claude/plans/patient-bridging-heron.md` extends this to a
sibling Option 1b for remote Linux (FC / mini3) so the same
one-command validation works for FC server-side PRs too.

### Option 1b — Remote Linux dev-binary bootstrap (FC / mini3 via SSH)

Surfaced as an explicit follow-on after the original Option 1 was
written. The structural problem Option 1 solves on local Mac applies
identically to remote Linux: today, FC server-side PRs validate
against the deb-installed v_{N-1} `shed-server` on `mini3` (or
`$SHED_FC_HOST`), not against the developer's source tree. Without
parity with Option 1, the validation gap closes for VZ but stays
open for FC.

> **Note (post-implementation):** the sketch below was the original
> framing. The shipped form (see §11) corrected some assumptions —
> the deb installs at `/usr/local/bin/shed-server` (not `/usr/bin/`),
> the GOARCH is detected at recipe time via `ssh <host> uname -m`
> rather than hardcoded to `arm64`, and the systemd drop-in uses an
> inline `Environment=` directive (not `EnvironmentFile=`). The
> sketch is preserved as the original record.

Shape mirrors Option 1 with three platform deltas:

- **Cross-compile** for the remote GOARCH. The original sketch
  hardcoded `arm64`; the shipped `build-fc-remote-server` target
  detects the remote arch at recipe time so x86_64 hosts (today's
  default `mini3`) also work.
- **systemd `Environment=` drop-in** at
  `/etc/systemd/system/shed-server.service.d/dev-override.conf`
  replaces the launchd `setenv` step. (The original sketch said
  `EnvironmentFile=`; the shipped form uses an inline `Environment=`
  directive — same effect, fewer indirections.)
- **No codesign** (Linux launches unsigned binaries fine).

```makefile
install-remote-server: build-linux-arm64
	@ssh $(FC_REMOTE_HOST) "test ! -f $(FC_REMOTE_BACKUP) || (echo 'backup exists' && exit 1)"
	@scp bin/shed-server-linux-arm64 $(FC_REMOTE_HOST):/tmp/shed-server-dev
	@ssh $(FC_REMOTE_HOST) "set -e; \
		sudo cp $(FC_REMOTE_BIN_PATH) $(FC_REMOTE_BACKUP); \
		sudo systemctl stop shed-server; \
		sudo install -m 755 /tmp/shed-server-dev $(FC_REMOTE_BIN_PATH); \
		sudo mkdir -p /etc/systemd/system/shed-server.service.d; \
		printf '[Service]\nEnvironment=SHED_BUILD_TOOLS_REF=$(RELEASE_BUILD_TOOLS_REF)\n' | sudo tee $(FC_REMOTE_ENVOVERRIDE) > /dev/null; \
		sudo systemctl daemon-reload; \
		sudo systemctl start shed-server"

restore-remote-server:
	# symmetric — restores from $(FC_REMOTE_BACKUP) on the remote, idempotent.

test-integration-local-fc: install-remote-server
	@SHED_FC_HOST=$(FC_REMOTE_HOST) $(MAKE) test-integration; STATUS=$$?; $(MAKE) restore-remote-server; exit $$STATUS
```

**Cost:** ~80-100 lines of Makefile + a small `tests/integration/`
README addition. Zero changes to fixtures. The backup lives on the
remote (`$FC_REMOTE_BACKUP`), so a developer's workstation reboot
mid-test doesn't strand the remote in dev-binary state.

**Catches:** the FC server-side asymmetry. After Options 1 + 1b
ship, every server-side PR — VZ or FC — has a one-command path to
exercise its own branch.

**Assumed infrastructure** (already true for the existing FC test
path the integration suite uses):
- Passwordless SSH from dev workstation to `$SHED_FC_HOST`.
- `sudo NOPASSWD` for the SSH user (needed for systemd + install).
- Deb-installed shed-server at `/usr/local/bin/shed-server`
  (the deb's actual install path, verified live on `mini3`;
  the shipped Makefile exposes `FC_REMOTE_BIN_PATH` for overrides).
- systemd unit named `shed-server.service`.

### Option 2 — Fixture-launched transient shed-server

Extend `tests/integration/fixtures/server.py` to optionally spawn
its own `shed-server` (built from current source) in a temporary
state dir on a non-default port, then point `shed -s` at it via a
generated config entry. Auto-cleanup on session teardown.

```python
@pytest.fixture(scope="session")
def transient_vz_server(tmp_path_factory):
    state = tmp_path_factory.mktemp("shed-state")
    server = TransientLocalServer.spawn(
        binary=ROOT / "bin/shed-server",
        state_dir=state,
        http_port=18080,
        ssh_port=12222,
        env={"SHED_BUILD_TOOLS_REF": RELEASE_BUILD_TOOLS_REF},
    )
    yield server
    server.shutdown()
```

The `shed_server` fixture selects between brew and transient based
on a pytest flag or env var (`SHED_TEST_MODE={installed,transient}`),
defaulting to `installed` (today's behavior) so the suite is
backward-compatible.

**Cost:** ~150 lines of fixture code (process lifecycle, port
allocation, state-dir cleanup, log capture). Care needed to keep
the two modes structurally identical from the test's point of view
so test code doesn't fork.

**Catches:** Option 1's caseload plus any test run in `transient`
mode by anyone. Closes the local validation gap properly. **Also
closes the CI gap on any runner that has a buildable shed-server**
(GHA macOS runners can build VZ-target binaries even without VZ
support; FC needs KVM and is still ungatable until §3's
availability gap gets a separate answer).

### Option 3 — Split the timing regression gate from the upper-template path

`test_plain_create_timing` today conflates two signals:
- The agent-phase boot path (vsock dial + healthPoll + first health
  response).
- The upper-allocation path (pre-formatted template clone vs
  in-guest `mkfs.ext4`).

A regression in either fires the same alarm. Splitting them:

> **Note (post-implementation):** the sketch below assumed `rootfs_ms`
> would be the discriminator. Validation during PR #157 showed the
> in-guest `mkfs.ext4` cost actually lands inside `agent_ms` (not
> `rootfs_ms`, which stays sub-100 ms in both modes because it only
> covers host-side allocation). The shipped form uses the server log
> marker `[<name>] upper template unavailable (...); formatting in
> guest` from `internal/vz/orchestrator.go:249` as the dev-mode
> discriminator, exposed via `ShedHandle.template_fallback`. Both
> tests SKIP cleanly in dev mode rather than asserting against a
> ceiling that holds in both modes. The intent (split the gates so
> dev binaries are safe to test against) is preserved; the
> implementation mechanism diverged.

```python
def test_create_agent_p50(shed_server):
    """Boot-path p50 regression gate. Skips when the VZ
    template-fallback signal is present on any sample (in-guest
    mkfs.ext4 inflates agent_ms by ~4 s on VZ dev builds for a
    structural reason that isn't a real regression). Reads
    `agent_ms` from PhaseTimer."""
    ...

def test_create_rootfs_template_present(shed_server):
    """VZ-only assertion that the host-side upper-template fast
    path is active. Skips on FC (no host-side template path) and
    on VZ dev mode (template_fallback set). On VZ release mode,
    asserts `rootfs_ms` ≤ 100 ms as a sanity check that the
    host-side clone actually happened fast."""
    ...
```

The split gives a clean regression gate that doesn't false-positive
on dev builds and that doesn't mask an actual agent-path regression
when the dev-build path happens to coincide with one.

**Cost:** ~30 lines of test code + a calibration note in the
fixtures file. No Makefile or fixture-lifecycle changes.

**Catches:** the false-positive case that triggered this whole
investigation. Also makes the suite friendly to anyone who runs it
in either mode.

### Option 4 — CI runner with local shed-server install

GHA workflow that:
- Builds the locally-changed shed-server
- Installs the brew binary (matching the release path)
- Swaps in the just-built binary (Option 1's flow, automated)
- Runs the integration suite
- Tears down

Pros: closes the CI-availability gap for VZ. Cons: GHA macOS
runners are slow and expensive; FC still needs separate
infrastructure (mini3 or equivalent). Probably not worth chasing
until 1+3 actually let a regression slip through.

---

## 6. Why this is worth a discovery doc

Every release cycle has involved at least one server-side PR.
v0.5.8 (the previous release) included #150's deb-postinst change
which had this same coverage gap; v0.5.9 had three (PR-A1, PR-A2,
PR-B1). Each one was validated with "make test-integration: N/N
pass" — and each time, that statement was technically true while
masking the structural gap.

Without action, the next cycle will repeat the pattern. With
Options 1+3, a developer doing server-side work has a one-command
path to actually exercise their code with timing comparable to
release builds. The remaining gap (CI availability) is a separate
piece of work that's worth doing after Options 1+3 have run for a
few cycles and demonstrated the local-validation flow holds.

---

## 7. Locked invariants

If a future PR changes either of these, the relevant Option above
needs to be reconsidered first:

- `internal/version/buildtools.go:BuildToolsRefForTag` returning
  `""` for non-release version strings is **intentional dev-build
  isolation**, not a bug. Any "fix" that makes dev builds resolve
  to the latest release's build-tools image risks a mismatch where
  a source change to the upper-template format silently works
  against a stale image.
- `tests/integration/fixtures/server.py:DEFAULT_AGENT_P50_MS` is
  calibrated against **release-build** behavior. Option 3 (split
  the gate) is what makes it safe to also run against dev builds.

---

## 8. How to recognize this issue in the future

The bug surfaced in this session as an unexpected timing-test
failure. Symptoms a future developer / future-me should treat as
"this is the integration-suite-server-coverage gap, not a real
regression":

| Symptom | What it actually means |
|---|---|
| `test_plain_create_timing[vz]` fires with agent p50 ~5500-6000 ms when nothing in the agent path changed | You're running a `make build` binary against a brew install path with `SHED_BUILD_TOOLS_REF` unset. Set the env var to the most recent release's tag and the gate passes. |
| PR validation says "make test-integration: 40/40 pass" but the PR only changes server-side code (orchestrator, lifecycle, backend internals) | The suite ran against the *installed* shed-server, not your branch. You haven't actually tested the new code paths. Use Option 1's `make test-integration-local` once it exists, or do the manual binary swap. |
| A new release ships and a user reports "shed create got slower" within a day | Possible v0.5.4 → v0.5.5 class regression: release flow built the binary correctly but a config knob (build-tools-ref, healthPoll constant, etc.) didn't propagate. The integration suite running against the old brew install missed it because the new binary wasn't installed yet. |
| `launchd` SIGKILLs the swapped-in shed-server with "Killed: 9" immediately after `brew services start shed` | You forgot `codesign -s -` on the binary after copying it into the brew Cellar. macOS refuses to launch unsigned binaries via launchd. (`spctl -a -vvv` reports "rejected" on the binary.) |

---

## 9. Acceptance criteria for Options 1 + 3

For a future session executing the recommended path, "done" means:

**Option 1 (`make install-local-server` / `make restore-brew-server`):**
- A `make install-local-server` target builds, codesigns, swaps the brew binary, sets `SHED_BUILD_TOOLS_REF`, restarts brew, and prints the restore command.
- A `make restore-brew-server` target reverses all of the above and unsets the env var.
- The restore target is idempotent — running it twice produces no error and no state change.
- The targets stash a backup of the brew binary on first install (`/tmp/shed-server-vN.M.K.bak` or similar) and refuse to overwrite an existing backup without a `--force` flag, so a developer who runs `install-local-server` twice doesn't lose the original.
- A `make test-integration-local` target chains `install-local-server` → `test-integration` → `restore-brew-server`, with the restore running even if the suite fails.
- `tests/integration/README.md` gains a "Validating server-side changes" subsection pointing at these targets.
- Running `make test-integration-local` from a clean checkout with PR-A1's changes hits a non-trivial line count in `internal/{vz,firecracker}/client.go:StartShed`'s defensive zombie check (verifiable via `go test -cover` against the running server — out of scope for v1, but the suite should at minimum exercise the path).

**Option 3 (split the timing gate) — as written; see the post-implementation note in §5 Option 3 above for the corrections that landed in the shipped form:**
- `test_plain_create_timing` is renamed `test_create_agent_p50` and asserts on PhaseTimer's `agent` phase, with a ceiling that holds for release builds AND for dev builds whose template fast path is active. **(Correction: the shipped form skips when `template_fallback` is set on any sample, because the in-guest mkfs cost on VZ dev binaries inflates `agent_ms` by ~4 s for a structural reason that isn't a real regression. The ceiling itself is unchanged from the original `test_plain_create_timing` — `DEFAULT_AGENT_P50_MS["vz"]=2200`, `DEFAULT_AGENT_P50_MS["firecracker"]=2900`, each leaving ~500 ms regression budget.)**
- A new `test_create_rootfs_template_present` asserts that `rootfs_ms` is sub-100 ms when the VZ template fast path is active (signal: absence of the `template_fallback` log marker) and skips with a clear message when the fast path isn't active. FC has no host-side template path and skips unconditionally on FC. **(Correction: the original sketch used `rootfs_ms` as the discriminator; empirically `rootfs_ms` stays sub-100 ms in both dev and release modes, so the shipped form uses the server log marker `[<name>] upper template unavailable` as the discriminator instead.)**
- The fixtures file's `DEFAULT_AGENT_P50_MS` comment block is updated to describe what the gate is *not* covering (the rootfs path) so the next person who fires the alarm doesn't bisect into the upper-template path again.
- A change to `internal/vmutil/agent.go:healthPoll` that genuinely regresses the agent phase by ~500 ms or more (e.g., a noticeable poll-interval bump on top of the existing ~1550 ms VZ median) fires the gate; a `make build` against current main does not.

**Both together — combined recommendation:**
- A developer with a server-side-only change can do: `git checkout -b foo; make build; make test-integration-local`; the suite runs against their code; if green they push with confidence; if red the failure points at their change, not at the upper-template path.

---

## 10. Out of scope

- Replacing the brew/deb install path with something more dev-
  friendly. The current install paths are correct for users; the
  fix belongs in the test-environment layer.
- Mocking out the VM entirely in the integration suite. The whole
  point of the suite is to catch live boot-path regressions; mocking
  defeats the purpose.
- Improving GHA test infrastructure (KVM-on-Linux, macOS Apple-
  Silicon-with-vfkit runners). Tracked separately as the CI
  availability question.

---

## 11. Execution plan — SHIPPED (2026-05-30)

Tracked at `~/.claude/plans/patient-bridging-heron.md` (not in this
repo — Claude Code plan files live in the user's home directory).
Three PRs, foundation-first; all merged 2026-05-30:

1. **PR 1 — Option 3 — Split the timing gate.** Shipped as
   [#157](https://github.com/charliek/shed/pull/157). Renamed
   `test_plain_create_timing` → `test_create_agent_p50`, added
   `test_create_rootfs_template_present`, surfaced the dev-mode
   discriminator via `ShedHandle.template_fallback` (reads the
   `[<name>] upper template unavailable` log marker from
   `internal/vz/orchestrator.go:249`).

   **Design correction caught during validation:** the plan
   hypothesised the in-guest mkfs cost would show in `rootfs_ms`;
   it actually lands in `agent_ms` (rootfs_ms stays sub-100 ms in
   both modes). Discriminator switched from `rootfs_ms` to the log
   marker. Plan §1.1 + §1.4 updated in-place per its own §4.3.
2. **PR 2 — Option 1 (local Mac).** Shipped as
   [#158](https://github.com/charliek/shed/pull/158). `make
   install-local-server` / `make restore-brew-server` /
   `make test-integration-local`. Validates VZ server-side PRs in
   one command. Two bugs caught during validation: `codesign -s -`
   on already-signed binary needs `--force`; `@if ... exit 0` in
   a sub-shell doesn't exit the recipe.
3. **PR 3 — Option 1b (remote Linux FC).** Shipped as
   [#159](https://github.com/charliek/shed/pull/159). `make
   install-remote-server` / `make restore-remote-server` /
   `make test-integration-local-fc`. Validates FC server-side PRs
   in one command via cross-compile (arch detected at recipe time
   via `ssh <host> uname -m`) + scp + systemd drop-in + suite +
   restore. Two plan corrections (path is `/usr/local/bin/shed-server`
   not `/usr/bin/`; mini3 is x86_64 not arm64) caught live. Four
   review iterations (CodeRabbit x2 + codex x2) caught real chain-
   target bugs around auto-restore semantics + pre/post snapshotting
   to avoid consuming pre-existing backups.

Workflow promise (now TRUE):

> Every server-side PR — VZ or FC — runs the suite against its own
> branch on the appropriate host (local Mac vfkit or remote Linux
> firecracker) in one command before opening, and that statement
> is true and meaningful, not a brew-binary alibi.

User-facing docs updated to reflect this: `CLAUDE.md` has a
"Server-side changes — required e2e validation" section and a
"Performance impact — vet against the released version" section;
`docs/development/testing.md` and `tests/integration/README.md`
have the corresponding operator guides. The pre-push validation
discipline (the gate that the v0.5.9 cycle skipped) is now
explicit in CLAUDE.md, and the Makefile targets enforce the
"restore even on suite failure" + "don't consume pre-existing
backups" semantics so a developer using them can't accidentally
strand the host.

---

## 12. Cross-host bootstrap framing (post-v0.5.9 reflection)

The original Option 1 (§5 above) was scoped to local Mac only — the
discovery surfaced on the developer's mac during PR-B1 validation,
so the framing was Mac-shaped. After v0.5.9 shipped, surfacing the
asymmetry showed that **the structural problem isn't "local Mac
testing is hard"; it's "the suite tests whatever's installed on the
target host."** That generalizes:

| Target | Today | After Options 1 + 1b |
|---|---|---|
| Local Mac vfkit (VZ) | Brew-installed shed-server tested via `make test-integration`. Dev binaries require manual 7-step swap. | `make test-integration-local` swaps a dev binary, runs the suite, restores. One command. |
| Remote Linux firecracker (FC / mini3) | Deb-installed shed-server tested via SSH'd `make test-integration`. Dev binaries require manual cross-compile + scp + systemd override + restore. | `make test-integration-local-fc` does all of it. One command. |
| Remote Mac vfkit (VZ on another mac) | Not covered. | Still not covered — extend Option 1 with launchd override + codesign step on the remote if a use case appears. Not in scope for the patient-bridging-heron plan. |

The pattern is the same in each row: **bootstrap a dev binary onto
the target host, run the suite, restore.** Per-host plumbing
differs (launchd vs systemd, codesign vs none, brew Cellar vs deb
path, local cp vs scp) but the contract is uniform.

### Side-by-side (Option 2) deferred — explicit revisit trigger

Option 2's value is running a release binary AND a dev binary
simultaneously on the same host for side-by-side comparison (e.g.,
"release agent_p50 = 1450 ms, dev agent_p50 = 1490 ms"). For PR
validation that gates merge, swap-based testing (Options 1 + 1b)
delivers the same signal: record baseline against release, swap to
dev, record again, compare manually.

The ~30-second swap delta and per-run jitter don't matter for PR
validation. They DO matter for:

1. **Timing micro-benchmarks** at the noise-floor level. Example:
   "I tuned `healthPoll` from 250 ms to 200 ms — what's the
   agent_p50 delta with ≤ 10 ms noise?" Swap-based can't deliver
   that; side-by-side simultaneous runs share OS-state-of-the-moment
   and reduce noise.
2. **Regressions that slip through Options 1+3** that side-by-side
   would have caught. The post-mortem informs Option 2's design.
3. **CI integration (Option 4) entering scope.** Side-by-side is
   how you do "for each PR, run the suite against the PR's branch
   AND against main, report a diff" — that workflow needs Option 2.

Without one of these triggers firing, the ~150 LOC of fixture
lifecycle code (port allocation, transient state-dir, log capture,
race-prone process supervision) plus the structural change to every
test (per-test server selection) don't pay for themselves.

When a trigger fires, this section + Option 2's §5 sketch + the
patient-bridging-heron plan's existing infrastructure
(`RELEASE_BUILD_TOOLS_REF` resolution, backup/restore patterns,
dogfood-via-PR#156 test approach) are the starting points.

### What remains a gap after Options 1 + 1b + 3 ship

- **CI availability** (no /dev/kvm in GHA, no Apple-Silicon-with-vfkit
  runners). Same as today; Option 4 territory. Tracked separately.
- **Mac remote bootstrap.** Lower priority than Linux remote; extend
  if a use case appears.
- **Side-by-side simultaneous run** (Option 2). Deferred per above.
- **A `--shed-server-version` flag** on the suite for picking which
  installed version to target (vs. the current "whatever's running"
  shape). Possible follow-on if Option 1+1b's "swap then restore"
  shape proves annoying for the bisect use case.

These are documented here so a future cycle isn't surprised by them.
