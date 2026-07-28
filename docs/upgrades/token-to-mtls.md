# Upgrading to mTLS

This guide covers adopting `auth.mode: mtls` — the client credential becomes a
short-lived certificate bound to a private key that never leaves the client's
device, issued over the same SSH `_bootstrap` channel that mints tokens today,
instead of a bearer token. It is **additive**, not a breaking config rename
like the v0.7.x auth patches: `open` and `token` are unaffected, `mtls` is a
new value alongside them, and `token` is retained (not deprecated) for
`curl`/scripts/CI/third-party callers that can't present a client certificate.
See [Security › mTLS mode](../reference/security.md#mtls-mode) for the full
model.

!!! note "Not a breaking config change"
    Unlike the v0.7.1–v0.7.4 auth patches, upgrading to a build with mTLS
    support changes nothing for an existing `open` or `token` server — no
    config key is removed or renamed. This guide is about *adopting* `mtls`
    where you want it, on your own schedule.

## Upgrade order: clients before servers

1. **Upgrade every client first** — the CLI, host-agent, desktop app, mobile
   app — to a build with mTLS support. A pre-mTLS client has no code path to
   obtain a certificate at all, so it must be upgraded before any server it
   talks to flips to `mtls`.
2. **Flip each server's `auth.mode: mtls`** independently, on your own
   schedule. There is no fleet-wide cutover requirement — mode is a
   per-server setting.
3. **Clients re-bootstrap silently on next use, both directions.** An
   upgraded client holding a `token`-mode entry against a server that just
   flipped to `mtls` hits an auth-shaped TLS failure on its next request,
   silently re-runs the SSH `_bootstrap` exchange, learns the new mode from
   the returned bundle, enrolls a certificate, and retries — no manual
   `server add` needed. The same happens in reverse (`mtls` → `token`): the
   existing certificate-holding entry gets a `401`, re-bootstraps, and
   migrates back to a bearer token. Both directions were live-verified with
   zero manual steps beyond the server-side config flip.

A pre-mTLS (old, released) client against a server already in `mtls` mode
gets an explicit, actionable error rather than a confusing failure — the
bootstrap channel replies `this server requires auth.mode: mtls; upgrade
shed (client certificate support)` when it sees a request with no `csr=`
argument. There is no silent lockout: the message names exactly what to do.

## `shed server add` is now SSH-first, for every mode

`shed server add` no longer starts with an HTTP TOFU probe of `/api/info`
(that path survives only as the `open`-mode fallback). It goes SSH-first
for **every** mode:

1. Connect over SSH to `--ssh-port` (default `2222` — pass it explicitly if
   your server's `ssh_port` differs) and confirm the host key fingerprint,
   same TOFU-with-confirmation UX as before.
2. Run the `_bootstrap` exchange over that connection. The returned bundle's
   `auth_mode` decides what happens next: `token` writes a bearer token,
   `mtls` enrolls a certificate, and an `open`-mode server's bootstrap
   rejection (`bootstrap requires auth.ssh.mode: enforce`) triggers the
   fallback to the old HTTP TOFU probe.
3. Write the entry + credential, then verify with an authenticated
   `GET /api/info` over the resulting transport.

**Behavior change, stated plainly:** `shed server add` against a `token` or
`mtls` server now requires an **allowlisted SSH key at add time** — previously
a token-mode add would succeed even before the caller's key was on the
allowlist, since the token was only needed lazily on the first API call.
Now the SSH `_bootstrap` step itself needs an authorized key, so an add
against an enforced server with an unlisted key fails immediately with "your
SSH key is not in this server's allowlist" rather than appearing to succeed.

**Hand-pasting a `~/.shed/config.yaml` entry no longer works** against an
enforced server. Because the credential (token or certificate) is minted
per-client over an authenticated SSH exchange, there's no static value you
can copy into a config file and have it work — the entry must be produced by
a real `shed server add` (or a client's own silent re-bootstrap) against that
specific server. This was already effectively true for token mode's minted
tokens; mtls makes it unambiguous, since a hand-pasted entry with no
certificate/key on disk has nothing to present at the TLS handshake at all.

## Desktop users: upgrade `shed-host-agent` before (or with) the app

The desktop app and `shed-host-agent` are **separate release components**
with independent version selectors (`desktop/VERSION` vs.
`crates/shed-host-agent/VERSION` — see the root `CLAUDE.md` release model).
That independence matters here: **a new desktop app talking to an old
`shed-host-agent` cannot use mTLS at all.**

- The app-to-host-agent protocol gained a new `credential.get` message (the
  mode-agnostic successor to the old `token.get`) plus a capability
  advertisement in the agent's `hello_ack`. An old agent's `hello_ack` simply
  carries no `agent_capabilities` field, so a new app recognizes the gap
  immediately rather than sending a `credential.get` into a switch with no
  case for it.
- Concretely: point a **new** desktop app at an **mtls** server through an
  **old** `shed-host-agent`, and the app sees the old agent's `hello_ack`
  carrying no `agent_capabilities` at all, recognizes it can't ask for a
  certificate, and every request against that mtls entry fails with an
  explicit **"upgrade shed-host-agent"** error. It fires on every request
  against that entry, not just the first — there's no silent degrade to
  token-mode behavior, because an mtls server has no token to fall back to.
  A `token`-mode entry through an old agent is unaffected (the legacy
  `token.get` path still works). The mirror-image mismatch — an **old** app
  talking through a **new** agent against an mtls server — is handled too:
  the old app still sends the legacy `token.get`, and the agent, knowing the
  server requires a certificate the old app can't obtain, replies with its
  own explicit **"upgrade the app"** error rather than fabricating a
  bearer token that would never authenticate.
- **Fix:** upgrade `shed-host-agent` to a build with `credential.get` support
  before (or in the same sitting as) upgrading the desktop app, whenever you
  plan to add or use an mtls server. If you only use `token`-mode servers,
  the old agent keeps working with a new app with no action needed — this
  ordering only matters for mtls.
- The reverse mismatch (new agent, old app) degrades gracefully: an old app
  never sends `credential.get`, so a new agent simply never gets asked for a
  certificate on the app's behalf, and everything that worked before keeps
  working.

## The CA story: rotation is manual, and it's fleet-wide

Each mtls-mode server generates a small internal CA the first time it starts
in that mode (`ca_cert.pem` / `ca_key.pem`, next to the existing TLS
cert/host key). It signs client certificates only — it has nothing to do with
the server's own TLS identity, which clients still pin the same way as
`token` mode.

**Deleting the CA files and restarting the server invalidates every
previously-issued client certificate at once.** This is not a partial or
per-client operation — it is fleet-wide by construction, since every issued
certificate's signature no longer chains to anything the server trusts.

The recovery story is intentionally simple, and mostly self-driving:

- Every well-behaved client (CLI, host-agent, desktop, mobile) detects the
  resulting TLS-level auth failure on its next request, silently re-runs the
  SSH `_bootstrap` exchange, and enrolls a fresh certificate against the new
  CA — the same reactive-migration mechanism that handles ordinary renewal
  and mode flips (see [Upgrade order](#upgrade-order-clients-before-servers)
  above). So the operator-facing cost of a CA rotation is **"every client
  pays one extra SSH round-trip on its next command,"** not a coordinated
  maintenance window.
- There is **no `shed server ca rotate` / `ca status` CLI yet** — rotation
  today means deleting the two files and restarting, and recovery means
  "wait for each client's next command" (or manually trigger one, e.g. `shed
  -s <server> list`, for anything you want back online immediately). A full
  CLI-assisted rotation story is future work.
- The server helps you *not* get surprised by this: it logs the CA
  fingerprint and expiry at startup (same as the TLS fingerprint today) and
  **warns when the CA is within 90 days of expiring**, so a rotation is a
  planned event rather than an outage discovered in the field.

## Checklist

- [ ] Upgrade every client (CLI, host-agent, desktop, mobile) that talks to a
      server you plan to flip to `mtls`.
- [ ] Desktop users: confirm `shed-host-agent` is upgraded, not just the app.
- [ ] Flip `auth.mode: mtls` on the server(s) you want hardened; leave others
      on `token` if `curl`/CI/scripts need direct access.
- [ ] Existing client entries re-bootstrap on their own on next use — no
      manual `shed server add` re-run needed after the flip.
- [ ] Note the CA fingerprint/expiry from the server's startup log somewhere
      you'll see the 90-day warning.
