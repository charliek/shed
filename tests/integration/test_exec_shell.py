"""Exec-and-shell semantics tests for the SSH command channel (§ #131).

Eight tests, parameterized over `["vz", "fc"]`. They split into two halves
that together encode the security model of #131's POSIX-shell SSH wrap:

1. **Raw SSH gets the full shell** — five tests drive raw `ssh host '<cmd>'`
   through `LocalServer.ssh_exec` to prove that pipes, `$VAR`, `$(…)`, bash
   builtins, and `/etc/profile.d/*.sh` PATH adjustments all fire on the
   guest. This is the new behavior gained by wrapping the server-side
   command in `bash -lc <raw>`.

2. **`shed exec` argv stays literal** — three tests drive
   `LocalServer.exec(name, [argv...])` (the CLI quoter path) to prove
   that even though raw SSH now runs through bash, the CLI's single-quote
   wrap keeps argv literal across the bash reparse. That is the security
   gate: shell metacharacters in user-supplied argv data do NOT escape.

The CLI quoter is now load-bearing: every regression in
`cmd/shed/console.go:validateAndQuoteArgs` would let a metacharacter
expand into shell semantics on the SSH server side. The Go-level
`TestShellQuoteBashRoundTrip` (cmd/shed/console_test.go) is the unit
audit; this file is the live wire audit.
"""

from __future__ import annotations

import pytest


# ---------------------------------------------------------------------------
# Raw-SSH path (server-side `bash -lc <raw>` wrap fires)
# ---------------------------------------------------------------------------


def test_ssh_exec_pipes(shed_server, test_shed_name):
    """`ssh host 'echo a | wc -c'` returns `2` (one byte + newline).

    Proves the server-side `bash -lc` wrap interprets the pipe. Without
    the wrap the agent would try to exec `echo` with `a | wc -c` as
    a literal argv element and bash semantics wouldn't fire.
    """
    shed_server.create(test_shed_name, image="base")
    r = shed_server.ssh_exec(test_shed_name, "echo a | wc -c")
    assert r.returncode == 0, (
        f"ssh exec with pipe failed: exit={r.returncode} stderr={r.stderr!r}"
    )
    assert r.stdout.strip() == "2", (
        f"expected '2' (one char + newline counted by wc), got {r.stdout!r}"
    )


def test_ssh_exec_dollar_var(shed_server, test_shed_name):
    """`ssh host 'echo "$HOME"'` expands `$HOME` to `/home/shed`.

    Proves env expansion happens server-side. The shed user is `shed`
    (UID 1000) per CLAUDE.md / config.WorkspacePath; the login shell's
    `$HOME` is `/home/shed`.
    """
    shed_server.create(test_shed_name, image="base")
    r = shed_server.ssh_exec(test_shed_name, 'echo "$HOME"')
    assert r.returncode == 0, (
        f"ssh exec with $HOME failed: exit={r.returncode} stderr={r.stderr!r}"
    )
    assert r.stdout.strip() == "/home/shed", (
        f"expected '/home/shed', got {r.stdout!r}"
    )


def test_ssh_exec_command_substitution(shed_server, test_shed_name):
    """`ssh host 'echo "$(hostname)"'` returns the guest hostname.

    Proves `$(…)` command substitution fires. The hostname is set
    per-shed at provisioning so we don't pin a specific value; we only
    require a non-empty result.
    """
    shed_server.create(test_shed_name, image="base")
    r = shed_server.ssh_exec(test_shed_name, 'echo "$(hostname)"')
    assert r.returncode == 0, (
        f"ssh exec with $(hostname) failed: exit={r.returncode} stderr={r.stderr!r}"
    )
    hostname = r.stdout.strip()
    assert hostname, f"expected non-empty hostname from $(hostname), got {r.stdout!r}"


def test_ssh_exec_bash_builtin(shed_server, test_shed_name):
    """`ssh host 'type cd && cd /tmp && pwd'` shows `cd is a shell builtin`
    and ends with `/tmp`.

    Proves bash builtins are available (vs the old direct-argv path where
    `type` and `cd` would resolve to whatever was on PATH and `cd` would
    have nothing to chain off of). This is also a strong signal that the
    -lc wrap covers compound statements (`&&`).
    """
    shed_server.create(test_shed_name, image="base")
    r = shed_server.ssh_exec(test_shed_name, "type cd && cd /tmp && pwd")
    assert r.returncode == 0, (
        f"ssh exec with builtin failed: exit={r.returncode} stderr={r.stderr!r}"
    )
    assert "shell builtin" in r.stdout, (
        f"expected 'shell builtin' in stdout (type cd output), got {r.stdout!r}"
    )
    assert r.stdout.strip().endswith("/tmp"), (
        f"expected stdout to end with '/tmp' (cd /tmp && pwd), got {r.stdout!r}"
    )


def test_ssh_exec_login_profile_sourced(shed_server, test_shed_name):
    """Seeding `/etc/profile.d/shedtest.sh` and then reading
    `$SHEDTEST` over raw SSH returns `ok`.

    Proves the `-l` in `bash -lc` actually fires (login-shell init runs
    `/etc/profile.d/*.sh`). Without `-l`, profile scripts wouldn't be
    sourced and PATH-mutating tools (mise, nvm, rustup) wouldn't take
    effect for SSH-driven commands.

    Uses raw `ssh_exec` for both the seed and the read-back: the seed
    relies on a here-doc + `sudo tee`, which needs shell features the
    argv-literal `exec()` path doesn't provide. Same wire, symmetric
    semantics — and any wrap-related regression would surface on both
    halves uniformly.
    """
    shed_server.create(test_shed_name, image="base")

    # Seed the profile snippet server-side via raw ssh + a here-doc so
    # the write happens in one shot. Direct-argv `shed exec` can't drive
    # sudo+tee+stdin in a single call (no stdin pipe through the
    # fixture), and raw ssh_exec is the same path that will read the
    # value back below — same wire, symmetric semantics. The shed user
    # has passwordless sudo per CLAUDE.md.
    seed = shed_server.ssh_exec(
        test_shed_name,
        "sudo tee /etc/profile.d/shedtest.sh > /dev/null <<'EOF'\n"
        "export SHEDTEST=ok\n"
        "EOF",
    )
    assert seed.returncode == 0, (
        f"failed to seed /etc/profile.d/shedtest.sh: "
        f"exit={seed.returncode} stderr={seed.stderr!r}"
    )

    # Now read $SHEDTEST back. The login-shell init sources
    # /etc/profile.d/*.sh, so $SHEDTEST should be `ok`.
    r = shed_server.ssh_exec(test_shed_name, 'echo "$SHEDTEST"')
    assert r.returncode == 0, (
        f"ssh exec of echo $SHEDTEST failed: "
        f"exit={r.returncode} stderr={r.stderr!r}"
    )
    assert r.stdout.strip() == "ok", (
        f"expected 'ok' (from seeded /etc/profile.d/shedtest.sh), "
        f"got {r.stdout!r}; this means `bash -lc` is NOT sourcing "
        f"profile scripts (the `-l` is missing or not effective)"
    )


# ---------------------------------------------------------------------------
# CLI-quoter path (`shed exec` argv stays literal — the security gate)
# ---------------------------------------------------------------------------


def test_shed_exec_still_direct_argv(shed_server, test_shed_name):
    """`shed exec name -- echo 'hello $USER'` echoes literal `hello $USER`.

    The security gate: even though raw SSH now runs through `bash -lc`,
    `shed exec` single-quote-wraps each argv element before sending. Bash
    treats single-quoted text as literal, so the `$USER` reaches `echo`
    as the literal token `$USER` and gets echoed back unexpanded.

    If this test ever flips to expanding `$USER`, the CLI quoter is
    broken and the SSH server side is reparsing argv data as shell
    metacharacters — that's the regression path #131 explicitly guards.
    """
    shed_server.create(test_shed_name, image="base")
    r = shed_server.exec(test_shed_name, ["echo", "hello $USER"])
    assert r.returncode == 0, (
        f"shed exec failed: exit={r.returncode} stderr={r.stderr!r}"
    )
    assert r.stdout.strip() == "hello $USER", (
        f"expected literal 'hello $USER' (CLI quoter MUST keep $USER literal), "
        f"got {r.stdout!r}. The CLI single-quote wrap is broken — the "
        f"server-side bash reparse expanded the variable, which means "
        f"user-supplied argv data is being reinterpreted as shell."
    )


def test_shed_exec_metachar_safety(shed_server, test_shed_name):
    """`shed exec name -- echo 'a;b;c'` echoes literal `a;b;c`.

    Security regression gate for command-splitting metacharacters. A
    leak here would let `shed exec name -- mytool "$(rm -rf /)"` style
    invocations execute the substitution server-side — exactly the
    scenario the single-quote wrap is designed to prevent.
    """
    shed_server.create(test_shed_name, image="base")
    r = shed_server.exec(test_shed_name, ["echo", "a;b;c"])
    assert r.returncode == 0, (
        f"shed exec failed: exit={r.returncode} stderr={r.stderr!r}"
    )
    assert r.stdout.strip() == "a;b;c", (
        f"expected literal 'a;b;c' (CLI quoter MUST escape semicolons), "
        f"got {r.stdout!r}. Semicolon expansion = command injection vector."
    )


def test_shed_exec_with_explicit_bash_lc(shed_server, test_shed_name):
    """`shed exec name -- bash -lc 'echo $HOME'` works.

    Proves the user-driven shell escape — when the operator *wants*
    shell semantics inside `shed exec`, they explicitly invoke a shell.
    This is the documented pattern (CLAUDE.md / docs/reference/cli.md).

    Doubles as a sanity check that login-shell expansion is symmetric
    between the raw SSH path and the user-driven bash path — both
    should resolve `$HOME` to `/home/shed`.
    """
    shed_server.create(test_shed_name, image="base")
    r = shed_server.exec(test_shed_name, ["bash", "-lc", "echo $HOME"])
    assert r.returncode == 0, (
        f"shed exec with bash -lc failed: "
        f"exit={r.returncode} stderr={r.stderr!r}"
    )
    assert r.stdout.strip() == "/home/shed", (
        f"expected '/home/shed' (explicit bash -lc expands $HOME), "
        f"got {r.stdout!r}"
    )
