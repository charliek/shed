# User Directory

A shed server can take its users from an **account service**: a service that
holds people and their SSH login keys, and hands each server enrolled with it
the list of users that server admits. People are added, removed and made
admins on the service, and each enrolled server follows within its refresh
interval, or at once with `shed users refresh`, with no config change or
restart. Who is listed, and whether as an admin, is what the service sends
that server: it may differ from one enrolled server to another.

How the two sides work together:

- The server **enrols once** with a one-time code the service's operator
  issues. It makes an Ed25519 key pair for the service and keeps the private
  key on the host.
- It **signs in** with OAuth 2.0 client credentials and a JWT client assertion
  signed by that key, asking the token endpoint for the scope `directory:read`
  and nothing else, and **pulls the list**: users, each a name, an admin
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
  fresh start, and [Keeping your sheds](#keeping-your-sheds) says how to keep
  yours.
- On a server installed with Homebrew, use the **Homebrew** tab of each block
  below: every `shed-server` command runs as you, never with `sudo`, and names
  the config with `--config`.

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

=== "Linux package"

    ```bash
    sudo shed-server --config /etc/shed/server.yaml config-validate
    ```

=== "Homebrew"

    ```bash
    "$(brew --prefix)/opt/shed/bin/shed-server" --config "$(brew --prefix)/etc/shed/server.yaml" config-validate
    ```

    The Homebrew service runs this binary with this config. Always pass
    `--config`: without it the server looks only for `./server.yaml`,
    `~/.config/shed/server.yaml` and `/etc/shed/server.yaml`, never in the
    Homebrew prefix.

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
Replace `build-1` with the name the service should show for this server.

=== "Linux package"

    ```bash
    sudo shed-server --config /etc/shed/server.yaml directory enroll --name build-1
    ```

    ```text
    State directory: /etc/shed
    Enrolment code (not echoed):
    Enrolled with the account service at https://accounts.example.test as srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a (organization example, name "build-1").
    Wrote /etc/shed/directory/enrolment.json (mode 0600): this server's key for the account service, which never leaves this host.
    If the server is already running with this config, it reads the enrolment at its next attempt, at most 15 minutes from now; to use it at once, restart the server or run 'shed users refresh' as a local admin.
    ```

=== "Homebrew"

    ```bash
    "$(brew --prefix)/opt/shed/bin/shed-server" --config "$(brew --prefix)/etc/shed/server.yaml" directory enroll --name build-1
    ```

    ```text
    State directory: /Users/pat/.shed
    Enrolment code (not echoed):
    Enrolled with the account service at https://accounts.example.test as srv_0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a (organization example, name "build-1").
    Wrote /Users/pat/.shed/directory/enrolment.json (mode 0600): this server's key for the account service, which never leaves this host.
    If the server is already running with this config, it reads the enrolment at its next attempt, at most 15 minutes from now; to use it at once, restart the server or run 'shed users refresh' as a local admin.
    ```

    Run it as yourself, **never with `sudo`**. The Homebrew service runs as
    your user, so its state directory is `~/.shed` (the enrolment goes to
    `~/.shed/directory/`) unless the config sets `state_dir`. Run with `sudo`
    and no `state_dir`, the command takes `/etc/shed` as the state directory,
    the default for root, and enrols there, where the service never looks:
    its log then says `Users: not enrolled` ([Troubleshooting](#troubleshooting)).
    (With `state_dir` set to a directory you own, `sudo` is refused: the
    directory is not root's.)

Without a terminal it reads one line: pipe the code from the command that
issued it, or read it from a file only you can read, then remove the file:

=== "Linux package"

    ```bash
    sudo shed-server --config /etc/shed/server.yaml directory enroll --name build-1 < ~/enrol-code   # mode 0600
    rm ~/enrol-code
    ```

=== "Homebrew"

    ```bash
    "$(brew --prefix)/opt/shed/bin/shed-server" --config "$(brew --prefix)/etc/shed/server.yaml" directory enroll --name build-1 < ~/enrol-code   # mode 0600
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

`enrolment.json` is written once, under the lock, and never replaced.

**A server already running** reads the enrolment at its next attempt, not at
once: it found none at its last attempt, and tries again 15 minutes after
that. Until it reads it, only the local admins can log in, and a new person's
first login starts no pull. The command's last line says so. To use the
enrolment at once, restart the server (below), or run `shed users refresh` as
a local admin: a pull reads the enrolment first.

## Start the server and check it

Restart the server, and read its start-up log:

=== "Linux package"

    ```bash
    sudo systemctl restart shed-server
    sudo journalctl -u shed-server | grep -E 'Users:|Local admins:'
    ```

    The journal keeps every start's lines: this start's are the last.

=== "Homebrew"

    ```bash
    brew services restart shed
    grep -E 'Users:|Local admins:' "$(brew --prefix)/var/log/shed-server.log"
    ```

    The service writes its log to `$(brew --prefix)/var/log/shed-server.log`
    and appends to it at every start: this start's lines are the last.

The start-up log names the source and its state:

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
(`shed server add` logs in as `_bootstrap`) makes the server pull the list.
That login waits at most 8 seconds: it is let in as soon as the key is known
(its own pull, or any other, brought it), and refused when the 8 seconds end
if the key is still unknown, even when the pull ended sooner. If the account
service is slower than that, their
logins are refused until a pull has brought the key; the server's pull goes on
and its result applies, so a later attempt succeeds. An admin running `shed
users refresh` before the person's first login avoids the wait altogether.
The server pulls for an unknown first login at most once a minute while pulls
succeed, once per 15 minutes while they fail, and never inside a pending
`Retry-After`; a second unknown key in that window is refused at once, not
after 8 seconds. If that
happens to a genuine newcomer, they try again a minute later, or an admin runs
`shed users refresh` first.

A key a person added on the service, used first to log in to an existing shed
(`ssh <shed>@build-1.example.test`), is refused at once; the server then pulls
the list in the background (at most once every 5 minutes) and a retry a few
seconds later gets in. An unknown key under any other name, one no shed on
the server has (such as `root` or `ubuntu`, unless a shed has that name),
starts no pull. [Security › A new
person](../reference/security.md#a-new-person) has the whole rule.

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
CLI entries for the server under its one host name, each with its own key
(`--identity-file`, see [CLI › One SSH key per
entry](../reference/cli.md#one-ssh-key-per-entry)). The CLI enrols an entry
again by itself (near its certificate's expiry, and whenever the server
rejects it), and an entry is held to the user it was enrolled as: a
re-enrolment that would make it another user fails, and nothing is saved
([see below](#if-the-everyday-entry-offers-the-backup-key)).

In the commands, replace `shed.example.test` with your server's host name, and
the key paths with yours.

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

3. **Add both entries** for the same host, each with its key, the everyday one
   first so that it is the default. Each prints the user it is held to
   (`  User: pat`, then `  User: admin`):

    ```bash
    shed server add shed.example.test --name shed --identity-file ~/.ssh/id_ed25519
    ssh-add -t 10m ~/.ssh/shed-admin
    shed server add shed.example.test --name shed-admin --identity-file ~/.ssh/shed-admin
    ssh-add -d ~/.ssh/shed-admin
    ```

    For an everyday entry you already have, set its key instead:

    ```bash
    shed server update shed --identity-file ~/.ssh/id_ed25519
    ```

    If your everyday key is only in an agent (no private key file), give the
    path of its public key: ssh then uses the agent's copy of that key. The
    CLI reads each identity file's public key without a passphrase, so the
    backup key must be in the OpenSSH format (as `ssh-keygen` writes it) or
    have its `.pub` file beside it.

4. **Check who each entry is**: `shed server list` shows it in the `USER`
   column, and `shed source ls` names the caller's user.

    ```bash
    shed server list                       # USER: pat for shed, admin for shed-admin
    shed --json -s shed source ls          # "user": "pat"
    shed --json -s shed-admin source ls    # "user": "admin"
    ```

Day to day, use the everyday entry. `shed console`, `shed exec` and `shed
attach` run `ssh` with the entry's key too (`-i` with `IdentitiesOnly=yes`), so
they reach the server as `pat`. ssh still offers, after the entry's key, any
`IdentityFile` your own `~/.ssh/config` names for that host: keep the backup
key out of such a stanza.

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
shed -s shed-admin users status    # State: expired (the list's trust time ended at ...)
shed -s shed-admin users refresh   # when the service is back: Directory pull: good: ...
```

The everyday entry then works again as `pat` at its next command, with the
certificate it holds. Without the refresh, the list comes back at the next
pull (`Next attempt` in `shed users status`).

### If the everyday entry offers the backup key

The CLI never moves an entry to another user by itself. If `ssh` offers the
backup key for the everyday entry while the list is not in force, the server
accepts it and issues a certificate for `admin`; the CLI refuses it, saves
nothing, and the command fails (exit 1).

With an `identity_file` on the entry, ssh can still offer the backup key when a
stanza in your `~/.ssh/config` names it for that host. The certificate is then
refused for its key:

```text
the server issued a certificate for SSH key "SHA256:...", but entry "shed" uses only its identity_file "/home/pat/.ssh/id_ed25519" (key SHA256:...); nothing was saved.
ssh authenticated with a different key than the entry's identity file. ...
```

Without an `identity_file` (both keys in the agent, say), it is refused for its
user:

```text
the server issued a certificate for user "admin", but entry "shed" is held to user "pat"; nothing was saved.
ssh offered a key that belongs to "admin". To stay "pat", let this entry offer only pat's key (shed server update shed --identity-file <pat's private key>) and retry.
To make this entry "admin": shed server update shed --refetch --allow-user-change
```

Either way the entry stays `pat`'s and keeps the certificate it holds. Give it
its key, remove the backup key from any `~/.ssh/config` stanza for the host,
and use the admin entry until the list is back:

```bash
shed server update shed --identity-file ~/.ssh/id_ed25519
```

Do not run `--allow-user-change` on the everyday entry for this: it would make
the entry the local admin.

Listing your everyday key under the local admin instead makes you the local
admin on that server for good: a list entry with a local admin's key is
skipped, so your list identity is not used there.

### Without an identity file

Two host names for the server, each with its own `~/.ssh/config` stanza, also
work. An entry's host is both its SSH host and its API address, so both names
must reach the server: its DNS name and a second one for the same address, or
the name and its address. A `Host` alias with `HostName` does not work (the API
address would be the alias). The CLI checks the server's TLS certificate by its
pin, not by name, so the second name need not be in `tls_names`.
`IdentitiesOnly yes` on **both** stanzas keeps each to its one key; without it
`ssh` also offers every key in the agent, and a command through the everyday
entry fails as above while the list is not in force.

```text
Host shed.example.test
    IdentityFile ~/.ssh/id_ed25519
    IdentitiesOnly yes

Host admin.shed.example.test
    IdentityFile ~/.ssh/shed-admin
    IdentitiesOnly yes
```

```bash
shed server add shed.example.test --name shed
ssh-add -t 10m ~/.ssh/shed-admin
shed server add admin.shed.example.test --name shed-admin
ssh-add -d ~/.ssh/shed-admin
```

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
  server older than v0.10.0 names no user: the CLI refuses such an entry
  before it sends anything, and `shed server update <name> --refetch` enrols
  it again.

GitHub usernames are not supported on a directory server: move each person's
keys to the service.

### Keeping your sheds

This is for a server one person has used, whose sheds should stay theirs once
they come from the account service: you, listed there as `pat`. It takes two
starts. The one-time upgrade step gives the sheds with no user only to a user
in `auth.users`, and beside `auth.directory` those are the local admins, never
the list. Switching straight to the directory with a local admin `admin` gives
every shed to `admin`; naming the local admin `pat` instead makes the list's
`pat` skipped, since a list entry with a local admin's name is.

In the commands, replace `home` with the name of your CLI entry for the
server, `pat` with your list name, and the key paths with yours.

1. **Start once with only yourself in `auth.users`**, as an admin, with your
   everyday key (the one the account service lists for you), and no
   `auth.directory` yet:

    ```yaml
    auth:
      mode: mtls
      users:
        pat:                                 # your list name; for this one start
          admin: true
          keys:
            - key: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA... pat@laptop
              label: laptop
    ```

    Restart the server, then read what **this start** logged about the step.
    The log keeps earlier starts too, so each command shows only the lines
    since the server's last start: the journal of the unit's current run, or
    the Homebrew log after its last `Starting shed-server...` line:

    === "Linux package"

        ```bash
        sudo systemctl restart shed-server
        sudo journalctl _SYSTEMD_INVOCATION_ID="$(systemctl show --property=InvocationID --value shed-server)" | grep -E 'one-time upgrade step|Users: found'
        ```

    === "Homebrew"

        ```bash
        brew services restart shed
        awk '/Starting shed-server[.][.][.]/ { found = "" } /one-time upgrade step|Users: found/ { found = found $0 "\n" } END { ORS = ""; print found }' "$(brew --prefix)/var/log/shed-server.log"
        ```

    ```text
    Users: first start with a user list (no marker at /etc/shed/users-upgrade.json): running the one-time upgrade step
    Users: the one-time upgrade step for the user list has begun for user pat (the only configured user); until it finishes it resumes only while pat is the only configured user
    Users: found no shed without a user or a service client
    Users: the one-time upgrade step for the user list is done and recorded; from now on a shed with no user is given one only by 'shed assign'
    ```

    (Under Homebrew the record is `~/.shed/users-upgrade.json`.) Step 1 is
    done when the **last** line of this start that names the one-time upgrade
    step is this one:

    ```text
    Users: the one-time upgrade step for the user list is done and recorded; from now on a shed with no user is given one only by 'shed assign'
    ```

    The pass can take up to two minutes: while the last such line is still
    the `has begun` or `resuming` line, run the check again. A `did not
    finish` line followed, in the same start, by `done and recorded` is a
    step that resumed and finished. If the last line says the step `did not
    finish`, restart the server again with the **same** config and check
    again, until it ends with `done and recorded`; change nothing in
    `auth.users` before then (see the warning below). If it says the step
    `ran at` an earlier start instead, this start gave nothing away: read
    [When step 1 can be skipped](#when-step-1-can-be-skipped).

    The start's full log names each shed and snapshot given to `pat`, as
    now `pat`'s, and the server's credential source, if it has one, becomes
    `pat`'s.

    Then, on your own machine, enrol your entry as `pat` (an entry enrolled
    before v0.10.0 is refused until you do). It prints `  User: pat`:

    ```bash
    shed server update home --refetch
    ```

2. **Make sure the account service lists `pat`** with the same everyday key,
   as an admin. The step gave you the sheds that carry host directories or
   server mounts because you were an admin, and only an admin can enter or
   start such a shed. How to list a person is in the account service's own
   documentation.

3. **Switch to the directory**: in `auth.users`, replace `pat` with a local
   admin under another name (`admin`) that holds a backup key only, and add
   `auth.directory` ([Configure the server](#configure-the-server), and
   [Keeping the local admin on a backup
   key](#keeping-the-local-admin-on-a-backup-key) for the key and its CLI
   entry):

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

4. **[Enrol](#enrol) and restart** ([Start the server and check
   it](#start-the-server-and-check-it)). The server pulls the list at once.

5. **Check, as `pat`**, through your everyday entry:

    ```bash
    shed -s home users status
    shed -s home list
    ```

    `shed users status` lists `"pat"` as an admin from `directory` and `admin`
    from `local`, and `shed list` shows your sheds. `shed server list` still
    shows `pat` as the entry's `USER`.

What carries over: the sheds and snapshots, which are `pat`'s by name;
`pat`'s credential-source records (a directory server drops no user's records
at start); the server's SSH host key and TLS certificate, so your entries'
pins stay valid (the state directory is the same); and your entry with its
certificate, which names `pat` and so keeps working once the list holds `pat`
with the same key. Until the first good pull only the local admins can log
in: a command through your entry in that moment enrols again and waits for
the pull, or fails once; run it again.

!!! warning "Finish step 1 before changing `auth.users`"
    A step that has begun and not finished resumes only while the user it
    began for is the only one in `auth.users`. A start with any other users,
    such as step 3's `admin` in place of `pat`, abandons it: the step is
    recorded as done, nothing more is given away, and the sheds and snapshots
    it had not reached stay with no user for good (the log says `the
    unfinished one-time upgrade step for user pat was abandoned`).

#### When step 1 can be skipped

Running with a user list is not enough: the step may be pending. Skip step 1
only when both hold:

- the server's **current** start, with its user list, logs the step as done:
  the last line that names the one-time upgrade step, read with the commands
  of step 1, is `... is done and recorded ...` (the step finished at this
  start) or says it `ran at` an earlier start:

    ```text
    Users: the one-time upgrade step for the user list ran at 2026-11-02T09:14:00Z (marker /etc/shed/users-upgrade.json); sheds with no user are not given one automatically, use 'shed assign'
    ```

- the server has not been started without a user list since. A start with
  no user list removes the step's record, and the next start with one takes
  the step again: then do step 1.

A server whose current start logs the step as begun and not finished (`began
at ... and did not finish`, or a last line that says `did not finish`) must
finish step 1 first: restart it with the same configuration until the last
line is `done and recorded`, before anything in `auth.users` changes.

#### What stays without a user

A step that ran at an earlier start gave the sheds and snapshots with no user
to the one user configured then, or, with several users, to nobody; an
abandoned step gave nothing more. What it did not give away keeps no user, and
the step does not run again (unless a start with no user list removes its
record, as above). Each start counts them, in the `Users: found`
lines the commands of step 1 show. As an admin, through your entry:

- **A shed with no host directories or server mounts:** `shed assign web pat`
  gives the shed `web` to `pat`.
- **A shed that carries host directories or server mounts** cannot be
  assigned. `shed assign` refuses it (`409 SHED_HOST_RESOURCES`): `... shed
  web carries host directories, which assigning it would hand to user pat;
  recreate it without them (no --local-dir or --add-dir, and --mounts none)
  and assign the new shed instead`. Nobody can enter a shed with no user, so
  shed cannot snapshot it or copy anything out of it: recreate it (a shed you
  create as an admin is yours, host directories included) and delete the old
  one.
- **A snapshot** cannot be given to anyone: no command assigns one. An admin
  can create a shed from it (`shed create web2 --from-snapshot old-snap`,
  which makes `web2` the creator's) and can delete it.

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
| Homebrew, after an enrolment run with `sudo`: the log says `Users: not enrolled (no enrolment: /Users/pat/.shed/directory does not exist): ...`, and `shed users status` shows `State: not-enrolled` | Run as root with no `state_dir`, `enroll` used `/etc/shed`, root's state directory; the service runs as you and reads `~/.shed` | Disable the client id the enrolment printed on the account service, remove `/etc/shed/directory` (`sudo rm -r /etc/shed/directory`), have a new code issued, and [enrol](#enrol) again as yourself, without `sudo` |
| `shed users status` still shows `State: not-enrolled (no enrolment: ...)` after `directory enroll` | The server was already running: it reads a first enrolment at its next attempt (`Next attempt`), at most 15 minutes after its last | Wait for that attempt, restart the server, or run `shed users refresh` as a local admin |
| Log: `Users: enrolment unusable (<file>: <check>); only the local admins can log in` | `enrolment.json` or its directory failed a check (owner, mode, symbolic link, size, content) | Put the file right (owned by the server's user, mode `0600`, in a `0700` directory), or [enrol again](#enrolling-again) |
| Log: `directory: the account service refused this server's login: ...` | The service does not recognise this server's key or assertion | Check the host's clock, then the server on the service; enrol again if it was removed there |
| Log: `directory: pull failed: token: refused (HTTP 400) "invalid_scope": ...` | The service refuses a token request that names the scope `directory:read`, which the contract allows and shed-server sends. Every refresh fails; the list held stays until its trust time ends | Update the service to accept `scope=directory:read` |
| Log: `directory: SERVER DISABLED: ...` | The server is disabled on the service | A disabled server stays disabled: have a new code issued and [enrol again](#enrolling-again) |
| Log: `directory: WARNING: the account service's clock is ... from this server's; ...` | The clocks differ by more than 30 s | Fix the host's clock (NTP) |
| Log: `directory: kept copy not written: ...; a restart starts with only the local admins until a good pull` | The state directory cannot be written | Free space or fix permissions; the next good pull writes it again |
| `shed users status`: `Error: <server>: this server has no status route (it is older than 'shed users status')` | The server is older than the CLI | Upgrade the server |
| `the server issued a certificate for user "admin", but entry "shed" is held to user "pat"; nothing was saved.` | The entry's certificate was rejected (for example while the list is not in force) and `ssh` offered another user's key when the CLI enrolled again. Nothing was written | To stay `pat`, let the entry offer only `pat`'s key, then retry: `shed server update shed --identity-file ~/.ssh/id_ed25519` names it ([backup key](#if-the-everyday-entry-offers-the-backup-key)). To make the entry `admin`: `shed server update shed --refetch --allow-user-change` |
| `the server issued a certificate for SSH key "SHA256:...", but entry "shed" uses only its identity_file "..." (key SHA256:...); nothing was saved.` | The entry has an `identity_file`, and ssh also offered a key your `~/.ssh/config` names for the host (a `Host` or `Match` stanza, or `Host *`), which the server accepted. Nothing was written | Keep no other `IdentityFile` for that host in `~/.ssh/config`, or make sure the server accepts the entry's own key, then retry |
| `entry "shed": identity_file "/home/pat/.ssh/id_backup" cannot be used: ...` (`the file does not exist`, `it is not an absolute path`, `it contains #, ...`, `its public key cannot be learned without a passphrase ...`) | The entry's key file is missing or unreadable, its path breaks the rule (see [CLI › One SSH key per entry](../reference/cli.md#one-ssh-key-per-entry)), or its public key cannot be read without the passphrase. Nothing was sent and `ssh` was not run | Put the key back, or point the entry at its key where it is now (`shed server update shed --identity-file ~/.ssh/id_ed25519`), or clear it (`shed server update shed --identity-file ""`). For a key whose public half cannot be read, put its `.pub` file beside it |
| `entry "shed" cannot be used: it holds a control token, which does not say which SSH key it was issued for, and the entry uses only its identity_file "..."; no request was sent. ...` | The entry was enrolled with a control token (a server older than v0.10.0 in token mode) before its `identity_file` was set | Upgrade the server, then `shed server update shed --refetch` |
| `entry "shed" cannot be used: its certificate names no user (issued by a shed-server older than 0.10.0); no request was sent. ...` | The entry was enrolled against a shed-server older than 0.10.0 | `shed server update shed --refetch` (it prints `User: <name>`: check it is you) |
| `entry "shed" cannot be used: its certificate is for user "pat" but the entry records no user (...); no request was sent. ...` (or `records user "admin" (...)`) | A CLI older than 0.10.0 saved `config.yaml` and dropped the entry's `user`; it was edited by hand; or an enrolment was interrupted (killed between writing the certificate and saving `config.yaml`) or overlapped another `shed` process | `shed server update shed --refetch`; use a 0.10.0 CLI on every machine that shares the config |
| `the server issued a certificate that names no user: it runs a shed-server older than 0.10.0; nothing was saved. ...` | The server is older than 0.10.0 | Upgrade the server, then retry |
| `entry "shed" changed while this command ran; nothing was saved. Run it again.` | Another `shed` process changed the entry's user during a `--refetch` | Run the command again |
| `entry "shed" now records user "admin", but this command was using it as user "pat" (...); nothing was saved and this command's certificate is no longer used. Run the command again.` | Another `shed` process changed the entry's user (for example `--refetch --allow-user-change`) while this command re-enrolled | Run the command again; it uses the entry as it now is |
| `entry "shed": a client certificate update did not complete and the previous certificate could not be put back (...); its certificate files and its recorded user may now disagree. ...` | Writing a new certificate, or saving `config.yaml` after it, failed, and putting the old certificate back failed too (a full or read-only disk) | Fix the disk, then `shed server update shed --refetch` |
| `the server issued a certificate for user "pat", but entry "shed" could not be updated to record that user (...); the certificate was not used and nothing was kept. ...` | The entry recorded no user yet, and saving `config.yaml` (or writing the certificate files) failed, so the first user could not be recorded. Nothing is sent as a user the entry does not record | Make `config.yaml` and `~/.shed/creds/` writable, then run the command again |
| A tunnel stops: `the server issued a certificate for user "pat", but entry "shed" records no user, and this long-running command (a tunnel) never records one; ...` | The server gained a user list while a tunnel daemon was running for an entry that records no user. A daemon never takes up a user the entry does not record | `shed server update shed --refetch` (it prints `User: <name>`: check it is you), then `shed tunnels stop <shed>` and `shed tunnels start <shed>` |
| `entry "shed" could not be verified against its recorded user and identity (...); nothing was saved and this command's certificate is no longer used. ...` | During a re-enrolment the CLI could not lock the entry's credential files or read `config.yaml`, the entry was removed or changed meanwhile, or the key in its `identity_file` changed while the command ran | Fix what the message names (permissions, a damaged `config.yaml`, the `identity_file`), check the entry with `shed server list`, then run the command again |
| `shed server list` shows `refused` | One of the refusals above; its message is printed above the table | As for that message |
| `shed ssh-config --all` fails with `no configured server could be used: <n> refused (see above)` | No configured server answered, and at least one is refused; each refused server's message is printed above | As for that message |
