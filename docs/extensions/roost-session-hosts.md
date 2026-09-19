# Putting `roost-session` on a shed or machine

S5 of the Roost Pivot (`epics/roost-pivot.md`) lets the shed desktop and mobile apps install
and start a [roost](https://github.com/charliek/roost) `roost-session` daemon on a shed or a
`machines:` entry that doesn't already have one — over the target's own SSH, with the same
choreography roost's own UI uses. This turns a shed's `codex` and `cursor` rows from
**liveness only** into roost-sourced activity, matching what a `machines:` entry already gets —
demonstrated live, not only in the hermetic tests that cover the mechanism this page describes:
a real shed's `codex` row carries roost-sourced `activity` with `source: "roost"`
(`epics/roost-pivot.md`'s S5 row).

This page describes the mechanism the clients drive: where the bytes come from, what consent
promises, what the rollback guarantee actually covers, and the edges (`shed reset`, two
clients, mini3-style read-only hosts). The button, the dialog, and the toast are the desktop
UI's concern (Tauri `roost.probe`/`roost.preview`/`roost.bootstrap`/`roost.launch` — see
[Desktop IPC](../desktop/ipc.md)); this page is about what happens once you click it.

## The source ladder

An install streams a `roost-session` binary at the target. Where those bytes come from is
decided in order, and the first rung that can answer wins:

1. **Override** — the file named by `$ROOST_SESSION_INSTALL_BIN` (roost's own environment
   variable; shed reads it itself). A binary of the wrong OS or architecture is refused rather
   than silently skipped, because an explicit override that cannot be used means you asked for
   a *specific* binary — installing a different one instead would be worse than failing.
2. **Sibling** — the `roost-session` sitting beside the client binary itself, **desktop only**.
   This requires the desktop to be running on Linux (there is no local candidate to compare
   against on macOS) and a local `identify` against that sibling reporting session protocol 6 —
   the same protocol this build speaks.
3. **Release asset** — a `roost-session-<version>-linux-<amd64|arm64>` tarball plus its
   `.sha256` sibling, fetched over HTTPS and checksum-verified, behind a version pin (below).
   **Both clients** (desktop and mobile) use shed's own fetch here, not roost's own `curl`
   rung — the descriptor shed hashes must be the exact descriptor it streams to the target, and
   roost's own download path keeps that open, hashed handle private. Contract: HTTPS only,
   redirects followed only to HTTPS, the asset capped at 256 MiB and its checksum file at
   4 KiB, downloaded into a private, freshly created directory with partial files removed on
   any failure.
4. **Nothing** — the target is left completely untouched, and the client shows the sentence in
   the next section.

### The release rung is real code, but it has never run against a real release

**`RELEASE_PIN` is `None` today, because no published roost release speaks session protocol
6** — the latest, `0.0.19`, speaks protocol 2. This is checked against roost's own repository
at the time of writing, not assumed. The message a client shows when every rung has failed:

> no roost release speaking session protocol 6 is published yet (the latest, 0.0.19, speaks
> 2). On a Linux machine with a protocol-6 roost installed the desktop uses that roost-session;
> otherwise point `ROOST_SESSION_INSTALL_BIN` at a protocol-6 build. `<target>` was left
> untouched.

Flipping `RELEASE_PIN` once roost ships a protocol-6 release is a one-line change (it sits
beside the version/protocol pair the message above is built from). **Say this plainly: the
release-asset rung is implemented and unit-tested against a loopback HTTP fixture, but it has
never been exercised against a real, published roost release, and nothing in this codebase
claims otherwise.** Until that pin flips, the only ways a fresh Linux target gets a
`roost-session` are the override variable and — desktop only — the sibling rung.

One source-related decision in this design is recorded as **not yet confirmed by the
project owner** rather than settled: fetching the release asset with shed's own HTTP client
instead of roost's `curl` rung (above). It is implemented as described here; it should not be
read as a closed decision.

## Compatibility: protocol only, not roost's exact triple

roost's own UI refuses a `roost-session` unless its version, protocol, and embedded ghostty
snapshot all match exactly. shed's compatibility rule is narrower and different: **a target is
compatible if its `session.identify` reports `session_protocol == 6`, full stop.** shed does
not build roost, never negotiates a ghostty snapshot, and cannot know a future release's exact
build fingerprint in advance — the protocol number is the one thing this codebase can commit
to checking. The accepted, documented consequence: a roost UI that later connects to a
shed-installed session applies its own stricter exact-triple rule and may offer to reinstall
something shed considers perfectly fine.

## What actually happens (the plan matrix)

| The target looks like | The action |
|---|---|
| Nothing there | **Install**, then **Start** |
| A stale or incompatible binary, nothing running | **Update** (backup + replace) then **Start** |
| A compatible binary, nothing running | **Start** |
| A session already running protocol 6 | Nothing — status only |
| A session running any other protocol | **Report only.** Never stopped, never restarted. |
| No source available (see the ladder above) | Unavailable — the ladder's own sentence in place of a button |

Every one of these is preceded by a fresh **probe** (read-only: OS/arch check, candidate
binaries, session state) and, for Install/Update, a **consent step** naming what will happen,
where, and where the bytes come from — nothing is downloaded or written before that consent is
given, and the install re-probes and refuses if the target changed since the card was shown.

**The protocol-5 case, specifically.** A shed at protocol 6 that finds a session already
running the previous generation, protocol 5, lands on the "any other protocol" row above: it
refuses that session by name, naming both protocol numbers in the report and telling you which
side to upgrade — the same mismatch report any other protocol disagreement gets. It never stops
or restarts the running session to do this (pin P6, above); the report is the only action
taken.

## The rollback promise, stated exactly as narrow as it is

An Install or Update follows roost's own staged order: prepare a temporary file beside the
destination, stream the binary onto it, verify the *staged* file's identity, commit it into
place (a rename), verify the *installed* file's identity, then discard the pre-commit backup.
The rollback guarantee tracks that order precisely — it is **not** "any failure leaves the
target unchanged":

- **A failure at prepare, stream, verify, or commit** removes the temporary file and leaves
  the incumbent binary exactly as it was (putting it back first, if the commit's rename had
  already landed before the failure was detected).
- **A failure during the post-commit identify** restores the incumbent when there was one to
  restore, and says plainly whether that restoration succeeded (`restored: true`/`false`) —
  including naming the path a binary is stranded at if it could not be put back.
- **Once the pre-commit backup has been discarded, the guarantee ends.** A `roost-session
  start` failure — or a failure in the identify that follows a successful start — after that
  point leaves the **new** binary in place; nothing rolls the install back. The copy at that
  point reads: "installed but wouldn't start: try `roost-session start` on `<target>`."

In short: the rollback promise covers everything up to and including the discard of the
backup. After that line, a failure is reported honestly, but the new binary stays.

## Hooks: the step that makes the payoff real

After a **Start that the client itself performed**, it sends
`session.set_agent_hooks {agents: ROOST_WIRED_AGENTS, client: "shed-desktop"}` (or
`"shed-mobile"`) to the fresh session — one wire call, nothing in front of it. Session
protocol 5 dropped the lease entirely, so there is no `session.connect` to open first and
nothing to hold or lose before sending it.

**At session protocol 6 this op is a raise, and a raise only ever widens.** `ROOST_WIRED_AGENTS`
is shed's own constant naming roost's whole wireable set, by value — exactly five names,
`claude`, `codex`, `cursor`, `grok`, `opencode` — and shed sends that same array on every call.
The host unions those names into its own `agent-hooks` key; there is no destructive direction
left on this wire. Generation 5's `{mode: "auto", skip: [...]}` shape, and the `mode: "off"`
"come clean" spelling that went with it, are both gone — nothing replaced them, because roost
made removal a deliberate local act on the host (`roostctl agent uninstall`), not something a
client can request over the wire.

**A raise re-widens — say this plainly, because it surprises people.** If someone narrows a
host's `agent-hooks` key by hand between two of shed's watcher cycles (`roostctl agent set`,
say), shed's very next cycle puts every one of the five names right back. That is not a bug:
shed re-sends the identical raise unconditionally, every cycle, with no memory of what the host
looked like a moment ago. If you want a host wired to fewer than everything, doing it by hand on
the host does not stick against a shed that is still watching it — removal has to stay a
deliberate, one-time local act, done knowing shed will not fight you over it as long as you stop
pointing a shed client at that host afterward.

**Unknown names come back verbatim, never filtered.** An agent name the host has no adapter for
at all lands in the result's `skipped` list with reason `"unknown"`, exactly as the host worded
it, and the rest of the raise still applies — the UI shows that row as-is rather than hiding or
rewording it. Seeing `skipped: "unknown"` for a name that should be known is the drift signal
that roost renamed or dropped an adapter shed's constant does not know about yet.

**gx is a special case worth knowing before you hit it live.** shed never sends `gx` — gx is the
owner's grok fork, and it shares `$GROK_HOME` with `grok` itself, so roost reports both under
`source: "grok"` and wiring `grok` wires gx too. But roost's installed-test for `grok` is
`$GROK_HOME` existing as a directory — so **a host with gx on `PATH` but no `~/.grok` yet
reports `grok` as `skipped: "not installed"`** until gx has actually run once and created that
directory. This is the single most likely surprise on a fresh live host: gx being on `PATH`
does not mean roost sees it as installed yet.

**This is a dotfile mutation, performed by the *host* session, not by shed.** shed sends one
wire call; `roost-session` on the target is the process that writes into the configuration
files of whichever of those five agents it finds already configured there, and nothing else.
The consent copy shown before Install/Update/Start names this plainly: "roost-session will also
wire its hooks into the agents already configured there — claude, codex, cursor, opencode,
grok — and nothing else."

**When it recurs:** the raise only wires an agent whose configuration directory exists *at that
moment*. An agent set up later is not retroactively wired by this one call — it gets wired the
next time hooks are (re)sent, which is:

- every `shed start` (each one performs a fresh Start-and-hooks cycle if a bootstrap runs), and
- every successful cycle of the watcher for a target this client bootstrapped — re-sent on
  every reconnect, not only once at install time.

**One op, no token, last writer wins.** Every client that wires a target sends the identical
call — the desktop, the phone, and any roost UI that connects all say `set_agent_hooks
{agents: ROOST_WIRED_AGENTS}` — and the host session applies whichever one it heard most
recently. That is the design, not a gap in it: one user owns every client that talks to their
sheds, so hook wiring is open to all of them and none of them needs to ask first. There is no
session-level owner left to displace and nothing a second client could "take over" from a
first — `roostctl agent status` on the host is where you read which client wired an agent's
hooks last.

## The PATH warning: named, never acted on

Immediately after Start, the bootstrap re-runs the same `command -v roost-session` check the
probe used. If the answer differs from — or is missing compared to — the binary that was just
installed and started, that fact rides into the result as a warning string surfaced in a
toast. **Nothing edits a dotfile or a shell profile to fix it.** Per roost's own precedent
(and pin P5 of this design), shed follows roost's PATH lead exactly and never writes into
`.bashrc`, `.profile`, or any other dotfile on the target — a mismatch is reported so you know
to fix your own PATH, and left at that.

## `shed reset`

`shed reset` wipes a shed's writable upper filesystem, including `~/.local/bin` — which is
where an install normally lands. There is no special-case handling for this: the next probe
after a reset simply reports `Missing` again, exactly as it would for a shed that never had
`roost-session` installed, and the client offers Install again from scratch. Nothing about the
hooks wiring or the source ladder needs to know a reset happened.

## Two clients, one target

The desktop app and the mobile app can both bootstrap the same target. Nothing coordinates
between them beyond the filesystem itself: concurrent installs are **last-writer-wins**, using
the same `.bak.<pid>` backup-chain naming roost's own installer already uses, with the
post-commit identify as the only detector that something unexpected landed (a different
protocol-6 build than the one this client just streamed). This is treated as the *normal*
case, not a hazard to guard against further — a person with both apps open, bootstrapping the
same shed from their phone and their laptop, is exactly the scenario this is built for.

## A read-only example: mini3

A target that is already running a `roost-session` speaking a protocol other than 6 (mini3, at
the time of writing, runs a release build of protocol 2) gets the **Report** row from the plan
matrix above and nothing else — probing it is read-only, and its daemon's start time is
unaffected before and after a probe. This is pin P6: an existing session is reported, never
restarted, no matter how out of date it is. The message names both protocol numbers and tells
you to upgrade whichever side is older.

## See also

- [Desktop IPC](../desktop/ipc.md) — the `roost.probe`/`roost.preview`/`roost.bootstrap`/
  `roost.launch` ops the desktop app calls to drive this.
- [The `shed` roost provider](roost-provider.md) — S4, the palette entry that opens a tab once
  a session exists.
- `epics/roost-pivot.md` — the Roost Pivot's tracking table (not a published doc page).
