// ColorHex.swift — a tiny sRGB hex decoder for the UI palette.
//
// Lives in ShedKit (not the SwiftUI layer) so the byte-order/alpha logic — the
// only error-prone part of the theme — is unit-testable without a UI target.
// AppKit is already a ShedKit dependency (Screenshot.swift).

import AppKit

public extension NSColor {
    /// Decode a `0xRRGGBB` value into an sRGB color with the given `alpha`.
    ///
    /// RGB-only on purpose: a packed `0xRRGGBBAA` form is ambiguous whenever the
    /// red byte is zero (`0x0000001A` is just `0x1A`), so translucency goes
    /// through `alpha` instead.
    convenience init(hex: UInt32, alpha: Double = 1.0) {
        let r = Double((hex >> 16) & 0xFF) / 255.0
        let g = Double((hex >> 8) & 0xFF) / 255.0
        let b = Double(hex & 0xFF) / 255.0
        self.init(srgbRed: r, green: g, blue: b, alpha: alpha)
    }
}
