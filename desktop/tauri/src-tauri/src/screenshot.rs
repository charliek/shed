//! `app.screenshot` for Tauri: shell out to a platform screenshot tool. Tauri has
//! no in-process window capture on Linux (WebKitGTK renders web content out of
//! process), so — unlike GTK's `GskRenderer` — we invoke an external tool. Tool
//! order: `grim` (Wayland/wlroots) → `scrot` (X11) → `import` (ImageMagick, X11) →
//! `screencapture` (macOS). Under Xvfb (CI) this captures the full display; the
//! harness assertion is only a non-empty PNG + dimensions, so `dashboard.dump`
//! stays the deterministic truth op and the pixel capture is best-effort.

use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, Instant};

static SHOT_SEQ: AtomicU64 = AtomicU64::new(0);

/// Kill a screenshot tool that hasn't finished within this window, so a hung or
/// wedged tool can't block the IPC request (and leak a child) until the client
/// socket times out.
const TOOL_TIMEOUT: Duration = Duration::from_secs(10);

/// Capture the screen to a PNG, returning `(png_bytes, width, height)` or an error
/// string. Blocking — call via `spawn_blocking`. Tries each platform tool in order
/// and validates a non-empty PNG *per candidate*, so a hung / empty / bogus tool
/// falls through to the next instead of failing the whole op.
pub fn capture() -> Result<(Vec<u8>, u32, u32), String> {
    let candidates: &[(&str, &[&str])] = if cfg!(target_os = "macos") {
        &[("screencapture", &["-x", "-t", "png"])]
    } else {
        &[("grim", &[]), ("scrot", &[]), ("import", &["-window", "root"])]
    };
    let mut problems: Vec<String> = Vec::new();
    for (tool, args) in candidates {
        // grim only works on wlroots compositors; skip it without a Wayland session
        // (it fails on GNOME-Wayland). The CI Xvfb leg is X11 → scrot/import.
        if *tool == "grim" && std::env::var_os("WAYLAND_DISPLAY").is_none() {
            continue;
        }
        let out = temp_png_path();
        let result = run_tool(tool, args, &out).and_then(|()| read_valid_png(&out));
        let _ = std::fs::remove_file(&out);
        match result {
            Ok((png, w, h)) => return Ok((png, w, h)),
            Err(e) => problems.push(format!("{tool}: {e}")),
        }
    }
    Err(format!("no screenshot captured ({})", problems.join("; ")))
}

fn temp_png_path() -> PathBuf {
    let n = SHOT_SEQ.fetch_add(1, Ordering::Relaxed);
    std::env::temp_dir().join(format!("shed-tauri-shot-{}-{}.png", std::process::id(), n))
}

/// Run one tool (outfile appended last), bounded by `TOOL_TIMEOUT` — kill it on
/// expiry so a hung tool can't wedge the request.
fn run_tool(tool: &str, args: &[&str], out: &Path) -> Result<(), String> {
    let mut child = Command::new(tool)
        .args(args)
        .arg(out)
        .spawn()
        .map_err(|e| format!("not runnable ({e})"))?;
    let deadline = Instant::now() + TOOL_TIMEOUT;
    loop {
        match child.try_wait() {
            Ok(Some(status)) if status.success() => return Ok(()),
            Ok(Some(status)) => {
                let code = status
                    .code()
                    .map(|c| c.to_string())
                    .unwrap_or_else(|| "signal".into());
                return Err(format!(
                    "exited {code} (Screen-Recording permission? no display?)"
                ));
            }
            Ok(None) => {
                if Instant::now() >= deadline {
                    let _ = child.kill();
                    let _ = child.wait();
                    return Err(format!("timed out after {}s", TOOL_TIMEOUT.as_secs()));
                }
                std::thread::sleep(Duration::from_millis(50));
            }
            Err(e) => return Err(format!("wait failed ({e})")),
        }
    }
}

/// Read the tool's output and confirm it's a non-empty, valid PNG — so a
/// zero-byte or garbage file from a nominally-"successful" tool is rejected
/// (and the caller tries the next candidate) rather than returned.
fn read_valid_png(out: &Path) -> Result<(Vec<u8>, u32, u32), String> {
    let bytes = std::fs::read(out).map_err(|e| format!("read output ({e})"))?;
    if bytes.is_empty() {
        return Err("produced an empty file".into());
    }
    match png_dimensions(&bytes) {
        Some((w, h)) => Ok((bytes, w, h)),
        None => Err("output was not a valid PNG".into()),
    }
}

/// Parse width/height from a PNG's IHDR (bytes 16..24), avoiding an image-decode
/// dependency. `None` if the buffer isn't a PNG whose first chunk is IHDR.
fn png_dimensions(bytes: &[u8]) -> Option<(u32, u32)> {
    const SIG: &[u8] = b"\x89PNG\r\n\x1a\n";
    if bytes.len() < 24 || &bytes[0..8] != SIG || &bytes[12..16] != b"IHDR" {
        return None;
    }
    let w = u32::from_be_bytes(bytes[16..20].try_into().ok()?);
    let h = u32::from_be_bytes(bytes[20..24].try_into().ok()?);
    Some((w, h))
}

#[cfg(test)]
mod tests {
    use super::png_dimensions;

    #[test]
    fn parses_ihdr_dimensions() {
        // 1x1 PNG (sig + IHDR len/type + 1x1 + bit-depth/colour...).
        let png: &[u8] = &[
            0x89, b'P', b'N', b'G', b'\r', b'\n', 0x1a, b'\n', // signature
            0, 0, 0, 13, b'I', b'H', b'D', b'R', // IHDR length + type
            0, 0, 0, 1, 0, 0, 0, 1, // width=1, height=1
        ];
        assert_eq!(png_dimensions(png), Some((1, 1)));
    }

    #[test]
    fn rejects_non_png() {
        assert_eq!(png_dimensions(b"not a png at all......."), None);
        assert_eq!(png_dimensions(b"short"), None);
    }
}
