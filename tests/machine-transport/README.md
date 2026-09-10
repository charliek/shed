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
| `dartssh2` | shed-mobile | nothing, as of plan 013 — the roost-session reach it drives execs a Rust-composed string wholesale rather than an `sx`-style argv (see "The Dart leg" below) |

Two implementations of one wire contract drift silently, and the drift is
invisible in ordinary testing because both usually *work*. This suite is the
mitigation, and it was not hypothetical: shed-mobile's own `shell_quote.dart`
quoted **conditionally** (bare-safe tokens unquoted) while Rust quotes
**always**. Post-`bash` the argv matched, so nothing failed — but the bytes on
the wire differed, and "these two transports agree" was simply untrue.

## The two legs

The contract lives in **`scenarios.json`** + **`goldens/`**, deliberately in
neither implementation's source tree, because a contract that lived inside one
leg would not be a contract.

| leg | lives in | run with | asserts |
|---|---|---|---|
| **Rust** | `crates/shed-core/tests/machine_transport_contract.rs` | `cargo test -p shed-core` | Rust composes `goldens/wire.json` — the BYTE-level pin |
| **live** | here | `make test-machine-transport` | that quoting really delivers `scenarios.json`'s argv through a real sshd |

**There is no third, Dart-side leg, and never has been.** An earlier revision
of this README claimed one — shed-mobile composing the same
`goldens/wire.json` under its own `make check` — but shed-mobile only ever
had a hand-written `shellQuote` table (`test/core/shell_quote_test.dart`), not
a leg that reads this contract. See "The Dart leg" below for where that
composing actually lives, and where it's going.

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

## The Dart leg

There isn't one, and plan 013 (the Roost Pivot's S1/S3m) is the reason there
won't be. shed-mobile composes no `sx`/`shed-ext-rc` wire of its own for
reaching a machine's `roost-session` — since that plan it execs the string
`roost_ipc::ssh::remote_command()` hands it through the bridge
(`roost_remote_command()` over FRB), the same composer every other client
uses, so there is nothing left for a Dart-side leg to independently verify.

The `shed_core::machine::*` argv builders this contract still pins — the ones
`sx rc` sends over the `ssh` binary — are on the S6 deletion path (see
`epics/roost-pivot.md`, S7): once the RC hub, `shed-ext-rc`, and the Go
engine retire and `sx` is stripped to a stub, this contract goes with them.
Until then, the Rust and live legs above are the whole of it.

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

### Recording `received.json` on a box with no sshd

`wire.json` is composed in-process, so it records anywhere. `received.json`
comes from the LIVE leg, which skips without an sshd — and a skip records
nothing, so a new scenario would land with half a golden. Many Linux dev boxes
have `openssh-client` only.

Record it in a throwaway container instead of installing a server on the host
(the run needs a **non-root** user — sshd refuses a root publickey login under
its default `PermitRootLogin prohibit-password` — and `/run/sshd`, its privilege
separation directory, which the package does not create):

```bash
docker run --rm -v "$PWD/tests/machine-transport:/work" ubuntu:24.04 bash -c '
  set -e
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq && apt-get install -y -qq openssh-server openssh-client python3 python3-pytest
  mkdir -p /run/sshd
  useradd -m -s /bin/bash probe
  cp -r /work /home/probe/mt && chown -R probe:probe /home/probe/mt
  su probe -c "cd /home/probe/mt && UPDATE_GOLDEN=1 python3 -m pytest -q"
  cp /home/probe/mt/goldens/*.json /work/goldens/
  chown --reference=/work/scenarios.json /work/goldens/*.json'
```

It copies the suite OUT of the mount and the goldens back, so a failed run
cannot leave the checkout half-written. `python3 -m pytest` rather than `uv run`
because the only dependency is pytest and the container needs no network beyond
apt.

The `chown` matters the day the contract gains a **new** golden filename: the
`cp` runs as root in the container, and `cp` onto an EXISTING file keeps that
file's owner — so overwriting today's two tracked goldens is invisible, while
creating a third would leave a root-owned file in the checkout that the
developer then cannot rewrite. `--reference` copies the ownership of a file that
is certainly the developer's, so the recipe needs no uid passed in.

## Changing the contract

1. Edit `scenarios.json` and **bump its `version`**.
2. Re-record both goldens (`UPDATE_GOLDEN=1`).
3. Update the pinned version in
   `crates/shed-core/tests/machine_transport_contract.rs`.

The version exists so the Rust leg cannot silently drift from what's checked
in: `machine_transport_contract.rs` pins the revision it was last validated
against, so an edit here that skips step 3 fails loudly in `cargo test
-p shed-core` instead of leaving the Rust byte pin and the live leg's fresh
read of `scenarios.json` quietly disagreeing about which contract is current.
There is no third repo to keep in step (see "The Dart leg" above).

A scenario is never deleted to make a leg pass.
