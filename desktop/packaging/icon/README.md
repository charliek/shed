# shed-desktop icon toolchain

The shed-desktop app icon is the shed owl mascot (shared lineage with
shed-mobile and the sibling roost/lumen projects), recolored to shed's brand.
The SVG source + the generator live here so the icon can be **regenerated with
one command** — no binary editing.

## Brand

A **white owl** on an **orange rounded squircle**, with **green** irises and
white pupils:

| Element    | Hex       |
|------------|-----------|
| Owl body   | `#FFFFFF` |
| Background | `#E8722A` |
| Irises     | `#3DAA5C` |
| Pupils     | `#FFFFFF` (baked into the SVG) |

The owl is composed near full-bleed (roost's larger-owl proportions), not the
smaller shed-mobile inset. Note: the **in-app** sidebar owl (in the Tauri UI)
deliberately keeps **amber** eyes, mirroring shed-mobile — that asset lives in
the UI tree, not here; this pipeline is only the app/launcher/dock icon.

## Files

- `reference/owl_logo_colored.svg` — owl with baked `#F4C430` irises + white
  pupils; the body (`fill="currentColor"`) is recolored and the irises are
  swapped to the eye color. **Source of truth — edit this to change the art.**
- `generate_icons.py` — renders + composes every packaging target.
- `regenerate.sh` — runs the generator in an ephemeral `uv` env (cairosvg + Pillow).

## Outputs (committed; CI does not regenerate)

- `tauri/src-tauri/icons/{32x32,128x128,128x128@2x}.png` + `icon.png` (512) —
  Tauri window/dock/app icon
- `tauri/src-tauri/icons/icon.icns` — macOS window/dock icon (via `iconutil`)
- `tauri/src-tauri/icons/icon.ico` — Windows multi-size (16/32/48/64/128/256, Pillow)
- `tauri/src-tauri/icons/{Square30x30Logo,StoreLogo}.png` — Windows Store tiles
  (regenerated for consistency)
- `tauri/src-tauri/AppIcon.icon/` — macOS 26 (Tahoe) **Icon Composer** catalog
  (`icon.json` + `Assets/owl.png`): an `srgb` `fill.solid` brand background with
  the owl as a foreground layer. `scripts/bundle-tauri-mac.sh` compiles it with
  `actool` into `Assets.car` + a flattened `AppIcon.icns` at bundle time — see
  the Tahoe note below
- `packaging/icons/hicolor/{256x256,512x512}/apps/shed-desktop.png` — Linux
  `.deb` hicolor icons (nfpm installs the same file under both `shed-desktop`
  and `ai.stridelabs.shed-desktop` names — see `packaging/nfpm.yaml`)
- `Resources/AppIcon.icns` — the Swift macOS app icon (via `iconutil`)

**Not produced here:** `tauri/src-tauri/icons/tray-template{,@2x}.png` — the
menu-bar template glyph is a separate black-on-transparent silhouette and stays
as committed. This pipeline never touches it.

## Regenerate

Paths below are relative to the **repo root** (the script resolves its own
location, so cwd doesn't affect the outputs — from `desktop/` drop the `desktop/`
prefix):

```sh
# Default: white owl on orange (#E8722A), green eyes
./desktop/packaging/icon/regenerate.sh

# Try a different background color (then commit the regenerated assets)
./desktop/packaging/icon/regenerate.sh --bg '#1F6FEB'
```

The PNG + `.ico` legs are cross-platform. The **`.icns` legs require `iconutil`,
which is macOS-only** — full regeneration (including `Resources/AppIcon.icns` and
`tauri/.../icon.icns`) must run on a Mac. On Linux the script writes the PNGs +
`.ico` and skips the `.icns` bundles. On a Mac dev box the wrapper adds
Homebrew's `lib` to the dyld path so cairosvg finds `libcairo`.

Regenerating is a deliberate local step — re-run, then commit the changed files.
Outputs are byte-deterministic (Pillow PNGs embed no timestamps), so a no-op
re-run produces no diff.

## The macOS icon-cache trap

LaunchServices / iconservices caches the Dock + Finder icon per bundle-id **and**
path. Rebuilding `build/ShedDesktop.app` in place and relaunching keeps showing
the **stale** icon — it looks like the change failed when it didn't. To actually
see a changed icon, launch a copy under a *fresh* bundle id:

```sh
cp -R build/ShedDesktop.app /tmp/ShedVerify.app
/usr/libexec/PlistBuddy -c "Set :CFBundleIdentifier ai.stridelabs.ShedVerify" \
  /tmp/ShedVerify.app/Contents/Info.plist
codesign --force --deep --sign - /tmp/ShedVerify.app
open /tmp/ShedVerify.app          # then look at the Dock
```

Or nuke the cache: `sudo rm -rf /Library/Caches/com.apple.iconservices.store &&
killall Dock Finder` (heavier hammer). On Linux, refresh with
`gtk-update-icon-cache` after installing the `.deb`.

## macOS 26 (Tahoe) glass icons

On Tahoe the system masks every Dock/Cmd-Tab icon to its own squircle and draws
a glass tile behind it. A loose `.icns` is treated as a **legacy** icon and
*inset* on that tile — a gray/white frame around the art, with muted color —
rather than filling it edge-to-edge. The fix (ported from the sibling roost app)
is a compiled **Icon Composer catalog**:

- `generate_icons.py` emits `tauri/src-tauri/AppIcon.icon/` — a hand-authored
  `icon.json` with the orange brand color as a **native `srgb` `fill.solid`**
  (not a baked bitmap) plus the shed owl as a transparent foreground layer, with
  roost's `shadow` (neutral 0.5) and `translucency` (enabled 0.5) tuning.
- `scripts/bundle-tauri-mac.sh` compiles it with `xcrun actool ... --app-icon
  AppIcon` into `Contents/Resources/Assets.car` (+ a flattened `AppIcon.icns`)
  *before* codesign, so the signature seals it. `actool` needs **full Xcode 26**
  (not the bare Command Line Tools); when it's absent the script falls back to
  the flat `AppIcon.icns` and logs it (the bundle still launches, just with the
  framed legacy icon on Tahoe).
- `CFBundleIconName=AppIcon` in `tauri/src-tauri/Info.plist` (merged into the
  bundle plist by the Tauri bundler) routes the OS to the catalog.

The catalog is committed; CI does not regenerate it. The Linux PNGs / Windows
`.ico` / flat `.icns` are unaffected (no system tile there) and remain the
pre-Tahoe / no-`actool` fallbacks.
