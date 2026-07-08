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
    #
    # Use the ABSOLUTE binary path: dpkg runs maintainer scripts with a minimal
    # PATH (/usr/sbin:/usr/bin:/sbin:/bin) that excludes /usr/local/bin, so a bare
    # `shed-server` is "command not found" here. That made the preflight *always*
    # fail (validation never ran), so every upgrade skipped the restart and left
    # the old binary serving. The path matches the unit's ExecStart.
    if ! validate_err="$(/usr/local/bin/shed-server --config /etc/shed/server.yaml config-validate 2>&1)"; then
        echo ""
        echo "shed-server was upgraded, but /etc/shed/server.yaml did not validate"
        echo "against the new binary, so the service was NOT restarted (the old"
        echo "process keeps running). The error was:"
        echo ""
        echo "  ${validate_err}"
        echo ""
        echo "This usually means the config uses keys removed in a breaking release"
        echo "(e.g. base_rootfs/images in v0.6.0, or http_bind/ssh_bind/"
        echo "internal_http_port in v0.7.4). Migrate per the upgrade guides, then:"
        echo "  sudo /usr/local/bin/shed-server --config /etc/shed/server.yaml config-validate"
        echo "  sudo systemctl restart shed-server"
        echo ""
        echo "Upgrade guides: https://github.com/charliek/shed/tree/main/docs/upgrades"
        exit 0
    fi

    # v0.7.4: with no bind_address, shed-server now binds 127.0.0.1 only (it was
    # all-interfaces before). config-validate still passes — the config is valid,
    # it just changed meaning — so warn explicitly that a networked server has
    # become local-only. (Loose grep: a commented "# bind_address" won't match.)
    if ! grep -Eq '^[[:space:]]*bind_address:' /etc/shed/server.yaml; then
        echo ""
        echo "NOTE (v0.7.4): /etc/shed/server.yaml sets no 'bind_address', so"
        echo "shed-server now binds 127.0.0.1 only (was all-interfaces). If you"
        echo "reach this server from other machines it is now LOCAL-ONLY. To"
        echo "restore network access, add to /etc/shed/server.yaml:"
        echo "    bind_address: 0.0.0.0"
        echo "  (open mode also requires allow_insecure_exposure: true; prefer"
        echo "   auth.mode: secure for anything exposed on a network)"
        echo "then: sudo systemctl restart shed-server"
        echo "Guide: https://github.com/charliek/shed/blob/main/docs/upgrades/v0.7.3-to-v0.7.4.md"
        echo ""
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
