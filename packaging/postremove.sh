#!/bin/bash
# nfpm-generated postrm for shed-server.
#
# dpkg invokes this with:
#   $1 = action  (remove, purge, upgrade, failed-upgrade, abort-install,
#                 abort-upgrade, disappear)
#
# /etc/shed/server.yaml is not a dpkg conffile (#109): packaging/postinstall.sh
# creates it from /usr/share/shed/server.yaml.default on a fresh install, so
# dpkg does not know to delete it on purge. Purge does that here; every other
# action leaves the operator's config alone.
set -e

case "$1" in
    purge)
        # The .dpkg-new is the conffile-era template a #109-stuck host may
        # still carry.
        rm -f /etc/shed/server.yaml /etc/shed/server.yaml.dpkg-new
        # /etc/shed normally still holds the SSH host key and the TLS and CA
        # material shed-server generated, which purge leaves in place, so a
        # non-empty directory must not fail the purge.
        rmdir /etc/shed 2>/dev/null || true
        ;;
    *)
        # remove, upgrade, failed-upgrade, abort-*, disappear: keep the config.
        ;;
esac

exit 0
