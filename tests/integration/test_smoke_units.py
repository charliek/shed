"""Plain unit tests for pure helpers in `test_smoke.py`.

No server, no fixtures, no `shed` CLI — these import `test_smoke` as a
module and exercise its pure functions directly. Importing `test_smoke`
is safe without a live server: its only non-stdlib import at module scope
is `fixtures.server.DEFAULT_AGENT_P50_MS`, and `fixtures/server.py` itself
only needs `pytest`/`pyyaml` at import time (no network, no subprocess
calls at import).

Keep this file for non-live unit coverage of `test_smoke.py` helpers —
it's the "non-live helpers" home referenced by plan 025 C13 item 3
(`~/.claude/plans/shed/025-craze-lane/reviews/c13-cursor-solhigh.md`).
"""

from __future__ import annotations

import test_smoke


def test_baked_image_verdict_not_listed_skips_regardless_of_image():
    """A backend absent from `SHED_IMAGE_HAS_CRAZE` always skips — it still
    boots the published image either way, so a missing `extensions` image
    there is not a failure."""
    assert (
        test_smoke._baked_image_verdict("vz", listed_backends=set(), image_present=True)
        == "skip"
    )
    assert (
        test_smoke._baked_image_verdict("vz", listed_backends=set(), image_present=False)
        == "skip"
    )
    assert (
        test_smoke._baked_image_verdict(
            "vz", listed_backends={"fc"}, image_present=False
        )
        == "skip"
    )


def test_baked_image_verdict_listed_and_present_runs():
    """A listed backend with the `extensions` image present runs the real
    checks."""
    assert (
        test_smoke._baked_image_verdict(
            "vz", listed_backends={"vz"}, image_present=True
        )
        == "run"
    )
    assert (
        test_smoke._baked_image_verdict(
            "fc", listed_backends={"vz", "fc"}, image_present=True
        )
        == "run"
    )


def test_baked_image_verdict_listed_and_missing_fails():
    """This is the C13 review finding (item 3): a backend the caller
    explicitly listed in `SHED_IMAGE_HAS_CRAZE` is a promise that its dev
    store was rebuilt with the craze bake. A missing `extensions` image
    there must be a FAILURE, not a skip — `make test-integration-dev` sets
    the variable unconditionally (Makefile), so a skip here let the
    suite finish green without ever checking `/usr/bin/craze`."""
    assert (
        test_smoke._baked_image_verdict(
            "vz", listed_backends={"vz"}, image_present=False
        )
        == "fail"
    )
    assert (
        test_smoke._baked_image_verdict(
            "fc", listed_backends={"vz", "fc"}, image_present=False
        )
        == "fail"
    )
