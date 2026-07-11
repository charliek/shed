#!/usr/bin/env bash
# Regenerate all shed-desktop icon assets from the owl SVG.
#
# Runs generate_icons.py in an ephemeral uv env (cairosvg + Pillow), so no
# global Python deps are needed. Outputs (committed):
#   tauri/src-tauri/icons/{32x32,128x128,128x128@2x,icon.png,icon.icns,icon.ico,
#                          Square30x30Logo,StoreLogo}
#   packaging/icons/hicolor/{256x256,512x512}/apps/shed-desktop.png   (Linux .deb)
#   Resources/AppIcon.icns                                            (Swift app)
#
# Change the brand colors (then commit the regenerated assets):
#   ./packaging/icon/regenerate.sh --bg '#1F6FEB'
#
# Defaults to the shed owl brand (white owl / orange #E8722A / green #3DAA5C
# eyes). Run on macOS to refresh the .icns bundles (iconutil is macOS-only; on
# Linux it writes the PNGs + .ico and skips the .icns).
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# cairosvg loads native libcairo via cairocffi/ctypes, which doesn't search
# Homebrew's prefix on macOS. Add it to the dyld fallback path so the render
# works on a Mac dev box. (Linux finds libcairo via the normal loader cache.)
if [ "$(uname -s)" = "Darwin" ] && command -v brew >/dev/null 2>&1; then
  export DYLD_FALLBACK_LIBRARY_PATH="$(brew --prefix)/lib:${DYLD_FALLBACK_LIBRARY_PATH:-/usr/local/lib:/usr/lib}"
fi

# --no-project: run in a throwaway env, ignoring the surrounding desktop/ and
# repo-root pyproject.toml (otherwise uv would sync one and leave a .venv).
exec uv run --no-project --with cairosvg --with pillow python3 "${SCRIPT_DIR}/generate_icons.py" "$@"
