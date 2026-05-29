"""Lifecycle tests: create → stop → start → exec → delete.

Catches regressions in the StartShed-after-StopShed path that the
plain create/delete smoke can't see. Lands with the §15 Phase 2
StartShed orchestrator migration (PR-B1/B2) so future churn in that
codepath has a live signal beyond the orchestrator unit tests.
"""

from __future__ import annotations


def test_lifecycle_plain(shed_server, test_shed_name):
    """create → stop → start → exec → delete on the plain image."""
    shed_server.create(test_shed_name, image="base")

    # First exec proves the shed boots cleanly out of CreateShed.
    r = shed_server.exec(test_shed_name, ["echo", "before-stop"])
    assert r.returncode == 0, f"pre-stop exec failed: stderr={r.stderr!r}"
    assert "before-stop" in r.stdout

    shed_server.stop(test_shed_name)

    # Re-list MUST show the shed in stopped state (`shed list` flips
    # a crashed/missing VM to stopped lazily; an explicit stop should
    # land at stopped immediately).
    names = shed_server.list_shed_names()
    assert test_shed_name in names, (
        f"shed {test_shed_name!r} disappeared after stop; got {names}"
    )

    shed_server.start(test_shed_name)

    # Exec after start proves the post-start hooks (workspace mount,
    # credential mount, agent re-attach) re-armed correctly.
    r = shed_server.exec(test_shed_name, ["echo", "after-start"])
    assert r.returncode == 0, f"post-start exec failed: stderr={r.stderr!r}"
    assert "after-start" in r.stdout

    shed_server.delete(test_shed_name, ignore_missing=False)
    assert test_shed_name not in shed_server.list_shed_names()
