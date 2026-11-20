"""The identity shapes of shed-server's audit log (`audit.jsonl`), for the
tests that read it (`test_service_client.py`).

Each line's `identity` says who acted (docs/reference/configuration.md,
"Audit log"):

  - a service client: `{"kind": "client", "name", "serial"}`, nothing else;
  - a person: `{"kind": "user", "user", "admin", "script", "subject",
    "serial"}`, with `admin` and `script` always present as booleans; the one
    user of a server with no user list carries `implicit: true`, `admin: true`
    and no `user`;
  - the server itself: `{"kind": "system"}`.

There is no `admin` kind. The helpers return a list of what is wrong (empty
when the identity is well formed), so an assertion can print every problem.
`developer_audit_user` says which person the developer's own entry is on the
dev server a run targets, so a test can expect that exact name (or the
implicit user) rather than any person.

An `ssh` line records a person's entry into a service client's shed over SSH
(`{"event": "ssh", "identity", "remote", "shed", "owner", "channel"}`, plus
`port` for a tunnel): `ssh_lines_by` picks one person's lines about a shed,
and `ssh_entry_problems` says what is wrong with one. The line never carries
the command, so any member beyond those is a problem.

A `connect` line records a tunnel a person opened into a service client's
shed through the HTTPS Connect API (`shed tunnels`): `{"event": "connect",
"identity", "remote", "shed", "owner", "port"}`. It carries no `request_id`
(the server's request id can come from a caller's `X-Request-Id` header).
`connect_lines_by` and `connect_entry_problems` are its two helpers. Every
line carries `ts`, its RFC 3339 UTC time, first, in exactly the shape the
server writes (`TS_SHAPE`).
"""

from __future__ import annotations

import re
from datetime import datetime
from typing import Optional

from .identity import DEV_USER, ITEST_SOURCES, LOCAL_ADMIN

CLIENT_MEMBERS = frozenset({"kind", "name", "serial"})
PERSON_MEMBERS = frozenset({"kind", "user", "implicit", "admin", "script", "subject", "serial"})
SSH_MEMBERS = frozenset({"ts", "event", "identity", "remote", "shed", "owner", "channel", "port"})
SSH_CHANNELS = ("shell", "exec", "sftp", "tunnel")
CONNECT_MEMBERS = frozenset({"ts", "event", "identity", "remote", "shed", "owner", "port"})

# The `ts` the server writes: Go's time.RFC3339Nano of a UTC time, so the
# date and time, then a fraction of one to nine digits with its trailing
# zeros cut (none when it is zero), then `Z`. Matched against the whole
# string.
TS_SHAPE = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{0,8}[1-9])?Z")

# `person_identity_problems(user=...)` left out: any person, named or implicit.
_ANY_PERSON = object()


def developer_audit_user(source: str, with_user_list: bool) -> Optional[str]:
    """The user name the dev server's audit log gives the developer's own CLI
    entry (the `admin` client of test_service_client.py), or None when it is
    the server's one implicit user, which has no name:

      - the directory source (`SHED_ITEST_SOURCE=directory`): `localadmin`,
        the local admin `render-directory` puts the developer's key under;
      - the config source on a server started with the itest user list
        (`SHED_ITEST_USERS`): `dev`, the admin `with_users` moves the
        config's `auth.ssh` key sources under;
      - the config source on a server with no user list: None."""
    if source not in ITEST_SOURCES:
        raise ValueError(f"source {source!r}: expected one of {', '.join(ITEST_SOURCES)}")
    if source == "directory":
        return LOCAL_ADMIN
    return DEV_USER if with_user_list else None


def client_identity_problems(identity) -> list[str]:
    """What is wrong with `identity` as a service client's identity."""
    if not isinstance(identity, dict):
        return [f"identity is not an object: {identity!r}"]
    problems = []
    if identity.get("kind") != "client":
        problems.append(f"kind is {identity.get('kind')!r}, not 'client'")
    if not isinstance(identity.get("name"), str) or not identity.get("name"):
        problems.append("no client name")
    extra = sorted(set(identity) - CLIENT_MEMBERS)
    if extra:
        problems.append(f"a client's identity carries {extra}")
    return problems


def person_identity_problems(identity, *, admin: Optional[bool] = None, user=_ANY_PERSON) -> list[str]:
    """What is wrong with `identity` as a person's identity (kind `user`).
    With `admin` set, the identity's `admin` flag must equal it. With `user` a
    name, the identity must be that named user, never the implicit user; with
    `user=None`, it must be the implicit user (`implicit: true`, no name).
    Left out, any well-formed person passes."""
    if not isinstance(identity, dict):
        return [f"identity is not an object: {identity!r}"]
    problems = []
    if identity.get("kind") != "user":
        problems.append(f"kind is {identity.get('kind')!r}, not 'user'")
    for flag in ("admin", "script"):
        if not isinstance(identity.get(flag), bool):
            problems.append(f"{flag} is {identity.get(flag)!r}, not true or false")
    if "implicit" in identity and identity["implicit"] is not True:
        problems.append(f"implicit is {identity['implicit']!r}: it is true or absent")
    if identity.get("implicit") is True:
        if "user" in identity:
            problems.append("the implicit user carries a user name")
        if identity.get("admin") is not True:
            problems.append("the implicit user is not an admin")
    elif not isinstance(identity.get("user"), str) or not identity.get("user"):
        problems.append("a named person has no user name")
    for member in ("subject", "serial"):
        if member in identity and (not isinstance(identity[member], str) or not identity[member]):
            problems.append(f"{member} is {identity[member]!r}: a non-empty string or absent")
    extra = sorted(set(identity) - PERSON_MEMBERS)
    if extra:
        problems.append(f"a person's identity carries {extra}")
    if admin is not None and identity.get("admin") is not admin:
        problems.append(f"admin is {identity.get('admin')!r}, want {admin!r}")
    if user is not _ANY_PERSON:
        implicit = identity.get("implicit") is True
        if user is None:
            if not implicit:
                problems.append(
                    f"want the implicit user (no user list), got user {identity.get('user')!r} "
                    f"with implicit {identity.get('implicit')!r}"
                )
        elif not isinstance(user, str) or not user:
            raise ValueError(f"user={user!r}: a user name, or None for the implicit user")
        elif implicit:
            problems.append(f"want user {user!r}, got the implicit user (no name)")
        elif identity.get("user") != user:
            problems.append(f"user is {identity.get('user')!r}, want {user!r}")
    return problems


def _is_person(identity, user: Optional[str]) -> bool:
    """Whether `identity` is the person `user` names: that named user, or,
    with `user=None`, the implicit user."""
    if not isinstance(identity, dict) or identity.get("kind") != "user":
        return False
    if user is None:
        return identity.get("implicit") is True
    return identity.get("implicit") is not True and identity.get("user") == user


def _lines_by(lines, event: str, *, shed: str, user: Optional[str]) -> list[dict]:
    if user is not None and (not isinstance(user, str) or not user):
        raise ValueError(f"user={user!r}: a user name, or None for the implicit user")
    return [
        l for l in lines
        if isinstance(l, dict) and l.get("event") == event and l.get("shed") == shed
        and _is_person(l.get("identity"), user)
    ]


def ssh_lines_by(lines, *, shed: str, user: Optional[str]) -> list[dict]:
    """The `ssh` lines in `lines` (parsed audit.jsonl objects) about `shed`
    written for the person `user` names: that named user, or, with
    `user=None`, the implicit user. Other people's lines, other sheds' and
    other events are left out."""
    return _lines_by(lines, "ssh", shed=shed, user=user)


def connect_lines_by(lines, *, shed: str, user: Optional[str]) -> list[dict]:
    """The `connect` lines in `lines` about `shed` written for the person
    `user` names (as `ssh_lines_by`)."""
    return _lines_by(lines, "connect", shed=shed, user=user)


def _ts_problems(line: dict) -> list[str]:
    """The line's `ts`: the server writes an RFC 3339 UTC time first on every
    line, in exactly the shape `TS_SHAPE` (the whole string), and a real
    date and time."""
    ts = line.get("ts")
    if not isinstance(ts, str) or not ts:
        return [f"ts is {ts!r}: every line carries its time"]
    if not TS_SHAPE.fullmatch(ts):
        return [f"ts is {ts!r}: not the RFC 3339 UTC time the server writes"]
    try:
        datetime.strptime(ts[:19], "%Y-%m-%dT%H:%M:%S")
    except ValueError:
        return [f"ts is {ts!r}: not a real date and time"]
    return []


def _port_problems(got, want: Optional[int]) -> list[str]:
    if not isinstance(got, int) or isinstance(got, bool) or not 1 <= got <= 65535:
        return [f"port is {got!r}: the line names the shed's port"]
    if want is not None and got != want:
        return [f"port is {got!r}, want {want!r}"]
    return []


def connect_entry_problems(line, *, shed: str, owner: str, admin: Optional[bool] = None,
                           user=_ANY_PERSON, port: Optional[int] = None) -> list[str]:
    """What is wrong with `line` as the `connect` line of a person's tunnel
    into `shed`, a shed of the service client `owner`, through the HTTPS
    Connect API. The identity must be a well-formed person
    (`person_identity_problems`, with `admin` and `user` as there); on an
    mtls server it carries the certificate's `subject` and `serial`, which
    open mode has not, so neither is required. `remote` is the peer's address
    and `port` the shed's port (equal to `port` when that is given). Any other
    member is a problem, `request_id` included: the line carries nothing a
    caller supplied."""
    if not isinstance(line, dict):
        return [f"line is not an object: {line!r}"]
    problems = []
    if line.get("event") != "connect":
        problems.append(f"event is {line.get('event')!r}, not 'connect'")
    problems += _ts_problems(line)
    extra = sorted(set(line) - CONNECT_MEMBERS)
    if extra:
        problems.append(f"a connect line carries {extra}")
    for member, want in (("shed", shed), ("owner", owner)):
        if line.get(member) != want:
            problems.append(f"{member} is {line.get(member)!r}, want {want!r}")
    remote = line.get("remote")
    if not isinstance(remote, str) or not remote:
        problems.append(f"remote is {remote!r}: a connect line names it")
    problems += _port_problems(line.get("port"), port)
    problems += person_identity_problems(line.get("identity"), admin=admin, user=user)
    return problems


def ssh_entry_problems(line, *, shed: str, owner: str, channel: str, admin: Optional[bool] = None,
                       user=_ANY_PERSON, port: Optional[int] = None) -> list[str]:
    """What is wrong with `line` as the `ssh` line of a person's entry into
    `shed`, a shed of the service client `owner`, through `channel` (`shell`,
    `exec`, `sftp` or `tunnel`). The identity must be a well-formed person
    (`person_identity_problems`, with `admin` and `user` as there) with the
    key's fingerprint as `subject` and no `serial` (an SSH login presents no
    certificate); `remote` is the peer's address; `port` is present for a
    tunnel only (and equal to `port` when that is given). Any other member is a
    problem: the line never carries the command."""
    if channel not in SSH_CHANNELS:
        raise ValueError(f"channel={channel!r}: expected one of {', '.join(SSH_CHANNELS)}")
    if not isinstance(line, dict):
        return [f"line is not an object: {line!r}"]
    problems = []
    if line.get("event") != "ssh":
        problems.append(f"event is {line.get('event')!r}, not 'ssh'")
    problems += _ts_problems(line)
    extra = sorted(set(line) - SSH_MEMBERS)
    if extra:
        problems.append(f"an ssh line carries {extra}")
    for member, want in (("shed", shed), ("owner", owner), ("channel", channel)):
        if line.get(member) != want:
            problems.append(f"{member} is {line.get(member)!r}, want {want!r}")
    remote = line.get("remote")
    if not isinstance(remote, str) or not remote:
        problems.append(f"remote is {remote!r}: the peer's address")
    if channel == "tunnel":
        problems += _port_problems(line.get("port"), port)
    elif "port" in line:
        problems.append(f"the {channel} line carries port {line['port']!r}: only a tunnel's has one")
    identity = line.get("identity")
    problems += person_identity_problems(identity, admin=admin, user=user)
    if isinstance(identity, dict):
        if not isinstance(identity.get("subject"), str) or not identity.get("subject"):
            problems.append("the identity has no subject (the key's fingerprint)")
        if "serial" in identity:
            problems.append(f"the identity carries serial {identity['serial']!r}: an SSH login has none")
    return problems
