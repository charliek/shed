"""Generate shed-desktop app-icon assets from one owl SVG.

A small, self-contained pipeline (shared lineage with shed-mobile + roost):
recolor a single owl SVG with cairosvg, compose the per-platform PNGs with
Pillow, assemble the `.icns` bundles with `iconutil`, and write every packaging
target in one run. The brand colors are the one place to tweak the look (or pass
--color/--bg/--eye to iterate without a code edit).

Brand (shed owl): a WHITE #FFFFFF owl body on an ORANGE #E8722A rounded
squircle, GREEN #3DAA5C irises, WHITE pupils. The owl is composed near
full-bleed (roost's larger-owl proportions), not the smaller shed-mobile inset.

The owl geometry is reference/owl_logo_colored.svg: the body is
`fill="currentColor"` (swapped for --color); the irises are the EYE_SENTINEL hex
(swapped for --eye); the white pupils are left untouched.

Outputs (all committed; CI does not regenerate):
    tauri/src-tauri/icons/32x32.png             32    Tauri app icon
    tauri/src-tauri/icons/128x128.png           128
    tauri/src-tauri/icons/128x128@2x.png        256
    tauri/src-tauri/icons/icon.png              512
    tauri/src-tauri/icons/icon.icns             macOS window/dock (iconutil)
    tauri/src-tauri/icons/icon.ico              Windows multi-size (Pillow)
    tauri/src-tauri/icons/Square30x30Logo.png   30    Windows Store tile
    tauri/src-tauri/icons/StoreLogo.png         50    Windows Store logo
    packaging/icons/hicolor/256x256/apps/shed-desktop.png   Linux .deb hicolor
    packaging/icons/hicolor/512x512/apps/shed-desktop.png   Linux .deb hicolor
    Resources/AppIcon.icns                      Swift macOS app (iconutil)

The tray-template*.png glyphs are NOT produced here — the menu-bar template is a
separate black-on-transparent silhouette and stays as committed.

The `.icns` legs require `iconutil` (macOS-only). On Linux the script writes the
PNGs + .ico and skips the .icns bundles. See ./regenerate.sh and ./README.md.
"""

import argparse
import io
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

import cairosvg
from PIL import Image, ImageDraw

ICON_DIR = Path(__file__).resolve().parent
REF_DIR = ICON_DIR / "reference"
DESKTOP_ROOT = ICON_DIR.parent.parent  # .../desktop

SVG = REF_DIR / "owl_logo_colored.svg"
TAURI_ICONS = DESKTOP_ROOT / "tauri" / "src-tauri" / "icons"
HICOLOR = DESKTOP_ROOT / "packaging" / "icons" / "hicolor"
APPICON_ICNS = DESKTOP_ROOT / "Resources" / "AppIcon.icns"

# --- Brand — tweak me (or pass --color/--bg/--eye) --------------------------
DEFAULT_COLOR = "#FFFFFF"  # owl body (white)
DEFAULT_BG = "#E8722A"     # squircle background (orange)
DEFAULT_EYE = "#3DAA5C"    # irises (green)
# ----------------------------------------------------------------------------

# The iris fill baked into the SVG; swapped for the eye color at render time.
# (The body uses `currentColor`; the pupils stay #FFFFFF.)
EYE_SENTINEL = "#F4C430"

# Composition constants (roost's larger-owl proportions: near full-bleed square,
# small owl padding = big owl). CORNER_PCT ~= Apple's icon-grid corner radius.
RENDER_PX = 2048    # SVG raster width before LANCZOS downscale
MARGIN_PCT = 0.0    # full-bleed: the squircle fills the whole canvas
CORNER_PCT = 0.2237
OWL_PAD_PCT = 0.085  # owl inset inside the square (small = big owl)


def hex_to_rgb(s: str) -> tuple[int, int, int]:
    s = s.lstrip("#")
    if len(s) != 6:
        raise ValueError(f"expected #RRGGBB, got {s!r}")
    return (int(s[0:2], 16), int(s[2:4], 16), int(s[4:6], 16))


def render_owl(owl_hex: str, eye_hex: str) -> Image.Image:
    """Rasterize the owl SVG with the body + irises recolored, on a transparent
    canvas (native aspect ratio preserved; white pupils untouched)."""
    svg = SVG.read_text()
    # Irises first: with body-first ordering, --color equal to the iris
    # sentinel would get its freshly written body fill recolored to eye_hex.
    svg = svg.replace(f'fill="{EYE_SENTINEL}"', f'fill="{eye_hex}"')
    svg = svg.replace('fill="currentColor"', f'fill="{owl_hex}"')
    png = cairosvg.svg2png(bytestring=svg.encode(), output_width=RENDER_PX)
    assert isinstance(png, bytes)
    return Image.open(io.BytesIO(png)).convert("RGBA")


def compose(owl: Image.Image, size: int, bg: tuple[int, int, int]) -> Image.Image:
    """Compose the owl on a rounded-square (squircle) at the requested px size."""
    canvas = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    margin = int(size * MARGIN_PCT)
    inner = size - 2 * margin  # squircle side

    draw = ImageDraw.Draw(canvas)
    draw.rounded_rectangle(
        [margin, margin, margin + inner - 1, margin + inner - 1],
        radius=int(inner * CORNER_PCT),
        fill=(*bg, 255),
    )

    # Fit the owl inside the square, preserving aspect ratio.
    owl_box = int(inner * (1 - 2 * OWL_PAD_PCT))
    aspect = owl.width / owl.height
    if aspect >= 1:
        fit_w, fit_h = owl_box, max(1, int(owl_box / aspect))
    else:
        fit_w, fit_h = max(1, int(owl_box * aspect)), owl_box
    resized = owl.resize((fit_w, fit_h), Image.Resampling.LANCZOS)

    canvas.paste(resized, ((size - fit_w) // 2, (size - fit_h) // 2), resized)
    return canvas


def write_png(img: Image.Image, path: Path) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    img.save(path, "PNG")
    print(f"  wrote {path.relative_to(DESKTOP_ROOT)}  ({img.width}x{img.height})")


def write_ico(owl: Image.Image, bg: tuple[int, int, int], path: Path) -> None:
    """Write a multi-size Windows .ico from a 256px master (Pillow)."""
    sizes = [16, 32, 48, 64, 128, 256]
    master = compose(owl, 256, bg)
    path.parent.mkdir(parents=True, exist_ok=True)
    master.save(path, "ICO", sizes=[(s, s) for s in sizes])
    print(f"  wrote {path.relative_to(DESKTOP_ROOT)}  ({','.join(str(s) for s in sizes)})")


def build_icns(owl: Image.Image, bg: tuple[int, int, int], out: Path) -> None:
    """Render the iconset sizes and assemble a .icns via iconutil (macOS-only)."""
    if not shutil.which("iconutil"):
        print(f"  iconutil not found (non-macOS) — skipping {out.relative_to(DESKTOP_ROOT)}; "
              "regenerate on a Mac to refresh it")
        return
    sizes = [
        ("icon_16x16.png", 16), ("icon_16x16@2x.png", 32),
        ("icon_32x32.png", 32), ("icon_32x32@2x.png", 64),
        ("icon_128x128.png", 128), ("icon_128x128@2x.png", 256),
        ("icon_256x256.png", 256), ("icon_256x256@2x.png", 512),
        ("icon_512x512.png", 512), ("icon_512x512@2x.png", 1024),
    ]
    with tempfile.TemporaryDirectory() as td:
        iconset = Path(td) / "AppIcon.iconset"
        iconset.mkdir()
        for name, px in sizes:
            compose(owl, px, bg).save(iconset / name, "PNG")
        out.parent.mkdir(parents=True, exist_ok=True)
        subprocess.run(["iconutil", "-c", "icns", str(iconset), "-o", str(out)], check=True)
    print(f"  wrote {out.relative_to(DESKTOP_ROOT)}")


def main() -> None:
    ap = argparse.ArgumentParser(description="Generate shed-desktop owl icon assets.")
    ap.add_argument("--color", default=DEFAULT_COLOR, help="owl body color (#RRGGBB)")
    ap.add_argument("--bg", default=DEFAULT_BG, help="squircle background color (#RRGGBB)")
    ap.add_argument("--eye", default=DEFAULT_EYE, help="iris color (#RRGGBB)")
    args = ap.parse_args()

    if not SVG.exists():
        print(f"error: source SVG not found: {SVG}", file=sys.stderr)
        sys.exit(1)

    bg = hex_to_rgb(args.bg)
    owl = render_owl(args.color, args.eye)

    print(f"Generating shed-desktop icons (owl={args.color} bg={args.bg} eyes={args.eye})")

    # Tauri client icons (window/dock + packaging targets).
    write_png(compose(owl, 32, bg), TAURI_ICONS / "32x32.png")
    write_png(compose(owl, 128, bg), TAURI_ICONS / "128x128.png")
    write_png(compose(owl, 256, bg), TAURI_ICONS / "128x128@2x.png")
    write_png(compose(owl, 512, bg), TAURI_ICONS / "icon.png")
    write_png(compose(owl, 30, bg), TAURI_ICONS / "Square30x30Logo.png")
    write_png(compose(owl, 50, bg), TAURI_ICONS / "StoreLogo.png")
    write_ico(owl, bg, TAURI_ICONS / "icon.ico")
    build_icns(owl, bg, TAURI_ICONS / "icon.icns")

    # Linux .deb hicolor PNGs.
    write_png(compose(owl, 256, bg), HICOLOR / "256x256" / "apps" / "shed-desktop.png")
    write_png(compose(owl, 512, bg), HICOLOR / "512x512" / "apps" / "shed-desktop.png")

    # Swift macOS app icon.
    build_icns(owl, bg, APPICON_ICNS)

    print("Done.")


if __name__ == "__main__":
    main()
