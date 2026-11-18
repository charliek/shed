# User Directory

A shed server can take its users from an **account service**: a service that
holds people and their SSH login keys, and hands each server enrolled with it
the list of users that server admits. People are added, removed and made
admins on the service, and every enrolled server follows within its refresh
interval, or at once with `shed users refresh`, with no config change or
restart.

How the two sides work together:

- The server **enrols once** with a one-time code the service's operator
  issues. It makes an Ed25519 key pair for the service and keeps the private
  key on the host.
- It **signs in** with OAuth 2.0 client credentials and a JWT client assertion
  signed by that key, and **pulls the list**: users, each a name, an admin
  flag and SSH public keys.
- It **keeps a copy** of each good pull, and honours it for a **trust time**
  while it cannot reach the service.
- **Local admins**, listed in the server's own config, log in whatever state
  the service is in.

People sign in to the service, and register their keys there, by whatever
means it offers; shed takes no part in that. The server and the service speak
the [directory contract](../reference/directory-contract.md), and any
implementation of the service side works. The repository has a [reference
server](../reference/directory-contract.md#the-reference-server) for tests and
trials, used [below](#try-it-with-the-reference-server).

Every field is in [Configuration › Users from an account
service](../reference/configuration.md#users-from-an-account-service), and the
security model, which an operator should read first, in [Security › Users from
an account service](../reference/security.md#users-from-an-account-service).
In short: **whoever controls the account service controls the host** of every
server enrolled with it, since an admin in its list is an admin there.

## Before you start

- The server runs `auth.mode: mtls` (see [Security
  Configuration](security-configuration.md#case-3-networked-mtls-facing-the-network)).
- The server can reach the service over HTTPS, through the environment's
  proxy settings if it has any.
- **The host's clock is accurate** (NTP). The trust time is measured on it,
  and a clock set back extends it.
- You have an enrolment code for this server from the service's operator.
- On a server that already has sheds, read [Switching a server to the
  directory](#switching-a-server-to-the-directory) first: the switch is a
  fresh start.

## Configure the server

Add `auth.directory`, and keep in `auth.users` only the **local admins**: at
least one, each `admin: true`, with keys only (no `github`, no `script` key).
Remove every other user from `auth.users`; they come from the service.

```yaml
auth:
  mode: mtls
  users:
    admin:                               # a local admin: config only, always an admin
      admin: true
      keys:
        - key: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA... admin@laptop
          label: laptop
  directory:
    url: https://accounts.example.test   # the service's issuer
    organization: example                # optional
    refresh: 24h                         # the default
    trust: 72h                           # the default
```

A local admin is a separate, local-only identity. A list entry with a local
admin's name, or with one of a local admin's keys, is skipped. If you also
have an account on the service and want to use it day to day, give the local
admin another name (such as `admin`, above), and [keep it on a backup
key](#keeping-the-local-admin-on-a-backup-key).

Check the config with the binary you will run:

```bash
sudo shed-server --config /etc/shed/server.yaml config-validate
```

It ends with the directory's summary:

```text
users: from the directory at https://accounts.example.test (organization example), refresh 24h0m0s, trust 72h0m0s
local admins: admin
```

## Enrol

Run `shed-server directory enroll` on the shed host, **as the user the server
runs as** (root for the Linux package, your own user for the Homebrew
service), with the same `--config`. It reads the code from standard input,
never from the command line: at a terminal it prompts without echoing it.

```bash
sudo shed-server --config /etc/shed/server.yaml directory enroll --name build-1
```

```text
State directory: /etc/shed
Enrolment code (not echoed):
Enrolled with the account service at https://accounts.example.test as srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a (organization example, name "build-1").
Wrote /etc/shed/directory/enrolment.json (mode 0600): this server's key for the account service, which never leaves this host.
```

Without a terminal it reads one line: pipe the code from the command that
issued it, or read it from a file only you can read, then remove the file:

```bash
sudo shed-server --config /etc/shed/server.yaml directory enroll --name build-1 < ~/enrol-code   # mode 0600
rm ~/enrol-code
```

Never put the code in a command's arguments or in `echo` or `printf`: it would
reach the shell's history and the process list.

| Flag | Default | Description |
|------|---------|-------------|
| `--name` | the host name | The name the service shows for this server: 1 to 255 printable ASCII characters. Required when the host name is not one |
| `--config` | Auto-detect | The server config: the URL, the organization and `state_dir` come from it |

What the command checks and refuses:

- **The state directory** (`state_dir`) must be owned by the user running the
  command, and `{state_dir}/directory/`, if it exists, must have mode `0700`.
  The command creates both when they do not exist.
- **One enrolment at a time.** It takes a lock and never waits for one:
  `another enrolment is running (it holds <state dir>/directory/enrol.lock);
  nothing was sent`.
- **Enrolled already.** `this server is already enrolled, as <client id>
  (<path>); to enrol again, stop the server, remove <path>, disable <client
  id> on the account service, and enrol with a new code`.
- **A leftover of an earlier run** (a temporary file it left when stopped
  between writing and renaming) is removed when it reads as a complete
  enrolment, saying so, and naming its client id to disable when this server
  keeps no key for it. When one cannot be read as a complete enrolment, the
  command stops before reading the code and removes nothing.
- **The code** is 1 to 64 bytes; more is refused (`the enrolment code is
  longer than 64 bytes; nothing was sent`).
- **The code is kept nowhere.** A `--name` or host name that contains the
  code is refused before anything is sent, and an answer whose client id,
  issuer, token endpoint or organization contains it fails its checks.
- **The service's answer** must name contract 1, a client id of the form
  `srv_` and 32 hex digits, the configured URL as its issuer byte for byte,
  `<url>/oauth2/token` as its token endpoint, and an organization that matches
  `auth.directory.organization` when that is set.

`enrolment.json` is written once, under the lock, and never replaced. A
running server reads it before each pull, so a server already running picks
up its first enrolment without a restart.

## Start the server and check it

Restart the server. Its start-up log names the source and its state:

```text
Users: from the directory at https://accounts.example.test (organization example, client id srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a)
Users: no kept copy; only the local admins can log in until the first good pull
Local admins: admin
```

The first pull starts at once. With a kept copy from an earlier run the second
line reads `Users: kept copy from <time>, version <version>, <n> users (<k>
skipped), trusted until <time>`, and the copy's users can log in from the
start. As a local admin (or any admin), check the state:

```bash
shed users status
```

```text
Source: directory (an account service's list, beside the local admins)
Account service:  https://accounts.example.test
Organization:     example
Client id:        srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a
State:            trusted
Last good pull:   2026-11-17T09:00:00Z
Version:          0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0
Trusted until:    2026-11-20T09:00:00Z (about 2d 22h 30m left, by this machine's clock)
Last attempt:     2026-11-17T09:00:00Z, good
Next attempt:     2026-11-18T09:00:00Z
Kept copy:        kept

Users:
  USER   ADMIN  SOURCE
  "ada"  yes    directory
  "bob"  no     directory
  admin  yes    local

Skipped entries: none
```

Names from the service are quoted. `State` is one of `trusted`, `expired`
(the trust time ended), `no-copy` (enrolled, no list yet), `not-enrolled` and
`enrolment-unusable`, with the reason beside every state but `trusted`. `Kept
copy` is `kept`, `none`, `unused` (the copy found at start failed a check),
`not-written` or `stale-on-disk` (see [Security › The kept copy and the trust
time](../reference/security.md#the-kept-copy-and-the-trust-time)). See [CLI ›
shed users status](../reference/cli.md#shed-users-status).

To pull now:

```bash
shed users refresh
```

```text
Directory pull: good: version 0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0, 2 users (0 skipped)
```

The count is the list's users the server admits; local admins are not
counted. A failed pull prints its reason and what is in force, and the command
exits non-zero:

```text
Directory pull: failed: directory: unavailable (HTTP 503) "UNAVAILABLE": "try later"
The list held stays: version 0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0, 2 users (0 skipped)
Error: refresh users on build-1: the directory pull failed; the list in force is unchanged
```

When no list is in force after it, the second line says why (`No list held:
only the local admins can log in.`, `The list held is past its trust time:
...`, `The list held is withdrawn: ...`) and the error ends `only the local
admins are admitted`.

`shed users refresh` pulls even while the service's `Retry-After` is pending;
nothing else does.

## People

### A new person

A person added on the service needs nothing on the server. They add it from
their own machine with their own key, as for any mtls server:

```bash
shed server add build-1.example.test --name build-1
```

Their key is not in the list the server holds yet, so their first SSH login
makes the server pull the list. That login waits at most 8 seconds. If the
account service is slower than that, the login is refused once; the server's
pull goes on and its result applies, so trying again succeeds. An admin
running `shed users refresh` before the person's first login avoids the wait
altogether. The server pulls for an unknown key at most once a minute while
pulls succeed, once per 15 minutes while they fail, and never inside a
pending `Retry-After`; a second unknown key in that window is refused at
once. If that happens to a genuine newcomer, they try again a minute later,
or an admin runs `shed users refresh` first.

### Admins, removals and keys

- **The admin flag** comes from the list. A change applies at the person's
  next SSH channel and API request, with the same certificate.
- **A person removed** on the service loses access at the next pull: their SSH
  connections are closed and their next login and request are refused. Run
  `shed users refresh` to make it immediate. Open API streams and Connect
  tunnels are not closed; they end by themselves. Their sheds and snapshots
  keep their name, and an admin may stop or delete them.
- **A key moved** from one person to another on the service belongs to the
  second person from the next pull.
- **A key's `expires`** in the list is an RFC 3339 instant
  (`2027-06-30T23:59:59Z`); a bare date, which `auth.users` accepts, is not
  accepted there, and a key whose `expires` does not read as an instant is
  not used.

The server skips list entries it cannot answer for (a name that is not a valid
user name, a name or key listed twice, no usable key, a local admin's name or
key, an `auth.clients` name) and logs each; `shed users status` lists them
under `Skipped entries`.

## When the service cannot be reached

A failed pull changes nothing: the list the server holds stays, and the next
attempt is 15 minutes later, or at the end of the service's `Retry-After` (up
to one hour) when that is later. The server logs each failure, for example
`directory: the account service is unavailable or did not answer in time:
...; the list held stays; next attempt at <time>`. The line's ending depends on
whether a list is in force:

| List state | The line ends |
|---|---|
| A list is in force | `the list held stays` (`the list held stays until its trust time ends` for a disabled server) |
| The trust time of the last list ended | `no list is in force (the trust time of the last list ended); only the local admins can log in` |
| No list was ever held | `no list is in force; only the local admins can log in` |

The list is honoured for `auth.directory.trust` (default `72h`) after the
good pull it came from. When that ends without a good pull, only the local
admins can log in, the listed users' SSH connections are closed, the kept copy
is removed (so a restart cannot bring the list back), and the log says
`directory: TRUST ENDED: ...`. The next good pull restores the list. A local
admin can log in throughout. The copy goes at once when a login or request
finds the end, within 30 seconds otherwise, and at a graceful stop. What
remains: a server that dies without a graceful stop in the moment between
the end of the trust time and the removal of the copy, followed by a clock
set back and a restart while the service cannot be reached, brings the list
back for what remains of its trust time on that clock. Keep the host's clock
accurate.

A server the service has disabled keeps its list until the trust time ends,
and logs `directory: SERVER DISABLED: ...`.

## Keeping the local admin on a backup key

This is for a person who is both a user in the service's list (`pat`, say)
and the server's local admin (`admin`). The everyday key stays in the list; a
second key, the **backup key**, is the local admin's only key. You hold two
CLI entries for the server, each reaching it with exactly one key: an entry's
client certificate names the key it enrolled with, and the CLI enrols again by
itself (near the certificate's expiry, and whenever the server rejects it)
with the first key `ssh` offers that the server accepts. An entry that can
offer both keys becomes the local admin when the list is not in force ([see
below](#if-the-everyday-entry-can-reach-the-backup-key)).

1. **Make the backup key** with a passphrase, and keep it out of your agent.
   The CLI runs `ssh` with `BatchMode=yes`, which cannot ask for a passphrase:
   load the key for a few minutes when you need it. Without it in the agent,
   adding the admin entry fails with `Error: your SSH key is not in this
   server's allowlist ...`.

    ```bash
    ssh-keygen -t ed25519 -f ~/.ssh/shed-admin -C shed-admin
    ```

2. **In the server's config**, the local admin holds the backup key only. The
   everyday key is `pat`'s in the list, not in the config.

    ```yaml
    auth:
      mode: mtls
      users:
        admin:                               # the local admin: the backup key only
          admin: true
          keys:
            - key: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA... shed-admin
              label: backup
      directory:
        url: https://accounts.example.test
    ```

3. **In `~/.ssh/config`**, give each entry its own name for the server. An
   entry's host is both its SSH host and its API address, so both names must
   reach the server: its DNS name and a second one for the same address, or the
   name and its address. A `Host` alias with `HostName` does not work (the API
   address would be the alias). The CLI checks the server's TLS certificate by
   its pin, not by name, so the second name need not be in `tls_names`.
   `IdentitiesOnly yes` on **both** stanzas keeps each to its one key; without
   it `ssh` also offers every key in the agent.

    ```text
    Host shed.example.test
        IdentityFile ~/.ssh/id_ed25519
        IdentitiesOnly yes

    Host admin.shed.example.test
        IdentityFile ~/.ssh/shed-admin
        IdentitiesOnly yes
    ```

4. **Add both entries**, the everyday one first so that it is the default:

    ```bash
    shed server add shed.example.test --name shed
    ssh-add -t 10m ~/.ssh/shed-admin
    shed server add admin.shed.example.test --name shed-admin
    ssh-add -d ~/.ssh/shed-admin
    ```

5. **Check who each entry is**: `shed source ls` names the caller's user.

    ```bash
    shed --json -s shed source ls          # "user": "pat"
    shed --json -s shed-admin source ls    # "user": "admin"
    ```

Day to day, use the everyday entry. `shed console` and `shed exec` run `ssh`
to the entry's host too, so its stanza gives them the everyday key.

### When the list is not in force

Every command through the everyday entry fails and exits 1:

```text
Error: failed to list sheds: re-authenticating after a TLS client-certificate rejection: sdk/bootstrap: ssh could not authenticate with any available identity (the daemon may have no SSH identity available — see IdentityAgent docs — or the key may not be on the server allowlist): ...
_bootstrap@shed.example.test: Permission denied (publickey).
```

Use the admin entry. It works with the backup key out of the agent while its
certificate is valid (`auth.token_ttl`, 24 hours by default); when a command
through it fails with `Permission denied (publickey)`, load the key
(`ssh-add -t 10m ~/.ssh/shed-admin`) and run it again.

```bash
shed -s shed-admin users status    # State: expired (the list's trust time ended at <time>)
shed -s shed-admin users refresh   # when the service is back: Directory pull: good: ...
```

The everyday entry then works again as `pat` at its next command, with the
certificate it holds. Without the refresh, the list comes back at the next
pull (`Next attempt` in `shed users status`).

### If the everyday entry can reach the backup key

Do not let `ssh` offer the backup key for the everyday host. When the list is
not in force, the server refuses the everyday key, `ssh` offers the backup
key, and the server accepts it: the command succeeds, prints nothing about it
(not even with `-vv`), and **from then on the everyday entry is the local
admin**. It stays so after the list is back, until it next enrols (in the last
2 hours of its certificate's lifetime: up to about 22 hours later with the
default `auth.token_ttl`).

| How `ssh` can reach the backup key for the everyday host | At add | List not in force | List back |
|---|---|---|---|
| A stanza names the everyday key, without `IdentitiesOnly yes`; both keys in the agent | `pat` | `admin`, silently | `admin` until it next enrols, then `pat` |
| No stanza; both keys in the agent | The user of the agent's first key | `admin` | `admin` until it next enrols, then the user of the agent's first key |
| No stanza; the everyday key in the agent, the backup key as the default identity file (`~/.ssh/id_ed25519`) | `pat` | `admin`, silently | `admin` until it next enrols, then `pat` |
| Both stanzas with `IdentitiesOnly yes` (above); the backup key in the agent or not | `pat` | Refused (exit 1) | `pat` at once |

Without a stanza, the agent's order decides at every enrolment, even while the
list is in force. To put a switched entry back (check it with step 5), take
the backup key out of the agent and fix the stanza, then add the entry again:

```bash
ssh-add -d ~/.ssh/shed-admin
shed server remove shed
shed server add shed.example.test --name shed
```

Listing your everyday key under the local admin instead makes you the local
admin on that server for good: a list entry with a local admin's key is
skipped, so your list identity is not used there.

## Switching a server to the directory

There is no migration: treat the switch as a **fresh start**. Ownership is by
name:

- A person whose list name equals an earlier user's name receives that user's
  sheds, snapshots and credential-source records. Pick list names that no
  existing shed or credential-source record carries, unless it is the same
  person.
- Sheds of earlier users who are not in the list keep their user; an admin can
  see, stop and delete them.
- Sheds with no user are admin-only until an admin runs `shed assign <shed>
  <user>`, which accepts a list user.
- The [one-time upgrade step](multi-user-server.md#sheds-with-no-user) reads
  the local admins only, never the list: on a server that never had a user
  list, a single local admin receives every shed with no user (and the
  server's credential source); with several local admins nothing is given
  away.
- Client certificates that name their user keep working where the list (or the
  local admins) has that name with the same key. A certificate issued by a
  server older than v0.10.0 names no user and is refused; the client enrolls
  again.

GitHub usernames are not supported on a directory server: move each person's
keys to the service.

## Enrolling again

An enrolment is written once. To replace it (for a new key, or after the
server was removed on the service):

1. Stop the server.
2. Remove `{state_dir}/directory/enrolment.json`.
3. Disable the old enrolment on the service, and have a new code issued.
4. [Enrol](#enrol) with the new code, then start the server.

The kept copy of the old enrolment is not used under the new one; the first
pull replaces it. Only the local admins can log in until it does.

Stop the server first: the server reads the enrolment before every pull, and
when a pull finds it missing or unusable the list is withdrawn at once (only
the local admins can log in, and the listed users' SSH connections are
closed).

A change of `auth.directory.url` or `organization` is a change of trust
domain: the same names may then mean other people. Treat it as a new server,
with a fresh `state_dir`.

## When an enrolment fails

Every failure says whether anything was sent and what to do. When it says
**nothing was written**, no key is kept.

- **Refused by the service** (a 4xx: an unknown, spent or expired code, a
  server name already enrolled): `the account service refused the enrolment
  (HTTP <status>); nothing was written.`, with the service's words quoted.
  Fix the cause and enrol again.
- **A lost answer** (no answer in 30 seconds, a lost connection, a 5xx, a
  redirect, or a 201 with no usable client id): the service may have enrolled
  the server under a client id this server never learned.

    ```text
    the account service's answer to the enrolment was lost (HTTP 503). Nothing was written and the key was discarded, but the service may have enrolled this server under a client id this server never learned.
    The service's words: code "UNAVAILABLE", message "try later"
    To recover: on the account service, find the server enrolled with this code that this server does not know and disable it; issue a new code; then run 'shed-server directory enroll' again
    ```

- **An answer that failed a check** (another issuer or organization than the
  config's, a wrong token endpoint): the message names the client id the
  service gave, to disable: `the account service enrolled this server as
  <client id>, but its answer failed a check. ... To recover: disable <client
  id> on the account service, check auth.directory against the service, issue
  a new code and enrol again`.

## Switching back

To leave the directory, remove `auth.directory` and list the users in
`auth.users` (or remove the user list). A certificate bound to a name stays
valid when `auth.users` lists that name with the same key; any other is
refused once and its client enrolls again. The enrolment file stays in
`{state_dir}/directory/`; disable the server on the service, and remove the
directory if you will not come back.

## Try it with the reference server

The repository's [reference
server](../reference/directory-contract.md#the-reference-server) serves the
contract on loopback, over plain HTTP, with its people in a file you edit. Use
it with a server you can restart freely, such as a development server, never
one people use. It listens on a loopback address only. From a checkout:

```bash
make build directory-ref
umask 077                                   # the control files must not be writable by others
mkdir -p ~/directory-trial/control
printf '[]\n' > ~/directory-trial/control/users.json
openssl rand -hex 16 > ~/directory-trial/code
sed 's/^/trial /' ~/directory-trial/code > ~/directory-trial/control/codes
bin/test/shed-directory-ref --control ~/directory-trial/control --listen 127.0.0.1:18470
```

It prints `listening on http://127.0.0.1:18470`. Copy the trial server's
config to `~/directory-trial/server.yaml`, give it its own `state_dir` (for
example `~/directory-trial/state`), and make its `auth` block:

```yaml
auth:
  mode: mtls
  users:
    admin:
      admin: true
      keys:
        - key: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA... admin@laptop
          label: laptop
  directory:
    url: http://127.0.0.1:18470
    allow_plain_http: true                # loopback only, for trials and tests
    refresh: 1m
    trust: 5m
```

In a second terminal, enrol with the code from its file and start the server:

```bash
bin/shed-server --config ~/directory-trial/server.yaml directory enroll --name trial < ~/directory-trial/code
bin/shed-server --config ~/directory-trial/server.yaml serve
```

The reference server prints `enrolled trial srv_...`. Then, as the local
admin:

- **Add a person**: write them into `~/directory-trial/control/users.json`
  with a key of theirs, for example
  `[{"name": "ada", "admin": false, "keys": [{"key": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA...", "label": "laptop"}]}]`,
  and run `shed users refresh`, or let them log in with `shed server add`.
- **Make them an admin, or remove them**, by editing the file again.
- **Take the service down**: `touch ~/directory-trial/control/outage`. Pulls
  fail, the list stays for the trust time (five minutes here), then only the
  local admin can log in. `rm ~/directory-trial/control/outage` and `shed
  users refresh` bring the list back.
- **Disable the server**: write its client id into
  `~/directory-trial/control/disabled`.

Files you add to the control directory must not be writable by group or
others (run `umask 077` in that shell too).

`shed users status` shows each step. The reference server keeps everything
else in memory: after it restarts, the trial server must [enrol
again](#enrolling-again), with a new code in `codes` (and a new server name
there, or the old client id in `disabled`). Stop both with Ctrl-C and remove
`~/directory-trial` when done.

## Troubleshooting

| You see | Why | Fix |
|---|---|---|
| `auth.directory requires auth.mode: mtls` | The server runs `open` | Set `auth.mode: mtls` |
| `auth.directory needs at least one local admin in auth.users ...` | No local admin | Add one, `admin: true`, with a key |
| `auth.users.<name>: beside auth.directory every auth.users entry is a local admin; ...` | A user who is not an admin in `auth.users` | Make them `admin: true`, or move them to the service |
| `auth.directory.url "http://..." is plain HTTP; use https, ...` | A plain-HTTP URL | Use `https://`; `allow_plain_http` is for loopback trials only |
| `auth.directory is not configured: this server does not take its users from an account service` | `enroll` with a config without `auth.directory` | Pass the server's `--config` |
| `state dir <dir> is owned by uid <n>, not by the user running this command (uid <m>): run it as the user the server runs as` | `enroll` run as another user | Run it as the server's user (`sudo` for the Linux package) |
| Log: `Users: not enrolled (no enrolment: ...): run 'shed-server directory enroll'; only the local admins can log in` | Not enrolled yet | [Enrol](#enrol) |
| Log: `Users: enrolment unusable (<file>: <check>); only the local admins can log in` | `enrolment.json` or its directory failed a check (owner, mode, symbolic link, size, content) | Put the file right (owned by the server's user, mode `0600`, in a `0700` directory), or [enrol again](#enrolling-again) |
| Log: `directory: the account service refused this server's login: ...` | The service does not recognise this server's key or assertion | Check the host's clock, then the server on the service; enrol again if it was removed there |
| Log: `directory: SERVER DISABLED: ...` | The server is disabled on the service | A disabled server stays disabled: have a new code issued and [enrol again](#enrolling-again) |
| Log: `directory: WARNING: the account service's clock is ... from this server's; ...` | The clocks differ by more than 30 s | Fix the host's clock (NTP) |
| Log: `directory: kept copy not written: ...; a restart starts with only the local admins until a good pull` | The state directory cannot be written | Free space or fix permissions; the next good pull writes it again |
| `shed users status`: `Error: <server>: this server has no status route (it is older than 'shed users status')` | The server is older than the CLI | Upgrade the server |
