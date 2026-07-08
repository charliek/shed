"""Image model adoption (shed v0.6.0): the image_digest badge + the picker.

Covers, end to end against the hermetic mock:
  - `images.list` surfaces the enriched `GET /api/images` (alias / is_default
    / cached), which the New-Shed picker renders.
  - a default-image shed (only `image_digest`, no `image`) still round-trips
    its digest over IPC — the regression this change fixes.
  - the picker's chosen alias reaches the create request and the new shed.
  - `ui.show_create` presents the sheet so the picker is screenshot-driveable.
"""

from __future__ import annotations

from _marks import mac_only

pytestmark = mac_only


def test_images_list_exposes_alias_and_default(shed):
    hosts = shed.images_list()
    assert hosts, "images.list returned no hosts"
    images = hosts[0]["images"]
    assert images, "no images for the mock host"

    by_alias = {i["alias"]: i for i in images if i.get("alias")}
    assert {"full", "base", "extensions"} <= set(by_alias), by_alias

    # Exactly the default_image entry is flagged, and it's cached. The IPC
    # wire shape is snake_case (ShedImage's CodingKeys mirror the server).
    assert by_alias["full"]["is_default"] is True
    assert by_alias["full"]["cached"] is True
    assert by_alias["base"].get("is_default", False) is False
    assert sum(1 for i in images if i.get("is_default")) == 1

    # The uncached alias still surfaces (picker shows "not pulled").
    assert by_alias["extensions"]["cached"] is False
    # The dangling blob carries no alias — the picker ignores it.
    assert any(not i.get("alias") for i in images)


def test_image_digest_round_trips(shed):
    """A default-image shed exposes only image_digest; the badge falls back to it."""
    shed.refresh()
    sheds = {s["name"]: s for s in shed.sheds_list()}

    callbell = sheds["callbell"]
    assert not callbell.get("image"), "default-image shed should have no image label"
    assert callbell.get("image_digest", "").startswith("sha256:")

    hello = sheds["hello-world"]
    assert hello["image"] == "full"
    assert hello["image_digest"].startswith("sha256:")


def test_image_digest_resolves_to_repo_tag(shed):
    """The Sheds badge shows repo:tag, resolved by joining a shed's image_digest
    against the host's image list (the new ref-keyed image model). Validates the
    image-API integration end to end with the data the resolver consumes."""
    by_digest = {i["digest"]: i
                 for hl in shed.images_list() for i in (hl.get("images") or [])
                 if i.get("digest")}
    assert by_digest, "images.list returned no digested images (new-model decode failed?)"
    shed.refresh()
    sheds = {s["name"]: s for s in shed.sheds_list()}

    # hello-world's digest matches an image → shown as repo:tag, not the bare sha.
    hello = sheds["hello-world"]
    img = by_digest.get(hello["image_digest"])
    assert img and img["docker_ref"].rsplit("/", 1)[-1] == "shed-vz-full:v0.6.0"

    # callbell's default-image digest has no match → the badge falls back to the digest.
    assert sheds["callbell"]["image_digest"] not in by_digest


def test_create_with_chosen_image(shed, mock):
    cid = shed.create_start("picked", image="base")
    shed.wait_until(
        lambda: shed.create_status(cid)["state"] == "complete",
        what="create complete with chosen image",
        timeout=10,
    )
    # The picker's alias reached the create request...
    assert mock.last_create()["image"] == "base"
    # ...and the created shed reflects it.
    shed.refresh()
    created = {s["name"]: s for s in shed.sheds_list()}["picked"]
    assert created["image"] == "base"


def test_create_picker_is_screenshot_driveable(shed):
    shed.show_create()
    png, w, h = shed.screenshot(surface="window", scale=2)
    assert png[:8] == b"\x89PNG\r\n\x1a\n", "expected a PNG"
    assert w > 0 and h > 0
