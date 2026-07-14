# Remote-control sessions (the Agents pane)

The **Agents** pane drives agent sessions inside a shed — `claude` (REPL and
remote-control broker), `codex`, `cursor-agent`, `opencode`, and plain shells:
detached `tmux` sessions named `rc-<slug>`, created and torn down over SSH. The pane
lists them with a live state, a "made by … · age" provenance line, an **Open console**
button (attach a terminal to the session's tmux), an **Open in Claude** button (the
`claude.ai/code` URL — Claude kinds only; the other agents have no browser URL), and a
kill button.

The create sheet's kind chips are **gated on the shed's capabilities**: the app reads
`shed-ext-rc capabilities` (kinds + per-agent install/version + features) and only
offers kinds whose agent is installed on that shed's image. A shed whose baked-in
`shed-ext-rc` predates multi-agent RC advertises no capabilities block and offers only
the Claude/shell kinds it supports; recreate the shed to pick up the newer agents.

## RC Session Convention v2 (`SHED_RC_*`)

shed-desktop is a conformant client of the tool-neutral **RC Session Convention
v2** (published by `shed-remote-agent`; spec:
`docs/reference/rc-session-convention.md` there). The tmux session is the single
source of truth — all durable metadata lives in its session environment, and
`state`/`url` are always derived fresh from the pane, never stored. This lets
shed-remote-agent, shed-desktop, the `shed` CLI, and future clients discover,
classify, attach to, and tear down each other's sessions without a registry.

The SSH+tmux choreography (bootstrap, classification, the `SHED_RC_*` metadata,
workspace-trust pre-seeding) lives in the **`shed-ext-rc`** guest binary baked into
the shed image; shed-desktop invokes it over SSH (`shed-ext-rc create --wait` /
`list` / `kill`) and decodes its neutral JSON DTO. So every tool produces
byte-compatible sessions. The metadata the binary writes:

| Key | Meaning |
|-----|---------|
| `SHED_RC_V` | Schema version (a positive integer; `2`). |
| `SHED_RC_ID` | Stable opaque id (a lowercase UUIDv4), generated once at create. |
| `SHED_RC_DISPLAY_NAME` | Human name; also `claude --name`. |
| `SHED_RC_KIND` | `claude-broker` \| `claude-rc` \| `codex` \| `cursor` \| `opencode` \| `shell` (v2 renamed v1's `agent`/`repl`). An unrecognized value is preserved verbatim and rendered neutrally (name + state only) — never aliased to `claude-broker`. |
| `SHED_RC_WORKDIR` | Working directory at create. |
| `SHED_RC_CREATED_BY` | Provenance `<tool>/<version>`, e.g. `shed-desktop/0.1.0`. |
| `SHED_RC_CREATED_AT` | Creation time, RFC 3339 UTC with a trailing `Z`. |
| `SHED_RC_TARGET` | Optional, advisory target label (`shed:<name>@<host>`); non-authoritative. |

### Managed vs legacy

A session is **managed** when `SHED_RC_V` is `2` or higher (a higher version stays
managed — known fields rendered, unknown keys ignored, never dropped). A `v1`
session (or an unrecognized kind) is treated as legacy — v2 does not alias the old
`agent`/`repl` values.

Any `rc-*` session without a valid (≥ 2) `SHED_RC_V` is **legacy/unmanaged**: it is
still listed and killable, but rendered with defaults (`kind = claude-broker`, a
`<shed>/<slug>` fallback name, the default workdir), any stray `SHED_RC_*` /
`SRA_*` values are ignored, and the Agents pane shows a `legacy` badge and
**confirms before killing** it.

> **Clean break.** v2 renamed the kinds (`agent`→`claude-broker`, `repl`→`claude-rc`)
> and bumped `SHED_RC_V` to `2` with no aliasing; earlier builds also used an
> app-named `SRA_*` prefix, no longer written or read. Until every tool you use
> adopts v2, each renders the other's older sessions as legacy/unmanaged (still
> attachable/killable, defaults only); kill + recreate a session to restore its
> metadata. The shed image must ship `shed-ext-rc` before this build can create
> sessions.

## Derived state

`state` and `url` are computed from a `tmux capture-pane` by the pure classifier
(`rc.classify` over IPC), never stored:

| `state` | Meaning |
|---------|---------|
| `starting` | No URL / status line yet (incl. `claude` still in first-run setup). |
| `ready` | Terminal-good for the kind (a URL for `claude-broker`/`claude-rc`; any output for `shell`). |
| `reconnecting` | `claude remote-control` is reconnecting (`claude-broker`). |
| `needs-trust` | `claude` refused — workspace not trusted (attach via the console button to trust). |
| `needs-auth` | `claude` needs a `claude.ai` login (attach + `claude auth login`). |
| `dead` | The tmux session is gone. |

## Transport

Listing pipes a single batched script to a remote `bash` over **stdin** (not
`bash -c`) and runs tmux directly: on shed images where the user has no
controlling terminal, tmux invoked under `bash -c` fails with "open terminal
failed: not a terminal", but works when bash reads from stdin. Random per-call
markers (`@@RC:<nonce>:…`) frame each session's env dump + pane so neither pane
text nor a metadata value can forge a delimiter.

As of this release the server enriches session listings with this metadata
server-side: `GET /api/sessions` and `GET /api/sheds/{n}/sessions` return an `rc`
object per `rc-*` row (populated by the server execing `shed-ext-rc list` over the agent
channel). HTTP-only clients no longer fan out one SSH connection per shed to read RC
state (closing the [shed follow-up](https://github.com/charliek/shed/issues/199)). Pass
`?rc=0` to skip enrichment on a hot poll path. `GET /api/overview` aggregates the same
data — server info (with a `features` discovery array), disk usage, and every shed with
its rc-enriched sessions and per-shed `rc_capabilities` — into one call for
mobile-style clients.

## Live activity (the rc hub)

On VZ sheds a resident guest daemon — the **RC activity hub** (`shed-ext-rc serve`) —
derives a live `activity` dimension for each session and, for codex, a message feed and
gated input. Two server endpoints expose it to clients (advertised via the `rc-proxy`
and `rc-events` feature tokens on `GET /api/info` / `GET /api/overview`):

- **`GET/POST /api/sheds/{name}/rc/*`** reverse-proxies the hub's `/v1` API (session
  list, SSE `/v1/events`, the codex `/messages` feed, and `POST /input`), ensure-starting
  the hub on demand. The server is the authorization boundary; the hub is loopback-only
  inside the guest.
- **`GET /api/rc/events`** is a single demand-driven aggregate SSE stream carrying
  `activity.changed` / `session.updated` / `message.appended` across every shed, so a
  client subscribes once for the whole host.

When a hub is running, enriched session listings additionally carry `activity`,
`activity_at`, and `last_message` inside each row's `rc` block (absent when no hub is
running — e.g. the hub hasn't started yet or the image predates it — in which case the
proxy degrades to `503 RC_HUB_UNAVAILABLE` and clients hide activity affordances). This
is backend-agnostic: both VZ and Firecracker reach the loopback hub through the guest
agent's vsock TCP proxy. The full contract — lifecycle-trumps-activity
precedence, SSE envelopes, message-feed and input semantics — lives in
[shed-ext-rc](../extensions/rc-helper.md#the-rc-activity-hub-serve).
