# Security

shed-server has two postures, selected by a single switch:

```yaml
auth:
  mode: open      # open (default, loopback only) | mtls
```

- **`open`** (the default) serves plain HTTP only, with no HTTP credential and
  no TLS, and **binds loopback only**: it is a local-development posture. A
  non-loopback `bind_address` in open mode fails config load. The SSH allowlist
  is the one tunable layer here (`auth.ssh.mode: off|warn`, where `warn` stages
  an allowlist before you enforce it).
- **`mtls`** is the posture for anything reachable from another host. It
  derives the whole hardening bundle (SSH allowlist enforced, a client
  certificate required on every request, TLS-only with no plain-HTTP listener)
  and **refuses to start** without an SSH key source, so a server is never
  half-hardened on a network address. The client credential is a short-lived
  certificate bound to a private key that never leaves the client's device,
  issued over SSH. See [mTLS mode](#mtls-mode).

| `auth.mode` | SSH allowlist | HTTP credential | Plain-HTTP listener | TLS | `bind_address` |
|-------------|---------------|-----------------|---------------------|-----|----------------|
| `open` | as configured (`auth.ssh`: `off`/`warn`) | none | served | none (`https_port` is refused) | loopback only |
| `mtls` | **forced `enforce`** (needs a key source) | client certificate, issued over SSH | **none: TLS-only** | **on, `RequireAndVerifyClientCert`** (`https_port` defaults to 8443) | loopback by default; any address you set |

Two invariants hold: **credential ⟺ TLS ⟺ mtls** and **https ⟺ mtls**. A
client can therefore treat an `https://` `api_url` as proof that the server
requires a credential.

The bearer-token mode (`auth.mode: token`, also spelled `secure`) and
`allow_insecure_exposure` were removed in v0.10.0 and now fail config load;
see [Removed in v0.10.0](#removed-in-v0100).

All server settings live in the server config (`/etc/shed/server.yaml` on
Linux, `~/.config/shed/server.yaml` on macOS). Client settings live per server
entry in `~/.shed/config.yaml`.

Beyond the posture, this page covers who a caller is and what they may reach:
[users and keys](#users-and-keys), [users from an account
service](#users-from-an-account-service), [ownership](#ownership) of sheds and
snapshots, the [credential bus](#credential-bus) and the one machine that
answers it for each user, [service clients](#service-clients) (orchestrators
limited to their own sheds), [mount selection](#mount-selection) (which host
directories a shed gets) and [network profiles](#network-profiles) (on
Firecracker, an `isolated` shed reaches the internet and, beyond it, only its
own ports on the gateway, any `network.allow_cidrs`, and what its egress proxy
dials for it).

## Users and keys

Identity comes from the SSH key a client authenticates with, GitHub-style. The
SSH username only selects the shed (or the reserved `_bootstrap` channel); it
never says who the caller is.

A server either has **no user list**, and then every accepted key is one
**implicit user**, an admin, exactly as before users existed, or it has a list
under [`auth.users`](configuration.md#users), and then every key belongs to
exactly one named user, or it takes its list from an account service
([`auth.directory`](configuration.md#users-from-an-account-service)), beside
local admins listed in `auth.users`.

| | No user list | `auth.users` | `auth.directory` |
|---|---|---|---|
| Key sources | `auth.ssh`: `github_users`, `authorized_keys`, `authorized_keys_file` | `auth.users.<name>.github` and `.keys`; `auth.ssh` key sources are refused beside it | the account service's list, and the local admins' `keys` in `auth.users`; no GitHub logins |
| Modes | `open` or `mtls` | `mtls` only | `mtls` only |
| Who a key is | the implicit user, an admin | the user it is listed under, with that user's `admin` flag | a local admin, or the list's user, with the list's `admin` flag |
| Sheds a caller reaches | every shed | their own; an admin also lists, stops and deletes others' (see [Ownership](#ownership)) | as with `auth.users` |
| Unknown key | refused in `enforce`, let in as the implicit user in `off` and `warn` | refused in every SSH mode | refused, unless a pull of the list it may start brings it (see [A new person](#a-new-person)) |

With a user list a user's key is a plain public key: a client that presents an
SSH certificate is refused, and a certificate listed as a key fails config
load. The [Multi-user server guide](../guides/multi-user-server.md) walks through
adding a user list.

### SSH key allowlist (no user list)

By default (open mode) any public key is accepted and is the implicit user.
`auth.ssh` restricts which keys may connect; `auth.mode: mtls` forces
`enforce`.

```yaml
auth:
  ssh:
    mode: enforce            # off | warn | enforce
    github_users: [alice-gh] # each login's keys, pinned to its GitHub account id
    authorized_keys:         # inline OpenSSH authorized_keys lines
      - ssh-ed25519 AAAA... laptop
    authorized_keys_file: ~/.shed/authorized_keys
    github_refresh: 1h       # re-fetch GitHub keys on this interval
    max_auth_tries: 10       # public-key attempts per connection (raise for many-key agents)
```

| Mode | Behavior |
|------|----------|
| `off` | Accept every key (the `open`-mode default). |
| `warn` | Log `SSH auth WOULD DENY (warn mode)` for a key not on the list, but still accept it. Open mode only; useful while building the list. |
| `enforce` | Reject keys not on the list. Forced in `mtls` mode, and valid only there. |

**`enforce` with no resolvable keys fails startup**: the server never starts
with an empty allowlist (which would lock everyone out) and never falls back
to accept-all. `github_refresh` and `max_auth_tries` also apply to a server
with a user list.

### GitHub logins are pinned to their account id

A GitHub login (`auth.ssh.github_users`, or `auth.users.<name>.github`) is
followed by its **numeric account id**, not its name: a login can be renamed,
deleted and registered again by someone else, and the id cannot. At the
login's first successful fetch the server records the id in
`{state_dir}/github_pins.json` and from then on accepts the login's keys only
from a fetch that shows the login still names that id.

- **What a fetch does.** It asks `api.github.com` for the login's account id
  (`GET /users/<login>`), compares it with the pin, and reads the keys of that
  id (`GET /user/<id>/keys`, every page). This happens at start, every
  `github_refresh` (default 1h), and on `shed users refresh`. `api.github.com`
  is an outbound dependency of a server that follows GitHub logins; the
  requests are unauthenticated and send the User-Agent `shed-server`.
- **The pin is written first.** It is written durably before any key from that
  fetch is accepted, and a failed pin write accepts nothing new.
- **A mismatch drops the login's keys.** A login that now names another
  account, or no account (a 404 for a pinned login), contributes no keys; they
  are logged and do not come back from memory or the key cache. To follow a
  re-created account on purpose, set `auth.users.<name>.github_id` (it wins
  over the stored pin), or remove the login's entry from `github_pins.json`.
- **GitHub unreachable or rate-limited.** The last known keys stay and no new
  key is accepted.
- **A damaged `github_pins.json` stops the server from starting.** The message
  names the file: restore it, or move it aside to pin every login again at its
  next fetch.
- **A new key on the account** is accepted at the next fetch and logged.
- **An unknown key.** In `enforce` mode, an unknown key offered for
  `_bootstrap` or for a validly formed shed name triggers at most one refetch a
  minute for the whole server; the login that takes that slot waits up to 5 s.
  The refetch reads `github.com/<login>.keys` only to notice a change, and
  accepts nothing from it; the REST API requests it leads to have their own
  budget of 20 an hour, so strangers cannot spend the scheduled refreshes'
  allowance. An expired key, a key dropped for a mismatch and the username
  `git` never trigger it. (A server whose users come from an account service
  has another rule: see [A new person](#a-new-person).)

The per-login key cache, `{state_dir}/github_keys/<login>.keys`, keeps its
name and format, and is trusted only when it matches the hash stored with the
pin. A login with no pin yet (at a server's first v0.10.0 start, its cache is
one an earlier release wrote) has no last known keys: the cache is not read,
and until a fetch succeeds the login contributes no key. A server whose only
keys are such logins then does not start (an `mtls` server always enforces its
keys). Let it reach `api.github.com` and start it again, or give it a key that
does not come from GitHub: with a user list (`auth.users`) a key under that
user's `keys`, without one `auth.ssh.authorized_keys` or `authorized_keys_file`
(the two cannot be combined).

### Expired keys and script keys

On a server with users, each key listed under `auth.users.<name>.keys` has a
`label` and may also have:

- **`expires`**: an RFC 3339 time, or a date (`2027-06-30`, which means the
  end of that day in UTC). From then on the key is refused at the SSH
  handshake, at the TLS handshake and on every API request. Channels and
  streams already open when it expires stay open. A server that starts with an
  expired key logs it.
- **`script: true`**: the key belongs to automation, not a person. It reaches
  the user's sheds like any of the user's keys, but it cannot take a
  `credentials` certificate at `_bootstrap` (`bootstrap: a script key cannot
  take the credentials scope`), subscribe to or answer on the credential bus
  (`403 FORBIDDEN`), or see or move the user's [credential
  source](#one-machine-answers-for-each-user) (`403 FORBIDDEN`, the read
  included).

### When a key is checked

The key's identity is resolved again for **every new SSH channel** (a session,
sftp, a forwarded port, a wake, `_bootstrap`) and, in mtls mode, on **every
API request**, against the live key set. A key that is removed from the
config or a GitHub account, expires, or loses its pin therefore opens no new
channel and makes no new request. Channels, SSE streams and Connect tunnels
that are already open stay open until they end.

### The SSH auth log

Every `SSH auth` line in the server log names the peer's address as
`remote=<host:port>`: right after `fingerprint=` on a line that names a key,
right after `user=` on the attempt-cap line, and right after `SSH auth failed:`
on the one line written per failed connection. `user=` is the username the
client asked for, quoted when it holds any character other than `a-z`, `0-9`,
`-` and `_`.

| Line | Written |
|------|---------|
| `SSH auth allowed: user=<name> fingerprint=<fp> remote=<host:port>` | Once per login, in every mode, when the client has proved it holds the key (its signature verified) and the connection authenticates. A key a client only offers is never logged as allowed. The end of the line describes the user the connection authenticated as (see below). |
| `SSH auth DENIED: user=<name> fingerprint=<fp> remote=<host:port>` | Once per refused key, followed by the reason when there is one (`(the key has expired)`, `(reserved username)`, ...). An agent that offers several keys can write several before one of its keys logs in. |
| `SSH auth attempt: ...` (`off`) and `SSH auth WOULD DENY (warn mode): ...` (`warn`) | When a key is offered. Neither claims a login. |
| `SSH auth: key <fp> remote=<host:port> is known after ...` | When fetching an unknown key's GitHub logins, or pulling the directory, made it known. |
| `SSH auth: public-key attempt cap (<n>) reached for user=<name> remote=<host:port> ...` | When a connection used up `max_auth_tries`. |
| `SSH auth failed: remote=<host:port> user=<name> keys=<n>` | Once per connection that offered at least one key and closed without proving one, in every mode: `user=` is the username of its last offer, `keys=` how many public-key offers the server examined on that connection (a key offered twice can count twice, and a repeat the SSH library answers from its cache of the last key counts once). A connection that logs in writes none, however many `DENIED` lines it drew first, and so does one that offered no key. This is the line to count failures by (see [Facing the internet](#facing-the-internet)). |

The `allowed` line, and the `DENIED` line for an expired key, end with what the
user lookup said about the key's user. On the `allowed` line that is the
lookup made when the key was proven, the one the connection is bound to, even
if the list has changed since:

| The key is | The line ends with |
|------------|--------------------|
| a named user's key | `key_user="<user>" admin=<true or false>`, then `label="<label>"` when the key has a label |
| a named user's script key | the same as any of that user's keys; the line does not say it is a script key |
| the key of the implicit user (a server with no user list) | `label="<label>"` when the key has a label, otherwise nothing |
| a key the lookup does not list (it logs in only in `off` or `warn` mode, on a server with no user list) | nothing |

## Users from an account service

With [`auth.directory`](configuration.md#users-from-an-account-service) a
server takes its users from an **account service**: a service that holds
people and their SSH login keys. People sign in to the service, and register
their keys there, by whatever means it offers; shed takes no part in that. The
server and the service speak the [directory contract](directory-contract.md),
and any implementation of the contract's service side works. The [User
Directory guide](../guides/user-directory.md) covers the operator's steps.

### How the server signs in and pulls

- **Enrolment, once.** `shed-server directory enroll` makes an Ed25519 key
  pair, sends the public key with a one-time code the service's operator
  issued, and keeps the private key in `{state_dir}/directory/enrolment.json`.
  The key never leaves the host. The code is read from standard input only,
  and is written to no file or log.
- **Sign-in.** OAuth 2.0 client credentials with a JWT client assertion
  signed by that key: a new assertion for every token request, none sent
  twice. The request asks for the scope `directory:read` and nothing else, so
  a scope the service adds later is never requested. The access token is kept
  in memory only.
- **The pull.** `GET <url>/v1/directory` with the access token returns the
  list: users, each a name, an admin flag and keys. A pull is due at start,
  `auth.directory.refresh` after each good pull, 15 minutes after a failed one
  (or at the end of the service's `Retry-After`, in seconds, when that is
  later, up to one hour), on `shed users refresh`, and on a first login
  (`_bootstrap`) or a login to an existing shed by a key the server does not
  know (see [A new person](#a-new-person)).
- **Transport.** HTTPS with the system's certificate checks; the environment's
  proxy settings apply. No redirect is followed. Plain HTTP only with
  `allow_plain_http`, and only to a loopback address (checked on the address
  actually dialled), with no proxy. Every answer is read up to the contract's
  size limits.
- **What a failed pull changes: nothing.** A refused login, a disabled
  server, a rate limit, an outage, a timeout, a redirect, another contract
  number or organization, or an answer that breaks the contract's rules
  leaves the list the server holds, and its kept copy, as they were.

### Which entries become users

Each entry of the list is decoded on its own, so one bad entry is skipped and
the rest stand. An entry is skipped when it breaks the contract's rules (rule
7 of the [contract](directory-contract.md)) or one of shed's:

- its name is not a valid user name (`^[a-z][a-z0-9-]{0,62}$`), or is listed
  twice;
- it has no key this server can use. A key is one plain OpenSSH public key
  (an SSH certificate is refused) with a label that is not empty and unique
  within the user. Its `expires`, when present, is an **RFC 3339 instant**
  (`2027-06-30T23:59:59Z`); a bare date, which `auth.users` accepts, is not
  accepted in the list, and a key whose `expires` does not read as an instant
  is not used;
- it lists a key that the list holds twice, under this entry or another
  (every entry that lists it is skipped);
- its name is a local admin's, or it lists a key a local admin holds, or its
  name is an `auth.clients` name.

A key's identity is its **blob**, not its text: the same key with another
comment is the same key. Every skipped entry is logged with its reason and
shown, by name, in `shed users status`. An entry with a local admin's name is
logged at every pull; the others once per version of the list. An empty list,
and a list whose every entry is skipped, leave only the local admins. A list
with no admin loads.

### Local admins

The `auth.users` entries beside `auth.directory` are the **local admins**: a
separate, local-only identity, always an admin, with keys from the config
only (no GitHub login, no script key). They log in with the service
unreachable, before enrolment and after the trust time; the trust time never
applies to them, while each key's own `expires` does. The service cannot
change them: a list entry with a local admin's name or key is skipped, and
the local admin keeps its name and keys whole. The server warns at start and
in `shed users status` when every local admin key has expired, since then no
local admin can log in. A person who is also a user in the list keeps the local
admin on a separate key: see [User Directory › Keeping the local admin on a
backup key](../guides/user-directory.md#keeping-the-local-admin-on-a-backup-key).

### A list admin controls the host

A list admin has an admin's powers on the server, host directories and server
mounts included, so **whoever controls the account service controls the
host**. Who is listed, and whether as an admin, is what the service sends
this server; it may differ from one enrolled server to another. Whoever can
add a key to a person there enters every server that lists that person, as
that person, and whoever can make a person an admin on a server gets that
server's host. Run the service with the care that implies; the local admins
are outside its reach.

### The kept copy and the trust time

- **Write, then publish.** A good pull's answer is written durably to
  `{state_dir}/directory/users.json` before its list is published, so the copy
  on disk is never older than the list in force. When it cannot be written
  the old copy is removed, so a restart cannot bring back someone the new list
  removed, and the new list is used in memory only (`shed users status`:
  `Kept copy: not-written`). If the old copy cannot be removed either, status
  shows `stale-on-disk` and the log warns at every pull: this is the one case
  in which a restart brings back the old list, for what remains of its trust
  time.
- **The trust time** is `auth.directory.trust` counted from when the good
  answer arrived, on the wall clock (a host that sleeps past it stops
  honouring the list when it wakes). From its end the server answers for the
  local admins only; within 30 seconds it also closes the listed users' SSH
  connections and logs `directory: TRUST ENDED: ...`, once. The end holds even
  if the clock moves back; only a good pull restores the list.
- **The end of the trust time is durable.** When it ends, the kept copy is
  removed, so a restart after it starts with only the local admins, whatever
  the clock says then. The removal happens at once when a login or request
  finds the end, within 30 seconds otherwise, and at a graceful stop of the
  server, checked as the stop begins and again as its last step, after the
  SSH server and the API have shut down (each check waited for up to 5
  seconds); a pull that ends after the stop began writes no copy. A removal
  that fails is tried again at each turn of the schedule, and status shows
  `stale-on-disk` meanwhile. What remains: a server that dies without a graceful stop in the
  moment between the end of the trust time and the removal of the copy,
  followed by a clock set back and a restart while the service cannot be
  reached, brings the list back for what remains of its trust time on that
  clock. An accurate clock is a prerequisite.
- **At start** the kept copy is used only when it passes the file checks
  below, names the current enrolment's issuer and client id and an accepted
  organization, was fetched no later than the current wall clock, passes
  today's rules, and is inside its trust time counted with today's
  `auth.directory.trust`. Otherwise only the local admins can log in until the
  first good pull, which starts at once. The listeners do not wait for it.
- **The trust time bounds only how long a server keeps honouring a list it
  cannot refresh.** It does not bound a compromised service that keeps
  answering.
- **An accurate host clock is a prerequisite.** A clock set back before the
  trust time ends extends it. The server warns when an answer's `Date` is more
  than 30 seconds from its own clock.
- **Backups.** Restoring an old state directory from a backup restores its
  users for what remains of that copy's trust time.

### The state directory is a security boundary

`{state_dir}/directory/` holds this server's private key for the service and
the kept copy, which is not signed: whoever can write it can add users (a
forged copy) or replace the server's key. Its integrity is a security boundary
beside the CA key (`ca_key.pem`, which can issue client certificates). The
server checks before every read: the directory is a directory owned by the
server's user with mode `0700`, not a symbolic link; each file is a regular
file owned by that user with no group or other permission bits, not a
symbolic link, within its size limit (16 KiB for `enrolment.json`, 12 MiB for
`users.json`), and decodes strictly. An `enrolment.json` that fails a check
makes the enrolment unusable: nothing is sent and only the local admins can
log in. A `users.json` that fails one is not used.

**The enrolment is read before every pull.** A first enrolment needs no
restart. An enrolment that is missing, unusable, or names another issuer or
organization than the config's, found while the server runs, **withdraws the
list at once**: only the local admins can log in, and the listed users' SSH
connections are closed.

### When the list changes

- **One snapshot per decision.** A request or a new SSH channel is authorized
  against one snapshot of the users at admission; the list never changes
  halfway through one decision. Work already admitted may continue, except as
  follows.
- **A person removed** from the list (or every listed person, when the trust
  time ends or the list is withdrawn): when the new list is published, every
  SSH connection of a user no longer in it is closed, with all its channels,
  and the server logs `SSH: closed <n> connection(s) of user "<name>": no
  longer a user of this server`. Their next SSH login and API request are
  refused.
- **Not tracked:** open API streams (create and pull progress, the egress
  audit stream) and Connect tunnels end by themselves; their next request is
  refused.
- **The credential bus.** A subscription stream checks its certificate again
  before each message, and ends when the certificate no longer authenticates
  its user; the message is not sent. A credential-bus request already
  delivered is not recalled.
- **An admin flag change** applies at the next SSH channel and the next
  request, with the same certificate.
- **A key that moves** from one person to another: channels already open on
  the first person's connections stay open, a new channel on those
  connections is refused, a new connection is the second person's, and a
  certificate bound to the first person is refused.
- **Their sheds and snapshots** keep their user; an admin sees them and may
  stop or delete them.
- **A change of issuer or organization** is a change of trust domain: the
  same names may then mean other people. Treat it as a new server, with a
  fresh state directory.

### A new person

A key the list the server holds does not have (a person added on the service
since the last pull, or a key they added there) can make the server pull the
list early. What it can start depends on the login name it is offered under:

| Login name | What happens |
|------------|--------------|
| `_bootstrap` (`shed server add`, a refetch) | The server pulls the list, at most **once a minute** while pulls succeed and **once per 15 minutes** while they fail, never inside a pending `Retry-After`, and only when enrolled. Only the login that takes that slot waits: it is let in as soon as its key is known (brought by its own pull or by any other: an admin's refresh, the schedule, a shed name's pull), and if its key is still unknown it is refused when its **8-second** wait ends, never earlier, however soon the pull ended. Every other unknown key at that moment is refused at once, so a refused `shed server add` takes 8 seconds only when its login took the slot. |
| The name of a shed that exists on this server | Refused at once. Off the login path the server checks that the shed exists, and only then pulls the list in the background, on a slot of its own: at most **once every 5 minutes** while pulls succeed, once per 15 minutes while they fail, never inside a pending `Retry-After`, only when enrolled. No login waits for it. A retry a few seconds later is let in once the pull has brought the key. |
| Any other name (a name no shed has, or one that cannot be a shed name) | Refused at once. No pull. |

If the account service answers more slowly than the wait, a new person's
logins are refused until a pull has brought their key; the pull carries on,
its result applies, and a later attempt succeeds. An admin's `shed users
refresh` before the person's first login avoids the wait altogether, and is
not limited by either slot. A local admin's login never waits for a pull.

**What a peer can learn about which shed names exist.** A login under a shed
name is refused without waiting for the server's check of the name, so that
answer does not depend on whether the shed exists. A pull started for a shed
name can delay a later `_bootstrap` pull that queues behind it, but a
`_bootstrap` login whose key stays unknown is answered at its fixed 8-second
deadline, so that delay does not show. For a peer whose key stays unknown, then,
the time of each answer does not depend on whether a guessed login name is an
existing shed, except in three cases, in which whether a shed-name pull was
sent changes when the `_bootstrap` slot may next be taken (a slot it may take
waits 8 seconds; one it may not is refused at once), so a peer probing at that
moment can learn whether a guessed name is a shed, at most one guess per
`_bootstrap` slot:

- while pulls fail;
- while a `Retry-After` is pending;
- when pulls had been succeeding and the shed-name pull itself fails: the
  server then enters the failed state, and a `_bootstrap` login that would have
  taken the slot (a minute after the last take) and waited 8 seconds is refused
  at once instead.

A person whose key is listed on the service learns something else, which is
accepted: their own first login is let in when a pull brings their key, so a
pull that was already running ahead of theirs delays that login, which can
show them that some pull was in flight.

At most one check of a shed name runs at a time, as the server itself, within
2 seconds; an offer that arrives while one runs is refused with no check, and a
check that takes longer asks for no pull.

The server logs each such pull: `directory: unknown key <fp>: pulling the
list (at most once a minute while pulls succeed, once every 15 minutes while
they fail)` for `_bootstrap`, and `directory: shed-name key <fp> for an
existing shed: pulling the list in the background (at most once every 5
minutes while pulls succeed, once every 15 minutes while they fail)` for a
shed name. Once an hour, when there was any, it logs how many unknown keys
were offered under a shed name: `directory: in the last hour <n> unknown keys
were offered as a name that is no shed here and <m> as an existing shed's name
(<p> asked for a pull, <q> skipped while a check ran, <e> checks failed:
<first error>)`. `<p>` counts the offers that asked for a pull; such a pull
may join one already asked for, or not be sent at all (a `Retry-After`, or the
failed state's rate, when it would start).

The server holds **one outstanding `Retry-After`** for itself: neither the
schedule nor an unknown key pulls before it ends. Only an admin's `shed users
refresh` may pull before it.

A server whose users come from `auth.users` with GitHub logins keeps its own
rule (see [GitHub logins are pinned to their account
id](#github-logins-are-pinned-to-their-account-id)): an unknown key under
`_bootstrap` or any valid shed name may start a GitHub fetch.

### Certificates

A client certificate issued before certificates named their user (by a server
older than v0.10.0) is refused here as on every server (see [The certificate is
the credential](#the-certificate-is-the-credential)). Certificates that name
their user are checked against the list on every request, as on any server
with users.

## Ownership

Every shed and snapshot records a `user`: the person it belongs to, or none.
Separately, a shed a [service client](#service-clients) creates records that
client as its `owner`. A shed has a user or an owner, never both.

On a server with **no user list** every caller is the implicit user and may do
everything to every shed, whatever its record says. The rest of this section
applies to a server with `auth.users`.

### Who may do what

| Caller | Their own shed | Another user's shed | A service client's shed | A shed with no user |
|---|---|---|---|---|
| A user | everything | invisible: `404` | invisible: `404` | invisible: `404` |
| An admin | everything | list, view, stop, delete; **not** enter | everything | list, view, stop, delete; **not** enter |
| A service client | n/a | invisible: `404` | its own only | invisible: `404` |

**Enter** means start, exec, a shell, sftp, a forwarded port or tunnel,
sessions, reset, egress set and off, and a snapshot of the shed. An admin who
may view but not enter a shed gets `403 FORBIDDEN` with a message saying so,
and over SSH the line `shed '<name>' belongs to another user; an admin can
list, stop and delete it but not enter it`. A record an admin views this way
has no `repo` and no `landing_dir`, since a clone URL can carry a token.

"An admin cannot enter" guards against a stolen admin credential, not against
the admin, who controls the host directories, server mounts and images every
shed runs on.

**Invisible** means the same answer as a shed that does not exist: `404
SHED_NOT_FOUND` on the API and `Error: shed '<name>' not found` over SSH, so
nobody learns which names other users hold. A list holds only the sheds the
caller may view. An SSH connection to a shed the caller may not enter does not
start it.

**Names are unique per server, across users.** Creating a shed under a name
someone else holds answers the usual `409 SHED_ALREADY_EXISTS`, worded the same
whoever holds it.

**Snapshots** belong to their user. A snapshot records the user of the shed it
was taken from, and a shed created from a snapshot belongs to whoever creates
it. A user sees and uses only their own snapshots (another's answers `404
SNAPSHOT_NOT_FOUND`). An admin may list, view and delete any snapshot and
create a shed from one with no user, but not from another user's. A service
client reaches no snapshot.

### Admin-only resources

What the host shares belongs to admins. A user who is not an admin may not:

| Resource | What happens |
|---|---|
| Host directories (`local_dir`, `add_dirs`; `--local-dir`, `--add-dir`) | `403 FORBIDDEN` (`admin only: local_dir`) |
| Server mounts (a named `mounts` entry) | `403 FORBIDDEN`; a create that omits `mounts` gets none |
| An image other than the server default or an `image_aliases` name | `403 FORBIDDEN` |
| The image store (tag, pull, push, delete, prune) and `system prune` | `403 FORBIDDEN` |
| Egress profile changes | `403 FORBIDDEN` |
| `env_file` | not refused: the user's `repo` clone simply never gets the variables |

A user who is not an admin also may not **enter or start** a shed that carries
host directories or server mounts, even their own (one made while they were an
admin): they may still stop and delete it. On Firecracker and VZ a shed stored
before mount selection existed counts as carrying server mounts.

`GET /api/system/df` shows a user who is not an admin only their own sheds and
snapshots and no orphans; the totals are the server's.

### Sheds with no user

Sheds and snapshots created before the server had a user list have no user.
On the first start with a user list, a server with **exactly one** user gives
them all to that user (a shed with host directories or server mounts only if
that user is an admin) and logs each. With several users nothing is given
away: only an admin sees such a shed, nobody enters it, and an admin gives it
to a user with [`shed assign`](cli.md#shed-assign). A marker,
`{state_dir}/users-upgrade.json`, records that this one-time step ran, or that
it began for one user and has not finished. While it has not finished, the
server **does not start** with any other users in `auth.users` (beside
`auth.directory`, the local admins; the account service's list never counts):
it stops before it opens anything or gives anything away, and says how to
finish the step or abandon it on purpose with [`shed-server serve
--abandon-user-upgrade`](cli.md#shed-server-serve). The
[Multi-user server guide](../guides/multi-user-server.md#sheds-with-no-user)
has the details.

### The reserved name `git`

A shed's SSH username on shed-server is its name, and a user's machine signs
SSH logins for the username `git` by default so that git over SSH keeps
working. So `git` cannot name a shed:

- Creating a shed named `git` answers `400 INVALID_SHED_NAME` (`shed name
  "git" is reserved`), on every backend and from a snapshot too.
- An SSH login as `git` is refused, in every SSH mode, before any key lookup.
- A shed named `git` made before this can still be listed, stopped,
  snapshotted and deleted over the API; SSH to it is refused, and the server
  warns at start. Snapshot it and recreate it under another name, or delete it.

### Shed names on the wire

Apart from the reserved `_bootstrap` enrollment channel, an SSH username must
be a valid shed name (lowercase letters, digits and hyphens, starting with a
letter); anything else, an empty username or a differently cased name such as
`Git` included, gets the unknown-shed answer. Every channel of a
connection (a session, sftp, a port forward) is routed by the username the
connection authenticated with. The same holds on the API: a route under
`/api/sheds/{name}` or `/api/egress/{name}` whose name is not a valid shed name
answers `404 SHED_NOT_FOUND` before anything else is checked, so a
case-insensitive macOS volume cannot be used to reach a shed under a second
spelling.

## mTLS mode

`auth.mode: mtls` is the posture for anything reachable from another host.
It forces the SSH allowlist to `enforce`, requires an SSH key source at
startup (`auth.users`, or one of `auth.ssh`'s), serves TLS only with
`https_port` defaulting to `8443`, needs no acknowledgment to bind the
network, and authenticates every request with a **client certificate bound
to a private key that never leaves the client's device**.

```yaml
auth:
  mode: mtls
  ssh:
    github_users: [alice-gh]   # a key source is required (or auth.users)
```

### Enrollment over SSH

There is no `shed-server cert new` and no static credential list. A client
that already holds an allowed SSH key enrolls over a reserved **`_bootstrap`**
SSH channel:

```bash
shed server add shed.example.com
#  → pins the SSH host key, then SSHes as _bootstrap@host with your key,
#    sends a CSR, and writes the issued certificate under ~/.shed/creds/.
```

The server re-checks the key for that channel, signs the CSR with a small
internal CA and returns a bundle the CLI persists:

```json
{ "auth_mode": "mtls", "https_port": 8443, "tls_cert_fingerprint": "sha256:…",
  "client_cert": "-----BEGIN CERTIFICATE-----\n…", "scope": "control",
  "cert_serial": "…", "expires_at": "2026-06-15T00:00:00Z" }
```

| Scope | Grants |
|-------|--------|
| `control` | The control plane (lifecycle, images, sessions, snapshots, users, credential sources), the Connect tunnel for `shed forward`, and the egress audit stream. |
| `credentials` | The credential bus (`/api/plugins/*`), plus the Connect tunnel and the egress audit stream (used by the desktop app's broker and egress subscriber). |

The bus requires `credentials`, so a leaked `control` certificate cannot reach
the live-secret bus, and a `credentials` certificate cannot manage the fleet
or move a credential source.

An open server answers every `_bootstrap` request with `bootstrap requires
auth.ssh.mode: enforce`; the CLI then adds it over plain HTTP.

There is no static credential to copy between machines. A certificate is
issued to one client over SSH and its private key stays with that client, so a
`~/.shed/config.yaml` entry written by hand cannot reach an mtls server: the
entry comes from `shed server add`, or from a client re-enrolling on its own.
`shed server add` therefore needs a key the server accepts at add time. When
the server refuses every key ssh offers, the add fails with `the server refused
every SSH key ssh offered for _bootstrap@<host>`, followed by the causes ssh
cannot tell apart: no offered key is a user's key on that server (wherever the
server takes its users from: its config, GitHub, or an account service), the
right key has a passphrase and is not in your agent (the CLI runs ssh with
`BatchMode=yes`, so `ssh-add` it), or the server's user list is not in force
and only its local admins can log in. A key listed moments ago can be refused
once while the server fetches the list: try again a minute later. For an entry
with its own key (`--identity-file`) the message names that key. See [CLI ›
shed server add](cli.md#shed-server-add).

**Clients older than v0.8.1** have no way to obtain a certificate. A CLI that
sends no CSR is answered `this server requires auth.mode: mtls; upgrade shed
(client certificate support)`. A desktop app older than that cannot obtain one
either: upgrade the app.

### Short TTL, transparent renewal

Certificates are short-lived: `auth.token_ttl`, default **24h** (the name is
kept from the removed token mode). Every client renews transparently, so the
TTL is invisible in normal use:

- **CLI**: re-enrolls when the stored certificate is near expiry, and on an
  auth failure retries the request once after re-enrolling. The expiry is kept
  in the client config as `client_cert_expires_at`.
- **desktop app's broker**: enrolls its **own** `credentials` certificate over the same
  SSH channel with its identity key.
- **background tunnels** (`shed tunnels start -d`): the detached daemon
  re-enrolls the same way, **in memory only**, so a multi-day daemon never
  rewrites `~/.shed/config.yaml`. It re-enrolls non-interactively (SSH
  `BatchMode`), so it needs SSH access without a prompt.

### What it guarantees, precisely

The server's HTTPS listener runs `RequireAndVerifyClientCert` against a small
internal CA (`ca_cert.pem` / `ca_key.pem`, generated on first mtls startup and
persisted next to the SSH host key and TLS cert). The precise claim:

- **An unauthenticated peer can never send an HTTP byte or reach the
  router.** The TLS handshake itself enforces the certificate: Go's
  `net/http` never invokes a handler until `RequireAndVerifyClientCert`
  passes, so there is no code path from "no cert" to a parsed request. mtls
  mode has no unauthenticated routes, `/api/info` included.
- **The listener and the TLS `CertificateRequest` remain observable to a
  scanner.** A port scan still finds the port open, and `openssl s_client`
  still completes a TCP connection and sees the server ask for a client
  certificate. The guarantee is about what happens after the handshake
  starts, not whether the port exists.

`curl -k` with no client certificate against an mtls server gets
`http_code=000` and curl exit 56: no HTTP status is ever obtained. `openssl
s_client` shows the server send `Acceptable client certificate CA names` and
then terminate with `tlsv13 alert certificate required`. With an enrolled
certificate the same request returns `200`.

### The certificate is the credential

The `Authorization` header is never read: a bearer token can neither stand in
for a certificate nor widen its scope. The certificate's subject carries the
principal, composed entirely server-side from authenticated knowledge, so a
client cannot request an identity it doesn't hold:

- **CN** = the SSH key fingerprint (`SHA256:...`).
- **OU** = scope (`control` or `credentials`), one scope per certificate.
- **O** = client kind (`cli`, `broker`, `desktop`, `mobile`).
- **userId** (RFC 4519 `uid`) = the user the key belonged to at enrolment
  (`!implicit` for the implicit user of a server with no user list).
- TTL = `auth.token_ttl` (default 24h), extended-key-usage `ClientAuth` only.

The server ignores every requested subject field, SAN or extension in the
CSR. A request acts as whoever the CN's key belongs to **now**, and only while
that is still the user the certificate names. A user's certificate with no
userId (one a server older than v0.10.0 issued) is refused on every server: at
the TLS handshake on a new connection, and with `401` on a connection already
open. A 0.9.x CLI or desktop app enrolls again by itself on that failure, and
so does the mobile app, which builds on the same core as the desktop app; if
one does not, run `shed server update NAME --refetch`. A service
client's certificate names no user either, but is told apart by its shape (see
below) and is not affected.

This describes a user's certificate. A [service client](#service-clients)'s
certificate has a different shape (CN = the client name, OU = `service`, a
`shed-client://` URI SAN), is signed by `shed-server client issue` on the shed
host, and is checked against `auth.clients` rather than the key set.

### Per-request re-validation

A TLS handshake happens once per connection, but a pooled keep-alive
connection can carry many requests, so the mtls middleware **re-validates
every request**: the certificate's `NotBefore`/`NotAfter` against the current
time, the CN against the *live* key set (and, with a user list, the key's
expiry and its user's current admin flag), and the OU against the route's
required scope. A certificate that fails is answered `401` and its connection
is closed, so the client's retry dials afresh; a scope failure is `403` and
keeps the connection.

### Revocation

Revoking a client certificate means **removing its SSH key** (from
`auth.users`, `auth.ssh`, or the GitHub account). The next request on any
connection presenting that certificate is refused by the per-request check,
and a new connection is refused at the handshake. Removing the key also cuts
the key's SSH access: one lever covers both. A key with an `expires` date
revokes itself.

A service client's certificates are revoked differently: remove the client
from `auth.clients` and restart `serve` (see [Service
clients](#service-clients)).

### Accepted limitations

- **Already-established streams survive revocation until they close.** An SSE
  subscription or a `shed forward` tunnel opened before a certificate is
  revoked or expires keeps running until it ends. Revocation and expiry bind
  on the *next* request or dial, not on an already-hijacked byte-copy loop.
- **`curl` and ad-hoc scripts need a client certificate.** There is no bearer
  header to hand a script. A script that acts as a person can use a
  certificate enrolled with that person's key (a [`script:
  true`](#expired-keys-and-script-keys) key keeps it off the credential bus);
  an orchestrator that only needs its own sheds can be a [service
  client](#service-clients), whose bundle works with `curl --cert`.
- **CA rotation is manual.** Deleting `ca_cert.pem` + `ca_key.pem` and
  restarting the server invalidates every previously issued client
  certificate at once: a fleet-wide re-enrollment. Every well-behaved client
  recovers automatically (one silent SSH round-trip on its next command). A
  [service client](#service-clients) is the exception: its certificate comes
  from `shed-server client issue`, so after a rotation it is refused until you
  issue and deliver a new bundle. There is no `shed server ca rotate` command
  yet. The server logs the CA fingerprint and expiry at startup and **warns
  when the CA is within 90 days of expiry**; `/api/info` carries
  `ca_fingerprint` and `ca_not_after` (see [API › GET
  /api/info](api.md#get-apiinfo)).

## Native pinned TLS

shed uses **pinned self-signed TLS** for the *server's* identity: no public
CA, no domain, no ACME. The server generates a self-signed certificate on
first start (the same lifecycle as the SSH host key) and clients pin it by the
SHA-256 fingerprint of its DER encoding, the trust model SSH host keys use.
`auth.mode: mtls` turns it on (`https_port` defaults to `8443`).

This is a **separate mechanism from the small internal CA** mtls mode uses to
sign *client* certificates: the server's own leaf and how clients pin it do
not depend on that CA.

```yaml
https_port: 8443                 # the pinned-TLS listener (mtls serves HTTPS only)
tls_names:                       # extra SANs so hostname verification passes
  - shed.example.com
  - 203.0.113.10
# localhost, 127.0.0.1, ::1 are always included.
```

`shed server add` pins the fingerprint automatically (it is in the bootstrap
bundle). All clients verify by pin: the Go CLI and sdk via
`VerifyPeerCertificate`, and `curl` via `--pinnedpubkey` alongside the cert
handed in with `--cacert` (with OpenSSL, `--cacert` alone can leave the system
CA directory trusted; see the [orchestrated sheds
guide](../guides/orchestrated-sheds.md#4-call-the-api)). A client that
configures a pin but a non-`https` URL **fails closed** rather than sending
plaintext.

**Rotation.** Changing `tls_names` regenerates the certificate (new
fingerprint). Re-pin clients with:

```bash
shed server update my-server --tls-fingerprint sha256:<new>   # pin out-of-band
shed server update my-server --refetch                        # fetch + re-pin over SSH
```

Rotating an existing pin in a non-interactive session requires
`--trust-on-first-use`, so a re-pin is never silently accepted.

## Network surface

There is **one HTTP listener per mode** (SSH always listens separately on
`ssh_port`): a single plain-HTTP listener on loopback in open mode, or a
single pinned-TLS (`https_port`) listener in mtls mode, with **no plain-HTTP
listener at all**. The credential bus (`/api/plugins/*`) and the Connect
tunnel (`/api/sheds/*/connect/*`) ride that same listener; in mtls mode the
bus needs the `credentials` scope while the Connect tunnel accepts `control`
or `credentials`, and both travel over TLS. The desktop app's broker reaches
the bus over `https://` with the pinned cert and its own
`credentials` certificate. These knobs shape what is reachable where:

| Field | Effect |
|-------|--------|
| `bind_address` | Interface every listener (plain HTTP, HTTPS, SSH) binds to. **Defaults to loopback (`127.0.0.1`).** In mtls mode set a specific IP, `0.0.0.0`/`*` (all IPv4), or `::` (all interfaces) to face the network. An open server is loopback only: any other value fails config load. |
| `https_port` | The HTTPS listener (bound to `bind_address`) serving the control plane, credential bus, and Connect tunnel over pinned TLS. **Requires `mtls` mode**; defaults to `8443` there. |
| `trusted_proxy` | Trust `X-Forwarded-For` (only safe behind a proxy that overwrites it). Default false uses the real TCP peer, so a source IP can't be forged to evade per-IP controls. |

### Facing the internet

The SSH listener accepts public-key auth only, with at most
`auth.ssh.max_auth_tries` keys tried per connection. It also limits
connections that have not authenticated yet, in every auth mode
(`off`, `warn`, `enforce`):

| Limit | Value | What happens |
|-------|-------|--------------|
| Login grace | 120 seconds | A connection that has not authenticated 120 seconds after it was accepted is closed. A connection that authenticated is never closed by it, however long it stays idle. |
| Unauthenticated connections | 100 at once | A new connection while 100 are unauthenticated is closed as soon as it is accepted. |
| Per address | 10 at once | A new connection from an address that holds 10 unauthenticated connections is closed as soon as it is accepted, while other addresses still get in. An address is an IPv4 address, or an IPv6 `/64`. |

A connection leaves both counts once, when it authenticates or when it
closes. The values are fixed. Loopback has no exemption: a proxy or a tunnel
on the same host that forwards to `127.0.0.1` would otherwise get past every
limit, so a proxy in front of the SSH port makes every connection one
address, and the per-address limit then applies to all of them together.

The limits write these lines:

| Line | Written |
|------|---------|
| `SSH: closed a connection from <host:port> that did not authenticate within 120 seconds` | When the login grace closes a connection. |
| `SSH: refusing connections from <address>: <n> are not yet authenticated (at most 10 from one address, 100 in all)` | When an address's share is full, at most once a minute per address. |
| `SSH: refusing new connections: <n> are not yet authenticated; the most from one address: <address> (<n>)` | When the limit of 100 is full, at most once a minute. |
| `SSH auth failed: remote=<host:port> user=<name> keys=<n>` | Once per connection that offered a key and closed without proving one ([The SSH auth log](#the-ssh-auth-log)). |

A connection is closed (and its place given back) before its line is
written, so a slow log never keeps a connection open.

Count failures by `SSH auth failed`, the grace line and the per-address
refusal line, never by `SSH auth DENIED`: that line is written once per
refused key, so a person whose agent offers several keys draws several before
one of them logs in. A connection that offered a key and then stalled past
the login grace writes two of these lines (the grace line and its
`SSH auth failed` line), so a count of lines is not a count of connections.
The line for the limit of 100 names only the address holding the most, not
the address refused, so it is not one to count failures by. [Public VPS
Deployment › Hardening the SSH
surface](../guides/vps-deployment.md#hardening-the-ssh-surface) has a fail2ban
filter.

**When the limit refuses everyone.** A peer that holds 100 unauthenticated
connections from many addresses keeps everyone else out until the login grace
closes them. The way back: the line for the limit of 100 names the address
holding the most; block it at the host's firewall (fail2ban's example filter
bans addresses from the per-address refusal and grace lines, not from that
line), then restart `shed-server serve`, which drops every connection it
holds. That ends one wave, not a flood: a peer with more addresses than you
have blocked fills the limit again after the restart. Against a flood from
many addresses, filter or rate-limit the SSH port before it reaches the
server (the firewall, or your provider's network controls).

**What an unknown key can start.** On a server whose users come from an
account service, a key the server does not know can make it pull the list
early, and nothing else: under `_bootstrap`, at most once a minute (the
newcomer's slot); under the name of a shed that exists on this server, at most
once every 5 minutes, in the background; while pulls fail, each at most once
every 15 minutes, and neither inside a pending `Retry-After`. Under any other
name it starts nothing, so scanners trying `root`, `admin` or `ubuntu` cost
the account service nothing unless a shed has that name, and at most one
check of a shed name (a read of the server's own records, as the server, within
2 seconds) runs at a time. A peer that knows `_bootstrap` can take the
newcomer's slot, up to 60 pulls an hour; one that names a real shed adds at
most 12. A newcomer refused because the slot was taken tries again a minute
later, or an admin runs `shed users refresh`, which neither slot limits. A
server whose users come from `auth.users` keeps its own rule for GitHub
logins. See [A new person](#a-new-person), which also states what a peer can
learn about which shed names exist.

## Connection flow: what's encrypted where

| Hop | open mode (loopback) | mtls mode |
|-----|----------------------|-----------|
| Control-plane / bus HTTP | plain `http://` on loopback | pinned `https://`; the client presents a certificate at the handshake, and a peer with none never completes it |
| SSH (shed sessions + the `_bootstrap` issuance channel) | encrypted; host key pinned in `~/.shed/known_hosts` | same |
| Credential issuance | none | client certificate over the pinned-host-key SSH `_bootstrap` channel (CSR in, signed cert out) |

In mtls mode **nothing plaintext faces the network**, and the only
trust-on-first-use moment is `shed server add` showing you the SSH host key
and TLS fingerprints for confirmation (or pass `--fingerprint` /
`--tls-fingerprint` to verify out-of-band). After that, every byte travels
over pinned TLS or pinned-host-key SSH.

## Credential delivery

[Credential delivery](delivery.md) puts a person's credentials into their shed when it is created and when they run `shed provision`: an SSH key, a GitHub token, API keys, an envsecrets login, named files. This is a different trade from the credential bus below, and it is stated plainly here.

- **A usable credential is inside the shed while it runs.** Anything in the shed can read it: the shed user, every process running as it, and the coding agents in the image. The bus decides per use, on the person's own machine; a delivered credential has no per-use check once it is in the shed. Deliver only what you would let that shed use.
- **The server sees values in transit.** An answer passes through shed-server on its way to the shed's agent. The server holds it in memory for the create or provision, never stores it, and never logs or returns it, but a compromised server can see it. A compromised server can also ask a connected source for a bundle for a shed that does not exist, and the source answers without a prompt. What limits this: only the user's **bound** machine is asked, the source's `only` rules (server, backend, repository) decide what it sends, and the person chose which servers to pin.
- **The files are plain files** (`0600`, or `0400` where asked) in the shed's home or project directory. They are in snapshots and on a stopped cloud shed's disk.
- **A shed assigned to a person may hold what someone else put there.** `shed assign` gives an unassigned shed to a user; a provision of such a shed writes the new user's credentials into a filesystem they did not build. Assigning is rare and admin-only.
- **Who may cause a delivery.** Only the shed's own user. An admin cannot provision another user's shed and sees its delivery state alone. A service client, a script key and a snapshot restore get none.
- **The envsecrets login** lets a shed read, and with the owner's current bucket key also change, every project's secrets. Prefer a read-only key.
- **The audit log is on by default in `mtls` mode** and records delivery and lifecycle events (states, reasons, the source machine, item names, results and fingerprints, never a value or a target). `audit.enabled: false` turns it off.
- **The writer is bounded.** The agent writes without following a symbolic link at any step, refuses a directory on another file system than the home, and the server trusts none of its output beyond a closed list of results. A hostile shed can misreport its own delivery and nothing more.

## Credential bus

The credential bus carries live secrets (SSH signatures, AWS and Docker
credentials) between a shed and a machine's desktop app, whose broker
subscribes to it with a `credentials` certificate. What the machine will sign
is decided on the machine: see the [SSH agent](../extensions/ssh-agent.md)
extension.

### Routed by user

A shed's requests go to its **user's** machine only, and an answer is accepted
only from that user. The shed's user comes from the server's own record of the
shed, never from the guest. Subscribe, respond, `GET /api/plugins/listeners`,
`GET /api/plugins/sheds` and the egress audit stream all act as the caller's
user; an admin sees every user's rows in the two listings but still cannot
subscribe or answer for another user. On a server with no user list there is
one user, so one machine answers every shed.

- The server mints its own id for each request it forwards and maps it back
  on the answer, so an id a guest chose never identifies anything on the
  server. An answer that matches no request it holds (forged, replayed,
  already answered, another user's, for a connection that is gone, or from
  before a restart) is refused with `403 FORBIDDEN`, in every auth mode.
- An answer is written only into the shed connection its request arrived on,
  while that connection still routes to the same user: a shed deleted and
  created again under the name, or a transport that reconnected, is never
  sent it.
- A request whose user has no machine subscribed for its type is refused at
  once; nothing is kept for a machine that connects later.

### One machine answers for each user

Each user has one **credential source** per server: the machine whose
subscriptions the server takes and whose answers it accepts. The **first
machine to connect for a user becomes it.** Any other machine of that user is
refused, told which machine is the source, and keeps retrying on its usual
backoff; it never takes over by itself. A move is explicit, with [`shed source
use`](cli.md#shed-source) or the desktop app's "Use this machine".

A machine names itself on every subscribe and respond with two headers:

| Header | Value |
|---|---|
| `X-Shed-Machine-Id` | 32 lowercase hex digits, the machine's stable id |
| `X-Shed-Machine-Name` | its display name, percent-encoded UTF-8; 1 to 63 bytes decoded, no control characters |

The desktop app 0.10.0 or newer sends them. A broker
released earlier sends neither and is refused, with a message ending "install the current
shed desktop app", until it is upgraded, so
upgrade machines before servers.

- **Who may move it.** A user moves their own source from any of their keys
  that is not a script key. An admin may name another configured user
  (`--user`) and choose among that user's own machines. A machine is known to
  the server only after its broker has tried to connect.
- **What a move does.** It takes effect at once: the old machine's streams
  end, its requests in flight are dropped, and its answers are refused
  (`SOURCE_NOT_BOUND`). The new machine starts answering when its broker next
  connects (within about 30 s, or at once from the desktop app's button).
  Nothing fails over: if the source is off or asleep, its user's credential
  requests get no answer until it reconnects or the source is moved.
- **A request in flight** belongs to the machine it was delivered to: a
  reconnect of that machine gets it back, and no other machine inherits it.
- **Where it is kept.** `source-bindings.json` in the backend's state
  directory (the parent of `instance_dir` on Firecracker and VZ), mode `0600`.
  The server does not start if it cannot read the file. A missing file is a
  first start: the server creates an empty one and warns that the first
  machine to connect for each user becomes that user's source. Up to 32
  machines are remembered per user.
- **Adding a user list later.** On the first start with a user list, a server
  with exactly one user gives that user the source it had before; with several
  users it is dropped. At every start the records of users no longer
  configured are removed.

`GET /api/info` lists the feature `source_binding` on a server that pins. The
routes are [`GET /api/sources`](api.md#get-apisources) and [`POST
/api/sources/use`](api.md#post-apisourcesuse) (control scope).

### Refusals on the bus

Subscribe (`GET /api/plugins/listeners/{ns}/messages`) and respond (`POST
/api/plugins/listeners/{ns}/respond`) answer, besides `200`/`204`:

| Status | `error.code` | When |
|---|---|---|
| `403` | `FORBIDDEN` | A service client, or a script key (subscribe and respond); on respond, also an answer that matches no pending request |
| `403` | `SOURCE_MACHINE_ID_REQUIRED` | The machine headers are missing or invalid (a broker released before v0.10.0). The message names the minimum versions |
| `403` | `SOURCE_NOT_BOUND` | Another machine is this user's source. The body also carries `source.machine_id` and `source.machine_name` |
| `409` | `SOURCE_DUPLICATE_STREAM` | The source machine already holds this request type (a second process with the same machine id, or a stream that has not ended yet). Retryable; nothing is replaced |
| `429` | `SOURCE_RATE_LIMITED` | Too many new or changed machines for this user lately (8 at once, then one every 15 s). With `Retry-After` |
| `429` | `TOO_MANY_RESPONSES` | Respond only: more than 8 answers in flight to one shed connection. With `Retry-After: 1` |
| `413` | `REQUEST_TOO_LARGE` | Respond only: a body over the guest framing limit |
| `503` | `RESPONSE_NOT_DELIVERED` | Respond only: the shed did not take the answer within 5 s; nothing was written and the request is still pending. With `Retry-After: 1` |
| `404` | `SHED_NOT_CONNECTED` | Respond only: the write into the shed failed part-way; not retryable |
| `500` | `INTERNAL_ERROR` | The bindings store could not be saved; nothing was registered |

None of the pin refusals is ever a `401`, and each body is under 1024 bytes:

```json
{"error":{"code":"SOURCE_NOT_BOUND","message":"this server takes your credential requests only from your machine \"laptop\"; to use this machine instead, run 'shed source use' naming this machine"},
 "source":{"machine_id":"0123456789abcdef0123456789abcdef","machine_name":"laptop"}}
```

### Limits

| Limit | Value |
|---|---|
| Messages held per shed connection | 128, and 4 MiB |
| Messages held by the server | 64 MiB |
| A guest-chosen identifier | 128 bytes |
| Answers in flight per shed connection | 8, each written within 5 s |
| Wait for room in a full listener queue | 1 s; then the message is dropped |
| An unanswered request | answerable for 1 hour |
| A write to a subscription stream | 15 s, then the stream is closed |
| Keepalive on an idle subscription | every 25 s |
| Unacknowledged data on a subscription's connection | 40 s (the kernel then fails it, so a machine that went to sleep is dropped and its own reconnect succeeds) |

### Service-client sheds

The bus is **always off for a shed a [service client](#service-clients)
owns** (not a policy option): the server drops that shed's plugin messages
before they reach a listener, refuses to send it any, and leaves it out of
`GET /api/plugins/sheds`. An owned shed can never ask for an SSH signature or a
cloud credential.

## Service clients

A **service client** is an orchestrator that drives sheds on a server it does
not administer: a CI system, or an agent runner such as Nocturn. It gets a
narrow identity and a narrow blast radius. Clients are declared under
[`auth.clients`](configuration.md#service-clients), exist only in `auth.mode:
mtls` (the config is refused otherwise), and are set up as the [Orchestrated
sheds guide](../guides/orchestrated-sheds.md) describes. A server with no
`auth.clients` behaves exactly as before.

### Identity

`shed-server client issue <name>`, run on the shed host, signs a certificate
with the server's existing mtls client CA. A service certificate is shaped so
it can never be confused with a user's (whose CN is an SSH key fingerprint):

- **CN** = the client name (`^[a-z][a-z0-9-]{0,62}$`, so it can never match an
  SSH fingerprint), **OU** = `service`, and exactly one URI SAN,
  `shed-client://<name>`. Nothing else.
- A certificate with either service marker is judged as a service certificate
  only. If its shape is wrong, it is refused, never treated as a user's.
- It is accepted only while `<name>` is in `auth.clients`, at the TLS
  handshake and again on every request. It is **never checked against the
  key set**, and it carries no SSH access.
- A client name may not also be an `auth.users` name (config load refuses it).
- A user's certificate (from `shed server add`) is never treated as a client,
  whatever its CSR asked for, because the server composes its subject itself.

### What a client can and cannot do

Client requests are deny by default. A client can read `/api/info` and
`/api/images`, list its own sheds, create sheds within its policy, and get,
start, stop, delete and [exec](api.md#post-apishedsnameexec) in the sheds it
owns. Everything else is `403 FORBIDDEN`: snapshots, images other than the
list, system and prune, the credential bus, Connect tunnels, egress, reset,
sessions. A shed it does not own is `404 SHED_NOT_FOUND`, with the same body
as a shed that does not exist. The full table is in [API › Service
clients](api.md#service-clients); a unit test walks every registered route and
fails when one is neither on the client allowlist nor asserted forbidden.

The create policy (`403 POLICY_VIOLATION`) refuses host directories
(`local_dir`, `add_dirs`), snapshots, images outside the policy, names without
its prefix, resources over its caps, more than `max_sheds` owned sheds, a
network profile other than the pinned one, and server mounts outside
`server_mounts`. It runs before anything touches the host's filesystem.

Ownership is the `owner` field in the shed's metadata and JSON. It is bound to
the shed an operation acts on: the backend re-checks the owner under its
per-shed lock, so a request cannot act on a shed someone else recreated under
the same name in the meantime. A client's shed has no `user`. **An admin may
list, stop, delete and enter a client's shed**; a user who is not an admin
cannot see it (see [Ownership](#ownership)). Users are unchanged for a
client: its certificate, routes and policy are the same on a server with or
without a user list.

Each owned shed:

- gets **no server mounts** unless the policy's `server_mounts` names some;
- has the **credential bus off** (see [Credential bus](#credential-bus));
- never sees the server's `env_file` variables, which are otherwise passed to
  a `repo` clone;
- has the client's pinned **network profile**, `isolated` by default (see
  [Network profiles](#network-profiles));
- is **deleted once it is older than the policy's `max_age`**: a reaper checks
  at start and every 60 s, stops the shed, deletes it and writes a `reap`
  audit line. It is the only server-side lifetime bound; the orchestrator is
  expected to delete its sheds itself.

Every client request, allowed or refused, writes one `api` line to the [audit
log](configuration.md#audit-log), and every HTTPS exec that passes the request
checks writes an `exec.end` line (argv[0] and the argument count by default,
never the arguments). Each line names who acted: the client (`kind: client`,
by name and certificate serial); a person (`kind: user`, with whether they are
an admin and whether they used a script key, and their key's fingerprint); or
the server itself (`kind: system`, on a `reap` line). On a server with a user
list, an admin's exec into a client's shed is recorded under that admin's user
name. On a server with no user list it carries no user name: the person is the
server's one implicit user (`implicit: true`), known on the line only by the
key's fingerprint.

**Every time a person enters a client's shed over SSH, an `ssh` line records
it**: a shell, a command (`shed exec`, `shed attach`), sftp or scp, and each
connection forwarded with `ssh -L`. Only an admin can enter a client's shed
(on a server with no user list, its one implicit user; a user who is not an
admin is told the shed does not exist), and the shed holds what
the orchestrator put there, so this is the record of who went in over SSH. The line
names the person as above (by user name and key fingerprint), the peer's
address, the shed, its client and the channel, never the command. It is
written once the entry has happened (the command was handed to the shed's
agent, where it may still fail to start, or the forwarded connection is open),
so a refused attempt, or one that failed before it reached the agent, writes
none and leaves only its server-log line.

The log is not yet a complete record of what a person does to a client's
shed. Of the ways a person (an admin) can reach into one:

- **Recorded:** an HTTPS exec (`exec.end`, written when the exec ends: one
  still running when a stopping server's 30-second bound passes leaves none); a shell, a command, sftp or scp,
  or a connection forwarded over SSH (`ssh`); a tunnel through the HTTPS
  Connect API, as `shed tunnels` opens (`connect`, once the tunnel is
  established).
- **Not recorded yet:** starting, stopping, deleting or resetting the shed;
  taking a snapshot of it; changing its egress rules; reading its egress
  decisions (its recent decisions, `GET /api/egress/{name}`, or the live
  egress stream, `GET /api/egress/stream`); listing or killing its sessions;
  reading its record (`shed list`, `GET /api/sheds/{name}`). The access guard
  lets an admin do each of these to a client's shed, and none writes an audit
  line.

### Residual risks

- **A leaked bundle is root in that client's sheds.** Whoever holds a client's
  certificate and key can do everything the client can: create sheds within its
  policy, run any command as root (through `sudo`) in its live sheds, read
  whatever the orchestrator put there (a clone, a token passed to a command),
  and delete them. Through the API it cannot touch a user's shed or another
  client's, and the certificate gives no admin route and no SSH. What a command
  run in its sheds can reach is a separate question, answered by the shed's
  network profile and mounts: with the default `isolated` profile, only the
  internet (see [Network profiles](#network-profiles) for the egress-proxy
  exception); with `policy.network: open` (required on VZ), whatever the host's
  NAT reaches, including the host, the LAN and other open sheds; and the host
  directories in the client's `server_mounts`. Store the bundle as a secret,
  and keep `cert_max_ttl` short.
- **No serial revocation.** One certificate cannot be revoked on its own.
  Removing the client from `auth.clients` and restarting `serve` revokes every
  certificate issued for it; re-adding it later trusts them again until they
  expire. Rename the client when a bundle leaks.
- **A guest is root in its VM**, by design, and an `isolated` guest still has
  the internet: isolation keeps it away from the host and its networks, not
  from exfiltrating what the shed holds.
- **Firecracker runs as root, without the jailer.** shed-server starts
  `firecracker` directly, not under Firecracker's `jailer` (chroot,
  namespaces, privilege drop), so a VMM escape would be root on the host. The
  KVM boundary and Firecracker's own seccomp filters are what stand between a
  guest and the host.
- **Exec cancellation is `SIGHUP`.** A timeout or a client disconnect closes
  the agent connection, and the guest agent sends `SIGHUP` to the command's
  process group. A process that ignores `SIGHUP` (or leaves the group) keeps
  running until the shed stops.

## Mount selection

Every shed gets every server mount (`mounts:`) by default, as before; on a
server with users, a user who is not an admin gets none (server mounts are
[admin-only](#admin-only-resources)). A shed can be created with fewer, through `shed create --mounts none|a,b` or the
API's `mounts` field, and a service client's sheds get only its
`server_mounts`. The choice is stored with the shed as an explicit mode
(`all`, or `selected` plus names; a stored selection with no names is none,
and an unknown mode mounts nothing) and is applied on every start, including
an SSH auto-start, on both backends. The selection controls which exports are
started or attached **for that shed**: without a mount, on Firecracker no 9P
server is started for it, and on VZ no VirtioFS device is attached.

!!! warning "Leaving out a mount is not, alone, an access boundary on Firecracker"
    Each Firecracker 9P server listens on the bridge gateway and does not
    authenticate which shed connects. An `open` shed with no mounts can still
    reach another running shed's 9P port, and with it that shed's export. Pair
    a reduced mount selection with the [`isolated`](#network-profiles) profile,
    whose rules let a guest reach only its own ports on the gateway, when the
    shed must not reach host directories. (Service clients' sheds are
    `isolated` by default.)

## Network profiles

Each shed has a network profile, chosen at create and fixed for its life:

- **`open`** (the default): today's network. The guest reaches whatever the
  host's NAT reaches, including the host, the LAN and other sheds.
- **`isolated`** (Firecracker only): the internet and nothing else. Not the
  host (on any of its addresses, the gateway included, except the shed's own
  ports there; see [How it is enforced](#how-it-is-enforced)), other sheds, RFC 1918,
  CGNAT and tailnet (`100.64.0.0/10`), link-local and cloud metadata
  (`169.254.0.0/16`), loopback, multicast and the other reserved IPv4 ranges,
  and no IPv6 at all. Anti-spoofing is on. These rules cover the guest's
  **direct** traffic. One exception is mediated: a shed with an [egress
  profile](egress.md) may reach its own egress proxy on the gateway, and the
  proxy dials upstream from the host, outside these rules. Its deny-CIDR guards
  refuse private, reserved and link-local ranges and the gateway, but not the
  host's own public addresses, so through the proxy an isolated guest can
  reach a service listening on a public address of the host (in `audit` mode,
  or when an allow rule matches it). The guest's DNS server (8.8.8.8) is
  on the internet, so name resolution works. The host still reaches the
  guest: `shed exec`, the HTTPS exec, tunnels and the agent's health checks
  run over vsock, not the network.

Choose it per shed with `shed create --network isolated`, for every create
without a `--network` with `network.default_profile`, or for a service client
with its `policy.network` (isolated unless set). VZ answers
`422 NETWORK_PROFILE_UNSUPPORTED` for anything but `open`.

### How it is enforced

shed-server owns two nftables tables named after the server's `name:`
(`bridge shed_<name>` and `inet shed_<name>`, every character outside
`[A-Za-z0-9_]` replaced by `_`). It installs, reasserts and removes them itself,
always as one `nft -f -` transaction that replaces both tables. The tables only
**add drops**: shed-server's existing iptables NAT and `FORWARD` rules, the
bridge, and every other table are left alone. Each isolated shed is a set of
elements (its TAP, MAC and IP):

- **Anti-spoofing, keyed on the claimed address.** A frame or ARP whose source
  IP is an isolated shed's is dropped unless it arrives on that shed's TAP with
  that shed's MAC, whatever port it arrives on, and a frame carrying an
  isolated shed's MAC from any other port is dropped. An isolated guest's own
  port passes only its own ARP and IPv4 (IPv6 and everything else are
  dropped). So neither the guest nor an open shed (or another server's guest
  on a shared bridge) can take an isolated shed's address.
- **Guest to guest:** anything to or from an isolated shed's TAP is dropped at
  the bridge.
- **Host input** from an isolated shed is dropped, except replies on
  connections the host opened and the shed's own ports on the gateway: its
  egress-proxy port when it has an [egress profile](egress.md) and its own 9P
  ports when it has server mounts or `--local-dir`/`--add-dir` mounts.
- **Forwarded traffic** from an isolated shed is dropped back onto the bridge
  and to the blocked ranges, minus `network.allow_cidrs`. New inbound
  connections routed to it are dropped.

The tables exist only while something needs them: an isolated shed exists or is
being created, or the default profile is `isolated`. An open-only server
installs nothing and does not need `nft`.

### Guarantees and limits

- **Fails closed at create and start.** An isolated shed's entry is installed
  before its VM exists or starts; if that fails, the create or start is refused
  with `503 NETWORK_ENFORCEMENT_UNAVAILABLE`. An isolated shed never runs
  unguarded because the rules could not be installed.
- **Rebuilt at `serve` start**, from shed metadata (stopped isolated sheds
  included), before the API listens and before running sheds are resumed. If
  that fails, every isolated shed's TAP is set down (no network) until it
  succeeds.
- **Checked every 30 s.** A missing table is put back and logged loudly. If the
  reapply fails, the isolated sheds' TAPs go down until it succeeds, then come
  back up. So **if something else flushes the ruleset** (`nft flush ruleset`, a
  firewall reload), isolated sheds can be open **for up to 30 s** before the
  check restores the tables.
- **`allow_cidrs` is forward-only.** It opens routed destinations (a LAN
  registry); it never opens the host's own addresses or other sheds.
- **While `serve` is stopped** the tables stay in the kernel; `shed-server
  network teardown` removes them, and isolated sheds are then open until
  `serve` starts again.
- **Not in this release:** a proxy-only or no-network profile, and MSS
  clamping.

## Removed in v0.10.0

These fail config load with a message naming `mtls`; nothing falls back to
open:

| Removed | Replacement |
|---------|-------------|
| `auth.mode: token` (bearer tokens minted over `_bootstrap`) | `auth.mode: mtls`. Every client since v0.8.1 re-enrolls with a certificate on its next request. |
| `auth.mode: secure` (the older spelling of `token`) | `auth.mode: mtls`. |
| `allow_insecure_exposure` (an open server off loopback), with any value | An open server is loopback only; use `auth.mode: mtls` to reach the server from other hosts. |

Also new in v0.10.0: an unknown or differently cased key anywhere under `auth`
fails config load (before, `auth: {Mode: mtls}` was ignored and the server
started open), and so does a top-level key that matches a known one only when
case is ignored (`Auth:`, `Bind_Address:`). The server holds no bearer-token
code: an `Authorization` header is never read. The clients still speak the
token protocol to servers older than v0.10.0. The [v0.9.0 → v0.10.0 upgrade
guide](../upgrades/v0.9.0-to-v0.10.0.md) takes a server of any mode across.

## Removed earlier

These keys are **rejected at startup** with a message naming them, so an old
config cannot silently weaken a deployment:

| Removed | In | Replacement today |
|---------|----|-------------------|
| `public_exposure: true` | v0.7.1 | `auth.mode: mtls` |
| `auth.http.tokens` (static list) + `shed-server token new` | v0.7.1 | Certificates enrolled over the `_bootstrap` SSH channel by `shed server add` |
| `auth.http` (the whole block) | v0.7.2 | HTTP enforcement derives from `auth.mode: mtls` |
| `internal_http_port` | v0.7.4 | The bus and Connect tunnel ride the single listener |
| `http_bind` / `ssh_bind` | v0.7.4 | One `bind_address` for every listener |

`https_port` and `auth.ssh.mode: enforce` are valid only in mtls mode, and an
explicit `auth.ssh.mode: off` or `warn` under mtls is refused. See the
[v0.7.0 → v0.7.1](https://github.com/charliek/shed/blob/v0.9.0/docs/upgrades/v0.7.0-to-v0.7.1.md),
[v0.7.1 → v0.7.2](https://github.com/charliek/shed/blob/v0.9.0/docs/upgrades/v0.7.1-to-v0.7.2.md)
and [v0.7.3 → v0.7.4](https://github.com/charliek/shed/blob/v0.9.0/docs/upgrades/v0.7.3-to-v0.7.4.md)
upgrade guides.

## Deferred

An **enrollment secret** (a transport-layer HMAC over the bootstrap handshake,
to gate issuance even tighter than the SSH key set) remains out of scope.

Also not built yet (see the [Roadmap](../ROADMAP.md)):

- **Listing users** (`shed users ls`), **moving a shed between users** (only a
  shed with no user can be assigned), and **assigning snapshots**.

- **Per-certificate revocation** for service clients (by serial); today a
  client is revoked as a whole.
- **Running Firecracker under its `jailer`** (chroot, namespaces, privilege
  drop).
- **A proxy-only and a no-network profile**: isolated with the [egress
  proxy](egress.md) as the only way out, and no network at all
  ([#205](https://github.com/charliek/shed/issues/205)).
