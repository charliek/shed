"""The craze ladder's PRODUCTION constants, run for real (plan 025 C5).

`shed_core::craze` composes the `sh -c` ladder every craze connection sends —
`craze-bridge-hub` and `craze-providers-hub` in `scenarios.json` ARE its output
(the Rust leg asserts the equality). The Rust behaviour tests
(`crates/shed-core/tests/craze_ladder.rs`) run that ladder with every absolute
rung re-rooted under a temp dir, because only root can put a file at
`/usr/local/bin`. This module closes the gap: it runs the scenarios' argv, byte
for byte, against the REAL rung paths.

That needs a box where the rung directories are ours to write — a throwaway
container. The README's Docker recipe prepares them (root creates each absolute
rung directory world-writable and sticky, so the unprivileged test user can add
and remove its OWN stub there and nothing else) and sets `MT_CRAZE_RUNGS=1`.
Without that variable every test here SKIPS: a developer box's `/usr/local/bin`
is not ours to write, and on this repo's own dev host it holds a real, stale
`craze` that must never be touched.

What is asserted, all of it restated here from craze's published contract
(`docs/reference/protocol.md`, "SSH exec") rather than imported from Rust:

* each production rung, in craze's order, execs `craze` with the form's
  arguments — the earliest rung holding one wins;
* the exec'd craze sees the enhanced PATH, the ladder's own directories in the
  ladder's order, then the original PATH, and `cz_path` never leaks into its
  environment — not even when the caller's environment already exported one;
* no craze on any rung → `craze: command not found` on stderr, exit 127;
* the `craze-bridge-hub` wire line, sent VERBATIM through the hermetic sshd
  (`argv[0]` stays `sh` — no receiver swap, unlike `test_wire_contract.py`),
  reaches a craze at `/opt/homebrew/bin` with `bridge --hub` and the enhanced
  PATH built from the remote login's own `HOME`, `USER` and `PATH`.
"""

from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path

import pytest

from conftest import load_golden, run_wire, updating

FORMS = {
    "craze-bridge-hub": ["bridge", "--hub"],
    "craze-providers-hub": ["providers", "--hub", "--json"],
}

# craze's published ladder (rungs 1-9), restated. `None` is rung 2: `command -v
# craze` against the ORIGINAL PATH. `{home}`/`{user}` are filled per run.
RUNGS = [
    ("home-local-bin", "{home}/.local/bin"),
    ("path", None),
    ("opt-homebrew", "/opt/homebrew/bin"),
    ("usr-local", "/usr/local/bin"),
    ("linuxbrew", "/home/linuxbrew/.linuxbrew/bin"),
    ("usr-bin", "/usr/bin"),
    ("nix-profile", "{home}/.nix-profile/bin"),
    ("nix-per-user", "/etc/profiles/per-user/{user}/bin"),
    ("nixos-system", "/run/current-system/sw/bin"),
]

# The enhanced PATH's directories: the ladder's own, in its order.
EXEC_PATH = [d for _, d in RUNGS if d is not None]

# An exported `cz_path` every local run starts with (see `_run_local`).
INHERITED_CZ_PATH = "/inherited/sentinel"


def _user() -> str:
    return subprocess.run(["id", "-un"], capture_output=True, text=True, check=True).stdout.strip()


def _wire_line(argv: list[str]) -> str:
    """The house quoting rule, restated (see `test_wire_contract.py`)."""
    return " ".join("'" + a.replace("'", "'\\''") + "'" for a in argv)


@pytest.fixture(scope="module")
def production_rungs() -> dict:
    """The absolute rung directories, verified to be prepared and empty.

    `MT_CRAZE_RUNGS=1` is a promise that this box is throwaway and its rung
    directories are writable; a promise that turns out false is a failure,
    never a quiet skip. A `craze` already sitting on a rung is refused outright
    — it is not ours to remove, and it would win the ladder.
    """
    if os.environ.get("MT_CRAZE_RUNGS") != "1":
        pytest.skip(
            "the production craze rungs run only where MT_CRAZE_RUNGS=1 says they "
            "are writable — the README's Docker recipe"
        )
    user = _user()
    for template in EXEC_PATH:
        if "{home}" in template:
            continue
        directory = Path(template.format(user=user))
        assert directory.is_dir() and os.access(directory, os.W_OK), (
            f"MT_CRAZE_RUNGS=1, but {directory} is not a writable directory"
        )
        assert not (directory / "craze").exists(), (
            f"refusing: a craze already exists at {directory / 'craze'}"
        )
    return {"user": user}


class Stubs:
    """Stub `craze` executables that record which rung ran, the argv, the PATH
    they saw and whether `cz_path` leaked — and remove themselves."""

    def __init__(self, record: Path):
        self.record = record
        self.placed: list[Path] = []

    def place(self, path: Path, label: str) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(
            "#!/bin/sh\n"
            "{\n"
            f"printf 'rung %s\\n' '{label}'\n"
            "printf 'path %s\\n' \"${PATH-<unset>}\"\n"
            "printf 'cz_path %s\\n' \"${cz_path-<unset>}\"\n"
            "for a in \"$@\"; do printf 'arg %s\\n' \"$a\"; done\n"
            f"}} >'{self.record}'\n"
        )
        path.chmod(0o755)
        self.placed.append(path)

    def remove_all(self) -> None:
        for path in self.placed:
            path.unlink(missing_ok=True)
        self.placed.clear()

    def read(self) -> dict | None:
        if not self.record.exists():
            return None
        out: dict = {"args": []}
        for line in self.record.read_text().splitlines():
            key, _, value = line.partition(" ")
            if key == "arg":
                out["args"].append(value)
            else:
                out[key] = value
        return out


@pytest.fixture
def stubs(tmp_path):
    placed = Stubs(tmp_path / "record")
    yield placed
    placed.remove_all()


def _rung_path(template: str | None, home: str, user: str, orig: Path) -> Path:
    if template is None:
        return orig / "craze"
    return Path(template.format(home=home, user=user)) / "craze"


def _enhanced(home: str, user: str, original: str) -> str:
    return ":".join([d.format(home=home, user=user) for d in EXEC_PATH] + [original])


def _scenario_argv(scenarios: list[dict], sid: str) -> list[str]:
    argv = next(s["argv"] for s in scenarios if s["id"] == sid)
    assert argv[:2] == ["sh", "-c"] and len(argv) == 3, argv
    return argv


def _run_local(argv: list[str], home: str, path: str, user: str, cwd: Path):
    """`env -i` with exactly HOME, PATH and USER — `sh` by absolute path, since
    the PATH handed to the script is deliberately one scratch directory — plus
    an EXPORTED `cz_path`, as a caller's environment might carry: without it the
    prologue's `cz_path` would be born unexported and the "never exported"
    assertion below could not fail."""
    return subprocess.run(
        [
            "/usr/bin/env",
            "-i",
            f"HOME={home}",
            f"PATH={path}",
            f"USER={user}",
            f"cz_path={INHERITED_CZ_PATH}",
            "/bin/sh",
        ]
        + argv[1:],
        capture_output=True,
        text=True,
        cwd=cwd,
        timeout=30,
    )


@pytest.mark.parametrize("sid", sorted(FORMS))
def test_each_production_rung_execs_craze_with_the_forms_arguments(
    production_rungs, scenarios, stubs, tmp_path, sid
):
    """Rung i and every LATER rung hold a stub; the one that runs is rung i,
    with the form's arguments and the production exec PATH."""
    argv = _scenario_argv(scenarios, sid)
    user = production_rungs["user"]
    home, orig = tmp_path / "home", tmp_path / "orig"
    home.mkdir()
    orig.mkdir()
    for i, (label, _) in enumerate(RUNGS):
        for later_label, template in RUNGS[i:]:
            stubs.place(_rung_path(template, str(home), user, orig), later_label)
        stubs.record.unlink(missing_ok=True)
        proc = _run_local(argv, str(home), str(orig), user, tmp_path)
        got = stubs.read()
        stubs.remove_all()

        assert proc.returncode == 0 and proc.stderr == "", (sid, label, proc)
        assert got is not None, f"{sid}: rung {i + 1} ({label}): no craze ran"
        assert got["rung"] == label, f"{sid}: rung {i + 1}: {got['rung']} won instead"
        assert got["args"] == FORMS[sid], f"{sid}: rung {i + 1}: {got['args']}"
        assert got["path"] == _enhanced(str(home), user, str(orig)), f"{sid}: {got['path']}"
        assert got["cz_path"] == "<unset>", f"{sid}: cz_path was exported"


@pytest.mark.parametrize("sid", sorted(FORMS))
def test_no_craze_on_any_production_rung_is_127(production_rungs, scenarios, tmp_path, sid):
    argv = _scenario_argv(scenarios, sid)
    home, orig = tmp_path / "home", tmp_path / "orig"
    home.mkdir()
    orig.mkdir()
    proc = _run_local(argv, str(home), str(orig), production_rungs["user"], tmp_path)
    assert proc.returncode == 127, proc
    assert proc.stderr == "craze: command not found\n", proc
    assert proc.stdout == "", proc


@pytest.mark.live
def test_the_bridge_wire_line_runs_verbatim_through_sshd(
    production_rungs, sshd, scenarios, stubs
):
    """The whole production path: the `craze-bridge-hub` wire line (the golden,
    byte for byte) → sshd → the login shell → `sh -c` → the ladder → a craze at
    `/opt/homebrew/bin`, exec'd with `bridge --hub` under the enhanced PATH
    built from the REMOTE login's own HOME, USER and PATH."""
    argv = _scenario_argv(scenarios, "craze-bridge-hub")
    line = _wire_line(argv)
    if not updating():
        assert load_golden("wire.json")["craze-bridge-hub"] == line, "wire golden drift"

    probe = run_wire(sshd, _wire_line(["sh", "-c", 'printf "%s\\n%s\\n%s" "$HOME" "$USER" "$PATH"']))
    assert probe.returncode == 0, probe.stderr
    remote_home, remote_user, remote_path = probe.stdout.split("\n")
    assert remote_home.startswith("/") and remote_user, probe.stdout
    # Nothing may sit on a rung ahead of /opt/homebrew/bin: not $HOME/.local/bin,
    # and nothing on the login's own PATH (where rung 2 would find it first).
    assert not Path(remote_home, ".local/bin/craze").exists()
    for entry in remote_path.split(":"):
        assert not entry or not Path(entry, "craze").exists(), f"a craze on the login PATH: {entry}"

    stubs.place(Path("/opt/homebrew/bin/craze"), "opt-homebrew")
    proc = run_wire(sshd, line)
    got = stubs.read()
    assert proc.returncode == 0, (proc.returncode, proc.stderr)
    assert got is not None, f"no craze ran: {proc.stderr!r}"
    assert got["rung"] == "opt-homebrew", got
    assert got["args"] == ["bridge", "--hub"], got
    assert got["path"] == _enhanced(remote_home, remote_user, remote_path), json.dumps(got)
    assert got["cz_path"] == "<unset>", got
