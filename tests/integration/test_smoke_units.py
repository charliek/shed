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

import test_delivery
import test_smoke


def test_baked_image_verdict_not_listed_skips_regardless_of_image():
    """A backend absent from `SHED_IMAGE_HAS_CRAZE` always skips — it still
    boots the published image either way, so a missing image there is not
    a failure."""
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
    """A listed backend with the image present runs the real checks."""
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
    store was rebuilt with the craze bake. A missing `base` image
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
        self.name = "unit-dev"
        self.created = []

    def create(self, name, image=None, **kwargs):
        self.created.append(name)
        raise AssertionError(f"shed create {name}: no image tag {image!r} found")

    def exec(self, *args, **kwargs):  # pragma: no cover - must not be reached
        raise AssertionError("exec reached after a missing-image create")


@pytest.mark.parametrize(
    "body, env_var",
    [
        (test_smoke.test_roost_session_baked, "SHED_IMAGE_HAS_ROOST"),
        (test_smoke.test_craze_baked, "SHED_IMAGE_HAS_CRAZE"),
        (test_smoke.test_home_dirs_owned_by_shed, "SHED_IMAGE_HAS_HOME_DIRS"),
        (test_delivery.test_delivery_image_baked, "SHED_IMAGE_HAS_DELIVERY"),
        (test_smoke.test_base_image_smoke, "SHED_IMAGE_HAS_DELIVERY"),
        (test_smoke.test_full_image_smoke, "SHED_IMAGE_HAS_DELIVERY"),
    ],
    ids=["roost", "craze", "home_dirs", "delivery", "base_image", "full_image"],
)
@pytest.mark.parametrize(
    "backend, key", [("firecracker", "fc"), ("vz", "vz")], ids=["fc", "vz"]
)
def test_baked_body_listed_backend_missing_image_fails(
    monkeypatch, body, env_var, backend, key
):
    monkeypatch.setenv(env_var, key)
    monkeypatch.setenv("SHED_CRAZE_VERSION", "0.0.0")  # skip the pin-file read
    # The delivery body checks its server is a dev server before it creates;
    # the fake is not in any config (test_delivery_image_body_guards_first).
    monkeypatch.setattr(test_delivery, "_assert_dev_target", lambda name: None)
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
        (test_smoke.test_home_dirs_owned_by_shed, "SHED_IMAGE_HAS_HOME_DIRS"),
        (test_delivery.test_delivery_image_baked, "SHED_IMAGE_HAS_DELIVERY"),
        (test_smoke.test_base_image_smoke, "SHED_IMAGE_HAS_DELIVERY"),
        (test_smoke.test_full_image_smoke, "SHED_IMAGE_HAS_DELIVERY"),
    ],
    ids=["roost", "craze", "home_dirs", "delivery", "base_image", "full_image"],
)
def test_baked_body_unlisted_backend_skips(monkeypatch, body, env_var):
    monkeypatch.setenv(env_var, "vz")
    with pytest.raises(pytest.skip.Exception):
        body(_FakeServer("firecracker"), "unit-shed")


def test_delivery_image_body_guards_first(monkeypatch):
    """test_delivery_image_baked runs the dev-target guard on its server
    before it creates anything: a server the guard refuses (an installed
    one) gets no shed."""
    monkeypatch.setenv("SHED_IMAGE_HAS_DELIVERY", "fc")
    seen = []

    def refuse(name):
        seen.append(name)
        raise RuntimeError(f"refusing to reconfigure non-dev server {name!r}")

    monkeypatch.setattr(test_delivery, "_assert_dev_target", refuse)
    server = _FakeServer("firecracker")
    with pytest.raises(RuntimeError, match="non-dev server"):
        test_delivery.test_delivery_image_baked(server, "unit-shed")
    assert seen == ["unit-dev"] and server.created == []


def test_delivery_module_uses_dev_targets_only():
    """No case of test_delivery.py takes a fixture that reaches an installed
    server (`shed_server`, `test_shed_name`, `vz_server`, `fc_server`): its
    image check takes `shed_server_dev`, every other case `users_env`."""
    import ast
    from pathlib import Path

    banned = {"shed_server", "test_shed_name", "vz_server", "fc_server"}
    tree = ast.parse(Path(test_delivery.__file__).read_text())
    for fn in ast.walk(tree):
        if isinstance(fn, ast.FunctionDef):
            params = {a.arg for a in fn.args.args + fn.args.kwonlyargs}
            assert not params & banned, f"{fn.name} takes {params & banned}"
            if fn.name == "test_delivery_image_baked":
                assert params == {"shed_server_dev", "delivery_image_name"}, params


def test_image_tests_create_the_two_variants():
    """No case in test_smoke.py creates the retired `extensions` image (plan
    039 §3.12): the variant smoke bodies create `base` and `full`, and the
    roost-session and craze bodies create `base`."""
    import ast
    from pathlib import Path

    tree = ast.parse(Path(test_smoke.__file__).read_text())
    images = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Call) and getattr(node.func, "attr", "") == "create":
            for kw in node.keywords:
                if kw.arg == "image" and isinstance(kw.value, ast.Constant):
                    images.add(kw.value.value)
    assert "extensions" not in images, images
    assert {"base"} <= images, images


class _RecordingServer:
    """A `shed_server` whose image exists: records each exec and answers
    as a rebuilt image of the given variant would."""

    def __init__(self, variant: str, usr_local_bin=None):
        self.backend = "vz"
        self.name = "unit-dev"
        self.variant = variant
        self.usr_local_bin = usr_local_bin or "network-setup.sh\nshed-agent\nshed-firstboot\n"
        self.execs = []

    def create(self, name, image=None, **kwargs):
        assert image == self.variant, image

    def exec(self, name, argv, **kwargs):
        import subprocess

        self.execs.append(argv)
        cmd = " ".join(argv)
        out, rc = "", 0
        if argv[:2] == ["ls", "-A"]:
            out = self.usr_local_bin
        elif "list-unit-files" in cmd:
            out = "shed-agent.service\nshed-firstboot.service\n"
        elif argv[:2] == ["sh", "-c"] and cmd.startswith("sh -c command -v"):
            tool = argv[2].split()[-1]
            rc = 0 if self.variant == "full" else 1
            out = f"/usr/bin/{tool}\n" if rc == 0 else ""
        return subprocess.CompletedProcess(argv, rc, out, "")


@pytest.mark.parametrize("variant", ["base", "full"])
def test_variant_smoke_body_passes_a_good_image(monkeypatch, variant):
    monkeypatch.setenv("SHED_IMAGE_HAS_DELIVERY", "vz")
    server = _RecordingServer(variant)
    test_smoke._variant_smoke(server, "unit-shed", variant)
    tools = [a[2].split()[-1] for a in server.execs if a[:2] == ["sh", "-c"]]
    assert tools == list(test_smoke._STRIDELABS_APT_TOOLS)


def test_variant_smoke_body_fails_on_a_stray_binary(monkeypatch):
    monkeypatch.setenv("SHED_IMAGE_HAS_DELIVERY", "vz")
    server = _RecordingServer("full", "network-setup.sh\nshed-agent\nshed-firstboot\nstray-helper\n")
    with pytest.raises(AssertionError, match="/usr/local/bin"):
        test_smoke._variant_smoke(server, "unit-shed", "full")
