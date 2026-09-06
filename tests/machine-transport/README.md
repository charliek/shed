# tests/machine-transport — the machine-transport differential

The **fifth** pytest suite in this repo, and — like the other four — never merged
with them:

| suite | what it drives |
|---|---|
| `tests/integration/` | a LIVE `shed-server` create cycle |
| `tests/host-agent-diff/` | the `shed-host-agent` daemon's wire output, vs recorded goldens |
| `desktop/tools/shedtest/` | the desktop app over its IPC socket |
| `tests/rc-parity/` | `sx rc <verb>` vs the Go oracle, side by side |
| **`tests/machine-transport/`** | **every transport that reaches a machine over SSH, against one shared contract** |

## Why it exists

SSH has **no argv API**. A remote command is sent as ONE string that the far
side's shell re-parses. Plan 012 gave machines more than one client, and they do
not share an SSH implementation (that was a deliberate decision — see the plan's
§2.1):

| transport | used by | composes the wire line in |
|---|---|---|
| the `ssh` binary as a child process | `sx`, the Tauri desktop app | Rust (`shed_core::machine::display_line`) |
| `dartssh2` | shed-mobile | Dart, via the FRB bridge |

Two implementations of one wire contract drift silently, and the drift is
invisible in ordinary testing because both usually *work*. This suite is the
mitigation, and it was not hypothetical: shed-mobile's own `shell_quote.dart`
quoted **conditionally** (bare-safe tokens unquoted) while Rust quotes
**always**. Post-`bash` the argv matched, so nothing failed — but the bytes on
the wire differed, and "these two transports agree" was simply untrue.

## The three legs

The contract lives in **`scenarios.json`** + **`goldens/`**, deliberately in
neither implementation's source tree, because a contract that lived inside one
leg would not be a contract.

| leg | lives in | run with | asserts |
|---|---|---|---|
| **Rust** | `crates/shed-core/tests/machine_transport_contract.rs` | `cargo test -p shed-core` | Rust composes `goldens/wire.json` — the BYTE-level pin |
| **live** | here | `make test-machine-transport` | that quoting really delivers `scenarios.json`'s argv through a real sshd |
| **Dart** | `shed-mobile` | its own `make check` | Dart composes the SAME `goldens/wire.json` |

**The two layers are covered by different legs, deliberately.** The live leg
cannot see a drift from always-quoting to conditional quoting, because after
`bash` parses either form the argv is identical — that is precisely why the byte
pin lives in Rust. Conversely the Rust leg cannot tell you those bytes actually
work against a real server. Neither covers both alone.

The live leg reimplements the quoting **rule** in five lines rather than calling
the Rust implementation — if it called Rust, a quoter bug would be invisible
because both sides would carry it. It also swaps `argv[0]` for a probe script
(the scenarios' `argv[0]` is `sx`, which does not exist on the test host), so it
transmits the same quoting rather than the literal golden line. What it asserts —
the argv the remote process received — comes from `scenarios.json`, not from
either golden, so `UPDATE_GOLDEN=1` cannot paper over a real quoting bug.

## What the live leg actually measures

A remote receiver script that base64-encodes each argument it was handed, one
per line. Whatever the composed line means to the remote shell, that is the argv
the process really got. base64 rather than escaping because the scenarios
deliberately contain newlines, tabs, backslashes and non-ASCII, and every
escaping scheme that must survive `sh` is a second thing that can be wrong.

Scenarios cover the cases that break hand-rolled quoting: embedded single and
double quotes, backslashes, `$VAR`, `$(…)` and backticks, `;`/`&&`/`|`,
redirection and globs, newlines, tabs, leading dashes, unicode, and the empty
argument. One test asserts the security property directly: a payload that would
`touch` a marker file must arrive as inert text and the marker must not exist.

## The forwarded-hub family

`test_hub_tunnel.py` runs `ssh -N -L` through the same hermetic sshd to a fake
hub, and asserts **delivered frames** — health, the snapshot, and the SSE frames
themselves — not merely that the tunnel came up.

That emphasis is deliberate and expensive-lesson-shaped. Plan 012 S2 found that
every event from a directly-read hub was being dropped at decode: a client opened
a healthy tunnel to a healthy hub, connected its stream, and rendered nothing
forever. **A "the tunnel is up and `/v1/health` answers" check passes with a
completely dead feed.** So the fixture pins the real frame shape, including the
empty `shed` a directly-read hub emits (it has no shed to name — only the shed
server's aggregate proxy fills that in).

## Running

```bash
make test-machine-transport         # from the repo root (uv guard)

# or directly:
cd tests/machine-transport && uv sync && uv run pytest -v
```

Requirements: **uv**, and the three OpenSSH executables — **`sshd`** (serves),
**`ssh-keygen`** (mints the throwaway host + client keys) and **`ssh`** (the
client under test). The suite skips cleanly if any is missing. No Rust, no Go, no
tmux. Nothing leaves 127.0.0.1.

Hermetic by construction: a throwaway OpenSSH server per session with freshly
generated host and client keys under a temp dir, its own `authorized_keys` and
`known_hosts`, and a loopback-only fake hub.

The client side is isolated with **`-F /dev/null`**, **`IdentityAgent=none`**,
`IdentitiesOnly=yes` and a pinned `GlobalKnownHostsFile` (see
`conftest.py:isolation_argv`). `IdentitiesOnly=yes` alone is **not** enough —
this repo already learned that in `tests/integration/test_bootstrap.py`: without
`-F /dev/null`, OpenSSH still reads the user's `~/.ssh/config`, where a `Host *`
stanza can add a `ProxyJump` (the run leaves loopback), a `ControlMaster`
(multiplexing onto a foreign socket), extra `IdentityFile`s (offered alongside
`-i`, exhausting `MaxAuthTries`), or a `RemoteCommand` — which would silently
change the very thing this suite measures.

## Recording and updating goldens

```bash
UPDATE_GOLDEN=1 uv run pytest        # (re-)record every visited cell
```

Recording is idempotent by content, so an unchanged golden leaves a clean
`git status`. **A missing golden is a failure, not an auto-record** — for an
existing cell it means the file was deleted, and re-recording would silently
bless whatever the code does today.

## Changing the contract

1. Edit `scenarios.json` and **bump its `version`**.
2. Re-record both goldens (`UPDATE_GOLDEN=1`).
3. Update the pinned version in
   `crates/shed-core/tests/machine_transport_contract.rs`.
4. **Re-run the Dart leg in `shed-mobile`** and update its pinned version.

The version exists precisely so step 4 cannot be skipped silently: shed-mobile
pins the revision it was last validated against, so an edit here that never
reached the other repo fails loudly there instead of leaving both repos green and
disagreeing.

A scenario is never deleted to make a leg pass.
