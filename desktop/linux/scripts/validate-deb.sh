#!/usr/bin/env bash
# Install the shed-desktop .deb in a CLEAN ubuntu:24.04 container and assert the
# installed binary launches (its runtime deps resolve) and answers identify, that
# the bundled shedctl can drive it, and that the polkit action is installed.
# Used by `make deb-validate` and the CI deb job — proves the .deb's `depends:`
# are correct on a machine without the -dev packages.
set -euo pipefail

DEB="${1:?usage: validate-deb.sh <path-to-.deb>}"
DEB_DIR="$(cd "$(dirname "${DEB}")" && pwd)"
DEB_FILE="$(basename "${DEB}")"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "==> Install-validating ${DEB_FILE} in a clean ubuntu:24.04 container"
# --cap-add SYS_ADMIN + seccomp=unconfined let WebKitGTK's web-process bubblewrap
# sandbox create the user namespaces Docker's default seccomp blocks (else the
# content process dies and the window never realizes); --shm-size gives WebKit room.
docker run --rm \
  --cap-add SYS_ADMIN --security-opt seccomp=unconfined --shm-size=1g \
  -v "${DEB_DIR}:/pkg:ro" \
  -v "${SCRIPT_DIR}:/scripts:ro" \
  ubuntu:24.04 bash -lc "
    set -e
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    # apt resolves the .deb's runtime deps (libwebkit2gtk-4.1-0, libgtk-3-0,
    # libayatana-appindicator3-1, librsvg2-2, libsoup-3.0-0, libc6). --no-install-recommends
    # so 'polkitd' (a Recommends) is NOT pulled — proves the app launches + fails
    # closed without polkit, i.e. that polkit is genuinely only a Recommends.
    apt-get install -y -qq --no-install-recommends /pkg/${DEB_FILE} xvfb python3
    command -v shed-desktop
    command -v shedctl
    # The polkit action must ship in the package (the credential-approval gate
    # authenticates against it).
    test -f /usr/share/polkit-1/actions/ai.stridelabs.shed-desktop.policy
    echo 'polkit action present'
    python3 /scripts/deb_identify_check.py
  "
