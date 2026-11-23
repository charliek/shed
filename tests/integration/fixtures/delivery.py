"""Provisioning by delivery, for the integration suite (`test_delivery.py`).

A shed-server asks a person's credential source (their bound machine) for a
bundle of files and environment values at create and at `shed provision`, and
writes it into the shed in two stages around the clone (plan 039;
`tests/provision-contract/v1/CONTRACT.md`). The suite plays the machine with
the contract's reference source, `bin/test/shed-provision-ref` (`make
provision-ref`), run as a subprocess (`RefSource`):

  - in a throwaway HOME (the test's tmp dir, 0700) with a minimal
    environment: no SSH agent, no proxy variables, nothing of the
    developer's;
  - with a throwaway credentials certificate, enrolled over `_bootstrap` as
    the itest user it stands for (`Identity.enrol`), and the machine id and
    name of a synthetic machine (`busclient.FakeMachine`) that the test makes
    the user's source first, through the routes a person has
    (`busclient.ensure_source`: `GET /api/sources`, `POST /api/sources/use`);
  - on loopback to the server under test only (`loopback_endpoint`): the
    server's own HTTPS port at 127.0.0.1 when it is on this host (its
    certificate always names 127.0.0.1), or else an `ssh -L` forward to
    127.0.0.1 on the server's host. The server's certificate, fetched and
    checked against the pin, is the only CA the source trusts.

The reference source prints one JSON line per event (stream, refused,
request, invalid_request, answer, dropped, end); `RefSource` reads them on a
thread so a case can wait for a request, see how its answer was judged, and
stop the source mid-request.

The pure parts have unit tests in `test_fixture_units.py` (`make
test-integration-units`): canned items and the answer document, the source's
command line and event lines, the record and its steps, the warning texts
(checked against the Go source), the sentinel forms and the scans, the
refuse-fixture selection, the server's file locations and the image gate.
Nothing here runs at import or needs a server.
"""

from __future__ import annotations

import base64
import binascii
import copy
import json
import os
import re
import secrets
import shlex
import shutil
import socket
import stat
import subprocess
import threading
import time
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Callable, Iterable, Iterator, Mapping, Optional, Sequence, Union

REPO_ROOT = Path(__file__).resolve().parents[3]
REF_SOURCE_BIN = REPO_ROOT / "bin" / "test" / "shed-provision-ref"
CONTRACT_DIR = REPO_ROOT / "tests" / "provision-contract" / "v1"
REFUSE_DIR = CONTRACT_DIR / "fixtures" / "answers" / "refuse"

# The shed user's home, where `base: home` items land.
HOME = "/home/shed"
# Where stage 1's env items go (internal/delivery/writer.go: EnvFileName),
# relative to the home directory.
ENV_FILE = ".config/environment.d/50-shed-delivery.conf"

# The server's own words (checked against the Go source by
# test_fixture_units.py): the warning a failed delivery prints
# (internal/delivery/lifecycle.go: Warning), its form for an image too old to
# take a delivery (which no `shed provision` finishes), and the one `shed
# start` prints per expired item (cmd/shed/provision.go: warnExpiredItems).
WARNING_FORMAT = "Credentials were not delivered ({failure}). Run: shed provision {shed}"
IMAGE_TOO_OLD_FORMAT = "Credentials were not delivered ({failure}). Recreate the shed from a current image."
EXPIRED_FORMAT = "Warning: delivered item {item} has expired. Run: shed provision {shed}"
# The feature tokens a server with delivery advertises (internal/api/overview.go).
FEATURES = ("delivery", "activity")
STEP_NAMES = ("stage1", "clone", "stage2", "install")

# The contract's item rules, as far as a canned answer is checked here (a
# canned answer is sent as it is, so a mistake in a test would otherwise be a
# refusal that looks like a server bug).
NAME_RE = re.compile(r"[a-z0-9][a-z0-9._-]{0,63}")
VARIABLE_RE = re.compile(r"[A-Z_][A-Z0-9_]{0,127}")
FINGERPRINT_RE = re.compile(r"[A-Za-z0-9_-]{1,64}")
MODES = ("0400", "0600", "0644")
MAX_FILE_BYTES = 1 << 20
MAX_VALUE_BYTES = 8192
MAX_PATH_BYTES = 255
REFUSED_VARIABLES = frozenset({
    "PATH", "HOME", "USER", "LOGNAME", "SHELL", "IFS", "ENV", "BASH_ENV",
    "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
})
REFUSED_HOME_TARGETS = (".ssh/authorized_keys", ".ssh/authorized_keys2", ".ssh/known_hosts")


# ----------------------------------------------------------------------------
# The image gate
# ----------------------------------------------------------------------------


def delivery_image_backends(environ: Mapping[str, str] = os.environ) -> set[str]:
    """`SHED_IMAGE_HAS_DELIVERY`: the backends (`vz`, `fc`, `remote`) whose
    image was rebuilt with this branch's agent (protocol 2, `shed-agent
    apply-items`, the user environment directory) and parcel/envsecrets in
    `full`. Unset for plain `make test-integration`: published images have
    none of it."""
    return {b.strip() for b in environ.get("SHED_IMAGE_HAS_DELIVERY", "").split(",") if b.strip()}


def backend_key(backend: str) -> str:
    """The gate's name for a backend: `firecracker` is `fc`."""
    return "fc" if backend == "firecracker" else backend


# ----------------------------------------------------------------------------
# Canned items and the answer document
# ----------------------------------------------------------------------------


def _printable(s: str) -> bool:
    """Go's unicode.IsPrint, near enough for a test's own strings: no control
    or format character, and the only space allowed is U+0020."""
    return all(ch == " " or (ch.isprintable() and not ch.isspace()) for ch in s)


def path_problems(path: str) -> list[str]:
    """What breaks the contract's path rules in `path`, a relative canonical
    path (empty when it is one)."""
    problems = []
    if not path:
        return ["empty path"]
    if len(path.encode()) > MAX_PATH_BYTES:
        problems.append(f"longer than {MAX_PATH_BYTES} bytes")
    if path.startswith("/") or path.endswith("/"):
        problems.append("a leading or trailing slash")
    if "\\" in path:
        problems.append("a backslash")
    if not _printable(path):
        problems.append("a character that is not printable")
    for seg in path.split("/"):
        if seg in ("", ".", ".."):
            problems.append(f"a segment {seg!r}")
            break
    return problems


def _fingerprint(name: str) -> str:
    return re.sub(r"[^A-Za-z0-9_-]", "-", f"fp-{name}")[:64]


def file_item(name: str, path: str, content: bytes, *, mode: str = "0600", stage: int = 1,
              base: str = "home", provider: str = "files", fingerprint: Optional[str] = None,
              expires_at: Optional[str] = None) -> dict:
    """One `kind: file` item as the answer carries it (content base64)."""
    item = {
        "name": name, "provider": provider, "kind": "file", "stage": stage, "base": base,
        "path": path, "mode": mode, "content": base64.b64encode(content).decode("ascii"),
        "fingerprint": fingerprint if fingerprint is not None else _fingerprint(name),
    }
    if expires_at is not None:
        item["expires_at"] = expires_at
    problems = item_problems(item)
    if problems:
        raise ValueError(f"file item {name!r}: {'; '.join(problems)}")
    return item


def env_item(name: str, variable: str, value: str, *, provider: str = "api-keys",
             fingerprint: Optional[str] = None, expires_at: Optional[str] = None) -> dict:
    """One `kind: env` item (stage 1 always; no stage member)."""
    item = {
        "name": name, "provider": provider, "kind": "env", "variable": variable, "value": value,
        "fingerprint": fingerprint if fingerprint is not None else _fingerprint(name),
    }
    if expires_at is not None:
        item["expires_at"] = expires_at
    problems = item_problems(item)
    if problems:
        raise ValueError(f"env item {name!r}: {'; '.join(problems)}")
    return item


def item_problems(item: Mapping) -> list[str]:
    """What breaks the contract in one canned item, as far as this checks."""
    problems = []
    if not NAME_RE.fullmatch(str(item.get("name", ""))):
        problems.append(f"name {item.get('name')!r} is not [a-z0-9][a-z0-9._-]{{0,63}}")
    if not NAME_RE.fullmatch(str(item.get("provider", ""))):
        problems.append(f"provider {item.get('provider')!r} is not in the name alphabet")
    if not FINGERPRINT_RE.fullmatch(str(item.get("fingerprint", ""))):
        problems.append(f"fingerprint {item.get('fingerprint')!r} is not 1 to 64 of [A-Za-z0-9_-]")
    kind = item.get("kind")
    if kind == "file":
        if item.get("stage") not in (1, 2):
            problems.append(f"stage {item.get('stage')!r} is not 1 or 2")
        if item.get("base") not in ("home", "project"):
            problems.append(f"base {item.get('base')!r} is not home or project")
        if item.get("base") == "project" and item.get("stage") != 2:
            problems.append("a project item is stage 2")
        if item.get("mode") not in MODES:
            problems.append(f"mode {item.get('mode')!r} is not one of {', '.join(MODES)}")
        path = str(item.get("path", ""))
        problems += [f"path {path!r}: {p}" for p in path_problems(path)]
        if item.get("base") == "home" and (path in REFUSED_HOME_TARGETS
                                            or path.startswith(".config/environment.d/")
                                            or path == ".config/environment.d"):
            problems.append(f"path {path!r} is a refused target")
        try:
            raw = base64.b64decode(str(item.get("content", "")), validate=True)
        except (binascii.Error, ValueError):
            problems.append("content is not base64")
        else:
            if len(raw) > MAX_FILE_BYTES:
                problems.append("content over 1 MiB")
    elif kind == "env":
        variable = str(item.get("variable", ""))
        if not VARIABLE_RE.fullmatch(variable):
            problems.append(f"variable {variable!r} is not [A-Z_][A-Z0-9_]{{0,127}}")
        elif variable in REFUSED_VARIABLES or variable.startswith(("LD_", "SHED_")):
            problems.append(f"variable {variable!r} is refused")
        value = str(item.get("value", ""))
        if not 1 <= len(value.encode()) <= MAX_VALUE_BYTES:
            problems.append("value is not 1 to 8192 bytes")
        if value[:1] == " " or value[-1:] == " ":
            problems.append("value starts or ends with a space")
        if not _printable(value):
            problems.append("value has a character that is not printable")
    else:
        problems.append(f"kind {kind!r} is not file or env")
    return problems


def canned(items: Sequence[Mapping], problems: Sequence[Mapping] = ()) -> dict:
    """The reference source's `--items` document: `{"items", "problems"}`."""
    return {"items": [dict(i) for i in items], "problems": [dict(p) for p in problems]}


def item_target(item: Mapping) -> str:
    """An item's target as the record keeps it: the path, or the variable."""
    return str(item["variable"] if item.get("kind") == "env" else item["path"])


def item_content(item: Mapping) -> bytes:
    """A file item's content, decoded."""
    return base64.b64decode(item["content"])


def item_shed_path(item: Mapping, project_dir: Optional[str]) -> str:
    """Where a file item lands in the shed (absolute)."""
    if item.get("base") == "project":
        if not project_dir:
            raise ValueError(f"item {item.get('name')!r} is a project item and the shed has no project directory")
        return project_dir.rstrip("/") + "/" + item["path"]
    return f"{HOME}/{item['path']}"


def env_file_text(items: Sequence[Mapping]) -> str:
    """The env file the writer makes from an answer's env items
    (cmd/shed-agent/applyitems.go: writeEnvFile): one `VARIABLE=value` line
    per env item, in the answer's order, values byte for byte."""
    return "".join(f"{i['variable']}={i['value']}\n" for i in items if i.get("kind") == "env")


# ----------------------------------------------------------------------------
# The record (a shed document's `delivery`)
# ----------------------------------------------------------------------------


def steps_of(record: Mapping) -> dict[str, tuple[Optional[str], Optional[str]]]:
    """Each step's `(state, reason)` (reason None when absent)."""
    steps = (record or {}).get("steps") or {}
    out = {}
    for name in STEP_NAMES:
        s = steps.get(name) or {}
        out[name] = (s.get("state"), s.get("reason"))
    return out


def step_problems(record: Mapping, want: Mapping[str, tuple]) -> list[str]:
    """How the record's steps differ from `want` ({step: (state, reason or
    None)}); a step `want` leaves out is not checked."""
    got = steps_of(record)
    return [f"{name}: {got[name]}, want {tuple(w)}" for name, w in want.items() if got.get(name) != tuple(w)]


def expected_record_item(item: Mapping, *, result: str = "ok") -> dict:
    """What the record keeps for one answer item: never its content or
    value."""
    out = {
        "name": item["name"], "provider": item["provider"], "kind": item["kind"],
        "stage": item.get("stage", 1) if item["kind"] == "file" else 1,
        "target": item_target(item), "fingerprint": item["fingerprint"], "result": result,
    }
    if item["kind"] == "file":
        out["base"] = item["base"]
    if "expires_at" in item:
        out["expires_at"] = item["expires_at"]
    return out


_RECORD_ITEM_MEMBERS = frozenset({"name", "provider", "stage", "kind", "base", "target", "fingerprint",
                                  "expires_at", "written_at", "result"})


def record_item_problems(record: Mapping, items: Sequence[Mapping], *,
                         results: Optional[Mapping[str, str]] = None) -> list[str]:
    """How the record's items differ from what `items` (an answer's) should
    leave: each named once, every member equal, `written_at` present exactly
    when the result is `ok`, and nothing else in the item (never a value or a
    content). `results` gives an item's expected result by name (default
    `ok`)."""
    results = dict(results or {})
    got = {}
    problems = []
    for ri in (record or {}).get("items") or []:
        name = ri.get("name")
        if name in got:
            problems.append(f"item {name!r} is in the record twice")
        got[name] = ri
        extra = sorted(set(ri) - _RECORD_ITEM_MEMBERS)
        if extra:
            problems.append(f"item {name!r} carries {extra}")
    for item in items:
        want = expected_record_item(item, result=results.get(item["name"], "ok"))
        ri = got.get(item["name"])
        if ri is None:
            problems.append(f"item {item['name']!r} is not in the record")
            continue
        for k, v in want.items():
            if ri.get(k) != v:
                problems.append(f"item {item['name']!r}: {k} is {ri.get(k)!r}, want {v!r}")
        for k in ("base", "expires_at"):
            if k in ri and k not in want:
                problems.append(f"item {item['name']!r} has {k} {ri[k]!r}, want none")
        written = bool(ri.get("written_at"))
        if written != (want["result"] == "ok"):
            problems.append(f"item {item['name']!r}: written_at {ri.get('written_at')!r} with result {ri.get('result')!r}")
    return problems


def without_times(record: Mapping) -> dict:
    """The record with what a re-delivery may change taken out: the last
    delivery's `at` and each item's `written_at`."""
    r = copy.deepcopy(dict(record or {}))
    if isinstance(r.get("last"), dict):
        r["last"].pop("at", None)
    for it in r.get("items") or []:
        if isinstance(it, dict):
            it.pop("written_at", None)
    return r


def failure_text(reason: str, *, rule: Optional[str] = None, item: Optional[str] = None,
                 result: Optional[str] = None) -> str:
    """How the warning names a failure (internal/delivery/lifecycle.go:
    FailureText)."""
    if reason == "answer_refused" and rule:
        return f"{reason}: {rule}"
    if reason == "write_failed" and item:
        return f"{reason}: {item} {result}"
    if reason == "write_failed":
        return f"{reason}: the env file {result}"
    return reason


def delivery_warning(shed: str, reason: str, **kw) -> str:
    """The fixed warning of a failed delivery."""
    if reason == "image_too_old":
        return IMAGE_TOO_OLD_FORMAT.format(failure=failure_text(reason, **kw))
    return WARNING_FORMAT.format(failure=failure_text(reason, **kw), shed=shed)


def expired_warning(shed: str, item: str) -> str:
    return EXPIRED_FORMAT.format(item=item, shed=shed)


def delivery_warnings(output: str) -> list[str]:
    """The failed-delivery warnings in a command's output, each as the
    server wrote it (the CLI's `Warning: ` prefix and indent removed)."""
    out = []
    for line in output.splitlines():
        s = line.strip()
        if s.startswith("Warning: "):
            s = s[len("Warning: "):]
        if s.startswith("Credentials were not delivered"):
            out.append(s)
    return out


def expired_warnings(output: str) -> list[str]:
    """The expired-item warnings in `shed start`'s output, whole."""
    return [line.strip() for line in output.splitlines() if line.strip().startswith("Warning: delivered item ")]


# ----------------------------------------------------------------------------
# Sentinels: values that must never leave the shed
# ----------------------------------------------------------------------------


def new_sentinel() -> str:
    """A value no server writes on its own: letters and digits only, so it
    reads the same raw, JSON-encoded and in a shell."""
    return "itestsntl" + secrets.token_hex(12)


def sentinel_forms(sentinel: str, min_len: int = 12) -> list[str]:
    """Every form the sentinel can take where the server may hold it: raw,
    and the part of its base64 that is the same whatever surrounds it, at
    each of the three alignments (a file's content travels as base64). A
    base64 of `P + sentinel + S` contains the form for `len(P) % 3`."""
    raw = sentinel.encode()
    forms = [sentinel]
    for k in range(3):
        enc = base64.b64encode(b"\0" * k + raw + b"\0" * 3).decode("ascii")
        first = -(-k // 3)
        end = (k + len(raw)) // 3
        core = enc[4 * first:4 * end]
        if len(core) >= min_len and core not in forms:
            forms.append(core)
    return forms


def found_forms(blob: Union[str, bytes, None], forms: Iterable[str]) -> list[str]:
    """The forms present in `blob`."""
    if blob is None:
        return []
    data = blob.encode("utf-8", errors="replace") if isinstance(blob, str) else bytes(blob)
    return [f for f in forms if f.encode() in data]


# The scans skip what is not the server's own writing: image blobs (pruned by
# directory name) and anything at least MAX_SCAN_BYTES, which is where a
# shed's disk lives (rootfs, upper, snapshot images are gigabytes).
MAX_SCAN_BYTES = 16 << 20
PRUNE_DIRS = ("blobs", "images")


class ScanError(AssertionError):
    """A sweep that could not read everything it was to read: it proves
    nothing, so it fails."""


def _read_for_scan(path: str) -> bytes:
    """A file's bytes (split out so the unit tests can make a file vanish
    or refuse to be read)."""
    with open(path, "rb") as f:
        return f.read()


def scan_tree(roots: Sequence[Union[str, Path]], needles: Sequence[str], *,
              max_bytes: int = MAX_SCAN_BYTES) -> list[tuple[str, str]]:
    """`(path, needle)` for each file under `roots` (local) that holds a
    needle: regular files under `max_bytes`, no link followed, the
    `PRUNE_DIRS` skipped, one file system per root.

    Nothing unread is passed over silently: a missing root, a directory that
    cannot be listed, and a file or directory that cannot be examined or read
    raise ScanError, naming every one. The one thing skipped is a file or
    directory that disappeared between being listed and being read (a
    server writes its state while it is swept)."""
    hits = []
    errors: list[str] = []
    data_needles = [n.encode() for n in needles]

    def walk_error(e: OSError) -> None:
        if isinstance(e, FileNotFoundError) and e.filename and e.filename not in [str(r) for r in roots]:
            return  # a directory that went away after it was listed
        errors.append(f"{getattr(e, 'filename', '?')}: cannot be listed: {e.strerror or e}")

    for root in roots:
        root = str(root)
        if not os.path.isdir(root):
            raise ScanError(f"scan root {root} is not a directory")
        dev = os.lstat(root).st_dev
        for dirpath, dirnames, filenames in os.walk(root, onerror=walk_error, followlinks=False):
            keep = []
            for d in dirnames:
                if d in PRUNE_DIRS:
                    continue
                p = os.path.join(dirpath, d)
                try:
                    st = os.lstat(p)
                except FileNotFoundError:
                    continue
                except OSError as e:
                    errors.append(f"{p}: cannot be examined: {e.strerror or e}")
                    continue
                if stat.S_ISDIR(st.st_mode) and st.st_dev == dev:
                    keep.append(d)
            dirnames[:] = keep
            for fn in filenames:
                p = os.path.join(dirpath, fn)
                try:
                    st = os.lstat(p)
                except FileNotFoundError:
                    continue
                except OSError as e:
                    errors.append(f"{p}: cannot be examined: {e.strerror or e}")
                    continue
                if not stat.S_ISREG(st.st_mode) or st.st_size >= max_bytes:
                    continue
                try:
                    data = _read_for_scan(p)
                except FileNotFoundError:
                    continue  # it went away after it was listed
                except OSError as e:
                    errors.append(f"{p}: cannot be read: {e.strerror or e}")
                    continue
                for n, nb in zip(needles, data_needles, strict=True):
                    if nb in data:
                        hits.append((p, n))
    if errors:
        raise ScanError(f"the sweep could not read {len(errors)} paths, so it proves nothing:\n" + "\n".join(errors[:50]))
    return hits


SCAN_DONE = "ITEST-SCAN-DONE"
# remote_scan_command's exit codes when it cannot finish.
SCAN_MISSING_ROOT = 3
SCAN_FIND_FAILED = 4
SCAN_GREP_FAILED = 5

# Run as root on the server's host: `bash -c SCAN_SCRIPT _ <max KiB>
# <needle count> <needle>... <root>...`. find's errors fail the scan unless
# every one is a path that vanished; each batch of files is grepped at once,
# and when grep reports an error (exit 2 or more, never 1, which is "no
# match") each file of the batch is grepped alone: one that no longer exists
# is skipped, any other error fails the scan.
SCAN_SCRIPT = r"""set -u
kib=$1; n=$2; shift 2
pats=(); for ((i = 0; i < n; i++)); do pats+=(-e "$1"); shift; done
roots=("$@")
for r in "${roots[@]}"; do
  [ -d "$r" ] || { echo "a scan root is missing: $r" >&2; exit 3; }
done
work=$(mktemp -d) || exit 4
trap 'rm -rf "$work"' EXIT
find "${roots[@]}" -xdev \( -name blobs -o -name images \) -prune -o -type f -size "-${kib}k" -print0   >"$work/list" 2>"$work/find.err"
frc=$?
if [ "$frc" -ne 0 ] && { [ ! -s "$work/find.err" ] || grep -qv 'No such file or directory$' "$work/find.err"; }; then
  echo "find failed (exit $frc):" >&2; head -n 20 "$work/find.err" >&2; exit 4
fi
grep_batch() {
  out=$(grep -lF "${pats[@]}" -- "$@" 2>/dev/null)
  rc=$?
  if [ "$rc" -le 1 ]; then
    [ -n "$out" ] && printf '%s\n' "$out"
    return 0
  fi
  for f in "$@"; do
    grep -qF "${pats[@]}" -- "$f" 2>"$work/grep.err"
    case $? in
      0) printf '%s
' "$f" ;;
      1) ;;
      *) [ -e "$f" ] || [ -L "$f" ] || continue
         echo "cannot read $f: $(cat "$work/grep.err")" >&2; return 1 ;;
    esac
  done
  return 0
}
batch=()
while IFS= read -r -d '' f; do
  batch+=("$f")
  if [ "${#batch[@]}" -ge 200 ]; then
    grep_batch "${batch[@]}"
    [ $? -eq 0 ] || exit 5
    batch=()
  fi
done <"$work/list"
if [ "${#batch[@]}" -gt 0 ]; then
  grep_batch "${batch[@]}"
  [ $? -eq 0 ] || exit 5
fi
echo ITEST-SCAN-DONE
"""


def remote_scan_command(roots: Sequence[str], needles: Sequence[str], *, max_bytes: int = MAX_SCAN_BYTES) -> str:
    """The shell command (run on the server's host, over ssh) that prints
    each file under `roots` holding a needle, chosen as `scan_tree` chooses
    them, then `SCAN_DONE`. It runs SCAN_SCRIPT as root (`sudo -n`) and
    fails (SCAN_MISSING_ROOT, SCAN_FIND_FAILED, SCAN_GREP_FAILED, with the
    reason on stderr) rather than report a sweep it could not finish; as in
    `scan_tree`, only a path that vanished mid-scan is skipped."""
    if not roots or not needles:
        raise ValueError("a scan needs roots and needles")
    kib = max(1, max_bytes // 1024)
    args = [str(kib), str(len(needles)), *needles, *roots]
    return "sudo -n bash -c " + shlex.quote(SCAN_SCRIPT) + " _ " + " ".join(shlex.quote(a) for a in args)


def parse_remote_scan(stdout: str) -> list[str]:
    """The files `remote_scan_command` printed. Raises AssertionError when
    its end marker is missing (the command did not finish)."""
    lines = stdout.splitlines()
    if SCAN_DONE not in lines:
        raise AssertionError(f"the scan did not finish: {stdout[-800:]!r}")
    return [l for l in lines[:lines.index(SCAN_DONE)] if l.strip()]


# ----------------------------------------------------------------------------
# The server's files: log, audit log, state directories
# ----------------------------------------------------------------------------


@dataclass(frozen=True)
class ServerFiles:
    """Where a dev server under test keeps what the sentinel sweep reads.
    `ssh_host` is the host to read them on (None: this host)."""

    audit_log: str
    state_dirs: tuple[str, ...]
    ssh_host: Optional[str] = None


def _env_list(environ: Mapping[str, str], name: str) -> Optional[tuple[str, ...]]:
    raw = environ.get(name, "").strip()
    if not raw:
        return None
    return tuple(os.path.expanduser(p.strip()) for p in raw.split(",") if p.strip())


def server_files(backend: str, *, ssh_host: Optional[str] = None,
                 environ: Mapping[str, str] = os.environ) -> ServerFiles:
    """The audit log and the state directories of the dev server for
    `backend` (`vz`, `fc`, `remote`), from the environment or the dev
    servers' defaults:

      - vz: the Mac dev server, this host. `SHED_VZ_DEV_AUDIT_LOG`,
        `SHED_VZ_DEV_STATE_DIRS` (a comma list); else the directory above its
        `vz.instance_dir` (audit log, source bindings, metadata; moved by
        `SHED_DEV_STATE_DIR`) and its identity `state_dir`
        (`fixtures/devcontrol.py`).
      - fc: on `ssh_host`. `SHED_FC_DEV_AUDIT_LOG` (as test_service_client.py
        reads it; default /var/lib/shed-dev/firecracker/audit.jsonl),
        `SHED_FC_DEV_STATE_DIRS`; else the audit log's directory and the
        `state` directory beside it (/var/lib/shed-dev/state).
      - remote: this host. `SHED_REMOTE_AUDIT_LOG`, `SHED_REMOTE_STATE_DIRS`;
        else under `SHED_REMOTE_DEV_DIR` (default ~/.shed/dev-remote): its
        `remote` (the backend's state, with the audit log) and `state`
        (identity) directories."""
    if backend == "vz":
        audit = environ.get("SHED_VZ_DEV_AUDIT_LOG", "").strip()
        states = _env_list(environ, "SHED_VZ_DEV_STATE_DIRS")
        if not audit or states is None:
            from . import devcontrol as dc

            cfg = dc._merge_config({})
            backend_dir = str(dc.dev_bindings_store(cfg, environ=dict(environ)).parent)
            audit = audit or os.path.join(backend_dir, "audit.jsonl")
            states = states or (backend_dir, str(dc.dev_state_dir(cfg, environ=dict(environ))))
        return ServerFiles(os.path.expanduser(audit), states)
    if backend == "fc":
        audit = environ.get("SHED_FC_DEV_AUDIT_LOG", "").strip() or "/var/lib/shed-dev/firecracker/audit.jsonl"
        states = _env_list(environ, "SHED_FC_DEV_STATE_DIRS")
        if states is None:
            backend_dir = os.path.dirname(audit)
            states = (backend_dir, os.path.join(os.path.dirname(backend_dir), "state"))
        return ServerFiles(audit, states, ssh_host)
    if backend == "remote":
        base = os.path.expanduser(environ.get("SHED_REMOTE_DEV_DIR", "").strip() or "~/.shed/dev-remote")
        audit = environ.get("SHED_REMOTE_AUDIT_LOG", "").strip() or os.path.join(base, "remote", "audit.jsonl")
        states = _env_list(environ, "SHED_REMOTE_STATE_DIRS") or (os.path.join(base, "remote"), os.path.join(base, "state"))
        return ServerFiles(os.path.expanduser(audit), states)
    raise ValueError(f"backend {backend!r}: expected vz, fc or remote")


def _ssh(host: str, command: str, timeout: float = 120) -> subprocess.CompletedProcess:
    return subprocess.run(
        ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", host, command],
        capture_output=True, text=True, errors="replace", timeout=timeout,
    )


def read_audit_log(files: ServerFiles) -> str:
    """The whole audit log (raises AssertionError when it cannot be read: a
    server with delivery keeps one, on by default in mtls)."""
    if files.ssh_host is None:
        try:
            return Path(files.audit_log).read_bytes().decode("utf-8", errors="replace")
        except OSError as e:
            raise AssertionError(f"cannot read the audit log {files.audit_log}: {e}; set the backend's "
                                 f"*_AUDIT_LOG (fixtures/delivery.py: server_files)") from e
    r = _ssh(files.ssh_host, f"sudo -n cat {shlex.quote(files.audit_log)}")
    if r.returncode != 0:
        raise AssertionError(f"cannot read the audit log {files.ssh_host}:{files.audit_log} (exit {r.returncode}): "
                             f"{r.stderr.strip()[-400:]!r}; set SHED_FC_DEV_AUDIT_LOG")
    return r.stdout


def audit_events(text: str) -> list[dict]:
    """The audit log's lines as objects (a line that does not parse, as a
    torn last line may not, is left out)."""
    out = []
    for line in text.splitlines():
        try:
            ev = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(ev, dict):
            out.append(ev)
    return out


def scan_state_dirs(files: ServerFiles, needles: Sequence[str]) -> list[str]:
    """The files in the server's state directories that hold any of
    `needles` (see `scan_tree`)."""
    if files.ssh_host is None:
        return sorted({p for p, _ in scan_tree(files.state_dirs, needles)})
    r = _ssh(files.ssh_host, remote_scan_command(files.state_dirs, needles), timeout=600)
    if r.returncode != 0:
        raise ScanError(f"the sweep of {files.ssh_host}:{','.join(files.state_dirs)} did not finish (exit "
                        f"{r.returncode}): {r.stderr.strip()[-800:]!r}"
                        + ("; set SHED_FC_DEV_STATE_DIRS" if r.returncode == SCAN_MISSING_ROOT else ""))
    return parse_remote_scan(r.stdout)


# ----------------------------------------------------------------------------
# The contract's refuse fixtures, live
# ----------------------------------------------------------------------------


@dataclass(frozen=True)
class RefuseFixture:
    name: str
    path: Path
    rule: str
    status: int
    request: str


# The request fixture every live-runnable refuse fixture is judged against.
# The others (a project directory above the home, a repository named .ssh)
# need a shed this suite does not make; the transport tests run them.
LIVE_REQUEST = "create-repo"
# Fixtures whose refusal comes from where the project directory of their
# request (/home/shed/widgets) places a home item: a live shed's project
# directory is another, so the answer could be accepted there.
PROJECT_PLACED_PREFIX = "cross-base-"
# For a rule with several fixtures, the one the live subset runs: the
# security-bearing ones (the `.` segment is the plan's named negative
# control; authorized_keys; LD_PRELOAD).
PREFERRED = {
    "path_dot_segment": "path-dot-segment",
    "refused_target": "refused-target-authorized-keys",
    "refused_variable": "refused-variable-ld-preload",
}


def load_refuse_fixtures(directory: Path = REFUSE_DIR) -> list[RefuseFixture]:
    out = []
    for p in sorted(directory.glob("*.json")):
        d = json.loads(p.read_text(encoding="utf-8"))
        exp = d.get("expect") or {}
        if exp.get("result") != "refused":
            raise ValueError(f"{p.name}: a refuse fixture that does not expect a refusal")
        out.append(RefuseFixture(name=d["name"], path=p, rule=exp["rule"], status=int(exp["status"]),
                                 request=d["request"]))
    return out


def runs_live(f: RefuseFixture) -> bool:
    """Whether a fixture's refusal holds against any live request with a
    project directory (see LIVE_REQUEST and PROJECT_PLACED_PREFIX)."""
    return f.request == LIVE_REQUEST and not f.name.startswith(PROJECT_PLACED_PREFIX)


def select_refuse_fixtures(fixtures: Sequence[RefuseFixture], everything: bool = False) -> list[RefuseFixture]:
    """The fixtures the live case sends: with `everything`, every
    live-runnable one; else one per rule (the PREFERRED one, or the first by
    name), so every rule the server can refuse an answer with live is
    covered, sorted by rule."""
    live = [f for f in fixtures if runs_live(f)]
    if everything:
        return sorted(live, key=lambda f: (f.rule, f.name))
    by_rule: dict[str, list[RefuseFixture]] = {}
    for f in live:
        by_rule.setdefault(f.rule, []).append(f)
    out = []
    for rule in sorted(by_rule):
        group = sorted(by_rule[rule], key=lambda f: f.name)
        pick = next((f for f in group if f.name == PREFERRED.get(rule)), group[0])
        out.append(pick)
    return out


# ----------------------------------------------------------------------------
# Checking files in the shed
# ----------------------------------------------------------------------------

# Run as `bash -c PATHS_SCRIPT _ <path>...` in the shed: one tab-separated
# line per path (path, kind, mode, owner, base64 content for a file), or
# `<path>\tmissing`. A link is reported as a link and never followed.
PATHS_SCRIPT = r"""for p in "$@"; do
  if [ -L "$p" ]; then printf '%s\tlink\n' "$p"
  elif [ -d "$p" ]; then printf '%s\tdir\t%s\t%s\n' "$p" "$(stat -c %a -- "$p")" "$(stat -c %U -- "$p")"
  elif [ -f "$p" ]; then printf '%s\tfile\t%s\t%s\t%s\n' "$p" "$(stat -c %a -- "$p")" "$(stat -c %U -- "$p")" "$(base64 -w0 -- "$p")"
  elif [ -e "$p" ]; then printf '%s\tother\n' "$p"
  else printf '%s\tmissing\n' "$p"
  fi
done
"""


@dataclass(frozen=True)
class ShedPath:
    path: str
    kind: str  # file, dir, link, other, missing
    mode: Optional[str] = None  # octal digits as stat prints them (600)
    owner: Optional[str] = None
    content: Optional[bytes] = None


def parse_paths(stdout: str) -> dict[str, ShedPath]:
    out = {}
    for line in stdout.splitlines():
        if not line.strip():
            continue
        f = line.split("\t")
        path, kind = f[0], f[1] if len(f) > 1 else "?"
        if kind == "file" and len(f) >= 5:
            out[path] = ShedPath(path, kind, f[2], f[3], base64.b64decode(f[4]))
        elif kind == "dir" and len(f) >= 4:
            out[path] = ShedPath(path, kind, f[2], f[3])
        else:
            out[path] = ShedPath(path, kind)
    return out


def file_problems(got: Mapping[str, ShedPath], items: Sequence[Mapping], project_dir: Optional[str]) -> list[str]:
    """How the shed's files differ from the file items: each a regular file
    owned by shed, with the item's mode and content byte for byte."""
    problems = []
    for item in items:
        if item.get("kind") != "file":
            continue
        p = item_shed_path(item, project_dir)
        sp = got.get(p)
        if sp is None or sp.kind != "file":
            problems.append(f"{p} ({item['name']}): {sp.kind if sp else 'not checked'}, want a file")
            continue
        if sp.mode != _octal(item["mode"]):
            problems.append(f"{p} ({item['name']}): mode {sp.mode}, want {item['mode']}")
        if sp.owner != "shed":
            problems.append(f"{p} ({item['name']}): owner {sp.owner!r}, want shed")
        if sp.content != item_content(item):
            problems.append(f"{p} ({item['name']}): content differs from what was delivered")
    return problems


def _octal(mode: str) -> str:
    """stat's `%a` (no leading zeros) for a contract mode (`0600`)."""
    return format(int(mode, 8), "o")


# ----------------------------------------------------------------------------
# The reference source, as a subprocess
# ----------------------------------------------------------------------------


def ref_source_binary(environ: Mapping[str, str] = os.environ) -> Path:
    """`SHED_PROVISION_REF`, or `bin/test/shed-provision-ref`."""
    raw = environ.get("SHED_PROVISION_REF", "").strip()
    return Path(raw).expanduser() if raw else REF_SOURCE_BIN


@dataclass(frozen=True)
class SourceIdentity:
    """What the reference source connects as: the server's URL on loopback,
    the server certificate it trusts (`ca_file`), the machine's credentials
    certificate, and the machine's id and name."""

    url: str
    ca_file: str
    cert_file: str
    key_file: str
    machine_id: str
    machine_name: str


def refsource_argv(binary: Union[str, Path], ident: SourceIdentity, *, items_file: Optional[str] = None,
                   refuse_fixture: Optional[str] = None, silent: bool = False, delay: Optional[float] = None,
                   twice: bool = False, second_connection: bool = False, answer_machine_id: Optional[str] = None,
                   answer_cert: Optional[tuple[str, str]] = None, once: bool = False) -> list[str]:
    """The reference source's command line
    (internal/delivery/refsource/cmd/shed-provision-ref/main.go)."""
    if items_file and refuse_fixture:
        raise ValueError("canned items or a refuse fixture, not both")
    argv = [str(binary), "--server", ident.url, "--cert", ident.cert_file, "--key", ident.key_file,
            "--ca", ident.ca_file, "--machine-id", ident.machine_id, "--machine-name", ident.machine_name]
    if items_file:
        argv += ["--items", items_file]
    if refuse_fixture:
        argv += ["--refuse-fixture", refuse_fixture]
    if silent:
        argv.append("--silent")
    if delay:
        argv += ["--delay", f"{delay:g}s"]
    if twice:
        argv.append("--twice")
    if second_connection:
        argv.append("--second-connection")
    if answer_machine_id:
        argv += ["--answer-machine-id", answer_machine_id]
    if answer_cert:
        argv += ["--answer-cert", answer_cert[0], "--answer-key", answer_cert[1]]
    if once:
        argv.append("--once")
    return argv


EVENT_KINDS = ("stream", "refused", "request", "invalid_request", "answer", "dropped", "end")


def parse_event_line(line: str) -> Optional[dict]:
    """One event line of the reference source, or None for a line that is
    not one (a JSON object with a known `event`)."""
    line = line.strip()
    if not line:
        return None
    try:
        ev = json.loads(line)
    except json.JSONDecodeError:
        return None
    if not isinstance(ev, dict) or ev.get("event") not in EVENT_KINDS:
        return None
    return ev


def source_env(home: Path) -> dict:
    """The reference source's whole environment: a throwaway HOME, a plain
    PATH, nothing else (no SSH agent, no proxy, nothing of the
    developer's)."""
    return {"HOME": str(home), "PATH": "/usr/local/bin:/usr/bin:/bin", "LC_ALL": "C"}


class RefSource:
    """One running reference source. Events are read on a thread into
    `events`; `wait_for` waits for one. Use as a context manager: the
    process is stopped (SIGTERM, then SIGKILL) on exit."""

    def __init__(self, argv: Sequence[str], home: Path, *, label: str = "source") -> None:
        self.argv = list(argv)
        self.home = Path(home)
        self.label = label
        self.events: list[dict] = []
        self._cond = threading.Condition()
        self._stderr_path = self.home / f"{label}.stderr"
        self._stderr = open(self._stderr_path, "wb")
        self.proc = subprocess.Popen(self.argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                     stderr=self._stderr, env=source_env(self.home), cwd=str(self.home))
        self._thread = threading.Thread(target=self._read, name=f"refsource-{label}", daemon=True)
        self._thread.start()

    def _read(self) -> None:
        try:
            assert self.proc.stdout is not None
            for raw in self.proc.stdout:
                ev = parse_event_line(raw.decode("utf-8", errors="replace"))
                if ev is None:
                    continue
                ev["_t"] = time.monotonic()
                with self._cond:
                    self.events.append(ev)
                    self._cond.notify_all()
        except Exception:  # noqa: BLE001 - a thread's exception is a warning, which this suite makes an error
            pass
        finally:
            with self._cond:
                self._cond.notify_all()

    def __enter__(self) -> "RefSource":
        return self

    def __exit__(self, *exc) -> None:
        self.stop()

    def wait_for(self, pred: Callable[[dict], bool], timeout: float) -> Optional[dict]:
        """The first event matching `pred`, waiting up to `timeout`; None when
        none came (or the process ended first)."""
        deadline = time.monotonic() + timeout
        with self._cond:
            while True:
                for ev in self.events:
                    if pred(ev):
                        return ev
                left = deadline - time.monotonic()
                if left <= 0 or (self.proc.poll() is not None and not self._thread.is_alive()):
                    return None
                self._cond.wait(min(left, 0.5))

    def matching(self, pred: Callable[[dict], bool]) -> list[dict]:
        with self._cond:
            return [e for e in self.events if pred(e)]

    def requests(self, shed: Optional[str] = None) -> list[dict]:
        return self.matching(lambda e: e["event"] == "request" and (shed is None or e.get("shed") == shed))

    def answers(self, request_id: Optional[str] = None) -> list[dict]:
        return self.matching(lambda e: e["event"] == "answer" and (request_id is None or e.get("id") == request_id))

    def stream_id(self) -> Optional[str]:
        got = self.matching(lambda e: e["event"] == "stream")
        return got[-1].get("stream") if got else None

    def wait_stream(self, timeout: float = 30) -> dict:
        """Wait until the source holds its stream (the server registered it
        before its 200). A refusal on the way (a duplicate stream the server
        has not yet seen end) is retried by the source itself."""
        ev = self.wait_for(lambda e: e["event"] == "stream", timeout)
        if ev is None:
            raise AssertionError(f"the reference source ({self.label}) did not open its stream within {timeout:.0f} s: "
                                 f"{self.describe()}")
        return ev

    def wait_request(self, shed: str, timeout: float = 60) -> dict:
        ev = self.wait_for(lambda e: e["event"] == "request" and e.get("shed") == shed, timeout)
        if ev is None:
            raise AssertionError(f"no request for shed {shed!r} reached the reference source ({self.label}) within "
                                 f"{timeout:.0f} s: {self.describe()}")
        return ev

    def describe(self) -> str:
        """The events so far and the end of stderr, for a failure message."""
        with self._cond:
            evs = [{k: v for k, v in e.items() if k != "_t"} for e in self.events]
        try:
            err = self._stderr_path.read_text(errors="replace")[-600:]
        except OSError:
            err = ""
        return f"exit={self.proc.poll()} events={evs!r} stderr={err!r}"

    def stop(self) -> None:
        """Stop the source: its stream ends (the server fails what it
        carried). Idempotent."""
        if self.proc.poll() is None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=10)
        self._thread.join(timeout=10)
        if self.proc.stdout is not None:
            self.proc.stdout.close()
        if not self._stderr.closed:
            self._stderr.close()


# ----------------------------------------------------------------------------
# Loopback to the server under test
# ----------------------------------------------------------------------------


def free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _pin_at(port: int) -> Optional[str]:
    """The fingerprint of the certificate served at 127.0.0.1:port, or None
    when nothing answers TLS there."""
    import ssl

    from .tlsclient import fingerprint

    try:
        return fingerprint(ssl.get_server_certificate(("127.0.0.1", port), timeout=5))
    except (OSError, ValueError):
        return None


@contextmanager
def loopback_endpoint(port: int, pin: str, ssh_host: Optional[str], *, timeout: float = 20) -> Iterator[str]:
    """`https://127.0.0.1:<port>` for the server whose certificate is `pin`:
    its own port when it serves on this host, else an `ssh -L` forward to
    127.0.0.1:<port> on `ssh_host`, held for the block. Fails when neither
    reaches a server with that certificate."""
    if _pin_at(port) == pin:
        yield f"https://127.0.0.1:{port}"
        return
    if not ssh_host:
        raise AssertionError(f"the server under test (pin {pin}) is not on 127.0.0.1:{port} and there is no host to "
                             f"forward to")
    local = free_port()
    proc = subprocess.Popen(
        ["ssh", "-N", "-o", "BatchMode=yes", "-o", "ExitOnForwardFailure=yes", "-o", "ConnectTimeout=10",
         "-L", f"127.0.0.1:{local}:127.0.0.1:{port}", ssh_host],
        stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
    )
    try:
        deadline = time.monotonic() + timeout
        while _pin_at(local) != pin:
            if proc.poll() is not None or time.monotonic() > deadline:
                err = proc.stderr.read().decode(errors="replace") if proc.poll() is not None and proc.stderr else ""
                raise AssertionError(f"the forward 127.0.0.1:{local} -> {ssh_host}:127.0.0.1:{port} did not reach the "
                                     f"server (pin {pin}) within {timeout:.0f} s: {err[-400:]!r}")
            time.sleep(0.5)
        yield f"https://127.0.0.1:{local}"
    finally:
        if proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=10)
        if proc.stderr is not None:
            proc.stderr.close()


def write_ca(path: Path, host: str, port: int, pin: str) -> Path:
    """Write the server's certificate (fetched from `host:port`, checked
    against `pin`) to `path`: the only CA the reference source trusts."""
    from .tlsclient import fingerprint, server_cert_pem

    pem = server_cert_pem(host, port)
    if fingerprint(pem) != pin:
        raise AssertionError(f"{host}:{port} presented {fingerprint(pem)}, pinned {pin}")
    path.write_text(pem, encoding="utf-8")
    return path


# ----------------------------------------------------------------------------
# Activity paging
# ----------------------------------------------------------------------------


def page_all(fetch: Callable[[Optional[str]], dict], *, max_pages: int = 1000) -> list[dict]:
    """Every event of a paged activity read: `fetch(since)` returns one page
    (`{"events", "next"}`); pages are followed until `next` is empty. Raises
    AssertionError on a repeated cursor or event id (paging that loops or
    repeats)."""
    out: list[dict] = []
    seen_ids: set = set()
    seen_cursors: set = set()
    since: Optional[str] = None
    for _ in range(max_pages):
        page = fetch(since)
        for ev in page.get("events") or []:
            if ev.get("id") in seen_ids:
                raise AssertionError(f"event {ev.get('id')!r} came twice while paging")
            seen_ids.add(ev.get("id"))
            out.append(ev)
        nxt = page.get("next") or ""
        if not nxt:
            return out
        if nxt in seen_cursors:
            raise AssertionError(f"cursor {nxt!r} came twice while paging")
        seen_cursors.add(nxt)
        since = nxt
    raise AssertionError(f"paging did not end within {max_pages} pages")


def rm_tree(path: Path) -> None:
    shutil.rmtree(path, ignore_errors=True)
