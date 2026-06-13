"""Guest-MTU auto-detection tests (issue #196).

Parameterized over `["vz", "fc"]` via the `shed_server` fixture. The core
assertion is an *internal consistency* invariant that holds on any host —
behind a VPN or not — and on both backends:

    - The guest's primary-interface MTU is always within [1280, 1500].
    - `shed.mtu=` is passed on the guest cmdline IFF shed-server detected a
      reduced host egress path (or a `guest_mtu` override is set); when present,
      the guest interface MTU equals that value (detection -> cmdline ->
      guest-apply round-trips), and `shed.mtu=0` is never emitted.
    - When absent (the common no-VPN path), the guest stays at 1500 — i.e. the
      change is invisible on a normal path (the "worst-case == today" guarantee).

Run under `make test-integration-dev` to exercise the new shed-server binary.
The reduced-MTU behavior itself (guest lowered to match + the VZ MSS clamp) is
validated separately by the simulated-reduced-MTU / real-VPN steps in the PR,
which need a <1500 host path the CI host does not have.
"""

from __future__ import annotations

import re

import pytest


def _guest_default_iface(shed_server, name: str) -> str:
    r = shed_server.exec(name, ["bash", "-lc", "ip route show default | awk '{print $5; exit}'"])
    assert r.returncode == 0, f"resolving default iface failed: {r.stdout!r} {r.stderr!r}"
    iface = r.stdout.strip()
    assert iface, f"no default-route interface in guest: {r.stdout!r} {r.stderr!r}"
    return iface


def test_guest_mtu_consistency(shed_server, test_shed_name):
    shed_server.create(test_shed_name, image="base")

    iface = _guest_default_iface(shed_server, test_shed_name)

    mtu_r = shed_server.exec(test_shed_name, ["cat", f"/sys/class/net/{iface}/mtu"])
    assert mtu_r.returncode == 0, f"reading guest MTU failed: {mtu_r.stdout!r} {mtu_r.stderr!r}"
    guest_mtu = int(mtu_r.stdout.strip())

    cmdline_r = shed_server.exec(test_shed_name, ["cat", "/proc/cmdline"])
    assert cmdline_r.returncode == 0, (
        f"reading /proc/cmdline failed: {cmdline_r.stdout!r} {cmdline_r.stderr!r}"
    )
    cmdline = cmdline_r.stdout

    assert "shed.mtu=0" not in cmdline, f"shed.mtu=0 must never be emitted: {cmdline!r}"
    assert 1280 <= guest_mtu <= 1500, f"guest MTU {guest_mtu} outside [1280, 1500]"

    m = re.search(r"shed\.mtu=(\d+)", cmdline)
    if m:
        passed = int(m.group(1))
        assert 1280 <= passed <= 1500, f"passed shed.mtu={passed} outside [1280, 1500]"
        assert guest_mtu == passed, (
            f"guest MTU {guest_mtu} does not match passed shed.mtu={passed}"
        )
    else:
        assert guest_mtu == 1500, (
            f"no shed.mtu= passed (normal 1500 path) but guest MTU is {guest_mtu}, expected 1500"
        )
