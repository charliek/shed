"""Fast `shed delete` of a running shed (issue #232).

Deleting a RUNNING shed used to take ~30s — the teardown ran the guest
`hooks.shutdown` + `sync` + a graceful VM shutdown before terminating — which
also raced the CLI's hardcoded 30s client timeout (a delete that succeeded
server-side would surface a scary `context deadline exceeded` on the client).

Delete always discards the writable upper, so the destroy path now SIGKILLs
immediately and skips that graceful work (a running shed with a writable
host-backed mount is still `sync`ed first). This is a live regression guard: a
running-shed delete must complete well under the old ~30s and the shed must
actually be gone. The fix is entirely server-side, so it holds regardless of
the CLI version driving the delete.

Parameterized over ["vz", "fc"] via the shed_server fixture; skips cleanly when
a backend is unreachable.
"""

from __future__ import annotations

import subprocess
import time


def test_delete_running_shed_is_fast(shed_server, test_shed_name):
    """A running shed deletes far under the old ~30s, and is actually gone."""
    shed_server.create(test_shed_name, image="base")

    start = time.monotonic()
    r = subprocess.run(
        ["shed", "-s", shed_server.name, "delete", test_shed_name, "--force"],
        capture_output=True,
        text=True,
        timeout=60,
    )
    elapsed = time.monotonic() - start

    assert r.returncode == 0, (
        f"delete failed (exit {r.returncode}): stdout={r.stdout!r} stderr={r.stderr!r}"
    )
    # The old graceful teardown took ~30s; the fast destroy path is ~1-2s. A
    # 10s ceiling decisively catches a regression to the old behavior while
    # tolerating CI/dev-machine variance and (for FC) SSH round-trips.
    assert elapsed < 10, (
        f"delete of a running shed took {elapsed:.1f}s — expected the fast "
        f"destroy path (<10s); a regression to the graceful ~30s teardown?"
    )

    # The shed must actually be gone (a fast delete that didn't delete is worse
    # than a slow one).
    listing = subprocess.run(
        ["shed", "-s", shed_server.name, "list"],
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert listing.returncode == 0, (
        f"list failed (exit {listing.returncode}): "
        f"stdout={listing.stdout!r} stderr={listing.stderr!r}"
    )
    assert test_shed_name not in listing.stdout, (
        f"shed {test_shed_name} still present after delete:\n{listing.stdout}"
    )
