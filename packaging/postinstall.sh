#!/bin/bash
# nfpm-generated postinst for shed-server.
#
# dpkg invokes this with:
#   $1 = action  ("configure" on install/upgrade; abort-* on rollback)
#   $2 = previous package version, empty on a fresh install
#
# Convention: a fresh install enables (but does NOT start) the unit and
# prints the "edit config then start" hint, because the operator needs
# to fill in /etc/shed/server.yaml first. An upgrade try-restarts the
# unit so the new binary actually replaces the running old one — pre-
# v0.5.9 the postinst skipped the restart, leaving operators with a
# 0.5.x-1 binary still serving while `dpkg-query` reported 0.5.x. See
# docs/upgrades/v0.5.7-to-v0.5.8.md for the original incident.
set -e

# Guard against non-systemd environments (chroots, container builds).
# `systemctl --system` returns nonzero outside a real systemd host.
# Redirect both stdout (which would otherwise dump the full unit list
# into dpkg's install output — caught by CodeRabbit on PR #150) and
# stderr.
if ! command -v systemctl >/dev/null 2>&1 || ! systemctl --system >/dev/null 2>&1; then
    exit 0
fi

systemctl daemon-reload || true
systemctl enable shed-server.service || true

if [ -n "${2:-}" ]; then
    # Upgrade path. Config files are installed `noreplace`, so an operator
    # upgrading across a breaking config change keeps their old
    # /etc/shed/server.yaml. v0.6.0 removed base_rootfs + images in favor of
    # default_image + image_aliases + pull_policy, and the new binary
    # rejects the old keys — so a blind restart would crash the service.
    # Preflight the config and SKIP the restart (pointing at the upgrade
    # guide) when it no longer validates, rather than take the service down.
    if ! shed-server --config /etc/shed/server.yaml config-validate >/dev/null 2>&1; then
        echo ""
        echo "shed-server was upgraded, but /etc/shed/server.yaml did not validate"
        echo "against the new binary, so the service was NOT restarted (the old"
        echo "process keeps running)."
        echo ""
        echo "This usually means the config still uses keys removed in v0.6.0"
        echo "(base_rootfs / images). Migrate to default_image + image_aliases +"
        echo "pull_policy, then restart:"
        echo "  sudo shed-server --config /etc/shed/server.yaml config-validate"
        echo "  sudo systemctl restart shed-server"
        echo ""
        echo "Upgrade guide: https://github.com/charliek/shed/blob/main/docs/upgrades/v0.5.9-to-v0.6.0.md"
        exit 0
    fi

    # `try-restart` is the Debian-standard primitive for "restart only if
    # the unit is already active". If the operator had stopped the service
    # deliberately (maintenance window, config debugging) we don't surprise
    # them by auto-starting it; we just swap binaries and leave activation
    # state alone.
    if command -v deb-systemd-invoke >/dev/null 2>&1; then
        deb-systemd-invoke try-restart shed-server.service || true
    else
        # Fallback for systems missing the dh-systemd helper (rare on
        # modern Debian/Ubuntu but possible on stripped derivatives).
        if systemctl is-active --quiet shed-server.service; then
            systemctl restart shed-server.service || true
        fi
    fi

    echo ""
    if systemctl is-active --quiet shed-server.service; then
        echo "shed-server has been upgraded and restarted (was running)."
    else
        echo "shed-server has been upgraded; unit was inactive so it was left stopped."
        echo "Start it with: sudo systemctl start shed-server"
    fi
else
    echo ""
    echo "shed-server has been installed and enabled."
    echo ""
    echo "Next steps:"
    echo "  1. Edit /etc/shed/server.yaml"
    echo "  2. Run: sudo shed-server setup"
    echo "  3. Run: sudo systemctl start shed-server"
fi
