"""Image API tests: the alias + is_default metadata on GET /api/images.

These pin the picker-enabling fields the shed-desktop image picker reads
(PR adding `alias` / `is_default` to `config.ImageInfo`). Parameterized
across `["vz", "fc"]` via `shed_server`, so they run on both backends.

We create from the `full` alias — which is the configured `default_image`
in the dev/test configs — so that image is pulled and cached and therefore
appears in `image ls` carrying BOTH its alias label and `is_default: true`.
(A smaller alias like `base` would leave the default uncached, making an
is_default assertion vacuous.)
"""

from __future__ import annotations

import pytest


def test_images_expose_alias_and_default(shed_server, test_shed_name):
    """`image ls` labels config images with their alias + marks the default."""
    # The "full" alias == default_image, so this warms the default. Generous
    # timeout: the full image may need a first-time pull.
    shed_server.create(test_shed_name, image="full", timeout=420)

    images = shed_server.list_images()
    assert images, "image ls returned no images after a create"

    config_imgs = [i for i in images if i.get("source") == "config"]
    assert config_imgs, f"no config-sourced images after create: {images}"

    # The fields are additive + omitempty: a shed-server that predates them
    # (e.g. the brew/deb prod server in a mixed-version dev run) simply omits
    # them. Skip cleanly there rather than red-fail — like the rest of the
    # suite skips on an environment that can't exercise the behavior.
    if not any("alias" in i or "is_default" in i for i in config_imgs):
        pytest.skip("shed-server predates the alias/is_default image metadata")

    # "full" is both the default_image and an alias → it carries both labels.
    full = [i for i in images if i.get("alias") == "full"]
    assert full, f"no image carrying alias 'full' after create --image full: {images}"
    assert full[0].get("source") == "config", full[0]
    assert full[0].get("is_default") is True, full[0]
    assert full[0].get("docker_ref"), f"aliased image missing docker_ref: {full[0]}"

    # is_default is mutually exclusive (exactly one), and alias/is_default only
    # ever appear on config-sourced images.
    assert sum(1 for i in images if i.get("is_default")) == 1, images
    for img in images:
        if img.get("alias") or img.get("is_default"):
            assert img.get("source") == "config", img
