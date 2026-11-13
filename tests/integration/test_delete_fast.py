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

Parameterized over ["vz", "fc", "remote"] via the shed_server fixture; skips
cleanly when a backend is unreachable.

On `remote` the claim is a different one. The CLI's delete (a streamed
request) returns only once the cloud VM is gone, so a create right after it
never finds the old VM still holding a run slot; that took 29 to 34 s on the
dev server. The ceiling there bounds a destroy that hangs, not a graceful
teardown.
"""

from __future__ import annotations

import subprocess
import time

# Seconds a `shed delete --force` of a running shed may take, per backend
# (`shed_server.backend`). Local VMs: the old graceful teardown took ~30s and
# the fast destroy path ~1-2s, so 10s decisively catches a regression while
# tolerating CI/dev-machine variance and (for FC) SSH round-trips. Remote: the
# command waits for the cloud to destroy the VM (about 30s measured).
DEFAULT_DELETE_CEILING_S = 10
DELETE_CEILING_S = {"remote": 120}


def test_delete_running_shed_is_fast(shed_server, test_shed_name):
    """A running shed deletes far under the old ~30s (on `remote`: within the
    cloud's destroy time), and is actually gone."""
    shed_server.create(test_shed_name, image="base")
    ceiling = DELETE_CEILING_S.get(shed_server.backend, DEFAULT_DELETE_CEILING_S)

    start = time.monotonic()
    r = subprocess.run(
        ["shed", "-s", shed_server.name, "delete", test_shed_name, "--force"],
        capture_output=True,
        text=True,
        timeout=max(60, ceiling + 30),
    )
    elapsed = time.monotonic() - start

    assert r.returncode == 0, (
        f"delete failed (exit {r.returncode}): stdout={r.stdout!r} stderr={r.stderr!r}"
    )
    assert elapsed < ceiling, (
        f"delete of a running shed took {elapsed:.1f}s on {shed_server.backend} "
        f"(ceiling {ceiling}s) — on a local VM backend, a regression to the "
        f"graceful ~30s teardown?"
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
