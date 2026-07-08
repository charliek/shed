// Screenshot.swift
//
// In-process window capture. Renders an NSWindow's contentView to a PNG
// via `cacheDisplay(in:to:)` into an off-screen NSBitmapImageRep — the
// same permission-free path roost uses. Works whether the window is
// focused, occluded, or off-screen (the whole point versus OS screen
// capture, which needs a TCC grant and a visible window).
//
// SwiftUI content is hosted in an NSHostingView inside a standard
// NSWindow, so `contentView.cacheDisplay` re-invokes the hosting view's
// draw into the bitmap exactly as it does for AppKit views.

import AppKit
import Foundation

public enum ScreenshotError: Error, CustomStringConvertible {
    case noWindow
    case minimized
    case noContentView
    case zeroSize
    case allocFailed
    case pngFailed
    case tooLarge(bytes: Int)

    public var description: String {
        switch self {
        case .noWindow: return "no window to capture"
        case .minimized: return "window is minimized; cannot capture"
        case .noContentView: return "window has no content view"
        case .zeroSize: return "window has zero size"
        case .allocFailed: return "failed to allocate bitmap rep"
        case .pngFailed: return "PNG encoding failed"
        case .tooLarge(let b):
            return "screenshot too large: \(b) base64 bytes exceeds the \(ipcMaxFrameBytes) byte IPC frame cap (try scale 1)"
        }
    }
}

public struct CapturedImage: Sendable {
    public let png: Data
    public let width: Int
    public let height: Int
}

/// Capture `window` to a PNG at `scale` (1 or 2). Throws a structured
/// `ScreenshotError` rather than writing an oversized frame the client
/// would reject.
@MainActor
public func captureWindowPNG(_ window: NSWindow?, scale: Int) throws -> CapturedImage {
    guard let window else { throw ScreenshotError.noWindow }
    if window.isMiniaturized { throw ScreenshotError.minimized }
    guard let contentView = window.contentView else { throw ScreenshotError.noContentView }

    let bounds = contentView.bounds
    let pixelsWide = Int((bounds.width * CGFloat(scale)).rounded())
    let pixelsHigh = Int((bounds.height * CGFloat(scale)).rounded())
    guard pixelsWide > 0, pixelsHigh > 0 else { throw ScreenshotError.zeroSize }

    guard let rep = NSBitmapImageRep(
        bitmapDataPlanes: nil,
        pixelsWide: pixelsWide,
        pixelsHigh: pixelsHigh,
        bitsPerSample: 8,
        samplesPerPixel: 4,
        hasAlpha: true,
        isPlanar: false,
        colorSpaceName: .deviceRGB,
        bytesPerRow: 0,
        bitsPerPixel: 0
    ) else { throw ScreenshotError.allocFailed }

    // A point-sized rep over a pixel-sized buffer makes AppKit super-sample
    // draw(_:) across the larger grid — the supported lever for crisp 2x.
    rep.size = bounds.size
    window.effectiveAppearance.performAsCurrentDrawingAppearance {
        contentView.cacheDisplay(in: bounds, to: rep)
    }

    guard let png = rep.representation(using: .png, properties: [:]) else {
        throw ScreenshotError.pngFailed
    }
    // Preflight the 16 MiB IPC frame cap (png dominates once base64-expanded).
    let encodedLen = (png.count + 2) / 3 * 4
    if encodedLen + 1024 > ipcMaxFrameBytes {
        throw ScreenshotError.tooLarge(bytes: encodedLen)
    }
    return CapturedImage(png: png, width: pixelsWide, height: pixelsHigh)
}
