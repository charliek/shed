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
"""

from __future__ import annotations

from typing import Optional

from .identity import DEV_USER, ITEST_SOURCES, LOCAL_ADMIN

CLIENT_MEMBERS = frozenset({"kind", "name", "serial"})
PERSON_MEMBERS = frozenset({"kind", "user", "implicit", "admin", "script", "subject", "serial"})

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
