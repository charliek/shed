# Multi-user Server

A shed server can have **named users**. Each user's SSH keys are listed under
their name, each user sees and enters only their own sheds and snapshots, and
each user's credential requests go to one machine of theirs. An admin may also
list, stop and delete anyone's sheds, and uses what the host shares: host
directories, server mounts and the image store.

This guide adds a user list to an mtls server. The rules it relies on are in
[Security › Ownership](../reference/security.md#ownership), and every field is
in [Configuration › Users](../reference/configuration.md#users).

Users separate people who trust the host and its admins. They are not
isolation between hostile tenants: an admin controls the host directories,
mounts and images every shed runs on, a guest is root in its VM, and
Firecracker runs without its jailer.

## Before you start

- The server runs `auth.mode: mtls` (see [Security
  Configuration](security-configuration.md#case-3-networked-mtls-facing-the-network)).
  A user list is refused in open mode.
- Upgrade every machine that answers credential requests (shed-host-agent and
  the desktop app) to 0.10.0 or newer before the server: a server that pins
  refuses an older broker.
- Decide the **whole** list before the first start with it. On that start a
  server with exactly one user gives that user every shed that has no user; a
  server with several gives none away (see [Sheds with no
  user](#sheds-with-no-user)).

## Write the user list

`auth.users` replaces `auth.ssh`'s key sources. Move each GitHub login or key
from `auth.ssh` to the user it belongs to, and remove the `auth.ssh` source:
config load refuses both at once. `auth.ssh.github_refresh` and
`max_auth_tries` still apply.

```yaml
auth:
  mode: mtls
  users:
    alice:
      github: alice-gh                 # her keys on GitHub, pinned to the account id
      admin: true
    bob:
      github: bob-gh
      keys:
        - key: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA... bob@phone
          label: phone                 # keys listed by hand need a label
    carol:
      keys:                            # no GitHub login: keys only
        - key: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA... carol@laptop
          label: laptop
          expires: 2027-06-30          # accepted through that day, UTC
        - key: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA... ci
          label: ci
          script: true                 # automation: no credential bus
```

Rules config load enforces, each with a message naming the field:

- a user needs a `github` login, a key, or both;
- a key belongs to one user and is listed once, and a GitHub login belongs to
  one user (compared without case);
- a key is one plain OpenSSH public key line: no `authorized_keys` options,
  and not an SSH certificate;
- `admin` and `script` take `true` or `false`, unquoted;
- at least one admin has a GitHub login or a key that is not marked `script`;
- a user name (`^[a-z][a-z0-9-]{0,62}$`) may not also name a service client.

Check the file before restarting:

```bash
shed-server config-validate      # with sudo for /etc/shed/server.yaml
```

Restart the server. Its log names the users (`Users: alice, bob, carol (each
key belongs to one user; an unknown key is nobody's)`) and reports what it did
with existing sheds.

Each person then adds the server from their own machine with their own key, as
for any mtls server:

```bash
shed server add shed.example.com --name work
```

An existing client entry keeps working: its certificate now acts as the user
its key belongs to, from the next request.

## Admins

`admin: true` makes a user an admin. An admin:

- lists every shed, and may stop and delete any of them;
- may **not** enter another user's shed (start, exec, a shell, sftp, a
  forwarded port, sessions, reset, a snapshot of it), and is told so: `shed
  '<name>' belongs to another user; an admin can list, stop and delete it but
  not enter it`;
- may enter a [service client](orchestrated-sheds.md)'s shed;
- uses what is admin-only (below), assigns sheds with no user, and runs `shed
  users refresh`;
- may see and move another user's credential source with `--user`.

"An admin cannot enter" guards against a stolen admin credential. It does not
guard against the admin, who controls what every shed runs on.

A user whose `admin` is removed loses those rights when the server restarts
with the new config, on their next request or SSH channel. A shed with host
directories or server mounts that such a user made while an admin stays
theirs to stop and delete, but they can no longer start or enter it.

## What is admin-only

A user who is not an admin is refused, with `403 FORBIDDEN` (`admin only:
<what>`):

| Admin-only | Where |
|---|---|
| Host directories | `shed create --local-dir`, `--add-dir` |
| Server mounts | `shed create --mounts <names>`; for a user who is not an admin the default gives none |
| Images other than the server default or an `image_aliases` name | `shed create --image` |
| The image store | `shed image pull`, `tag`, `push`, `rm`, `prune` |
| `shed system prune` and egress profile changes | |

The server's `env_file` variables reach only an admin's `repo` clone; a user
who is not an admin is not refused, the clone runs without them. `shed system
df` shows such a user their own sheds and snapshots only.

## Sheds with no user

Sheds and snapshots made before the server had a user list have no user. What
happens to them is decided **once**, on the first start with a user list:

| User list at that start | Sheds and snapshots with no user |
|---|---|
| Exactly one user | Become that user's, each one logged. A shed with host directories or server mounts becomes theirs only if that user is an admin. This is not undone if users are added later. |
| Several users | Nothing is given away. Only an admin sees such a shed; an admin may view, stop and delete it, and nobody enters it until it is assigned. |

The step is recorded by a marker, `{state_dir}/users-upgrade.json`, so it
never runs again: cutting a list from two users to one later does not hand the
unassigned sheds to whoever is left. With one user, the marker names that
user before anything is given away. A start that does not finish (a shed or
snapshot it could not read or stamp) leaves the step pending for that user,
and the next start resumes it only while that same user is still the only
one listed; with any other list it gives nothing more away, records the step
as done, and logs that the unfinished step was abandoned, so the sheds still
without a user need `shed assign`. A start with **no** user list removes the
marker, so removing the list and adding it back repeats the step. At every
start the log reports how many sheds have no user and how many of those carry
host resources (with their names at `log_level: debug`).

So the simplest path for a server one person used before: **start once with
only yourself listed**, which gives you every existing shed, then add the
others.

### Assigning a shed

An admin gives a shed with no user to a user with `shed assign`:

```bash
shed assign old-box bob
# Shed old-box on work now belongs to user bob.
```

It is refused for:

- a shed that already has a user or a service client: `409
  SHED_ALREADY_ASSIGNED`. A shed is never moved between users;
- a shed that carries host directories or server mounts: `409
  SHED_HOST_RESOURCES`, because the user would get those too. On Firecracker
  and VZ a shed stored before mount selection existed counts as carrying
  server mounts, so on a server that goes straight to several users no old
  shed can be assigned. Start once with one user, or recreate the shed with
  `--mounts none` and without host directories.

Snapshots cannot be assigned: a snapshot with no user stays usable and
deletable by an admin only.

## Keys

**GitHub logins** are pinned to their numeric account id at the first fetch
(in `{state_dir}/github_pins.json`); a login that later names another account,
or none, contributes no keys. New keys on the account are accepted at the next
fetch, every `auth.ssh.github_refresh` (default 1h). To fetch now:

```bash
shed users refresh
```

It prints one row per login, with what was added and removed (see [CLI › shed
users refresh](../reference/cli.md#shed-users-refresh)). The server calls
`api.github.com` for each login at start, on each refresh and on this command,
so a firewall must allow it. When GitHub cannot be reached the last known keys
stay and no new key is accepted. To follow an account that was deleted and
created again on purpose, set `github_id` to the new account's id.

**Keys listed by hand** need a `label`, which names the key in logs. An
`expires` date stops the key at the end of that day, UTC: it is then refused
at the SSH and TLS handshakes and on every request (connections already open
stay open). Remove a key from the list and restart to revoke it.

**Script keys** (`script: true`) are for automation acting as the user: a CI
job, a scheduled task. A script key reaches the user's sheds like any of their
keys, but it cannot take a `credentials` certificate, subscribe to or answer
on the credential bus, or see or move the user's credential source. So
automation never answers, or receives, the user's credential requests.

## One machine answers for each user

Each user's credential requests (SSH signatures, AWS and Docker credentials)
go to one machine of theirs: their **credential source** on this server. The
first of their machines whose shed-host-agent or desktop app connects becomes
it. Any other machine of that user is refused, shows the source's name, and
keeps retrying; it never takes over by itself.

On an existing server's first start with this version there are no sources,
so the first machine to connect for each user wins: start the machine you
want first, or move it afterwards. On a server that had a source before its
user list, a single configured user keeps it; with several users it is
dropped.

See which machine it is, and move it, from any machine:

```bash
shed source status
shed source ls
shed source use desktop        # by name, id, or an id prefix of 8+ hex digits
```

or from the desktop app on the machine you want, with "Use this machine". The
old machine stops answering at once, and its requests in flight are dropped.
The new one answers when its broker next connects (within about 30 s, or at
once from the desktop app). Nothing fails over: while the source is off or
asleep, its user's credential requests get no answer.

An admin can see and move another user's source with `--user bob`, choosing
among bob's own machines. The details are in [Security › One machine answers
for each user](../reference/security.md#one-machine-answers-for-each-user).

## Removing a user

Remove the user from `auth.users` and restart. Their keys and certificates
stop working at once, and their credential-source records are dropped. Their
sheds and snapshots keep their user, so only an admin sees them (to view,
stop and delete); a user added again under the same name gets them back.
Renaming a user strands their sheds the same way, since a shed cannot be moved
between users.
