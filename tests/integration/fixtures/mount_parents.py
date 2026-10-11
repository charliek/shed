"""Server-free classifier for the #410 mount-parent test (`test_mounts.py`).

The live test asks the guest two things — every mount (`findmnt -rn -o
TARGET,FSTYPE`) and, in one pass, the kind, device and owner of `/`, HOME and
every candidate parent (`PATH_FACTS_SH`) — and this module decides which
directories the mount prelude was responsible for. Kept pure so
`test_fixture_units.py` can pin it without a server: a wrong classification
here sends the test's `sudo chown root:root` into a HOST directory.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Optional

HOME = "/home/shed"

_FINDMNT_ESCAPE = re.compile(r"\\x([0-9a-fA-F]{2})")

# Prints `<path>\t<link|dir|other|missing>\t<st_dev> <user:group>` per path
# argument (an empty third field when stat fails).
PATH_FACTS_SH = r"""
for p in "$@"; do
  if [ -L "$p" ]; then k=link; elif [ -d "$p" ]; then k=dir; elif [ -e "$p" ]; then k=other; else k=missing; fi
  printf '%s\t%s\t%s\n' "$p" "$k" "$(stat -c '%d %U:%G' -- "$p" 2>/dev/null)"
done
"""


@dataclass(frozen=True)
class PathFact:
    kind: str  # link | dir | other | missing
    dev: str  # st_dev, "" when stat failed
    owner: str  # "user:group", "" when stat failed


@dataclass(frozen=True)
class MountParents:
    deep: list[str]  # 9p/virtiofs targets >= 2 levels below HOME
    eligible: list[str]  # the parents the prelude creates or repairs
    home_blocker: Optional[str]  # why HOME itself is off limits, or None


def parse_findmnt(stdout: str) -> list[tuple[str, str]]:
    """`findmnt -rn -o TARGET,FSTYPE` output as [(target, fstype)]."""
    mounts = []
    for line in stdout.splitlines():
        target, _, fstype = line.partition(" ")
        if fstype:
            # -r hex-escapes spaces and other unsafe bytes in TARGET.
            target = _FINDMNT_ESCAPE.sub(lambda m: chr(int(m.group(1), 16)), target)
            mounts.append((target, fstype.strip()))
    return mounts


def parse_path_facts(stdout: str) -> dict[str, PathFact]:
    """`PATH_FACTS_SH` output as {path: PathFact}."""
    facts = {}
    for line in stdout.splitlines():
        path, kind, stat = line.rsplit("\t", 2)  # a tab inside the path stays in it
        dev, _, owner = stat.partition(" ")
        facts[path] = PathFact(kind, dev, owner)
    return facts


def home_parts(target: str) -> list[str]:
    """The components of `target` below HOME ([] when it is not below HOME)."""
    if not target.startswith(HOME + "/"):
        return []
    return target[len(HOME) + 1 :].split("/")


def deep_mounts(mounts: list[tuple[str, str]]) -> list[str]:
    """9p/virtiofs mount targets at least two levels below HOME, sorted."""
    return sorted(
        t for t, fs in mounts if fs in ("9p", "virtiofs") and len(home_parts(t)) >= 2
    )


def fact_paths(deep: list[str]) -> list[str]:
    """Every path the classifier needs facts for: `/`, HOME, and each
    intermediate directory strictly between HOME and a deep target."""
    parents = {
        f"{HOME}/" + "/".join(parts[:i])
        for t in deep
        for parts in [home_parts(t)]
        for i in range(1, len(parts))
    }
    return ["/", HOME, *sorted(parents)]


def classify(mounts: list[tuple[str, str]], facts: dict[str, PathFact]) -> MountParents:
    """Which intermediate parents of the deep mounts the prelude owns.

    HOME itself must be a real directory on `/`'s filesystem and not a
    mountpoint; otherwise it is inside a host mount (one at HOME or above it)
    and nothing below it is the prelude's — `home_blocker` says why and
    `eligible` is empty. Below HOME, each target's walk stops at the first
    component that is a mountpoint, not a real directory, or on a different
    device than `/` (inside a mount findmnt did not report as one of these
    paths); only the components before that stop are eligible.
    """
    deep = deep_mounts(mounts)
    mountpoints = {t for t, _ in mounts}
    missing = PathFact("missing", "", "")
    root, home = facts.get("/", missing), facts.get(HOME, missing)
    blocker = None
    if not root.dev:
        blocker = "the device of / could not be read"
    elif HOME in mountpoints:
        blocker = f"{HOME} is itself a mountpoint"
    elif home.kind != "dir":
        blocker = f"{HOME} is a {home.kind}, not a directory"
    elif home.dev != root.dev:
        blocker = f"{HOME} is on device {home.dev or '?'}, not /'s {root.dev}: it is inside a mount"
    if blocker is not None:
        return MountParents(deep, [], blocker)

    eligible: list[str] = []
    for t in deep:
        parts = home_parts(t)
        for i in range(1, len(parts)):
            p = f"{HOME}/" + "/".join(parts[:i])
            f = facts.get(p, missing)
            if p in mountpoints or f.kind != "dir" or f.dev != root.dev:
                break
            if p not in eligible:
                eligible.append(p)
    return MountParents(deep, eligible, None)
