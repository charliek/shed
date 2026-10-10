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

import pytest

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


# ---------------------------------------------------------------------------
# The real test bodies (shed#398). The verdict helper being right is not
# enough: `test_roost_session_baked` used to carry its own skip branch and
# never called it. These call the bodies directly with fake fixtures.
# ---------------------------------------------------------------------------


class _FakeServer:
    """Stands in for `shed_server`: `create()` raises the missing-image
    error the real fixture raises; anything else the body touches fails
    loudly (a listed-but-missing backend must stop at `create`)."""

    def __init__(self, backend: str):
        self.backend = backend

    def create(self, name, image=None, **kwargs):
        raise AssertionError(f"shed create {name}: no image tag {image!r} found")

    def exec(self, *args, **kwargs):  # pragma: no cover - must not be reached
        raise AssertionError("exec reached after a missing-image create")


@pytest.mark.parametrize(
    "body, env_var",
    [
        (test_smoke.test_roost_session_baked, "SHED_IMAGE_HAS_ROOST"),
        (test_smoke.test_craze_baked, "SHED_IMAGE_HAS_CRAZE"),
    ],
    ids=["roost", "craze"],
)
@pytest.mark.parametrize(
    "backend, key", [("firecracker", "fc"), ("vz", "vz")], ids=["fc", "vz"]
)
def test_baked_body_listed_backend_missing_image_fails(
    monkeypatch, body, env_var, backend, key
):
    monkeypatch.setenv(env_var, key)
    monkeypatch.setenv("SHED_CRAZE_VERSION", "0.0.0")  # skip the pin-file read
    # A Skipped outcome would propagate out of `pytest.raises(Failed)` and be
    # reported as a skip (exit 0), so convert it into a real failure.
    try:
        body(_FakeServer(backend), "unit-shed")
    except pytest.fail.Exception:
        return
    except pytest.skip.Exception as e:
        pytest.fail(f"{body.__name__} skipped a listed backend's missing image: {e}")
    pytest.fail(f"{body.__name__} neither failed nor skipped")


@pytest.mark.parametrize(
    "body, env_var",
    [
        (test_smoke.test_roost_session_baked, "SHED_IMAGE_HAS_ROOST"),
        (test_smoke.test_craze_baked, "SHED_IMAGE_HAS_CRAZE"),
    ],
    ids=["roost", "craze"],
)
def test_baked_body_unlisted_backend_skips(monkeypatch, body, env_var):
    monkeypatch.setenv(env_var, "vz")
    with pytest.raises(pytest.skip.Exception):
        body(_FakeServer("firecracker"), "unit-shed")
