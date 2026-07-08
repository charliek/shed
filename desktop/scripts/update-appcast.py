#!/usr/bin/env python3
"""Append a release to shed-desktop's Sparkle appcast (M8).

Ported from roost's update-appcast.py (itself adapted from Ghostty). Parses the
appcast in place, drops any existing <item> for the same version (duplicate
versions make Sparkle pick a signature nondeterministically), appends a new
<item> for this release, and writes the file back.

We append-in-repo rather than regenerating with Sparkle's `generate_appcast`,
which would need every historical DMG present on disk.

Inputs (environment):
  SHED_DESKTOP_VERSION    required   e.g. "0.2.0" or "0.2.0-beta1"
  SHED_DESKTOP_TAG        optional   git tag; default "v$SHED_DESKTOP_VERSION"
  SHED_DESKTOP_APPCAST    optional   appcast path; default "docs/appcast.xml"
  SHED_DESKTOP_SIGN_FILE  optional   sign_update output file; default "sign_update.txt"
  SHED_DESKTOP_REPO       optional   "owner/repo"; default "charliek/shed-desktop"
  SHED_DESKTOP_MIN_MACOS  optional   minimum system version; default "14.0.0"

`sign_update.txt` must hold the line Sparkle's `sign_update` prints for the
released DMG:

    sparkle:edSignature="<base64>" length="<bytes>"

A prerelease tag (one containing "-", e.g. v0.2.0-beta1) gets a
<sparkle:channel>beta</sparkle:channel> so it reaches only beta subscribers.
"""

import os
import sys
import xml.etree.ElementTree as ET
from datetime import datetime, timezone

SPARKLE_NS = "http://www.andymatuschak.org/xml-namespaces/sparkle"
# RFC-822, the format Sparkle expects for <pubDate>.
PUBDATE_FMT = "%a, %d %b %Y %H:%M:%S %z"


def parse_sign_update(path):
    """Parse the `key="value"` pairs from a `sign_update` output line."""
    with open(path, encoding="utf-8") as f:
        text = f.read().strip()
    if not text:
        sys.exit(f"error: {path} is empty (did sign_update run?)")
    attrs = {}
    for pair in text.split():
        if "=" not in pair:
            continue
        key, value = pair.split("=", 1)
        attrs[key] = value.strip().strip('"')
    return attrs


def qname(local):
    return f"{{{SPARKLE_NS}}}{local}"


def main():
    try:
        version = os.environ["SHED_DESKTOP_VERSION"]
    except KeyError:
        sys.exit("error: SHED_DESKTOP_VERSION is required")
    tag = os.environ.get("SHED_DESKTOP_TAG", f"v{version}")
    appcast_path = os.environ.get("SHED_DESKTOP_APPCAST", "docs/appcast.xml")
    sign_file = os.environ.get("SHED_DESKTOP_SIGN_FILE", "sign_update.txt")
    repo = os.environ.get("SHED_DESKTOP_REPO", "charliek/shed-desktop")
    min_macos = os.environ.get("SHED_DESKTOP_MIN_MACOS", "14.0.0")
    is_prerelease = "-" in tag

    attrs = parse_sign_update(sign_file)
    sig = attrs.get("sparkle:edSignature")
    length = attrs.get("length")
    if not sig or not length:
        sys.exit(f"error: {sign_file} missing sparkle:edSignature/length (got {attrs})")

    # Preserve in-tree XML comments across the rewrite (Python 3.8+).
    ET.register_namespace("sparkle", SPARKLE_NS)
    parser = ET.XMLParser(target=ET.TreeBuilder(insert_comments=True))
    tree = ET.parse(appcast_path, parser)
    channel = tree.getroot().find("channel")
    if channel is None:
        sys.exit(f"error: {appcast_path} has no <channel>")

    # Dedupe by version so a re-run (or re-tag) replaces rather than duplicates.
    # Preserve the prior pubDate when replacing: EdDSA signatures over the same
    # bytes + key are byte-identical, so without this a re-run's only diff is a
    # fresh timestamp — making the bot push a no-content-change commit.
    preserved_pubdate = None
    for item in channel.findall("item"):
        existing = item.find(qname("version"))
        if existing is not None and existing.text == version:
            prev = item.find("pubDate")
            if prev is not None and prev.text:
                preserved_pubdate = prev.text
            channel.remove(item)

    now = datetime.now(timezone.utc)
    dmg = f"ShedDesktop-{version}.dmg"
    url = f"https://github.com/{repo}/releases/download/{tag}/{dmg}"

    item = ET.SubElement(channel, "item")
    ET.SubElement(item, "title").text = version
    ET.SubElement(item, "pubDate").text = preserved_pubdate or now.strftime(PUBDATE_FMT)
    ET.SubElement(item, qname("version")).text = version
    ET.SubElement(item, qname("shortVersionString")).text = version
    ET.SubElement(item, qname("minimumSystemVersion")).text = min_macos
    if is_prerelease:
        ET.SubElement(item, qname("channel")).text = "beta"
    enclosure = ET.SubElement(item, "enclosure")
    enclosure.set("url", url)
    enclosure.set("type", "application/octet-stream")
    enclosure.set(qname("edSignature"), sig)
    enclosure.set("length", length)

    ET.indent(tree, space="  ")
    tree.write(appcast_path, xml_declaration=True, encoding="utf-8")
    with open(appcast_path, "a", encoding="utf-8") as f:
        f.write("\n")

    channel_kind = "beta" if is_prerelease else "stable"
    print(f"appended {version} ({channel_kind}) -> {appcast_path}")
    print(f"  enclosure: {url}")
    print(f"  length={length} edSignature={sig[:16]}...")


if __name__ == "__main__":
    main()
