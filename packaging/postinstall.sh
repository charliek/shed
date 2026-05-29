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
if ! command -v systemctl >/dev/null 2>&1 || ! systemctl --system 2>/dev/null; then
    exit 0
fi

systemctl daemon-reload || true
systemctl enable shed-server.service || true

if [ -n "${2:-}" ]; then
    # Upgrade path. `try-restart` is the Debian-standard primitive for
    # "restart only if the unit is already active". If the operator had
    # stopped the service deliberately (maintenance window, config
    # debugging) we don't surprise them by auto-starting it; we just
    # swap binaries and leave activation state alone.
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
